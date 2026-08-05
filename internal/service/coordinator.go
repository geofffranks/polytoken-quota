// Package service holds the Coordinator: the single CLI-facing mutator that
// wires hook decoding, policy/state, reconciliation, staging, validation,
// publication, and recovery into one common locked transaction path.
//
// Every mutating operation (Init, HandleEvent, Reconcile, Sync, Set, Clear) goes
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
// remain last-known-good/pending at the same observed revision. An accepted hook
// event is persisted even when every target stays pending. Exit codes: 0
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
	"slices"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/hook"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
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
	// Sources provides live Polytoken source layers for the Init proposal and
	// the Sync import (both call policy.Init/policy.Import). It is nil-safe:
	// Init reports an error and Sync refuses when it is unset.
	Sources policy.SourceReader
	// DiagnosticState is the concrete state.Store used by the read-only Status
	// and Doctor diagnostic methods. It is separate from State (the
	// transactional StateStore interface) because doctor.Run needs the concrete
	// store's Load/RecoveredRetention. It is nil-safe: diagnostics return an
	// empty report when unset.
	DiagnosticState state.Store
	// DoctorInspectors carries the optional doctor inspectors (policy/target/
	// live/publish). Each is nil-safe: an unset inspector contributes no
	// findings, so Doctor never panics before full inspector wiring.
	DoctorInspectors DoctorInspectors
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

// DoctorInspectors holds the optional inspectors Doctor delegates to. Each field
// is nil-safe.
type DoctorInspectors struct {
	Policy    doctorPolicyInspector
	Targets   doctorTargetInspector
	Validator doctorLiveValidator
	Publisher doctorPublishInspector
}

// doctor inspector aliases (unexported) so the service package can reference the
// doctor interfaces without exporting them. They mirror doctor's
// PolicyInspector, TargetInspector, LiveValidator, and PublishInspector.
type (
	doctorPolicyInspector = interface {
		Findings(context.Context) []doctor.Finding
	}
	doctorTargetInspector = interface {
		Findings(context.Context) []doctor.Finding
	}
	doctorLiveValidator = interface {
		Findings(context.Context) []doctor.Finding
	}
	doctorPublishInspector = interface {
		Findings(context.Context) []doctor.Finding
	}
)

// transactionKind identifies which public mutator invoked transact.
type transactionKind uint8

const (
	txInit transactionKind = iota
	txEvent
	txReconcile
	txSync
	txSet
	txClear
	txDisable
	txEnable
	txReset
	txQuotaCheck
)

// transactionInput carries the kind-specific arguments into the common path.
type transactionInput struct {
	Event       *hook.Event
	DryRun      bool
	KeepStaging bool
	Force       bool
	Provider    string
	Patch       state.ProviderPatch
	Selector    state.Selector
	// Reconcile is set by QuotaCheck to trigger the full stage/validate/publish
	// flow after observations are applied.
	Reconcile bool
}

// Rejected/stale outcomes are non-mutating and exit 1.
var (
	errHookRejected = errors.New("service: rejected hook event; no mutation")
	errStaleEvent   = errors.New("service: stale event ignored; no mutation")
)

// defaultValidationTimeout is used when the policy omits an operational timeout.
const defaultValidationTimeout = 30 * time.Second

// --- public mutators (each enters the single transact path) -----------------

// Init is strict create-only: it rejects an existing desired.yaml with
// policy.ErrDesiredExists after lock/recovery and before any mutation. Otherwise
// it proposes a starter policy from sources, writes it create-only, and
// reconciles all targets.
func (c *Coordinator) Init(ctx context.Context) Outcome {
	return c.transact(ctx, txInit, transactionInput{})
}

// HandleEvent applies a decoded CodexBar hook event through the transaction
// path. A valid event is accepted and persisted even when all targets remain
// pending; a malformed/unknown event is rejected without mutation.
func (c *Coordinator) HandleEvent(ctx context.Context, e hook.Event) Outcome {
	ev := e
	return c.transact(ctx, txEvent, transactionInput{Event: &ev})
}

// Reconcile regenerates candidates from the current policy and persisted state.
// With dryRun it reports managed-field diffs and validation intent without
// mutating state or targets, while still locking and recovering first.
func (c *Coordinator) Reconcile(ctx context.Context, dryRun, keepStaging bool) Outcome {
	return c.transact(ctx, txReconcile, transactionInput{DryRun: dryRun, KeepStaging: keepStaging})
}

// Sync imports current managed fields as desired intent (guarded unless forced),
// atomically replaces desired.yaml, reconciles all targets, and publishes state.
func (c *Coordinator) Sync(ctx context.Context, force bool) Outcome {
	return c.transact(ctx, txSync, transactionInput{Force: force})
}

