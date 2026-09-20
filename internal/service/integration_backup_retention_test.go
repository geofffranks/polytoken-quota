package service

// Integration tests for the policy→publisher backup-retention seam (AC1/AC2).
//
// The Coordinator must apply the policy-loaded operational.backup_count to the
// concrete publisher's BackupStore.Limit before every locked apply:
//
//   - AC1: a policy omitting backup_count yields runtime retention 1 per
//     managed file (the load-time default), overriding whatever limit the
//     publisher was constructed with.
//   - AC2: an explicit backup_count N yields runtime retention N.
//
// These tests mirror cmd's newCoordinator wiring (real Coordinator + real
// PublisherAdapter + real Publisher over a real policy file), because
// newCoordinator lives in package main and cannot be imported by tests; the
// implementation-review step confirms main's wiring matches this shape.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// TestCoordinatorAppliesPolicyBackupRetention proves the loaded
// operational.backup_count governs runtime retention through the real
// coordinator→publisher path, pruning pre-existing backups oldest-first.
func TestCoordinatorAppliesPolicyBackupRetention(t *testing.T) {
	for _, tc := range []struct {
		name            string
		operationalYAML string
		wantCount       int
	}{
		{
			// AC1: omitted backup_count → policy load default 1 at runtime.
			name:            "policy omitting backup_count retains one pre-apply backup",
			operationalYAML: "",
			wantCount:       1,
		},
		{
			// AC2: explicit backup_count 3 → runtime retention 3.
			name:            "explicit backup_count 3 retains three pre-apply backups",
			operationalYAML: "operational:\n  backup_count: 3\n",
			wantCount:       3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()

			// --- synthetic source layer (the global Polytoken config root) ---
			sourceDir := filepath.Join(root, "source")
			agentPath := filepath.Join(sourceDir, "subagents", "agent.md")
			const liveBefore = "---\npolytoken:\n  model: codex/gpt\n---\nbody\n"
			writeFile(t, filepath.Join(sourceDir, "config.yaml"),
				"models:\n  codex/gpt:\n    enabled: true\n  zai/glm:\n    enabled: true\n"+
					"providers:\n  codex:\n    api_key: inert\n"+
					"defaults:\n  full: codex/gpt\n")
			writeFile(t, agentPath, liveBefore)

			// --- durable utility paths ---
			desiredPath := filepath.Join(root, "desired.yaml")
			desiredYAML := "version: 1\n" +
				"providers:\n" +
				"  codex:\n    models: [codex/gpt]\n" +
				"  zai:\n    models: [zai/glm]\n" +
				"global:\n" +
				"  id: global\n" +
				"  root: " + sourceDir + "\n" +
				"  definitions:\n" +
				"    - path: subagents/agent.md\n" +
				"      chain: [codex/gpt, zai/glm]\n" +
				tc.operationalYAML
			writeFile(t, desiredPath, desiredYAML)

			statePath := filepath.Join(root, "state.json")
			lockPath := filepath.Join(root, "lock", "apply.lock")
			journalPath := filepath.Join(root, "journal", "apply.json")
			backupRoot := filepath.Join(root, "backups")
			stageTmp := filepath.Join(root, "stage")

			clock := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }
			store := state.Store{Path: statePath, Now: clock, RecoveredRetention: 24 * time.Hour}

			// Seed persisted observed state at revision 1 with codex exhausted,
			// so reconcile promotes the healthy alternative and the definition's
			// content changes — forcing a real apply with a real pre-apply
			// snapshot.
			prior := state.State{
				Schema:    1,
				Revision:  1,
				Providers: map[string]state.ProviderState{"codex": {Quota: state.QuotaExhausted, Availability: state.Available}},
				Targets:   map[string]state.TargetState{},
			}
			if err := store.Save(prior); err != nil {
				t.Fatal(err)
			}

			// Seed >1 pre-existing backups for the single managed definition
			// file, exactly as legacy applies would have left behind. The
			// seeding store uses a high limit so seeding itself does not prune.
			// The first seeded path pins the exact sanitized base (readable
			// fold + digest) so assertions below count only this file's
			// backups — config.yaml is also managed and gets its own.
			seeder := publish.BackupStore{Root: backupRoot, Limit: 99}
			var agentBase string
			for i := 0; i < 3; i++ {
				bp, err := seeder.Snapshot(agentPath)
				if err != nil {
					t.Fatalf("seed backup %d: %v", i+1, err)
				}
				if i == 0 {
					name := filepath.Base(bp)
					agentBase = name[:strings.LastIndexByte(name, '.')]
				}
			}
			if seeded := agentBackups(backupFiles(t, backupRoot), agentBase); len(seeded) != 3 {
				t.Fatalf("seeded %d agent.md backups, want 3", len(seeded))
			}

			// The publisher is constructed with a retention limit (5) that
			// differs from both policy outcomes; the Coordinator must overwrite
			// it from the loaded policy before the apply.
			pub := publish.Publisher{
				Locker:      publish.NewFileLock(lockPath),
				State:       store,
				JournalPath: journalPath,
				Backups:     publish.BackupStore{Root: backupRoot, Limit: 5},
				ManagedRoot: sourceDir,
				Clock:       clock,
			}
			builder := staging.Builder{
				TempRoot: stageTmp,
				AuthMode: staging.AuthInert,
				Sources:  staging.FSMaterializer{GlobalDir: sourceDir},
			}
			runner := validate.Runner{Binary: "polytoken", Commands: fakeCommandRunner{}}

			// Pointer adapter — the same shape main's newCoordinator wires — so
			// SetBackupLimit is reachable through the Publisher interface.
			coord := &Coordinator{
				Lock:         publish.NewFileLock(lockPath),
				Policy:       FilePolicyLoader{Path: desiredPath},
				PolicyWriter: nilPolicyWriter{},
				State:        StoreState{Store: store},
				Targets:      NewTargetRegistry(),
				Builder:      NewReconciler(),
				Stage:        StagingStager{Builder: builder},
				Validate:     ValidateRunner{Runner: runner},
				Publish:      &PublisherAdapter{Publisher: pub},
				Clock:        fixedClock{t: clock()},
			}

			out := coord.Reconcile(context.Background(), false, false, false)
			if !out.Accepted {
				t.Fatalf("reconcile not accepted: %+v err=%v", out, out.Error)
			}
			if out.Revision != 2 {
				t.Fatalf("revision=%d want 2", out.Revision)
			}
			if out.PendingCount() != 0 {
				t.Fatalf("pending targets after publish: %+v", out.Targets)
			}

			// The apply must have happened: the live definition carries the
			// reconciled content (zai/glm promoted ahead of degraded codex).
			liveAfter, err := os.ReadFile(agentPath)
			if err != nil {
				t.Fatalf("read live agent: %v", err)
			}
			if !strings.Contains(string(liveAfter), "zai/glm") {
				t.Fatalf("live file not published with reconciled content:\n%s", liveAfter)
			}

			// Runtime retention equals the loaded policy count per managed
			// file, not the constructor limit (which would have retained 4
			// agent.md backups). The fresh snapshot carries sequence 4 — the
			// newest, and the one a journal from this apply would reference —
			// so it must never be pruned.
			retained := agentBackups(backupFiles(t, backupRoot), agentBase)
			if len(retained) != tc.wantCount {
				t.Fatalf("retained agent.md backups=%v (count %d), want %d", retained, len(retained), tc.wantCount)
			}
			newest := backupFileForSeq(t, backupRoot, retained, 4)
			backupContent, err := os.ReadFile(newest)
			if err != nil {
				t.Fatalf("read newest backup: %v", err)
			}
			if string(backupContent) != liveBefore {
				t.Fatalf("newest backup is not the pre-apply content:\n%s", backupContent)
			}
		})
	}
}

