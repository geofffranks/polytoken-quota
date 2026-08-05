package reconcile

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// These tests cover the routing ranking overlay: an opt-in, stable reordering of
// survivors within each partition by global rank, applied only when routing is
// enabled. When routing is disabled the output is byte-for-byte identical to the
// pre-routing behavior; the overlay never adds, removes, or resurrects survivors.

// routingFixture builds a Desired whose mappings are keyed by the provider prefix
// of each entry's base model, each carrying the given balance group (empty =
// default) in its optional QuotaConfig. It returns routing enabled/disabled per
// the flag plus a single-target definition chain of entries. Every provider is
// observed as normal (healthy).
func routingFixture(enabled bool, entries []string, groups map[string]string) (policy.Desired, state.State, policy.Target) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{}}
	for _, e := range entries {
		ref, err := ParseModelRef(e)
		if err != nil {
			panic(err)
		}
		mid := policy.MappingID(providerOf(ref.Base))
		m, ok := d.Providers[mid]
		if !ok {
			m = policy.Mapping{
				CodexBarProviders:  []string{string(mid)},
				PolytokenProviders: []string{string(mid)},
				Models:             map[string]policy.ModelBaseline{},
			}
		}
		if _, dup := m.Models[ref.Base]; !dup {
			m.Models[ref.Base] = policy.ModelBaseline{Enabled: true, HadEnabledKey: false}
		}
		if g, want := groups[string(mid)]; want || g != "" {
			m.Quota = &policy.QuotaConfig{Adapter: string(mid), FreshnessTTL: 30 * time.Minute, BalanceGroup: g, Weight: 1}
		}
		d.Providers[mid] = m
	}
	if enabled {
		d.Routing = policy.RoutingConfig{Enabled: true}
	}
	chain := append(policy.Chain(nil), entries...)
	target := policy.Target{ID: "t", Root: "/r", Definitions: []policy.Definition{{Path: "agent.md", Chain: chain}}}
	return d, state.State{Revision: 7}, target
}

