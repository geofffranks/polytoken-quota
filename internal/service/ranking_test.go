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
// routing.ProviderPolicy, observed state → routing.ProviderObs, usage history →
// routing.UsageShare, and the resulting reconcile.RankLookup consumed by Build.

// rankNow is a fixed, Monday-morning UTC instant used as the injected clock. Jan
// 1 2024 is a Monday, so an all-day-Monday off-peak window is active at 10:00.
var rankNow = time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

// qmap builds a Desired with the given mappings, each carrying a Quota config.
// CodexBarProviders uses the mapping ID itself (1:1) so state keys line up.
func qmap(routingEnabled bool, mappings ...rankMapping) policy.Desired {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{}}
	if routingEnabled {
		d.Routing = policy.RoutingConfig{Enabled: true}
	}
	for _, mm := range mappings {
		m := policy.Mapping{
			CodexBarProviders:  []string{mm.id},
			PolytokenProviders: []string{mm.id},
			Models:             map[string]policy.ModelBaseline{},
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

func TestQuotaStatusAggregatesCodexBarProvidersUnderMappingID(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	usedA, limitA := 10.0, 100.0
	usedB, limitB := 80.0, 100.0
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"codex-a", "codex-b"}, Quota: &policy.QuotaConfig{FreshnessTTL: time.Hour}},
	}}
	attempt := &quota.QuotaSnapshot{MappingID: "codex-a", Status: quota.SourceFailed, Error: "failed"}
	observed := state.State{Revision: 3, Providers: map[string]state.ProviderState{
		"codex-a":   {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &usedA, Limit: &limitA}}}, QuotaAttempt: attempt},
		"codex-b":   {Availability: state.Unavailable, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &usedB, Limit: &limitB}}}},
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

func TestQuotaStatusAggregatedAliasesRemainAvailableWhenAllAvailable(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"codex-a", "codex-b"}},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		"codex-a": {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourceFresh}},
		"codex-b": {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourceFresh}},
	}}
	report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
	if got := report.Providers[0].Availability; got != quota.QuotaAvailable {
		t.Fatalf("availability=%s, want available", got)
	}
}

func TestQuotaStatusAggregatedAliasesMissingObservationUnavailable(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"codex-a", "codex-b"}, Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		"codex-a": {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourceFresh}},
	}}
	report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
	if got := report.Providers[0].Availability; got != quota.QuotaUnavailable || !report.Problem {
		t.Fatalf("availability/problem=%s/%v, want unavailable/true", got, report.Problem)
	}
}

func TestQuotaStatusAggregatedAliasesUnknownOrNoSignalFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"usable", "unsafe"}, Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	for name, unsafe := range map[string]*quota.QuotaSnapshot{
		"unknown availability":   {CheckedAt: now, Availability: quota.QuotaUnknown, Status: quota.SourceFresh},
		"no effective remaining": {CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourcePartial, Windows: []quota.QuotaWindow{{Name: "unreported"}}},
	} {
		t.Run(name, func(t *testing.T) {
			observed := state.State{Providers: map[string]state.ProviderState{
				"usable": {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{UsagePercent: ptrFloat(10)}}}},
				"unsafe": {Availability: state.Available, QuotaSnapshot: unsafe},
			}}
			report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
			if got := report.Providers[0].Availability; got != quota.QuotaUnavailable || !report.Problem {
				t.Fatalf("availability/problem=%s/%v, want unavailable/true", got, report.Problem)
			}
			_, ranking := ComputeRanking(desired, observed, now)
			entry, _ := rankEntry(ranking, "openai")
			if entry.Eligible {
				t.Fatalf("mixed unsafe aliases must be ineligible: %+v", entry)
			}
		})
	}
}

func TestConfiguredAliasNilSnapshotFailsClosedDespiteLegacyMappingState(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"usable", "missing-snapshot"}, Quota: &policy.QuotaConfig{FreshnessTTL: time.Hour}},
	}}
	used, limit := 10.0, 100.0
	observed := state.State{Providers: map[string]state.ProviderState{
		"usable":           {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &used, Limit: &limit}}}},
		"missing-snapshot": {Availability: state.Available},
		"openai":           {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &used, Limit: &limit}}}},
	}}
	_, ranking := ComputeRanking(desired, observed, now)
	entry, ok := rankEntry(ranking, "openai")
	if !ok || entry.Eligible {
		t.Fatalf("unsafe alias must make mapping ineligible: ok=%v entry=%+v", ok, entry)
	}
	report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
	if len(report.Providers) == 0 || report.Providers[0].Availability != quota.QuotaUnavailable || !report.Problem {
		t.Fatalf("unsafe alias must make status unavailable/problem: %+v", report)
	}
	if hasMissingAlias([]string{"usable", "missing-snapshot"}, observed.Providers) {
		t.Fatal("present nil-snapshot alias must not be treated as missing")
	}
}

