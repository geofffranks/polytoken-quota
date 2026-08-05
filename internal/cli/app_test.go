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
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/geofffranks/codexbar-hooks/internal/doctor"
	"github.com/geofffranks/codexbar-hooks/internal/hook"
	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/service"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// NOTE: the production binding assertion below pins that service.Coordinator
// satisfies the Mutator interface at compile time. Task 12 introduces
// service.Coordinator.

// Compile-time proof that the production Coordinator implements Mutator.
var _ Mutator = (*service.Coordinator)(nil)

// depsSpy is a test double that records Mutator/Diagnoser invocations. It
// satisfies both Mutator and Diagnoser.
type depsSpy struct {
	Mutations           int
	SetProvider         string
	SetPatch            state.ProviderPatch
	ClearSelector       state.Selector
	DisableProvider     string
	EnableProvider      string
	ResetCalled         bool
	RoutingEnabledSet   bool
	RoutingEnabledValue bool
	RoutingToggleErr    error
	QuotaCheckProvider  string
	QuotaCheckReconcile bool
	QuotaCheckOutcome   service.Outcome
	QuotaCheckSet       bool
	StatusReportValue   service.StatusReport
}

func newDepsSpy() *depsSpy { return &depsSpy{} }

func (s *depsSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:        s,
		Diagnoser:      s,
		RankExplainer:  s,
		QuotaStater:    s,
		RoutingToggler: s,
		Environment:    func() map[string]string { return map[string]string{} },
	}
}

// Compile-time proof that the spy satisfies the read-only and toggling surfaces.
var (
	_ service.RankingExplainer = (*depsSpy)(nil)
	_ service.QuotaStater      = (*depsSpy)(nil)
	_ service.RoutingToggler   = (*depsSpy)(nil)
)

func (s *depsSpy) Init(context.Context) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) HandleEvent(context.Context, hook.Event) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Reconcile(context.Context, bool, bool) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Sync(context.Context, bool) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Set(_ context.Context, provider string, patch state.ProviderPatch) service.Outcome {
	s.Mutations++
	s.SetProvider = provider
	s.SetPatch = patch
	return service.Outcome{Accepted: true}
}

func (s *depsSpy) Clear(_ context.Context, selector state.Selector) service.Outcome {
	s.Mutations++
	s.ClearSelector = selector
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

func (s *depsSpy) Doctor(context.Context, bool) doctor.Report { return doctor.Report{} }

// --- RankingExplainer / QuotaStater / RoutingToggler ---

func (s *depsSpy) RankingExplain(context.Context) service.RankingReport {
	s.Mutations++
	return service.RankingReport{Enabled: true, Entries: []service.RankEntryReport{
		{MappingID: "codex", Rank: 0, Eligible: true, Explanation: "peak, 80% headroom"},
	}}
}

func (s *depsSpy) QuotaStatus(context.Context) service.QuotaStatusReport {
	s.Mutations++
	return service.QuotaStatusReport{Revision: 1, Providers: []service.QuotaSnapshotReport{
		{MappingID: "codex", Status: "fresh", Windows: []service.QuotaWindowReport{{Name: "daily", Remaining: floatPtr(0.8)}}},
	}}
}

func (s *depsSpy) SetRoutingEnabled(_ context.Context, enabled bool) error {
	s.Mutations++
	s.RoutingEnabledSet = true
	s.RoutingEnabledValue = enabled
	return s.RoutingToggleErr
}

func floatPtr(v float64) *float64 { return &v }

func TestCommandTree(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"init"}, 0},
		{[]string{"hook"}, 0},
		{[]string{"status", "--json"}, 0},
		{[]string{"reconcile", "--dry-run"}, 0},
		{[]string{"sync", "--from-polytoken", "--force"}, 0},
		{[]string{"state", "set", "codex", "--quota", "low"}, 0},
		{[]string{"state", "clear", "codex"}, 0},
		{[]string{"state", "clear", "--all"}, 0},
		{[]string{"doctor", "--json"}, 0},
		{[]string{"unknown"}, 1},
		{[]string{"sync"}, 1},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			stdin := strings.NewReader("")
			if len(tc.args) > 0 && tc.args[0] == "hook" {
				stdin = strings.NewReader(`{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`)
			}
			if got := Run(context.Background(), tc.args, stdin, io.Discard, io.Discard, spy.Dependencies()); got != tc.want {
				t.Fatalf("exit=%d want=%d", got, tc.want)
			}
			if tc.want == 1 && spy.Mutations != 0 {
				t.Fatalf("invalid syntax mutated %d times", spy.Mutations)
			}
		})
	}
}

