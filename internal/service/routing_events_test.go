package service

// Tests for the routing_changed event timeline. A quota-check reconcile that
// provably changes managed files records a routing_changed event for every
// provider whose ranking view moved (rank, eligibility, off-peak, or
// explanation), and refreshes every ranked provider's stored Routing.Decision
// baseline. Providers whose view did not move, transactions without proven
// file changes, and routing-disabled policies record nothing.

import (
	"context"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// routingDesired returns a routing-enabled desired policy with two quota-
// enabled mappings in one balance group, backed by the ranking test fixtures.
func routingDesired() policy.Desired {
	return qmap(true,
		rankMapping{id: "codex", bases: []string{"codex/model"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "default", Weight: 1}},
		rankMapping{id: "zai", bases: []string{"zai/model"}, quota: &policy.QuotaConfig{Adapter: "zai", BalanceGroup: "default", Weight: 1}},
	)
}

// provenOutcomes returns target outcomes carrying one target with a proven
// managed-file change, satisfying the history qualification gate.
func provenOutcomes() []TargetOutcome {
	return []TargetOutcome{{
		TargetID: "global",
		Prepare:  &PrepareResult{TargetID: "global", PlanComputed: true, ChangedFiles: map[string]bool{"desired.yaml": true}},
	}}
}

// unprovenOutcomes returns target outcomes whose preparation saw no changed
// files, so no routing_changed event may be emitted.
func unprovenOutcomes() []TargetOutcome {
	return []TargetOutcome{{
		TargetID: "global",
		Prepare:  &PrepareResult{TargetID: "global", PlanComputed: true, ChangedFiles: map[string]bool{}},
	}}
}

// rankedState returns observed state with two healthy quota snapshots: codex
// less depleted than zai, so codex ranks first.
func rankedState() state.State {
	return state.State{Revision: 7, Providers: map[string]state.ProviderState{
		"codex": pstate(qsnap(10, 100)),
		"zai":   pstate(qsnap(20, 100)),
	}}
}

// withDecision returns ps carrying a stored prior routing decision.
func withDecision(ps state.ProviderState, rank int, eligible bool, explanation string) state.ProviderState {
	ps.Routing.Decision = &state.RoutingDecision{Rank: rank, Eligible: eligible, Explanation: explanation, EvaluatedAt: rankNow}
	return ps
}

func eventForMapping(events []state.EventRecord, mappingID string) (state.EventRecord, bool) {
	for _, e := range events {
		if e.MappingID == mappingID {
			return e, true
		}
	}
	return state.EventRecord{}, false
}

func TestAppendRoutingChangeEventsEmitsDiffAndStoresBaseline(t *testing.T) {
	desired := routingDesired()
	s := rankedState()
	// codex carries a stale prior view; zai has never been ranked.
	s.Providers["codex"] = withDecision(s.Providers["codex"], 1, true, "stale explanation")

	_, ranking := ComputeRanking(desired, s, rankNow)
	codexEntry, ok := rankEntry(ranking, "codex")
	if !ok || !codexEntry.Eligible {
		t.Fatalf("codex should rank eligible: %+v", codexEntry)
	}
	zaiEntry, ok := rankEntry(ranking, "zai")
	if !ok || !zaiEntry.Eligible {
		t.Fatalf("zai should rank eligible: %+v", zaiEntry)
	}

	next := s
	appendRoutingChangeEvents(&next, desired, provenOutcomes(), rankNow)

	if len(next.EventHistory.Events) != 2 {
		t.Fatalf("event count = %d, want 2 (codex diff + zai first view): %+v", len(next.EventHistory.Events), next.EventHistory.Events)
	}

	codex, ok := eventForMapping(next.EventHistory.Events, "codex")
	if !ok {
		t.Fatal("missing codex routing_changed event")
	}
	if codex.Category != state.EventRoutingChange || codex.Action != "routing_changed" || codex.Result != state.EventChanged {
		t.Fatalf("codex event taxonomy: %+v", codex)
	}
	if codex.Reason != codexEntry.Explanation || codexEntry.Explanation == "" {
		t.Fatalf("codex reason = %q, want fresh ranking explanation %q", codex.Reason, codexEntry.Explanation)
	}
	if codex.OldRank == nil || *codex.OldRank != 1 {
		t.Fatalf("codex OldRank = %v, want 1", codex.OldRank)
	}
	if codex.NewRank == nil || *codex.NewRank != codexEntry.Rank {
		t.Fatalf("codex NewRank = %v, want %d", codex.NewRank, codexEntry.Rank)
	}
	if codex.OldEligible == nil || !*codex.OldEligible || codex.NewEligible == nil || !*codex.NewEligible {
		t.Fatalf("codex eligibility pointers: %+v", codex)
	}
	if codex.OldOffPeak == nil || *codex.OldOffPeak || codex.NewOffPeak == nil {
		t.Fatalf("codex off-peak pointers: %+v", codex)
	}

	zai, ok := eventForMapping(next.EventHistory.Events, "zai")
	if !ok {
		t.Fatal("missing zai first-view event")
	}
	if zai.OldRank != nil || zai.OldEligible != nil || zai.OldOffPeak != nil {
		t.Fatalf("zai first view must carry no old values: %+v", zai)
	}
	if zai.NewRank == nil || *zai.NewRank != zaiEntry.Rank || zai.NewEligible == nil || !*zai.NewEligible {
		t.Fatalf("zai new view pointers: %+v", zai)
	}

	// The stored baseline refreshed for every ranked provider with a state entry.
	for id, entry := range map[string]routing.RankEntry{"codex": codexEntry, "zai": zaiEntry} {
		d := next.Providers[id].Routing.Decision
		if d == nil {
			t.Fatalf("%s decision baseline missing", id)
		}
		if d.Rank != entry.Rank || !d.Eligible || d.Explanation != entry.Explanation || !d.EvaluatedAt.Equal(rankNow.UTC()) {
			t.Fatalf("%s decision baseline = %+v, want rank %d eligible with %q at %v", id, d, entry.Rank, entry.Explanation, rankNow.UTC())
		}
	}
}

func TestAppendRoutingChangeEventsNoEventWhenViewUnchanged(t *testing.T) {
	desired := routingDesired()
	next := rankedState()
	appendRoutingChangeEvents(&next, desired, provenOutcomes(), rankNow)
	seeded := len(next.EventHistory.Events)
	if seeded == 0 {
		t.Fatal("seed run should record first views")
	}

	// Same ranking view one minute later, with a proven file change: no new
	// events, but the baseline refreshes.
	refreshed := next
	appendRoutingChangeEvents(&refreshed, desired, provenOutcomes(), rankNow.Add(time.Minute))
	if len(refreshed.EventHistory.Events) != seeded {
		t.Fatalf("event count = %d, want %d (no change)", len(refreshed.EventHistory.Events), seeded)
	}
	d := refreshed.Providers["codex"].Routing.Decision
	if d == nil || !d.EvaluatedAt.Equal(rankNow.Add(time.Minute).UTC()) {
		t.Fatalf("codex baseline not refreshed: %+v", d)
	}
}

func TestAppendRoutingChangeEventsRequireProvenChange(t *testing.T) {
	desired := routingDesired()
	s := rankedState()
	s.Providers["codex"] = withDecision(s.Providers["codex"], 1, true, "stale explanation")

	next := s
	appendRoutingChangeEvents(&next, desired, unprovenOutcomes(), rankNow)

	if len(next.EventHistory.Events) != 0 {
		t.Fatalf("events emitted without proven file change: %+v", next.EventHistory.Events)
	}
	_, ranking := ComputeRanking(desired, s, rankNow)
	entry, _ := rankEntry(ranking, "codex")
	d := next.Providers["codex"].Routing.Decision
	if d == nil || d.Explanation != entry.Explanation {
		t.Fatalf("baseline not refreshed despite unchanged gate: %+v", d)
	}
}

func TestAppendRoutingChangeEventsNoopWhenRoutingDisabled(t *testing.T) {
	desired := routingDesired()
	desired.Routing = policy.RoutingConfig{}
	next := rankedState()

	appendRoutingChangeEvents(&next, desired, provenOutcomes(), rankNow)

	if len(next.EventHistory.Events) != 0 {
		t.Fatalf("events emitted with routing disabled: %+v", next.EventHistory.Events)
	}
	if next.Providers["codex"].Routing.Decision != nil {
		t.Fatalf("decision stored with routing disabled: %+v", next.Providers["codex"].Routing.Decision)
	}
}

// TestQuotaCheckReconcileRecordsRoutingDecisionBaseline verifies the transaction
// wiring: a quota check with reconcile stores every ranked provider's routing
// decision baseline, and — because the spy's synthetic staging proves no file
// change — emits no events.
func TestQuotaCheckReconcileRecordsRoutingDecisionBaseline(t *testing.T) {
	spy := newQuotaCheckSpy().withTargets("global", validTargetKey)
	spy.desired = routingDesired()
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 20)
	p.results["zai"] = freshSnap("zai", 40)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", true)

	if !out.Accepted {
		t.Fatalf("outcome: %+v", out)
	}
	saved := spy.LastSaved
	d := saved.Providers["codex"].Routing.Decision
	if d == nil || !d.Eligible {
		t.Fatalf("routing decision baseline not stored on the reconcile path: %+v", d)
	}
	if saved.Providers["zai"].Routing.Decision == nil {
		t.Fatal("zai routing decision baseline not stored")
	}
	if len(saved.EventHistory.Events) != 0 {
		t.Fatalf("events emitted without proven file change: %+v", saved.EventHistory.Events)
	}
}

func TestAppendRoutingChangeEventsSkipProvidersWithoutStateEntry(t *testing.T) {
	desired := routingDesired()
	s := rankedState()
	delete(s.Providers, "zai")

	next := s
	appendRoutingChangeEvents(&next, desired, provenOutcomes(), rankNow)

	if _, ok := eventForMapping(next.EventHistory.Events, "zai"); ok {
		t.Fatal("event emitted for ranked provider with no state entry")
	}
	if _, exists := next.Providers["zai"]; exists {
		t.Fatal("state entry fabricated for ranked provider")
	}
	codex, ok := eventForMapping(next.EventHistory.Events, "codex")
	if !ok {
		t.Fatal("missing codex routing_changed event")
	}
	if codex.Sequence == 0 {
		t.Fatalf("codex event sequence not assigned: %+v", codex)
	}
}
