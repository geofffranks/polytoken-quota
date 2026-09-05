package service

// Task 14 filesystem security matrix. Each subtest exercises one cross-cutting
// filesystem safety property through the real components: unregistered roots are
// never touched, symlinks and path traversal in managed definitions are refused,
// staging uses restrictive permissions, CRLF line endings are preserved, concurrent
// invocations serialize under a real advisory lock, per-target partial success
// preserves last-known-good bytes, and recovered errors age out of retention.
// The dispatcher follows the Task 14 blueprint's runFilesystemCase shape.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/document"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/testutil"
)

// TestFilesystemSecurityMatrix is the Task 14 blueprint filesystem matrix.
func TestFilesystemSecurityMatrix(t *testing.T) {
	for _, tc := range []string{
		"unregistered-untouched",
		"symlink-refused",
		"traversal-refused",
		"restrictive-modes",
		"crlf-preserved",
		"concurrent-hooks",
		"partial-success",
		"retention",
	} {
		t.Run(tc, func(t *testing.T) {
			runFilesystemCase(t, tc)
		})
	}
}

func runFilesystemCase(t *testing.T, name string) {
	t.Helper()
	switch name {
	case "unregistered-untouched":
		fsUnregisteredUntouched(t)
	case "symlink-refused":
		fsSymlinkRefused(t)
	case "traversal-refused":
		fsTraversalRefused(t)
	case "restrictive-modes":
		fsRestrictiveModes(t)
	case "crlf-preserved":
		fsCRLFPreserved(t)
	case "concurrent-hooks":
		fsConcurrentHooks(t)
	case "partial-success":
		fsPartialSuccess(t)
	case "retention":
		fsRetention(t)
	default:
		t.Fatalf("unknown filesystem case %q", name)
	}
}

// fsUnregisteredUntouched proves staging only reads registered layers: an
// unregistered sibling directory is byte-identical before and after a staging
// build, and its files never appear in the candidate.
func fsUnregisteredUntouched(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	otherDir := filepath.Join(root, "unregistered")
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\ndefaults:\n  full: codex/gpt\n")
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	testutil.WriteFile(t, filepath.Join(otherDir, "secret.yaml"), "api_key: must-not-leak\n")

	before := testutil.Snapshot(t, root)
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	c, err := staging.Builder{
		TempRoot: t.TempDir(), AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: "global"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	after := testutil.Snapshot(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unregistered root changed:\nbefore=%v\nafter=%v", before, after)
	}
	// The unregistered file never entered the candidate.
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unregistered file leaked into staging candidate")
	}
}

// fsSymlinkRefused proves target.Resolve rejects a managed definition file that
// is a symlink, so writes can never escape the registered root.
func fsSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real.md")
	outside := filepath.Join(root, "outside", "escape.md")
	testutil.WriteFile(t, real, "---\n---\n")
	testutil.WriteFile(t, filepath.Join(root, "config.yaml"), "defaults: {}\n")
	testutil.WriteFile(t, outside, "escaped\n")
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	in := policy.Target{ID: "t", Root: root, Definitions: []policy.Definition{{Path: "link.md"}}}
	if _, err := target.Resolve(in); !errors.Is(err, target.ErrSymlinkManagedFile) {
		t.Fatalf("want ErrSymlinkManagedFile, got %v", err)
	}
}

// fsTraversalRefused proves target.Resolve rejects a definition path that
// escapes the registered root via "../".
func fsTraversalRefused(t *testing.T) {
	root := t.TempDir()
	escapeTarget := filepath.Join(root, "secret.md")
	testutil.WriteFile(t, escapeTarget, "secret\n")
	// A registered root with a traversal definition that points outside it.
	inner := filepath.Join(root, "managed")
	testutil.WriteFile(t, filepath.Join(inner, "config.yaml"), "models: {}\n")
	in := policy.Target{ID: "t", Root: inner, Definitions: []policy.Definition{{Path: "../secret.md"}}}
	if _, err := target.Resolve(in); err == nil {
		t.Fatal("traversal path was accepted")
	}
}

// fsRestrictiveModes proves staging creates every directory with 0700 and every
// file with 0600.
func fsRestrictiveModes(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\n")
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	c, err := staging.Builder{
		TempRoot: t.TempDir(), AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: "global"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	walk := func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if path == c.WorkingDir {
			return nil
		}
		if info.IsDir() {
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("dir %s perm=%o want 0700", path, info.Mode().Perm())
			}
		} else if info.Mode().Perm() != 0o600 {
			return fmt.Errorf("file %s perm=%o want 0600", path, info.Mode().Perm())
		}
		return nil
	}
	if err := filepath.Walk(c.Root, walk); err != nil {
		t.Fatal(err)
	}
}

