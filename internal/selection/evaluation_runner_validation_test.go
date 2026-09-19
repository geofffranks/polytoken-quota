package selection

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEvaluationRunnerConsentBlocksNewJev(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	stub := &evalStub{}
	runner := newEvaluationRunner(stub)
	snap := runner.Snapshot.(*runnerSnapshot)
	snap.desired.Selection.Jev.Enabled = false
	built := false
	runner.NewJev = func(string, time.Duration) (EvalRunner, error) {
		built = true
		return stub, nil
	}

	report, err := runner.RunEval(context.Background(), EvalInvocation{
		PolicyPath: policyPath, FixturesPath: fixturesPath,
	})
	if report != nil {
		t.Fatalf("consent failure must not produce a report: %+v", report)
	}
	var fatalErr *FatalError
	if !errors.As(err, &fatalErr) || fatalErr.Kind != FatalConsent {
		t.Fatalf("err = %v, want FatalConsent", err)
	}
	if built || stub.calls != 0 {
		t.Fatalf("NewJev built=%v, assessor calls=%d with consent disabled; want neither", built, stub.calls)
	}
}

func TestEvaluationRunnerInvalidCandidateInUnrelatedTierBlocksAssessment(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	policy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policy = append(policy, []byte("\n# invalid unrelated candidate\n")...)
	policy = []byte(strings.Replace(string(policy), "    very_difficult:\n      - - codex/gpt-5.6-sol\n", "    very_difficult:\n      - - unregistered/provider-model\n", 1))
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}

	stub := &evalStub{}
	report, err := newEvaluationRunner(stub).RunEval(context.Background(), EvalInvocation{
		PolicyPath: policyPath, FixturesPath: fixturesPath,
	})
	if report != nil {
		t.Fatalf("invalid candidate policy must not produce a report: %+v", report)
	}
	var fatalErr *FatalError
	if !errors.As(err, &fatalErr) || fatalErr.Kind != FatalPolicy {
		t.Fatalf("err = %v, want FatalPolicy", err)
	}
	if stub.calls != 0 {
		t.Fatalf("assessed %d cases despite invalid unrelated tier", stub.calls)
	}
}

func TestEvaluationRunnerLaterMissingFixtureTierFailsBeforeAssessment(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	policy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policy = []byte(strings.Replace(string(policy), "    normal: [[anthropic/claude-opus]]\n", "", 1))
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	fixtures, err := os.ReadFile(fixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	fixtures = []byte(strings.Replace(string(fixtures), "    phase: planning\n    prompt: three\n    expected:\n      abstention: true", "    phase: planning\n    prompt: three\n    expected:\n      tier: normal", 1))
	if err := os.WriteFile(fixturesPath, fixtures, 0o600); err != nil {
		t.Fatal(err)
	}

	stub := &evalStub{}
	report, err := newEvaluationRunner(stub).RunEval(context.Background(), EvalInvocation{
		PolicyPath: policyPath, FixturesPath: fixturesPath,
	})
	if report != nil {
		t.Fatalf("coverage failure must not produce a report: %+v", report)
	}
	var fatalErr *FatalError
	if !errors.As(err, &fatalErr) || fatalErr.Kind != FatalPolicy {
		t.Fatalf("err = %v, want FatalPolicy", err)
	}
	if stub.calls != 0 {
		t.Fatalf("assessed %d cases before later fixture coverage was validated", stub.calls)
	}
}

func TestEvaluationRunnerActualMissingTierIsReportPolicyFailure(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	policy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policy = []byte(strings.Replace(string(policy), "    normal: [[anthropic/claude-opus]]\n", "", 1))
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}

	stub := &evalStub{responses: map[string]Assessment{
		"case-1": {Model: "stub-eval", Tier: TierNormal},
		"case-2": {Model: "stub-eval", Tier: TierRoutine},
		"case-3": {Model: "stub-eval", Tier: TierNormal},
	}}
	report, err := newEvaluationRunner(stub).RunEval(context.Background(), EvalInvocation{
		PolicyPath: policyPath, FixturesPath: fixturesPath,
	})
	if err != nil {
		t.Fatalf("actual missing tier is a report policy failure, not fatal: %v", err)
	}
	if report == nil || report.Total != 3 || report.Assessed != 3 || report.PolicyFailures != 1 {
		t.Fatalf("report totals = %+v, want total/assessed=3 and one policy failure", report)
	}
	if stub.calls != 3 {
		t.Fatalf("assessed %d cases, want all 3", stub.calls)
	}
	if !report.Records[2].PolicyRejected || report.Records[2].ID != "case-3" {
		t.Fatalf("record 3 = %+v, want policy-rejected case-3", report.Records[2])
	}
}
