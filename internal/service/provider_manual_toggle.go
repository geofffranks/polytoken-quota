package service

// Manual provider enable/disable under a provider-only policy: `routing
// disable <id>` and `routing enable <id>` write the exact
// providers.<id>.enabled managed field on the registered global target — the
// same managed field the automatic provider gate edits — through the same
// stage/validate/publish machinery: a real staging candidate, the bounded
// validation run, the single journaled global transaction, and the same
// ownership claim/release semantics the gate uses for its durable disabled
// axis. A manual disable records the durable manual-disable state axis and a
// disabled-axis ownership claim holding the operator's pre-toggle baseline, so
// the next gate pass treats the field as quota-held (restoring nothing), and a
// manual enable clears the axis, releases the claim, and restores the recorded
// baseline shape (an originally absent key is removed again, not set true).
//
// This transaction is deliberately standalone: it does not compose the gate's
// safety analyzer or input-snapshot preflight. In their place it re-reads the
// live global configuration immediately before publication and refuses when
// the planned pre-toggle field moved during the validation window — quota
// never publishes over an operator edit it did not plan against.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// transactProviderManualToggle implements routing disable/enable for a
// provider-only policy under the already-held transaction lock. The caller has
// already verified the mapping ID is non-empty and enrolled.
func (c *Coordinator) transactProviderManualToggle(ctx context.Context, observed state.State, in transactionInput, desired policy.Desired, kind transactionKind) Outcome {
	switch kind {
	case txDisable, txEnable:
	default:
		return Outcome{Accepted: false, Error: errors.New("service: unknown provider-only manual transition")}
	}
	c.step("load-sources")
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if len(targets) == 0 || !targets[0].Resolved.Global {
		return Outcome{Accepted: false, Error: errors.New("service: provider enable/disable requires a registered global target")}
	}
	global := targets[0]

	c.step("read-live-provider-field")
	configPath := filepath.Join(global.Resolved.CanonicalRoot, "config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return Outcome{Accepted: false, Error: fmt.Errorf("service: read registered global configuration: %w", err)}
	}
	present, value, known, err := providerEnabledField(raw, in.Provider)
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	if !known {
		return Outcome{Accepted: false, Error: fmt.Errorf("service: enrolled provider %q is absent from the registered global configuration", sanitizeFailure(in.Provider))}
	}

	now := c.now()
	var next state.State
	switch kind {
	case txDisable:
		c.step("manual-disable")
		next, err = state.SetManualDisabled(observed, []string{in.Provider}, true, now)
	case txEnable:
		c.step("manual-enable")
		next, err = state.SetManualDisabled(observed, []string{in.Provider}, false, now)
	}
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}

	record, hasRecord := observed.OwnershipOf(in.Provider)
	edit, wantPresent, wantValue := providerToggleEdit(kind, in.Provider, record, hasRecord)
	liveMatches := present == wantPresent && (!wantPresent || value == wantValue)
	stateChanged := state.ManualDisableChanged(observed, []string{in.Provider}, kind == txDisable)
	claimChanged := false
	if kind == txEnable {
		claimChanged = hasRecord // a manual enable always releases a held claim
	} else {
		// Any held disabled-axis claim makes a repeated disable a no-op; the
		// recorded baseline is the pre-toggle operator value, not the live
		// field, so it is not re-derived here.
		claimChanged = !hasRecord || !record.Owned || record.Axis != state.OwnershipAxisDisabled
	}
	if !stateChanged && liveMatches && !claimChanged {
		return Outcome{Accepted: true, HandledWithoutRevision: true, Revision: observed.Revision}
	}

	revision := observed.Revision + 1
	next.Revision = revision
	ownership := state.CloneProviderOwnership(observed.ProviderOwnership)
	if ownership == nil {
		ownership = map[string]state.ProviderOwnership{}
	}
	switch kind {
	case txDisable:
		// Claim exactly as the gate's durable disabled axis would: the
		// operator's pre-toggle shape is the recorded baseline.
		ownership[in.Provider] = state.ProviderOwnership{
			BaselinePresent: present,
			BaselineValue:   value,
			Owned:           true,
			Axis:            state.OwnershipAxisDisabled,
			EngagedRevision: revision,
		}
	case txEnable:
		delete(ownership, in.Provider)
	}
	next.ProviderOwnership = ownership

	plan := reconcile.Plan{TargetID: targetID(global), Revision: revision, Edits: []reconcile.FieldEdit{edit}, ProviderOnly: true}
	timeout := c.validationTimeout(desired)

	c.step("stage-provider-toggle")
	candidate, stageErr := c.Stage.Stage(ctx, global.Resolved, plan, nil)
	if stageErr != nil {
		return Outcome{Accepted: false, Error: fmt.Errorf("service: stage provider %s toggle for target %s: %w", toggleVerb(kind), sanitizeFailure(targetID(global)), stageErr)}
	}
	validation := c.Validate.Validate(ctx, candidate, timeout)
	if !validation.StartupValid {
		_ = candidate.Cleanup()
		reason := fmt.Errorf("service: staged validation refused the provider %s toggle for target %s", toggleVerb(kind), sanitizeFailure(targetID(global)))
		if validation.Error != nil {
			// Mirror pendingValidate: the sanitized CommandError summary
			// is the operator's "why" — stage alone is not diagnosable.
			reason = fmt.Errorf("service: staged validation refused the provider %s toggle for target %s at %s: %s", toggleVerb(kind), sanitizeFailure(targetID(global)), validation.Error.Stage, sanitizeFailure(validation.Error.Summary))
		}
		return Outcome{Accepted: false, Error: reason}
	}

	// The assessed input must still be the live input immediately before
	// publication: an operator edit during the validation window makes the
	// planned edit stale.
	c.step("recheck-live-provider-field")
	fresh, err := os.ReadFile(configPath)
	if err != nil {
		_ = candidate.Cleanup()
		return Outcome{Accepted: false, Error: fmt.Errorf("service: re-read registered global configuration: %w", err)}
	}
	if err := providerToggleInputCurrent(fresh, in.Provider, present, value); err != nil {
		_ = candidate.Cleanup()
		return Outcome{Accepted: false, Error: err}
	}

	c.step("publish-provider-toggle")
	stagedDir := candidate.PublishDir
	if stagedDir == "" {
		stagedDir = candidate.ConfigDir
	}
	var prep *PrepareResult
	if p, prepErr := BuildPrepareResult(targetID(global), plan, global.Resolved.CanonicalRoot, stagedDir); prepErr == nil {
		prep = &p
	}
	// The notice debt mirrors the gate: keep debt whose committed value still
	// matches the live field, and record this fresh committed edit so the
	// notice explains the new provider state.
	enabled := map[string]bool{}
	for _, id := range sortedProviderIDs(desired) {
		if p, v, k, err := providerEnabledField(fresh, id); err == nil && k {
			enabled[id] = !p || v
		}
	}
	// The committed value is the planned post-toggle field shape, not the
	// toggle direction: an enable that restores a held-off baseline commits
	// enabled=false, and a key removal commits default-enabled (true).
	enabled[in.Provider] = !wantPresent || wantValue
	reason := "manual routing disable"
	if kind == txEnable {
		reason = "manual routing enable"
	}
	next.PendingProviderNotice = reconcileProviderNoticeDebt(
		observed.PendingProviderNotice, enabled, revision,
		[]policyProviderEdit{{id: in.Provider, enabled: enabled[in.Provider], reason: reason}},
	)

	tx, err := c.buildTransaction(observed, next, global, plan, candidate, prep)
	tx.ProviderNoticeSet = true
	tx.ProviderNotice = next.PendingProviderNotice
	if err != nil {
		_ = candidate.Cleanup()
		return Outcome{Accepted: false, Error: fmt.Errorf("service: prepare provider %s toggle publication: %w", toggleVerb(kind), err)}
	}
	// ApplyUnderLock: the Coordinator already holds the transaction lock; the
	// publisher must NOT re-acquire it (flock LOCK_EX is not re-entrant).
	c.applyBackupRetention(desired.Operational.BackupCount)
	if _, err := c.Publish.ApplyUnderLock(ctx, tx); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: revision, Error: fmt.Errorf("service: publish provider %s toggle: %w", toggleVerb(kind), err)}
	}

	outcomes := providerGateAppliedOutcomes(targets, revision, prep)
	next = c.retireSyntheticPendings(next)
	next = c.recordTargetOutcomes(next, outcomes)
	next = appendManualEvent(next, observed, kind, in, now)
	c.recordHistoryIfQualified(&next, kind, in, outcomes, targets, desired)
	c.step("save-state")
	if err := c.State.Save(next); err != nil {
		return Outcome{Accepted: false, DurabilityFailure: true, Revision: next.Revision, Targets: outcomes, Error: err}
	}
	if c.notifyProviderGate(desired, &next, providerEdits(outcomes)) {
		_ = c.State.Save(next) // best-effort persist of notice bookkeeping
	}
	return Outcome{Accepted: true, Revision: next.Revision, Targets: outcomes}
}