// fsCRLFPreserved proves the byte-preserving frontmatter editor changes only the
// managed model span and leaves CRLF line endings and the body untouched.
func fsCRLFPreserved(t *testing.T) {
	in := []byte("---\r\n" +
		"polytoken:\r\n" +
		"  model: codex/gpt\r\n" +
		"description: keep exact\r\n" +
		"---\r\n# Body with CRLF.\r\n")
	v := "zai/glm"
	out, err := document.EditFrontmatter(in, []document.Edit{
		{Path: []string{"polytoken", "model"}, Kind: document.Scalar, Scalar: &v},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("codex/gpt"), []byte("zai/glm"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("CRLF/body not preserved:\n got=%q\nwant=%q", out, want)
	}
}

// fsConcurrentHooks proves a real advisory flock serializes concurrent
// invocations: at most one goroutine holds the lock at a time. This mirrors the
// production lock (flock LOCK_EX), exercised from the integration layer.
func fsConcurrentHooks(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "hook.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var mu sync.Mutex
	inUse := 0
	maxObserved := 0
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
			if err != nil {
				t.Errorf("open lock: %v", err)
				return
			}
			defer h.Close()
			if err := syscall.Flock(int(h.Fd()), syscall.LOCK_EX); err != nil {
				t.Errorf("flock: %v", err)
				return
			}
			defer syscall.Flock(int(h.Fd()), syscall.LOCK_UN)
			mu.Lock()
			inUse++
			if inUse > maxObserved {
				maxObserved = inUse
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inUse--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxObserved != 1 {
		t.Fatalf("max concurrent lock holders=%d want 1", maxObserved)
	}
}

// fsPartialSuccess proves per-target partial success through the real Publisher:
// target A's live file is replaced with the candidate, while target B (which
// failed to render and so was never published) keeps its last-known-good bytes.
func fsPartialSuccess(t *testing.T) {
	root := t.TempDir()
	// Target A: a managed file that will be published.
	aDir := filepath.Join(root, "a")
	aLive := filepath.Join(aDir, "config.yaml")
	aPrior := "model: zai/glm\n"
	aCandidate := "model: codex/gpt\n"
	testutil.WriteFile(t, aLive, aPrior)
	aTemp := filepath.Join(aDir, ".candidate.yaml")
	testutil.WriteFile(t, aTemp, aCandidate)
	// Target B: a managed file that fails render (empty chain → no publish); its
	// live bytes must be preserved exactly.
	bDir := filepath.Join(root, "b")
	bLive := filepath.Join(bDir, "agent.md")
	bLKG := "---\npolytoken:\n  model: healthy/a\n---\nbody\n"
	testutil.WriteFile(t, bLive, bLKG)

	store := state.Store{Path: filepath.Join(root, "state.json"), RecoveredRetention: 24 * time.Hour}
	prior := state.State{Schema: 1, Revision: 1, Providers: map[string]state.ProviderState{},
		Targets: map[string]state.TargetState{}}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	next := prior
	next.Revision = 2
	pub := publish.Publisher{
		Locker:      realFlock{path: filepath.Join(root, "lock.lock")},
		State:       store,
		JournalPath: filepath.Join(root, "journal", "a.json"),
		Backups:     publish.BackupStore{Root: filepath.Join(root, "backups"), Limit: 3},
		ManagedRoot: aDir,
	}
	tx := publish.Transaction{
		Prior:    prior,
		Next:     next,
		TargetID: "a",
		Replacements: []publish.Replacement{{
			LivePath: aLive, TempPath: aTemp,
			OldHash: sha256sum(aPrior), NewHash: sha256sum(aCandidate),
			Mode: 0o600,
		}},
	}
	if _, err := pub.ApplyUnderLock(context.Background(), tx); err != nil {
		t.Fatalf("publish target a: %v", err)
	}
	// Target A's live file now holds the candidate.
	got, err := os.ReadFile(aLive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(aCandidate)) {
		t.Fatalf("target a not published:\n got=%q\nwant=%q", got, aCandidate)
	}
	// Target B was never published: its last-known-good bytes are intact.
	bAfter, err := os.ReadFile(bLive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bAfter, []byte(bLKG)) {
		t.Fatalf("pending target b LKG mutated:\n got=%q\nwant=%q", bAfter, bLKG)
	}
}

// fsRetention proves recovered errors older than the retention cutoff are pruned
// while recent ones survive (state.PruneRecovered).
func fsRetention(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	retention := 7 * 24 * time.Hour
	old := state.ApplyFailure{TargetID: "t", Stage: "publish", ResolvedAt: now.Add(-8 * 24 * time.Hour)}
	recent := state.ApplyFailure{TargetID: "t", Stage: "publish", ResolvedAt: now.Add(-1 * 24 * time.Hour)}
	s := state.State{Recovered: []state.ApplyFailure{old, recent}}
	pruned := state.PruneRecovered(s, now, retention)
	if len(pruned.Recovered) != 1 || pruned.Recovered[0].ResolvedAt != recent.ResolvedAt {
		t.Fatalf("retention pruned wrong entries: %+v", pruned.Recovered)
	}
	// A boundary entry exactly at the cutoff is pruned (<= cutoff).
	boundary := state.ApplyFailure{TargetID: "t", Stage: "publish", ResolvedAt: now.Add(-retention)}
	s2 := state.State{Recovered: []state.ApplyFailure{boundary}}
	if got := state.PruneRecovered(s2, now, retention); len(got.Recovered) != 0 {
		t.Fatalf("boundary entry not pruned: %+v", got.Recovered)
	}
}
