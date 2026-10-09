package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	sanitizepkg "github.com/geofffranks/polytoken-quota/internal/sanitize"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func mergedView(t *testing.T, d *diagnosticDeps) MergedStatusReport {
	t.Helper()
	return diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
}

func mergedRouteByName(t *testing.T, report MergedStatusReport, name string) MergedStatusRoute {
	t.Helper()
	for _, route := range report.Routes {
		if route.Name == name {
			return route
		}
	}
	t.Fatalf("route %q not found in %d routes", name, len(report.Routes))
	return MergedStatusRoute{}
}

func TestMergedStatusCopiesResetCreditNestedData(t *testing.T) {
	asOf := diagnosticAsOf
	expiry := asOf.Add(time.Hour)
	snapshot := DiagnosticSnapshot{asOf: asOf, providers: []ProviderProjection{{
		MappingID: "renamed", Adapter: "codex", Freshness: FreshnessMissing,
		ResetCredits: &ResetCreditReport{Freshness: FreshnessStale, UsableCount: intRef(1),
			EarliestExpiryAt: &expiry, AvailableExpiries: []*time.Time{&expiry},
			LastSuccess:   &ResetCreditInventoryReport{AvailableExpiries: []*time.Time{&expiry}},
			LatestAttempt: &ResetCreditAttemptReport{Status: quota.CreditAttemptFailed, Inventory: &ResetCreditInventoryReport{AvailableExpiries: []*time.Time{&expiry}}}},
	}}, ranks: []RankEntryReport{{MappingID: "renamed"}}}
	first := snapshot.MergedStatusView().Providers[0]
	*first.ResetCredits.EarliestExpiryAt = time.Time{}
	*first.ResetCredits.AvailableExpiries[0] = time.Time{}
	*first.ResetCredits.LastSuccess.AvailableExpiries[0] = time.Time{}
	*first.ResetCredits.LatestAttempt.Inventory.AvailableExpiries[0] = time.Time{}
	again := snapshot.MergedStatusView().Providers[0]
	if again.Adapter != "codex" || again.ResetCredits == nil || again.ResetCredits.EarliestExpiryAt == nil || !again.ResetCredits.EarliestExpiryAt.Equal(expiry) || !again.ResetCredits.AvailableExpiries[0].Equal(expiry) || !again.ResetCredits.LastSuccess.AvailableExpiries[0].Equal(expiry) || !again.ResetCredits.LatestAttempt.Inventory.AvailableExpiries[0].Equal(expiry) {
		t.Fatalf("merged report reset data was not independently copied: %+v", again)
	}
}

func intRef(v int) *int { return &v }

func TestMergedStatusAdditiveAttemptPollingAndPendingDetails(t *testing.T) {
	observedAt := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	attemptedAt := observedAt.Add(time.Hour)
	view := DiagnosticSnapshot{
		asOf: observedAt.Add(2 * time.Hour),
		providers: []ProviderProjection{
			{MappingID: "failed", CheckedAt: observedAt, LatestAttempt: &QuotaAttemptReport{
				Status: quota.SourceFailed, CheckedAt: attemptedAt, Error: "sanitized error",
			}, Adapter: "codex"},
			{MappingID: "never", Adapter: "", Freshness: FreshnessMissing},
		},
		ranks:          []RankEntryReport{{MappingID: "failed"}, {MappingID: "never"}},
		pendingDetails: []PendingTargetDetail{{TargetID: sanitizepkg.Identifier("bad target\nsecret"), LastAttemptAt: attemptedAt}},
	}
	report := view.MergedStatusView()
	if got := report.Providers[0]; !got.CheckedAt.Equal(observedAt) || !got.ObservationAt.Equal(observedAt) || got.LatestAttempt == nil || !got.LatestAttempt.CheckedAt.Equal(attemptedAt) {
		t.Fatalf("snapshot and attempt provenance conflated: %+v", got)
	}
	if report.Providers[0].PollingStatus != "disabled" || report.Providers[1].PollingStatus != "unknown" {
		t.Fatalf("polling statuses = %q, %q; want disabled, unknown", report.Providers[0].PollingStatus, report.Providers[1].PollingStatus)
	}
	if len(report.PendingDetails) != 1 || report.PendingDetails[0].TargetID != sanitizepkg.Identifier("bad target\nsecret") || !report.PendingDetails[0].LastAttemptAt.Equal(attemptedAt) {
		t.Fatalf("pending details = %+v", report.PendingDetails)
	}
}

