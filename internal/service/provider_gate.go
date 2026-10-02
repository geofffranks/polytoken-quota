package service

// Provider-only reconcile and recovery (approved plan task 4).
//
// A provider-only policy maps each enrolled provider's independent quota
// observation to an `enabled` action on the registered GLOBAL configuration
// only: reserve and disabled gate the provider off; normal restores only the
// quota-owned operator baseline (absent key, explicit false, or explicit true)
// recorded when quota claimed the field. Exact-span edits touch enrolled
// `providers.<id>.enabled` fields and nothing else; unrelated bytes are
// preserved by the document editor.
//
// Before any single global-file publication the COMBINED provider changes are
// evaluated: the composed candidate is staged and validated (config validate +
// doctor) for the global root and every registered project root — each project
// candidate sees the composed global layer, never its live bytes — and the
// input snapshots are re-verified immediately before publication. Whether a
// provider may be gated off is the signal/durable gate's decision alone: the
// real Polytoken binary is the validator of the composed configuration, and a
// rejected validation retains the stale active selection. Publication is exactly
// one global journal transaction whose Next state carries the intended
// provider-ownership snapshot, so roll-forward adopts claims atomically with
// the live bytes they describe.
//
// On refusal (staging/validation failure, snapshot change, publish error) no
// file is edited, no notice is published, the target outcome records a
// sanitized pending reason, and quota observations/history stay independent of
// the failed edit. New ownership claims never persist without the bytes that
// back them: only refreshed conflict markers persist on a refusal.
//
// This path runs entirely under the coordinator's transaction lock.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// providerRestore is one recorded operator baseline that normal mode restores:
// the exact original shape of `providers.<id>.enabled` (present with a value,
// or absent). Restoring an explicit-false baseline writes false; restoring an
// absent baseline removes the key. Quota never forces true.
type providerRestore struct {
	ID             string
	RestorePresent bool
	RestoreValue   bool
}

// providerGatePlan is the derived gating plan for one reconcile: the combined
// provider changes (disables and restores) as exact-span global edits, plus the
// ownership bookkeeping each publish outcome requires.
type providerGatePlan struct {
	// Disables lists the enrolled provider IDs gated off this pass, sorted.
	Disables []string
	// Restores lists the quota-owned baselines restored this pass, sorted by ID.
	Restores []providerRestore
	// Edits is the combined global config.yaml edit list (disables and
	// restores, provider-ID order). Exact span: each edit addresses
	// providers.<id>.enabled only.
	Edits []reconcile.FieldEdit
	// PublishedOwnership is the full intended ownership map carried by the
	// journal transaction when the gate publishes: observed records plus new
	// claims, releases, and refreshed conflict markers.
	PublishedOwnership map[string]state.ProviderOwnership
	// RefusalOwnership is the ownership map persisted when the gate refuses to
	// publish: the observed claims (never new claims, never releases) with
	// conflict markers refreshed from live evidence.
	RefusalOwnership map[string]state.ProviderOwnership
	// Conflicts lists providers whose quota-owned expectation no longer
	// matches the live field because of an operator edit. They are reported
	// pending and never overwritten.
	Conflicts []string
	// PoolSkips lists the balance groups whose pace axis was skipped this
	// pass by the all-hot pool rule, sorted.
	PoolSkips []string
	// GateSummary is the sanitized per-provider gate decision channel, one
	// row per enrolled provider, for outcome and verbose-trace surfacing.
	GateSummary []ProviderGateSummary
	// Changed reports whether the plan would edit any managed byte or mutate
	// any ownership record.
	Changed bool
	// Enabled records the effective committed enabled value for every enrolled
	// provider observed in the registered global config.
	Enabled map[string]bool
}

// providerGateRefusal is a refused gate evaluation. Stage names the refusal
// point (render, stage, validate, publish), TargetID the registered
// root that caused it (empty for a whole-plan refusal), Err the sanitized
// reason for the pending outcome, and Retained the dry-run keep-staging
// diagnostic roots keyed by target ID.
type providerGateRefusal struct {
	Stage    string
	TargetID string
	Err      error
	Retained map[string]string
}

// providerGateResult is the outcome of one gate evaluation: the per-target
// outcomes (applied or pending), the plan (its ownership maps drive the
// caller's next state on every path), and the refusal when evaluation did not
// complete.
type providerGateResult struct {
	Outcomes []TargetOutcome
	Plan     providerGatePlan
	Refusal  *providerGateRefusal
}

