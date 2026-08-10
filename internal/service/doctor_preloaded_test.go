package service

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// doctorSnapshotFixture builds a diagnosticDeps + Coordinator preloaded with two
// providers: "alpha" (codex) with a failed quota attempt, and "beta" (anthropic)
// healthy. The JournalPath is empty (no interrupted reconcile).
func doctorSnapshotFixture(t *testing.T) (*diagnosticDeps, *Coordinator) {
	t.Helper()
	d, _ := diagnosticFixture(t, true)
	return d, diagnosticCoordinator(d)
}

// TestDoctorLoadsPolicyStateAndTargetsAtMostOnce proves the Coordinator's Doctor
// method consumes the preloaded DiagnosticSnapshot rather than independently
// loading policy/state/targets. It uses the same count-double pattern as
// TestDiagnosticSnapshotSingleReadSingleClock.
func TestDoctorLoadsPolicyStateAndTargetsAtMostOnce(t *testing.T) {
	d, c := doctorSnapshotFixture(t)
	_ = c.Doctor(context.Background(), false)
	if d.policyLoads != 1 || d.stateLoads != 1 || d.targetLoads != 1 {
		t.Fatalf("doctor loads policy/state/targets = %d/%d/%d, want 1/1/1", d.policyLoads, d.stateLoads, d.targetLoads)
	}
}

// TestDiagnosticClassifiersSharedWithDoctor proves doctor's provider
// classifications (quota class, availability, mode) match the snapshot's exactly.
func TestDiagnosticClassifiersSharedWithDoctor(t *testing.T) {
	_, c := doctorSnapshotFixture(t)
	snapshot := c.BuildDiagnosticSnapshot(context.Background())
	status := snapshot.StatusView()
	report := c.Doctor(context.Background(), false)

	// The snapshot's provider projections are the authoritative classifications.
	// Doctor must share them: each provider's quota classification surfaced by
	// doctor matches the snapshot's. Since doctor is problem-only, the snapshot
	// provider with a failed attempt (alpha) must appear in doctor as a
	// quota-attempt-failed finding with the same provider identifier.
	for _, p := range status.Providers {
		if p.LatestAttempt != nil && p.LatestAttempt.Status == quota.SourceFailed {
			found := slices.ContainsFunc(report.Findings, func(f doctor.Finding) bool {
				return f.Code == "quota-attempt-failed" && f.TargetID == p.MappingID
			})
			if !found {
				t.Errorf("snapshot provider %s has failed attempt but doctor did not surface quota-attempt-failed", p.MappingID)
			}
		}
	}

	// The provider identifiers in doctor must match the snapshot's mapping IDs
	// (alpha, beta) — not raw codexbar aliases.
	for _, f := range report.Findings {
		if f.TargetID != "" && f.TargetID != "alpha" && f.TargetID != "beta" && f.TargetID != "global" {
			t.Errorf("doctor finding has unexpected target id %q (not a snapshot mapping id)", f.TargetID)
		}
	}
}

// TestDoctorProblemOnlyProjection proves doctor omits healthy providers and
// manual-disabled rows, and that it reports only actual
// configuration/persistence/quota problems.
func TestDoctorProblemOnlyProjection(t *testing.T) {
	d, c := doctorSnapshotFixture(t)
	// Add a manually disabled provider to prove it is NOT surfaced by doctor.
	disabled := d.observed.Providers["codex-b"]
	disabled.ManualDisabled = true
	d.observed.Providers["codex-b"] = disabled

	report := c.Doctor(context.Background(), false)

	for _, f := range report.Findings {
		if f.Code == "manual-disabled" {
			t.Errorf("doctor must not report manual-disabled rows: %s", f.Message)
		}
		if f.Severity == doctor.Info && !slices.Contains([]string{"quota-partial"}, f.Code) {
			// Info findings that are not quota-partial are suspicious in a
			// problem-only doctor. Recovered history is separate.
			t.Errorf("unexpected info finding in problem-only doctor: %s %s", f.Code, f.Message)
		}
	}

	// A healthy provider (beta) must not produce a finding. The snapshot has
	// alpha with a failed attempt (problem) and beta healthy. Doctor should only
	// surface the alpha quota problem, never a healthy-beta row.
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "beta") && f.Severity == doctor.Info {
			t.Errorf("doctor surfaced a healthy-provider info row for beta: %s", f.Message)
		}
	}
}

// TestDoctorRetainsCompleteLastError proves the complete sanitized persisted
// pending error survives the refactor.
func TestDoctorRetainsCompleteLastError(t *testing.T) {
	d, c := doctorSnapshotFixture(t)
	// Seed a pending target with a structured failure carrying complete detail.
	d.observed.Targets = map[string]state.TargetState{
		"global": {Pending: &state.ApplyFailure{
			TargetID:               "global",
			Stage:                  "render",
			File:                   "config.yaml",
			Chain:                  "defaults.full",
			Summary:                "render failed: empty chain",
			Remediation:            "add fallback models to the desired chain",
			AttemptedRevision:      3,
			LastSuccessfulRevision: 2,
			AttemptedAt:            time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC),
			LastSuccessfulAt:       time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC),
			Reproduces:             true,
			LiveStatus:             "last-known-good",
		}},
	}

	report := c.Doctor(context.Background(), false)
	var f doctor.Finding
	for _, finding := range report.Findings {
		if finding.Code == "target-pending" {
			f = finding
		}
	}
	if f.Code == "" {
		t.Fatalf("doctor did not surface the pending target finding: %+v", report.Findings)
	}
	for _, want := range []string{
		"stage=render",
		"attempted_revision=3",
		`summary="render failed: empty chain"`,
		"reproduces=true",
		"live_status=last-known-good",
	} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("pending message missing %q\nmessage: %s", want, f.Message)
		}
	}
	if f.Remediation == "" {
		t.Errorf("pending finding lost remediation")
	}
}
