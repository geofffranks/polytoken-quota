package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/groupsafety"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

type preflightLocker struct{}

func (preflightLocker) Lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

type preflightPolicy struct{ desired policy.Desired }

func (p preflightPolicy) LoadPolicy() (policy.Desired, error) { return p.desired, nil }
func (preflightPolicy) DesiredExists() bool                   { return true }

type preflightState struct{ saves int }

func (*preflightState) LoadState() (state.State, error) { return state.State{Revision: 4}, nil }
func (s *preflightState) Save(state.State) error        { s.saves++; return nil }

type preflightTargets []RegisteredTarget

func (r preflightTargets) ResolveTargets(policy.Desired) ([]RegisteredTarget, error) { return r, nil }

type preflightStager struct {
	plans   []reconcile.Plan
	globals []*reconcile.Plan
	failAt  string
}

func (s *preflightStager) Stage(_ context.Context, res target.Resolved, plan reconcile.Plan, global *reconcile.Plan) (staging.Candidate, error) {
	s.plans = append(s.plans, plan)
	s.globals = append(s.globals, global)
	return staging.Candidate{TargetID: res.ID}, nil
}

type preflightValidator struct {
	calls      []string
	failTarget string
}

func (v *preflightValidator) Validate(_ context.Context, c staging.Candidate, _ time.Duration) validate.Result {
	v.calls = append(v.calls, c.TargetID)
	if c.TargetID == v.failTarget {
		return validate.Result{Error: &validate.CommandError{Stage: validate.Doctor}}
	}
	return validate.Result{ConfigValid: true, StartupValid: true}
}