// transactProviderGateReconcile implements the reconcile transaction for a
// provider-only policy under the already-held lock. Dry-run evaluates the full
// gate (staged validation through the real Polytoken binary, snapshot recheck)
// but publishes and saves nothing.
func (c *Coordinator) transactProviderGateReconcile(ctx context.Context, observed state.State, in transactionInput, desired policy.Desired) Outcome {
	if in.KeepStaging && !in.DryRun {
		return Outcome{Accepted: false, Error: errors.New("service: --keep-staging requires --dry-run")}
	}
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	revision := observed.Revision
	if !in.DryRun {
		revision = observed.Revision + 1
	}
	c.step("provider-gate")
	res := c.runProviderGate(ctx, desired, observed, targets, revision, !in.DryRun, in.KeepStaging, in.Verbose)
	if in.DryRun {
		return Outcome{Accepted: true, Revision: observed.Revision, Targets: res.Outcomes}
	}
	next := observed
	next.Revision = revision
	if res.Refusal == nil {
		next.ProviderOwnership = res.Plan.PublishedOwnership
		// The pace gate's durable transitions ride the same commit as the
		// bytes they describe; a refused pass records nothing.
		next = appendSignalGateEvents(next, observed, res.Plan, revision, c.now())
	} else {
		next.ProviderOwnership = res.Plan.RefusalOwnership
	}
	// Keep unpublished notice debt while the enrolled providers still have
	// the committed enabled values it describes. Ownership metadata can move
	// independently (for example, a conflict marker refresh or release).
	freshEdits := providerEdits(res.Outcomes)
	if res.Refusal != nil {
		freshEdits = nil // A refusal cannot have committed fresh provider edits.
	}
	next.PendingProviderNotice = reconcileProviderNoticeDebt(observed.PendingProviderNotice, res.Plan.Enabled, revision, freshEdits)
	next = c.retireSyntheticPendings(next)
	next = c.recordTargetOutcomes(next, res.Outcomes)
	c.recordHistoryIfQualified(&next, txReconcile, in, res.Outcomes, targets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: res.Outcomes, Error: err}
	}
	if res.Refusal == nil && c.notifyProviderGate(desired, &next, providerEdits(res.Outcomes)) {
		_ = c.State.Save(next) // best-effort persist of notice bookkeeping
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: res.Outcomes}
}

