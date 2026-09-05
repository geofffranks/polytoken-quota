package service

// Routing-change event recording. A quota-check reconcile that provably
// changes managed files appends a routing_changed event for every provider
// whose ranking view moved (rank, eligibility, off-peak, or explanation), and
// refreshes each ranked provider's stored Routing.Decision baseline. This
// restores the automatic routing-change timeline entries for the automatic
// peak/pace path, reattached to the proven-change qualification gate so the
// timeline only records changes that actually reached managed files.

import (
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// appendRoutingChangeEvents diffs the fresh ranking view against each ranked
// provider's stored Routing.Decision. Every ranked provider with an existing
// state entry has its baseline refreshed, regardless of qualification; the
// moved providers also receive a routing_changed event, but only when the
// transaction carries at least one proven managed-file change. A routing-
// disabled policy records nothing. Ranked mappings without a state entry are
// skipped — no observation means no routing bookkeeping to update. It mutates
// s in place and must run before the sole state commit.
func appendRoutingChangeEvents(s *state.State, desired policy.Desired, outcomes []TargetOutcome, now time.Time) {
	if !desired.Routing.Enabled {
		return
	}
	_, ranking := ComputeRanking(desired, *s, now)
	proven := HasProvenChangeAcrossTargets(prepareResultsOf(outcomes))
	at := now.UTC()
	for _, entry := range ranking.Entries {
		ps, ok := s.Providers[entry.MappingID]
		if !ok {
			continue
		}
		fresh := state.RoutingDecision{
			Rank:        entry.Rank,
			Eligible:    entry.Eligible,
			OffPeak:     entry.OffPeak,
			Explanation: entry.Explanation,
			EvaluatedAt: at,
		}
		prior := ps.Routing.Decision
		changed := routingDecisionMoved(prior, fresh)
		ps.Routing.Decision = &fresh
		s.Providers[entry.MappingID] = ps
		if !proven || !changed {
			continue
		}
		e := state.EventRecord{
			Sequence:    nextEventSequence(s),
			Revision:    s.Revision,
			Ordinal:     len(s.EventHistory.Events),
			At:          at,
			RecordedAt:  at,
			Category:    state.EventRoutingChange,
			Action:      "routing_changed",
			MappingID:   entry.MappingID,
			Result:      state.EventChanged,
			Reason:      entry.Explanation,
			Explanation: entry.Explanation,
			NewRank:     eventIntPtr(fresh.Rank),
			NewEligible: eventBoolPtr(fresh.Eligible),
			NewOffPeak:  eventBoolPtr(fresh.OffPeak),
		}
		if prior != nil {
			e.OldRank = eventIntPtr(prior.Rank)
			e.OldEligible = eventBoolPtr(prior.Eligible)
			e.OldOffPeak = eventBoolPtr(prior.OffPeak)
		}
		s.EventHistory, _ = state.AppendEvent(s.EventHistory, e)
	}
}

// routingDecisionMoved reports whether the fresh ranking view differs from the
// stored prior decision. A nil prior (first recorded view) counts as moved.
func routingDecisionMoved(prior *state.RoutingDecision, fresh state.RoutingDecision) bool {
	return prior == nil ||
		prior.Rank != fresh.Rank ||
		prior.Eligible != fresh.Eligible ||
		prior.OffPeak != fresh.OffPeak ||
		prior.Explanation != fresh.Explanation
}

// prepareResultsOf collects the preparation results carried by target outcomes.
func prepareResultsOf(outcomes []TargetOutcome) []PrepareResult {
	preps := make([]PrepareResult, 0, len(outcomes))
	for i := range outcomes {
		if outcomes[i].Prepare != nil {
			preps = append(preps, *outcomes[i].Prepare)
		}
	}
	return preps
}

func eventIntPtr(v int) *int    { return &v }
func eventBoolPtr(v bool) *bool { return &v }
