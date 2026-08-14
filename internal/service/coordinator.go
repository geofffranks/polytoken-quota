// Package service holds the Coordinator: the single CLI-facing mutator that
// wires policy/state, reconciliation, staging, validation, publication, and
// recovery into one common locked transaction path.
//
// Every mutating operation (Init, Reconcile, Set, Clear) goes
// through Coordinator.transact with this exact order:
//
//  1. acquire the advisory lock
//  2. recover any prior unfinished apply journal
//  3. load and validate policy, state, and target sources
//  4. compute the accepted next state revision in memory
//  5. independently render, stage, and validate each target
//  6. publish valid targets / record pending invalid targets
//  7. atomically publish state (the commit record)
//  8. release the lock
//
// Partial success: valid targets apply the accepted revision; invalid targets
// remain last-known-good/pending at the same observed revision. Exit codes: 0
// accepted + all applied, 2 accepted + one or more pending, 1 rejected/no
// mutation.
package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// Coordinator is the only CLI-facing mutator. Every dependency is injected for
// testability. transact owns the single locked transaction path; no dependency
// re-enters locking.
type Coordinator struct {
	Lock         publish.Locker
	Policy       PolicyLoader
	PolicyWriter policy.Writer
	State        StateStore
	Targets      TargetRegistry
	Builder      Reconciler
	Stage        Stager
	Validate     Validator
	Publish      Publisher
	Clock        Clock
	// Sources provides live Polytoken source layers for the Init proposal
	// (policy.Init) and the forced import (policy.Import). It is nil-safe:
	// init reports an error when it is unset.
	Sources policy.SourceReader
	// QuotaPoller polls provider quota adapters for the QuotaCheck transaction.
	// It is nil-safe: QuotaCheck reports an error when it is unset. Production
	// wires the real adapter-backed poller; tests inject a fake.
	QuotaPoller QuotaPoller
	// JournalPath is the write-ahead apply journal path used by the doctor's
	// quota inspector to detect an interrupted quota-check reconcile (a journal
	// left on disk). It is nil-safe: when empty the reconcile-pending check is
	// skipped.
	JournalPath string
	// tracer is the observability seam that records each transaction step. It
	// is nil in production; tests inject a recording tracer.
	tracer Tracer
}

// transactionKind identifies which public mutator invoked transact.
type transactionKind uint8

const (
	txInit transactionKind = iota
	txReconcile
	txSet
	txClear
	txDisable
	txEnable
	txReset
	txQuotaCheck
)

// transactionInput carries the kind-specific arguments into the common path.
type transactionInput struct {
	DryRun      bool
	KeepStaging bool
	Force       bool
	Provider    string
	Patch       state.ProviderPatch
	Selector    state.Selector
	// Reconcile is set by QuotaCheck to trigger the full stage/validate/publish
	// flow after observations are applied.
	Reconcile bool
	// Verbose requests the verbose reconcile trace in target outcomes.
	Verbose bool
}

// defaultValidationTimeout is used when the policy omits an operational timeout.
const defaultValidationTimeout = 30 * time.Second

// --- public mutators (each enters the single transact path) -----------------

// InitOptions controls whether init may replace an existing valid desired.yaml
// by importing the current managed Polytoken fields.
type InitOptions struct {
	Force bool
}

// InitWithOptions classifies desired.yaml under the lock before state loading or
// journal recovery. Plain init creates only when absent; forced init replaces
// only an existing valid policy.
func (c *Coordinator) InitWithOptions(ctx context.Context, opts InitOptions) Outcome {
	return c.transact(ctx, txInit, transactionInput{Force: opts.Force})
}

// Reconcile regenerates candidates from the current policy and persisted state.
// With dryRun it reports managed-field diffs and validation intent without
// mutating state or targets, while still locking and recovering first.
// With verbose it populates each target outcome with a decision trace.
func (c *Coordinator) Reconcile(ctx context.Context, dryRun, keepStaging, verbose bool) Outcome {
	return c.transact(ctx, txReconcile, transactionInput{DryRun: dryRun, KeepStaging: keepStaging, Verbose: verbose})
}

