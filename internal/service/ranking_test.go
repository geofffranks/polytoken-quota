package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// These tests cover ComputeRanking: the mapping of desired quota configs →
// routing.ProviderPolicy, observed state → routing.ProviderObs, and the
// resulting reconcile.RankLookup consumed by Build.

// rankNow is a fixed, Monday-morning UTC instant used as the injected clock. Jan
// 1 2024 is a Monday, so an all-day-Monday off-peak window is active at 10:00.
var rankNow = time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

// qmap builds a Desired with the given mappings, each carrying a Quota config.
// Provider state uses the mapping ID itself (1:1) so state keys line up.
func qmap(routingEnabled bool, mappings ...rankMapping) policy.Desired {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{}}
	if routingEnabled {
		d.Routing = policy.RoutingConfig{Enabled: true}
	}
	for _, mm := range mappings {
		m := policy.Mapping{
			Models: map[string]policy.ModelBaseline{},
		}
		for _, base := range mm.bases {
			m.Models[base] = policy.ModelBaseline{Enabled: true}
		}
		m.Quota = mm.quota
		d.Providers[policy.MappingID(mm.id)] = m
	}
	return d
}

type rankMapping struct {
	id    string
	bases []string
	quota *policy.QuotaConfig
}

// qsnap returns a fresh, healthy QuotaSnapshot with a single window carrying the
// given used/limit pair (so EffectiveRemaining = (limit-used)/limit).
func qsnap(used, limit float64) *quota.QuotaSnapshot {
	u, l := used, limit
	return &quota.QuotaSnapshot{
		MappingID:    "",
		CheckedAt:    rankNow,
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows:      []quota.QuotaWindow{{Used: &u, Limit: &l}},
	}
}

// pstate builds a healthy (normal-mode) ProviderState carrying a snapshot.
func pstate(snap *quota.QuotaSnapshot) state.ProviderState {
	return state.ProviderState{
		Quota:         state.QuotaNormal,
		Availability:  state.Available,
		QuotaSnapshot: snap,
	}
}

func TestQuotaStatusIncludesUnobservedMappings(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"codex":  {Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour}},
		"legacy": {Models: map[string]policy.ModelBaseline{"legacy/model": {}}},
	}}
	coord := &Coordinator{State: staticStateStore{state: state.State{Providers: map[string]state.ProviderState{}}}, Policy: staticPolicyLoader{desired: desired}, Clock: fixedClock{t: now}}
	report := coord.QuotaStatus(context.Background())
	seen := map[string]bool{}
	for _, provider := range report.Providers {
		seen[provider.MappingID] = true
	}
	if !seen["codex"] || !seen["legacy"] {
		t.Fatalf("providers=%+v, want codex and legacy", report.Providers)
	}
}

func TestQuotaStatusUsesMappingID(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	usedB, limitB := 80.0, 100.0
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {Quota: &policy.QuotaConfig{FreshnessTTL: time.Hour}},
	}}
	attempt := &quota.QuotaSnapshot{MappingID: "openai", Status: quota.SourceFailed, Error: "failed"}
	observed := state.State{Revision: 3, Providers: map[string]state.ProviderState{
		"openai":    {Availability: state.Unavailable, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &usedB, Limit: &limitB}}}, QuotaAttempt: attempt},
		"unmanaged": {QuotaAttempt: &quota.QuotaSnapshot{MappingID: "unmanaged", Status: quota.SourceFailed}},
	}}
	coord := &Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}, Clock: fixedClock{t: now}}
	report := coord.QuotaStatus(context.Background())
	if len(report.Providers) != 2 || report.Providers[0].MappingID != "openai" || report.Providers[1].MappingID != "unmanaged" {
		t.Fatalf("providers=%+v, want mapping and residual only", report.Providers)
	}
	p := report.Providers[0]
	if p.Availability != quota.QuotaUnavailable || len(p.Windows) != 1 || p.Windows[0].Used == nil || *p.Windows[0].Used != usedB {
		t.Fatalf("aggregated mapping report=%+v, want unavailable aggregate with most depleted snapshot", p)
	}
	if p.Attempt == nil || p.Attempt.Status != quota.SourceFailed || !report.Problem {
		t.Fatalf("aggregated attempt/problem=%+v/%v", p.Attempt, report.Problem)
	}
}