// Set applies a typed provider override, reconciles all targets, and publishes.
func (c *Coordinator) Set(ctx context.Context, provider string, patch state.ProviderPatch) Outcome {
	return c.transact(ctx, txSet, transactionInput{Provider: provider, Patch: patch})
}

// Clear resets provider(s) by selector, reconciles all targets, and publishes.
func (c *Coordinator) Clear(ctx context.Context, sel state.Selector) Outcome {
	return c.transact(ctx, txClear, transactionInput{Selector: sel})
}

// Disable marks one configured provider as manually disabled, reconciles all
// targets, and publishes the accepted state even when targets remain pending.
func (c *Coordinator) Disable(ctx context.Context, provider string) Outcome {
	return c.transact(ctx, txDisable, transactionInput{Provider: provider})
}

// Enable clears one configured provider's manual disable and reconciles all
// targets using the current automatic provider state.
func (c *Coordinator) Enable(ctx context.Context, provider string) Outcome {
	return c.transact(ctx, txEnable, transactionInput{Provider: provider})
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
// recovers any prior journal, dispatches to the kind-specific handler, and
// releases the lock on every return path.
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
		return c.transactInit(ctx, recovered)
	case txEvent:
		return c.transactEvent(ctx, recovered, in)
	case txReconcile:
		return c.transactReconcile(ctx, recovered, in)
	case txSync:
		return c.transactSync(ctx, recovered, in)
	case txSet, txClear:
		return c.transactSetClear(ctx, recovered, in, kind)
	case txDisable, txEnable, txReset:
		return c.transactManual(ctx, recovered, in, kind)
	case txQuotaCheck:
		return c.transactQuotaCheck(ctx, recovered, in)
	}
	return Outcome{Error: errors.New("service: unknown transaction kind")}
}