// runProviderGate derives the gating plan from the observed state and the live
// global config, evaluates the combined changes safely, and — when publish is
// true and every gate passes — commits the single global journal transaction
// carrying the next ownership state. It never acquires the lock and never
// saves state: the caller owns both.
func (c *Coordinator) runProviderGate(ctx context.Context, desired policy.Desired, observed state.State, targets []RegisteredTarget, revision uint64, publish, keepStaging, verbose bool) (res providerGateResult) {
	res = providerGateResult{Plan: providerGatePlan{
		PublishedOwnership: state.CloneProviderOwnership(observed.ProviderOwnership),
		RefusalOwnership:   state.CloneProviderOwnership(observed.ProviderOwnership),
	}}
	globalPending := func(stage string, err error) {
		res.Refusal = &providerGateRefusal{Stage: stage, Err: err}
		res.Outcomes = providerGateRefusalOutcomes(targets, res.Refusal, revision)
	}
	if len(targets) == 0 || !targets[0].Resolved.Global {
		globalPending("render", errors.New("service: provider gating requires a registered global target"))
		return res
	}

	c.step("read-sources")
	// Snapshot every registered root's managed sources for the publish-time
	// input recheck, and read the global config bytes the gate plans against.
	var snapshots []ProviderSourceSnapshot
	var globalConfig []byte
	for i, rt := range targets {
		if i == 0 {
			cfg, readErr := os.ReadFile(filepath.Join(rt.Resolved.CanonicalRoot, "config.yaml"))
			if readErr != nil {
				globalPending("stage", fmt.Errorf("service: read registered root %s: %w", sanitizeFailure(rt.Resolved.ID), readErr))
				return res
			}
			globalConfig = cfg
		}
		grown, snapErr := appendProviderSnapshotsValue(snapshots, rt.Resolved.ID, rt.Resolved.CanonicalRoot)
		if snapErr != nil {
			globalPending("stage", snapErr)
			return res
		}
		snapshots = grown
	}

	c.step("signal-verdicts")
	// The signal verdicts are a pure function of the observed state at now,
	// computed once per pass from the coordinator's clock so the gate and the
	// rank can never disagree about freshness.
	now := c.now()
	verdicts := routing.SignalGateVerdicts(signalGateInputs(desired, observed), now)

	c.step("plan-provider-gate")
	plan, planErr := planProviderGate(desired, observed, globalConfig, now, verdicts, revision)
	if planErr != nil {
		res.Plan = plan
		globalPending("render", planErr)
		return res
	}
	res.Plan = plan
	// Whatever this evaluation produces — applied, pending, or a dry-run
	// report — every outcome carries the sanitized per-provider gate summary,
	// and --verbose adds the decision trace. Dry runs report the same rows
	// without publishing anything.
	defer func() {
		for i := range res.Outcomes {
			res.Outcomes[i].ProviderGates = plan.GateSummary
			if verbose {
				res.Outcomes[i].Trace = &ReconcileTrace{
					ProviderModes: providerDetailsToReports(ProjectProviders(desired, observed)),
					ProviderGates: plan.GateSummary,
				}
			}
		}
	}()

	if len(plan.Conflicts) > 0 {
		// An operator edit diverged from a held claim: report pending and
		// publish nothing. Claims stay held (with refreshed conflict markers
		// persisted via RefusalOwnership); quota never writes around an
		// operator's change.
		quoted := make([]string, 0, len(plan.Conflicts))
		for _, id := range plan.Conflicts {
			quoted = append(quoted, fmt.Sprintf("%q", sanitizeFailure(id)))
		}
		globalPending("conflict", fmt.Errorf("service: provider gating pending on operator edit conflict for %s; no provider field was changed", strings.Join(quoted, ", ")))
		return res
	}

	c.step("evaluate-provider-gate")
	candidate, evalRefusal := c.evaluateProviderGate(ctx, desired, targets, snapshots, plan, revision, keepStaging)
	if evalRefusal != nil {
		res.Refusal = evalRefusal
		res.Outcomes = providerGateRefusalOutcomes(targets, evalRefusal, revision)
		return res
	}
	defer func() { _ = candidate.Cleanup() }()

	global := targets[0]
	txPlan := reconcile.Plan{TargetID: targetID(global), Revision: revision, Edits: plan.Edits, ProviderOnly: true}
	stagedDir := candidate.PublishDir
	if stagedDir == "" {
		stagedDir = candidate.ConfigDir
	}
	var prep *PrepareResult
	if p, prepErr := BuildPrepareResult(targetID(global), txPlan, global.Resolved.CanonicalRoot, stagedDir); prepErr == nil {
		prep = &p
	}

	if !publish {
		res.Outcomes = providerGateAppliedOutcomes(targets, revision, prep)
		return res
	}

	c.step("publish-provider-gate")
	if err := c.publishProviderGate(ctx, desired, observed, global, plan, txPlan, revision, candidate); err != nil {
		res.Refusal = &providerGateRefusal{Stage: "publish", Err: err}
		res.Outcomes = providerGateRefusalOutcomes(targets, res.Refusal, revision)
		return res
	}
	res.Outcomes = providerGateAppliedOutcomes(targets, revision, prep)
	return res
}

// appendProviderSnapshotsValue snapshots every managed source of one root and
// returns the grown snapshot slice. appendProviderSnapshots mutates a
// *ProviderPreflight in place; this wrapper keeps the slice ownership explicit
// for the gate path.
func appendProviderSnapshotsValue(snapshots []ProviderSourceSnapshot, id, root string) ([]ProviderSourceSnapshot, error) {
	holder := &ProviderPreflight{Snapshots: snapshots}
	if err := appendProviderSnapshots(holder, id, root); err != nil {
		return snapshots, err
	}
	return holder.Snapshots, nil
}

