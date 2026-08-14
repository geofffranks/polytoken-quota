package service

// Pure trigger projections and record-template construction for bounded
// reconcile history. These helpers convert the coordinator's typed
// transaction kind/input into a state-owned Trigger, and combine shared
// decision context with per-target preparation results into a bounded
// RecordTemplate with no completion timestamp or claimed runtime outcomes.
// Task 4 finalizes the template with outcomes, timestamp, and tier selection.

import (
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TriggerFromTransaction maps the coordinator's transaction kind and input
// into a typed state history trigger. The trigger records the transaction
// that initiated reconciliation, not an inference from resulting state.
func TriggerFromTransaction(kind transactionKind, in transactionInput) state.Trigger {
	switch kind {
	case txInit:
		return state.Trigger{Kind: state.TriggerInit}
	case txReconcile:
		return state.Trigger{Kind: state.TriggerReconcile}
	case txDisable:
		return state.Trigger{Kind: state.TriggerRoutingDisable, MappingID: in.Provider}
	case txEnable:
		return state.Trigger{Kind: state.TriggerRoutingEnable, MappingID: in.Provider}
	case txReset:
		return state.Trigger{Kind: state.TriggerRoutingReset}
	case txQuotaCheck:
		return state.Trigger{Kind: state.TriggerQuotaCheck, MappingID: in.Provider}
	case txSet:
		return state.Trigger{Kind: state.TriggerSet, Set: setEvidenceFromInput(in)}
	case txClear:
		return state.Trigger{Kind: state.TriggerClear, Clear: clearEvidenceFromInput(in)}
	default:
		return state.Trigger{}
	}
}

func setEvidenceFromInput(in transactionInput) *state.SetEvidence {
	e := &state.SetEvidence{Provider: in.Provider}
	if in.Patch.Quota != nil {
		q := *in.Patch.Quota
		e.Quota = &q
	}
	if in.Patch.Availability != nil {
		a := *in.Patch.Availability
		e.Availability = &a
	}
	return e
}

func clearEvidenceFromInput(in transactionInput) *state.ClearEvidence {
	return &state.ClearEvidence{
		Provider: in.Selector.Provider,
		All:      in.Selector.All,
	}
}

// TargetTemplateInput is the per-target data needed to project a template
// target entry. It combines the target's policy with its preparation result.
type TargetTemplateInput struct {
	Target       policy.Target
	TargetID     string
	PlanComputed bool
	ChangedEdits []reconcile.FieldEdit
}

// BuildRecordTemplate combines the typed trigger, shared decision context, and
// per-target preparation results into a bounded RecordTemplate. The template
// has no completion timestamp or claimed runtime outcomes — those are added
// by Task 4 finalization. Projections truncate to state-layer limits and
// record typed omitted counts.
func BuildRecordTemplate(
	trigger state.Trigger,
	revision uint64,
	desired policy.Desired,
	observed state.State,
	ranks reconcile.RankLookup,
	ranking routing.RankingResult,
	targets []TargetTemplateInput,
) state.RecordTemplate {
	// Shared decision context: providers and ranks are the same for all
	// targets in a revision.
	providers := ProjectProviders(desired, observed)
	rankDetails := ProjectRanks(ranking)

	provOmitted := 0
	if len(providers) > state.HistoryProvidersPerRecord {
		provOmitted = len(providers) - state.HistoryProvidersPerRecord
		providers = providers[:state.HistoryProvidersPerRecord]
	}
	rankOmitted := 0
	if len(rankDetails) > state.HistoryRanksPerRecord {
		rankOmitted = len(rankDetails) - state.HistoryRanksPerRecord
		rankDetails = rankDetails[:state.HistoryRanksPerRecord]
	}

	tplTargets := make([]state.TemplateTarget, 0, len(targets))
	var chainOmittedTotal, chainEntryOmittedTotal, editOmittedTotal int

	for _, ti := range targets {
		chains := ProjectChains(desired, observed, ti.Target, ranks)
		edits := ProjectEdits(ti.ChangedEdits, nil)

		chainOmitted := 0
		if len(chains) > state.HistoryChainsPerTarget {
			chainOmitted = len(chains) - state.HistoryChainsPerTarget
			chains = chains[:state.HistoryChainsPerTarget]
		}
		editOmitted := 0
		if len(edits) > state.HistoryEditsPerTarget {
			editOmitted = len(edits) - state.HistoryEditsPerTarget
			edits = edits[:state.HistoryEditsPerTarget]
		}
		chainOmittedTotal += chainOmitted
		chainEntryOmittedTotal += 0 // per-chain entry omission tracked inside ProjectChains/chainDetail
		editOmittedTotal += editOmitted

		tplTargets = append(tplTargets, state.TemplateTarget{
			ID:           ti.TargetID,
			PlanComputed: ti.PlanComputed,
			Chains:       chains,
			Edits:        edits,
			Omitted: state.TargetOmittedCounts{
				Chains: chainOmitted,
				Edits:  editOmitted,
			},
		})
	}

	return state.RecordTemplate{
		Revision:  revision,
		Trigger:   trigger,
		Providers: providers,
		Ranks:     rankDetails,
		Targets:   tplTargets,
		Omitted: state.RecordOmittedCounts{
			Providers: provOmitted,
			Ranks:     rankOmitted,
			Chains:    chainOmittedTotal,
			Edits:     editOmittedTotal,
		},
	}
}

// ShouldRecordHistory reports whether a transaction should produce a history
// record. Dry runs, unaccepted/rejected transactions, and transactions with
// no proven file change are excluded at this pure boundary.
func ShouldRecordHistory(dryRun, accepted bool, prepResults []PrepareResult) bool {
	if dryRun {
		return false
	}
	if !accepted {
		return false
	}
	if !HasProvenChangeAcrossTargets(prepResults) {
		return false
	}
	return true
}
