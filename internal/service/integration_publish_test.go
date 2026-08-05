package service

// Integration regression test for the coordinator→publisher seam (C1 + C2).
//
// Before the fix, no test wired the real Coordinator to the real Publisher: all
// coordinator tests used a spy publisher, and all publisher tests used
// pre-computed transactions with pre-computed hashes. So two integration bugs
// were invisible:
//
//   - C1: buildTransaction left NewHash/OldHash/Mode zero, so the Publisher's
//     applyOne hash assertion (sha256(temp) == NewHash) ALWAYS failed and no
//     target ever published.
//   - C2: transact recovered from an empty state (Revision=0) instead of the
//     persisted state, so staleness detection and the revision counter reset
//     every invocation.
//
// This test wires the REAL Coordinator (real Lock, real policy loader, real
// state store, real target registry, real reconcile.Builder, real staging
// Builder, real validate Runner backed by a fake CommandRunner that always
// succeeds) to the REAL Publisher via the PublisherAdapter, then drives a valid
// hook event through the full path: load state → recover → accept event →
// render → stage → validate → publish (with real hash computation) → commit
// state. It asserts at least one target successfully publishes (live files are
// written with the reconciled content, the state revision advances, and no
// target is pending).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/hook"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// fakeCommandRunner is a validate.CommandRunner whose commands always succeed
// (exit 0), standing in for the real Polytoken binary so the validation stage
// passes without an external dependency.
type fakeCommandRunner struct{}

func (fakeCommandRunner) Run(context.Context, string, []string, int64, map[string]string) (stdout, stderr []byte, exit int, err error) {
	return nil, nil, 0, nil
}

// TestCoordinatorPublisherIntegrationPublishesRealTarget is the C1/C2 regression
// test. It exercises the genuine end-to-end transaction and proves a valid hook
// event publishes at least one target: the live managed file is rewritten, the
// state revision advances, and no target remains pending.
func TestCoordinatorPublisherIntegrationPublishesRealTarget(t *testing.T) {
	root := t.TempDir()

	// --- synthetic source layer (the global Polytoken config root) ---
	sourceDir := filepath.Join(root, "source")
	agentPath := filepath.Join(sourceDir, "subagents", "agent.md")
	writeFile(t, filepath.Join(sourceDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\n  zai/glm:\n    enabled: true\n"+
			"providers:\n  codex:\n    api_key: inert\n"+
			"defaults:\n  full: codex/gpt\n")
	// The live managed definition with a chain that reconcile will reorder when
	// codex goes degraded: codex/gpt first becomes zai/glm first.
	writeFile(t, agentPath, "---\npolytoken:\n  model: codex/gpt\n---\nbody\n")

	// --- durable utility paths ---
	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "lock", "apply.lock")
	journalPath := filepath.Join(root, "journal", "apply.json")
	backupRoot := filepath.Join(root, "backups")
	stageTmp := filepath.Join(root, "stage")

	clock := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }
	store := state.Store{Path: statePath, Now: clock, RecoveredRetention: 24 * time.Hour}

	// Seed persisted observed state at revision 1 with codex healthy, so the
	// event (codex quota_reached) is accepted and advances to revision 2.
	prior := state.State{
		Schema:    1,
		Revision:  1,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

	// --- real production collaborators ---
	pub := publish.Publisher{
		Locker:      publish.NewFileLock(lockPath),
		State:       store,
		JournalPath: journalPath,
		Backups:     publish.BackupStore{Root: backupRoot, Limit: 3},
		ManagedRoot: sourceDir,
		Clock:       clock,
	}
	builder := staging.Builder{
		TempRoot: stageTmp,
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: sourceDir},
	}
	runner := validate.Runner{Binary: "polytoken", Commands: fakeCommandRunner{}}

	// The single global target: the agent definition whose chain owns codex/gpt.
	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				CodexBarProviders:  []string{"codex"},
				PolytokenProviders: []string{"codex"},
				Models:             map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
			},
			"zai": {
				CodexBarProviders:  []string{"zai"},
				PolytokenProviders: []string{"zai"},
				Models:             map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}},
			},
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

	coord := &Coordinator{
		Lock:            publish.NewFileLock(lockPath),
		Policy:          fixedPolicyLoader{desired: desired},
		PolicyWriter:    nilPolicyWriter{},
		State:           StoreState{Store: store},
		Targets:         NewTargetRegistry(),
		Builder:         NewReconciler(),
		Stage:           StagingStager{Builder: builder},
		Validate:        ValidateRunner{Runner: runner},
		Publish:         PublisherAdapter{Publisher: pub},
		Clock:           fixedClock{t: clock()},
		DiagnosticState: store,
	}

	// Drive a valid hook event: codex quota_reached degrades codex, so the
	// reconciler reorders the chain to put zai/glm first.
	ev := hook.Event{
		Type:      hook.QuotaReached,
		Provider:  "codex",
		Timestamp: clock().Add(time.Second),
	}
	out := coord.HandleEvent(context.Background(), ev)

	// The event must be accepted.
	if !out.Accepted {
		t.Fatalf("event not accepted: %+v err=%v", out, out.Error)
	}
	// The revision must advance from 1 to 2 (C2: persisted state was loaded, not
	// reset to 0).
	if out.Revision != 2 {
		t.Fatalf("revision=%d want 2 (C2: state not loaded from disk)", out.Revision)
	}
	// No target may remain pending: the publish path (C1: real hashes) must have
	// succeeded for every valid target.
	if out.PendingCount() != 0 {
		for _, tgt := range out.Targets {
			if tgt.Pending != nil {
				t.Logf("pending detail: %+v", *tgt.Pending)
			}
		}
		t.Fatalf("pending targets after publish: %+v", out.Targets)
	}

	// The live managed file must have been rewritten by the real Publisher with
	// the reconciled content (zai/glm promoted ahead of the degraded codex/gpt).
	liveAfter, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatalf("read live agent: %v", err)
	}
	if !strings.Contains(string(liveAfter), "zai/glm") {
		t.Fatalf("live file not published with reconciled content (C1):\n%s", liveAfter)
	}

	// The committed state on disk must carry the advanced revision.
	committed, err := store.Load()
	if err != nil {
		t.Fatalf("load committed state: %v", err)
	}
	if committed.Revision != 2 {
		t.Fatalf("committed revision=%d want 2", committed.Revision)
	}
	// The global target must be recorded as applied at revision 2.
	ts, ok := committed.Targets["global"]
	if !ok {
		t.Fatal("global target missing from committed state")
	}
	if ts.AppliedRevision != 2 || ts.Pending != nil {
		t.Fatalf("global target not applied: %+v", ts)
	}
}

// fixedPolicyLoader returns a preset desired policy without touching the
// filesystem, so the integration test controls the policy graph directly.
type fixedPolicyLoader struct {
	desired policy.Desired
}

func (f fixedPolicyLoader) LoadPolicy() (policy.Desired, error) { return f.desired, nil }
func (f fixedPolicyLoader) DesiredExists() bool                 { return true }

// nilPolicyWriter is a no-op policy.Writer; the hook path does not write
// desired.yaml.
type nilPolicyWriter struct{}

func (nilPolicyWriter) CreateAtomic(context.Context, policy.Desired) error  { return nil }
func (nilPolicyWriter) ReplaceAtomic(context.Context, policy.Desired) error { return nil }

// fixedClock implements Clock, returning a constant time.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// writeFile is a small test helper that creates parent directories and writes a
// file with 0600.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