// providerToggleEdit resolves the byte edit and the post-toggle field shape a
// manual toggle publishes. A disable always writes enabled: false. An enable
// restores the recorded baseline shape when quota holds a claim (an originally
// absent key is removed again), and writes enabled: true when quota holds
// nothing (the operator had held the field off themselves).
func providerToggleEdit(kind transactionKind, id string, record state.ProviderOwnership, hasRecord bool) (edit reconcile.FieldEdit, wantPresent, wantValue bool) {
	if kind == txDisable {
		off := false
		return providerFieldEdit(id, &off, false), true, false
	}
	if hasRecord && record.Owned && record.BaselinePresent {
		v := record.BaselineValue
		return providerFieldEdit(id, &v, false), true, v
	}
	if hasRecord && record.Owned && !record.BaselinePresent {
		return providerFieldEdit(id, nil, true), false, false
	}
	on := true
	return providerFieldEdit(id, &on, false), true, true
}

// providerToggleInputCurrent verifies the live providers.<id>.enabled field
// still matches the planned pre-toggle observation immediately before
// publication: an operator edit during the staging/validation window makes the
// planned edit stale, and quota never publishes over a change it did not plan
// against.
func providerToggleInputCurrent(config []byte, id string, wantPresent, wantValue bool) error {
	stale := fmt.Errorf("service: registered global configuration changed during validation; the provider %s toggle is stale and no provider field was changed", sanitizeFailure(id))
	present, value, known, err := providerEnabledField(config, id)
	if err != nil {
		return fmt.Errorf("service: re-read registered global providers section: %w", err)
	}
	if !known || present != wantPresent || (present && value != wantValue) {
		return stale
	}
	return nil
}

func toggleVerb(kind transactionKind) string {
	if kind == txEnable {
		return "enable"
	}
	return "disable"
}
