package selection

// runner_test.go — orchestration contract: sequencing (refresh before
// snapshot), local validation before disclosure, fatal-versus-safe
// classification, floor semantics, and the guaranteed-absent behaviors
// (explicit tier never assesses; fatal input never invents a status).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// runnerAssessor is a scripted assessor recording every call.
type runnerAssessor struct {
	calls   int
	enabled bool
	prompt  string
	tier    Tier
	abstain bool
	err     error
}

func (a *runnerAssessor) Assess(_ context.Context, enabled bool, prompt string) (Assessment, error) {
	a.calls++
	a.enabled = enabled
	a.prompt = prompt
	if a.err != nil {
		return Assessment{}, a.err
	}
	return Assessment{Tier: a.tier, Abstained: a.abstain}, nil
}

// runnerSnapshot is a scripted snapshot source recording call order.
type runnerSnapshot struct {
	calls   int
	order   *int
	desired policy.Desired
	st      state.State
	asOf    time.Time
	err     error
}

func (s *runnerSnapshot) SelectionSnapshot(context.Context) (Snapshot, error) {
	s.calls++
	if s.order != nil {
		*s.order = 2 // refresh records 1
	}
	if s.err != nil {
		return Snapshot{}, s.err
	}
	return Snapshot{Desired: s.desired, State: s.st, AsOf: s.asOf}, nil
}

// runnerRefresher records how many times the opt-in refresh ran, and when.
type runnerRefresher struct {
	calls int
	order *int
	err   error
}

func (r *runnerRefresher) RefreshQuota(context.Context) error {
	r.calls++
	if r.order != nil {
		*r.order = 1 // snapshot overwrites with 2
	}
	return r.err
}

type runnerEnv struct {
	policyPath string
	snap       *runnerSnapshot
	assessor   *runnerAssessor
	refresher  *runnerRefresher
	built      int
}

// newAssessor is the scripted assessor factory; it records each build.
func (e *runnerEnv) newAssessor() func(string, time.Duration) (Assessor, error) {
	return func(string, time.Duration) (Assessor, error) {
		e.built++
		return e.assessor, nil
	}
}

func newRunnerEnv(t *testing.T, mutate func(*policy.Desired)) runnerEnv {
	t.Helper()
	desired := selDesired()
	if mutate != nil {
		mutate(&desired)
	}
	path := filepath.Join(t.TempDir(), "candidates.yaml")
	if err := os.WriteFile(path, []byte(selPolicyDoc), 0o600); err != nil {
		t.Fatalf("write candidate policy: %v", err)
	}
	return runnerEnv{
		policyPath: path,
		snap:       &runnerSnapshot{desired: desired, asOf: selNow},
		assessor:   &runnerAssessor{tier: TierNormal},
		refresher:  &runnerRefresher{},
	}
}

// freshCodexState returns a durable state holding a fresh, complete, available
// codex snapshot so explicit-tier paths can confirm against real evidence.
func freshCodexState() state.State {
	return selState(map[string]state.ProviderState{
		"codex": *selHealthyPS(
			selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable,
				selWin("five_hour", 0.1, 1.0)),
			nil,
		),
	})
}

func TestSelectRunnerExplicitTierSkipsAssessment(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.snap.st = freshCodexState()
	env.assessor.tier = TierRoutine // must never be consulted
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), Refresh: env.refresher}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath,
		Phase:      "execution",
		Tier:       TierNormal,
	})
	if err != nil {
		t.Fatalf("explicit tier: unexpected error: %v", err)
	}
	if out.Status != SelectConfirmed || out.Tier != TierNormal {
		t.Fatalf("status=%q tier=%q, want confirmed/normal", out.Status, out.Tier)
	}
	if out.Result.Reference != "codex/gpt-5.6-sol(medium)" || out.Result.Kind != KindConfirmed {
		t.Fatalf("unexpected result %+v", out.Result)
	}
	if env.assessor.calls != 0 {
		t.Fatalf("assessor called %d times for an explicit-tier request", env.assessor.calls)
	}
	if env.built != 0 {
		t.Fatalf("assessor built %d times for an explicit-tier request", env.built)
	}
	if out.Refreshed {
		t.Fatal("refresh recorded without a refresh request")
	}
	if !out.AsOf.Equal(selNow) {
		t.Fatalf("outcome AsOf=%s, want the snapshot clock sample %s", out.AsOf, selNow)
	}
}