// transactInit implements the strict create-only guard and the full create path.
func (c *Coordinator) transactInit(ctx context.Context, recovered state.State) Outcome {
	if c.Policy.DesiredExists() {
		c.step("desired-exists")
		return Outcome{Accepted: false, Error: policy.ErrDesiredExists}
	}
	if c.Sources == nil {
		return Outcome{Accepted: false, Error: errors.New("service: init requires a source reader")}
	}
	proposed, _, err := policy.Init(ctx, c.Sources)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	desired := proposed
	c.step("load-sources")
	if _, err := c.Targets.ResolveTargets(desired); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if err := c.PolicyWriter.CreateAtomic(ctx, proposed); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	observed := recovered
	next := observed
	if next.Revision == 0 {
		next.Revision = 1
	}
	outcomes := c.reconcileAll(ctx, desired, observed, next, true)
	next = c.recordTargetOutcomes(next, outcomes)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// transactEvent accepts a hook event and reconciles all targets with a detailed
// per-target trace.
func (c *Coordinator) transactEvent(ctx context.Context, recovered state.State, in transactionInput) Outcome {
	if !validEvent(in.Event) {
		return Outcome{Accepted: false, Error: errHookRejected}
	}
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
	c.step("accept-revision")
	arrival := state.Arrival{Sequence: observed.Revision + 1, ReceivedAt: c.now()}
	next, accepted, _, err := state.ApplyEvent(observed, *in.Event, arrival)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if !accepted {
		return Outcome{Accepted: false, Error: errStaleEvent}
	}
	next.Revision = observed.Revision + 1
	outcomes := c.processTargets(ctx, desired, observed, next, targets, true)
	next = c.recordTargetOutcomes(next, outcomes)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
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
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// transactSync performs a guarded/forced import, atomically replaces the policy,
// then reconciles all targets.
func (c *Coordinator) transactSync(ctx context.Context, recovered state.State, in transactionInput) Outcome {
	if c.Sources == nil {
		return Outcome{Accepted: false, Error: errors.New("service: sync requires a source reader")}
	}
	c.step("load-policy")
	if _, err := c.Policy.LoadPolicy(); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	c.step("load-state")
	observed := recovered
	imported, _, err := policy.Import(ctx, c.Sources, observed, in.Force)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	desired := imported
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if err := c.PolicyWriter.ReplaceAtomic(ctx, imported); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	next := observed
	next.Revision = observed.Revision + 1
	outcomes := c.processTargets(ctx, desired, observed, next, targets, true)
	next = c.recordTargetOutcomes(next, outcomes)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// transactManual applies a manual provider transition and reconciles all targets
// with the same coarse trace used by Set/Clear.
func (c *Coordinator) transactManual(ctx context.Context, recovered state.State, in transactionInput, kind transactionKind) Outcome {
	c.step("load-policy")
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if kind != txReset {
		if in.Provider == "" {
			return Outcome{Accepted: false, Error: errors.New("service: manual provider command requires a provider")}
		}
		configured := false
		for _, mapping := range desired.Providers {
			if slices.Contains(mapping.CodexBarProviders, in.Provider) || slices.Contains(mapping.PolytokenProviders, in.Provider) {
				configured = true
				break
			}
		}
		if !configured {
			return Outcome{Accepted: false, Error: fmt.Errorf("service: provider %q is not configured", in.Provider)}
		}
	}
	c.step("load-state")
	observed := recovered
	var next state.State
	switch kind {
	case txDisable:
		c.step("manual-disable")
		next, err = state.DisableProvider(observed, in.Provider, c.now())
	case txEnable:
		c.step("manual-enable")
		next, err = state.EnableProvider(observed, in.Provider, c.now())
	case txReset:
		c.step("manual-reset")
		next, err = state.ResetManualDisables(observed, c.now())
	default:
		return Outcome{Accepted: false, Error: errors.New("service: unknown manual transition")}
	}
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	next.Revision = observed.Revision + 1
	c.step("reconcile")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		pending := pendingOutcome("manual-resolution", next.Revision, "resolve_targets", err)
		outcomes := []TargetOutcome{pending}
		next = c.recordTargetOutcomes(next, outcomes)
		if saveErr := c.State.Save(next); saveErr != nil {
			return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: fmt.Errorf("%w (state save: %v)", err, saveErr)}
		}
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	timeout := c.validationTimeout(desired)
	c.step("publish-targets")
	outcomes := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, observed, next, rt, timeout, true, false, false))
	}
	next = c.recordTargetOutcomes(next, outcomes)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
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
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, observed, next, rt, timeout, true, false, false))
	}
	next = c.recordTargetOutcomes(next, outcomes)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes, Error: err}
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
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, prior, next, rt, timeout, publish, true, keepStaging))
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
func (c *Coordinator) processOneTarget(ctx context.Context, desired policy.Desired, prior, next state.State, rt RegisteredTarget, timeout time.Duration, publish, detailed, keepStaging bool) TargetOutcome {
	id := targetID(rt)
	step := func(name string) {
		if detailed {
			c.step(name + ":" + id)
		}
	}
	step("render")
	ranks, _ := ComputeRanking(desired, next, c.now())
	plan, err := c.Builder.Build(desired, next, rt.Policy, ranks)
	if err != nil {
		step("record-pending")
		return pendingOutcome(id, next.Revision, "render", err)
	}
	step("stage")
	candidate, err := c.Stage.Stage(ctx, rt.Resolved, plan)
	if err != nil {
		step("record-pending")
		return pendingOutcome(id, next.Revision, "stage", err)
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
		return outcome
	}
	if publish {
		step("publish")
		tx, err := c.buildTransaction(prior, next, rt, plan, candidate)
		if err != nil {
			step("record-pending")
			return pendingOutcome(id, next.Revision, "publish", err)
		}
		// ApplyUnderLock: the Coordinator already holds the transaction lock;
		// the publisher must NOT re-acquire it (flock LOCK_EX is not re-entrant).
		if _, err := c.Publish.ApplyUnderLock(ctx, tx); err != nil {
			step("record-pending")
			return pendingOutcome(id, next.Revision, "publish", err)
		}
	}
	return appliedOutcome(id, next.Revision)
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

// validEvent reports whether a decoded hook event carries the required identity.
func validEvent(e *hook.Event) bool {
	return e != nil && e.Type != "" && e.Provider != "" && !e.Timestamp.IsZero()
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
// to their live and staged paths. The publisher's applyOne re-reads the temp
// file and asserts sha256(data) == NewHash before renaming, so NewHash MUST be
// the SHA-256 of the staged temp content the staging.Builder wrote. OldHash is
// the SHA-256 of the current live file (zero [32]byte on first publish when no
// live file exists). Mode is the live file's permission bits (0600 default when
// no live file exists) so the renamed file preserves it.
func (c *Coordinator) buildTransaction(prior, next state.State, rt RegisteredTarget, plan reconcile.Plan, candidate staging.Candidate) (publish.Transaction, error) {
	var replacements []publish.Replacement
	seen := map[string]bool{}
	for _, fe := range plan.Edits {
		if seen[fe.File] {
			continue
		}
		seen[fe.File] = true
		livePath := filepath.Join(rt.Resolved.CanonicalRoot, filepath.FromSlash(fe.File))
		tempPath := filepath.Join(candidate.ConfigDir, filepath.FromSlash(fe.File))
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