// Set applies a typed provider override, reconciles all targets, and publishes.
func (c *Coordinator) Set(ctx context.Context, provider string, patch state.ProviderPatch) Outcome {
	return c.transact(ctx, txSet, transactionInput{Provider: provider, Patch: patch})
}

// Clear resets provider(s) by selector, reconciles all targets, and publishes.
func (c *Coordinator) Clear(ctx context.Context, sel state.Selector) Outcome {
	return c.transact(ctx, txClear, transactionInput{Selector: sel})
}

// Disable marks the provider mapping with one exact mapping ID as manually
// disabled, then reconciles all targets and publishes the accepted state.
func (c *Coordinator) Disable(ctx context.Context, mappingID string) Outcome {
	return c.transact(ctx, txDisable, transactionInput{Provider: mappingID})
}

// Enable clears the manual disable on the provider mapping with one exact
// mapping ID while preserving its automatic provider state.
func (c *Coordinator) Enable(ctx context.Context, mappingID string) Outcome {
	return c.transact(ctx, txEnable, transactionInput{Provider: mappingID})
}

// Reset clears every manual provider disable while preserving automatic state,
// then reconciles and publishes all targets.
func (c *Coordinator) Reset(ctx context.Context) Outcome {
	return c.transact(ctx, txReset, transactionInput{})
}

// QuotaCheck polls configured provider quota adapters and persists the
// observations through the locked transaction path. A failed attempt never
// replaces the last usable QuotaSnapshot; provider failures are isolated. With
// reconcile true it also runs the full stage/validate/publish pipeline against
// the freshly observed state. The provider filter, when non-empty, restricts
// polling to one mapping.
func (c *Coordinator) QuotaCheck(ctx context.Context, provider string, reconcile bool) Outcome {
	return c.transact(ctx, txQuotaCheck, transactionInput{Provider: provider, Reconcile: reconcile})
}

// --- the common locked transaction path -------------------------------------

// transact is the single entry point for every mutation. It acquires the lock,
// performs init's policy preflight before state/recovery, recovers accepted
// transactions, dispatches to the kind-specific handler, and releases the lock
// on every return path.
func (c *Coordinator) transact(ctx context.Context, kind transactionKind, in transactionInput) Outcome {
	c.step("lock")
	unlock, err := c.Lock.Lock(ctx)
	if err != nil {
		return Outcome{Error: fmt.Errorf("service: acquire lock: %w", err)}
	}
	defer func() {
		c.step("unlock")
		_ = unlock()
	}()

	var initExisting bool
	if kind == txInit {
		_, err := c.Policy.LoadPolicy()
		switch {
		case err == nil:
			initExisting = true
			if !in.Force {
				c.step("desired-exists")
				return Outcome{Error: policy.ErrDesiredExists}
			}
		case errors.Is(err, fs.ErrNotExist):
			if in.Force {
				return Outcome{Error: fmt.Errorf("service: forced init requires an existing desired.yaml: %w", err)}
			}
		default:
			return Outcome{Error: err}
		}
	}

	c.step("load-state")
	loaded, err := c.State.LoadState()
	if err != nil {
		return Outcome{Error: fmt.Errorf("service: load state: %w", err)}
	}

	c.step("recover")
	recovered, err := c.Publish.Recover(ctx, loaded)
	if err != nil {
		return Outcome{Error: fmt.Errorf("service: recover journal: %w", err)}
	}

	switch kind {
	case txInit:
		return c.transactInit(ctx, recovered, in, initExisting)
	case txReconcile:
		return c.transactReconcile(ctx, recovered, in)
	case txSet, txClear:
		return c.transactSetClear(ctx, recovered, in, kind)
	case txDisable, txEnable, txReset:
		return c.transactManual(ctx, recovered, in, kind)
	case txQuotaCheck:
		return c.transactQuotaCheck(ctx, recovered, in)
	}
	return Outcome{Error: errors.New("service: unknown transaction kind")}
}