// TestRoutingOverlayNeverAddsDesiredChainOnlyCandidates proves the overlay only
// reorders survivors drawn from the desired chain; it never injects a model not
// present in the chain.
func TestRoutingOverlayNeverAddsDesiredChainOnlyCandidates(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
	ranks := RankLookup{"a": 2, "b": 0, "c": 1}
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	allowed := map[string]bool{"a/x": true, "b/x": true, "c/x": true}
	if len(got) != len(allowed) {
		t.Fatalf("overlay changed survivor count: got=%v", got)
	}
	for _, m := range got {
		if !allowed[m] {
			t.Fatalf("overlay injected %q (not in desired chain)", m)
		}
	}
	// Ranked within the same group -> sorted best (lowest rank) first: b(0),c(1),a(2).
	want := []string{"b/x", "c/x", "a/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestRoutingOverlayProviderGrouping proves entries sharing a balance group are
// reordered by rank, while entries in a different group keep their relative
// order (not interleaved with the ranked group).
func TestRoutingOverlayProviderGrouping(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x", "d/x"}
	// a,b in group "g1"; c,d in group "g2" (unranked relative to g1).
	groups := map[string]string{"a": "g1", "b": "g1", "c": "g2", "d": "g2"}
	d, s, target := routingFixture(true, entries, groups)
	// Rank only g1 members; c,d absent from the lookup keep their order.
	ranks := RankLookup{"a": 5, "b": 1}
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	// g1 reordered: b(rank1) before a(rank5). g2 untouched, relative order and
	// position vs g1 preserved (stable sort only reorders within-group pairs).
	want := []string{"b/x", "a/x", "c/x", "d/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestRoutingOverlaySameMappingPreservation proves two chain entries resolving to
// the same mapping keep their relative order (the comparator never returns true
// for equal mid, so a stable sort preserves it).
func TestRoutingOverlaySameMappingPreservation(t *testing.T) {
	entries := []string{"a/x", "a/y", "b/x"}
	d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1"})
	ranks := RankLookup{"a": 1, "b": 0}
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	// b(0) ranks better than a(1), so b sorts first; the two a-entries keep their
	// relative order (x before y).
	want := []string{"b/x", "a/x", "a/y"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestRoutingOverlayMissingRankPreservation proves a mapping absent from the rank
// lookup keeps its position; it is never reordered relative to others.
func TestRoutingOverlayMissingRankPreservation(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
	ranks := RankLookup{"a": 1, "b": 0} // c absent from lookup
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	// b(0) and a(1) reorder ahead of c; c is unranked so it stays after both in
	// its original relative position (it was last originally, stays last).
	want := []string{"b/x", "a/x", "c/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestRoutingOverlayEventDisablePrecedence proves a disabled provider's models are
// dropped before the overlay runs; the overlay never resurrects them even if that
// provider carries the best rank.
func TestRoutingOverlayEventDisablePrecedence(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
	setMode(&s, "b", state.ModeDisabled)        // b disabled -> dropped before overlay
	ranks := RankLookup{"a": 2, "b": 0, "c": 1} // b has the best rank but is gone
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	// b must not appear; remaining a(2),c(1) reorder by rank: c first.
	want := []string{"c/x", "a/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v (b must be dropped, not resurrected)", got, want)
	}
	if slices.Contains(got, "b/x") {
		t.Fatalf("overlay resurrected disabled b: %v", got)
	}
}

// TestRoutingOverlayDisabledByteIdentical proves that with routing disabled (the
// default), the Plan is byte-for-byte identical to the pre-routing output. It
// builds the same desired/chain twice — once with routing disabled + nil ranks
// (the legacy path) and asserts it deep-equals the disabled-ranks path, and that
// passing non-nil ranks when routing is disabled still yields the legacy order.
func TestRoutingOverlayDisabledByteIdentical(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	d, s, target := routingFixture(false, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
	// Legacy baseline: routing disabled, nil ranks.
	baseline, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := chainEdit(t, baseline, "agent.md")
	if !slices.Equal(want, []string{"a/x", "b/x", "c/x"}) {
		t.Fatalf("baseline (routing disabled) unexpected order=%v", want)
	}
	// Routing disabled but ranks non-empty: must STILL be identical (overlay not
	// applied because desired.Routing.Enabled is false).
	ranks := RankLookup{"a": 2, "b": 0, "c": 1}
	withRanks, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baseline, withRanks) {
		t.Fatalf("routing disabled but non-nil ranks changed the plan:\nbase=%+v\nranks=%+v", baseline, withRanks)
	}
}

// TestRoutingOverlayNoDesiredMutation proves Build never mutates the desired
// policy, including the Routing config and each mapping's Quota pointer.
func TestRoutingOverlayNoDesiredMutation(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
	snapshot := snapshotDesired(d)
	ranks := RankLookup{"a": 2, "b": 0, "c": 1}
	if _, err := Build(d, s, target, ranks); err != nil {
		t.Fatal(err)
	}
	after := snapshotDesired(d)
	if !reflect.DeepEqual(snapshot, after) {
		t.Fatalf("Build mutated desired:\nbefore=%+v\nafter=%+v", snapshot, after)
	}
}

// TestEffectiveOrderRouting proves the EffectiveOrder helper returns the
// post-overlay order when routing is enabled, and equals the survivor order when
// disabled.
func TestEffectiveOrderRouting(t *testing.T) {
	entries := []string{"a/x", "b/x", "c/x"}
	t.Run("disabled equals survivor order", func(t *testing.T) {
		d, s, target := routingFixture(false, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
		got, err := EffectiveOrder(d, s, target.Definitions[0].Chain, RankLookup{"a": 2, "b": 0, "c": 1})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a/x", "b/x", "c/x"}
		if !slices.Equal(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
	t.Run("enabled applies overlay", func(t *testing.T) {
		d, s, target := routingFixture(true, entries, map[string]string{"a": "g1", "b": "g1", "c": "g1"})
		got, err := EffectiveOrder(d, s, target.Definitions[0].Chain, RankLookup{"a": 2, "b": 0, "c": 1})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"b/x", "c/x", "a/x"}
		if !slices.Equal(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
}

// snapshotDesired captures the comparable parts of a Desired that the overlay must
// not mutate (routing flag and each mapping's quota balance group). It does not
// deep-copy the whole struct; it records the values Build is contractually
// forbidden from changing.
func snapshotDesired(d policy.Desired) policy.Desired {
	cp := policy.Desired{Version: d.Version, Routing: d.Routing, Providers: map[policy.MappingID]policy.Mapping{}}
	for id, m := range d.Providers {
		mm := policy.Mapping{
			CodexBarProviders:  append([]string(nil), m.CodexBarProviders...),
			PolytokenProviders: append([]string(nil), m.PolytokenProviders...),
			Models:             map[string]policy.ModelBaseline{},
		}
		for k, v := range m.Models {
			mm.Models[k] = v
		}
		if m.Quota != nil {
			q := *m.Quota
			mm.Quota = &q
		}
		cp.Providers[id] = mm
	}
	return cp
}
