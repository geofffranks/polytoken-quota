package service

// Neuralwatt subscription overage persistence and projection: an in-overage
// subscription observation with retained usable numbers (windowed, fresh,
// unavailable, naming the condition) must survive the durable state round
// trip and the read-only projections with its numbers, next reset, and
// condition intact — while an invalid-overage observation replaces a prior
// healthy snapshot rather than retaining it.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func overageWindowedSnap(mappingID string) quota.QuotaSnapshot {
	used, limit, pct := 2.911, 2.353, 100.0
	reset := time.Date(2026, 9, 12, 21, 48, 47, 250000000, time.UTC)
	return quota.QuotaSnapshot{
		MappingID: mappingID,
		CheckedAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		Windows: []quota.QuotaWindow{{
			Name: "subscription_kwh", Used: &used, Limit: &limit, UsagePercent: &pct, ResetAt: &reset,
		}},
		Availability: quota.QuotaUnavailable,
		Status:       quota.SourceFresh,
		Condition:    quota.ConditionInOverage,
	}
}

// TestOverageWindowedObservationReplacesPriorHealthy proves a windowed
// unavailable overage observation is a successful attempt: it replaces the
// prior healthy last-good snapshot in the durable state.
func TestOverageWindowedObservationReplacesPriorHealthy(t *testing.T) {
	priorGood := freshSnap("neuralwatt", 10)
	observed := state.State{
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"neuralwatt": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
	}
	attempts := map[string]quota.QuotaSnapshot{"neuralwatt": overageWindowedSnap("neuralwatt")}

	next := applyQuotaObservations(observed, overageDesired(), attempts)

	snap := next.Providers["neuralwatt"].QuotaSnapshot
	if snap == nil || snap.Condition != quota.ConditionInOverage || snap.Availability != quota.QuotaUnavailable || len(snap.Windows) != 1 {
		t.Fatalf("prior healthy snapshot retained over windowed overage observation: %+v", snap)
	}
}

// TestOverageWindowedObservationPersistsAndProjects proves the retained
// overage numbers, reset, and condition survive the state Store round trip and
// feed the diagnostic projections the same way healthy numbers do.
func TestOverageWindowedObservationPersistsAndProjects(t *testing.T) {
	dir := t.TempDir()
	store := state.Store{Path: dir + "/state.json"}
	before := state.State{
		Schema:   state.CurrentSchema,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"neuralwatt": {QuotaSnapshot: ptrSnap(overageWindowedSnap("neuralwatt"))},
		},
	}
	if err := store.Save(before); err != nil {
		t.Fatalf("save: %v", err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	snap := after.Providers["neuralwatt"].QuotaSnapshot
	if snap == nil {
		t.Fatal("snapshot lost in persistence")
	}
	if snap.Condition != quota.ConditionInOverage || snap.Availability != quota.QuotaUnavailable || snap.Status != quota.SourceFresh {
		t.Fatalf("condition/availability/status lost: %+v", snap)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Used == nil || *snap.Windows[0].Used != 2.911 {
		t.Fatalf("window numbers lost: %+v", snap.Windows)
	}
	wantReset := time.Date(2026, 9, 12, 21, 48, 47, 250000000, time.UTC)
	if snap.Windows[0].ResetAt == nil || !snap.Windows[0].ResetAt.Equal(wantReset) {
		t.Fatalf("reset lost: %+v", snap.Windows[0].ResetAt)
	}

	// The read projections must show the numbers, the reset, and the
	// condition, not collapse the row to a windowless label.
	report := windowsReport(snap)
	if len(report) != 1 || report[0].Used == nil || *report[0].Used != 2.911 {
		t.Fatalf("windows projection lost numbers: %+v", report)
	}
	asOf := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	reset := quota.NextQuotaResetAt(snap.Windows, asOf)
	if reset == nil || !reset.Equal(wantReset) {
		t.Fatalf("next-reset projection = %v, want %v", reset, wantReset)
	}
	if got := quota.NormalizeQuotaCondition(snap.Condition); got != quota.ConditionInOverage {
		t.Fatalf("condition normalization = %q", got)
	}

	desired := overageDesired()
	providers, errs := projectProviders(desired, after, asOf)
	if len(errs) != 0 || len(providers) != 1 {
		t.Fatalf("provider projection errors=%v providers=%+v", errs, providers)
	}
	projection := providers[0]
	if projection.Condition != quota.ConditionInOverage || projection.SnapshotAvailability != quota.QuotaUnavailable ||
		len(projection.Windows) != 1 || projection.Windows[0].Used == nil || *projection.Windows[0].Used != 2.911 ||
		projection.NextResetAt == nil || !projection.NextResetAt.Equal(wantReset) {
		t.Fatalf("structured projection lost overage details: %+v", projection)
	}
}

// TestInvalidOverageReplacesPriorHealthyObservation proves an in-overage
// observation whose details are invalid — a windowless fresh unavailable
// snapshot naming the condition, never a failure — still replaces a prior
// healthy last-good snapshot instead of retaining stale healthy numbers.
func TestInvalidOverageReplacesPriorHealthyObservation(t *testing.T) {
	priorGood := freshSnap("neuralwatt", 10)
	observed := state.State{
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"neuralwatt": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
	}
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.NeuralwattEvidence(now))
	source := quota.NewNeuralwattSource("neuralwatt", &quota.BoundedClient{
		Transport: malformedOverageTransport{},
	}, malformedOverageResolver{}, reg, now)
	invalid, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch malformed overage: %v", err)
	}
	if invalid.Status != quota.SourceFresh || invalid.Availability != quota.QuotaUnavailable || invalid.Condition != quota.ConditionInOverage || len(invalid.Windows) != 0 {
		t.Fatalf("invalid overage snapshot=%+v", invalid)
	}
	next := applyQuotaObservations(observed, overageDesired(), map[string]quota.QuotaSnapshot{"neuralwatt": invalid})

	snap := next.Providers["neuralwatt"].QuotaSnapshot
	if snap == nil || snap.Condition != quota.ConditionInOverage || len(snap.Windows) != 0 {
		t.Fatalf("prior healthy snapshot retained over invalid overage observation: %+v", snap)
	}
}

type malformedOverageTransport struct{}

func (malformedOverageTransport) Do(*http.Request) (*http.Response, error) {
	body := `{"snapshot_at":"2026-08-20T00:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"subscription":{"in_overage":true,"kwh_included":5,"kwh_remaining":0,"kwh_used":"bad"}}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

type malformedOverageResolver struct{}

func (malformedOverageResolver) Resolve(quota.CredentialRef) (string, error) {
	return "synthetic-key", nil
}

func overageDesired() policy.Desired {
	return policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"neuralwatt": {
				Quota:  &policy.QuotaConfig{Adapter: "neuralwatt", FreshnessTTL: 30 * time.Minute},
				Models: map[string]policy.ModelBaseline{"neuralwatt/model": {Enabled: true}},
			},
		},
	}
}

func ptrSnap(s quota.QuotaSnapshot) *quota.QuotaSnapshot { return &s }
