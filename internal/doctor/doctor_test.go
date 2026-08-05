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

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
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

func TestManualDisabledProviderIsInformational(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{Schema: 1, Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, Targets: map[string]state.TargetState{}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), Dependencies{State: state.Store{Path: statePath}, Now: time.Now})
	if r.Actionable() {
		t.Fatal("manual disable should not be actionable")
	}
	if len(r.Findings) != 1 || r.Findings[0].Code != "manual-disabled" || r.Findings[0].Severity != Info {
		t.Fatalf("findings=%+v", r.Findings)
	}
	if !strings.Contains(r.Findings[0].Message, "codex") || !strings.Contains(r.Findings[0].Remediation, "enable codex") {
		t.Fatalf("finding=%+v", r.Findings[0])
	}
}

func TestManualDisabledFindingsAreSortedAndSanitized(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{Schema: 1, Providers: map[string]state.ProviderState{
		"zai path": {ManualDisabled: true}, "codex\nsecret": {ManualDisabled: true},
	}, Targets: map[string]state.TargetState{}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), Dependencies{State: state.Store{Path: statePath}, Now: time.Now})
	if len(r.Findings) != 2 {
		t.Fatalf("findings=%+v", r.Findings)
	}
	if strings.Contains(r.Findings[0].Message, "\n") || strings.Contains(r.Findings[0].Message, "secret") {
		t.Fatalf("provider text leaked: %+v", r.Findings[0])
	}
	if r.Findings[0].Message > r.Findings[1].Message {
		t.Fatalf("findings not sorted: %+v", r.Findings)
	}
}

// TestNilInspectorsAreSafe proves Run does not panic when inspectors are nil.
func TestDoctorSurfacesPersistedManualResolutionFailure(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{Schema: 1, Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, Targets: map[string]state.TargetState{
		"manual-resolution": {Pending: &state.ApplyFailure{TargetID: "manual-resolution", Stage: "resolve_targets", Summary: "target resolution failed", Remediation: "fix policy"}},
	}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), Dependencies{State: state.Store{Path: statePath}, Now: time.Now})
	if !r.Actionable() || !slices.Contains(findingCodes(r), "target-pending") {
		t.Fatalf("report did not surface persisted failure: %+v", r)
	}
}

// TestDoctorSanitizesTamperedPendingFailure proves a secret-bearing pending
// failure written directly into the state file (bypassing the sanitizing Save
// path) is re-sanitized at the doctor output boundary in both message and
// structured fields.
func TestDoctorSanitizesTamperedPendingFailure(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := state.State{Schema: 1, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{
		"global": {Pending: &state.ApplyFailure{
			TargetID:    "global\napi_key=CANARY-tampered",
			Stage:       "publish",
			File:        "/home/alice/.config/polytoken/config.yaml",
			Summary:     "failed: Authorization: Bearer CANARY-tampered-token",
			Remediation: "token=CANARY-tampered rerun",
			LiveStatus:  "last-known-good",
		}},
	}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), Dependencies{State: state.Store{Path: statePath}, Now: time.Now})
	f := pendingFinding(t, r)
	for _, field := range []string{f.Message, f.TargetID, f.File, f.Chain, f.Remediation} {
		if strings.Contains(field, "CANARY-tampered") {
			t.Fatalf("tampered secret leaked through doctor output: %+v", f)
		}
		if strings.Contains(field, "alice") {
			t.Fatalf("home path leaked through doctor output: %+v", f)
		}
	}
}

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

// --- quota diagnostic tests (Task 9 Part A) ---------------------------------

// quotaInspectorStub returns a fixed set of findings, recording that it was
// called.
type quotaInspectorStub struct {
	called   bool
	findings []Finding
}

func (s *quotaInspectorStub) Findings(context.Context) []Finding {
	s.called = true
	return s.findings
}

func ptrTime(t time.Time) *time.Time { return &t }

// freshSnapshot builds a QuotaSnapshot with the given status and checked-at.
func freshSnapshot(status quota.SourceStatus, checkedAt time.Time) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		MappingID:    "codex",
		CheckedAt:    checkedAt,
		Status:       status,
		Availability: quota.QuotaAvailable,
		Windows:      []quota.QuotaWindow{{Name: "daily", UsagePercent: ptrFloat(20)}},
	}
}

func ptrFloat(v float64) *float64 { return &v }

// TestQuotaFindingsStaleSnapshot verifies a snapshot past its freshness TTL
// produces a warning finding with the expected code and message.
func TestQuotaFindingsStaleSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	checked := now.Add(-2 * time.Hour)
	probes := []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		FreshnessTTL:   30 * time.Minute,
		Snapshot:       freshSnapshot(quota.SourceFresh, checked),
		Supported:      true,
	}}
	findings := QuotaFindings(probes, false, now)
	if len(findings) != 1 || findings[0].Code != "quota-stale-snapshot" || findings[0].Severity != Warning {
		t.Fatalf("findings=%+v", findings)
	}
	if !strings.Contains(findings[0].Message, "stale") || !strings.Contains(findings[0].Message, "30m") {
		t.Fatalf("message=%q", findings[0].Message)
	}
}

// TestQuotaFindingsPartialData verifies a SourcePartial snapshot produces an
// info finding.
func TestQuotaFindingsPartialData(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	probes := []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		FreshnessTTL:   30 * time.Minute,
		Snapshot:       freshSnapshot(quota.SourcePartial, now),
		Supported:      true,
	}}
	findings := QuotaFindings(probes, false, now)
	if len(findings) != 1 || findings[0].Code != "quota-partial" || findings[0].Severity != Info {
		t.Fatalf("findings=%+v", findings)
	}
	if !strings.Contains(findings[0].Message, "partial") {
		t.Fatalf("message=%q", findings[0].Message)
	}
}