func TestMergedStatusSkipsDisabledModelWithRankReason(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["alpha"]
	ps.ManualDisabled = true
	d.observed.Providers["alpha"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "alpha/full", Reason: "manual disable"}}) {
		t.Fatalf("full skipped = %+v, want [alpha/full: manual disable]", full.Skipped)
	}
	// The mini chain is entirely alpha/full: all desired entries are skipped.
	mini := mergedRouteByName(t, report, "mini")
	if !reflect.DeepEqual(mini.Skipped, []SkippedModel{{Model: "alpha/full", Reason: "manual disable"}}) {
		t.Fatalf("mini skipped = %+v, want [alpha/full: manual disable]", mini.Skipped)
	}
	if len(mini.Effective) != 0 {
		t.Fatalf("mini effective = %v, want empty", mini.Effective)
	}
	if mini.ProjectionError {
		t.Fatal("mini flagged as projection error, want successful projection")
	}
}

func TestMergedStatusSkipsQuotaExhaustedModel(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.Quota = state.QuotaExhausted
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	// beta/full precedes alpha/full in the desired chain and is dropped with
	// its disabled-mode condition.
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "quota exhausted"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: quota exhausted]", full.Skipped)
	}
}

func TestMergedStatusSkipsUnavailableModel(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.Availability = state.Unavailable
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "unavailable"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: unavailable]", full.Skipped)
	}
}

func TestMergedStatusSnapshotUnavailableRemovesModelAndReportsReason(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.QuotaSnapshot.Availability = quota.QuotaUnavailable
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "unavailable"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: unavailable]", full.Skipped)
	}
	if len(full.Effective) != 1 || full.Effective[0] != "alpha/full" {
		t.Fatalf("full effective = %v, want [alpha/full]", full.Effective)
	}
}

func TestMergedStatusNoSkipsWhenChainsUntouched(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	report := mergedView(t, d)
	for _, route := range report.Routes {
		if route.ProjectionError {
			t.Fatalf("route %q flagged projection error", route.Name)
		}
		if len(route.Skipped) != 0 {
			t.Fatalf("route %q skipped = %+v, want none", route.Name, route.Skipped)
		}
	}
}

func TestMergedStatusProjectionErrorRoute(t *testing.T) {
	root := t.TempDir()
	desired := policy.Desired{
		Version: 1,
		Routing: policy.RoutingConfig{Enabled: true},
		Providers: map[policy.MappingID]policy.Mapping{
			"alpha": {
				Models: map[string]policy.ModelBaseline{"alpha/full": {Enabled: true}},
			},
		},
		Global: policy.Target{
			ID: "global", Root: root, Global: true,
			Full: policy.Chain{"alpha/full", "ghost/full"},
		},
	}
	resolved, err := NewTargetRegistry().ResolveTargets(desired)
	if err != nil {
		t.Fatal(err)
	}
	d := &diagnosticDeps{desired: desired, observed: state.State{Revision: 1}, targets: resolved, policyExists: true}
	report := mergedView(t, d)
	if report.Error != "" {
		t.Fatalf("unexpected fatal error: %q", report.Error)
	}

	route := mergedRouteByName(t, report, "full")
	if !route.ProjectionError {
		t.Fatalf("route full projection error = false, want true")
	}
	if len(route.Skipped) != 0 {
		t.Fatalf("projection-error route skipped = %+v, want none", route.Skipped)
	}
	found := false
	for _, e := range report.Errors {
		if e.Scope == ErrorScopeRoute && e.TargetID == "global" {
			found = true
		}
	}
	if !found {
		t.Fatalf("route-scope error for target global missing: %+v", report.Errors)
	}
}

