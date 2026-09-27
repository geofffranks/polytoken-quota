package service

// End-to-end fixtures for the legacy modelgroups guard. Each drives a real
// Coordinator (real registry, real reconciler, real staging, fake validation
// binary) against synthetic private roots and proves that a legacy policy's
// tier-default selections are left operator-owned when the composed config
// surface uses modelgroups, while models.*.enabled and definition-file edits
// still publish. Scenario 1: modelgroups on the global layer. Scenario 2:
// modelgroups on a registered project layer only — the staged merge combines
// the layers, so the shared global plan must not carry defaults edits either.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
	"gopkg.in/yaml.v3"
)

func modelGroupsCoord(t *testing.T, desired policy.Desired, globalDir, workRoot string) *Coordinator {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	store := state.Store{Path: filepath.Join(workRoot, "state.json"), Now: clock, RecoveredRetention: 24 * time.Hour}
	pub := publish.Publisher{
		Locker:      publish.NewFileLock(filepath.Join(workRoot, "lock", "apply.lock")),
		State:       store,
		JournalPath: filepath.Join(workRoot, "journal", "apply.json"),
		Backups:     publish.BackupStore{Root: filepath.Join(workRoot, "backups"), Limit: 3},
		ManagedRoot: globalDir,
		Clock:       clock,
	}
	builder := staging.Builder{
		TempRoot: filepath.Join(workRoot, "stage"),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}
	return &Coordinator{
		Lock:         publish.NewFileLock(filepath.Join(workRoot, "lock", "apply.lock")),
		Policy:       fixedPolicyLoader{desired: desired},
		PolicyWriter: nilPolicyWriter{},
		State:        StoreState{Store: store},
		Targets:      NewTargetRegistry(),
		Builder:      NewReconciler(),
		Stage:        StagingStager{Builder: builder},
		Validate:     ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: fakeCommandRunner{}}},
		Publish:      PublisherAdapter{Publisher: pub},
		Clock:        fixedClock{t: clock()},
	}
}

// skipFields returns the sorted skipped-field names reported for one target.
func skipFields(t *testing.T, out TargetOutcome) []string {
	t.Helper()
	got := make([]string, 0, len(out.Skipped))
	for _, sk := range out.Skipped {
		if sk.Reason != "config uses modelgroups" {
			t.Fatalf("skip reason for %s = %q, want the modelgroups reason", sk.Field, sk.Reason)
		}
		got = append(got, sk.Field)
	}
	slices.Sort(got)
	return got
}

