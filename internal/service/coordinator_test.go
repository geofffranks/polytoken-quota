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
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

func TestBuildTransactionPublishesOnlyPreparedChanges(t *testing.T) {
	root := t.TempDir()
	publishDir := t.TempDir()
	changedLive := filepath.Join(root, "config.yaml")
	unchangedLive := filepath.Join(root, "subagents", "same.md")
	changedTemp := filepath.Join(publishDir, "config.yaml")
	unchangedTemp := filepath.Join(publishDir, "subagents", "same.md")
	mustWrite(t, changedLive, "old-config")
	mustWrite(t, unchangedLive, "same-definition")
	mustWrite(t, changedTemp, "new-config")
	mustWrite(t, unchangedTemp, "same-definition")

	plan := reconcile.Plan{Edits: []reconcile.FieldEdit{
		{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("codex/gpt")},
		{File: "subagents/same.md", Path: []string{"polytoken", "model"}, Scalar: strPtr("codex/gpt")},
	}}
	prepValue, err := BuildPrepareResult("global", plan, root, publishDir)
	if err != nil {
		t.Fatalf("BuildPrepareResult: %v", err)
	}
	prep := &prepValue
	rt := RegisteredTarget{
		Policy:   policy.Target{ID: "global", Global: true},
		Resolved: target.Resolved{ID: "global", CanonicalRoot: root, Global: true},
	}
	candidate := staging.Candidate{PublishDir: publishDir}

	tx, err := (&Coordinator{}).buildTransaction(
		state.State{Revision: 1}, state.State{Revision: 2}, rt, plan, candidate, prep,
	)
	if err != nil {
		t.Fatalf("buildTransaction: %v", err)
	}
	if len(tx.Replacements) != 1 {
		t.Fatalf("got %d replacements, want 1: %+v", len(tx.Replacements), tx.Replacements)
	}
	if tx.Replacements[0].LivePath != changedLive {
		t.Fatalf("published replacement = %q, want changed file %q", tx.Replacements[0].LivePath, changedLive)
	}
}

const validTargetKey = "valid"

type testSourceReader struct{}

type missingPolicyLoader struct{}

func (missingPolicyLoader) LoadPolicy() (policy.Desired, error) {
	return policy.Desired{}, fs.ErrNotExist
}
func (missingPolicyLoader) DesiredExists() bool { return false }

func (testSourceReader) Global(context.Context) (policy.SourceSet, error) {
	return policy.SourceSet{ID: "global", Global: true, Config: policy.SourceConfig{Providers: []policy.SourceMapping{{ID: "codex", Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}}}}, Definitions: []policy.SourceDefinition{{Path: "model.md", Model: "codex/gpt"}}}, nil
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
	desired           policy.Desired
	recovered         state.State
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
	if s.desired.Providers != nil {
		return s.desired, nil
	}
	return policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex-mapping": {
			Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
		},
	}}, nil
}
func (s *coordinatorSpy) DesiredExists() bool { return s.desiredExists }

// --- policy.Writer ---
func (s *coordinatorSpy) CreateAtomic(context.Context, policy.Desired) (policy.PublicationResult, error) {
	if s.desiredExists {
		return policy.PublicationResult{}, policy.ErrDesiredExists
	}
	s.files["desired.yaml"] = "created"
	return policy.PublicationResult{Committed: true}, nil
}
func (s *coordinatorSpy) ReplaceAtomic(context.Context, policy.Desired) (policy.PublicationResult, error) {
	s.files["desired.yaml"] = "replaced"
	return policy.PublicationResult{Committed: true}, nil
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
func (s *coordinatorSpy) Stage(_ context.Context, res target.Resolved, plan reconcile.Plan, _ *reconcile.Plan) (staging.Candidate, error) {
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
	if s.recovered.Providers != nil {
		return s.recovered, nil
	}
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

func TestDryRunIsNonMutating(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	out := spy.Coordinator.Reconcile(context.Background(), true, false, false)
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

	out := spy.Coordinator.Reconcile(context.Background(), false, false, false)
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
	out := spy.Coordinator.Reconcile(context.Background(), true, true, false)
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
	out := spy.Coordinator.Reconcile(context.Background(), true, true, false)
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

	out := spy.Coordinator.Reconcile(context.Background(), true, false, false)
	if out.PendingCount() != 1 || !out.Targets[0].Pending.AttemptedAt.Equal(when) {
		t.Fatalf("out=%+v", out)
	}
	if spy.StateSaves != 0 || spy.Publishes != 0 {
		t.Fatalf("dry-run mutated state: saves=%d publishes=%d", spy.StateSaves, spy.Publishes)
	}
}

func TestInitTargetResolutionFailureDoesNotCreateDesired(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.Coordinator.Policy = missingPolicyLoader{}
	spy.resolveErr = errors.New("resolve targets failed")
	spy.Coordinator.Sources = testSourceReader{}
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{})
	if out.Accepted || out.Error == nil || spy.files["desired.yaml"] != "" {
		t.Fatalf("out=%+v files=%v", out, spy.files)
	}
}

func TestForcedInitTargetResolutionFailureDoesNotReplaceDesired(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.resolveErr = errors.New("resolve targets failed")
	spy.Coordinator.Sources = testSourceReader{}
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{Force: true})
	if out.Accepted || out.Error == nil || spy.files["desired.yaml"] != "" {
		t.Fatalf("out=%+v files=%v", out, spy.files)
	}
}

