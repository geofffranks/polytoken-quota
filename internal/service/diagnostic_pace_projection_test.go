package service

// Synthetic projection tests for the per-provider pace signal and freshness
// exposed by the merged status view: canonical signal at the projection's
// AsOf, TTL freshness classification, zero-vs-absent signal, and the
// invariants that the diagnostic signal never changes state, rank, or gate
// attribution. All inputs are synthetic in-memory state — no config, state
// files, or live providers.

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// paceSnapshot builds a synthetic quota snapshot with one qualifying window
// (Period ≥ one day, ResetAt, usable remaining) observed at checkedAt.
func paceSnapshot(checkedAt time.Time, used, limit float64) *quota.QuotaSnapshot {
	reset := checkedAt.Add(24 * time.Hour)
	period := 48 * time.Hour
	return &quota.QuotaSnapshot{
		CheckedAt: checkedAt,
		Windows: []quota.QuotaWindow{{
			Used: fptr64(used), Limit: fptr64(limit),
			ResetAt: tptr64(reset), Period: durptr64(period),
		}},
	}
}

// TestProviderProjectionComputesPaceSignalAtAsOf proves the provider
// projection's signal is the canonical routing.ComputeSignal value at exactly
// the projection's AsOf instant — the same formula the ranking uses — and is
// absent (nil, never zero) for a provider without a snapshot.
func TestProviderProjectionComputesPaceSignalAtAsOf(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snapshot := paceSnapshot(asOf.Add(-time.Minute), 68, 100)
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
		"gp": {},
		"nq": {},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		"gp": {QuotaSnapshot: snapshot},
	}}
	providers, errs := projectProviders(desired, observed, asOf)
	if len(errs) != 0 {
		t.Fatalf("projection errors: %v", errs)
	}
	byID := map[string]ProviderProjection{}
	for _, p := range providers {
		byID[p.MappingID] = p
	}

	want, ok := routing.ComputeSignal(snapshot, asOf)
	if !ok {
		t.Fatalf("ComputeSignal not computable for the synthetic snapshot")
	}
	got := byID["gp"]
	if got.Signal == nil {
		t.Fatalf("gp signal = nil, want the canonical %v", want)
	}
	if *got.Signal != want {
		t.Fatalf("gp signal = %v, want the canonical ComputeSignal value %v at asOf", *got.Signal, want)
	}
	if got.Freshness != FreshnessFresh {
		t.Fatalf("gp freshness = %q, want fresh for a one-minute-old snapshot", got.Freshness)
	}

	nq := byID["nq"]
	if nq.Signal != nil {
		t.Fatalf("nq signal = %v, want nil without a snapshot", *nq.Signal)
	}
	if nq.Freshness != FreshnessMissing {
		t.Fatalf("nq freshness = %q, want missing without a snapshot", nq.Freshness)
	}
}

// TestProviderProjectionSignalSurvivesGateExclusion proves the pace signal is
// diagnostic-only: it is computed for every provider with a usable snapshot,
// including a manual-disabled provider and providers held by non-signal gates
// — and that for a signal-held gate it equals the gate attribution's own
// observed signal, both from ComputeSignal at the same instant.
func TestProviderProjectionSignalSurvivesGateExclusion(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snapshot := paceSnapshot(asOf.Add(-time.Minute), 68, 100)
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
		"gp": {},
		"sg": {},
		"rv": {},
	}}
	observed := state.State{
		Providers: map[string]state.ProviderState{
			"gp": {ManualDisabled: true, QuotaSnapshot: snapshot},
			"sg": {QuotaSnapshot: snapshot},
			"rv": {QuotaSnapshot: snapshot},
		},
		ProviderOwnership: map[string]state.ProviderOwnership{
			"sg": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: -0.25, EngagedRevision: 7},
			"rv": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 3},
		},
	}
	providers, errs := projectProviders(desired, observed, asOf)
	if len(errs) != 0 {
		t.Fatalf("projection errors: %v", errs)
	}
	byID := map[string]ProviderProjection{}
	for _, p := range providers {
		byID[p.MappingID] = p
	}

	// Manual disable keeps the computable signal and carries no gate.
	if gp := byID["gp"]; gp.Signal == nil {
		t.Fatalf("manual-disabled gp signal = nil, want the computed pace")
	} else if gp.Gate != nil {
		t.Fatalf("manual-disabled gp gate = %+v, want none", gp.Gate)
	}
	// A signal-held gate keeps the computable signal, equal to the gate's
	// claim observation at the same instant.
	sg := byID["sg"]
	if sg.Signal == nil {
		t.Fatalf("signal-gated sg signal = nil, want the computed pace")
	}
	if sg.Gate == nil || sg.Gate.Signal == nil {
		t.Fatalf("sg gate = %+v, want the signal attribution", sg.Gate)
	} else if *sg.Gate.Signal != *sg.Signal {
		t.Fatalf("gate signal %v != projection signal %v; both must be ComputeSignal at asOf", *sg.Gate.Signal, *sg.Signal)
	}
	// A reserve-held gate keeps the computable signal too.
	if rv := byID["rv"]; rv.Signal == nil {
		t.Fatalf("reserve-gated rv signal = nil, want the computed pace")
	}
}

