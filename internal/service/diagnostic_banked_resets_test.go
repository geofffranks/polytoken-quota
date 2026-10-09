package service

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
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