// evaluateProviderGate runs the two write gates over the combined plan:
// staged config-validate+doctor for the global root and every project root
// with the composed global layer, and the input-snapshot recheck. On success
// it returns the validated GLOBAL candidate (cleanup is the caller's
// responsibility); on refusal the candidate is empty and every staged
// artifact has been removed (except dry-run keep-staging retention, reported
// in the refusal).
func (c *Coordinator) evaluateProviderGate(ctx context.Context, desired policy.Desired, targets []RegisteredTarget, snapshots []ProviderSourceSnapshot, plan providerGatePlan, revision uint64, keepStaging bool) (staging.Candidate, *providerGateRefusal) {
	// Gate 1: staged validation of the composed candidate on every registered
	// root. The global candidate is kept alive for publication; every project
	// candidate is cleaned after its validation. Plans carry ProviderOnly so
	// staging publishes the raw global bytes plus the exact edits.
	timeout := c.validationTimeout(desired)
	globalPlan := reconcile.Plan{TargetID: targetID(targets[0]), Revision: revision, Edits: plan.Edits, ProviderOnly: true}
	var globalCandidate staging.Candidate
	for i, rt := range targets {
		p := reconcile.Plan{TargetID: targetID(rt), Revision: revision, ProviderOnly: true}
		var projectGlobal *reconcile.Plan
		if i == 0 {
			p = globalPlan
		} else {
			projectGlobal = &globalPlan
		}
		candidate, stageErr := c.Stage.Stage(ctx, rt.Resolved, p, projectGlobal)
		if stageErr != nil {
			if globalCandidate.Root != "" {
				_ = globalCandidate.Cleanup()
			}
			return staging.Candidate{}, &providerGateRefusal{Stage: "stage", TargetID: targetID(rt), Err: fmt.Errorf("service: stage provider gate target %s: %w", sanitizeFailure(targetID(rt)), stageErr)}
		}
		if i == 0 {
			globalCandidate = candidate
		}
		validation := c.Validate.Validate(ctx, candidate, timeout)
		if !validation.StartupValid {
			reason := fmt.Errorf("service: staged validation refused provider gating for target %s", sanitizeFailure(targetID(rt)))
			if validation.Error != nil {
				// Mirror pendingValidate: the sanitized CommandError summary
				// is the operator's "why" — stage alone is not diagnosable.
				reason = fmt.Errorf("service: staged validation refused provider gating for target %s at %s: %s", sanitizeFailure(targetID(rt)), validation.Error.Stage, sanitizeFailure(validation.Error.Summary))
			}
			retained := map[string]string{}
			if keepStaging {
				if retainedRoot, retainErr := candidate.Retain(); retainErr == nil {
					retained[targetID(rt)] = retainedRoot
				} else {
					retained[targetID(rt)] = candidate.Root
				}
			} else {
				_ = candidate.Cleanup()
			}
			if globalCandidate.Root != "" && globalCandidate.Root != candidate.Root {
				_ = globalCandidate.Cleanup()
			}
			return staging.Candidate{}, &providerGateRefusal{Stage: "validate", TargetID: targetID(rt), Err: reason, Retained: retained}
		}
		if i > 0 {
			_ = candidate.Cleanup()
		}
	}

	// Gate 2: the assessed input must still be the live input immediately
	// before publication.
	if err := RecheckProviderPreflight(ProviderPreflight{Snapshots: snapshots}); err != nil {
		_ = globalCandidate.Cleanup()
		return staging.Candidate{}, &providerGateRefusal{Stage: "publish", Err: err}
	}
	return globalCandidate, nil
}

// publishProviderGate commits the single global journal transaction. The
// transaction's Next state carries the intended ownership snapshot so journal
// roll-forward adopts claims atomically with the live bytes they describe.
// Project roots are never published: their files carry no quota-owned fields.
func (c *Coordinator) publishProviderGate(ctx context.Context, desired policy.Desired, observed state.State, global RegisteredTarget, plan providerGatePlan, txPlan reconcile.Plan, revision uint64, candidate staging.Candidate) error {
	stagedDir := candidate.PublishDir
	if stagedDir == "" {
		stagedDir = candidate.ConfigDir
	}
	var prep *PrepareResult
	if p, err := BuildPrepareResult(targetID(global), txPlan, global.Resolved.CanonicalRoot, stagedDir); err == nil {
		prep = &p
	}
	next := observed
	next.Revision = revision
	next.ProviderOwnership = plan.PublishedOwnership
	// The provider edit and its unpublished notice debt are one journaled
	// outcome. Recovery must not roll forward bytes without also preserving
	// the notice state when the caller's subsequent state save fails.
	next.PendingProviderNotice = reconcileProviderNoticeDebt(
		observed.PendingProviderNotice, plan.Enabled, revision, providerPlanNoticeEdits(plan),
	)
	tx, err := c.buildTransaction(observed, next, global, txPlan, candidate, prep)
	tx.ProviderNoticeSet = true
	tx.ProviderNotice = next.PendingProviderNotice
	if err != nil {
		return fmt.Errorf("service: prepare provider gate publication: %w", err)
	}
	// ApplyUnderLock: the Coordinator already holds the transaction lock; the
	// publisher must NOT re-acquire it (flock LOCK_EX is not re-entrant).
	c.applyBackupRetention(desired.Operational.BackupCount)
	if _, err := c.Publish.ApplyUnderLock(ctx, tx); err != nil {
		return fmt.Errorf("service: publish provider gate: %w", err)
	}
	return nil
}

