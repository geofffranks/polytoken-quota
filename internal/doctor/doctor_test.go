package doctor

import (
	"context"
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
// named condition. "pending" seeds the observed state with a target carrying a
// structured ApplyFailure.
func doctorFixture(t *testing.T, conditions ...string) Dependencies {
	t.Helper()
	conds := make(map[string]bool)
	for _, c := range conditions {
		conds[c] = true
	}

	observed := state.State{
		Schema:    1,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
	}
	if conds["pending"] {
		observed.Targets["global"] = state.TargetState{
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

	now := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

	return Dependencies{
		Policy:    policyInspectorStub{conds},
		Targets:   targetInspectorStub{conds},
		Validator: liveValidatorStub{conds},
		Publisher: publishInspectorStub{conds},
		Observed:  observed,
		Now:       now,
	}
}

// recoveredOnlyFixture builds Dependencies with no actionable findings and a
// single recovered error resolved at resolvedAt.
func recoveredOnlyFixture(t *testing.T, resolvedAt time.Time, retention time.Duration, now func() time.Time) Dependencies {
	t.Helper()
	observed := state.State{
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

	return Dependencies{
		Policy:             noopInspector{},
		Targets:            noopInspector{},
		Validator:          noopInspector{},
		Publisher:          noopInspector{},
		Observed:           observed,
		RecoveredRetention: retention,
		Now:                now,
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

func TestLegacyAndOrphanedDiscoverabilityFindings(t *testing.T) {
	t.Run("legacy and orphaned", func(t *testing.T) {
		observed := state.State{Providers: map[string]state.ProviderState{
			"configured":               {},
			"orphaned":                 {},
			"orphaned\napi_key=CANARY": {},
		}}
		r := Run(context.Background(), Dependencies{
			DesiredRaw:       []byte("version: 1\ncodexbar_providers:\n  CANARY-account: {}\npolytoken_providers: {}\n"),
			DesiredProviders: map[string]struct{}{"configured": {}},
			Observed:         observed,
		})

		var legacy, orphaned *Finding
		for i := range r.Findings {
			switch r.Findings[i].Code {
			case "legacy-config-keys":
				legacy = &r.Findings[i]
			case "orphaned-provider-state":
				orphaned = &r.Findings[i]
			}
		}
		if legacy == nil || orphaned == nil {
			t.Fatalf("missing discoverability findings: %+v", r.Findings)
		}
		for _, f := range []*Finding{legacy, orphaned} {
			if f.Severity != Info {
				t.Errorf("finding %s severity=%q, want %q", f.Code, f.Severity, Info)
			}
		}
		for _, want := range []string{"codexbar_providers", "polytoken_providers"} {
			if !strings.Contains(legacy.Message, want) {
				t.Errorf("legacy message missing key %q: %s", want, legacy.Message)
			}
		}
		if !strings.Contains(orphaned.Message, "orphaned") || strings.Contains(orphaned.Message, "CANARY") || strings.Contains(orphaned.Message, "api_key") {
			t.Errorf("orphaned message was not bounded and sanitized: %s", orphaned.Message)
		}
	})

	t.Run("clean config and state", func(t *testing.T) {
		r := Run(context.Background(), Dependencies{
			DesiredRaw:       []byte("version: 1\\nproviders: {}\\n"),
			DesiredProviders: map[string]struct{}{"configured": {}},
			Observed:         state.State{Providers: map[string]state.ProviderState{"configured": {}}},
		})
		for _, f := range r.Findings {
			if f.Code == "legacy-config-keys" || f.Code == "orphaned-provider-state" {
				t.Fatalf("unexpected discoverability finding: %+v", f)
			}
		}
	})
}

// TestLegacyQuotaAdapterKeyDetection proves the ignored legacy `adapter` key
// inside a provider quota block is detected structurally (block and flow
// styles), scoped to providers.<id>.quota only.
func TestLegacyQuotaAdapterKeyDetection(t *testing.T) {
	hasLegacyAdapter := func(raw string) *Finding {
		findings := DiscoverabilityFindings([]byte(raw), nil, nil)
		for i := range findings {
			if findings[i].Code == "legacy-quota-adapter" {
				return &findings[i]
			}
		}
		return nil
	}
	t.Run("block style", func(t *testing.T) {
		raw := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\n    quota:\n      adapter: codex\n"
		f := hasLegacyAdapter(raw)
		if f == nil {
			t.Fatal("missing legacy-quota-adapter finding for block-style adapter key")
		}
		if !strings.Contains(f.Message, "codex") || !strings.Contains(f.Message, "adapter") {
			t.Errorf("finding message lacks provider or key: %s", f.Message)
		}
	})
	t.Run("flow style", func(t *testing.T) {
		raw := "version: 1\nproviders:\n  zai: {models: [zai/m], quota: {adapter: zai}}\n"
		if hasLegacyAdapter(raw) == nil {
			t.Fatal("missing legacy-quota-adapter finding for flow-style adapter key")
		}
	})
	t.Run("clean quota block", func(t *testing.T) {
		raw := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\n    quota: {}\n"
		if hasLegacyAdapter(raw) != nil {
			t.Fatal("unexpected legacy-quota-adapter finding for clean quota block")
		}
	})
	t.Run("adapter key outside quota not flagged", func(t *testing.T) {
		raw := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\nnotproviders:\n  adapter: x\n"
		if hasLegacyAdapter(raw) != nil {
			t.Fatal("unexpected legacy-quota-adapter finding for adapter key outside a quota block")
		}
	})
	t.Run("multiple providers bounded and sorted", func(t *testing.T) {
		raw := "version: 1\nproviders:\n  zai:\n    models: [zai/m]\n    quota: {adapter: zai}\n  codex:\n    models: [codex/m]\n    quota: {adapter: codex}\n"
		f := hasLegacyAdapter(raw)
		if f == nil {
			t.Fatal("missing legacy-quota-adapter finding")
		}
		if !strings.Contains(f.Message, "codex") || !strings.Contains(f.Message, "zai") {
			t.Errorf("finding message lacks providers: %s", f.Message)
		}
	})
}

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
	// Every persisted-error detail token appears in the message; the
	// never-computed reproduces flag must not.
	for _, want := range []string{
		"stage=render",
		"attempted_revision=3",
		"attempted_at=2026-07-19 11:00:00 +0000 UTC",
		"last_successful_revision=2",
		"last_successful_at=2026-07-19 10:00:00 +0000 UTC",
		`summary="render failed: empty chain"`,
		"live_status=last-known-good",
	} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message missing %q\nmessage: %s", want, f.Message)
		}
	}
	if strings.Contains(f.Message, "reproduces=") {
		t.Errorf("message reports a never-computed reproduces flag: %s", f.Message)
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

func TestManualDisabledProviderNotSurfaced(t *testing.T) {
	// Doctor is problem-only: a manual disable is NOT a configuration fault and
	// must not appear as a finding.
	observed := state.State{Schema: 1, Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, Targets: map[string]state.TargetState{}}
	r := Run(context.Background(), Dependencies{Observed: observed, Now: time.Now})
	if r.Actionable() {
		t.Fatal("manual disable should not be actionable")
	}
	for _, f := range r.Findings {
		if f.Code == "manual-disabled" {
			t.Fatalf("doctor must not report manual-disabled rows: %+v", f)
		}
	}
}

// TestNilInspectorsAreSafe proves Run does not panic when inspectors are nil.
func TestDoctorSurfacesPersistedManualResolutionFailure(t *testing.T) {
	observed := state.State{Schema: 1, Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, Targets: map[string]state.TargetState{
		"manual-resolution": {Pending: &state.ApplyFailure{TargetID: "manual-resolution", Stage: "resolve_targets", Summary: "target resolution failed", Remediation: "fix policy"}},
	}}
	r := Run(context.Background(), Dependencies{Observed: observed, Now: time.Now})
	if !r.Actionable() || !slices.Contains(findingCodes(r), "target-pending") {
		t.Fatalf("report did not surface persisted failure: %+v", r)
	}
}

// TestDoctorSanitizesTamperedPendingFailure proves a secret-bearing pending
// failure written directly into the state file (bypassing the sanitizing Save
// path) is re-sanitized at the doctor output boundary in both message and
// structured fields.
func TestDoctorSanitizesTamperedPendingFailure(t *testing.T) {
	observed := state.State{Schema: 1, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{
		"global": {Pending: &state.ApplyFailure{
			TargetID:    "global\napi_key=CANARY-tampered",
			Stage:       "publish",
			File:        "/home/alice/.config/polytoken/config.yaml",
			Summary:     "failed: Authorization: Bearer CANARY-tampered-token",
			Remediation: "token=CANARY-tampered rerun",
			LiveStatus:  "last-known-good",
		}},
	}}
	r := Run(context.Background(), Dependencies{Observed: observed, Now: time.Now})
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
	r := Run(context.Background(), Dependencies{
		Observed:           state.State{Schema: 1},
		RecoveredRetention: time.Hour,
		Now:                func() time.Time { return time.Now() },
	})
	if r.Actionable() {
		t.Fatal("empty report must not be actionable")
	}
}

// --- quota diagnostic tests (Task 9 Part A) ---------------------------------

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

// TestDoctorRunEvaluatesQuotaProbes verifies Run evaluates the preloaded
// QuotaProbes through the pure QuotaFindings classifier and folds the results
// into the report.
func TestDoctorRunEvaluatesQuotaProbes(t *testing.T) {
	deps := doctorFixture(t)
	deps.QuotaProbes = []QuotaProbe{{
		Provider:       "codex",
		HasQuotaConfig: true,
		Supported:      true,
		Attempt: &quota.QuotaSnapshot{
			Status: quota.SourceFailed,
			Error:  "codex: server error (HTTP 503)",
		},
	}}
	r := Run(context.Background(), deps)
	if !slices.Contains(findingCodes(r), "quota-attempt-failed") {
		t.Fatalf("quota finding missing from report: %v", findingCodes(r))
	}
	if !r.Actionable() {
		t.Fatal("warning quota finding must be actionable")
	}
}

// TestDoctorRunNilQuotaProbesSafe verifies Run does not panic and produces no
// quota findings when QuotaProbes is empty.
func TestDoctorRunNilQuotaProbesSafe(t *testing.T) {
	r := Run(context.Background(), doctorFixture(t))
	// No panic, and no quota findings (probes are empty).
	for _, f := range r.Findings {
		if strings.HasPrefix(f.Code, "quota-") {
			t.Fatalf("unexpected quota finding with no probes: %+v", f)
		}
	}
}