func TestInitExistingPolicyIsRejectedCreateOnly(t *testing.T) {
	spy := newCoordinatorSpy("desired-exists")
	before := spy.Snapshot()
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{})
	if out.Accepted || !errors.Is(out.Error, policy.ErrDesiredExists) {
		t.Fatalf("out=%+v", out)
	}
	if !reflect.DeepEqual(before, spy.Snapshot()) {
		t.Fatal("rejected init mutated files")
	}
	if !reflect.DeepEqual(spy.Trace, []string{"lock", "desired-exists", "unlock"}) {
		t.Fatalf("trace=%v", spy.Trace)
	}
	if !strings.Contains(out.Error.Error(), "use polytoken-quota init --force") {
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
			if tc.name == "clear-all" {
				if !out.Accepted || !out.HandledWithoutRevision || spy.StateSaves != 0 || spy.Publishes != 0 {
					t.Fatalf("clear no-op out=%+v saves=%d publishes=%d", out, spy.StateSaves, spy.Publishes)
				}
				return
			}
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
		{"disable", func(c *Coordinator) Outcome { return c.Disable(context.Background(), "codex-mapping") }, "manual-disable"},
		{"enable", func(c *Coordinator) Outcome { return c.Enable(context.Background(), "codex-mapping") }, "manual-enable"},
		{"reset", func(c *Coordinator) Outcome { return c.Reset(context.Background()) }, "manual-reset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			out := tc.call(&spy.Coordinator)
			if tc.name == "enable" || tc.name == "reset" {
				if !out.Accepted || !out.HandledWithoutRevision || spy.StateSaves != 0 || spy.Publishes != 0 {
					t.Fatalf("manual no-op out=%+v saves=%d publishes=%d", out, spy.StateSaves, spy.Publishes)
				}
				return
			}
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

func TestRoutingMappingControlsMappingID(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.desired = policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex-pool": {
			Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
		},
	}}

	out := spy.Coordinator.Disable(context.Background(), "codex-pool")
	if !out.Accepted || spy.StateSaves != 1 || spy.Publishes != 1 || count(spy.Trace, "lock") != 1 || count(spy.Trace, "reconcile") != 1 {
		t.Fatalf("out=%+v trace=%v saves=%d publishes=%d", out, spy.Trace, spy.StateSaves, spy.Publishes)
	}
	if !spy.LastSaved.Providers["codex-pool"].ManualDisabled {
		t.Fatalf("mapping ID not disabled in saved state: %+v", spy.LastSaved)
	}
}

func TestRoutingEnablePreservesAutomaticExclusion(t *testing.T) {
	observedAt := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.desired = policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex-pool": {Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}},
	}}
	spy.recovered = state.State{Revision: 7, Providers: map[string]state.ProviderState{
		"codex-pool": {Quota: state.QuotaExhausted, Availability: state.Available, ManualDisabled: true, QuotaAt: observedAt, QuotaArrival: 11},
	}, Targets: map[string]state.TargetState{}}

	out := spy.Coordinator.Enable(context.Background(), "codex-pool")
	if !out.Accepted {
		t.Fatalf("out=%+v", out)
	}
	provider := spy.LastSaved.Providers["codex-pool"]
	if provider.ManualDisabled || provider.Quota != state.QuotaExhausted || !provider.QuotaAt.Equal(observedAt) || provider.QuotaArrival != 11 {
		t.Fatalf("automatic exclusion changed: %+v", provider)
	}
	if state.EffectiveMode(provider) != state.ModeDisabled {
		t.Fatalf("enable incorrectly restored eligibility: mode=%s", state.EffectiveMode(provider))
	}
}

