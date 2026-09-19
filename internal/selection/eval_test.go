package selection

// Tests for the offline evaluation harness (eval.go) and the fixture
// schema. Everything here is synthetic and offline: the fake runner touches
// no network.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ----- helpers ---------------------------------------------------------------

// evalStepClock returns a deterministic clock advancing 1ms per call.
func evalStepClock() func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return time.Unix(0, 0).Add(time.Duration(n) * time.Millisecond)
	}
}

func evalAssessment(tier Tier) Assessment {
	return Assessment{Model: "eval-candidate-a", Tier: tier, Confidence: 0.9, InputTokens: 10, OutputTokens: 1}
}

func evalAbstention() Assessment {
	return Assessment{Model: "eval-candidate-a", Abstained: true, Confidence: 0.9}
}

// evalTierPolicy builds a phase policy with one trivial group per tier.
func evalTierPolicy() PhasePolicy {
	phase := PhasePolicy{}
	for _, tier := range Tiers {
		phase[tier] = TierPolicy{Groups: []Group{{"fake/example"}}}
	}
	return phase
}

// evalFloatEq compares floats at report precision.
func evalFloatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// ----- fixture schema ----------------------------------------------------------

const evalValidDoc = `version: 1
rubric: selection-difficulty-v1
fixtures:
  - id: execute-normal-case-001
    phase: execute
    prompt: Add a GET endpoint following the existing pattern.
    expected:
      tier: normal
  - id: plan-abstention-case-002
    phase: plan
    prompt: |
      Make it better. The details were discussed somewhere else.
    expected:
      abstention: true
`