func TestSelectRunnerAssessmentAndFloor(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
	env.assessor.tier = TierNormal
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), Refresh: env.refresher}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath,
		Phase:      "execution",
		MinTier:    TierDifficult,
		Prompt:     "task",
	})
	if err != nil {
		t.Fatalf("assessed selection: %v", err)
	}
	if out.Status != SelectUncertain {
		t.Fatalf("status=%q, want uncertain (difficult tier maps to zai with no evidence)", out.Status)
	}
	if out.AssessedTier != TierNormal || out.Tier != TierDifficult {
		t.Fatalf("assessed=%q tier=%q, want floor to raise normal to difficult", out.AssessedTier, out.Tier)
	}
	if env.assessor.calls != 1 || env.assessor.prompt != "task" {
		t.Fatalf("assessor calls=%d prompt=%q, want one call with the task", env.assessor.calls, env.assessor.prompt)
	}
	if !env.assessor.enabled {
		t.Fatal("assessor must be invoked with the desired-config consent flag enabled")
	}
}

func TestSelectRunnerFloorDoesNotLower(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
	env.assessor.tier = TierVeryDifficult
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", MinTier: TierRoutine, Prompt: "task",
	})
	if err != nil {
		t.Fatalf("floor must never lower: %v", err)
	}
	if out.Tier != TierVeryDifficult {
		t.Fatalf("tier=%q, want the higher assessed tier", out.Tier)
	}
}

func TestSelectRunnerFloorCannotRescueAbstention(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
	env.assessor.abstain = true
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath,
		Phase:      "execution",
		MinTier:    TierVeryDifficult,
		Prompt:     "task",
	})
	if err != nil {
		t.Fatalf("abstention must be a safe outcome, got error: %v", err)
	}
	if out.Status != SelectAssessmentUnavailable || !out.Abstained {
		t.Fatalf("status=%q abstained=%v, want assessment_unavailable abstention", out.Status, out.Abstained)
	}
	if out.Tier != "" || out.AssessedTier != "" {
		t.Fatalf("abstention must not name a tier (tier=%q assessed=%q)", out.Tier, out.AssessedTier)
	}
}

func TestSelectRunnerAssessmentErrorsClassify(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		fatal   bool
		consent bool
	}{
		{"remote unavailable", errors.New("selection: assessment request: timeout"), false, true},
		{"missing credential", ErrNoAPIKey, true, true},
		{"consent disabled", ErrAssessmentDisabled, true, false},
		{"prompt empty", ErrPromptEmpty, true, true},
		{"prompt not utf8", ErrPromptNotUTF8, true, true},
		{"prompt too large", ErrPromptTooLarge, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = tc.consent })
			env.assessor.err = tc.err
			r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

			out, err := r.Run(context.Background(), SelectRequest{
				PolicyPath: env.policyPath, Phase: "execution", Prompt: "task",
			})
			if tc.fatal {
				if err == nil {
					t.Fatalf("status=%q, want a fatal error", out.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("safe status expected, got error: %v", err)
			}
			if out.Status != SelectAssessmentUnavailable {
				t.Fatalf("status=%q, want assessment_unavailable", out.Status)
			}
		})
	}
}

func TestSelectRunnerConsentCheckedBeforeAssessorBuilt(t *testing.T) {
	// Consent disabled in the desired configuration: fatal, and the
	// assessor is neither built nor invoked.
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = false })
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Prompt: "task",
	}); err == nil {
		t.Fatal("missing consent must be a fatal error")
	}
	if env.built != 0 || env.assessor.calls != 0 {
		t.Fatalf("assessor built=%d called=%d with consent disabled, want neither",
			env.built, env.assessor.calls)
	}
}

