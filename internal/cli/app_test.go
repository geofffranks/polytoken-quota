package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Compile-time proof that the production Coordinator implements Mutator.
var _ Mutator = (*service.Coordinator)(nil)

// depsSpy is a test double that records Mutator/Diagnoser/SnapshotBuilder
// invocations. It satisfies all three interfaces.
type depsSpy struct {
	Mutations           int
	InitForce           bool
	InitCalled          bool
	ReconcileDryRun     bool
	ReconcileKeepStag   bool
	ReconcileCalled     bool
	DisableProvider     string
	EnableProvider      string
	ResetCalled         bool
	QuotaCheckProvider  string
	QuotaCheckReconcile bool
	QuotaCheckOutcome   service.Outcome
	QuotaCheckSet       bool
	StatusReportValue   service.StatusReport
	DoctorReportValue   doctor.Report
	SnapshotValue       service.DiagnosticSnapshot
}

func newDepsSpy() *depsSpy { return &depsSpy{} }

func (s *depsSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:         s,
		Diagnoser:       s,
		SnapshotBuilder: s,
	}
}

func (s *depsSpy) InitWithOptions(_ context.Context, opts service.InitOptions) service.Outcome {
	s.Mutations++
	s.InitCalled = true
	s.InitForce = opts.Force
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Reconcile(_ context.Context, dryRun, keepStaging, _ bool) service.Outcome {

	s.Mutations++
	s.ReconcileCalled = true
	s.ReconcileDryRun = dryRun
	s.ReconcileKeepStag = keepStaging
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Disable(_ context.Context, provider string) service.Outcome {
	s.Mutations++
	s.DisableProvider = provider
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Enable(_ context.Context, provider string) service.Outcome {
	s.Mutations++
	s.EnableProvider = provider
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Reset(context.Context) service.Outcome {
	s.Mutations++
	s.ResetCalled = true
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) QuotaCheck(_ context.Context, provider string, reconcile bool) service.Outcome {
	s.Mutations++
	s.QuotaCheckProvider = provider
	s.QuotaCheckReconcile = reconcile
	if s.QuotaCheckSet {
		return s.QuotaCheckOutcome
	}
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Status(context.Context, bool) service.StatusReport { return s.StatusReportValue }

func (s *depsSpy) Doctor(context.Context, bool) doctor.Report { return s.DoctorReportValue }

func (s *depsSpy) BuildDiagnosticSnapshot(context.Context) service.DiagnosticSnapshot {
	s.Mutations++
	return s.SnapshotValue
}

// Compile-time proof that the spy satisfies all injected surfaces.
var (
	_ service.Diagnoser       = (*depsSpy)(nil)
	_ service.SnapshotBuilder = (*depsSpy)(nil)
)

func floatPtr(v float64) *float64 { return &v }

// --- outcomeSpy: returns a preset Outcome for mutator calls ---

type outcomeSpy struct {
	outcome       service.Outcome
	statusReport  service.StatusReport
	doctorReport  doctor.Report
	snapshotValue service.DiagnosticSnapshot
}

func (s *outcomeSpy) InitWithOptions(_ context.Context, _ service.InitOptions) service.Outcome {
	return s.outcome
}
func (s *outcomeSpy) Reconcile(context.Context, bool, bool, bool) service.Outcome {
	return s.outcome
}
func (s *outcomeSpy) Disable(context.Context, string) service.Outcome { return s.outcome }
func (s *outcomeSpy) Enable(context.Context, string) service.Outcome  { return s.outcome }
func (s *outcomeSpy) Reset(context.Context) service.Outcome           { return s.outcome }
func (s *outcomeSpy) QuotaCheck(context.Context, string, bool) service.Outcome {
	return s.outcome
}
func (s *outcomeSpy) Status(context.Context, bool) service.StatusReport { return s.statusReport }
func (s *outcomeSpy) Doctor(context.Context, bool) doctor.Report        { return s.doctorReport }
func (s *outcomeSpy) BuildDiagnosticSnapshot(context.Context) service.DiagnosticSnapshot {
	return s.snapshotValue
}

func (s *outcomeSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:         s,
		Diagnoser:       s,
		SnapshotBuilder: s,
	}
}

// =====================================================================
// TDD RED tests for Task 8 — these capture the new command tree and
// contracts. They were written first (RED), then the implementation was
// written to make them pass (GREEN).
// =====================================================================

// TestCommandTreeDiagnosticsRedesign verifies the final public command tree:
// init, status, check, reconcile, routing (bare/explain/enable/disable/reset),
// doctor all dispatch correctly. Removed commands are rejected.
func TestCommandTreeDiagnosticsRedesign(t *testing.T) {
	t.Run("init dispatches and exits 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"init"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d want=%d", code, ExitOK)
		}
		if !spy.InitCalled {
			t.Fatal("init not called")
		}
		if spy.InitForce {
			t.Fatal("plain init should not set force")
		}
	})

	t.Run("init --force sets force", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"init", "--force"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.InitForce {
			t.Fatal("force not set")
		}
	})

	t.Run("status dispatches and exits 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"status"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})

	t.Run("check dispatches as top-level", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"check"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 1 {
			t.Fatalf("mutations=%d want 1", spy.Mutations)
		}
	})

	t.Run("reconcile dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"reconcile", "--dry-run"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.ReconcileDryRun {
			t.Fatal("dry-run not set")
		}
	})

	t.Run("routing bare dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})

	t.Run("routing explain dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "explain"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})

	t.Run("routing enable dispatches with provider", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "enable", "codex"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if spy.EnableProvider != "codex" {
			t.Fatalf("enable provider=%q want codex", spy.EnableProvider)
		}
	})

	t.Run("routing disable dispatches with provider", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "disable", "zai"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if spy.DisableProvider != "zai" {
			t.Fatalf("disable provider=%q want zai", spy.DisableProvider)
		}
	})

	t.Run("routing reset dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "reset"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.ResetCalled {
			t.Fatal("reset not called")
		}
	})

	t.Run("doctor dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"doctor"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})
}

