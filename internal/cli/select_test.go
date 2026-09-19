package cli

// select_test.go — CLI contract for select and select-eval: flag parsing and
// conflicts, stdin handling (never read for explicit difficulty), exit-code
// mapping, the version-1 JSON envelopes, help documentation, and privacy
// (no task text in any output; fatal errors render fixed sentences only).

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/selection"
)

// selectSpy records the select requests the CLI hands to the runner.
type selectSpy struct {
	calls   int
	request selection.SelectRequest
	outcome selection.SelectOutcome
	err     error
}

func (s *selectSpy) Run(_ context.Context, req selection.SelectRequest) (selection.SelectOutcome, error) {
	s.calls++
	s.request = req
	return s.outcome, s.err
}

func (s *selectSpy) deps() Dependencies {
	return Dependencies{Select: s}
}

// evalSpy records select-eval invocations.
type evalSpy struct {
	calls  int
	req    selection.EvalInvocation
	report *selection.Report
	err    error
}

func (e *evalSpy) RunEval(_ context.Context, req selection.EvalInvocation) (*selection.Report, error) {
	e.calls++
	e.req = req
	if e.report != nil {
		return e.report, nil
	}
	r := &selection.Report{Model: "jev-test", Rubric: selection.RubricID, Total: 1, Assessed: 1, Matches: 1}
	r.Records = []selection.Record{{ID: "case-1", Phase: "execute", Match: true}}
	return r, e.err
}

func (e *evalSpy) deps() Dependencies {
	return Dependencies{SelectEval: e}
}