func TestRoutingResetPreservesAutomaticState(t *testing.T) {
	observedAt := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.recovered = state.State{Revision: 4, Providers: map[string]state.ProviderState{
		"codex-primary": {Quota: state.QuotaExhausted, Availability: state.Unavailable, ManualDisabled: true, QuotaAt: observedAt, AvailabilityAt: observedAt, QuotaArrival: 3, AvailabilityArrival: 4},
		"zai":           {Quota: state.QuotaLow, Availability: state.Available, ManualDisabled: true, QuotaAt: observedAt, QuotaArrival: 5},
	}, Targets: map[string]state.TargetState{}}

	out := spy.Coordinator.Reset(context.Background())
	if !out.Accepted || spy.StateSaves != 1 || spy.Publishes != 1 {
		t.Fatalf("out=%+v saves=%d publishes=%d", out, spy.StateSaves, spy.Publishes)
	}
	codex := spy.LastSaved.Providers["codex-primary"]
	zai := spy.LastSaved.Providers["zai"]
	if codex.ManualDisabled || codex.Quota != state.QuotaExhausted || codex.Availability != state.Unavailable || !codex.QuotaAt.Equal(observedAt) || !codex.AvailabilityAt.Equal(observedAt) || codex.QuotaArrival != 3 || codex.AvailabilityArrival != 4 {
		t.Fatalf("codex automatic state changed: %+v", codex)
	}
	if zai.ManualDisabled || zai.Quota != state.QuotaLow || zai.Availability != state.Available || !zai.QuotaAt.Equal(observedAt) || zai.QuotaArrival != 5 {
		t.Fatalf("zai automatic state changed: %+v", zai)
	}
}