func TestQuotaStatusSanitizesPersistedAttemptErrorBeforeJSON(t *testing.T) {
	secret := "Bearer status-secret-token"
	attempt := &quota.QuotaSnapshot{MappingID: "codex", Status: quota.SourceFailed, Error: secret}
	coord := &Coordinator{
		State: staticStateStore{state: state.State{Revision: 1, Providers: map[string]state.ProviderState{
			"codex": {QuotaAttempt: attempt},
		}}},
	}
	report := coord.QuotaStatus(context.Background())
	if report.Providers[0].Attempt == nil {
		t.Fatal("missing attempt projection")
	}
	if report.Providers[0].Attempt.Error == secret || strings.Contains(report.Providers[0].Attempt.Error, "status-secret-token") {
		t.Fatalf("secret survived DTO projection: %q", report.Providers[0].Attempt.Error)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(data), "status-secret-token") {
		t.Fatalf("secret survived quota status JSON: %s", data)
	}
}

func TestRankingUnknownManualStablePosition(t *testing.T) {
	desired := qmap(true,
		rankMapping{id: "codex", bases: []string{"codex/model"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "default", Weight: 1}},
		rankMapping{id: "legacy", bases: []string{"legacy/model"}},
	)
	observed := state.State{Providers: map[string]state.ProviderState{
		"codex": pstate(qsnap(10, 100)),
	}}
	lookup, ranking := ComputeRanking(desired, observed, rankNow)
	if _, ok := lookup["legacy"]; ok {
		t.Fatal("unknown/manual mapping entered RankLookup")
	}
	if _, ok := rankEntry(ranking, "legacy"); ok {
		t.Fatal("raw ranking should not contain an unpollable mapping")
	}
}

func TestMappingNilSnapshotFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {Quota: &policy.QuotaConfig{FreshnessTTL: time.Hour}},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		"openai": {Availability: state.Available},
	}}
	_, ranking := ComputeRanking(desired, observed, now)
	entry, ok := rankEntry(ranking, "openai")
	if !ok || entry.Eligible {
		t.Fatalf("missing snapshot must make mapping ineligible: ok=%v entry=%+v", ok, entry)
	}
	report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
	if len(report.Providers) == 0 || report.Providers[0].Availability != quota.QuotaUnavailable || !report.Problem {
		t.Fatalf("missing snapshot must make status unavailable/problem: %+v", report)
	}
}

func ptrFloat(v float64) *float64 { return &v }

type staticStateStore struct{ state state.State }

type staticPolicyLoader struct{ desired policy.Desired }

func (s staticPolicyLoader) LoadPolicy() (policy.Desired, error) { return s.desired, nil }
func (staticPolicyLoader) DesiredExists() bool                   { return true }

func (s staticStateStore) LoadState() (state.State, error) { return s.state, nil }
func (staticStateStore) Save(state.State) error            { return nil }

// rankEntry finds a RankingResult entry by mapping ID.
func rankEntry(r routing.RankingResult, id string) (routing.RankEntry, bool) {
	for _, e := range r.Entries {
		if e.MappingID == id {
			return e, true
		}
	}
	return routing.RankEntry{}, false
}

