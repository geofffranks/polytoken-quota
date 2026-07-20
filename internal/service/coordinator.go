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
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/hook"
	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/publish"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/validate"
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
	// tracer is the observability seam that records each transaction step. It
	// is nil in production; tests inject a recording tracer.
	tracer Tracer
}

// transactionKind identifies which public mutator invoked transact.
type transactionKind uint8

const (
	txInit transactionKind = iota
	txEvent
	txReconcile
	txSync
	txSet
	txClear
)

// transactionInput carries the kind-specific arguments into the common path.
type transactionInput struct {
	Event    *hook.Event
	DryRun   bool
	Force    bool
	Provider string
	Patch    state.ProviderPatch
	Selector state.Selector
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
func (c *Coordinator) Reconcile(ctx context.Context, dryRun bool) Outcome {
	return c.transact(ctx, txReconcile, transactionInput{DryRun: dryRun})
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

	c.step("recover")
	recovered, err := c.Publish.Recover(ctx, state.State{})
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
	if err := c.PolicyWriter.CreateAtomic(ctx, proposed); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	desired := proposed
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
	if in.DryRun {
		outcomes := c.processTargets(ctx, desired, observed, observed, targets, false)
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
	if err := c.PolicyWriter.ReplaceAtomic(ctx, imported); err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	desired := imported
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
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
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, observed, next, rt, timeout, true, false))
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
func (c *Coordinator) processTargets(ctx context.Context, desired policy.Desired, prior, next state.State, targets []RegisteredTarget, publish bool) []TargetOutcome {
	timeout := c.validationTimeout(desired)
	outcomes := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		outcomes = append(outcomes, c.processOneTarget(ctx, desired, prior, next, rt, timeout, publish, true))
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
func (c *Coordinator) processOneTarget(ctx context.Context, desired policy.Desired, prior, next state.State, rt RegisteredTarget, timeout time.Duration, publish, detailed bool) TargetOutcome {
	id := targetID(rt)
	step := func(name string) {
		if detailed {
			c.step(name + ":" + id)
		}
	}
	step("render")
	plan, err := c.Builder.Build(desired, next, rt.Policy)
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
	step("validate")
	result := c.Validate.Validate(ctx, candidate, timeout)
	if !result.StartupValid {
		step("record-pending")
		return pendingValidate(id, next.Revision, result)
	}
	if publish {
		step("publish")
		tx := c.buildTransaction(prior, next, rt, plan, candidate)
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

// buildTransaction constructs a publish.Transaction from the plan's files mapped
// to their live and staged paths. The publisher computes hashes, modes, backups,
// and journals from these paths under the Coordinator's already-held lock.
func (c *Coordinator) buildTransaction(prior, next state.State, rt RegisteredTarget, plan reconcile.Plan, candidate staging.Candidate) publish.Transaction {
	var replacements []publish.Replacement
	seen := map[string]bool{}
	for _, fe := range plan.Edits {
		if seen[fe.File] {
			continue
		}
		seen[fe.File] = true
		replacements = append(replacements, publish.Replacement{
			LivePath: filepath.Join(rt.Resolved.CanonicalRoot, filepath.FromSlash(fe.File)),
			TempPath: filepath.Join(candidate.ConfigDir, filepath.FromSlash(fe.File)),
		})
	}
	return publish.Transaction{
		Prior:        prior,
		Next:         next,
		TargetID:     targetID(rt),
		Replacements: replacements,
	}
}

// recordTargetOutcomes folds the per-target outcomes into the committed state's
// per-target metadata. Applied targets clear their pending error; pending
// targets record their structured failure at last-known-good.
func (c *Coordinator) recordTargetOutcomes(s state.State, outcomes []TargetOutcome) state.State {
	if s.Targets == nil {
		s.Targets = map[string]state.TargetState{}
	}
	now := c.now()
	for _, o := range outcomes {
		ts := state.TargetState{
			AttemptedRevision: o.AttemptedRevision,
			AppliedRevision:   o.AppliedRevision,
			AttemptedAt:       now,
		}
		if o.Pending != nil {
			ts.Pending = o.Pending
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
			TargetID:          id,
			Stage:             stage,
			Summary:           err.Error(),
			AttemptedRevision: rev,
			LiveStatus:        "last-known-good",
		},
	}
}

// pendingValidate records a target whose staged candidate failed validation.
func pendingValidate(id string, rev uint64, result validate.Result) TargetOutcome {
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
			TargetID:          id,
			Stage:             stage,
			Summary:           summary,
			AttemptedRevision: rev,
			LiveStatus:        "last-known-good",
		},
	}
}