func TestQuotaFindingsFreshUnusableSnapshotIsActionable(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for _, snap := range []*quota.QuotaSnapshot{
		{MappingID: "codex", CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Name: "daily"}}},
		{MappingID: "codex", CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaUnknown, Windows: []quota.QuotaWindow{{Name: "daily", UsagePercent: ptrFloat(20)}}},
	} {
		findings := QuotaFindings([]QuotaProbe{{Provider: "codex", HasQuotaConfig: true, FreshnessTTL: time.Hour, Snapshot: snap, Supported: true}}, false, now)
		if len(findings) != 1 || findings[0].Severity != Warning {
			t.Fatalf("snapshot=%+v findings=%+v, want one warning", snap, findings)
		}
	}
}

// TestQuotaFindingsUnsupportedAdapter verifies a provider with a quota config
// but an unsupported adapter produces a warning finding.
func TestQuotaFindingsUnsupportedAdapter(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	probes := []QuotaProbe{{
		Provider:       "zai",
		HasQuotaConfig: true,
		Supported:      false,
		SupportReason:  "provider zai contract evidence expired; re-verify and update",
	}}
	findings := QuotaFindings(probes, false, now)
	if len(findings) != 1 || findings[0].Code != "quota-adapter-unsupported" || findings[0].Severity != Warning {
		t.Fatalf("findings=%+v", findings)
	}
	if !strings.Contains(findings[0].Message, "adapter unsupported") {
		t.Fatalf("message=%q", findings[0].Message)
	}
}

// TestQuotaFindingsFailedAttempt verifies a SourceFailed attempt produces a
// warning finding with the sanitized error summary.
func TestQuotaFindingsFailedAttempt(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	probes := []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		Supported:      true,
		Attempt: &quota.QuotaSnapshot{
			Status: quota.SourceFailed,
			Error:  "codex: server error (HTTP 503)",
		},
	}}
	findings := QuotaFindings(probes, false, now)
	if len(findings) != 1 || findings[0].Code != "quota-attempt-failed" || findings[0].Severity != Warning {
		t.Fatalf("findings=%+v", findings)
	}
	if !strings.Contains(findings[0].Message, "HTTP 503") {
		t.Fatalf("message=%q", findings[0].Message)
	}
}

// TestQuotaFindingsReconcilePending verifies the pending-reconcile flag produces
// a warning finding.
func TestQuotaFindingsReconcilePending(t *testing.T) {
	findings := QuotaFindings(nil, true, time.Now())
	if len(findings) != 1 || findings[0].Code != "quota-reconcile-pending" || findings[0].Severity != Warning {
		t.Fatalf("findings=%+v", findings)
	}
	if !strings.Contains(findings[0].Message, "interrupted") {
		t.Fatalf("message=%q", findings[0].Message)
	}
}

// TestQuotaFindingsSanitized verifies that secret-bearing strings in probe data
// are stripped from finding messages (defense in depth).
func TestQuotaFindingsSanitized(t *testing.T) {
	probes := []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		Supported:      true,
		Attempt: &quota.QuotaSnapshot{
			Status: quota.SourceFailed,
			Error:  "bearer sk-secret-token-here api_key=sk-live-1234567890wxyz",
		},
	}}
	findings := QuotaFindings(probes, false, time.Now())
	for _, f := range findings {
		for _, secret := range []string{"sk-secret-token-here", "sk-live-1234567890wxyz"} {
			if strings.Contains(f.Message, secret) {
				t.Fatalf("secret %q leaked in finding: %s", secret, f.Message)
			}
		}
	}
}

// TestQuotaFindingsHealthyProviderNoFinding verifies a healthy provider with a
// fresh snapshot, supported adapter, and no failed attempt produces no finding.
func TestQuotaFindingsHealthyProviderNoFinding(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	probes := []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		FreshnessTTL:   30 * time.Minute,
		Snapshot:       freshSnapshot(quota.SourceFresh, now),
		Supported:      true,
		Attempt:        freshSnapshot(quota.SourceFresh, now),
	}}
	findings := QuotaFindings(probes, false, now)
	if len(findings) != 0 {
		t.Fatalf("healthy provider produced findings: %+v", findings)
	}
}

// TestDoctorRunWiresQuotaInspector verifies Run consults the injected
// QuotaInspector and folds its findings into the report.
func TestDoctorRunWiresQuotaInspector(t *testing.T) {
	qi := &quotaInspectorStub{findings: []Finding{{
		Code: "quota-attempt-failed", Severity: Warning, Message: "provider codex quota attempt failed: HTTP 503",
	}}}
	deps := doctorFixture(t)
	deps.Quota = qi
	r := Run(context.Background(), deps)
	if !qi.called {
		t.Fatal("quota inspector was not called")
	}
	if !slices.Contains(findingCodes(r), "quota-attempt-failed") {
		t.Fatalf("quota finding missing from report: %v", findingCodes(r))
	}
	if !r.Actionable() {
		t.Fatal("warning quota finding must be actionable")
	}
}

// TestDoctorRunNilQuotaInspectorSafe verifies Run does not panic when the quota
// inspector is nil.
func TestDoctorRunNilQuotaInspectorSafe(t *testing.T) {
	r := Run(context.Background(), doctorFixture(t))
	// No panic, and no quota findings (inspector is nil).
	for _, f := range r.Findings {
		if strings.HasPrefix(f.Code, "quota-") {
			t.Fatalf("unexpected quota finding with nil inspector: %+v", f)
		}
	}
}