// planProviderGate derives the per-provider actions from the enrolled set, the
// observed quota state, the durable ownership records, the live global config
// bytes, the signal verdicts, and the evaluation time. It is a pure decision
// function: it reads bytes handed to it and never touches the filesystem.
// now is the time the verdicts were computed at, threaded per the documented
// signature; the verdicts have already folded it in.
//
// Precedence per provider: the durable reserve/disabled axes gate first
// (unchanged); then the signal axis — fresh use-it-or-lose-it evidence at or
// below the configured threshold that survived the all-hot pool rule — gates
// with a signal-attributed claim; then the normal branch restores only a
// quota-owned baseline. An operator edit that diverges from a held claim is a
// conflict: reported pending, never overwritten, no forced value. When the
// live field already equals the recorded baseline exactly, the claim is
// released without an edit — the operator restored it themselves. Because a
// signal gate holds only while its evidence stays fresh and at/below
// threshold, every degraded-evidence case (surplus signal, stale snapshot,
// uncomputable signal) falls to the normal branch and releases a held signal
// gate automatically.
func planProviderGate(desired policy.Desired, observed state.State, globalConfig []byte, now time.Time, verdicts map[string]routing.SignalGateVerdict, revision uint64) (providerGatePlan, error) {
	plan := providerGatePlan{
		PublishedOwnership: state.CloneProviderOwnership(observed.ProviderOwnership),
		RefusalOwnership:   state.CloneProviderOwnership(observed.ProviderOwnership),
	}
	// claimTarget returns the ownership map to write, initializing it lazily:
	// fresh states carry no ownership map, and a new claim must materialize
	// one on the publish path without inventing records on the refusal path.
	claimTarget := func(refusal bool) map[string]state.ProviderOwnership {
		if refusal {
			if plan.RefusalOwnership == nil {
				plan.RefusalOwnership = map[string]state.ProviderOwnership{}
			}
			return plan.RefusalOwnership
		}
		if plan.PublishedOwnership == nil {
			plan.PublishedOwnership = map[string]state.ProviderOwnership{}
		}
		return plan.PublishedOwnership
	}
	ids := sortedProviderIDs(desired)
	plan.Enabled = make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return plan, nil
	}
	// Read every enrolled provider's live enabled field before any per-provider
	// decision: the pool rule must see whole pools, and a partial snapshot is
	// never authoritative for debt retirement.
	type liveField struct{ present, value bool }
	live := make(map[string]liveField, len(ids))
	for _, id := range ids {
		present, value, known, err := providerEnabledField(globalConfig, id)
		if err != nil {
			plan.Enabled = nil
			return plan, err
		}
		if !known {
			plan.Enabled = nil
			return plan, fmt.Errorf("service: enrolled provider %q is absent from the registered global configuration", sanitizeFailure(id))
		}
		live[id] = liveField{present: present, value: value}
		plan.Enabled[id] = !present || value
	}
	// Effective modes drive both the durable axes and the pool rule. The
	// quota-gate exemption set rides along so the pool rule's coverage and
	// release uses see it (reconcile.MappingMode already clamped the exempt
	// members' modes).
	modes := make(map[string]state.Mode, len(ids))
	exempt := make(map[string]bool, len(ids))
	signalHeld := make(map[string]bool, len(ids))
	for _, id := range ids {
		modes[id] = reconcile.MappingMode(desired, observed, policy.MappingID(id))
		exempt[id] = reconcile.QuotaGateExempt(desired.Providers[policy.MappingID(id)])
		if record, ok := observed.OwnershipOf(id); ok && record.SignalHeld() {
			signalHeld[id] = true
		}
	}
	pool := signalPoolRule(desired, verdicts, plan.Enabled, modes, signalHeld, exempt)
	plan.PoolSkips = pool.Pools
	poolSkipDetail := "pool fully overdrawn this pass; signal gate skipped"
	operatorHeldDetail := "operator holds the provider off"

	for _, id := range ids {
		lf := live[id]
		record, hasRecord := observed.OwnershipOf(id)
		owned := hasRecord && record.Owned
		mode := modes[id]
		verdict := verdicts[id] // zero value: no verdict means never gate
		gateEnabled, gateThreshold := signalGateConfigOf(desired, id)
		// The signal axis engages only on fresh at-or-below-threshold evidence
		// that the pool rule did not suppress this pass, and never on a
		// keep_enabled provider: the operator has marked it spared from
		// automatic disabling (its recovery enable path is unaffected).
		gated := gateEnabled && verdict.Gated && !pool.SkipSignal[id] && !desired.Providers[policy.MappingID(id)].KeepEnabled

		summary := ProviderGateSummary{Provider: id}
		if verdict.Signal != nil {
			s := *verdict.Signal
			summary.Signal = &s
		}
		thresholdCopy := gateThreshold
		summary.Threshold = &thresholdCopy
		appendSummary := func() {
			plan.GateSummary = append(plan.GateSummary, summary)
		}

		// The gate consumes the reconciler's single mode derivation: the
		// durable axes plus the fail-closed snapshot boundary, with the
		// quota-gate exemption's per-cause clamp already applied. A
		// non-exempt reserve remains the existing low-quota gating axis. An
		// exempt provider's clamped reserve is NOT a gating axis: it flows
		// to the normal branch below, which restores any claim the quota
		// gate had held — the affirmative restore duty — and never plans a
		// disable. An exempt provider at mode disabled (a manual disable or
		// a corrupted observation) keeps the durable branch exactly as a
		// non-exempt provider would.
		if mode == state.ModeDisabled || (mode == state.ModeReserve && !exempt[id]) {
			axis := state.OwnershipAxisReserve
			summary.Axis = string(axis)
			if mode == state.ModeDisabled {
				axis = state.OwnershipAxisDisabled
				summary.Axis = string(axis)
			}
			if owned && !(lf.present && !lf.value) {
				// Quota holds an expected-off claim but the live field moved
				// (enabled true, or the key was removed — absence enables).
				// Report pending; never re-disable, never overwrite.
				plan.Conflicts = append(plan.Conflicts, id)
				plan.Changed = plan.Changed || !record.Conflict
				record.Conflict = true
				claimTarget(false)[id] = record
				claimTarget(true)[id] = record
				summary.Action = GateActionConflict
				summary.Detail = "operator edit diverges from the held claim"
				appendSummary()
				continue
			}
			if owned {
				// Claim intact: already off, nothing to write. Refresh a stale
				// conflict marker — live evidence shows the claim holds again.
				if record.Conflict {
					record.Conflict = false
					claimTarget(false)[id] = record
					claimTarget(true)[id] = record
					plan.Changed = true
				}
				summary.Action = GateActionHeld
				appendSummary()
				continue
			}
			if lf.present && !lf.value {
				// The operator already holds it off; quota claims nothing.
				summary.Action = GateActionUnchanged
				summary.Detail = operatorHeldDetail
				appendSummary()
				continue
			}
			// Write the disable and claim the operator baseline.
			plan.Disables = append(plan.Disables, id)
			off := false
			plan.Edits = append(plan.Edits, providerFieldEdit(id, &off, false))
			claimTarget(false)[id] = state.ProviderOwnership{
				BaselinePresent: lf.present,
				BaselineValue:   lf.value,
				Owned:           true,
				Axis:            axis,
				EngagedRevision: revision,
			}
			plan.Changed = true
			summary.Action = GateActionDisabled
			appendSummary()
			continue
		}

		// normal mode: the signal axis takes precedence over baseline restore.
		if gated {
			summary.Axis = state.OwnershipAxisSignal
			if owned && !(lf.present && !lf.value) {
				// The operator re-enabled a provider a signal claim holds off:
				// identical conflict handling to the durable axes.
				plan.Conflicts = append(plan.Conflicts, id)
				plan.Changed = plan.Changed || !record.Conflict
				record.Conflict = true
				claimTarget(false)[id] = record
				claimTarget(true)[id] = record
				summary.Action = GateActionConflict
				summary.Detail = "operator edit diverges from the held claim"
				appendSummary()
				continue
			}
			if owned {
				if record.SignalHeld() && record.Threshold == gateThreshold {
					// Signal claim intact with the same threshold: nothing to
					// write. Refresh a stale conflict marker like the durable
					// axes do.
					if record.Conflict {
						record.Conflict = false
						claimTarget(false)[id] = record
						claimTarget(true)[id] = record
						plan.Changed = true
					}
					summary.Action = GateActionHeld
					appendSummary()
					continue
				}
				// Transfer attribution to the signal axis in place: the field
				// is already exactly as quota wrote it, so no byte changes and
				// the recorded operator baseline is preserved verbatim.
				changed := !record.SignalHeld() || record.Threshold != gateThreshold
				record.Axis = state.OwnershipAxisSignal
				record.Threshold = gateThreshold
				record.EngagedRevision = revision
				claimTarget(false)[id] = record
				claimTarget(true)[id] = record
				plan.Changed = plan.Changed || changed
				summary.Action = GateActionHeld
				if changed {
					summary.Detail = "gate attribution moved to the signal axis"
				}
				appendSummary()
				continue
			}
			if lf.present && !lf.value {
				summary.Action = GateActionUnchanged
				summary.Detail = operatorHeldDetail
				appendSummary()
				continue
			}
			// Write the disable and claim the operator baseline for signal.
			plan.Disables = append(plan.Disables, id)
			off := false
			plan.Edits = append(plan.Edits, providerFieldEdit(id, &off, false))
			claimTarget(false)[id] = state.ProviderOwnership{
				BaselinePresent: lf.present,
				BaselineValue:   lf.value,
				Owned:           true,
				Axis:            state.OwnershipAxisSignal,
				Threshold:       gateThreshold,
				EngagedRevision: revision,
			}
			plan.Changed = true
			summary.Action = GateActionDisabled
			// The signal reason rides the summary so the durable signal_gated
			// event (and any notice) carries why the gate engaged.
			if verdict.Reason != "" {
				summary.Detail = verdict.Reason
			}
			appendSummary()
			continue
		}

		// normal branch: the signal axis did not gate this pass.
		if !owned {
			summary.Action = GateActionUnchanged
			if pool.SkipSignal[id] {
				summary.Action = GateActionPoolSkip
				summary.Detail = poolSkipDetail
			}
			appendSummary()
			continue
		}
		if lf.present && !lf.value {
			// Still exactly as quota wrote it: restore the recorded operator
			// baseline in its original shape.
			if record.BaselinePresent {
				v := record.BaselineValue
				plan.Edits = append(plan.Edits, providerFieldEdit(id, &v, false))
			} else {
				plan.Edits = append(plan.Edits, providerFieldEdit(id, nil, true))
			}
			plan.Restores = append(plan.Restores, providerRestore{ID: id, RestorePresent: record.BaselinePresent, RestoreValue: record.BaselineValue})
			delete(plan.PublishedOwnership, id)
			plan.Changed = true
			summary.Action = GateActionRestored
			if pool.SkipSignal[id] {
				summary.Detail = poolSkipDetail
			} else if verdict.Reason != "" {
				summary.Detail = verdict.Reason
			}
			appendSummary()
			continue
		}
		if lf.present == record.BaselinePresent && (!lf.present || lf.value == record.BaselineValue) {
			// The operator already restored the exact baseline themselves:
			// release the claim without editing.
			delete(plan.PublishedOwnership, id)
			if record.Conflict {
				record.Conflict = false
				claimTarget(true)[id] = record
				plan.Changed = true
			}
			summary.Action = GateActionReleased
			if pool.SkipSignal[id] {
				summary.Detail = poolSkipDetail
			} else if verdict.Reason != "" {
				summary.Detail = verdict.Reason
			}
			appendSummary()
			continue
		}
		// Operator moved the field somewhere the recorded baseline does not
		// describe: report pending, restore nothing, force nothing.
		plan.Conflicts = append(plan.Conflicts, id)
		plan.Changed = plan.Changed || !record.Conflict
		record.Conflict = true
		claimTarget(false)[id] = record
		claimTarget(true)[id] = record
		summary.Action = GateActionConflict
		summary.Detail = "operator edit diverges from the held claim"
		appendSummary()
	}
	return plan, nil
}

