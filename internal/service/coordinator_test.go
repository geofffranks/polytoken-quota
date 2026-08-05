package service

// Task 12 coordinator tests. These verify the one common locked transaction
// path, partial success, strict create-only init, dry-run non-mutation, and the
// single locked cycle used by Set/Clear. A spy implements every injected
// dependency and records the exact operation order into Trace, plus counters for
// state saves, target publishes, and validation intents.
//
// The spy is the authoritative observable: the Coordinator emits each
// transaction step through a Tracer, so internal steps (accept-revision,
// record-pending) and per-kind granularity (detailed for the hook path, coarse
// for Set/Clear) are observable without the dependencies themselves recording.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/hook"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

const validTargetKey = "valid"

type testSourceReader struct{}

func (testSourceReader) Global(context.Context) (policy.SourceSet, error) {
	return policy.SourceSet{ID: "global", Global: true, Config: policy.SourceConfig{Providers: []policy.SourceMapping{{ID: "codex", CodexBarProviders: []string{"codex"}, PolytokenProviders: []string{"codex"}, Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}}}}, Definitions: []policy.SourceDefinition{{Path: "model.md", Model: "codex/gpt"}}}, nil
}
func (testSourceReader) Projects(context.Context) ([]policy.SourceSet, error) { return nil, nil }

// --- coordinator spy --------------------------------------------------------

// coordinatorSpy is a test double implementing every Coordinator dependency. It
// records the operation order into Trace (via the Tracer the Coordinator calls)
// and counts state saves, target publishes, and validation intents. It holds a
// named Coordinator field (not embedded) so dependency method names never clash
// with the Coordinator's own fields.
type coordinatorSpy struct {
	Coordinator       // named field; NOT embedded, so no field promotion clashes
	Trace             []string
	StateSaves        int
	LastSaved         state.State
	Publishes         int
	ValidationIntents int
	targetList        []RegisteredTarget
	invalidTargets    map[string]bool
	desiredExists     bool
	files             map[string]string
	stageRoot         string
	resolveErr        error
}

func newCoordinatorSpy(specs ...string) *coordinatorSpy {
	spy := &coordinatorSpy{
		Trace:          []string{},
		invalidTargets: map[string]bool{},
		files:          map[string]string{},
	}
	spy.Coordinator = Coordinator{
		Lock:         spy,
		Policy:       spy,
		PolicyWriter: spy,
		State:        spy,
		Targets:      spy,
		Builder:      spy,
		Stage:        spy,
		Validate:     spy,
		Publish:      spy,
		Clock:        spy,
	}
	spy.Coordinator.tracer = spy
	for _, spec := range specs {
		if spec == "desired-exists" {
			spy.desiredExists = true
		}
	}
	return spy
}

// withTargets registers synthetic targets as (id, outcome) pairs. A target whose
// outcome is not "valid" fails validation and stays pending.
func (s *coordinatorSpy) withTargets(idOutcome ...string) *coordinatorSpy {
	for i := 0; i+1 < len(idOutcome); i += 2 {
		id, outcome := idOutcome[i], idOutcome[i+1]
		t := policy.Target{ID: id, Global: id == "global"}
		r := target.Resolved{ID: id, CanonicalRoot: "/" + id, Global: id == "global"}
		s.targetList = append(s.targetList, RegisteredTarget{Policy: t, Resolved: r})
		if outcome != validTargetKey {
			s.invalidTargets[id] = true
		}
	}
	return s
}

func (s *coordinatorSpy) Step(step string) { s.Trace = append(s.Trace, step) }

func (s *coordinatorSpy) Now() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

type failingValidator struct{ result validate.Result }

func (v failingValidator) Validate(_ context.Context, _ staging.Candidate, _ time.Duration) validate.Result {
	return v.result
}

// --- Locker ---
func (s *coordinatorSpy) Lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

// --- PolicyLoader ---
func (s *coordinatorSpy) LoadPolicy() (policy.Desired, error) {
	return policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex-mapping": {
			CodexBarProviders: []string{"codex"},
			Models:            map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
		},
	}}, nil
}
func (s *coordinatorSpy) DesiredExists() bool { return s.desiredExists }