// TestMergedStatusGatedPrecedence pins the consolidated status matrix:
// signal- and reserve-held gates read `gated` over any quota health, manual
// disable still wins, conflicted claims keep their quota-health status, and
// disabled-axis, legacy, and no-claim rows render exactly as before.
func TestMergedStatusGatedPrecedence(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	gate := func(axis string, conflict bool) *GateReport {
		return &GateReport{Axis: axis, Conflict: conflict, EngagedRevision: 3}
	}
	for name, tc := range map[string]struct {
		provider ProviderProjection
		want     string
	}{
		"signal gate over fresh":                  {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisSignal, false), CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusGated},
		"reserve gate over fresh":                 {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisReserve, false), CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusGated},
		"manual disable beats gate":               {ProviderProjection{MappingID: "p", ManualDisabled: true, Gate: gate(state.OwnershipAxisSignal, false), CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusDisabled},
		"gated never observed":                    {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisSignal, false)}, StatusGated},
		"gated over unavailable":                  {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisSignal, false), Availability: state.Unavailable, CheckedAt: asOf, Freshness: FreshnessFresh}, StatusGated},
		"conflicted signal claim stays healthy":   {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisSignal, true), CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusAvailable},
		"conflicted reserve claim stays healthy":  {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisReserve, true), CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusAvailable},
		"disabled axis keeps availability status": {ProviderProjection{MappingID: "p", Gate: gate(state.OwnershipAxisDisabled, false), Availability: state.Unavailable, CheckedAt: asOf, Freshness: FreshnessFresh}, StatusUnavailable},
		"legacy claim keeps quota health":         {ProviderProjection{MappingID: "p", CheckedAt: asOf, Freshness: FreshnessFresh, Availability: state.Available}, StatusAvailable},
		"unavailable without gate":                {ProviderProjection{MappingID: "p", Availability: state.Unavailable}, StatusUnavailable},
		"never observed without gate":             {ProviderProjection{MappingID: "p", Freshness: FreshnessMissing}, StatusEnabled},
	} {
		t.Run(name, func(t *testing.T) {
			view := DiagnosticSnapshot{
				providers: []ProviderProjection{tc.provider},
				ranks:     []RankEntryReport{{MappingID: tc.provider.MappingID}},
			}
			report := view.MergedStatusView()
			if len(report.Providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(report.Providers))
			}
			if got := report.Providers[0].Status; got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMergedStatusWindowlessPartialKeepsUnknownAvailability pins the
// projection wiring end to end: a stored zero-window partial snapshot (the
// codex/zai class) must keep its snapshot availability unknown through
// aggregation — the row axis collapses to unavailable — so the QUOTA fallback
// renders plain "no data" instead of claiming unavailable.
func TestMergedStatusWindowlessPartialKeepsUnknownAvailability(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"codex": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	observed := state.State{
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &quota.QuotaSnapshot{
				MappingID: "codex", CheckedAt: asOf, Status: quota.SourcePartial, Availability: quota.QuotaUnknown,
			}},
		},
	}
	providers, errs := projectProviders(desired, observed, asOf)
	if len(errs) != 0 {
		t.Fatalf("projection errors: %v", errs)
	}
	view := DiagnosticSnapshot{providers: providers, ranks: []RankEntryReport{{MappingID: "codex"}}}
	row := view.MergedStatusView().Providers[0]
	if row.Availability != quota.QuotaUnknown {
		t.Fatalf("availability = %q, want unknown from the raw snapshot", row.Availability)
	}
	if row.Condition != "" {
		t.Fatalf("condition = %q, want none", row.Condition)
	}
	if row.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable from the fail-closed row axis", row.Status)
	}
}