func TestManualProviderCommandsDispatchTypedMutations(t *testing.T) {
	cases := []struct {
		args        []string
		wantDisable string
		wantEnable  string
		wantReset   bool
	}{
		{args: []string{"disable", "codex"}, wantDisable: "codex"},
		{args: []string{"enable", "zai"}, wantEnable: "zai"},
		{args: []string{"reset"}, wantReset: true},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			if got := Run(context.Background(), tc.args, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != ExitOK {
				t.Fatalf("exit=%d", got)
			}
			if spy.DisableProvider != tc.wantDisable || spy.EnableProvider != tc.wantEnable || spy.ResetCalled != tc.wantReset {
				t.Fatalf("disable=%q enable=%q reset=%v", spy.DisableProvider, spy.EnableProvider, spy.ResetCalled)
			}
		})
	}
}

func TestManualProviderCommandSyntaxRejectsWithoutMutation(t *testing.T) {
	for _, args := range [][]string{
		{"disable"}, {"disable", "codex", "extra"}, {"disable", "--bad"},
		{"enable"}, {"enable", "zai", "extra"}, {"enable", "--bad"},
		{"reset", "extra"}, {"reset", "--bad"},
	} {
		spy := newDepsSpy()
		if got := Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != ExitRejected {
			t.Errorf("args=%v exit=%d want=%d", args, got, ExitRejected)
		}
		if spy.Mutations != 0 {
			t.Errorf("args=%v mutated %d times", args, spy.Mutations)
		}
	}
}