// backupFile describes one retained backup under a backup root.
type backupFile struct {
	name string
	seq  int
}

// path returns the backup's full path under root.
func (f backupFile) path(root string) string {
	return filepath.Join(root, f.name)
}

// backupFiles lists every backup file under root as name+sequence pairs. The
// test layout keeps a single managed file, so every entry must parse as
// "<base>.<seq>".
func backupFiles(t *testing.T, root string) []backupFile {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	files := make([]backupFile, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		i := strings.LastIndexByte(name, '.')
		if i < 0 {
			t.Fatalf("unexpected backup entry %q", name)
		}
		n, err := strconv.Atoi(name[i+1:])
		if err != nil {
			t.Fatalf("unexpected backup entry %q", name)
		}
		files = append(files, backupFile{name: name, seq: n})
	}
	return files
}

// agentBackups filters retained backups to the managed definition file's
// sanitized base, so per-file retention is asserted exactly.
func agentBackups(files []backupFile, base string) []backupFile {
	out := make([]backupFile, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.name, base) {
			out = append(out, f)
		}
	}
	return out
}

// backupFileForSeq returns the full path of the retained backup with sequence
// n, failing the test when no retained backup carries it.
func backupFileForSeq(t *testing.T, root string, files []backupFile, seq int) string {
	t.Helper()
	for _, f := range files {
		if f.seq == seq {
			return f.path(root)
		}
	}
	t.Fatalf("no retained backup with sequence %d among %v", seq, files)
	return ""
}
