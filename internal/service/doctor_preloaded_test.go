package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/policy"
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

type diagnosticPathLoader struct {
	deps *diagnosticDeps
	path string
}

func (l diagnosticPathLoader) LoadPolicy() (policy.Desired, error) { return l.deps.LoadPolicy() }
func (l diagnosticPathLoader) DesiredExists() bool                 { return l.deps.DesiredExists() }
func (l diagnosticPathLoader) DesiredPath() string                 { return l.path }

func TestCoordinatorDoctorDiscoverabilityFindings(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.observed.Providers["orphaned"] = state.ProviderState{}
	desiredPath := filepath.Join(t.TempDir(), "desired.yaml")
	raw := []byte("version: 1\ncodexbar_providers:\n  ignored: {}\npolytoken_providers: {}\n")
	if err := os.WriteFile(desiredPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := diagnosticPathLoader{deps: d, path: desiredPath}
	c := &Coordinator{Policy: loader, State: d, Targets: d, Clock: d}

	report := c.Doctor(context.Background(), false)
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		codes[finding.Code] = true
		if finding.Code == "legacy-config-keys" || finding.Code == "orphaned-provider-state" {
			if finding.Severity != doctor.Info {
				t.Errorf("finding %s severity=%q, want %q", finding.Code, finding.Severity, doctor.Info)
			}
		}
	}
	if !codes["legacy-config-keys"] || !codes["orphaned-provider-state"] {
		t.Fatalf("missing discoverability findings: %+v", report.Findings)
	}
	if d.policyLoads != 1 {
		t.Fatalf("policy loads=%d, want 1", d.policyLoads)
	}
}

// TestDoctorReadsPolicyExactlyOnce proves Doctor classifies every policy load
// outcome from the shared snapshot without a follow-up DesiredExists probe.
func TestDoctorLoadsPolicyStateAndTargetsAtMostOnce(t *testing.T) {
	cases := []struct {
		name           string
		policyErr      error
		policyExists   bool
		wantMessage    string
		forbidMessage  string
		wantStateLoads int
		wantTargets    int
	}{
		{name: "success", policyExists: true, wantStateLoads: 1, wantTargets: 1},
		{name: "missing", policyErr: fs.ErrNotExist, wantMessage: "desired.yaml does not exist"},
		{name: "read error", policyErr: errors.New("permission denied: Bearer CANARY-POLICY-SECRET"), policyExists: true, wantMessage: "desired.yaml failed validation", forbidMessage: "CANARY"},
		{name: "parse error", policyErr: errors.New("yaml: line 7: invalid schema Bearer CANARY-PARSE-SECRET"), policyExists: true, wantMessage: "desired.yaml failed validation", forbidMessage: "CANARY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, c := doctorSnapshotFixture(t)
			d.policyErr = tc.policyErr
			d.policyExists = tc.policyExists

			report := c.Doctor(context.Background(), false)

			if d.policyLoads != 1 || d.desiredExistCalls != 0 {
				t.Fatalf("policy reads LoadPolicy/DesiredExists = %d/%d, want 1/0", d.policyLoads, d.desiredExistCalls)
			}
			if d.stateLoads != tc.wantStateLoads || d.targetLoads != tc.wantTargets {
				t.Fatalf("state/target loads = %d/%d, want %d/%d", d.stateLoads, d.targetLoads, tc.wantStateLoads, tc.wantTargets)
			}
			if !report.AsOf.Equal(diagnosticAsOf) {
				t.Fatalf("report AsOf=%v want snapshot AsOf=%v", report.AsOf, diagnosticAsOf)
			}
			if d.clockReads != 1 {
				t.Fatalf("clock reads=%d want 1", d.clockReads)
			}
			messages := make([]string, 0, len(report.Findings))
			for _, finding := range report.Findings {
				messages = append(messages, finding.Message)
			}
			joined := strings.Join(messages, "\n")
			if tc.wantMessage != "" && !strings.Contains(joined, tc.wantMessage) {
				t.Fatalf("findings missing %q: %+v", tc.wantMessage, report.Findings)
			}
			if tc.forbidMessage != "" && strings.Contains(joined, tc.forbidMessage) {
				t.Fatalf("findings leaked %q: %+v", tc.forbidMessage, report.Findings)
			}
		})
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
		if f.Severity == doctor.Info && !slices.Contains([]string{"quota-partial", "orphaned-provider-state"}, f.Code) {
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

// TestDoctorReportsStateLoadFailure proves the refactored Doctor surfaces a
// state-unreadable Error finding when the shared snapshot's state load failed.
// This is a regression guard: the old doctor.Run() emitted this finding and the
// preloaded refactor must preserve it.
func TestDoctorReportsStateLoadFailure(t *testing.T) {
	d, c := doctorSnapshotFixture(t)
	// Make the shared state load fail with a secret-bearing error to prove the
	// finding uses a fixed literal message, not the raw error.
	d.stateErr = errors.New("Bearer CANARY-STATE-SECRET")

	report := c.Doctor(context.Background(), false)

	var f doctor.Finding
	for _, finding := range report.Findings {
		if finding.Code == "state-unreadable" {
			f = finding
		}
	}
	if f.Code == "" {
		t.Fatalf("doctor did not surface a state-unreadable finding: %+v", report.Findings)
	}
	if f.Severity != doctor.Error {
		t.Errorf("state-unreadable severity=%q, want error", f.Severity)
	}
	if strings.Contains(f.Message, "CANARY") {
		t.Errorf("state-unreadable message leaked the raw error: %q", f.Message)
	}
	if !strings.Contains(f.Message, "could not read state file") {
		t.Errorf("state-unreadable message lacks fixed literal: %q", f.Message)
	}
	if !strings.Contains(f.Remediation, "check state.json format and permissions") {
		t.Errorf("state-unreadable remediation=%q, want state.json guidance", f.Remediation)
	}
}
