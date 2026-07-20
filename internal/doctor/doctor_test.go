package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// --- test stubs implementing the inspector interfaces -----------------------

type policyInspectorStub struct{ conds map[string]bool }

func (s policyInspectorStub) Findings(context.Context) []Finding {
	var out []Finding
	if s.conds["schema"] {
		out = append(out, Finding{
			Code: "policy-schema", Severity: Error,
			Message:     "desired.yaml has a schema error",
			Remediation: "fix the desired.yaml schema and reload",
		})
	}
	if s.conds["ambiguous"] {
		out = append(out, Finding{
			Code: "mapping-ambiguous", Severity: Error,
			Message:     "a codexbar or polytoken provider is ambiguous across mappings",
			Remediation: "disambiguate the provider mapping",
		})
	}
	if s.conds["stale"] {
		out = append(out, Finding{
			Code: "enumeration-stale", Severity: Warning,
			Message:     "the live configuration exposes models or references not in the desired enumeration",
			Remediation: "refresh the enumeration or add the new references to desired policy",
		})
	}
	return out
}

type targetInspectorStub struct{ conds map[string]bool }

func (s targetInspectorStub) Findings(context.Context) []Finding {
	var out []Finding
	if s.conds["uncovered"] {
		out = append(out, Finding{
			Code: "model-uncovered", Severity: Warning,
			Message:     "a live model reference is not covered by any mapping",
			Remediation: "add the model to a mapping or import it",
		})
	}
	if s.conds["drift"] {
		out = append(out, Finding{
			Code: "managed-drift", Severity: Warning,
			Message:     "a managed field differs from the desired policy",
			Remediation: "run reconcile to converge",
		})
	}
	if s.conds["empty"] {
		out = append(out, Finding{
			Code: "empty-chain", Severity: Error,
			Message:     "a required chain has no survivor",
			Remediation: "add cross-provider fallback models",
		})
	}
	if s.conds["provider-loss"] {
		out = append(out, Finding{
			Code: "provider-loss", Severity: Warning,
			Message:     "a desired chain cannot survive loss of a mapped provider",
			Remediation: "add models from an independent provider",
		})
	}
	return out
}

type liveValidatorStub struct{ conds map[string]bool }

func (s liveValidatorStub) Findings(context.Context) []Finding {
	var out []Finding
	if s.conds["invalid-live"] {
		out = append(out, Finding{
			Code: "config-invalid", Severity: Error,
			Message:     "the current live configuration fails validation",
			Remediation: "inspect the sanitized validation output and fix the config",
		})
	}
	if s.conds["symlink"] {
		out = append(out, Finding{
			Code: "definition-symlink", Severity: Warning,
			Message:     "a managed definition file is a broken or unexpected symlink",
			Remediation: "repair or remove the symlink and republish the definition",
		})
	}
	return out
}

type publishInspectorStub struct{ conds map[string]bool }

func (s publishInspectorStub) Findings(context.Context) []Finding {
	var out []Finding
	if s.conds["journal"] {
		out = append(out, Finding{
			Code: "journal-incomplete", Severity: Warning,
			Message:     "an apply journal is incomplete and needs recovery",
			Remediation: "run reconcile to roll forward or restore",
		})
	}
	if s.conds["permission"] {
		out = append(out, Finding{
			Code: "permission", Severity: Warning,
			Message:     "a backup or utility-root permission issue was detected",
			Remediation: "check directory and file permissions",
		})
	}
	return out
}

// noopInspector contributes no findings.
type noopInspector struct{}

func (noopInspector) Findings(context.Context) []Finding { return nil }

// --- fixtures ---------------------------------------------------------------