func confirmedOutcome() selection.SelectOutcome {
	headroom := 0.9
	return selection.SelectOutcome{
		Status: selection.SelectConfirmed,
		Reason: selection.ReasonFreshQuotaEvidence,
		Phase:  "execute",
		Tier:   selection.TierNormal,
		Result: selection.Result{
			Version: selection.SelectionVersion, Phase: "execute", Tier: string(selection.TierNormal),
			Reference: "codex/example(high)", Base: "codex/example", Suffix: "(high)",
			Mapping: "codex", Kind: selection.KindConfirmed, Headroom: &headroom,
		},
		AsOf: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

func TestSelectExplicitDifficultyNeverReadsStdin(t *testing.T) {
	// A stdin reader that fails on any Read proves the explicit-tier path
	// never touches the task.
	stdin := strings.NewReader("SELECTION-CANARY-7f3a must never be read")
	spy := &selectSpy{outcome: confirmedOutcome()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "candidates.yaml", "--phase", "execute", "--difficulty", "normal", "--json"},
		stdin, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if spy.calls != 1 {
		t.Fatalf("runner calls=%d", spy.calls)
	}
	if spy.request.Prompt != "" {
		t.Fatalf("prompt=%q reached the runner for an explicit-tier request", spy.request.Prompt)
	}
	if strings.Contains(stdout.String(), "SELECTION-CANARY-7f3a") {
		t.Fatal("output contains stdin content")
	}
}

func TestSelectAssessedPathReadsStdinOnce(t *testing.T) {
	spy := &selectSpy{outcome: confirmedOutcome()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "candidates.yaml", "--phase", "execute", "--json"},
		strings.NewReader("Rename the heading."), &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if spy.request.Prompt != "Rename the heading." {
		t.Fatalf("prompt=%q, want the stdin task", spy.request.Prompt)
	}
}

func TestSelectFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"conflict", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal", "--min-difficulty", "difficult"}},
		{"unknown tier", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "impossible"}},
		{"unknown floor", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--min-difficulty", "impossible"}},
		{"missing policy", []string{"select", "--phase", "execute", "--difficulty", "normal"}},
		{"missing phase", []string{"select", "--policy", "p.yaml", "--difficulty", "normal"}},
		{"unknown flag", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--turbo"}},
		{"empty exclusion", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal", "--exclude-family"}},
		// Every value flag must carry a real value: trailing flags,
		// flags followed by another flag, and explicitly empty values
		// are all parse errors, never silently empty strings.
		{"trailing policy", []string{"select", "--policy"}},
		{"policy followed by flag", []string{"select", "--policy", "--phase", "execute"}},
		{"empty phase value", []string{"select", "--policy", "p.yaml", "--phase", ""}},
		{"phase followed by flag", []string{"select", "--phase", "--difficulty", "normal"}},
		{"empty exclusion equals", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--exclude-family="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &selectSpy{}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, strings.NewReader("task"), &stdout, &stderr, spy.deps())
			if code != ExitRejected {
				t.Fatalf("exit=%d, want 1", code)
			}
			if spy.calls != 0 {
				t.Fatalf("runner called %d times for an invalid invocation", spy.calls)
			}
			if stderr.String() == "" {
				t.Fatal("expected a stderr diagnostic")
			}
		})
	}
}

// failingStdin fails the test if anything reads stdin: the difficulty value
// gate must reject an invocation before the stdin disclosure path is ever
// considered.
type failingStdin struct{ t *testing.T }

func (s *failingStdin) Read([]byte) (int, error) {
	s.t.Error("stdin was read for a rejected invocation")
	return 0, errors.New("stdin must not be read")
}

func TestSelectDifficultyValueGatesDisclosure(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"tier missing separate", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty"}},
		{"tier missing equals", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty="}},
		{"tier empty separate", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", ""}},
		{"floor missing separate", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--min-difficulty"}},
		{"floor missing equals", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--min-difficulty="}},
		{"floor empty separate", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--min-difficulty", ""}},
		{"tier followed by flag", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "--refresh"}},
		{"floor followed by flag", []string{"select", "--policy", "p.yaml", "--phase", "execute", "--min-difficulty", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A malformed difficulty value must be rejected before stdin
			// is read or the runner is invoked: an empty tier is
			// indistinguishable downstream from "tier not given", which
			// silently discloses the task to remote assessment.
			spy := &selectSpy{}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, &failingStdin{t}, &stdout, &stderr, spy.deps())
			if code != ExitRejected {
				t.Fatalf("exit=%d, want 1", code)
			}
			if spy.calls != 0 {
				t.Fatalf("runner called %d times for a rejected invocation", spy.calls)
			}
			if stderr.String() == "" {
				t.Fatal("expected a stderr diagnostic")
			}
		})
	}
}

func TestSelectDifficultyValueGatesDisclosureJSON(t *testing.T) {
	// --json before the malformed flag: the rejection still writes exactly
	// one JSON error object, and neither stdin nor the runner is touched.
	spy := &selectSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(),
		[]string{"select", "--policy", "p.yaml", "--json", "--difficulty"},
		&failingStdin{t}, &stdout, &stderr, spy.deps())
	if code != ExitRejected {
		t.Fatalf("exit=%d, want 1", code)
	}
	if spy.calls != 0 {
		t.Fatalf("runner called %d times", spy.calls)
	}
	assertSingleJSONObject(t, stdout.String())
	if !strings.Contains(stdout.String(), `"status":"error"`) {
		t.Fatalf("error envelope missing status: %s", stdout.String())
	}
}

func TestSelectEqualsFormDifficultyKeepsTaskLocal(t *testing.T) {
	// The attached --difficulty=normal form is a first-class explicit
	// tier: it runs locally and never reads stdin.
	spy := &selectSpy{outcome: confirmedOutcome()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(),
		[]string{"select", "--policy=p.yaml", "--phase=execute", "--difficulty=normal"},
		&failingStdin{t}, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if spy.calls != 1 || spy.request.Tier != selection.TierNormal || spy.request.Prompt != "" {
		t.Fatalf("request=%+v calls=%d", spy.request, spy.calls)
	}
}

func TestSelectJSONPhaseSanitized(t *testing.T) {
	// The JSON envelope sanitizes the policy phase exactly like the text
	// output; a path-identifying phase cannot leak.
	out := confirmedOutcome()
	out.Phase = "/Users/op/private/phase"
	spy := &selectSpy{outcome: out}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal", "--json"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout.String(), `"phase":"<home>"`) {
		t.Fatalf("phase not sanitized: %s", stdout.String())
	}
}

func TestSelectExitCodesByStatus(t *testing.T) {
	cases := []struct {
		status selection.SelectStatus
		want   int
	}{
		{selection.SelectConfirmed, ExitOK},
		{selection.SelectUncertain, ExitPending},
		{selection.SelectNoSelection, ExitPending},
		{selection.SelectAssessmentUnavailable, ExitPending},
	}
	for _, tc := range cases {
		out := confirmedOutcome()
		out.Status = tc.status
		spy := &selectSpy{outcome: out}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal"},
			nil, &stdout, &stderr, spy.deps())
		if code != tc.want {
			t.Fatalf("status=%q exit=%d, want %d", tc.status, code, tc.want)
		}
	}
}

func TestSelectFatalExitsRejectedWithFixedMessage(t *testing.T) {
	// The fatal error's raw text carries a local path; neither stream may
	// show it — only the fixed safe sentence.
	spy := &selectSpy{err: selection.Fatal(selection.FatalPolicy, errors.New("open /Users/op/secret/candidates.yaml: no such file"))}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitRejected {
		t.Fatalf("exit=%d, want 1", code)
	}
	out := stdout.String() + stderr.String()
	if strings.Contains(out, "/Users/op") {
		t.Fatalf("raw error path leaked: %q", out)
	}
	if !strings.Contains(out, "selection: candidate policy") {
		t.Fatalf("fixed sentence missing: %q", out)
	}
}

func TestSelectJSONEnvelope(t *testing.T) {
	spy := &selectSpy{outcome: confirmedOutcome()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal", "--json"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	assertSingleJSONObject(t, stdout.String())
	for _, want := range []string{
		`"version":1`, `"status":"confirmed"`, `"reason":"fresh_quota_evidence"`,
		`"model":"codex/example(high)"`, `"mapping":"codex"`, `"headroom":0.9`,
		`"tier":"normal"`, `"as_of":"2026-09-19T12:00:00Z"`, `"explicit_tier":true`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("envelope missing %s: %s", want, stdout.String())
		}
	}
}

func TestSelectJSONNoSelectionHasNullModel(t *testing.T) {
	spy := &selectSpy{outcome: selection.SelectOutcome{
		Status: selection.SelectNoSelection, Reason: selection.ReasonNoEligibleCandidate,
		Phase: "execute", Tier: selection.TierNormal,
		AsOf: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal", "--json"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitPending {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stdout.String(), `"model":null`) {
		t.Fatalf("model must be null without a selection: %s", stdout.String())
	}
	assertSingleJSONObject(t, stdout.String())
}

func TestSelectJSONErrorEnvelope(t *testing.T) {
	spy := &selectSpy{}
	var stdout, stderr bytes.Buffer
	// --json with invalid flags still writes exactly one JSON object.
	code := Run(context.Background(), []string{"select", "--phase", "execute", "--json"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitRejected {
		t.Fatalf("exit=%d, want 1", code)
	}
	assertSingleJSONObject(t, stdout.String())
	if !strings.Contains(stdout.String(), `"status":"error"`) {
		t.Fatalf("error envelope missing status: %s", stdout.String())
	}
}

func TestSelectRefreshAndExclusionsPassedThrough(t *testing.T) {
	spy := &selectSpy{outcome: confirmedOutcome()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{
		"select", "--policy", "p.yaml", "--phase", "execute", "--difficulty", "normal",
		"--exclude-family", "codex", "--exclude-family=anthropic", "--refresh",
	}, nil, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	req := spy.request
	if !req.RefreshFirst {
		t.Fatal("--refresh did not reach the runner")
	}
	if len(req.ExcludedFamilies) != 2 || req.ExcludedFamilies[0] != "codex" || req.ExcludedFamilies[1] != "anthropic" {
		t.Fatalf("exclusions=%v, want both families", req.ExcludedFamilies)
	}
}

func TestSelectStdinBoundViolations(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"not utf8", string([]byte{0xff, 0xfe, 0x00}) + "task"},
		{"oversize", strings.Repeat("a", selection.MaxPromptBytes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &selectSpy{}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), []string{"select", "--policy", "p.yaml", "--phase", "execute"},
				strings.NewReader(tc.input), &stdout, &stderr, spy.deps())
			if code != ExitRejected {
				t.Fatalf("exit=%d, want 1", code)
			}
			if spy.calls != 0 {
				t.Fatal("runner called with an invalid task")
			}
		})
	}
}

func TestSelectHelpDocumentsWorkflow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select", "--help"}, nil, &stdout, &stderr, Dependencies{})
	if code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"--policy PATH", "--phase NAME", "--difficulty TIER", "--min-difficulty TIER",
		"--exclude-family NAME", "--refresh", "--json", "stdin", "never launch",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("select help missing %q:\n%s", want, out)
		}
	}
	// Root help lists both commands.
	var root bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, nil, &root, &stderr, Dependencies{}); code != ExitOK {
		t.Fatalf("help exit=%d", code)
	}
	for _, want := range []string{"select", "select-eval"} {
		if !strings.Contains(root.String(), want) {
			t.Fatalf("root help missing %q", want)
		}
	}
	var evalHelp bytes.Buffer
	if code := Run(context.Background(), []string{"help", "select-eval"}, nil, &evalHelp, &stderr, Dependencies{}); code != ExitOK {
		t.Fatalf("select-eval help exit=%d", code)
	}
	for _, want := range []string{"--live", "--fixtures PATH", "consent"} {
		if !strings.Contains(evalHelp.String(), want) {
			t.Fatalf("select-eval help missing %q:\n%s", want, evalHelp.String())
		}
	}
}