func TestSelectRunnerRefreshOnceBeforeSnapshot(t *testing.T) {
	env := newRunnerEnv(t, nil)
	order := 0
	env.snap.order = &order
	env.refresher.order = &order
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), Refresh: env.refresher}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine, RefreshFirst: true,
	})
	if err != nil {
		t.Fatalf("refresh run: %v", err)
	}
	if !out.Refreshed || env.refresher.calls != 1 {
		t.Fatalf("refreshed=%v calls=%d, want exactly one refresh", out.Refreshed, env.refresher.calls)
	}
	if order != 2 {
		t.Fatalf("order=%d, want refresh(1) before snapshot(2)", order)
	}
}

func TestSelectRunnerNoRefreshByDefault(t *testing.T) {
	env := newRunnerEnv(t, nil)
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), Refresh: env.refresher}
	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine,
	}); err != nil {
		t.Fatalf("default run: %v", err)
	}
	if env.refresher.calls != 0 {
		t.Fatalf("default selection polled quota %d times, want zero", env.refresher.calls)
	}
}

func TestSelectRunnerRefreshFatalStopsSelection(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.refresher.err = errors.New("service: acquire lock: boom")
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), Refresh: env.refresher}

	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine, RefreshFirst: true,
	}); err == nil {
		t.Fatal("a fatal refresh must stop selection")
	}
	if env.snap.calls != 0 {
		t.Fatalf("snapshot loaded %d times after a fatal refresh", env.snap.calls)
	}
}

func TestSelectRunnerLocalValidationBeforeDisclosure(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	// An unknown phase must fail before the prompt could reach the assessor.
	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "nonexistent", Prompt: "task",
	}); err == nil {
		t.Fatal("unknown phase must be a fatal error")
	}
	if env.assessor.calls != 0 {
		t.Fatalf("assessor called %d times despite a locally invalid phase", env.assessor.calls)
	}
	if env.built != 0 {
		t.Fatalf("assessor built %d times despite a locally invalid phase", env.built)
	}
}

func TestSelectRunnerFatalInputs(t *testing.T) {
	env := newRunnerEnv(t, nil)
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("version: 2\nphases: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  SelectRequest
	}{
		{"missing policy path", SelectRequest{Phase: "execution", Tier: TierRoutine}},
		{"missing phase", SelectRequest{PolicyPath: env.policyPath, Tier: TierRoutine}},
		{"conflict", SelectRequest{PolicyPath: env.policyPath, Phase: "execution", Tier: TierNormal, MinTier: TierNormal}},
		{"bad tier", SelectRequest{PolicyPath: env.policyPath, Phase: "execution", Tier: Tier("hardest")}},
		{"bad floor", SelectRequest{PolicyPath: env.policyPath, Phase: "execution", MinTier: Tier("hardest")}},
		{"missing policy file", SelectRequest{PolicyPath: filepath.Join(t.TempDir(), "absent.yaml"), Phase: "execution", Tier: TierRoutine}},
		{"broken policy file", SelectRequest{PolicyPath: broken, Phase: "execution", Tier: TierRoutine}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), tc.req); err == nil {
				t.Fatalf("%s: want fatal error", tc.name)
			}
		})
	}
}

func TestSelectRunnerSnapshotFatal(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.snap.err = errors.New("load policy failed")
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}
	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine,
	}); err == nil {
		t.Fatal("a fatal snapshot must stop selection")
	}
}

func TestSelectRunnerNoSelectionStatus(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.snap.st = freshCodexState()
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	// The planning difficult tier maps to a baseline-disabled model with no
	// other candidates: excluded outright, no_selection, not an error.
	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "planning", Tier: TierDifficult,
	})
	if err != nil {
		t.Fatalf("no_selection must be a safe outcome, got error: %v", err)
	}
	if out.Status != SelectNoSelection || out.Result.Reference != "" {
		t.Fatalf("status=%q result=%+v, want empty no_selection", out.Status, out.Result)
	}
}

func TestSelectRunnerUncertainStatus(t *testing.T) {
	env := newRunnerEnv(t, nil) // empty state: missing evidence everywhere
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine,
	})
	if err != nil {
		t.Fatalf("uncertain must be a safe outcome, got error: %v", err)
	}
	if out.Status != SelectUncertain || out.Result.Kind != KindUncertain {
		t.Fatalf("status=%q kind=%q, want uncertain", out.Status, out.Result.Kind)
	}
	if out.Result.Evidence == "" {
		t.Fatal("uncertain result must carry its evidence category")
	}
}