// signalGateConfigOf resolves a provider's signal-gate configuration from the
// desired policy: an absent quota section (or signal_gate block) means the
// documented defaults — gating on at DefaultSignalGateThreshold.
func signalGateConfigOf(desired policy.Desired, id string) (enabled bool, threshold float64) {
	sg := policy.DefaultSignalGate()
	if m, ok := desired.Providers[policy.MappingID(id)]; ok && m.Quota != nil {
		sg = m.Quota.SignalGate.Resolved()
	}
	return !sg.Disabled, sg.Threshold
}

// providerFieldEdit addresses one exact-span providers.<id>.enabled edit in
// the global config.yaml. When remove is true the key is deleted (restoring an
// absent baseline); otherwise the boolean value is written.
func providerFieldEdit(id string, value *bool, remove bool) reconcile.FieldEdit {
	return reconcile.FieldEdit{
		File:    "config.yaml",
		Path:    []string{"providers", id, "enabled"},
		Enabled: value,
		Remove:  remove,
	}
}

// providerEnabledField reads the live `providers.<id>.enabled` field from the
// global config bytes. present/value describe the key; known is false only
// when the provider's own block is missing entirely (an enrolled ID the
// registered global config does not describe — refused, never invented).
//
// The read is strict about duplicate keys inside the providers section: the
// lenient decoder resolves duplicates last-wins, and ownership bookkeeping
// (claim releases, conflict markers) must never be derived from ambiguous
// bytes, so any duplicated key under `providers` is a refusal.
func providerEnabledField(config []byte, id string) (present, value, known bool, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(config, &root); err != nil {
		return false, false, false, fmt.Errorf("service: read registered global providers section: %w", err)
	}
	providers := documentNodeChild(&root, "providers")
	if providers == nil {
		return false, false, false, nil
	}
	if dup := firstDuplicateKey(providers); dup != "" {
		return false, false, false, fmt.Errorf("service: registered global providers section has duplicate key %q; the field is ambiguous", sanitizeFailure(dup))
	}
	var block struct {
		Enabled *bool `yaml:"enabled"`
	}
	entry := documentNodeChild(providers, id)
	if entry == nil {
		return false, false, false, nil
	}
	if err := entry.Decode(&block); err != nil {
		return false, false, false, fmt.Errorf("service: read registered global providers section: %w", err)
	}
	if block.Enabled == nil {
		return false, false, true, nil
	}
	return true, *block.Enabled, true, nil
}