func TestQuotaStatusMixedMissingAndUnsafeAliasFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {CodexBarProviders: []string{"unsafe", "missing"}, Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	for name, unsafe := range map[string]*quota.QuotaSnapshot{
		"nil snapshot":         nil,
		"unknown availability": {CheckedAt: now, Availability: quota.QuotaUnknown, Status: quota.SourceFresh},
		"no usable remaining":  {CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourcePartial, Windows: []quota.QuotaWindow{{Name: "unreported"}}},
	} {
		t.Run(name, func(t *testing.T) {
			observed := state.State{Providers: map[string]state.ProviderState{
				"unsafe": {Availability: state.Available, QuotaSnapshot: unsafe},
				"openai": {Availability: state.Available, QuotaSnapshot: &quota.QuotaSnapshot{CheckedAt: now, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{UsagePercent: ptrFloat(10)}}}},
			}}
			report := (&Coordinator{State: staticStateStore{state: observed}, Policy: staticPolicyLoader{desired: desired}}).QuotaStatus(context.Background())
			if len(report.Providers) != 1 || report.Providers[0].Availability != quota.QuotaUnavailable || !report.Problem {
				t.Fatalf("mixed missing/unsafe aliases must remain unavailable/problem: %+v", report)
			}
		})
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
				CodexBarProviders: []string{"codex-a", "codex-b"},
				Quota:             &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1},
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

// TestComputeRankingHeadroom verifies larger effective headroom ranks first
// within a balance group (off-peak aside).
func TestComputeRankingHeadroom(t *testing.T) {
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
	if full.Rank != 0 || sparse.Rank != 1 {
		t.Fatalf("want full (80%%) rank 0 before sparse (20%%) rank 1; got full=%d sparse=%d", full.Rank, sparse.Rank)
	}
	if lookup["full"] != 0 || lookup["sparse"] != 1 {
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

// TestComputeRankingLookupReordersBuild proves the produced RankLookup, when
// passed to reconcile.Build with routing enabled, reorders a definition chain by
// global rank (headroom tie-break).
func TestComputeRankingLookupReordersBuild(t *testing.T) {
	d := qmap(true,
		rankMapping{id: "full", bases: []string{"full/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
		rankMapping{id: "sparse", bases: []string{"sparse/x"}, quota: &policy.QuotaConfig{Adapter: "codex", BalanceGroup: "g", Weight: 1}},
	)
	s := state.State{Providers: map[string]state.ProviderState{
		"full":   pstate(qsnap(20, 100)), // 80% headroom → rank 0
		"sparse": pstate(qsnap(80, 100)), // 20% headroom → rank 1
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
	if got != "full/x" {
		t.Fatalf("with routing enabled the higher-headroom provider should lead; got model=%q want %q", got, "full/x")
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
		Global: policy.Target{ID: "global", Root: root,
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
	desired := policy.Desired{Global: policy.Target{ID: "global", Root: secretRoot, Definitions: []policy.Definition{{Path: "subagents/agent.md", Chain: policy.Chain{"codex/gpt"}}}}}
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

	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Root: secretRoot, Definitions: []policy.Definition{{Path: filepath.Join(secretRoot, "missing.md")}}}})
	if err == nil {
		t.Fatal("absolute definition path accepted")
	}
	if strings.Contains(err.Error(), secretRoot) || strings.Contains(err.Error(), "CANARY-SECRET-ROOT") {
		t.Fatalf("resolution error leaked canonical root: %v", err)
	}

	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Root: secretRoot, Definitions: []policy.Definition{{Path: "subagents/missing.md"}}}})
	if err == nil {
		t.Fatal("missing definition accepted")
	}
	if strings.Contains(err.Error(), secretRoot) || strings.Contains(err.Error(), "CANARY-SECRET-ROOT") {
		t.Fatalf("missing-definition error leaked canonical root: %v", err)
	}

	missingRoot := filepath.Join(t.TempDir(), "home", "victim", ".config", "CANARY-MISSING-ROOT")
	_, err = NewTargetRegistry().ResolveTargets(policy.Desired{Global: policy.Target{ID: "global", Root: missingRoot}})
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