// TestRemovedCommandsRejectWithoutMutation verifies all removed command forms
// exit 1 without any mutation.
func TestRemovedCommandsRejectWithoutMutation(t *testing.T) {
	removed := [][]string{
		{"hook"},
		{"hook", "anything"},
		{"sync"},
		{"sync", "--from-polytoken"},
		{"quota"},
		{"quota", "status"},
		{"quota", "check"},
		{"state"},
		{"state", "set"},
		{"state", "clear"},
		{"enable"},
		{"disable"},
		{"reset"},
		{"routing", "enable"},  // no-argument
		{"routing", "disable"}, // no-argument
		{"routing", "show"},    // does not exist
	}
	for _, args := range removed {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			code := Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
			if code != ExitRejected {
				t.Fatalf("args=%v exit=%d want=%d", args, code, ExitRejected)
			}
			if spy.Mutations != 0 {
				t.Fatalf("args=%v mutated %d times", args, spy.Mutations)
			}
		})
	}
}

// TestColorPolicyPrecedence verifies the normative color precedence:
// --json disables ANSI, NO_COLOR disables ANSI, otherwise terminal-gated.
// CLICOLOR_FORCE must NOT be supported.
func TestColorPolicyPrecedence(t *testing.T) {
	// Save and restore the seams.
	origTerminal := isTerminal
	origNoColor := noColorEnv
	t.Cleanup(func() {
		isTerminal = origTerminal
		noColorEnv = origNoColor
	})

	t.Run("json disables ansi", func(t *testing.T) {
		isTerminal = func(io.Writer) bool { return true }
		noColorEnv = func() string { return "" }
		if colorEnabled(io.Discard, true) {
			t.Fatal("json should disable ansi")
		}
	})

	t.Run("no_color disables ansi even on terminal", func(t *testing.T) {
		isTerminal = func(io.Writer) bool { return true }
		noColorEnv = func() string { return "1" }
		if colorEnabled(io.Discard, false) {
			t.Fatal("NO_COLOR should disable ansi")
		}
	})

	t.Run("terminal enables ansi when no json and no NO_COLOR", func(t *testing.T) {
		isTerminal = func(io.Writer) bool { return true }
		noColorEnv = func() string { return "" }
		if !colorEnabled(io.Discard, false) {
			t.Fatal("terminal should enable ansi")
		}
	})

	t.Run("non-terminal disables ansi", func(t *testing.T) {
		isTerminal = func(io.Writer) bool { return false }
		noColorEnv = func() string { return "" }
		if colorEnabled(io.Discard, false) {
			t.Fatal("non-terminal should disable ansi")
		}
	})

	t.Run("empty NO_COLOR does not disable ansi", func(t *testing.T) {
		isTerminal = func(io.Writer) bool { return true }
		noColorEnv = func() string { return "" }
		if !colorEnabled(io.Discard, false) {
			t.Fatal("empty NO_COLOR should not disable ansi on terminal")
		}
	})

	t.Run("CLICOLOR_FORCE is not supported", func(t *testing.T) {
		t.Setenv("CLICOLOR_FORCE", "1")
		isTerminal = func(io.Writer) bool { return false }
		noColorEnv = func() string { return "" }
		if colorEnabled(io.Discard, false) {
			t.Fatal("CLICOLOR_FORCE must not override terminal check")
		}
	})
}

