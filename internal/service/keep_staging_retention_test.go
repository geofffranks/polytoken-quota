package service

// Keep-staging retention durability (pq-m4k7). These tests drive the real
// production collaborator chain — staging.Builder, the ValidateRunner adapter
// (which detaches the Runner's internal cleanup), and the Coordinator's
// keep-staging branch — with only the external Polytoken binary stubbed.
//
// The defect under test: a candidate retained by `reconcile --dry-run
// --keep-staging` sits at the deterministic per-target stage root
// (quota-stage-<id>). Every later Builder.Build claims that same path with
// RemoveAll first, so the next reconcile silently destroys the retained
// diagnostic while the CLI has just told the operator to inspect it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// doctorFailingRunner stands in for the real Polytoken binary: `config
// validate` passes, `doctor` exits non-zero. This reproduces the doctor-stage
// pending that the keep-staging remediation text targets.
type doctorFailingRunner struct{}

func (doctorFailingRunner) Run(_ context.Context, _ string, args []string, _ int64, _ map[string]string) (stdout, stderr []byte, exit int, err error) {
	for _, a := range args {
		if a == "doctor" {
			return []byte("doctor: simulated startup failure"), nil, 1, nil
		}
	}
	return nil, nil, 0, nil
}

// newRetentionHarness builds a real-collaborator coordinator whose single
// global target has drift (codex exhausted in persisted state, so the chain
// reorders), mirroring the production wiring in cmd/polytoken-quota.
func newRetentionHarness(t *testing.T) *Coordinator {
	t.Helper()
	root := t.TempDir()

	sourceDir := filepath.Join(root, "source")
	writeFile(t, filepath.Join(sourceDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\n  zai/glm:\n    enabled: true\n"+
			"providers:\n  codex:\n    api_key: inert\n"+
			"defaults:\n  full: codex/gpt\n")
	writeFile(t, filepath.Join(sourceDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")

	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "lock", "apply.lock")
	journalPath := filepath.Join(root, "journal", "apply.json")
	backupRoot := filepath.Join(root, "backups")
	stageTmp := filepath.Join(root, "stage")

	when := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	store := state.Store{Path: statePath, Now: func() time.Time { return when }, RecoveredRetention: 24 * time.Hour}
	prior := state.State{
		Schema:    1,
		Revision:  1,
		Providers: map[string]state.ProviderState{"codex": {Quota: state.QuotaExhausted, Availability: state.Available}},
		Targets:   map[string]state.TargetState{},
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

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
			Definitions: []policy.Definition{{
				Path:  filepath.Join("subagents", "agent.md"),
				Chain: policy.Chain{"codex/gpt", "zai/glm"},
			}},
		},
	}

	return &Coordinator{
		Lock:         publish.NewFileLock(lockPath),
		Policy:       fixedPolicyLoader{desired: desired},
		PolicyWriter: nilPolicyWriter{},
		State:        StoreState{Store: store},
		Targets:      NewTargetRegistry(),
		Builder:      NewReconciler(),
		Stage: StagingStager{Builder: staging.Builder{
			TempRoot: stageTmp,
			AuthMode: staging.AuthInert,
			Sources:  staging.FSMaterializer{GlobalDir: sourceDir},
		}},
		Validate: ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: doctorFailingRunner{}}},
		Publish: PublisherAdapter{Publisher: publish.Publisher{
			Locker:      publish.NewFileLock(lockPath),
			State:       store,
			JournalPath: journalPath,
			Backups:     publish.BackupStore{Root: backupRoot, Limit: 3},
			ManagedRoot: sourceDir,
			Clock:       func() time.Time { return when },
		}},
		Clock: fixedClock{t: when},
	}
}

// TestKeepStagingRetentionSurvivesSubsequentReconcile proves the diagnostic a
// keep-staging dry-run retains is not destroyed by the tool's own next build
// (pq-m4k7). The operator workflow is: retain, then inspect later — often
// after running reconcile again.
func TestKeepStagingRetentionSurvivesSubsequentReconcile(t *testing.T) {
	coord := newRetentionHarness(t)
	ctx := context.Background()

	out := coord.Reconcile(ctx, true, true, false)
	if out.PendingCount() != 1 || out.Targets[0].StagingRoot == "" {
		t.Fatalf("keep-staging dry-run did not retain: %+v", out)
	}
	retained := out.Targets[0].StagingRoot
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("retained root missing immediately after retention: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(retained) })

	// A later plain reconcile (no keep-staging) must leave the retained
	// diagnostic untouched.
	if next := coord.Reconcile(ctx, true, false, false); !next.Accepted {
		t.Fatalf("subsequent dry-run not accepted: %+v", next)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("retained staging root destroyed by subsequent reconcile: %v", err)
	}
}

// TestKeepStagingRetainsFailedCandidateThroughRealValidator pins single-run
// retention through the real validation adapter chain (not the test spy): a
// doctor-stage failure with keep-staging must leave the reported staging root
// on disk at the doctor pending stage.
func TestKeepStagingRetainsFailedCandidateThroughRealValidator(t *testing.T) {
	coord := newRetentionHarness(t)

	out := coord.Reconcile(context.Background(), true, true, false)
	if out.PendingCount() != 1 {
		t.Fatalf("expected one pending target: %+v", out)
	}
	target := out.Targets[0]
	if target.Pending == nil || target.Pending.Stage != string(state.PendingDoctor) {
		t.Fatalf("expected doctor-stage pending: %+v", target)
	}
	if target.StagingRoot == "" {
		t.Fatal("doctor failure with keep-staging reported no staging root")
	}
	if _, err := os.Stat(target.StagingRoot); err != nil {
		t.Fatalf("retained staging root=%q missing after run: %v", target.StagingRoot, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target.StagingRoot) })
}