// --- policy.Writer ---
func (s *coordinatorSpy) CreateAtomic(context.Context, policy.Desired) error {
	if s.desiredExists {
		return policy.ErrDesiredExists
	}
	s.files["desired.yaml"] = "created"
	return nil
}
func (s *coordinatorSpy) ReplaceAtomic(context.Context, policy.Desired) error {
	s.files["desired.yaml"] = "replaced"
	return nil
}

// --- StateStore ---
// The observed state starts at revision 1 so an accepted event advances to 2.
func (s *coordinatorSpy) LoadState() (state.State, error) {
	return state.State{Revision: 1, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}, nil
}
func (s *coordinatorSpy) Save(st state.State) error {
	s.StateSaves++
	s.LastSaved = st
	s.files["state.json"] = "saved"
	return nil
}

// --- TargetRegistry ---
func (s *coordinatorSpy) ResolveTargets(policy.Desired) ([]RegisteredTarget, error) {
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	return s.targetList, nil
}

// --- Reconciler ---
func (s *coordinatorSpy) Build(_ policy.Desired, _ state.State, t policy.Target, _ reconcile.RankLookup) (reconcile.Plan, error) {
	return reconcile.Plan{TargetID: t.ID}, nil
}

// --- Stager ---
func (s *coordinatorSpy) Stage(_ context.Context, res target.Resolved, plan reconcile.Plan) (staging.Candidate, error) {
	id := res.ID
	if id == "" {
		id = plan.TargetID
	}
	root := s.stageRoot
	if root == "" {
		root = "/tmp/synthetic-stage"
	}
	return staging.Candidate{Root: root, TargetID: id}, nil
}

// --- Validator ---
func (s *coordinatorSpy) Validate(_ context.Context, c staging.Candidate, _ time.Duration) validate.Result {
	s.ValidationIntents++
	if s.invalidTargets[c.TargetID] {
		return validate.Result{ConfigValid: true, Error: &validate.CommandError{Stage: validate.Doctor, Summary: "synthetic startup failure"}}
	}
	return validate.Result{ConfigValid: true, StartupValid: true}
}

// --- Publisher ---
// Recover returns the observed state at revision 1 so an accepted event
// advances to revision 2 (the Coordinator uses the recovered state).
func (s *coordinatorSpy) Recover(context.Context, state.State) (state.State, error) {
	return state.State{Revision: 1, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}, nil
}

// ApplyUnderLock mirrors the production seam: the Coordinator already holds the
// lock, so the publisher does not re-acquire it. The spy records the publish.
func (s *coordinatorSpy) ApplyUnderLock(context.Context, publish.Transaction) (state.State, error) {
	s.Publishes++
	return state.State{}, nil
}

// --- Snapshot for init non-mutation ---
func (s *coordinatorSpy) Snapshot() map[string]string {
	out := make(map[string]string, len(s.files))
	for k, v := range s.files {
		out[k] = v
	}
	return out
}

// --- helpers ---
func event(t hook.Type, seq int) hook.Event {
	return hook.Event{Type: t, Provider: "codex", Timestamp: time.Date(2026, 7, 19, 12, 0, seq, 0, time.UTC)}
}

func count(trace []string, needle string) int {
	n := 0
	for _, s := range trace {
		if s == needle {
			n++
		}
	}
	return n
}

// --- tests (binding, from the Task 12 blueprint) ----------------------------