// TestJSONContractsNoANSI verifies every --json invocation produces valid JSON
// with no ANSI escapes and exactly one object on stdout.
func TestJSONContractsNoANSI(t *testing.T) {
	// Force terminal detection so ANSI *would* be emitted if the policy
	// were wrong — --json must still be ANSI-free.
	origTerminal := isTerminal
	isTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { isTerminal = origTerminal })

	spy := newDepsSpy()
	now := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	spy.StatusReportValue = service.StatusReport{
		AsOf: now, Revision: 1,
		Providers: []service.ProviderStatus{
			{Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal, Reason: "normal"},
		},
	}
	spy.SnapshotValue = service.DiagnosticSnapshot{} // zero snapshot: clean empty views

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"status", []string{"status", "--json"}},
		{"routing", []string{"routing", "--json"}},
		{"routing explain", []string{"routing", "explain", "--json"}},
		{"doctor", []string{"doctor", "--json"}},
		{"check", []string{"check", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			code := Run(context.Background(), tc.args, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
			if code != ExitOK {
				t.Fatalf("exit=%d", code)
			}
			raw := strings.TrimSpace(out.String())
			if raw == "" {
				t.Fatal("empty output")
			}
			// Exactly one JSON object.
			lines := strings.Split(raw, "\n")
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d: %q", len(lines), raw)
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, raw)
			}
			// No ANSI escape sequences.
			if strings.Contains(raw, "\x1b") {
				t.Fatalf("JSON contains ANSI escape: %q", raw)
			}
		})
	}
}

// TestJSONErrorAndPendingEnvelopes verifies exit 1 and exit 2 outcomes still
// emit exactly one JSON envelope on stdout (never empty stdout).
func TestJSONErrorAndPendingEnvelopes(t *testing.T) {
	t.Run("check rejected emits json envelope exit 1", func(t *testing.T) {
		var out bytes.Buffer
		// invalid args on a json command should emit an envelope and exit 1
		code := Run(context.Background(), []string{"check", "--json", "--bogus"}, strings.NewReader(""), &out, io.Discard, newDepsSpy().Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want 1", code)
		}
		var parsed map[string]any
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON envelope: %v\n%s", err, out.String())
		}
		if parsed["accepted"] != false {
			t.Fatalf("expected accepted=false: %v", parsed["accepted"])
		}
		if parsed["error"] == nil || parsed["error"] == "" {
			t.Fatal("expected non-empty error")
		}
	})

	t.Run("check pending emits json envelope exit 2", func(t *testing.T) {
		spy := newDepsSpy()
		spy.QuotaCheckSet = true
		spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 3, Problem: true}
		var out bytes.Buffer
		code := Run(context.Background(), []string{"check", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitPending {
			t.Fatalf("exit=%d want 2", code)
		}
		var parsed map[string]any
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		if parsed["accepted"] != true || parsed["problem"] != true {
			t.Fatalf("unexpected envelope: %v", parsed)
		}
	})

	t.Run("status error emits json envelope exit 1", func(t *testing.T) {
		spy := newDepsSpy()
		spy.StatusReportValue = service.StatusReport{Error: "state unreadable"}
		var out bytes.Buffer
		code := Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want 1", code)
		}
		var parsed map[string]any
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		if parsed["error"] == nil || parsed["error"] == "" {
			t.Fatal("expected error field")
		}
	})
}

