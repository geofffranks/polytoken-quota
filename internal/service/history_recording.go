package service

// History recording integration: after all targets are processed, this builds
// a typed record template, qualifies the transaction by proven file changes,
// finalizes the record with runtime outcomes, and appends it to the durable
// state history before the sole state commit.

import (
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// recordHistoryIfQualified builds a typed record template from the transaction
// context and per-target preparation results, qualifies it by proven managed-
// file byte changes, and — when qualified — finalizes and appends a durable
// ReconcileRecord to the state's ReconcileHistory. It mutates s in place.
// Dry runs, unaccepted/rejected transactions, and transactions with no proven
// change produce no history record.
func (c *Coordinator) recordHistoryIfQualified(
	s *state.State,
	kind transactionKind,
	in transactionInput,
	outcomes []TargetOutcome,
	targets []RegisteredTarget,
	desired policy.Desired,
) {
	if in.DryRun {
		return
	}

	// Collect preparation results from outcomes.
	prepResults := make([]PrepareResult, 0, len(outcomes))
	for i := range outcomes {
		if outcomes[i].Prepare != nil {
			prepResults = append(prepResults, *outcomes[i].Prepare)
		}
	}

	// Qualify: at least one target must have a proven file change.
	if !HasProvenChangeAcrossTargets(prepResults) {
		return
	}

	// Build the typed trigger from the transaction context.
	trigger := TriggerFromTransaction(kind, in)

	// Recompute ranking for shared decision context projection.
	ranks, rankingResult := ComputeRanking(desired, *s, c.now())

	// Build per-target template inputs by matching outcomes to targets.
	targetByID := make(map[string]policy.Target, len(targets))
	for _, rt := range targets {
		targetByID[targetID(rt)] = rt.Policy
	}

	tis := make([]TargetTemplateInput, 0, len(outcomes))
	for i := range outcomes {
		o := outcomes[i]
		ti := TargetTemplateInput{
			TargetID: o.TargetID,
		}
		if o.Prepare != nil {
			ti.PlanComputed = o.Prepare.PlanComputed
			ti.ChangedEdits = o.Prepare.ChangedEdits
		}
		if pol, ok := targetByID[o.TargetID]; ok {
			ti.Target = pol
		}
		tis = append(tis, ti)
	}

	// Build the bounded template.
	tpl := BuildRecordTemplate(trigger, s.Revision, desired, *s, ranks, rankingResult, tis)

	// Convert outcomes to CompactTarget list for finalization.
	compactOutcomes := make([]state.CompactTarget, 0, len(outcomes))
	for _, o := range outcomes {
		ct := state.CompactTarget{
			ID:      o.TargetID,
			Outcome: outcomeKind(o),
		}
		if o.Pending != nil {
			ct.Pending = compactPending(o.Pending)
		}
		compactOutcomes = append(compactOutcomes, ct)
	}

	// Finalize: attach runtime outcomes and completion timestamp, apply
	// tier selection and record ceiling.
	record, err := state.FinalizeHistoryRecord(tpl, compactOutcomes, c.now())
	if err != nil {
		return
	}

	// Append to durable state history.
	newHist, err := state.AppendHistory(s.ReconcileHistory, record)
	if err != nil {
		return
	}
	s.ReconcileHistory = newHist
}

// outcomeKind converts a TargetOutcome to a state TargetOutcomeKind.
func outcomeKind(o TargetOutcome) state.TargetOutcomeKind {
	if o.Pending != nil {
		return state.OutcomePending
	}
	return state.OutcomeApplied
}

// compactPending converts an ApplyFailure to a PendingDetail for compact
// target entries.
func compactPending(af *state.ApplyFailure) *state.PendingDetail {
	if af == nil {
		return nil
	}
	return &state.PendingDetail{
		Stage:   state.PendingStage(af.Stage),
		Summary: af.Summary,
	}
}