func TestParseFixtureSetStrictContract(t *testing.T) {
	set, err := ParseFixtureSet([]byte(evalValidDoc))
	if err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	if set.Rubric != RubricID || len(set.Fixtures) != 2 {
		t.Fatalf("set = %+v", set)
	}
	f := set.Fixtures[0]
	if f.ID != "execute-normal-case-001" || f.Phase != "execute" || f.Expected.Tier != TierNormal || f.Expected.Abstention {
		t.Errorf("fixture 0 = %+v", f)
	}
	if got := set.Fixtures[1].Outcome(); !got.Abstained {
		t.Errorf("fixture 1 outcome = %+v, want abstention", got)
	}

	oversizePrompt := strings.Repeat("a", MaxPromptBytes+1)
	invalid := map[string]string{
		"unknown field":        evalValidDoc + "extra: 1\n",
		"duplicate id":         strings.Replace(evalValidDoc, "plan-abstention-case-002", "execute-normal-case-001", 1),
		"unknown tier":         strings.Replace(evalValidDoc, "tier: normal", "tier: impossible", 1),
		"empty id":             strings.Replace(evalValidDoc, "execute-normal-case-001", "  ", 1),
		"empty phase":          strings.Replace(evalValidDoc, "phase: execute", "phase: \"  \"", 1),
		"empty prompt":         strings.Replace(evalValidDoc, "Add a GET endpoint following the existing pattern.", "\"\"", 1),
		"whitespace prompt":    strings.Replace(evalValidDoc, "Add a GET endpoint following the existing pattern.", "\"   \"", 1),
		"oversize prompt":      strings.Replace(evalValidDoc, "Add a GET endpoint following the existing pattern.", "\""+oversizePrompt+"\"", 1),
		"wrong version":        strings.Replace(evalValidDoc, "version: 1", "version: 2", 1),
		"wrong rubric":         strings.Replace(evalValidDoc, "rubric: selection-difficulty-v1", "rubric: other-rubric", 1),
		"no fixtures":          strings.Replace(evalValidDoc, "fixtures:", "fixtures: []\nzombies:", 1),
		"tier and abstention":  evalValidDoc + "", // replaced below
		"neither expectation":  strings.Replace(evalValidDoc, "      tier: normal\n", "", 1),
		"trailing document":    evalValidDoc + "---\nversion: 1\n",
		"not utf-8":            "",
		"missing rubric field": strings.Replace(evalValidDoc, "rubric: selection-difficulty-v1\n", "", 1),
	}
	invalid["tier and abstention"] = strings.Replace(evalValidDoc,
		"      tier: normal\n", "      tier: normal\n      abstention: true\n", 1)

	for name, doc := range invalid {
		if name == "not utf-8" {
			if _, err := ParseFixtureSet([]byte{0xff, 0xfe}); err == nil {
				t.Errorf("%s: expected error", name)
			}
			continue
		}
		if _, err := ParseFixtureSet([]byte(doc)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestDocsSelectionFixturesLoadAndCover(t *testing.T) {
	set, err := LoadFixtureSet("../../docs/selection-fixtures.yaml")
	if err != nil {
		t.Fatalf("LoadFixtureSet: %v", err)
	}
	if set.Rubric != RubricID {
		t.Errorf("rubric = %q", set.Rubric)
	}
	seenTiers := map[Tier]int{}
	abstentions := 0
	phases := map[string]bool{}
	for i := range set.Fixtures {
		f := &set.Fixtures[i]
		if len(f.Prompt) > MaxPromptBytes {
			t.Errorf("fixture %s prompt over bound", f.ID)
		}
		phases[f.Phase] = true
		if f.Expected.Abstention {
			abstentions++
			continue
		}
		seenTiers[f.Expected.Tier]++
	}
	for _, tier := range Tiers {
		if seenTiers[tier] == 0 {
			t.Errorf("fixtures lack coverage for tier %s", tier)
		}
	}
	if abstentions == 0 {
		t.Error("fixtures lack abstention coverage")
	}
	for phase := range phases {
		switch phase {
		case "execute", "plan", "orchestrate":
		default:
			t.Errorf("fixture phase %q is outside the documented set", phase)
		}
	}

	// The documented fixture set must be covered by a policy carrying the
	// documented phases.
	p := Policy{Version: PolicyVersion, Phases: map[string]PhasePolicy{
		"execute":     evalTierPolicy(),
		"plan":        evalTierPolicy(),
		"orchestrate": evalTierPolicy(),
	}}
	if err := PolicyCoverage(p, set); err != nil {
		t.Errorf("documented fixtures must satisfy a covering policy: %v", err)
	}
}

// ----- policy coverage -----------------------------------------------------------

func TestPolicyCoverageRequiresPhaseAndTier(t *testing.T) {
	set := &FixtureSet{Fixtures: []Fixture{
		{ID: "a", Phase: "execute", Prompt: "p", Expected: ExpectedOutcome{Tier: TierNormal}},
		{ID: "b", Phase: "plan", Prompt: "p", Expected: ExpectedOutcome{Abstention: true}},
	}}
	p := Policy{Version: PolicyVersion, Phases: map[string]PhasePolicy{
		"execute": evalTierPolicy(),
		"plan":    evalTierPolicy(),
	}}
	if err := PolicyCoverage(p, set); err != nil {
		t.Errorf("covered set rejected: %v", err)
	}

	extended := append([]Fixture{}, set.Fixtures...)
	extended = append(extended, Fixture{ID: "c", Phase: "orchestrate", Prompt: "p", Expected: ExpectedOutcome{Tier: TierRoutine}})
	noOrchestrate := &FixtureSet{Fixtures: extended}
	err := PolicyCoverage(p, noOrchestrate)
	if err == nil || !strings.Contains(err.Error(), "orchestrate") {
		t.Errorf("missing phase err = %v, want orchestrate named", err)
	}

	partial := Policy{Version: PolicyVersion, Phases: map[string]PhasePolicy{
		"execute": {TierRoutine: TierPolicy{Groups: []Group{{"fake/example"}}}},
	}}
	err = PolicyCoverage(partial, &FixtureSet{Fixtures: set.Fixtures[:1]})
	if err == nil || !strings.Contains(err.Error(), "normal") {
		t.Errorf("missing tier err = %v, want tier named", err)
	}
}

// ----- report arithmetic -----------------------------------------------------------

func evalScriptedSet() *FixtureSet {
	return &FixtureSet{
		Rubric: RubricID,
		Fixtures: []Fixture{
			{ID: "f1", Phase: "execute", Prompt: "PROMPT-CANARY-1", Expected: ExpectedOutcome{Tier: TierRoutine}},
			{ID: "f2", Phase: "execute", Prompt: "p2", Expected: ExpectedOutcome{Tier: TierNormal}},
			{ID: "f3", Phase: "execute", Prompt: "p3", Expected: ExpectedOutcome{Tier: TierDifficult}},
			{ID: "f4", Phase: "plan", Prompt: "p4", Expected: ExpectedOutcome{Tier: TierVeryDifficult}},
			{ID: "f5", Phase: "plan", Prompt: "PROMPT-CANARY-2", Expected: ExpectedOutcome{Tier: TierNormal}},
			{ID: "f6", Phase: "orchestrate", Prompt: "p6", Expected: ExpectedOutcome{Tier: TierDifficult}},
		},
	}
}

func evalScriptedRunner() *FakeEvalRunner {
	tokens := []int64{10, 20, 30, 40, 50, 60}
	responses := map[string]Assessment{
		// f1: correct routine
		"f1": {Model: "eval-candidate-a", Tier: TierRoutine, Confidence: 0.9, InputTokens: tokens[0], OutputTokens: 1},
		// f2: under-classified (normal expected, routine actual)
		"f2": {Model: "eval-candidate-a", Tier: TierRoutine, Confidence: 0.9, InputTokens: tokens[1], OutputTokens: 2},
		// f3: over-classified (difficult expected, very_difficult actual)
		"f3": {Model: "eval-candidate-a", Tier: TierVeryDifficult, Confidence: 0.9, InputTokens: tokens[2], OutputTokens: 3},
		// f4: correct very_difficult
		"f4": {Model: "eval-candidate-a", Tier: TierVeryDifficult, Confidence: 0.9, InputTokens: tokens[3], OutputTokens: 4},
		// f5: abstained
		"f5": {Model: "eval-candidate-a", Abstained: true, Confidence: 0.9, InputTokens: tokens[4], OutputTokens: 5},
		// f6: correct difficult
		"f6": {Model: "eval-candidate-a", Tier: TierDifficult, Confidence: 0.9, InputTokens: tokens[5], OutputTokens: 6},
	}
	return &FakeEvalRunner{ModelName: "eval-candidate-a", Responses: responses}
}

func TestEvaluateReportArithmetic(t *testing.T) {
	report, err := Evaluate(context.Background(), evalScriptedRunner(), evalScriptedSet(),
		EvalOptions{Now: evalStepClock()})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if report.Model != "eval-candidate-a" || report.Rubric != RubricID {
		t.Errorf("model/rubric = %q/%q", report.Model, report.Rubric)
	}
	if report.Total != 6 || report.Assessed != 6 || report.Errors != 0 {
		t.Errorf("total/assessed/errors = %d/%d/%d", report.Total, report.Assessed, report.Errors)
	}
	if report.Matches != 3 {
		t.Errorf("matches = %d, want 3", report.Matches)
	}
	if report.AbstainedCount != 1 || !evalFloatEq(report.AbstentionRate, 1.0/6.0) {
		t.Errorf("abstention = %d @ %v", report.AbstainedCount, report.AbstentionRate)
	}
	if report.TierComparable != 5 {
		t.Errorf("tier comparable = %d, want 5", report.TierComparable)
	}
	if report.UnderCount != 1 || !evalFloatEq(report.UnderRate, 0.2) {
		t.Errorf("under = %d @ %v", report.UnderCount, report.UnderRate)
	}
	if report.OverCount != 1 || !evalFloatEq(report.OverRate, 0.2) {
		t.Errorf("over = %d @ %v", report.OverCount, report.OverRate)
	}
	if report.TotalInputTokens != 210 || report.TotalOutputTokens != 21 {
		t.Errorf("token totals = %d/%d, want 210/21", report.TotalInputTokens, report.TotalOutputTokens)
	}
	if report.AbstentionByExpectedTier[TierNormal] != 0.5 {
		t.Errorf("abstention by expected tier (normal) = %v, want 0.5", report.AbstentionByExpectedTier[TierNormal])
	}

	// Confusion: rows expected, columns actual.
	checks := []struct {
		e, a Tier
		want int
	}{
		{TierRoutine, TierRoutine, 1},
		{TierNormal, TierRoutine, 1},
		{TierDifficult, TierVeryDifficult, 1},
		{TierVeryDifficult, TierVeryDifficult, 1},
		{TierDifficult, TierDifficult, 1},
		{TierRoutine, TierNormal, 0},
		{TierNormal, TierNormal, 0},
	}
	for _, tc := range checks {
		if got := report.Confusion.Counts[TierIndex(tc.e)][TierIndex(tc.a)]; got != tc.want {
			t.Errorf("confusion[%s][%s] = %d, want %d", tc.e, tc.a, got, tc.want)
		}
	}

	if report.Latency.Samples != 6 || !evalFloatEq(report.Latency.MeanMS, 1.0) ||
		!evalFloatEq(report.Latency.P50MS, 1.0) || !evalFloatEq(report.Latency.P95MS, 1.0) ||
		!evalFloatEq(report.Latency.MaxMS, 1.0) {
		t.Errorf("latency stats = %+v", report.Latency)
	}

	if len(report.Records) != 6 {
		t.Fatalf("records = %d", len(report.Records))
	}
	if report.Records[0].ID != "f1" || !report.Records[0].Match {
		t.Errorf("record 0 = %+v", report.Records[0])
	}
	if report.Records[1].Match || report.Records[1].Actual.Tier != TierRoutine {
		t.Errorf("record 1 = %+v", report.Records[1])
	}
	if report.Records[4].Actual.Tier != "" || !report.Records[4].Actual.Abstained {
		t.Errorf("record 4 = %+v", report.Records[4])
	}
}

func TestEvaluateErrorsExcludedFromRates(t *testing.T) {
	set := &FixtureSet{Rubric: RubricID, Fixtures: []Fixture{
		{ID: "e1", Phase: "execute", Prompt: "p1", Expected: ExpectedOutcome{Tier: TierNormal}},
		{ID: "e2", Phase: "execute", Prompt: "p2", Expected: ExpectedOutcome{Tier: TierNormal}},
		{ID: "e3", Phase: "execute", Prompt: "p3", Expected: ExpectedOutcome{Tier: TierNormal}},
	}}
	runner := &FakeEvalRunner{
		ModelName: "eval-candidate-a",
		Errors: map[string]error{
			"e1": ErrAssessmentDisabled,
			"e2": &RemoteError{Status: 429, Kind: RemoteRateLimited},
		},
		Responses: map[string]Assessment{
			"e3": evalAssessment(TierNormal),
		},
	}
	report, err := Evaluate(context.Background(), runner, set, EvalOptions{Now: evalStepClock()})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if report.Errors != 2 || report.Assessed != 1 || report.Matches != 1 {
		t.Errorf("errors/assessed/matches = %d/%d/%d", report.Errors, report.Assessed, report.Matches)
	}
	if report.Records[0].Err != "disabled" || report.Records[1].Err != "remote_rate_limited" {
		t.Errorf("error kinds = %q/%q", report.Records[0].Err, report.Records[1].Err)
	}
	if !evalFloatEq(report.AbstentionRate, 0) || !evalFloatEq(report.UnderRate, 0) {
		t.Errorf("rates must exclude errored cases: %+v", report)
	}
	if report.TotalInputTokens != 10 {
		t.Errorf("tokens = %d, want only the assessed case", report.TotalInputTokens)
	}
}

func TestEvaluatePolicyValidationHook(t *testing.T) {
	runner := evalScriptedRunner()
	runner.Responses["f5"] = evalAbstention()
	hookCalls := 0
	opts := EvalOptions{
		Now: evalStepClock(),
		ValidatePolicy: func(phase string, actual Outcome) error {
			hookCalls++
			if actual.Abstained {
				return fmt.Errorf("abstention has no candidate in policy")
			}
			return nil
		},
	}
	report, err := Evaluate(context.Background(), runner, evalScriptedSet(), opts)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hookCalls != 6 {
		t.Errorf("hook calls = %d, want 6 (assessed cases only)", hookCalls)
	}
	if report.PolicyFailures != 1 {
		t.Errorf("policy failures = %d, want 1", report.PolicyFailures)
	}
	if !report.Records[4].PolicyRejected {
		t.Errorf("record 4 must be policy-rejected: %+v", report.Records[4])
	}
}

func TestReportJSONStaysSafe(t *testing.T) {
	report, err := Evaluate(context.Background(), evalScriptedRunner(), evalScriptedSet(),
		EvalOptions{Now: evalStepClock()})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	for _, banned := range []string{"PROMPT-CANARY", "\"prompt\"", "canary-credential"} {
		if strings.Contains(s, banned) {
			t.Errorf("report JSON leaks %s:\n%s", banned, s)
		}
	}
	for _, wanted := range []string{"\"expected\"", "\"actual\"", "\"id\"", "\"latency_ms\"", "\"confusion\"", "\"under_rate\"", "\"abstention_rate\""} {
		if !strings.Contains(s, wanted) {
			t.Errorf("report JSON missing %s", wanted)
		}
	}
}

func TestEvaluateGuardsAndCancellation(t *testing.T) {
	if _, err := Evaluate(context.Background(), nil, evalScriptedSet(), EvalOptions{}); err == nil {
		t.Error("nil runner must error")
	}
	if _, err := Evaluate(context.Background(), evalScriptedRunner(), nil, EvalOptions{}); err == nil {
		t.Error("nil set must error")
	}
	if _, err := Evaluate(context.Background(), evalScriptedRunner(), &FixtureSet{Rubric: RubricID}, EvalOptions{}); err == nil {
		t.Error("empty set must error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Evaluate(ctx, evalScriptedRunner(), evalScriptedSet(), EvalOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// ----- runner adapters -------------------------------------------------------------

func TestJevEvalRunnerAdaptsClient(t *testing.T) {
	transport := jevRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no network when disabled")
		return nil, errors.New("no network expected")
	})
	c := jevClient(t, transport, nil)
	disabled := &JevEvalRunner{Client: c, Enabled: false}
	if _, err := disabled.Assess(context.Background(), EvalRequest{Prompt: "p"}); !errors.Is(err, ErrAssessmentDisabled) {
		t.Errorf("err = %v, want ErrAssessmentDisabled", err)
	}

	live := &JevEvalRunner{Client: jevClient(t, jevRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jevResponse(http.StatusOK, jevBodyString(t, nil), req), nil
	}), nil), Enabled: true}
	if live.Model() != "jev-1.13.0" {
		t.Errorf("Model = %q", live.Model())
	}
	a, err := live.Assess(context.Background(), EvalRequest{ID: "x", Phase: "execute", Prompt: "p"})
	if err != nil || a.Tier != TierNormal {
		t.Errorf("assess = %+v, %v", a, err)
	}
}

func TestFakeEvalRunnerScripting(t *testing.T) {
	runner := &FakeEvalRunner{
		ModelName: "fake",
		Responses: map[string]Assessment{"a": evalAssessment(TierDifficult)},
		Errors:    map[string]error{"b": ErrResponseTooLarge},
	}
	if runner.Model() != "fake" {
		t.Errorf("Model = %q", runner.Model())
	}
	if a, err := runner.Assess(context.Background(), EvalRequest{ID: "a"}); err != nil || a.Tier != TierDifficult {
		t.Errorf("scripted response = %+v, %v", a, err)
	}
	if _, err := runner.Assess(context.Background(), EvalRequest{ID: "b"}); !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("scripted error = %v", err)
	}
	if _, err := runner.Assess(context.Background(), EvalRequest{ID: "unscripted"}); err == nil {
		t.Error("unscripted fixture must error")
	}
	unnamed := &FakeEvalRunner{}
	if unnamed.Model() != "fake-offline" {
		t.Errorf("default model = %q", unnamed.Model())
	}
}

// ----- bounded fixture reads and fatal credential handling -----------------------

func TestLoadFixtureSetBoundedRead(t *testing.T) {
	dir := t.TempDir()
	padded := func(total int) []byte {
		base := evalValidDoc + "\n# "
		if total < len(base) {
			t.Fatalf("bound %d smaller than the test document", total)
		}
		return []byte(base + strings.Repeat("x", total-len(base)))
	}

	// Exactly the bound parses; the trailing comment padding is ignored.
	atBound := filepath.Join(dir, "at-bound.yaml")
	if err := os.WriteFile(atBound, padded(MaxFixtureBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := LoadFixtureSet(atBound)
	if err != nil || set == nil || len(set.Fixtures) != 2 {
		t.Fatalf("at-bound document: set=%+v err=%v", set, err)
	}

	// One byte over the bound is rejected with the fixed oversize error,
	// before the document is ever buffered whole or parsed.
	over := filepath.Join(dir, "over.yaml")
	if err := os.WriteFile(over, padded(MaxFixtureBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadFixtureSet(over)
	if err == nil || !strings.Contains(err.Error(), "over the") || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversize document err = %v, want the fixed byte-limit error", err)
	}

	// Unreadable paths keep the fixtures-prefixed error.
	if _, err := LoadFixtureSet(filepath.Join(dir, "absent.yaml")); err == nil || !strings.HasPrefix(err.Error(), "selection: fixtures:") {
		t.Fatalf("absent file err = %v, want a fixtures-prefixed error", err)
	}
}

// evalCountingRunner records the order cases reach a runner.
type evalCountingRunner struct {
	calls int
	ids   []string
	err   error
}

func (r *evalCountingRunner) Model() string { return "stub-eval" }

func (r *evalCountingRunner) Assess(_ context.Context, req EvalRequest) (Assessment, error) {
	r.calls++
	r.ids = append(r.ids, req.ID)
	if r.err != nil {
		return Assessment{}, r.err
	}
	return evalAssessment(TierNormal), nil
}

func TestEvaluateMissingCredentialAbortsRun(t *testing.T) {
	set := &FixtureSet{Rubric: RubricID, Fixtures: []Fixture{
		{ID: "c1", Phase: "execute", Prompt: "p1", Expected: ExpectedOutcome{Tier: TierNormal}},
		{ID: "c2", Phase: "execute", Prompt: "p2", Expected: ExpectedOutcome{Tier: TierNormal}},
		{ID: "c3", Phase: "plan", Prompt: "p3", Expected: ExpectedOutcome{Abstention: true}},
	}}
	runner := &evalCountingRunner{err: ErrNoAPIKey}
	report, err := Evaluate(context.Background(), runner, set, EvalOptions{Now: evalStepClock()})
	if report != nil {
		t.Fatalf("a missing credential must not produce a report: %+v", report)
	}
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
	if runner.calls != 1 || len(runner.ids) != 1 || runner.ids[0] != "c1" {
		t.Fatalf("calls=%d ids=%v, want exactly the first case assessed before the abort", runner.calls, runner.ids)
	}
}