// --- EvaluationRunner (select-eval orchestration) --------------------------------

// evalFixtureDoc is a valid fixture set over the shared candidate policy's
// execution and planning phases.
const evalFixtureDoc = `version: 1
rubric: selection-difficulty-v1
fixtures:
  - id: case-1
    phase: execution
    prompt: one
    expected:
      tier: normal
  - id: case-2
    phase: execution
    prompt: two
    expected:
      tier: routine
  - id: case-3
    phase: planning
    prompt: three
    expected:
      abstention: true
`

// evalStub is a scripted EvalRunner recording every assessed case in order.
type evalStub struct {
	calls     int
	ids       []string
	responses map[string]Assessment
	errs      map[string]error
	onAssess  func()
}

func (s *evalStub) Model() string { return "stub-eval" }

func (s *evalStub) Assess(_ context.Context, req EvalRequest) (Assessment, error) {
	s.calls++
	s.ids = append(s.ids, req.ID)
	if s.onAssess != nil {
		s.onAssess()
	}
	if err := s.errs[req.ID]; err != nil {
		return Assessment{}, err
	}
	if a, ok := s.responses[req.ID]; ok {
		return a, nil
	}
	return Assessment{}, errors.New("stub: unscripted case")
}

// newEvalEnv writes the shared candidate policy and a fixture set over its
// phases, returning both paths.
func newEvalEnv(t *testing.T) (policyPath, fixturesPath string) {
	t.Helper()
	dir := t.TempDir()
	policyPath = filepath.Join(dir, "candidates.yaml")
	if err := os.WriteFile(policyPath, []byte(selPolicyDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	fixturesPath = filepath.Join(dir, "fixtures.yaml")
	if err := os.WriteFile(fixturesPath, []byte(evalFixtureDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	return policyPath, fixturesPath
}

// newEvaluationRunner builds an EvaluationRunner with consent enabled in the
// desired configuration and the stub behind the live-assessor factory.
func newEvaluationRunner(stub *evalStub) *EvaluationRunner {
	desired := selDesired()
	desired.Selection.Jev.Enabled = true
	return &EvaluationRunner{
		Snapshot: &runnerSnapshot{desired: desired, asOf: selNow},
		NewJev:   func(string, time.Duration) (EvalRunner, error) { return stub, nil },
	}
}

func TestEvaluationRunnerMissingCredentialIsFatal(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	stub := &evalStub{errs: map[string]error{"case-1": ErrNoAPIKey}}

	report, err := newEvaluationRunner(stub).RunEval(context.Background(),
		EvalInvocation{PolicyPath: policyPath, FixturesPath: fixturesPath})
	if report != nil {
		t.Fatalf("a missing credential must not produce a report: %+v", report)
	}
	if err == nil {
		t.Fatal("a missing credential must be a fatal error")
	}
	var ferr *FatalError
	if !errors.As(err, &ferr) || ferr.Kind != FatalCredential {
		t.Fatalf("err = %v (%T), want FatalError{FatalCredential}", err, err)
	}
	if ferr.Error() != "selection: assessment credential is unavailable" {
		t.Fatalf("fatal text = %q, want the fixed safe sentence", ferr.Error())
	}
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("cause chain lost ErrNoAPIKey: %v", err)
	}
	if stub.calls != 1 || len(stub.ids) != 1 || stub.ids[0] != "case-1" {
		t.Fatalf("assessed %d cases (%v), want only case-1 before the fatal stop", stub.calls, stub.ids)
	}
}

func TestEvaluationRunnerRemoteFailuresRemainReportErrors(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	stub := &evalStub{
		responses: map[string]Assessment{
			"case-1": {Model: "stub-eval", Tier: TierNormal, Confidence: 0.9},
			"case-3": {Model: "stub-eval", Abstained: true, Confidence: 0.9},
		},
		errs: map[string]error{
			"case-2": &RemoteError{Status: 503, Kind: RemoteServer},
		},
	}

	report, err := newEvaluationRunner(stub).RunEval(context.Background(),
		EvalInvocation{PolicyPath: policyPath, FixturesPath: fixturesPath})
	if err != nil {
		t.Fatalf("remote failures are report errors, not fatal: %v", err)
	}
	if report == nil || report.Total != 3 || report.Assessed != 2 || report.Errors != 1 {
		t.Fatalf("report totals = %+v", report)
	}
	if report.Records[1].ID != "case-2" || report.Records[1].Err != "remote_server" {
		t.Fatalf("record 1 = %+v, want the classified remote error", report.Records[1])
	}
	if !report.Records[0].Match || !report.Records[2].Actual.Abstained {
		t.Fatalf("healthy cases must still be assessed: %+v", report.Records)
	}
}

func TestEvaluationRunnerCancellationStopsRun(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	stub := &evalStub{
		responses: map[string]Assessment{
			"case-1": {Model: "stub-eval", Tier: TierNormal, Confidence: 0.9},
			"case-2": {Model: "stub-eval", Tier: TierRoutine, Confidence: 0.9},
			"case-3": {Model: "stub-eval", Abstained: true, Confidence: 0.9},
		},
		onAssess: cancel, // the user cancels during case-1's assessment
	}

	report, err := newEvaluationRunner(stub).RunEval(ctx,
		EvalInvocation{PolicyPath: policyPath, FixturesPath: fixturesPath})
	cancel()
	if report != nil {
		t.Fatalf("a canceled run must not produce a report: %+v", report)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled preserved", err)
	}
	if stub.calls != 1 || len(stub.ids) != 1 || stub.ids[0] != "case-1" {
		t.Fatalf("assessed %d cases (%v), want case-1 only — cancellation must not loop the rest", stub.calls, stub.ids)
	}
}

// CR-COMP39-2: a canceled run is classified as its own fatal kind carrying
// the fixed safe cancellation sentence — not FatalPolicy, which would claim
// the candidate policy or fixtures were missing, unreadable, or invalid.
func TestEvaluationRunnerCancellationIsClassifiedCanceled(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	stub := &evalStub{
		responses: map[string]Assessment{
			"case-1": {Model: "stub-eval", Tier: TierNormal, Confidence: 0.9},
		},
		onAssess: cancel, // the caller cancels during case-1's assessment
	}

	_, err := newEvaluationRunner(stub).RunEval(ctx,
		EvalInvocation{PolicyPath: policyPath, FixturesPath: fixturesPath})
	cancel()
	var ferr *FatalError
	if !errors.As(err, &ferr) {
		t.Fatalf("err = %v (%T), want a *FatalError", err, err)
	}
	if ferr.Kind != FatalCanceled {
		t.Fatalf("kind = %q, want %q — cancellation is not a policy failure", ferr.Kind, FatalCanceled)
	}
	if got, want := ferr.Error(), "selection: run was canceled before completion"; got != want {
		t.Fatalf("fatal text = %q, want the fixed safe sentence %q", got, want)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cause chain lost context.Canceled: %v", err)
	}
}

// CR-COMP39-2: an expired run deadline is the same classification as
// cancellation — the run could not complete — never FatalPolicy.
func TestEvaluationRunnerDeadlineIsClassifiedCanceled(t *testing.T) {
	policyPath, fixturesPath := newEvalEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	stub := &evalStub{}

	_, err := newEvaluationRunner(stub).RunEval(ctx,
		EvalInvocation{PolicyPath: policyPath, FixturesPath: fixturesPath})
	var ferr *FatalError
	if !errors.As(err, &ferr) || ferr.Kind != FatalCanceled {
		t.Fatalf("err = %v (%T), want FatalError{FatalCanceled}", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cause chain lost context.DeadlineExceeded: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("assessed %d cases under an expired deadline, want none", stub.calls)
	}
}

func TestSelectRunnerWhitespaceTaskRejectedBeforeAssessor(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	for _, prompt := range []string{"   ", "\t\n", ""} {
		if _, err := r.Run(context.Background(), SelectRequest{
			PolicyPath: env.policyPath, Phase: "execution", Prompt: prompt,
		}); err == nil {
			t.Fatalf("prompt %q: an unusable task must be a fatal error", prompt)
		}
		if env.assessor.calls != 0 || env.built != 0 {
			t.Fatalf("prompt %q: assessor built=%d called=%d, want neither", prompt, env.built, env.assessor.calls)
		}
	}
	var ferr *FatalError
	if _, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Prompt: " ",
	}); !errors.As(err, &ferr) || ferr.Kind != FatalTask {
		t.Fatalf("err = %v, want FatalError{FatalTask}", err)
	}
}

func TestSelectRunnerEvidenceCheckedAtNullable(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.snap.st = freshCodexState()
	r := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor()}

	// A timestamped quota snapshot publishes its evidence date.
	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine,
	})
	if err != nil {
		t.Fatalf("confirmed run: %v", err)
	}
	if out.EvidenceCheckedAt == nil || !out.EvidenceCheckedAt.Equal(selNow.Add(-time.Minute)) {
		t.Fatalf("evidence checked at = %v, want the snapshot's timestamp", out.EvidenceCheckedAt)
	}

	// A quota snapshot without a timestamp keeps null provenance.
	undated := freshCodexState()
	if ps := undated.Providers["codex"]; ps.QuotaSnapshot != nil {
		ps.QuotaSnapshot.CheckedAt = time.Time{}
		undated.Providers["codex"] = ps
	}
	env.snap.st = undated
	out, err = r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath, Phase: "execution", Tier: TierRoutine,
	})
	if err != nil {
		t.Fatalf("undated run: %v", err)
	}
	if out.EvidenceCheckedAt != nil {
		t.Fatalf("evidence checked at = %v, want nil for a missing timestamp", out.EvidenceCheckedAt)
	}
}