// transactInit builds either a starter proposal or a forced import after the
// locked preflight and recovery have completed.
func (c *Coordinator) transactInit(ctx context.Context, recovered state.State, in transactionInput, existing bool) Outcome {
	if c.Sources == nil {
		return Outcome{Accepted: false, Error: errors.New("service: init requires a source reader")}
	}
	var desired policy.Desired
	var err error
	if in.Force {
		desired, _, err = policy.Import(ctx, c.Sources, recovered, true)
	} else {
		desired, _, err = policy.Init(ctx, c.Sources)
	}
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	c.step("load-sources")
	initTargets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	var published policy.PublicationResult
	if existing {
		published, err = c.PolicyWriter.ReplaceAtomic(ctx, desired)
	} else {
		published, err = c.PolicyWriter.CreateAtomic(ctx, desired)
	}
	if err != nil || !published.Committed {
		if err == nil {
			err = errors.New("service: policy publication did not commit")
		}
		return Outcome{Accepted: false, Error: err}
	}
	observed := recovered
	next := observed
	if next.Revision == 0 {
		next.Revision = 1
	}
	outcomes := c.reconcileAll(ctx, desired, observed, next, true)
	next = c.recordTargetOutcomes(next, outcomes)
	c.recordHistoryIfQualified(&next, txInit, in, outcomes, initTargets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: errors.Join(published.Warning, err)}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: published.Warning}
}
func nextEventSequence(s *state.State) uint64 {
	if s.NextEventSequence == 0 {
		s.NextEventSequence = 1
	}
	seq := s.NextEventSequence
	if seq == ^uint64(0) {
		return seq
	}
	s.NextEventSequence++
	return seq
}
func trackedProviders(s state.State) []string {
	out := make([]string, 0, len(s.Providers))
	for provider := range s.Providers {
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}
func appendManualEvent(next, prior state.State, kind transactionKind, in transactionInput, now time.Time) state.State {
	action := ""
	switch kind {
	case txDisable:
		action = string(state.TriggerRoutingDisable)
	case txEnable:
		action = string(state.TriggerRoutingEnable)
	case txReset:
		action = string(state.TriggerRoutingReset)
	case txSet:
		action = string(state.TriggerSet)
	case txClear:
		action = string(state.TriggerClear)
	default:
		return next
	}
	mapping := in.Provider
	if mapping == "" && in.Selector.Provider != "" {
		mapping = in.Selector.Provider
	}
	e := state.EventRecord{Sequence: nextEventSequence(&next), Revision: next.Revision, Ordinal: len(next.EventHistory.Events), At: now.UTC(), RecordedAt: now.UTC(), Category: state.EventManual, Action: action, MappingID: mapping, Result: state.EventChanged}
	next.EventHistory, _ = state.AppendEvent(next.EventHistory, e)
	return next
}

// transactReconcile regenerates candidates. Dry-run reports intent without
// mutation; otherwise it advances the revision, reconciles, and publishes.
func (c *Coordinator) transactReconcile(ctx context.Context, recovered state.State, in transactionInput) Outcome {
	c.step("load-policy")
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	c.step("load-state")
	observed := recovered
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if in.KeepStaging && !in.DryRun {
		return Outcome{Accepted: false, Error: errors.New("service: --keep-staging requires --dry-run")}
	}
	if in.DryRun {
		outcomes := c.processTargets(ctx, desired, observed, observed, targets, false, in.KeepStaging)
		return Outcome{Accepted: true, Revision: observed.Revision, Targets: outcomes}
	}
	next := observed
	next.Revision = observed.Revision + 1
	outcomes := c.processTargets(ctx, desired, observed, next, targets, true)
	next = c.recordTargetOutcomes(next, outcomes)
	c.recordHistoryIfQualified(&next, txReconcile, in, outcomes, targets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// transactManual resolves exact mapping IDs, applies one manual transition, and
// reconciles all targets with the Set/Clear coarse trace.
func (c *Coordinator) transactManual(ctx context.Context, recovered state.State, in transactionInput, kind transactionKind) Outcome {
	c.step("load-policy")
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if kind != txReset {
		if in.Provider == "" {
			return Outcome{Accepted: false, Error: errors.New("service: manual provider command requires a mapping ID")}
		}
		_, ok := desired.Providers[policy.MappingID(in.Provider)]
		if !ok {
			return Outcome{Accepted: false, Error: fmt.Errorf("service: mapping %q is not configured", sanitizeFailure(in.Provider))}
		}
	}
	c.step("load-state")
	observed := recovered
	var next state.State
	switch kind {
	case txDisable:
		c.step("manual-disable")
		next, err = state.SetManualDisabled(observed, []string{in.Provider}, true, c.now())
	case txEnable:
		c.step("manual-enable")
		next, err = state.SetManualDisabled(observed, []string{in.Provider}, false, c.now())
	case txReset:
		c.step("manual-reset")
		next, err = state.ResetManualDisables(observed, c.now())
	default:
		return Outcome{Accepted: false, Error: errors.New("service: unknown manual transition")}
	}
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	changed := true
	if kind == txDisable {
		changed = state.ManualDisableChanged(observed, []string{in.Provider}, true)
	}
	if kind == txEnable {
		changed = state.ManualDisableChanged(observed, []string{in.Provider}, false)
	}
	if kind == txReset {
		changed = state.ManualDisableChanged(observed, trackedProviders(observed), false)
	}
	if !changed {
		return Outcome{Accepted: true, HandledWithoutRevision: true, Revision: observed.Revision}
	}
	next.Revision = observed.Revision + 1
	c.step("reconcile")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		pending := pendingOutcome("manual-resolution", next.Revision, "resolve_targets", err)
		outcomes := []TargetOutcome{pending}
		next = c.recordTargetOutcomes(next, outcomes)
		if saveErr := c.State.Save(next); saveErr != nil {
			return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: fmt.Errorf("%w (state save: %v)", err, saveErr)}
		}
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	timeout := c.validationTimeout(desired)
	c.step("publish-targets")
	outcomes := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, observed, next, rt, timeout, true, false, false, false))
	}
	next = c.recordTargetOutcomes(next, outcomes)
	next = appendManualEvent(next, observed, kind, in, c.now())
	c.recordHistoryIfQualified(&next, kind, in, outcomes, targets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// transactSetClear applies a typed state transition (Set or Clear) and reconciles
// all targets with a coarse trace: the transition step, then a single reconcile
// and publish-targets step.
func (c *Coordinator) transactSetClear(ctx context.Context, recovered state.State, in transactionInput, kind transactionKind) Outcome {
	c.step("load-policy")
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	c.step("load-state")
	observed := recovered
	var next state.State
	switch kind {
	case txSet:
		c.step("state-set")
		next, err = state.SetProvider(observed, in.Provider, in.Patch, c.now())
	case txClear:
		c.step("state-clear")
		next, err = state.ClearProvider(observed, in.Selector, c.now())
	}
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	changed := true
	if kind == txSet {
		changed = state.SetChanged(observed, in.Provider, in.Patch)
	}
	if kind == txClear {
		changed = state.ClearChanged(observed, in.Selector)
	}
	if !changed {
		return Outcome{Accepted: true, HandledWithoutRevision: true, Revision: observed.Revision}
	}
	next.Revision = observed.Revision + 1
	c.step("reconcile")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	timeout := c.validationTimeout(desired)
	// The coarse path reports a single reconcile and publish-targets step for
	// the whole batch; processOneTarget emits no per-target steps (detailed=false).
	c.step("publish-targets")
	outcomes := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, observed, next, rt, timeout, true, false, false, false))
	}
	next = c.recordTargetOutcomes(next, outcomes)
	next = appendManualEvent(next, observed, kind, in, c.now())
	c.recordHistoryIfQualified(&next, kind, in, outcomes, targets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// reconcileAll is the detailed per-target pipeline used by Init. It emits
// render/stage/validate and (when publish is true) publish per target.
func (c *Coordinator) reconcileAll(ctx context.Context, desired policy.Desired, prior, next state.State, publish bool) []TargetOutcome {
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return nil
	}
	return c.processTargets(ctx, desired, prior, next, targets, publish)
}