// TestComputeRankingMapsPolicy verifies quota configs map to routing policy
// correctly: freshness TTL default (30m) is honored, weight is a tie-breaker,
// and balance groups partition comparisons.
func TestComputeRankingMapsPolicy(t *testing.T) {
	t.Run("freshness default 30m keeps a 20m-old snapshot eligible", func(t *testing.T) {
		// FreshnessTTL is zero → routing applies its 30m default. A snapshot
		// 20 minutes old is therefore still fresh (eligible). Were the default
		// not applied (TTL 0), it would be stale and ineligible.
		old := *qsnap(10, 100)
		old.CheckedAt = rankNow.Add(-20 * time.Minute)
		d := qmap(true, rankMapping{
			id: "m1", bases: []string{"m1/x"},
			quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1},
		})
		s := state.State{Providers: map[string]state.ProviderState{"m1": {QuotaSnapshot: &old, Quota: state.QuotaNormal, Availability: state.Available}}}
		_, res := ComputeRanking(d, s, rankNow)
		e, ok := rankEntry(res, "m1")
		if !ok {
			t.Fatal("m1 missing from ranking")
		}
		if !e.Eligible {
			t.Fatalf("m1 should be eligible (freshness default 30m); explanation=%q", e.Explanation)
		}
	})

	t.Run("higher weight wins a headroom tie", func(t *testing.T) {
		// Both have identical 50% headroom; usage incomparable; no reset. The
		// higher-weight mapping (m2) must rank first.
		d := qmap(true,
			rankMapping{id: "m1", bases: []string{"m1/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
			rankMapping{id: "m2", bases: []string{"m2/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 5}},
		)
		s := state.State{Providers: map[string]state.ProviderState{
			"m1": pstate(qsnap(50, 100)),
			"m2": pstate(qsnap(50, 100)),
		}}
		_, res := ComputeRanking(d, s, rankNow)
		m1, _ := rankEntry(res, "m1")
		m2, _ := rankEntry(res, "m2")
		if m2.Rank != 0 || m1.Rank != 1 {
			t.Fatalf("want m2 rank 0 (weight 5) before m1 rank 1 (weight 1); got m1=%d m2=%d", m1.Rank, m2.Rank)
		}
	})
}

// TestComputeRankingMapsObs verifies observed state maps to routing observations
// correctly: the effective mode is derived from state.EffectiveMode, and a
// disabled-mode provider is ineligible while a reserve-mode provider is rankable.
func TestComputeRankingMapsObs(t *testing.T) {
	t.Run("reserve mode is rankable", func(t *testing.T) {
		d := qmap(true, rankMapping{
			id: "m1", bases: []string{"m1/x"},
			quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1},
		})
		// Available + QuotaLow ⇒ reserve.
		s := state.State{Providers: map[string]state.ProviderState{"m1": {
			Quota: state.QuotaLow, Availability: state.Available, QuotaSnapshot: qsnap(10, 100),
		}}}
		_, res := ComputeRanking(d, s, rankNow)
		e, _ := rankEntry(res, "m1")
		if !e.Eligible {
			t.Fatalf("reserve-mode provider should be eligible; got %q", e.Explanation)
		}
	})
	t.Run("disabled mode is ineligible", func(t *testing.T) {
		d := qmap(true, rankMapping{
			id: "m1", bases: []string{"m1/x"},
			quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1},
		})
		// QuotaExhausted ⇒ disabled.
		s := state.State{Providers: map[string]state.ProviderState{"m1": {
			Quota: state.QuotaExhausted, Availability: state.Available, QuotaSnapshot: qsnap(100, 100),
		}}}
		lookup, res := ComputeRanking(d, s, rankNow)
		e, _ := rankEntry(res, "m1")
		if e.Eligible {
			t.Fatal("disabled-mode provider should be ineligible")
		}
		if _, ok := lookup["m1"]; ok {
			t.Fatal("ineligible provider must not appear in the RankLookup")
		}
	})
	t.Run("missing configured alias is ineligible", func(t *testing.T) {
		d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"m1": {
				Quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1},
			},
		}}
		s := state.State{Providers: map[string]state.ProviderState{"codex-a": pstate(qsnap(10, 100))}}
		lookup, res := ComputeRanking(d, s, rankNow)
		e, _ := rankEntry(res, "m1")
		if e.Eligible {
			t.Fatal("mapping with missing configured alias should be ineligible")
		}
		if _, ok := lookup["m1"]; ok {
			t.Fatal("ineligible mapping must not appear in the RankLookup")
		}
	})
}