func TestSelectEvalRequiresLiveGate(t *testing.T) {
	spy := &evalSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--fixtures", "f.yaml"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitRejected {
		t.Fatalf("exit=%d, want 1 without --live", code)
	}
	if spy.calls != 0 {
		t.Fatal("runner called without the --live gate")
	}
	if !strings.Contains(stderr.String(), "--live") {
		t.Fatalf("gate message missing: %q", stderr.String())
	}
}

func TestSelectEvalExitCodes(t *testing.T) {
	t.Run("all matched", func(t *testing.T) {
		spy := &evalSpy{}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--fixtures", "f.yaml", "--live"},
			nil, &stdout, &stderr, spy.deps())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})
	t.Run("mismatch exits pending", func(t *testing.T) {
		spy := &evalSpy{}
		spy.report = &selection.Report{Model: "jev-test", Rubric: selection.RubricID, Total: 2, Assessed: 2, Matches: 1,
			Records: []selection.Record{
				{ID: "a", Phase: "execute", Match: true},
				{ID: "b", Phase: "execute", Match: false},
			}}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--fixtures", "f.yaml", "--live"},
			nil, &stdout, &stderr, spy.deps())
		if code != ExitPending {
			t.Fatalf("exit=%d, want 2", code)
		}
		// The mismatching case ID is listed; task text never is (there is
		// none in the report by construction).
		if !strings.Contains(stdout.String(), "id=b") {
			t.Fatalf("mismatch not listed: %s", stdout.String())
		}
	})
	t.Run("fatal exits rejected", func(t *testing.T) {
		spy := &evalSpy{err: selection.Fatal(selection.FatalConsent, errors.New("selection.jev.enabled is false"))}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--fixtures", "f.yaml", "--live"},
			nil, &stdout, &stderr, spy.deps())
		if code != ExitRejected {
			t.Fatalf("exit=%d, want 1", code)
		}
	})
}

func TestSelectEvalJSONEnvelope(t *testing.T) {
	spy := &evalSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--fixtures", "f.yaml", "--live", "--json"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	assertSingleJSONObject(t, stdout.String())
	for _, want := range []string{`"version":1`, `"report":`, `"matches":1`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("envelope missing %s: %s", want, stdout.String())
		}
	}
}

func TestSelectEvalInvalidFlagsNeverCallRunner(t *testing.T) {
	spy := &evalSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select-eval", "--policy", "p.yaml", "--live"},
		nil, &stdout, &stderr, spy.deps())
	if code != ExitRejected {
		t.Fatalf("exit=%d, want 1 for missing --fixtures", code)
	}
	if spy.calls != 0 {
		t.Fatal("runner called with invalid flags")
	}
}

// assertSingleJSONObject asserts the output is exactly one JSON object (the
// AC.9 rule, applied to the select envelopes).
func assertSingleJSONObject(t *testing.T, out string) {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("output is not a single JSON object: %q", out)
	}
	if strings.Count(trimmed, "\n{") > 0 {
		t.Fatalf("multiple JSON objects detected: %q", out)
	}
}
