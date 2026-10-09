package service

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func TestResetCreditReportUsesIndependentFreshnessAndEarliestFutureExpiry(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	past, equal, later, earliest := asOf.Add(-time.Hour), asOf, asOf.Add(3*time.Hour), asOf.Add(time.Hour)
	credits := quota.ResetCreditState{LastSuccess: &quota.ResetCreditInventory{
		ObservedAt: asOf.Add(-time.Hour), UsableCount: 4,
		AvailableExpiries: []*time.Time{&later, nil, &past, &equal, &earliest},
	}}
	report := resetCreditReport(credits, 30*time.Minute, asOf)
	if report.Freshness != FreshnessStale {
		t.Fatalf("reset freshness=%q, want stale independent of ordinary usage", report.Freshness)
	}
	if report.UsableCount == nil || *report.UsableCount != 3 {
		t.Fatalf("usable count=%v, want three future/unknown entries", report.UsableCount)
	}
	if report.EarliestExpiryAt == nil || !report.EarliestExpiryAt.Equal(earliest) {
		t.Fatalf("earliest expiry=%v, want %v", report.EarliestExpiryAt, earliest)
	}
	if len(report.AvailableExpiries) != 3 {
		t.Fatalf("filtered expiries=%v, want future and unknown only", report.AvailableExpiries)
	}
}

func TestResetCreditReportMissingAndCompleteEmpty(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	missing := resetCreditReport(quota.ResetCreditState{}, 0, asOf)
	if missing.Freshness != FreshnessMissing || missing.UsableCount != nil {
		t.Fatalf("missing report=%+v", missing)
	}
	credits := quota.ResetCreditState{LastSuccess: &quota.ResetCreditInventory{ObservedAt: asOf, UsableCount: 0}}
	empty := resetCreditReport(credits, 30*time.Minute, asOf)
	if empty.Freshness != FreshnessFresh || empty.UsableCount == nil || *empty.UsableCount != 0 || empty.EarliestExpiryAt != nil {
		t.Fatalf("empty report=%+v", empty)
	}
}

func TestProjectProvidersKeepsResetHistoryForAnyAdapterAndCodexWithoutQuota(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	observedAt := asOf.Add(-time.Minute)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"other":          {Models: map[string]policy.ModelBaseline{"other/model": {}}, Quota: &policy.QuotaConfig{Adapter: "zai"}},
		"codex-no-quota": {Models: map[string]policy.ModelBaseline{"codex-no-quota/model": {}}},
	}}
	expiry := asOf.Add(time.Hour)
	observed := state.State{Providers: map[string]state.ProviderState{
		"other":          {ResetCredits: quota.ResetCreditState{LastSuccess: &quota.ResetCreditInventory{ObservedAt: observedAt, UsableCount: 1, AvailableExpiries: []*time.Time{&expiry}}}},
		"codex-no-quota": {ResetCredits: quota.ResetCreditState{LastSuccess: &quota.ResetCreditInventory{ObservedAt: observedAt, UsableCount: 1, AvailableExpiries: []*time.Time{&expiry}}}},
	}}
	projections, _ := projectProviders(desired, observed, asOf)
	byID := make(map[string]ProviderProjection, len(projections))
	for _, projection := range projections {
		byID[projection.MappingID] = projection
	}
	other := byID["other"].ResetCredits
	if other == nil || other.LastSuccess == nil || other.UsableCount == nil || *other.UsableCount != 1 {
		t.Fatalf("non-Codex reset history lost: %+v", byID["other"])
	}
	noQuota := byID["codex-no-quota"]
	if noQuota.Adapter != "" || noQuota.ResetCredits == nil || noQuota.ResetCredits.LastSuccess == nil {
		t.Fatalf("no-quota reset history lost: %+v", noQuota)
	}

	desired.Providers["codex-empty"] = policy.Mapping{Models: map[string]policy.ModelBaseline{"codex-empty/model": {}}, Quota: &policy.QuotaConfig{Adapter: "codex"}}
	projections, _ = projectProviders(desired, observed, asOf)
	for _, projection := range projections {
		if projection.MappingID == "codex-empty" && (projection.ResetCredits == nil || projection.ResetCredits.Freshness != FreshnessMissing) {
			t.Fatalf("configured Codex missing reset report = %+v", projection)
		}
	}
}