// TestReconcileModelGroupsGlobalLayerLeavesDefaultsOperatorOwned is scenario 1:
// a legacy policy with full/mini/nano/classifier chains and a managed subagent,
// over a v4 global config that Polytoken re-saved with modelgroups. The
// reconcile must apply without pending, leave every defaults byte untouched,
// still flip the disabled model to enabled, still manage the subagent, and
// report the skipped tier-default fields.
func TestReconcileModelGroupsGlobalLayerLeavesDefaultsOperatorOwned(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	config := "version: 4\n" +
		"providers:\n" +
		"  codex:\n" +
		"    api_key: inert\n" +
		"  zai:\n" +
		"    api_key: inert\n" +
		"models:\n" +
		"  codex/gpt:\n" +
		"    enabled: true\n" +
		"  zai/glm:\n" +
		"    enabled: false\n" +
		"modelgroups:\n" +
		"  failover:\n" +
		"    - codex/gpt\n" +
		"    - zai/glm\n" +
		"defaults:\n" +
		"  full: codex/gpt\n"
	writeFile(t, filepath.Join(sourceDir, "config.yaml"), config)
	writeFile(t, filepath.Join(sourceDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}},
			"zai":   {Models: map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}}},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   sourceDir,
			Global: true,
			Full:   policy.Chain{"codex/gpt", "zai/glm"},
			Mini:   policy.Chain{"zai/glm"},
			Nano:   policy.Chain{"codex/gpt"},
			Definitions: []policy.Definition{{
				Path:  filepath.Join("subagents", "agent.md"),
				Chain: policy.Chain{"codex/gpt", "zai/glm"},
			}},
		},
	}

	coord := modelGroupsCoord(t, desired, sourceDir, filepath.Join(root, "util"))
	out := coord.Reconcile(context.Background(), false, false, false)
	if !out.Accepted {
		t.Fatalf("reconcile not accepted: %+v err=%v", out, out.Error)
	}
	if out.PendingCount() != 0 {
		t.Fatalf("targets pending: %+v", out.Targets)
	}
	if len(out.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(out.Targets))
	}
	wantSkips := []string{
		"defaults.full",
		"defaults.mini",
		"defaults.nano",
	}
	if got := skipFields(t, out.Targets[0]); !slices.Equal(got, wantSkips) {
		t.Fatalf("skipped = %v, want %v", got, wantSkips)
	}

	// The only config.yaml value change is the models.<base>.enabled flip. The
	// legacy pipeline re-marshals the merged effective config (pre-existing
	// behavior), so the guarantee the guard adds is value-level: the defaults
	// section stays exactly the operator's own — the live defaults.full value,
	// and no defaults.mini/nano the plan would have introduced — and the
	// modelgroups section survives verbatim.
	gotConfig, err := os.ReadFile(filepath.Join(sourceDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defaults    map[string]string `yaml:"defaults"`
		ModelGroups map[string]any    `yaml:"modelgroups"`
		Models      map[string]struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal(gotConfig, &doc); err != nil {
		t.Fatalf("parse published config: %v\n%s", err, gotConfig)
	}
	if !maps.Equal(doc.Defaults, map[string]string{"full": "codex/gpt"}) {
		t.Fatalf("defaults not left operator-owned: %v", doc.Defaults)
	}
	if leaves, ok := doc.ModelGroups["failover"].([]any); !ok || len(leaves) != 2 ||
		leaves[0] != "codex/gpt" || leaves[1] != "zai/glm" {
		t.Fatalf("modelgroups changed: %v", doc.ModelGroups)
	}
	if !doc.Models["zai/glm"].Enabled || !doc.Models["codex/gpt"].Enabled {
		t.Fatalf("models.enabled edits not applied: %+v", doc.Models)
	}

	// Definition-file edits keep working: the subagent gained the fallback.
	gotAgent, err := os.ReadFile(filepath.Join(sourceDir, "subagents", "agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotAgent), "fallback_models") || !strings.Contains(string(gotAgent), "zai/glm") {
		t.Fatalf("subagent fallback_models not written:\n%s", gotAgent)
	}
	if !strings.Contains(string(gotAgent), "model: codex/gpt") {
		t.Fatalf("subagent primary model changed:\n%s", gotAgent)
	}
}

// TestReconcileModelGroupsProjectLayerBlocksSharedDefaultsEdits is scenario 2:
// the global layer carries no modelgroups, but a registered project layer
// does. The staged merge composes the layers, so the shared global plan must
// not propose defaults edits either: without the guard the project candidate's
// merged config would combine the inserted tier default with the project's
// modelgroups and Polytoken would reject it. The defaults block must never
// appear and both targets must report the skip.
func TestReconcileModelGroupsProjectLayerBlocksSharedDefaultsEdits(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	projDir := filepath.Join(root, "proj")
	config := "version: 4\n" +
		"providers:\n" +
		"  codex:\n" +
		"    api_key: inert\n" +
		"  zai:\n" +
		"    api_key: inert\n" +
		"models:\n" +
		"  codex/gpt:\n" +
		"    enabled: true\n" +
		"  zai/glm:\n" +
		"    enabled: true\n"
	writeFile(t, filepath.Join(globalDir, "config.yaml"), config)
	writeFile(t, filepath.Join(projDir, ".polytoken", "config.yaml"),
		"version: 4\nmodelgroups:\n  failover:\n    - codex/gpt\n")

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}},
			"zai":   {Models: map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}}},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   globalDir,
			Global: true,
			Full:   policy.Chain{"codex/gpt", "zai/glm"},
		},
		Projects: []policy.Target{{ID: "proj", Root: projDir}},
	}

	coord := modelGroupsCoord(t, desired, globalDir, filepath.Join(root, "util"))
	out := coord.Reconcile(context.Background(), false, false, false)
	if !out.Accepted {
		t.Fatalf("reconcile not accepted: %+v err=%v", out, out.Error)
	}
	if out.PendingCount() != 0 {
		t.Fatalf("targets pending: %+v", out.Targets)
	}
	for _, tgt := range out.Targets {
		if tgt.TargetID != "global" {
			continue // the project target manages no chains: nothing to skip
		}
		if got := skipFields(t, tgt); !slices.Equal(got, []string{"defaults.full"}) {
			t.Fatalf("target %s skipped = %v, want [defaults.full]", tgt.TargetID, got)
		}
	}

	// The global config gains no defaults section: the shared global plan is
	// what project staging applies to its merged validation copies, so the
	// guard must have kept every tier-default write out of it. (The legacy
	// pipeline re-marshals the merged effective config — pre-existing
	// behavior — so the assertion is value-level.)
	gotConfig, err := os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(gotConfig, &doc); err != nil {
		t.Fatalf("parse published config: %v\n%s", err, gotConfig)
	}
	if _, ok := doc["defaults"]; ok {
		t.Fatalf("defaults section written despite modelgroups on the project layer:\n%s", gotConfig)
	}
	if _, ok := doc["modelgroups"]; ok {
		t.Fatalf("modelgroups appeared on the global layer:\n%s", gotConfig)
	}
	if models, ok := doc["models"].(map[string]any); !ok ||
		len(models) != 2 || models["codex/gpt"] == nil || models["zai/glm"] == nil {
		t.Fatalf("models block changed:\n%s", gotConfig)
	}
	// The project layer is untouched too.
	gotProj, err := os.ReadFile(filepath.Join(projDir, ".polytoken", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotProj) != "version: 4\nmodelgroups:\n  failover:\n    - codex/gpt\n" {
		t.Fatalf("project config.yaml changed:\n%s", gotProj)
	}
}