func TestRoutingControlRejectsNonMappingIDs(t *testing.T) {
	for _, identity := range []string{"codex-primary", "polytoken-codex", "codex/gpt"} {
		t.Run(identity, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			spy.desired = policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
				"codex-pool": {
					Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
				},
			}}
			before := state.State{Revision: 9, Providers: map[string]state.ProviderState{"codex-primary": {Quota: state.QuotaLow, Availability: state.Available}}, Targets: map[string]state.TargetState{}}
			spy.recovered = before

			out := spy.Coordinator.Disable(context.Background(), identity)
			if out.Accepted || out.Error == nil || spy.StateSaves != 0 || spy.Publishes != 0 || count(spy.Trace, "reconcile") != 0 {
				t.Fatalf("identity=%q out=%+v trace=%v saves=%d publishes=%d", identity, out, spy.Trace, spy.StateSaves, spy.Publishes)
			}
			if !reflect.DeepEqual(spy.recovered, before) {
				t.Fatalf("rejected identity mutated recovered state: before=%+v after=%+v", before, spy.recovered)
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
	out := spy.Coordinator.Disable(context.Background(), "codex-mapping")
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
	out := spy.Coordinator.Disable(context.Background(), "codex-mapping")
	if !out.Accepted || out.Error == nil || out.PendingCount() != 1 || spy.StateSaves != 1 {
		t.Fatalf("out=%+v saves=%d", out, spy.StateSaves)
	}
	if !spy.LastSaved.Providers["codex-mapping"].ManualDisabled {
		t.Fatalf("saved state lost manual disable: %+v", spy.LastSaved)
	}
	pending := spy.LastSaved.Targets["manual-resolution"]
	if pending.Pending == nil || pending.Pending.Stage != "resolve_targets" {
		t.Fatalf("saved pending=%+v", pending)
	}
}

// TestAllMutatorsUseSingleTransactSeam proves Init, Reconcile, Set, and Clear
// each acquire exactly one lock and release it exactly once,
// confirming they share the one Coordinator.transact path.
func TestAllMutatorsUseSingleTransactSeam(t *testing.T) {
	low := state.QuotaLow
	ctx := context.Background()
	cases := []struct {
		name string
		call func(*Coordinator) Outcome
	}{
		{"init", func(c *Coordinator) Outcome { return c.InitWithOptions(ctx, InitOptions{Force: true}) }},
		{"reconcile", func(c *Coordinator) Outcome { return c.Reconcile(ctx, false, false, false) }},
		{"set", func(c *Coordinator) Outcome { return c.Set(ctx, "codex", state.ProviderPatch{Quota: &low}) }},
		{"clear", func(c *Coordinator) Outcome { return c.Clear(ctx, state.Selector{All: true}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCoordinatorSpy().withTargets("global", validTargetKey)
			// init needs no source reader for the accepted path here because
			// the spy's desired does not pre-exist and Sources is nil; it still
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

// TestInitForceMatrixWithRecoverableJournal proves init classifies desired.yaml
// under the lock before touching state or recovering a real interrupted publish.
func TestInitForceMatrixWithRecoverableJournal(t *testing.T) {
	cases := []struct {
		name       string
		desired    []byte
		loadErr    error
		force      bool
		accepted   bool
		wantExists bool
	}{
		{name: "absent-plain", accepted: true, wantExists: true},
		{name: "valid-existing-plain", desired: validInitDesired(), accepted: false, wantExists: true},
		{name: "valid-existing-force", desired: validInitDesired(), force: true, accepted: true, wantExists: true},
		{name: "malformed-force", desired: []byte("version: [malformed\n"), force: true, accepted: false, wantExists: true},
		{name: "unreadable-force", desired: validInitDesired(), loadErr: os.ErrPermission, force: true, accepted: false, wantExists: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRecoverableInitFixture(t, tc.desired, tc.loadErr)
			before := snapshotInitFiles(t, fx.paths...)
			out := fx.coord.InitWithOptions(context.Background(), InitOptions{Force: tc.force})
			if out.Accepted != tc.accepted {
				t.Fatalf("out=%+v want accepted=%v", out, tc.accepted)
			}
			if tc.accepted {
				if out.Error != nil || fx.publisher.recovers != 1 || fx.state.loads != 1 {
					t.Fatalf("out=%+v recovers=%d loads=%d", out, fx.publisher.recovers, fx.state.loads)
				}
			} else {
				if out.Error == nil || fx.publisher.recovers != 0 || fx.state.loads != 0 {
					t.Fatalf("out=%+v recovers=%d loads=%d", out, fx.publisher.recovers, fx.state.loads)
				}
				after := snapshotInitFiles(t, fx.paths...)
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected init mutated durable bytes:\nbefore=%q\nafter =%q", before, after)
				}
			}
			_, statErr := os.Stat(fx.desiredPath)
			if (statErr == nil) != tc.wantExists {
				t.Fatalf("desired existence err=%v wantExists=%v", statErr, tc.wantExists)
			}
		})
	}
}

// TestInitCreatePostCommitFailureReportsAccepted proves a cleanup/durability
// warning after the create link boundary cannot be reported as a rollback.
func TestInitCreatePostCommitFailureReportsAccepted(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.Coordinator.Policy = missingPolicyLoader{}
	spy.Coordinator.Sources = testSourceReader{}
	warning := errors.New("injected post-create cleanup warning")
	spy.Coordinator.PolicyWriter = postCommitPolicyWriter{createWarning: warning}
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{})
	if !out.Accepted || !errors.Is(out.Error, warning) {
		t.Fatalf("out=%+v want accepted warning", out)
	}
}

// TestInitReplacePostCommitFailureReportsAccepted proves a cleanup/durability
// warning after rename cannot be reported as a rollback.
func TestInitReplacePostCommitFailureReportsAccepted(t *testing.T) {
	spy := newCoordinatorSpy("desired-exists")
	spy.Coordinator.Sources = testSourceReader{}
	warning := errors.New("injected post-replace cleanup warning")
	spy.Coordinator.PolicyWriter = postCommitPolicyWriter{replaceWarning: warning}
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{Force: true})
	if !out.Accepted || !errors.Is(out.Error, warning) {
		t.Fatalf("out=%+v want accepted warning", out)
	}
}

type postCommitPolicyWriter struct {
	createWarning  error
	replaceWarning error
}

func (w postCommitPolicyWriter) CreateAtomic(context.Context, policy.Desired) (policy.PublicationResult, error) {
	return policy.PublicationResult{Committed: true, Warning: w.createWarning}, nil
}
func (w postCommitPolicyWriter) ReplaceAtomic(context.Context, policy.Desired) (policy.PublicationResult, error) {
	return policy.PublicationResult{Committed: true, Warning: w.replaceWarning}, nil
}

type initPolicyLoader struct {
	FilePolicyLoader
	err error
}

func (l initPolicyLoader) LoadPolicy() (policy.Desired, error) {
	if l.err != nil {
		return policy.Desired{}, l.err
	}
	return l.FilePolicyLoader.LoadPolicy()
}

type countingStateStore struct {
	StoreState
	loads int
}

func (s *countingStateStore) LoadState() (state.State, error) {
	s.loads++
	return s.StoreState.LoadState()
}

type countingPublisher struct {
	PublisherAdapter
	recovers int
}

func (p *countingPublisher) Recover(ctx context.Context, prior state.State) (state.State, error) {
	p.recovers++
	return p.PublisherAdapter.Recover(ctx, prior)
}

type recoverableInitFixture struct {
	coord       *Coordinator
	state       *countingStateStore
	publisher   *countingPublisher
	desiredPath string
	paths       []string
}

func newRecoverableInitFixture(t *testing.T, desired []byte, loadErr error) recoverableInitFixture {
	t.Helper()
	root := t.TempDir()
	desiredPath := filepath.Join(root, "desired.yaml")
	statePath := filepath.Join(root, "state.json")
	liveDir := filepath.Join(root, "live")
	livePath := filepath.Join(liveDir, "managed.md")
	tempPath := filepath.Join(liveDir, ".candidate-managed.md")
	journalPath := filepath.Join(root, "journal", "apply.json")
	if desired != nil {
		if err := os.WriteFile(desiredPath, desired, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldLive := []byte("exact old live bytes\n")
	newLive := []byte("interrupted candidate bytes\n")
	if err := os.WriteFile(livePath, oldLive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, newLive, 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Path: statePath}
	prior := state.State{Schema: 1, Revision: 7, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	next := prior
	next.Revision = 8
	pub := publish.Publisher{
		State: store, FS: publish.OSFS{}, JournalPath: journalPath,
		Backups:     publish.BackupStore{Root: filepath.Join(root, "backups"), Limit: 3},
		ManagedRoot: liveDir,
		Fault: func(step string) error {
			if step == "rename" {
				return errors.New("seed interrupted publish")
			}
			return nil
		},
	}
	_, err := pub.ApplyUnderLock(context.Background(), publish.Transaction{
		Prior: prior, Next: next, TargetID: "global", ManagedRoot: liveDir,
		Replacements: []publish.Replacement{{
			LivePath: livePath, TempPath: tempPath,
			OldHash: sha256.Sum256(oldLive), NewHash: sha256.Sum256(newLive), Mode: 0o600,
		}},
	})
	if err == nil {
		t.Fatal("seed publish unexpectedly completed")
	}
	pub.Fault = nil

	spy := newCoordinatorSpy()
	loader := initPolicyLoader{FilePolicyLoader: FilePolicyLoader{Path: desiredPath}, err: loadErr}
	countState := &countingStateStore{StoreState: StoreState{Store: store}}
	countPub := &countingPublisher{PublisherAdapter: PublisherAdapter{Publisher: pub}}
	spy.Coordinator.Lock = publish.NewFileLock(filepath.Join(root, "apply.lock"))
	spy.Coordinator.Policy = loader
	spy.Coordinator.PolicyWriter = policy.NewWriter(desiredPath)
	spy.Coordinator.State = countState
	spy.Coordinator.Publish = countPub
	spy.Coordinator.Sources = testSourceReader{}
	return recoverableInitFixture{
		coord: &spy.Coordinator, state: countState, publisher: countPub, desiredPath: desiredPath,
		paths: []string{desiredPath, statePath, livePath, journalPath},
	}
}

func validInitDesired() []byte {
	return []byte("version: 1\nproviders:\n  codex:\n    models: [codex/gpt]\n")
}

func snapshotInitFiles(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			out[path] = nil
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[path] = bytes.Clone(data)
	}
	return out
}