// TestUnavailableSnapshotIsRemovedFromEffectiveRoute proves the quota snapshot's
// explicit unavailable signal is applied to reconciliation, not only ranking.
// The raw state axes intentionally remain available/normal to reproduce the
// polling boundary where the snapshot is the only exhaustion signal.
func TestUnavailableSnapshotIsRemovedFromEffectiveRoute(t *testing.T) {
	d := qmap(true, rankMapping{
		id: "zai", bases: []string{"zai/model"},
		quota: &policy.QuotaConfig{Adapter: "zai", BalanceGroup: "g", Weight: 1},
	})
	snap := qsnap(100, 100)
	snap.Availability = quota.QuotaUnavailable
	s := state.State{Providers: map[string]state.ProviderState{
		"zai": {Quota: state.QuotaNormal, Availability: state.Available, QuotaSnapshot: snap},
	}}
	ranks, result := ComputeRanking(d, s, rankNow)
	entry, ok := rankEntry(result, "zai")
	if !ok || entry.Eligible {
		t.Fatalf("unavailable snapshot ranking=%+v, want ineligible", entry)
	}
	got, err := reconcile.EffectiveOrder(d, s, policy.Chain{"zai/model"}, ranks)
	if err != nil {
		t.Fatalf("EffectiveOrder: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("effective route=%v, want unavailable model removed", got)
	}
}

// TestZeroOrOverLimitSnapshotRemainingDisablesReconciliation proves that the
// zero remaining value produced by quota exhaustion is treated consistently by
// ranking and model reconciliation, including baseline enablement.
func TestZeroOrOverLimitSnapshotRemainingDisablesReconciliation(t *testing.T) {
	for name, used := range map[string]float64{
		"exactly exhausted": 100,
		"over limit":        125,
	} {
		t.Run(name, func(t *testing.T) {
			d := qmap(true,
				rankMapping{id: "zai", bases: []string{"zai/model"}, quota: &policy.QuotaConfig{Adapter: "zai", BalanceGroup: "g", Weight: 1}},
				rankMapping{id: "codex", bases: []string{"codex/model"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
			)
			snap := qsnap(used, 100)
			s := state.State{Providers: map[string]state.ProviderState{
				"zai":   {Quota: state.QuotaNormal, Availability: state.Available, QuotaSnapshot: snap},
				"codex": pstate(qsnap(10, 100)),
			}}

			ranks, result := ComputeRanking(d, s, rankNow)
			entry, ok := rankEntry(result, "zai")
			if !ok || entry.Eligible {
				t.Fatalf("ranking=%+v, want zai ineligible", entry)
			}

			chain := policy.Chain{"zai/model", "codex/model"}
			got, err := reconcile.EffectiveOrder(d, s, chain, ranks)
			if err != nil {
				t.Fatalf("EffectiveOrder: %v", err)
			}
			if len(got) != 1 || got[0] != "codex/model" {
				t.Fatalf("effective route=%v, want [codex/model]", got)
			}

			plan, err := reconcile.Build(d, s, policy.Target{
				ID: "target", Root: "/root",
				Definitions: []policy.Definition{{Path: "agent.md", Chain: chain}},
			}, ranks)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			var disabled *bool
			for _, edit := range plan.Edits {
				if len(edit.Path) == 3 && edit.Path[0] == "models" && edit.Path[1] == "zai/model" && edit.Path[2] == "enabled" {
					disabled = edit.Enabled
					break
				}
			}
			if disabled == nil || *disabled {
				t.Fatalf("zai baseline enabled edit=%v, want false", disabled)
			}
		})
	}
}

// TestComputeRankingNoPaceSharesRank verifies raw headroom does not invent a
// preference when neither provider has a qualifying pace window.
func TestComputeRankingNoPaceSharesRank(t *testing.T) {
	d := qmap(true,
		rankMapping{id: "full", bases: []string{"full/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
		rankMapping{id: "sparse", bases: []string{"sparse/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
	)
	s := state.State{Providers: map[string]state.ProviderState{
		"full":   pstate(qsnap(20, 100)), // 80% headroom
		"sparse": pstate(qsnap(80, 100)), // 20% headroom
	}}
	lookup, res := ComputeRanking(d, s, rankNow)
	full, _ := rankEntry(res, "full")
	sparse, _ := rankEntry(res, "sparse")
	if full.Rank != 0 || sparse.Rank != 0 {
		t.Fatalf("want shared rank without pace; got full=%d sparse=%d", full.Rank, sparse.Rank)
	}
	if lookup["full"] != 0 || lookup["sparse"] != 0 {
		t.Fatalf("RankLookup mismatch: got full=%d sparse=%d", lookup["full"], lookup["sparse"])
	}
}

// TestComputeRankingOffPeakFirst verifies an off-peak provider ranks above a
// peak provider even when the peak provider has more headroom.
func TestComputeRankingOffPeakFirst(t *testing.T) {
	offSched, err := routing.ParseSchedule("UTC", []routing.OffPeakWindow{{
		Days:  []routing.DayOfWeek{routing.Monday},
		Start: "00:00", End: "24:00",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := qmap(true,
		rankMapping{id: "off", bases: []string{"off/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1, Schedule: &offSched}},
		rankMapping{id: "peak", bases: []string{"peak/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
	)
	s := state.State{Providers: map[string]state.ProviderState{
		"off":  pstate(qsnap(80, 100)), // off-peak but only 20% headroom
		"peak": pstate(qsnap(20, 100)), // peak but 80% headroom
	}}
	_, res := ComputeRanking(d, s, rankNow)
	off, _ := rankEntry(res, "off")
	peak, _ := rankEntry(res, "peak")
	if !off.OffPeak {
		t.Fatal("off should be off-peak at rankNow")
	}
	if off.Rank != 0 || peak.Rank != 1 {
		t.Fatalf("off-peak must rank first; got off=%d peak=%d", off.Rank, peak.Rank)
	}
}

// TestComputeRankingUsageAbsent verifies that with no usage history the ranking
// still succeeds and usage does not influence order (shares incomparable).
func TestComputeRankingUsageAbsent(t *testing.T) {
	d := qmap(true,
		rankMapping{id: "m1", bases: []string{"m1/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
		rankMapping{id: "m2", bases: []string{"m2/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
	)
	s := state.State{Providers: map[string]state.ProviderState{
		"m1": pstate(qsnap(50, 100)),
		"m2": pstate(qsnap(50, 100)),
	}}
	// No UsageHistory: ranking must still return both eligible and ordered by
	// the lexical ID tie-break (equal headroom, no usage, no reset, equal weight).
	lookup, res := ComputeRanking(d, s, rankNow)
	if len(lookup) != 2 {
		t.Fatalf("want 2 eligible entries; got %d (%v)", len(lookup), lookup)
	}
	m1, _ := rankEntry(res, "m1")
	m2, _ := rankEntry(res, "m2")
	if !m1.Eligible || !m2.Eligible {
		t.Fatal("both should be eligible with usage absent")
	}
}

// TestComputeRankingLookupPreservesChainTie proves a shared semantic rank lets
// reconcile.Build preserve the definition's authored provider preference.
func TestComputeRankingLookupPreservesChainTie(t *testing.T) {
	d := qmap(true,
		rankMapping{id: "full", bases: []string{"full/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
		rankMapping{id: "sparse", bases: []string{"sparse/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
	)
	s := state.State{Providers: map[string]state.ProviderState{
		"full":   pstate(qsnap(20, 100)), // no qualifying pace window
		"sparse": pstate(qsnap(80, 100)), // no qualifying pace window
	}}
	ranks, _ := ComputeRanking(d, s, rankNow)
	target := policy.Target{
		ID: "t", Root: "/r",
		Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"sparse/x", "full/x"}}},
	}
	plan, err := reconcile.Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	got := modelScalar(plan, "agent.md")
	if got != "sparse/x" {
		t.Fatalf("shared routing rank should preserve authored head; got model=%q want %q", got, "sparse/x")
	}
}

// TestRoutingDefinitionNamesAndOrdering proves route metadata preserves the
// canonical core order and deterministically sorts named definitions while
// retaining exact target/path/chain identity. Duplicate names are disambiguated
// with policy-relative path context and missing names use a stable path fallback.
func TestRoutingDefinitionMetadataIncludesAllCoreRoutes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home", "alice", ".config", "CANARY-routing")
	writeDefinition := func(rel, frontmatter string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\n"+frontmatter+"\n---\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeDefinition("subagents/zeta.md", "name: Shared\npolytoken:\n  model: live/zeta")
	writeDefinition("subagents/alpha.md", "name: Shared\npolytoken:\n  fallback_models: [live/alpha]")
	writeDefinition("facets/no-name.md", "polytoken:\n  model: live/fallback")

	desired := policy.Desired{
		Global: policy.Target{ID: "global", Global: true, Root: root,
			Full: policy.Chain{"full/a"}, Mini: policy.Chain{"mini/a"}, Nano: policy.Chain{"nano/a"}, Classifier: policy.Chain{"classifier/a"},
			Definitions: []policy.Definition{
				{Path: "subagents/zeta.md", Chain: policy.Chain{"desired/zeta"}},
				{Path: "facets/no-name.md", Chain: policy.Chain{"desired/fallback"}},
				{Path: "subagents/alpha.md", Chain: policy.Chain{"desired/alpha"}},
			},
		},
	}
	resolved, err := NewTargetRegistry().ResolveTargets(desired)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RoutingDefinitionMetadata(resolved)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"full", "mini", "nano", "classifier", "no-name", "Shared (subagents/alpha.md)", "Shared (subagents/zeta.md)"}
	wantPaths := []string{"config.yaml", "config.yaml", "config.yaml", "config.yaml", "facets/no-name.md", "subagents/alpha.md", "subagents/zeta.md"}
	wantHeads := []string{"full/a", "mini/a", "nano/a", "classifier/a", "desired/fallback", "desired/alpha", "desired/zeta"}
	if len(got) != len(wantNames) {
		t.Fatalf("metadata=%+v", got)
	}
	for i := range wantNames {
		if got[i].TargetID != "global" || got[i].Name != wantNames[i] || got[i].SourcePath != wantPaths[i] || len(got[i].Desired) != 1 || got[i].Desired[0] != wantHeads[i] {
			t.Fatalf("metadata[%d]=%+v", i, got[i])
		}
	}
}

// TestRoutingOutputNeverLeaksCanonicalRoots proves the service-ready metadata
// and resolver errors expose only target IDs and normalized policy-relative
// paths, never synthetic home/secret canonical roots or raw frontmatter secrets.
func TestRoutingOutputNeverLeaksCanonicalRoots(t *testing.T) {
	secretRoot := filepath.Join(t.TempDir(), "home", "victim", ".config", "CANARY-SECRET-ROOT")
	path := filepath.Join(secretRoot, "subagents", "agent.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: 'Agent\\napi_key=CANARY-NAME-SECRET'\npolytoken:\n  model: codex/gpt\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	desired := policy.Desired{Global: policy.Target{ID: "global", Global: true, Root: secretRoot, Definitions: []policy.Definition{{Path: "subagents/agent.md", Chain: policy.Chain{"codex/gpt"}}}}}
	resolved, err := NewTargetRegistry().ResolveTargets(desired)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := RoutingDefinitionMetadata(resolved)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	for _, forbidden := range []string{secretRoot, "CANARY-SECRET-ROOT", "CANARY-NAME-SECRET", "api_key"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("metadata leaked %q: %s", forbidden, output)
		}
	}
	if !strings.Contains(output, `"target_id":"global"`) || !strings.Contains(output, `"source_path":"subagents/agent.md"`) {
		t.Fatalf("metadata lacks safe location: %s", output)
	}

	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Global: true, Root: secretRoot, Definitions: []policy.Definition{{Path: filepath.Join(secretRoot, "missing.md")}}}})
	if err == nil {
		t.Fatal("absolute definition path accepted")
	}
	if strings.Contains(err.Error(), secretRoot) || strings.Contains(err.Error(), "CANARY-SECRET-ROOT") {
		t.Fatalf("resolution error leaked canonical root: %v", err)
	}

	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Global: true, Root: secretRoot, Definitions: []policy.Definition{{Path: "subagents/missing.md"}}}})
	if err == nil {
		t.Fatal("missing definition accepted")
	}
	if strings.Contains(err.Error(), secretRoot) || strings.Contains(err.Error(), "CANARY-SECRET-ROOT") {
		t.Fatalf("missing-definition error leaked canonical root: %v", err)
	}

	missingRoot := filepath.Join(t.TempDir(), "home", "victim", ".config", "CANARY-MISSING-ROOT")
	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Global: true, Root: missingRoot}})
	if err == nil {
		t.Fatal("missing root accepted")
	}
	if strings.Contains(err.Error(), missingRoot) || strings.Contains(err.Error(), "CANARY-MISSING-ROOT") {
		t.Fatalf("missing-root error leaked canonical root: %v", err)
	}
	if !strings.Contains(err.Error(), "global") {
		t.Fatalf("missing-root error lacks safe target identity: %v", err)
	}
}

// modelScalar extracts the scalar value of a definition's polytoken.model edit.
func modelScalar(plan reconcile.Plan, file string) string {
	for _, e := range plan.Edits {
		if e.File != file || e.Scalar == nil {
			continue
		}
		if len(e.Path) == 2 && e.Path[0] == "polytoken" && e.Path[1] == "model" {
			return *e.Scalar
		}
	}
	return ""
}