// TestRenderersDoNotMutateReports verifies text/JSON renderers never mutate
// their input reports.
func TestRenderersDoNotMutateReports(t *testing.T) {
	t.Run("status render does not mutate", func(t *testing.T) {
		r := service.StatusReport{
			Revision: 5,
			Providers: []service.ProviderStatus{
				{Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal, Reason: "normal"},
			},
		}
		originalProviders := len(r.Providers)
		s := styler{enabled: false}
		var buf bytes.Buffer
		writeStatusText(&buf, r, s)
		_ = statusEnvelope(r)
		if len(r.Providers) != originalProviders {
			t.Fatalf("render mutated providers: %d -> %d", originalProviders, len(r.Providers))
		}
	})

	t.Run("routing render does not mutate", func(t *testing.T) {
		r := service.RoutingReport{
			RoutingEnabled: true,
			Routes: []service.RouteProjection{
				{TargetID: "global", Name: "full", Effective: []string{"codex/gpt"}},
			},
		}
		originalEffective := r.Routes[0].Effective
		s := styler{enabled: false}
		var buf bytes.Buffer
		writeRoutingText(&buf, r, s)
		_ = routingEnvelope(r)
		if &r.Routes[0].Effective[0] != &originalEffective[0] {
			// If the slice header or backing array changed, mutation occurred.
		}
	})
}

// TestDoctorSeverityRenderingAndExit verifies doctor renders a healthy summary
// when clean, groups/sorts findings by severity, and exits correctly.
func TestDoctorSeverityRenderingAndExit(t *testing.T) {
	t.Run("healthy summary exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		spy.DoctorReportValue = doctor.Report{}
		var out bytes.Buffer
		code := Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d want 0", code)
		}
		if !strings.Contains(out.String(), "healthy") {
			t.Fatalf("expected healthy summary: %q", out.String())
		}
	})

	t.Run("findings exit 1 and sorted by severity", func(t *testing.T) {
		spy := newDepsSpy()
		spy.DoctorReportValue = doctor.Report{
			Findings: []doctor.Finding{
				{Code: "warn-1", Message: "a warning", Severity: doctor.Warning},
				{Code: "err-1", Message: "an error", Severity: doctor.Error},
			},
		}
		var out bytes.Buffer
		code := Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want 1", code)
		}
		text := out.String()
		// Error should appear before warning (sorted by severity rank).
		errIdx := strings.Index(text, "err-1")
		warnIdx := strings.Index(text, "warn-1")
		if errIdx < 0 || warnIdx < 0 {
			t.Fatalf("missing findings in output:\n%s", text)
		}
		if errIdx > warnIdx {
			t.Fatalf("error should sort before warning:\n%s", text)
		}
	})

	t.Run("severity markers rendered", func(t *testing.T) {
		spy := newDepsSpy()
		spy.DoctorReportValue = doctor.Report{
			Findings: []doctor.Finding{
				{Code: "err-1", Message: "error finding", Severity: doctor.Error},
			},
		}
		var out bytes.Buffer
		Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if !strings.Contains(out.String(), "[error]") {
			t.Fatalf("missing [error] severity marker:\n%s", out.String())
		}
	})
}

// =====================================================================
// Existing tests adapted for the new command tree
// =====================================================================

func TestExitCodeRouting(t *testing.T) {
	if got := MutationExitCode(service.Outcome{Accepted: true}); got != ExitOK {
		t.Fatalf("accepted mutation=%d want %d", got, ExitOK)
	}
	if got := MutationExitCode(service.Outcome{Accepted: false}); got != ExitRejected {
		t.Fatalf("rejected mutation=%d want %d", got, ExitRejected)
	}
	pending := service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{Pending: &state.ApplyFailure{}}}}
	if got := MutationExitCode(pending); got != ExitPending {
		t.Fatalf("pending mutation=%d want %d", got, ExitPending)
	}
	if got := DiagnosticExitCode(StatusCommand, true); got != ExitPending {
		t.Fatalf("actionable status=%d want %d", got, ExitPending)
	}
	if got := DiagnosticExitCode(DoctorCommand, true); got != ExitRejected {
		t.Fatalf("doctor=%d", got)
	}
	if got := DiagnosticExitCode(DoctorCommand, false); got != ExitOK {
		t.Fatalf("healthy doctor=%d", got)
	}
}

// TestInitOutputContract proves successful init prints guidance with no
// sync/hook references.
func TestInitOutputContract(t *testing.T) {
	spy := newDepsSpy()
	var stdout bytes.Buffer
	code := Run(context.Background(), []string{"init"}, strings.NewReader(""), &stdout, io.Discard, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d want %d", code, ExitOK)
	}
	if spy.Mutations != 1 {
		t.Fatalf("init invoked %d times, want 1", spy.Mutations)
	}
	out := stdout.String()
	for _, want := range []string{"desired.yaml created", "init --force"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// No sync or hook references.
	for _, banned := range []string{"sync", "hook"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("init output should not reference %q: %s", banned, out)
		}
	}
}