// TestClassifyFreshnessTTLEdges pins the freshness classification edges: a
// zero CheckedAt is missing, a snapshot exactly at the TTL is fresh, and only
// strictly older ones are stale; a non-positive TTL falls back to the default
// 30 minutes.
func TestClassifyFreshnessTTLEdges(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		checkedAt time.Time
		ttl       time.Duration
		want      Freshness
	}{
		{"never observed", time.Time{}, time.Hour, FreshnessMissing},
		{"exactly at ttl is fresh", asOf.Add(-time.Hour), time.Hour, FreshnessFresh},
		{"one nanosecond past ttl is stale", asOf.Add(-time.Hour).Add(-time.Nanosecond), time.Hour, FreshnessStale},
		{"within ttl is fresh", asOf.Add(-30 * time.Minute), time.Hour, FreshnessFresh},
		{"default ttl applies when zero", asOf.Add(-30 * time.Minute), 0, FreshnessFresh},
		{"default ttl stale when passed", asOf.Add(-31 * time.Minute), 0, FreshnessStale},
	}
	for _, tc := range cases {
		if got := classifyFreshness(tc.checkedAt, tc.ttl, asOf); got != tc.want {
			t.Fatalf("%s: classifyFreshness = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestProjectProvidersFreshnessUsesConfiguredTTL proves the projection
// classifies freshness from the mapping's configured freshness TTL, not an
// inferred one.
func TestProjectProvidersFreshnessUsesConfiguredTTL(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
		"gp": {Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour}},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		"gp": {QuotaSnapshot: paceSnapshot(asOf.Add(-2*time.Hour), 68, 100)},
	}}
	providers, errs := projectProviders(desired, observed, asOf)
	if len(errs) != 0 {
		t.Fatalf("projection errors: %v", errs)
	}
	if providers[0].Freshness != FreshnessStale {
		t.Fatalf("freshness = %q, want stale for a 2h-old snapshot under a 1h TTL", providers[0].Freshness)
	}

	observed.Providers["gp"] = state.ProviderState{QuotaSnapshot: paceSnapshot(asOf.Add(-30*time.Minute), 68, 100)}
	providers, _ = projectProviders(desired, observed, asOf)
	if providers[0].Freshness != FreshnessFresh {
		t.Fatalf("freshness = %q, want fresh for a 30m-old snapshot under a 1h TTL", providers[0].Freshness)
	}
}

// TestMergedStatusViewCarriesFreshnessSignalAndAsOf proves the merged status
// view propagates the row freshness, the optional pace signal (a real zero
// stays present, absent stays nil), and the evaluation AsOf — including on the
// fatal-error report — without changing status or gate attribution.
func TestMergedStatusViewCarriesFreshnessSignalAndAsOf(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snap := DiagnosticSnapshot{
		asOf: asOf,
		providers: []ProviderProjection{
			{MappingID: "fresh", Freshness: FreshnessFresh, Signal: fptr64(-0.72), Availability: state.Available, CheckedAt: asOf.Add(-time.Minute)},
			{MappingID: "zero", Freshness: FreshnessFresh, Signal: fptr64(0), Availability: state.Available, CheckedAt: asOf.Add(-time.Minute)},
			{MappingID: "absent", Freshness: FreshnessMissing},
			{MappingID: "held", Freshness: FreshnessStale, Signal: fptr64(0.4),
				Gate: &GateReport{Axis: state.OwnershipAxisSignal, Signal: fptr64(-0.9), Threshold: fptr64(0), EngagedRevision: 7}},
		},
		ranks: []RankEntryReport{
			{MappingID: "fresh", Rank: 0, Eligible: true, Explanation: "peak, signal -0.72"},
			{MappingID: "zero", Rank: 1, Eligible: true, Explanation: "peak, signal 0.00"},
			{MappingID: "absent", Rank: 2, Explanation: "ineligible: no fresh snapshot"},
			{MappingID: "held", Rank: 3, Explanation: "ineligible: no fresh snapshot"},
		},
	}
	report := snap.MergedStatusView()
	if !report.AsOf.Equal(asOf) {
		t.Fatalf("report as_of = %v, want the snapshot's clock sample %v", report.AsOf, asOf)
	}
	rows := map[string]MergedStatusProvider{}
	for _, row := range report.Providers {
		rows[row.Provider] = row
	}
	if rows["fresh"].Freshness != FreshnessFresh || rows["fresh"].Signal == nil || *rows["fresh"].Signal != -0.72 {
		t.Fatalf("fresh row = %+v, want freshness fresh and signal -0.72", rows["fresh"])
	}
	if rows["zero"].Signal == nil || *rows["zero"].Signal != 0 {
		t.Fatalf("zero row = %+v, want a present real zero signal", rows["zero"])
	}
	if rows["absent"].Freshness != FreshnessMissing || rows["absent"].Signal != nil {
		t.Fatalf("absent row = %+v, want freshness missing and no signal", rows["absent"])
	}
	// The stale classification rides through, and the held row keeps both its
	// gate attribution and its own signal — state and gate are untouched.
	if rows["held"].Freshness != FreshnessStale {
		t.Fatalf("held row freshness = %q, want stale", rows["held"].Freshness)
	}
	if rows["held"].Status != StatusGated || rows["held"].Gate == nil || rows["held"].Gate.Signal == nil || *rows["held"].Gate.Signal != -0.9 {
		t.Fatalf("held row = %+v, want unchanged gated status and gate attribution", rows["held"])
	}
	if rows["held"].Signal == nil || *rows["held"].Signal != 0.4 {
		t.Fatalf("held row signal = %+v, want the current 0.4 pace distinct from the gate claim", rows["held"].Signal)
	}
	if rows["held"].Rank != 3 || rows["absent"].Eligible {
		t.Fatalf("rank/eligibility changed: held rank %d absent eligible %v", rows["held"].Rank, rows["absent"].Eligible)
	}

	// A fatal diagnostic still identifies its evaluation instant.
	fatal := DiagnosticSnapshot{asOf: asOf, fatalError: "load state failed"}.MergedStatusView()
	if fatal.Error == "" || !fatal.AsOf.Equal(asOf) {
		t.Fatalf("fatal report = %+v, want the error and the as_of instant", fatal)
	}
}