// TestSelectRunnerLayaBackendRoutesToLayaFactory proves an enabled laya
// backend constructs (only) the laya assessor and assesses through it.
func TestSelectRunnerLayaBackendRoutesToLayaFactory(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) {
		d.Selection.Jev.Enabled = false
		d.Selection.Laya.Enabled = true
	})
	env.snap.st = freshCodexState()
	env.assessor.tier = TierNormal
	layaBuilt := 0
	r := &SelectRunner{
		Snapshot: env.snap,
		Refresh:  env.refresher,
		NewLayaAssessor: func(time.Duration) (Assessor, error) {
			layaBuilt++
			return env.assessor, nil
		},
	}
	out, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath,
		Phase:      "execution",
		Prompt:     "refactor the quota reconciler lock",
	})
	if err != nil {
		t.Fatalf("laya-backed run: %v", err)
	}
	if out.Status != SelectConfirmed || out.Tier != TierNormal {
		t.Fatalf("status=%q tier=%q, want confirmed/normal", out.Status, out.Tier)
	}
	if layaBuilt != 1 {
		t.Fatalf("laya factory built %d times, want exactly one", layaBuilt)
	}
	if env.built != 0 {
		t.Fatalf("jev factory built %d times for a laya-backed request", env.built)
	}
}

// TestSelectRunnerNoBackendEnabledIsConsent proves the both-disabled case is
// a consent failure before any factory is consulted.
func TestSelectRunnerNoBackendEnabledIsConsent(t *testing.T) {
	env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = false })
	r := &SelectRunner{
		Snapshot:        env.snap,
		NewAssessor:     env.newAssessor(),
		NewLayaAssessor: func(time.Duration) (Assessor, error) { return env.assessor, nil },
	}
	_, err := r.Run(context.Background(), SelectRequest{
		PolicyPath: env.policyPath,
		Phase:      "execution",
		Prompt:     "refactor the quota reconciler lock",
	})
	if err == nil {
		t.Fatal("both backends disabled must not assess")
	}
	var fe *FatalError
	if !errors.As(err, &fe) || fe.Kind != FatalConsent {
		t.Fatalf("err = %v, want a FatalConsent failure", err)
	}
	if cause := fe.Unwrap(); cause == nil || !strings.Contains(cause.Error(), "backend is enabled") {
		t.Fatalf("cause = %v, want the backend consent cause", cause)
	}
	if env.built != 0 {
		t.Fatalf("jev factory built %d times without consent", env.built)
	}
}