func TestCoordinatorOrderAndPartialSuccess(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey, "project", "invalid")
	out := spy.Coordinator.HandleEvent(context.Background(), event(hook.QuotaReached, 2))
	want := []string{
		"lock", "load-state", "recover",
		"load-policy", "load-state", "load-sources", "accept-revision",
		"render:global", "stage:global", "validate:global", "publish:global",
		"render:project", "stage:project", "validate:project", "record-pending:project",
		"save-state", "unlock",
	}
	if !reflect.DeepEqual(spy.Trace, want) {
		t.Fatalf("trace mismatch:\ngot =%v\nwant=%v", spy.Trace, want)
	}
	if !out.Accepted || out.Revision != 2 || out.PendingCount() != 1 {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestRejectedInputDoesNotMutate(t *testing.T) {
	spy := newCoordinatorSpy()
	out := spy.Coordinator.HandleEvent(context.Background(), hook.Event{})
	if out.Accepted || spy.StateSaves != 0 || spy.Publishes != 0 {
		t.Fatalf("out=%+v saves=%d publishes=%d", out, spy.StateSaves, spy.Publishes)
	}
}

func TestDryRunIsNonMutating(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	out := spy.Coordinator.Reconcile(context.Background(), true, false)
	if out.Error != nil || spy.StateSaves != 0 || spy.Publishes != 0 || spy.ValidationIntents == 0 {
		t.Fatalf("out=%+v saves=%d publishes=%d intents=%d", out, spy.StateSaves, spy.Publishes, spy.ValidationIntents)
	}
}

func TestPendingValidationCarriesAttemptTimestampAndDiagnostics(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	when := time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC)
	spy.Coordinator.Clock = fixedClock{t: when}
	spy.Coordinator.Validate = failingValidator{result: validate.Result{
		Error: &validate.CommandError{
			Stage:       validate.ConfigValidate,
			Summary:     "config validate: invalid model chain",
			Remediation: "inspect staged config",
		},
	}}

	out := spy.Coordinator.Reconcile(context.Background(), false, false)
	if out.PendingCount() != 1 {
		t.Fatalf("out=%+v", out)
	}
	pending := out.Targets[0].Pending
	if pending == nil || pending.Summary != "config validate: invalid model chain" || pending.Remediation != "inspect staged config" {
		t.Fatalf("pending=%+v", pending)
	}
	if !pending.AttemptedAt.Equal(when) {
		t.Fatalf("pending attempted_at=%v want %v", pending.AttemptedAt, when)
	}
}

func TestDryRunKeepStagingCleansSuccessfulCandidate(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.stageRoot = t.TempDir()
	out := spy.Coordinator.Reconcile(context.Background(), true, true)
	if out.PendingCount() != 0 || out.Targets[0].StagingRoot != "" {
		t.Fatalf("out=%+v", out)
	}
}

func TestDryRunKeepStagingRetainsFailedCandidate(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.stageRoot = t.TempDir()
	spy.Coordinator.Validate = failingValidator{result: validate.Result{Error: &validate.CommandError{
		Stage: validate.ConfigValidate, Summary: "config validate: invalid model",
	}}}
	out := spy.Coordinator.Reconcile(context.Background(), true, true)
	if out.PendingCount() != 1 || out.Targets[0].StagingRoot == "" {
		t.Fatalf("out=%+v", out)
	}
	if _, err := os.Stat(out.Targets[0].StagingRoot); err != nil {
		t.Fatalf("retained staging root=%q: %v", out.Targets[0].StagingRoot, err)
	}
	_ = os.RemoveAll(out.Targets[0].StagingRoot)
}

func TestDryRunPendingValidationCarriesAttemptTimestamp(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	when := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	spy.Coordinator.Clock = fixedClock{t: when}
	spy.Coordinator.Validate = failingValidator{result: validate.Result{Error: &validate.CommandError{
		Stage: validate.ConfigValidate, Summary: "config validate: invalid model",
	}}}

	out := spy.Coordinator.Reconcile(context.Background(), true, false)
	if out.PendingCount() != 1 || !out.Targets[0].Pending.AttemptedAt.Equal(when) {
		t.Fatalf("out=%+v", out)
	}
	if spy.StateSaves != 0 || spy.Publishes != 0 {
		t.Fatalf("dry-run mutated state: saves=%d publishes=%d", spy.StateSaves, spy.Publishes)
	}
}