// processTargets runs the detailed per-target pipeline (render → stage →
// validate → publish), emitting a trace step for each stage and target. When
// publish is false (dry-run) no publish or state mutation occurs.
func (c *Coordinator) processTargets(ctx context.Context, desired policy.Desired, prior, next state.State, targets []RegisteredTarget, publish bool, retain ...bool) []TargetOutcome {
	keepStaging := len(retain) > 0 && retain[0]
	timeout := c.validationTimeout(desired)
	outcomes := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, prior, next, rt, timeout, publish, true, keepStaging, false))
	}
	return outcomes
}

// processOneTarget renders, stages, validates, and (when publish is true)
// publishes a single target, returning its outcome. This is the single place
// the render → stage → validate → publish-or-pending pipeline lives. When
// detailed is true it emits a per-target trace step for each stage and outcome
// (render:/stage:/validate:/publish:/record-pending: suffixed with the target
// id); the coarse Set/Clear path passes false so the whole batch reports only
// the batch-level reconcile/publish-targets steps emitted by its caller.
func (c *Coordinator) processOneTarget(ctx context.Context, desired policy.Desired, prior, next state.State, rt RegisteredTarget, timeout time.Duration, publish, detailed, keepStaging, verbose bool) TargetOutcome {
	id := targetID(rt)
	step := func(name string) {
		if detailed {
			c.step(name + ":" + id)
		}
	}
	step("render")
	ranks, rankingResult := ComputeRanking(desired, next, c.now())
	plan, err := c.Builder.Build(desired, next, rt.Policy, ranks)
	if err != nil {
		step("record-pending")
		out := pendingOutcome(id, next.Revision, "render", err)
		if verbose {
			out.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
		}
		return out
	}
	step("stage")
	candidate, err := c.Stage.Stage(ctx, rt.Resolved, plan)
	if err != nil {
		step("record-pending")
		out := pendingOutcome(id, next.Revision, "stage", err)
		if verbose {
			out.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
		}
		return out
	}
	// Compute hash-based change qualification from the staged candidate before
	// validation/publish. This is the preparation data used by history recording.
	stagedDir := candidate.PublishDir
	if stagedDir == "" {
		stagedDir = candidate.ConfigDir
	}
	var prep *PrepareResult
	if p, err := BuildPrepareResult(id, plan, rt.Resolved.CanonicalRoot, stagedDir); err == nil {
		prep = &p
	}
	// The Validator adapter runs validation against a no-cleanup copy of the
	// candidate so the staged files survive into publish (applyOne renames the
	// temp files to their live paths). The Coordinator owns the candidate's
	// lifecycle and removes the staging root on every exit path after staging.
	cleanupCandidate := true
	defer func() {
		if cleanupCandidate {
			_ = candidate.Cleanup()
		}
	}()
	step("validate")
	result := c.Validate.Validate(ctx, candidate, timeout)
	if !result.StartupValid {
		step("record-pending")
		outcome := pendingValidate(id, next.Revision, c.now(), result)
		if keepStaging {
			cleanupCandidate = false
			outcome.StagingRoot = candidate.Root
		}
		if verbose {
			outcome.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
		}
		outcome.Prepare = prep
		return outcome
	}
	if publish {
		step("publish")
		tx, err := c.buildTransaction(prior, next, rt, plan, candidate)
		if err != nil {
			step("record-pending")
			out := pendingOutcome(id, next.Revision, "publish", err)
			if verbose {
				out.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
			}
			out.Prepare = prep
			return out
		}
		// ApplyUnderLock: the Coordinator already holds the transaction lock;
		// the publisher must NOT re-acquire it (flock LOCK_EX is not re-entrant).
		if _, err := c.Publish.ApplyUnderLock(ctx, tx); err != nil {
			step("record-pending")
			out := pendingOutcome(id, next.Revision, "publish", err)
			if verbose {
				out.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
			}
			out.Prepare = prep
			return out
		}
	}
	out := appliedOutcome(id, next.Revision)
	if verbose {
		out.Trace = c.buildTraceSafe(desired, next, rt, ranks, rankingResult, plan)
	}
	out.Prepare = prep
	return out
}