// documentNodeChild returns the value node of key in a mapping node, or nil
// when the document is not a mapping carrying that key. A decoded document
// node descends into its single content node first.
func documentNodeChild(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return documentNodeChild(n.Content[0], key)
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// firstDuplicateKey walks a mapping subtree and returns the first repeated
// mapping key it finds (yaml.v3 keeps duplicate keys as successive Content
// pairs), or "" when every mapping in the subtree has unique keys.
func firstDuplicateKey(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k == nil || k.Kind != yaml.ScalarNode {
				continue
			}
			if seen[k.Value] {
				return k.Value
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if dup := firstDuplicateKey(c); dup != "" {
			return dup
		}
	}
	return ""
}

// providerGateRefusalOutcomes projects a refused evaluation onto every
// registered target: the named root (when any) carries its own reason; every
// other target carries the publication-withheld reason. Nothing is reported
// applied.
func providerGateRefusalOutcomes(targets []RegisteredTarget, refusal *providerGateRefusal, revision uint64) []TargetOutcome {
	out := make([]TargetOutcome, 0, len(targets))
	for _, rt := range targets {
		id := targetID(rt)
		reason := refusal.Err
		if refusal.TargetID != "" && id != refusal.TargetID {
			reason = fmt.Errorf("%w (publication withheld: target %s failed %s)", refusal.Err, sanitizeFailure(refusal.TargetID), sanitizeFailure(refusal.Stage))
		}
		outcome := pendingOutcome(id, revision, refusal.Stage, reason)
		if root, ok := refusal.Retained[id]; ok {
			outcome.StagingRoot = root
		}
		out = append(out, outcome)
	}
	if len(out) == 0 {
		// No registered targets to record on: surface a synthetic global
		// pending so the refusal is never silent.
		out = append(out, pendingOutcome("global", revision, refusal.Stage, refusal.Err))
	}
	return out
}

// providerGateAppliedOutcomes records every registered target reconciled clean
// at the revision. The global target carries the preparation result so history
// qualification sees the proven managed-file change.
func providerGateAppliedOutcomes(targets []RegisteredTarget, revision uint64, prep *PrepareResult) []TargetOutcome {
	out := make([]TargetOutcome, 0, len(targets))
	for i, rt := range targets {
		o := appliedOutcome(targetID(rt), revision)
		if i == 0 {
			o.Prepare = prep
		}
		out = append(out, o)
	}
	return out
}