func TestInitTargetResolutionFailureDoesNotCreateDesired(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.resolveErr = errors.New("resolve targets failed")
	spy.Coordinator.Sources = testSourceReader{}
	out := spy.Coordinator.Init(context.Background())
	if out.Accepted || out.Error == nil || spy.files["desired.yaml"] != "" {
		t.Fatalf("out=%+v files=%v", out, spy.files)
	}
}

func TestSyncTargetResolutionFailureDoesNotReplaceDesired(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.resolveErr = errors.New("resolve targets failed")
	spy.Coordinator.Sources = testSourceReader{}
	out := spy.Coordinator.Sync(context.Background(), true)
	if out.Accepted || out.Error == nil || spy.files["desired.yaml"] != "" {
		t.Fatalf("out=%+v files=%v", out, spy.files)
	}
}

func TestInitExistingPolicyIsRejectedCreateOnly(t *testing.T) {
	spy := newCoordinatorSpy("desired-exists")
	before := spy.Snapshot()
	out := spy.Coordinator.Init(context.Background())
	if out.Accepted || !errors.Is(out.Error, policy.ErrDesiredExists) {
		t.Fatalf("out=%+v", out)
	}
	if !reflect.DeepEqual(before, spy.Snapshot()) {
		t.Fatal("rejected init mutated files")
	}
	if !reflect.DeepEqual(spy.Trace, []string{"lock", "load-state", "recover", "desired-exists", "unlock"}) {
		t.Fatalf("trace=%v", spy.Trace)
	}
	if !strings.Contains(out.Error.Error(), "use sync --from-polytoken") {
		t.Fatalf("error=%v", out.Error)
	}
}

func TestSetAndClearUseSingleLockedTransactionCycle(t *testing.T) {
	low := state.QuotaLow
	cases := []struct {
		name       string
		call       func(*Coordinator) Outcome
		transition string
	}{
		{"set", func(c *Coordinator) Outcome {
			return c.Set(context.Background(), "codex", state.ProviderPatch{Quota: &low})
		}, "state-set"},
		{"clear-all", func(c *Coordinator) Outcome { return c.Clear(context.Background(), state.Selector{All: true}) }, "state-clear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			out := tc.call(&spy.Coordinator)
			want := []string{"lock", "load-state", "recover", "load-policy", "load-state", tc.transition, "reconcile", "publish-targets", "save-state", "unlock"}
			if !reflect.DeepEqual(spy.Trace, want) {
				t.Fatalf("trace=%v want=%v", spy.Trace, want)
			}
			if !out.Accepted || count(spy.Trace, "lock") != 1 || count(spy.Trace, "recover") != 1 || count(spy.Trace, "save-state") != 1 {
				t.Fatalf("out=%+v trace=%v", out, spy.Trace)
			}
		})
	}
}

func TestManualProviderCommandsUseSingleLockedTransactionCycle(t *testing.T) {
	cases := []struct {
		name       string
		call       func(*Coordinator) Outcome
		transition string
	}{
		{"disable", func(c *Coordinator) Outcome { return c.Disable(context.Background(), "codex") }, "manual-disable"},
		{"enable", func(c *Coordinator) Outcome { return c.Enable(context.Background(), "codex") }, "manual-enable"},
		{"reset", func(c *Coordinator) Outcome { return c.Reset(context.Background()) }, "manual-reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			out := tc.call(&spy.Coordinator)
			want := []string{"lock", "load-state", "recover", "load-policy", "load-state", tc.transition, "reconcile", "publish-targets", "save-state", "unlock"}
			if !reflect.DeepEqual(spy.Trace, want) {
				t.Fatalf("trace=%v want=%v", spy.Trace, want)
			}
			if !out.Accepted || spy.StateSaves != 1 || spy.Publishes != 1 {
				t.Fatalf("out=%+v saves=%d publishes=%d", out, spy.StateSaves, spy.Publishes)
			}
		})
	}
}

func TestManualProviderCommandsRejectUnknownProviderWithoutMutation(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	out := spy.Coordinator.Disable(context.Background(), "unknown")
	if out.Accepted || out.Error == nil || spy.StateSaves != 0 || spy.Publishes != 0 || count(spy.Trace, "reconcile") != 0 {
		t.Fatalf("out=%+v trace=%v saves=%d publishes=%d", out, spy.Trace, spy.StateSaves, spy.Publishes)
	}
}