// TestMergedStatusReasonNamesOutOfQuota pins the merged view's reason
// refinement: the ranking's generic mode-disabled explanation becomes
// "ineligible: out of quota" exactly when the row's aggregated axes name a
// quota cause (an exhausted quota axis, or an availability axis held off by
// out-of-quota evidence) and a real snapshot backs the row.
func TestMergedStatusReasonNamesOutOfQuota(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		provider ProviderProjection
		want     string
	}{
		"exhausted quota axis": {
			provider: ProviderProjection{MappingID: "p", Quota: state.QuotaExhausted, EffectiveMode: state.ModeDisabled,
				Availability: state.Available, CheckedAt: asOf, Freshness: FreshnessFresh},
			want: "ineligible: out of quota",
		},
		"availability axis held off by overage": {
			provider: ProviderProjection{MappingID: "p", Availability: state.Unavailable, EffectiveMode: state.ModeDisabled,
				CheckedAt: asOf, Freshness: FreshnessFresh, SnapshotAvailability: quota.QuotaUnavailable, Condition: "in overage"},
			want: "ineligible: out of quota",
		},
		"manual disable keeps generic text": {
			provider: ProviderProjection{MappingID: "p", ManualDisabled: true, Quota: state.QuotaExhausted,
				EffectiveMode: state.ModeDisabled, CheckedAt: asOf, Freshness: FreshnessFresh},
			want: "ineligible: disabled",
		},
		"no snapshot keeps generic text": {
			// The fail-closed axes for a missing observation say exhausted and
			// unavailable, but without a snapshot the row was never observed:
			// "out of quota" would fabricate data.
			provider: ProviderProjection{MappingID: "p", Quota: state.QuotaExhausted, Availability: state.Unavailable,
				EffectiveMode: state.ModeDisabled, Freshness: FreshnessMissing},
			want: "ineligible: disabled",
		},
		"corrupted axis keeps generic text": {
			provider: ProviderProjection{MappingID: "p", Quota: state.Quota("bogus"), EffectiveMode: state.ModeDisabled,
				Availability: state.Available, CheckedAt: asOf, Freshness: FreshnessFresh},
			want: "ineligible: disabled",
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := DiagnosticSnapshot{
				providers: []ProviderProjection{tc.provider},
				ranks:     []RankEntryReport{{MappingID: tc.provider.MappingID, Explanation: disabledRankExplanation}},
			}
			report := view.MergedStatusView()
			if len(report.Providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(report.Providers))
			}
			if got := report.Providers[0].Reason; got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMergedStatusOutOfQuotaReasonEndToEnd runs the refinement through the
// real projection pipeline: a quota-exhausted axis names the quota cause while
// the healthy sibling keeps its own ranking explanation.
func TestMergedStatusOutOfQuotaReasonEndToEnd(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.Quota = state.QuotaExhausted
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)
	rowFor := func(id string) MergedStatusProvider {
		for _, row := range report.Providers {
			if row.Provider == id {
				return row
			}
		}
		t.Fatalf("provider %q missing from report", id)
		return MergedStatusProvider{}
	}
	if got := rowFor("beta").Reason; got != "ineligible: out of quota" {
		t.Fatalf("beta reason = %q, want %q", got, "ineligible: out of quota")
	}
	if got := rowFor("alpha").Reason; got == "ineligible: out of quota" {
		t.Fatalf("healthy alpha reason = %q, must not read as out of quota", got)
	}
}

// TestMergedStatusUnavailableSnapshotReasonOutOfQuota pins the in-overage
// shape: a raw snapshot reporting unavailable (no usable remaining) drives the
// aggregated availability axis down, and the reason names the quota cause.
func TestMergedStatusUnavailableSnapshotReasonOutOfQuota(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.observed.Providers["beta"].QuotaSnapshot.Availability = quota.QuotaUnavailable
	report := mergedView(t, d)
	for _, row := range report.Providers {
		if row.Provider != "beta" {
			continue
		}
		if row.Reason != "ineligible: out of quota" {
			t.Fatalf("beta reason = %q, want %q", row.Reason, "ineligible: out of quota")
		}
		return
	}
	t.Fatal("beta missing from report")
}
