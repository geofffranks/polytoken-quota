package service

// pq-m4k9 (A): a project candidate's global layer must reflect the reconciled
// global view, not stale live files. Without this, live global subagents that
// reference disabled/unknown models (which polytoken's registry rejects) are
// copied verbatim into the project candidate while the same run fixes them only
// in the global candidate, so the project can never validate. The coordinator
// shares the global target's rendered plan into project staging.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// hospitalFailRunner passes config validate and fails doctor (so candidates are
// retained for inspection), standing in for the real Polytoken binary.
type projectDoctorFailRunner struct{}

func (projectDoctorFailRunner) Run(_ context.Context, _ string, args []string, _ int64, _ map[string]string) ([]byte, []byte, int, error) {
	for _, a := range args {
		if a == "doctor" {
			return []byte("doctor: simulated failure"), nil, 1, nil
		}
	}
	return nil, nil, 0, nil
}

// projectGlobalHarness builds a real-collaborator coordinator with one global
// target and one project target. The global target's plan rewrites
// subagents/global-agent.md; the test asserts the PROJECT candidate's copy of
// that file carries the reconciled model, not the stale live one.
func projectGlobalHarness(t *testing.T) (*Coordinator, string) {
	t.Helper()
	root := t.TempDir()

	globalDir := filepath.Join(root, "global")
	globalDef := filepath.Join(globalDir, "subagents", "global-agent.md")
	writeFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  minime/m:\n    enabled: true\n  codex/gpt-5.6-sol:\n    enabled: true\n  zai/glm-5.3-flash:\n    enabled: true\nproviders:\n  codex:\n    api_key: inert\n")
	writeFile(t, globalDef, "---\npolytoken:\n  model: codex/gpt-5.6-sol\n---\nbody\n")

	projRoot := filepath.Join(root, "projroot")
	projDef := filepath.Join(projRoot, ".polytoken", "subagents", "proj-agent.md")
	writeFile(t, filepath.Join(projRoot, ".polytoken", "config.yaml"), "models:\n  zai/glm-5.3-flash:\n    enabled: true\n")
	writeFile(t, projDef, "---\npolytoken:\n  model: zai/glm-5.3-flash\n---\nbody\n")

	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "lock", "apply.lock")
	journalPath := filepath.Join(root, "journal", "apply.json")
	backupRoot := filepath.Join(root, "backups")
	stageTmp := filepath.Join(root, "stage")
	when := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	store := state.Store{Path: statePath, Now: func() time.Time { return when }, RecoveredRetention: 24 * time.Hour}

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"minime": {Models: map[string]policy.ModelBaseline{"minime/m": {Enabled: true}}},
			"codex":  {Models: map[string]policy.ModelBaseline{"codex/gpt-5.6-sol": {Enabled: true}}},
			"zai":    {Models: map[string]policy.ModelBaseline{"zai/glm-5.3-flash": {Enabled: true}}},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   globalDir,
			Global: true,
			Definitions: []policy.Definition{{
				Path:  filepath.Join("subagents", "global-agent.md"),
				Chain: policy.Chain{"minime/m(high)", "codex/gpt-5.6-sol(high)"},
			}},
		},
		Projects: []policy.Target{{
			ID:   "proj",
			Root: projRoot,
			Definitions: []policy.Definition{{
				Path:  filepath.Join("subagents", "proj-agent.md"),
				Chain: policy.Chain{"zai/glm-5.3-flash(high)"},
			}},
		}},
	}

	coord := &Coordinator{
		Lock:         publish.NewFileLock(lockPath),
		Policy:       fixedPolicyLoader{desired: desired},
		PolicyWriter: nilPolicyWriter{},
		State:        StoreState{Store: store},
		Targets:      NewTargetRegistry(),
		Builder:      NewReconciler(),
		Stage: StagingStager{Builder: staging.Builder{
			TempRoot: stageTmp,
			AuthMode: staging.AuthInert,
			Sources:  staging.FSMaterializer{GlobalDir: globalDir},
		}},
		Validate: ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: projectDoctorFailRunner{}}},
		Publish: PublisherAdapter{Publisher: publish.Publisher{
			Locker:      publish.NewFileLock(lockPath),
			State:       store,
			JournalPath: journalPath,
			Backups:     publish.BackupStore{Root: backupRoot, Limit: 3},
			ManagedRoot: globalDir,
			Clock:       func() time.Time { return when },
		}},
		Clock: fixedClock{t: when},
	}
	return coord, projDef
}

func TestProjectCandidateUsesReconciledGlobalLayer(t *testing.T) {
	coord, _ := projectGlobalHarness(t)
	out := coord.Reconcile(context.Background(), true /*dry-run*/, true /*keep-staging*/, false)

	var projectRoot string
	for _, target := range out.Targets {
		if target.Pending == nil || target.StagingRoot == "" {
			t.Fatalf("expected retained pending target: %+v", target)
		}
		if target.TargetID == "proj" {
			projectRoot = target.StagingRoot
		}
		t.Cleanup(func() { _ = os.RemoveAll(target.StagingRoot) })
	}
	if projectRoot == "" {
		t.Fatalf("no project target retained: %+v", out.Targets)
	}

	// The global plan must rewrite the global definition inside the PROJECT
	// candidate's global layer (both the XDG copy doctor reads and config/).
	for _, layer := range []string{"user-config/polytoken", "config"} {
		path := filepath.Join(projectRoot, layer, "subagents", "global-agent.md")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(b), "model: minime/m(high)") {
			t.Errorf("%s does not carry the reconciled global model:\n%s", path, b)
		}
	}

	// The project's own definition is still reconciled from the project plan.
	b, err := os.ReadFile(filepath.Join(projectRoot, "user-config", "polytoken", "subagents", "proj-agent.md"))
	if err != nil {
		t.Fatalf("read project def: %v", err)
	}
	if !strings.Contains(string(b), "model: zai/glm-5.3-flash(high)") {
		t.Errorf("project definition not reconciled from project plan:\n%s", b)
	}
}