func TestMutationExitReportsPendingTargetImmediately(t *testing.T) {
	stderr := new(strings.Builder)
	spy := &outcomeSpy{outcome: service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{{TargetID: "global", Pending: &state.ApplyFailure{
			Stage: "doctor", Summary: "startup validation failed", Remediation: "run reconcile",
		}}},
	}}
	code := Run(context.Background(), []string{"disable", "codex"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want=%d", code, ExitPending)
	}
	for _, want := range []string{"target global pending", "stage=doctor", "startup validation failed", "run reconcile"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestMutationExitDoesNotPrintUnsanitizedPendingDetails(t *testing.T) {
	secret := "SECRET_TOKEN_123"
	stderr := new(strings.Builder)
	spy := &outcomeSpy{outcome: service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{
		TargetID: "global", Pending: &state.ApplyFailure{Stage: "publish", Summary: "token=" + secret, Remediation: "/home/dev/private"},
	}}}}
	Run(context.Background(), []string{"disable", "codex"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
	if strings.Contains(stderr.String(), secret) || strings.Contains(stderr.String(), "/home/dev/private") {
		t.Fatalf("unsanitized pending details leaked: %q", stderr.String())
	}
}

func TestStateAdaptersPassTypedValues(t *testing.T) {
	spy := newDepsSpy()
	if got := Run(context.Background(), []string{"state", "set", "codex", "--quota", "low", "--availability", "unavailable"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	low, unavailable := state.QuotaLow, state.Unavailable
	if spy.SetProvider != "codex" || !reflect.DeepEqual(spy.SetPatch, state.ProviderPatch{Quota: &low, Availability: &unavailable}) {
		t.Fatalf("provider=%q patch=%+v", spy.SetProvider, spy.SetPatch)
	}

	spy = newDepsSpy()
	if got := Run(context.Background(), []string{"state", "clear", "--all"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies()); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	if !reflect.DeepEqual(spy.ClearSelector, state.Selector{All: true}) {
		t.Fatalf("selector=%+v", spy.ClearSelector)
	}
}

func TestKeepStagingRequiresDryRun(t *testing.T) {
	stderr := new(strings.Builder)
	spy := &outcomeSpy{outcome: service.Outcome{Accepted: false, Error: errors.New("service: --keep-staging requires --dry-run")}}
	code := Run(context.Background(), []string{"reconcile", "--keep-staging"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
	if code != ExitRejected || !strings.Contains(stderr.String(), "requires --dry-run") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestDryRunReportsRetainedStagingPath(t *testing.T) {
	stderr := new(strings.Builder)
	spy := &outcomeSpy{outcome: service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{TargetID: "global", StagingRoot: "/tmp/staged"}}}}
	code := Run(context.Background(), []string{"reconcile", "--dry-run", "--keep-staging"}, strings.NewReader(""), io.Discard, stderr, spy.Dependencies())
	if code != ExitOK || !strings.Contains(stderr.String(), "staged candidate retained at: /tmp/staged") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestDryRunReportsPendingValidationWithoutPendingExit(t *testing.T) {
	stderr := new(strings.Builder)
	spy := &outcomeSpy{outcome: service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{{TargetID: "global", Pending: &state.ApplyFailure{
			Stage: "config_validate", Summary: "config validate: invalid model", Remediation: "inspect staged config",
		}}},
	}}
	code := Run(context.Background(), []string{"reconcile", "--dry-run"}, strings.NewReader(""), io.Discard, stderr, Dependencies{Mutator: spy, Diagnoser: spy, Environment: func() map[string]string { return nil }})
	if code != ExitOK {
		t.Fatalf("exit=%d want %d", code, ExitOK)
	}
	if !strings.Contains(stderr.String(), "config validate: invalid model") || !strings.Contains(stderr.String(), "inspect staged config") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

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

// TestInitOutputContract proves a successful create-only init prints setup
// guidance that names an absolute executable, all six CodExBar events, the
// CodExBar 0.44.0 minimum, direct shell-free invocation, no automatic CodExBar
// edit, and no overwrite option. The spy returns a successful Outcome, so this
// also guards that Task 2's spy contract stays intact.
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
	for _, want := range []string{
		"/usr/local/bin/polytoken-quota", // an absolute executable path
		"0.44.0",                         // CodExBar minimum version
		"without a shell",                // direct, shell-free invocation
		"no automatic CodExBar edit",     // never modifies CodExBar
		"not overwrite",                  // strict create-only, no overwrite option
		"quota_low", "quota_reached", "quota_reset",
		"provider_unavailable", "provider_recovered", "refresh_failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// --- Task 13: status advisory, exit contracts, and process-control guard ----

// statusFixture returns a representative StatusReport for rendering tests.
func statusFixture() service.StatusReport {
	return service.StatusReport{
		Revision: 5,
		Providers: []service.ProviderStatus{
			{Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal},
			{Provider: "zai", Quota: state.QuotaLow, Availability: state.Available, Mode: state.ModeReserve, LastEvent: "quota_low"},
		},
		Targets: []service.TargetStatus{
			{TargetID: "global", AttemptedRevision: 5, AppliedRevision: 4, Pending: true},
		},
		Pending: 1,
		Drift:   false,
	}
}

// TestStateClearRejectsContradictoryArguments proves `state clear --all
// <provider>` is rejected without mutation: contradictory arguments must never
// silently become the destructive all-provider clear.
func TestStateClearRejectsContradictoryArguments(t *testing.T) {
	spy := newDepsSpy()
	got := Run(context.Background(), []string{"state", "clear", "--all", "codex"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
	if got != ExitRejected {
		t.Fatalf("exit=%d want=%d", got, ExitRejected)
	}
	if spy.Mutations != 0 {
		t.Fatalf("contradictory state clear mutated %d times", spy.Mutations)
	}
}

// TestStatusStateLoadFailureExitsRejected proves a status report carrying an
// Error (state unreadable) exits 1 instead of rendering as a clean report.
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

// TestStatusAlwaysWarnsAboutRunningSessions proves both text and JSON output
// include the unconditional running-session advisory. The advisory uses the
// production constant to avoid duplicating the forbidden-adjacency text.
func TestStatusAlwaysWarnsAboutRunningSessions(t *testing.T) {
	text, jsonText := renderStatus(statusFixture())
	advisory := RunningSessionAdvisory
	if !strings.Contains(text, advisory) || !strings.Contains(jsonText, "running_session_advisory") {
		t.Fatal("missing advisory")
	}
}

func TestStatusRendersQuotaAndRoutingExplanations(t *testing.T) {
	remaining := 0.8
	r := service.StatusReport{
		RoutingEnabled:  true,
		Ranking:         []service.RankEntryReport{{MappingID: "codex", Rank: 0, Eligible: true, Explanation: "fresh quota"}},
		EffectiveOrders: []service.ChainOrderReport{{TargetID: "global", Chain: "full", Desired: []string{"zai/gpt", "codex/gpt"}, Effective: []string{"codex/gpt", "zai/gpt"}}},
		Quota:           []service.QuotaSnapshotReport{{MappingID: "codex", Availability: "available", Status: "fresh", Windows: []service.QuotaWindowReport{{Name: "daily", Remaining: &remaining}}, Attempt: &service.QuotaAttemptReport{Status: "fresh"}, LastRank: 0}},
	}
	text, jsonText := renderStatus(r)
	for _, want := range []string{"routing: enabled", "fresh quota", "desired=", "effective=", "quota codex", "remaining=80%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q: %s", want, text)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"routing_enabled", "ranking", "effective_orders", "quota", "running_session_advisory"} {
		if _, ok := parsed[key]; !ok {
			t.Fatalf("JSON missing %q: %s", key, jsonText)
		}
	}
}

func TestStatusRoutingDisabledOmitsRanking(t *testing.T) {
	text, jsonText := renderStatus(service.StatusReport{RoutingEnabled: false, Ranking: []service.RankEntryReport{{MappingID: "secret-provider", Explanation: "must not show"}}})
	if !strings.Contains(text, "routing: disabled") {
		t.Fatalf("text=%q", text)
	}
	if strings.Contains(text, "must not show") || strings.Contains(jsonText, "must not show") {
		t.Fatalf("ranking leaked while disabled: text=%q json=%q", text, jsonText)
	}
}

func TestStatusRendersManualDisableReason(t *testing.T) {
	r := service.StatusReport{Providers: []service.ProviderStatus{{
		Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available,
		Mode: state.ModeDisabled, ManualDisabled: true, Reason: "manual_disabled",
	}}}
	text, jsonText := renderStatus(r)
	if !strings.Contains(text, "reason=manual_disabled") || !strings.Contains(jsonText, "manual_disabled") {
		t.Fatalf("text=%q json=%q", text, jsonText)
	}
}

// TestStatusPendingDriftAlwaysExitsZero proves status exits 0 when its report
// contains pending targets or drift alone, while actionable quota problems exit
// 2 and an actionable doctor report exits 1.
func TestStatusPendingDriftAlwaysExitsZero(t *testing.T) {
	// Pending targets and drift are advisory status conditions, not actionable
	// quota problems, so they do not affect status's exit code.
	if got := DiagnosticExitCode(StatusCommand, false); got != 0 {
		t.Fatalf("pending/drift status exit=%d", got)
	}
	if got := DiagnosticExitCode(StatusCommand, true); got != ExitPending {
		t.Fatalf("problem status exit=%d", got)
	}
	if got := DiagnosticExitCode(DoctorCommand, true); got != 1 {
		t.Fatalf("doctor exit=%d", got)
	}
}

// outcomeSpy is a test double that returns a preset Outcome for every mutator.
type outcomeSpy struct{ outcome service.Outcome }

func (s *outcomeSpy) Init(context.Context) service.Outcome                    { return s.outcome }
func (s *outcomeSpy) HandleEvent(context.Context, hook.Event) service.Outcome { return s.outcome }
func (s *outcomeSpy) Reconcile(context.Context, bool, bool) service.Outcome   { return s.outcome }
func (s *outcomeSpy) Sync(context.Context, bool) service.Outcome              { return s.outcome }
func (s *outcomeSpy) Set(context.Context, string, state.ProviderPatch) service.Outcome {
	return s.outcome
}
func (s *outcomeSpy) Clear(context.Context, state.Selector) service.Outcome    { return s.outcome }
func (s *outcomeSpy) Disable(context.Context, string) service.Outcome          { return s.outcome }
func (s *outcomeSpy) Enable(context.Context, string) service.Outcome           { return s.outcome }
func (s *outcomeSpy) Reset(context.Context) service.Outcome                    { return s.outcome }
func (s *outcomeSpy) QuotaCheck(context.Context, string, bool) service.Outcome { return s.outcome }
func (s *outcomeSpy) Status(context.Context, bool) service.StatusReport {
	return service.StatusReport{}
}
func (s *outcomeSpy) Doctor(context.Context, bool) doctor.Report { return doctor.Report{} }

func (s *outcomeSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:     s,
		Diagnoser:   s,
		Environment: func() map[string]string { return map[string]string{} },
	}
}

// runWithOutcome runs the CLI with a spy returning the given outcome and
// returns the exit code.
func runWithOutcome(t *testing.T, args []string, outcome service.Outcome) int {
	t.Helper()
	spy := &outcomeSpy{outcome: outcome}
	stdin := strings.NewReader("")
	if len(args) > 0 && args[0] == "hook" {
		stdin = strings.NewReader(`{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`)
	}
	return Run(context.Background(), args, stdin, io.Discard, io.Discard, spy.Dependencies())
}

// TestCLIExitContract proves mutating commands exit 0/1/2 and diagnostic
// commands exit 0/1 per the stable exit-code contract.
func TestCLIExitContract(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		outcome service.Outcome
		want    int
	}{
		{[]string{"hook"}, service.Outcome{Accepted: true}, 0},
		{[]string{"hook"}, service.Outcome{Accepted: true, Targets: []service.TargetOutcome{{Pending: &state.ApplyFailure{}}}}, 2},
		{[]string{"hook"}, service.Outcome{}, 1},
		{[]string{"init"}, service.Outcome{Error: policy.ErrDesiredExists}, 1},
	} {
		if got := runWithOutcome(t, tc.args, tc.outcome); got != tc.want {
			t.Fatalf("got=%d want=%d", got, tc.want)
		}
	}
}

func TestMutationErrorsArePrinted(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{command: "init", args: []string{"init"}},
		{command: "sync", args: []string{"sync", "--from-polytoken"}},
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

// moduleRoot returns the absolute project root containing go.mod, derived from
// this test file's own path via runtime.Caller(0) so the scan is independent of
// the test binary's CWD (which is the package source dir, not the project root).
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

// scanGoSources walks the whole module root for .go files and invokes fn for
// each, passing the path relative to the module root. It walks from the module
// root (not the test CWD) so every package is covered.
func scanGoSources(t *testing.T, fn func(relPath string, b []byte)) {
	t.Helper()
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/build cache directories if ever present.
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

// TestNoProcessControl scans Go sources across the whole module for forbidden
// daemon restart/signal/kill references, excluding the validator's legitimate
// child-process cleanup.
func TestNoProcessControl(t *testing.T) {
	// Build the forbidden pattern from fragments so the test source itself does
	// not trip the guard on a single line.
	verbs := "(restart|signal|kill)"
	name := "polytoken"
	pat := "(?i)" + verbs + ".*" + name + "|" + name + ".*" + verbs
	forbidden := regexp.MustCompile(pat)
	scanGoSources(t, func(relPath string, b []byte) {
		// The validator legitimately performs child-process cleanup.
		if strings.HasPrefix(relPath, "internal/validate/") && !strings.HasSuffix(relPath, "_test.go") {
			return
		}
		if forbidden.Match(b) {
			t.Errorf("forbidden process control in %s", relPath)
		}
	})
}

// TestRenderStatusDoesNotMutateInput proves renderStatus leaves the caller's
// StatusReport.RunningSessionAdvisory untouched.
func TestRenderStatusDoesNotMutateInput(t *testing.T) {
	r := statusFixture()
	if r.RunningSessionAdvisory != "" {
		t.Fatalf("fixture precondition: advisory already set %q", r.RunningSessionAdvisory)
	}
	renderStatus(r)
	if r.RunningSessionAdvisory != "" {
		t.Fatalf("renderStatus mutated input advisory to %q", r.RunningSessionAdvisory)
	}
}

// TestDoctorJSONContract proves doctor --json emits snake_case keys for Finding
// and Report, matching the status command's JSON shape.
func TestDoctorJSONContract(t *testing.T) {
	report := doctor.Report{
		Findings: []doctor.Finding{{
			Code:        "config-invalid",
			Message:     "the current live configuration fails validation",
			TargetID:    "global",
			File:        "config.yaml",
			Chain:       "defaults.full",
			Remediation: "fix the config",
			Severity:    doctor.Error,
		}},
	}
	var buf bytes.Buffer
	writeDoctor(&buf, report, true)
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatalf("unmarshal doctor json: %v\nraw: %s", err, buf.String())
	}
	for _, key := range []string{"findings", "recovered"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("report json missing top-level %q\nraw: %s", key, buf.String())
		}
	}
	findings, _ := decoded["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	first, _ := findings[0].(map[string]any)
	for _, key := range []string{"code", "message", "target_id", "file", "chain", "remediation", "severity"} {
		if _, ok := first[key]; !ok {
			t.Errorf("finding json missing %q\nraw: %s", key, buf.String())
		}
	}
}
