package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
)

func TestStatusJSONIncludesAdditiveResetCreditReport(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	expiry := asOf.Add(2 * time.Hour)
	raw := runStatusJSON(t, 0, service.MergedStatusReport{AsOf: asOf, Providers: []service.MergedStatusProvider{{
		Provider: "renamed", Adapter: "codex", ResetCredits: &service.ResetCreditReport{
			Freshness: service.FreshnessStale, EarliestExpiryAt: &expiry,
			UsableCount: intPointer(1), AvailableExpiries: []*time.Time{&expiry},
		},
	}}})
	var decoded struct {
		Providers []struct {
			Adapter string `json:"adapter"`
			Reset   struct {
				Freshness string `json:"freshness"`
				Count     *int   `json:"usable_count"`
				Expiry    string `json:"earliest_expiry_at"`
			} `json:"reset_credits"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	p := decoded.Providers[0]
	if p.Adapter != "codex" || p.Reset.Freshness != "stale" || p.Reset.Count == nil || *p.Reset.Count != 1 || p.Reset.Expiry != expiry.Format(time.RFC3339Nano) {
		t.Fatalf("reset JSON = %+v", p)
	}
	legacy := runStatusJSON(t, 0, service.MergedStatusReport{Providers: []service.MergedStatusProvider{{Provider: "old"}}})
	if strings.Contains(string(legacy), "adapter") || strings.Contains(string(legacy), "reset_credits") {
		t.Fatalf("legacy provider gained optional fields: %s", legacy)
	}
}

func intPointer(v int) *int { return &v }