// TestReconcileWithoutModelGroupsStillWritesDefaults is scenario 3's control at
// the integration layer: with no modelgroups anywhere, the legacy behavior is
// unchanged — the stale defaults.full value is rewritten and no skip is
// reported.
func TestReconcileWithoutModelGroupsStillWritesDefaults(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	config := "providers:\n" +
		"  codex:\n" +
		"    api_key: inert\n" +
		"  zai:\n" +
		"    api_key: inert\n" +
		"models:\n" +
		"  codex/gpt:\n" +
		"    enabled: true\n" +
		"  zai/glm:\n" +
		"    enabled: true\n" +
		"defaults:\n" +
		"  full: zai/glm\n"
	writeFile(t, filepath.Join(sourceDir, "config.yaml"), config)

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Models: map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}}},
			"zai":   {Models: map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}}},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   sourceDir,
			Global: true,
			Full:   policy.Chain{"codex/gpt", "zai/glm"},
		},
	}

	coord := modelGroupsCoord(t, desired, sourceDir, filepath.Join(root, "util"))
	out := coord.Reconcile(context.Background(), false, false, false)
	if !out.Accepted {
		t.Fatalf("reconcile not accepted: %+v err=%v", out, out.Error)
	}
	if out.PendingCount() != 0 {
		t.Fatalf("targets pending: %+v", out.Targets)
	}
	if got := skipFields(t, out.Targets[0]); len(got) != 0 {
		t.Fatalf("unexpected skips without modelgroups: %v", got)
	}
	gotConfig, err := os.ReadFile(filepath.Join(sourceDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defaults map[string]string `yaml:"defaults"`
	}
	if err := yaml.Unmarshal(gotConfig, &doc); err != nil {
		t.Fatalf("parse published config: %v\n%s", err, gotConfig)
	}
	if !maps.Equal(doc.Defaults, map[string]string{"full": "codex/gpt"}) {
		t.Fatalf("defaults.full not rewritten: %v", doc.Defaults)
	}
}