func preflightFixture(t *testing.T, projectCount int) (*Coordinator, *preflightState, *preflightStager, *preflightValidator, []string) {
	t.Helper()
	base := t.TempDir()
	globalRoot := filepath.Join(base, "global")
	if err := os.MkdirAll(globalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `version: 4
providers:
  gp:
    kind: {type: custom_open_ai_compatible}
    url: http://127.0.0.1:9
    auth: {type: no_auth}
    enabled: true
  pp:
    kind: {type: custom_open_ai_compatible}
    url: http://127.0.0.1:9
    auth: {type: no_auth}
    enabled: true
models:
  gp/g1: {provider: gp, enabled: true}
  pp/p1: {provider: pp, enabled: true}
modelgroups:
  failover: [gp/g1, pp/p1]
  polytoken:default_model_full: pp/p1
`
	if err := os.WriteFile(filepath.Join(globalRoot, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	desired := policy.Desired{Version: 1, Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{"gp": {}, "pp": {}}, Global: policy.Target{ID: "global", Root: globalRoot, Global: true}}
	registered := []RegisteredTarget{{Policy: desired.Global, Resolved: target.Resolved{ID: "global", CanonicalRoot: globalRoot, Global: true}}}
	paths := []string{filepath.Join(globalRoot, "config.yaml")}
	for i := 0; i < projectCount; i++ {
		root := filepath.Join(base, "project-"+string(rune('a'+i)))
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		projectConfig := "version: 4\nmodelgroups:\n  failover: [gp/g1, pp/p1]\n"
		if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(projectConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		id := "project-" + string(rune('a'+i))
		desired.Projects = append(desired.Projects, policy.Target{ID: id, Root: root})
		registered = append(registered, RegisteredTarget{Policy: desired.Projects[i], Resolved: target.Resolved{ID: id, CanonicalRoot: root}})
		paths = append(paths, filepath.Join(root, "config.yaml"))
	}
	store := &preflightState{}
	stager := &preflightStager{}
	validator := &preflightValidator{}
	coord := &Coordinator{Lock: preflightLocker{}, Policy: preflightPolicy{desired}, State: store, Targets: preflightTargets(registered), Stage: stager, Validate: validator}
	return coord, store, stager, validator, paths
}

func TestProviderPreflightLaterProjectFailureBlocksReadyAndWritesNothing(t *testing.T) {
	coord, store, stager, validator, sources := preflightFixture(t, 2)
	validator.failTarget = "project-b"
	// gp remains eligible through the global-only group, while the project
	// candidate is independently made to fail by the injected validator.
	before := make([][]byte, len(sources))
	for i, path := range sources {
		before[i], _ = os.ReadFile(path)
	}
	result, err := coord.PreflightProviderDisables(context.Background(), []string{"gp"})
	if err == nil || result.Ready {
		t.Fatalf("result=%+v err=%v; want blocked, not ready", result, err)
	}
	if len(validator.calls) != 3 {
		t.Fatalf("validated %v, want global and both projects; preflight error: %v", validator.calls, err)
	}
	if store.saves != 0 {
		t.Fatalf("state saves=%d, want none", store.saves)
	}
	if len(stager.plans) != 3 || len(stager.plans[0].Edits) != 1 {
		t.Fatalf("plans=%+v", stager.plans)
	}
	edit := stager.plans[0].Edits[0]
	if !reflect.DeepEqual(edit.Path, []string{"providers", "gp", "enabled"}) || edit.Enabled == nil || *edit.Enabled {
		t.Fatalf("edit=%+v", edit)
	}
	if stager.globals[1] == nil || len(stager.globals[1].Edits) != 1 {
		t.Fatalf("project global plan=%+v", stager.globals[1])
	}
	for i, path := range sources {
		after, _ := os.ReadFile(path)
		if !reflect.DeepEqual(after, before[i]) {
			t.Fatalf("source %s changed", path)
		}
	}
}

func TestProviderPreflightUnsafeProviderInCombinedSetBlocks(t *testing.T) {
	coord, _, stager, _, _ := preflightFixture(t, 0)
	result, err := coord.PreflightProviderDisables(context.Background(), []string{"gp", "pp"})
	if err == nil || result.Ready {
		t.Fatalf("result=%+v err=%v; expected unsafe combined proposal", result, err)
	}
	if report := result.Reports["pp"]; report.Verdict != "unsafe" {
		t.Fatalf("pp report=%+v", report)
	}
	if len(stager.plans) != 0 {
		t.Fatalf("staged unsafe proposal: %+v", stager.plans)
	}
}

func TestProviderPreflightRecheckDetectsStaleSource(t *testing.T) {
	coord, _, _, _, sources := preflightFixture(t, 0)
	result, err := coord.PreflightProviderDisables(context.Background(), []string{"gp"})
	if err != nil || !result.Ready {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := os.WriteFile(sources[0], []byte("version: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecheckProviderPreflight(result); err == nil {
		t.Fatal("stale source snapshot was accepted")
	}
}

func TestProviderPreflightRejectsEmptyProposal(t *testing.T) {
	coord, _, _, _, _ := preflightFixture(t, 0)
	_, err := coord.PreflightProviderDisables(context.Background(), nil)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want empty proposal rejection", err)
	}
}

// preflightRootConfig is the global layer of the production-load fixture: two
// enrolled providers with concrete enabled models, a composed group, and the
// full tier default.
const preflightRootConfig = `version: 4
providers:
  gp:
    kind: {type: custom_open_ai_compatible}
    url: http://127.0.0.1:9
    auth: {type: no_auth}
    enabled: true
  pp:
    kind: {type: custom_open_ai_compatible}
    url: http://127.0.0.1:9
    auth: {type: no_auth}
    enabled: true
models:
  gp/g1: {provider: gp, enabled: true}
  pp/p1: {provider: pp, enabled: true}
modelgroups:
  failover: [gp/g1, pp/p1]
  polytoken:default_model_full: pp/p1
`

// writePreflightRoot creates a registered root directory holding one config.yaml.
func writePreflightRoot(t *testing.T, root, config string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// writePreflightPolicy writes a provider-only desired.yaml registering the
// given project roots and returns its path.
func writePreflightPolicy(t *testing.T, globalRoot string, projectRoots map[string]string) string {
	t.Helper()
	doc := `version: 1
mode: provider-only
providers:
  gp: {}
  pp: {}
global:
  id: global
  root: ` + globalRoot + `
projects:
`
	ids := make([]string, 0, len(projectRoots))
	for id := range projectRoots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		doc += "  - id: " + id + "\n    root: " + projectRoots[id] + "\n"
	}
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProviderPreflightProductionPolicyLoadAssessesEveryRegisteredRoot proves
// the preflight runs on a policy loaded by the production loader from a real
// desired.yaml with registered project roots, resolves them through the real
// target registry, assesses the global layer plus each project layer, stages
// every registered root for validation, and adopts nothing that is not
// explicitly registered.
func TestProviderPreflightProductionPolicyLoadAssessesEveryRegisteredRoot(t *testing.T) {
	base := t.TempDir()
	globalRoot := writePreflightRoot(t, filepath.Join(base, "global"), preflightRootConfig)
	projectConfig := "version: 4\nmodelgroups:\n  failover: [gp/g1, pp/p1]\n"
	rootA := writePreflightRoot(t, filepath.Join(base, "project-a"), projectConfig)
	rootB := writePreflightRoot(t, filepath.Join(base, "project-b"), projectConfig)
	// An unregistered root on disk: the preflight must never adopt it.
	writePreflightRoot(t, filepath.Join(base, "project-c"), projectConfig)
	loader := FilePolicyLoader{Path: writePreflightPolicy(t, globalRoot, map[string]string{
		"project-a": rootA,
		"project-b": rootB,
	})}
	desired, err := loader.LoadPolicy()
	if err != nil {
		t.Fatalf("production policy load: %v", err)
	}
	registered, err := realTargetRegistry{}.ResolveTargets(desired)
	if err != nil {
		t.Fatalf("resolve registered targets: %v", err)
	}
	store := &preflightState{}
	stager := &preflightStager{}
	validator := &preflightValidator{}
	coord := &Coordinator{Lock: preflightLocker{}, Policy: loader, State: store, Targets: preflightTargets(registered), Stage: stager, Validate: validator}

	sources := []string{filepath.Join(globalRoot, "config.yaml"), filepath.Join(rootA, "config.yaml"), filepath.Join(rootB, "config.yaml"), filepath.Join(base, "project-c", "config.yaml")}
	before := make([][]byte, len(sources))
	for i, path := range sources {
		if before[i], err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}

	result, err := coord.PreflightProviderDisables(context.Background(), []string{"gp"})
	if err != nil || !result.Ready {
		t.Fatalf("result=%+v err=%v, want ready preflight", result, err)
	}
	if got := validator.calls; len(got) != 3 {
		t.Fatalf("validated %v, want exactly global, project-a, project-b", got)
	}
	assessed := map[string]bool{}
	for _, snapshot := range result.Snapshots {
		assessed[snapshot.TargetID] = true
	}
	for _, want := range []string{"global", "project-a", "project-b"} {
		if !assessed[want] {
			t.Fatalf("registered root %s was not snapshotted: %+v", want, result.Snapshots)
		}
	}
	if assessed["project-c"] || len(assessed) != 3 {
		t.Fatalf("preflight adopted unregistered roots: %+v", assessed)
	}
	if len(stager.plans) != 3 || len(stager.plans[0].Edits) != 1 {
		t.Fatalf("staged plans=%+v, want three roots with one global edit", stager.plans)
	}
	edit := stager.plans[0].Edits[0]
	if !reflect.DeepEqual(edit.Path, []string{"providers", "gp", "enabled"}) || edit.Enabled == nil || *edit.Enabled {
		t.Fatalf("global edit=%+v", edit)
	}
	for i := 1; i < len(stager.plans); i++ {
		if len(stager.plans[i].Edits) != 0 {
			t.Fatalf("project plan %d carries edits: %+v", i, stager.plans[i])
		}
		if stager.globals[i] == nil || len(stager.globals[i].Edits) != 1 {
			t.Fatalf("project global plan %d=%+v", i, stager.globals[i])
		}
	}
	if store.saves != 0 {
		t.Fatalf("state saves=%d, want none", store.saves)
	}
	for i, path := range sources {
		after, rerr := os.ReadFile(path)
		if rerr != nil || !reflect.DeepEqual(after, before[i]) {
			t.Fatalf("source %s changed", path)
		}
	}
}

// TestProviderPreflightProjectIsolationBlocksUnsafeProjectAlone proves each
// registered project is assessed independently: a disable the global layer and
// project-a survive is still blocked because project-b's own group would lose
// its only leaf.
func TestProviderPreflightProjectIsolationBlocksUnsafeProjectAlone(t *testing.T) {
	base := t.TempDir()
	globalRoot := writePreflightRoot(t, filepath.Join(base, "global"), preflightRootConfig)
	rootA := writePreflightRoot(t, filepath.Join(base, "project-a"), "version: 4\nmodelgroups:\n  failover: [gp/g1, pp/p1]\n")
	rootB := writePreflightRoot(t, filepath.Join(base, "project-b"), "version: 4\nmodelgroups:\n  sole: [pp/p1]\n")
	loader := FilePolicyLoader{Path: writePreflightPolicy(t, globalRoot, map[string]string{
		"project-a": rootA,
		"project-b": rootB,
	})}
	desired, err := loader.LoadPolicy()
	if err != nil {
		t.Fatalf("production policy load: %v", err)
	}
	registered, err := realTargetRegistry{}.ResolveTargets(desired)
	if err != nil {
		t.Fatalf("resolve registered targets: %v", err)
	}
	store := &preflightState{}
	stager := &preflightStager{}
	validator := &preflightValidator{}
	coord := &Coordinator{Lock: preflightLocker{}, Policy: loader, State: store, Targets: preflightTargets(registered), Stage: stager, Validate: validator}

	sources := []string{filepath.Join(globalRoot, "config.yaml"), filepath.Join(rootA, "config.yaml"), filepath.Join(rootB, "config.yaml")}
	before := make([][]byte, len(sources))
	for i, path := range sources {
		if before[i], err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}

	result, err := coord.PreflightProviderDisables(context.Background(), []string{"pp"})
	if err == nil || result.Ready {
		t.Fatalf("result=%+v err=%v, want blocked preflight", result, err)
	}
	report := result.Reports["pp"]
	if report.Verdict != groupsafety.Unsafe {
		t.Fatalf("pp report=%+v, want unsafe", report)
	}
	// The global layer alone survives the disable (gp/g1 remains), isolating
	// the failure to project-b's sole group.
	if got := report.GroupsAfter["failover"]; len(got) != 1 || got[0] != "gp/g1" {
		t.Fatalf("global failover after disable=%v, want [gp/g1]", got)
	}
	if got := report.GroupsBefore["sole"]; len(got) != 1 || got[0] != "pp/p1" {
		t.Fatalf("project-b sole before disable=%v, want [pp/p1]", got)
	}
	if got := report.GroupsAfter["sole"]; len(got) != 0 {
		t.Fatalf("project-b sole after disable=%v, want no usable leaf", got)
	}
	if len(stager.plans) != 0 {
		t.Fatalf("staged an unsafe proposal: %+v", stager.plans)
	}
	if store.saves != 0 {
		t.Fatalf("state saves=%d, want none", store.saves)
	}
	for i, path := range sources {
		after, rerr := os.ReadFile(path)
		if rerr != nil || !reflect.DeepEqual(after, before[i]) {
			t.Fatalf("source %s changed", path)
		}
	}
}

// TestProviderPreflightRecheckDetectsRemovedSource proves a registered source
// file that disappears after assessment refuses the recheck, like a changed
// one: callers must re-run the preflight.
func TestProviderPreflightRecheckDetectsRemovedSource(t *testing.T) {
	coord, _, _, _, sources := preflightFixture(t, 0)
	result, err := coord.PreflightProviderDisables(context.Background(), []string{"gp"})
	if err != nil || !result.Ready {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := os.Remove(sources[0]); err != nil {
		t.Fatal(err)
	}
	if err := RecheckProviderPreflight(result); err == nil {
		t.Fatal("a removed source snapshot was accepted")
	}
}