// doctorFixture builds Dependencies whose inspectors emit a finding for every
// named condition. "pending" seeds persisted state with a target carrying a
// structured ApplyFailure.
func doctorFixture(t *testing.T, conditions ...string) Dependencies {
	t.Helper()
	conds := make(map[string]bool)
	for _, c := range conditions {
		conds[c] = true
	}

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{
		Schema:    1,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
	}
	if conds["pending"] {
		st.Targets["global"] = state.TargetState{
			Pending: &state.ApplyFailure{
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
			},
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	now := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

	return Dependencies{
		Policy:    policyInspectorStub{conds},
		Targets:   targetInspectorStub{conds},
		Validator: liveValidatorStub{conds},
		Publisher: publishInspectorStub{conds},
		State: state.Store{
			Path:               statePath,
			Now:                now,
			RecoveredRetention: 7 * 24 * time.Hour,
		},
		Now: now,
	}
}

// recoveredOnlyFixture builds Dependencies with no actionable findings and a
// single recovered error resolved at resolvedAt.
func recoveredOnlyFixture(t *testing.T, resolvedAt time.Time, retention time.Duration, now func() time.Time) Dependencies {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{
		Schema:    1,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
		Recovered: []state.ApplyFailure{{
			TargetID:   "global",
			Stage:      "publish",
			Summary:    "transient publish failure",
			ResolvedAt: resolvedAt,
			LiveStatus: "last-known-good",
		}},
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	return Dependencies{
		Policy:    noopInspector{},
		Targets:   noopInspector{},
		Validator: noopInspector{},
		Publisher: noopInspector{},
		State: state.Store{
			Path:               statePath,
			Now:                now,
			RecoveredRetention: retention,
		},
		Now: now,
	}
}

// --- helpers ----------------------------------------------------------------

func findingCodes(r Report) []string {
	codes := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		codes = append(codes, f.Code)
	}
	return codes
}

func pendingFinding(t *testing.T, r Report) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Code == "target-pending" {
			return f
		}
	}
	t.Fatal("no target-pending finding in report")
	return Finding{}
}

// --- tests ------------------------------------------------------------------

// TestDoctorFindingsAndPersistedErrorFields proves Run emits a finding for every
// static/live/pending condition and that the persisted pending target error
// carries the required detail fields.
func TestDoctorFindingsAndPersistedErrorFields(t *testing.T) {
	r := Run(context.Background(), doctorFixture(t,
		"schema", "ambiguous", "stale", "uncovered", "drift", "empty",
		"provider-loss", "invalid-live", "symlink", "journal", "permission", "pending"))
	codes := findingCodes(r)
	for _, want := range []string{
		"policy-schema", "mapping-ambiguous", "enumeration-stale",
		"model-uncovered", "managed-drift", "empty-chain", "provider-loss",
		"config-invalid", "definition-symlink", "journal-incomplete",
		"permission", "target-pending",
	} {
		if !slices.Contains(codes, want) {
			t.Errorf("missing %s in %v", want, codes)
		}
	}

	f := pendingFinding(t, r)
	// Locating detail fields are populated.
	for name, value := range map[string]string{
		"file":        f.File,
		"chain":       f.Chain,
		"remediation": f.Remediation,
	} {
		if value == "" {
			t.Errorf("empty %s", name)
		}
	}
	// Every persisted-error detail token appears in the message.
	for _, want := range []string{
		"stage=render",
		"attempted_revision=3",
		"attempted_at=2026-07-19 11:00:00 +0000 UTC",
		"last_successful_revision=2",
		"last_successful_at=2026-07-19 10:00:00 +0000 UTC",
		`summary="render failed: empty chain"`,
		"reproduces=true",
		"live_status=last-known-good",
	} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message missing %q\nmessage: %s", want, f.Message)
		}
	}
}

// TestRecoveredRetentionVisibilityAndExit proves that recovered-only errors
// strictly within the retention window remain visible and the report is not
// actionable.
func TestRecoveredRetentionVisibilityAndExit(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	retention := 24 * time.Hour
	r := Run(context.Background(),
		recoveredOnlyFixture(t, now.Add(-retention+time.Second), retention, func() time.Time { return now }))
	if r.Actionable() || len(r.Recovered) != 1 {
		t.Fatalf("actionable=%v recovered=%d: %+v", r.Actionable(), len(r.Recovered), r)
	}
}

// TestRecoveredRetentionExpiredAbsent proves that recovered errors at or beyond
// the retention boundary are pruned and absent from the report.
func TestRecoveredRetentionExpiredAbsent(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	retention := 24 * time.Hour
	r := Run(context.Background(),
		recoveredOnlyFixture(t, now.Add(-retention), retention, func() time.Time { return now }))
	if len(r.Recovered) != 0 {
		t.Fatalf("expected 0 recovered after expiry, got %d", len(r.Recovered))
	}
}

// TestPendingTargetIsActionable proves a pending target makes the report
// actionable so doctor exits nonzero.
func TestPendingTargetIsActionable(t *testing.T) {
	r := Run(context.Background(), doctorFixture(t, "pending"))
	if !r.Actionable() {
		t.Fatal("pending target must be actionable")
	}
}

// TestNilInspectorsAreSafe proves Run does not panic when inspectors are nil.
func TestNilInspectorsAreSafe(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	_ = os.WriteFile(statePath, []byte(`{"schema":1}`), 0o600)
	r := Run(context.Background(), Dependencies{
		State: state.Store{Path: statePath, RecoveredRetention: time.Hour},
		Now:   func() time.Time { return time.Now() },
	})
	if r.Actionable() {
		t.Fatal("empty report must not be actionable")
	}
}