// --- helpers ----------------------------------------------------------------

// step records a transaction step through the tracer when one is configured.
func (c *Coordinator) step(s string) {
	if c.tracer != nil {
		c.tracer.Step(s)
	}
}

func (c *Coordinator) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now()
}

// buildTraceSafe assembles the verbose decision trace from the ranking result
// and plan. It converts the routing.RankingResult into RankEntryReports and
// delegates to buildTrace.
func (c *Coordinator) buildTraceSafe(desired policy.Desired, next state.State, rt RegisteredTarget, ranks reconcile.RankLookup, rankingResult routing.RankingResult, plan reconcile.Plan) *ReconcileTrace {
	entries := make([]RankEntryReport, 0, len(rankingResult.Entries))
	for _, e := range rankingResult.Entries {
		entries = append(entries, RankEntryReport{
			MappingID:   e.MappingID,
			Rank:        e.Rank,
			OffPeak:     e.OffPeak,
			Eligible:    e.Eligible,
			Explanation: e.Explanation,
		})
	}
	tr := buildTrace(desired, next, rt.Policy, ranks, entries, plan)
	return &tr
}

func (c *Coordinator) validationTimeout(desired policy.Desired) time.Duration {
	if desired.Operational.ValidationTimeout > 0 {
		return desired.Operational.ValidationTimeout
	}
	return defaultValidationTimeout
}