func TestManualDisablePersistsWhenTargetApplicationIsPending(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", "invalid")
	out := spy.Coordinator.Disable(context.Background(), "codex")
	if !out.Accepted || out.PendingCount() != 1 || spy.StateSaves != 1 {
		t.Fatalf("out=%+v saves=%d", out, spy.StateSaves)
	}
	if out.Targets[0].Pending == nil || out.Targets[0].Pending.Stage != "doctor" {
		t.Fatalf("pending=%+v", out.Targets[0].Pending)
	}
}

func TestManualDisablePersistsWhenTargetResolutionFails(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.resolveErr = errors.New("target resolution failed")
	out := spy.Coordinator.Disable(context.Background(), "codex")
	if !out.Accepted || out.Error == nil || out.PendingCount() != 1 || spy.StateSaves != 1 {
		t.Fatalf("out=%+v saves=%d", out, spy.StateSaves)
	}
	if !spy.LastSaved.Providers["codex"].ManualDisabled {
		t.Fatalf("saved state lost manual disable: %+v", spy.LastSaved)
	}
	pending := spy.LastSaved.Targets["manual-resolution"]
	if pending.Pending == nil || pending.Pending.Stage != "resolve_targets" {
		t.Fatalf("saved pending=%+v", pending)
	}
}

// TestAllMutatorsUseSingleTransactSeam proves Init, HandleEvent, Reconcile, Sync,
// Set, and Clear each acquire exactly one lock and release it exactly once,
// confirming they share the one Coordinator.transact path.
func TestAllMutatorsUseSingleTransactSeam(t *testing.T) {
	low := state.QuotaLow
	ctx := context.Background()
	cases := []struct {
		name string
		call func(*Coordinator) Outcome
	}{
		{"init", func(c *Coordinator) Outcome { return c.Init(ctx) }},
		{"event", func(c *Coordinator) Outcome { return c.HandleEvent(ctx, event(hook.QuotaLow, 3)) }},
		{"reconcile", func(c *Coordinator) Outcome { return c.Reconcile(ctx, false, false) }},
		{"sync", func(c *Coordinator) Outcome { return c.Sync(ctx, true) }},
		{"set", func(c *Coordinator) Outcome { return c.Set(ctx, "codex", state.ProviderPatch{Quota: &low}) }},
		{"clear", func(c *Coordinator) Outcome { return c.Clear(ctx, state.Selector{All: true}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			// init/sync need no source reader for the accepted path here because
			// the spy's desired does not pre-exist and Sources is nil; both still
			// must acquire and release exactly one lock before any early return.
			tc.call(&spy.Coordinator)
			if got := count(spy.Trace, "lock"); got != 1 {
				t.Fatalf("%s: lock count=%d want 1 (trace=%v)", tc.name, got, spy.Trace)
			}
			if got := count(spy.Trace, "unlock"); got != 1 {
				t.Fatalf("%s: unlock count=%d want 1 (trace=%v)", tc.name, got, spy.Trace)
			}
			if got := count(spy.Trace, "recover"); got != 1 {
				t.Fatalf("%s: recover count=%d want 1 (trace=%v)", tc.name, got, spy.Trace)
			}
		})
	}
}

// BenchmarkHookReconcile measures the Coordinator's per-hook transaction cost
// (lock, recover, accept, reconcile, stage, validate, publish, commit) with a
// single registered target, using the in-process spy so the benchmark isolates
// orchestration overhead from real filesystem and Polytoken subprocess latency.
func BenchmarkHookReconcile(b *testing.B) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.Coordinator.tracer = nil // no trace recording during the benchmark
	ev := event(hook.QuotaLow, 1)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := spy.Coordinator.HandleEvent(context.Background(), ev)
		if !out.Accepted {
			b.Fatalf("not accepted: %+v", out)
		}
	}
}