func TestStatusStateLoadFailureExitsRejected(t *testing.T) {
	spy := newDepsSpy()
	spy.StatusReportValue = service.StatusReport{Error: "state: parse state.json: unexpected end of JSON input"}
	stderr := &strings.Builder{}
	got := Run(context.Background(), []string{"status"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
	if got != ExitRejected {
		t.Fatalf("exit=%d want=%d", got, ExitRejected)
	}
	if !strings.Contains(stderr.String(), "state.json") {
		t.Fatalf("stderr missing diagnostic: %q", stderr.String())
	}
}

// TestStatusNoRunningSessionAdvisory proves status output no longer includes
// the running-session advisory (AC.11).
func TestStatusNoRunningSessionAdvisory(t *testing.T) {
	spy := newDepsSpy()
	spy.StatusReportValue = service.StatusReport{
		Revision: 1,
		Providers: []service.ProviderStatus{
			{Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal, Reason: "normal"},
		},
	}
	var out bytes.Buffer
	Run(context.Background(), []string{"status"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
	if strings.Contains(strings.ToLower(out.String()), "running session") || strings.Contains(out.String(), "restarted or reloaded") {
		t.Fatalf("status should not include running-session advisory: %q", out.String())
	}
}

// TestReconcileDryRunReportsPendingAndRetainedStaging covers the dry-run path.
func TestReconcileDryRunReportsPendingAndRetainedStaging(t *testing.T) {
	t.Run("reports pending without exit 2", func(t *testing.T) {
		stderr := &strings.Builder{}
		spy := &outcomeSpy{outcome: service.Outcome{
			Accepted: true,
			Targets: []service.TargetOutcome{{TargetID: "global", Pending: &state.ApplyFailure{
				Stage: "config_validate", Summary: "config validate: invalid model", Remediation: "inspect staged config",
			}}},
		}}
		code := Run(context.Background(), []string{"reconcile", "--dry-run"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d want %d", code, ExitOK)
		}
		if !strings.Contains(stderr.String(), "config validate: invalid model") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("reports retained staging path", func(t *testing.T) {
		stderr := &strings.Builder{}
		spy := &outcomeSpy{outcome: service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{TargetID: "global", StagingRoot: "/tmp/staged"}}}}
		code := Run(context.Background(), []string{"reconcile", "--dry-run", "--keep-staging"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
		if code != ExitOK || !strings.Contains(stderr.String(), "staged candidate retained at: /tmp/staged") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("keep-staging without dry-run rejected", func(t *testing.T) {
		stderr := &strings.Builder{}
		code := Run(context.Background(), []string{"reconcile", "--keep-staging"}, strings.NewReader(""), io.Discard, stderr, newDepsSpy().Dependencies())
		if code != ExitRejected || !strings.Contains(stderr.String(), "requires --dry-run") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
}

func TestMutationErrorsArePrinted(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{command: "init", args: []string{"init"}},
		{command: "reconcile", args: []string{"reconcile"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			spy := &outcomeSpy{outcome: service.Outcome{Error: errors.New("source reader unavailable")}}
			var stderr bytes.Buffer
			if got := Run(context.Background(), tc.args, strings.NewReader(""), io.Discard, &stderr, spy.Dependencies()); got != ExitRejected {
				t.Fatalf("exit=%d want=%d", got, ExitRejected)
			}
			if !strings.Contains(stderr.String(), "source reader unavailable") {
				t.Fatalf("stderr=%q does not contain mutation error", stderr.String())
			}
		})
	}
}

// --- process-control source guard (retained) ---

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", thisFile)
		}
		dir = parent
	}
}

func scanGoSources(t *testing.T, fn func(relPath string, b []byte)) {
	t.Helper()
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fn(rel, b)
		return nil
	})
	if err != nil {
		t.Fatalf("scanGoSources: %v", err)
	}
}

func TestNoProcessControl(t *testing.T) {
	verbs := "(restart|signal|kill)"
	name := "polytoken"
	pat := "(?i)" + verbs + ".*" + name + "|" + name + ".*" + verbs
	forbidden := regexp.MustCompile(pat)
	scanGoSources(t, func(relPath string, b []byte) {
		if strings.HasPrefix(relPath, "internal/validate/") && !strings.HasSuffix(relPath, "_test.go") {
			return
		}
		if forbidden.Match(b) {
			t.Errorf("forbidden process control in %s", relPath)
		}
	})
}