// targetID returns the target id, defaulting to "global" for the global target.
func targetID(rt RegisteredTarget) string {
	if rt.Policy.ID != "" {
		return rt.Policy.ID
	}
	if rt.Policy.Global {
		return "global"
	}
	return rt.Resolved.ID
}

// sortedProviderNames returns the keys of m in sorted order for deterministic
// status output.
func sortedProviderNames(m map[string]state.ProviderState) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// sortedTargetIDs returns the keys of m in sorted order for deterministic
// status output.
func sortedTargetIDs(m map[string]state.TargetState) []string {
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

// buildTransaction constructs a publish.Transaction from the plan's files mapped
// to their live and staged paths. Temp paths read from the candidate's
// PublishDir — the real-content copies with plan edits applied — so the
// inert placeholders used for validation in ConfigDir never reach live files.
// The publisher's applyOne re-reads the temp file and asserts sha256(data) ==
// NewHash before renaming, so NewHash MUST be the SHA-256 of the staged temp
// content the staging.Builder wrote. OldHash is the SHA-256 of the current live
// file (zero [32]byte on first publish when no live file exists). Mode is the
// live file's permission bits (0600 default when no live file exists) so the
// renamed file preserves it.
func (c *Coordinator) buildTransaction(prior, next state.State, rt RegisteredTarget, plan reconcile.Plan, candidate staging.Candidate) (publish.Transaction, error) {
	var replacements []publish.Replacement
	seen := map[string]bool{}
	// SourceDir is the real-content publish dir; fall back to ConfigDir when
	// the publish dir is empty (e.g. a plan with no edits, or an older
	// candidate constructed without PublishDir).
	sourceDir := candidate.PublishDir
	if sourceDir == "" {
		sourceDir = candidate.ConfigDir
	}
	for _, fe := range plan.Edits {
		if seen[fe.File] {
			continue
		}
		seen[fe.File] = true
		livePath := filepath.Join(rt.Resolved.CanonicalRoot, filepath.FromSlash(fe.File))
		tempPath := filepath.Join(sourceDir, filepath.FromSlash(fe.File))
		r, err := buildReplacement(livePath, tempPath)
		if err != nil {
			return publish.Transaction{}, err
		}
		replacements = append(replacements, r)
	}
	return publish.Transaction{
		Prior:        prior,
		Next:         next,
		TargetID:     targetID(rt),
		ManagedRoot:  rt.Resolved.CanonicalRoot,
		Replacements: replacements,
	}, nil
}

// buildReplacement computes the hash and mode metadata for one managed file
// replacement from its live and staged temp paths. Any failure to read the
// staged temp file, or any live-file inspection error other than not-exist, is
// returned rather than swallowed: a zero NewHash would otherwise surface later
// as an opaque publisher hash-mismatch instead of the real filesystem error.
func buildReplacement(livePath, tempPath string) (publish.Replacement, error) {
	r := publish.Replacement{LivePath: livePath, TempPath: tempPath, Mode: defaultReplacementMode}
	// NewHash is the SHA-256 of the staged temp file the Builder wrote. This is
	// the hash applyOne asserts before the atomic rename, so it must match the
	// on-disk temp bytes exactly.
	data, err := os.ReadFile(tempPath)
	if err != nil {
		return publish.Replacement{}, fmt.Errorf("read staged file: %w", err)
	}
	r.NewHash = sha256.Sum256(data)
	// OldHash and Mode come from the live file when it exists; on a first
	// publish (no live file) OldHash stays the zero digest and Mode the default.
	info, err := os.Stat(livePath)
	switch {
	case err == nil:
		r.Mode = info.Mode()
		live, err := os.ReadFile(livePath)
		if err != nil {
			return publish.Replacement{}, fmt.Errorf("read live file: %w", err)
		}
		r.OldHash = sha256.Sum256(live)
	case os.IsNotExist(err):
		// First publish: zero OldHash and the default mode.
	default:
		return publish.Replacement{}, fmt.Errorf("stat live file: %w", err)
	}
	return r, nil
}

// defaultReplacementMode is the permission applied to a newly created live
// managed file on first publish (when no live file exists to inherit from).
const defaultReplacementMode fs.FileMode = 0o600

// recordTargetOutcomes folds the per-target outcomes into the committed state's
// per-target metadata. Applied targets clear their pending error; pending
// targets record their structured failure at last-known-good.
func (c *Coordinator) recordTargetOutcomes(s state.State, outcomes []TargetOutcome) state.State {
	if s.Targets == nil {
		s.Targets = map[string]state.TargetState{}
	}
	now := c.now()
	for _, o := range outcomes {
		prior := s.Targets[o.TargetID]
		ts := state.TargetState{
			AttemptedRevision: o.AttemptedRevision,
			AppliedRevision:   o.AppliedRevision,
			AttemptedAt:       now,
		}
		if o.Pending != nil {
			pending := *o.Pending
			pending.TargetID = o.TargetID
			pending.AttemptedRevision = o.AttemptedRevision
			if pending.AttemptedAt.IsZero() {
				pending.AttemptedAt = now
			}
			pending.LastSuccessfulRevision = prior.AppliedRevision
			pending.LastSuccessfulAt = prior.AppliedAt
			ts.Pending = &pending
		} else {
			ts.AppliedAt = now
		}
		s.Targets[o.TargetID] = ts
	}
	return s
}

// appliedOutcome records a target that published the accepted revision.
func appliedOutcome(id string, rev uint64) TargetOutcome {
	return TargetOutcome{TargetID: id, AttemptedRevision: rev, AppliedRevision: rev}
}

// pendingOutcome records a target that failed before or during publication.
func pendingOutcome(id string, rev uint64, stage string, err error) TargetOutcome {
	return TargetOutcome{
		TargetID:          id,
		AttemptedRevision: rev,
		Pending: &state.ApplyFailure{
			TargetID:          sanitizeFailure(id),
			Stage:             sanitizeFailure(stage),
			Summary:           sanitizeFailure(err.Error()),
			AttemptedRevision: rev,
			LiveStatus:        "last-known-good",
		},
	}
}

func sanitizeFailure(s string) string {
	return validate.DefaultSanitize([]byte(s))
}

func validationRemediation(result validate.Result) string {
	if result.Error == nil {
		return "re-run reconcile after resolving the validation failure"
	}
	return sanitizeFailure(result.Error.Remediation)
}

// pendingValidate records a target whose staged candidate failed validation.
func pendingValidate(id string, rev uint64, attemptedAt time.Time, result validate.Result) TargetOutcome {
	stage := string(validate.ConfigValidate)
	summary := "validation failed"
	if result.Error != nil {
		stage = string(result.Error.Stage)
		summary = result.Error.Summary
	}
	return TargetOutcome{
		TargetID:          id,
		AttemptedRevision: rev,
		Pending: &state.ApplyFailure{
			TargetID:          sanitizeFailure(id),
			Stage:             sanitizeFailure(stage),
			Summary:           sanitizeFailure(summary),
			Remediation:       validationRemediation(result),
			AttemptedRevision: rev,
			AttemptedAt:       attemptedAt,
			LiveStatus:        "last-known-good",
		},
	}
}
