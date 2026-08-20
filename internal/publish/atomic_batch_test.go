package publish

// Tests for the live-file rename batch: within one transaction the temp→live
// renames must be contiguous filesystem operations, so a concurrent reader of
// the managed directory never observes a durable journal rewrite or directory
// fsync interleaved between two live-file replacements. Those interleaved
// durable steps previously widened the mixed old/new file-set window to tens
// or hundreds of milliseconds, which a concurrent `polytoken` launch could
// intermittently catch as an invalid configuration.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/testutil"
)

// recordingFS decorates a DurableFS, recording every operation as
// "<op>:<detail>" in call order.
type recordingFS struct {
	DurableFS
	mu  sync.Mutex
	ops []string
}

func (r *recordingFS) record(op string) {
	r.mu.Lock()
	r.ops = append(r.ops, op)
	r.mu.Unlock()
}

func (r *recordingFS) opsCopy() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ops))
	copy(out, r.ops)
	return out
}

func (r *recordingFS) MkdirAll(path string, perm os.FileMode) error {
	r.record("MkdirAll")
	return r.DurableFS.MkdirAll(path, perm)
}

func (r *recordingFS) ReadFile(name string) ([]byte, error) {
	r.record("ReadFile:" + name)
	return r.DurableFS.ReadFile(name)
}

func (r *recordingFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	r.record("WriteFile:" + name)
	return r.DurableFS.WriteFile(name, data, perm)
}

func (r *recordingFS) CreateTemp(dir, pattern string) (*os.File, error) {
	r.record("CreateTemp:" + dir)
	return r.DurableFS.CreateTemp(dir, pattern)
}

func (r *recordingFS) Open(name string) (*os.File, error) {
	r.record("Open:" + name)
	return r.DurableFS.Open(name)
}

func (r *recordingFS) Fsync(f *os.File) error {
	r.record("Fsync")
	return r.DurableFS.Fsync(f)
}

func (r *recordingFS) Rename(oldpath, newpath string) error {
	r.record("Rename:" + newpath)
	return r.DurableFS.Rename(oldpath, newpath)
}

func (r *recordingFS) SyncDir(dir string) error {
	r.record("SyncDir:" + dir)
	return r.DurableFS.SyncDir(dir)
}

func (r *recordingFS) RemoveAll(path string) error {
	r.record("RemoveAll:" + path)
	return r.DurableFS.RemoveAll(path)
}

// multiEnv is a stagedEnv-like fixture whose transaction replaces TWO managed
// live files, mirroring a real reconcile plan (config file + definition file).
type multiEnv struct {
	stagedEnv
	LivePath2 string
	TempPath2 string
}

func newMultiEnv(t *testing.T) multiEnv {
	t.Helper()
	env := newStagedEnv(t, "")
	root := filepath.Dir(env.LivePath)

	live2 := filepath.Join(root, "defs", "facet.yaml")
	constLive2 := "polytoken:\n  model: zai/glm\n"
	testutil.WriteFile(t, live2, constLive2)
	constCand2 := "polytoken:\n  model: codex/gpt\n"
	temp2 := filepath.Join(root, ".candidate-facet.yaml")
	testutil.WriteFile(t, temp2, constCand2)

	mode := fileMode(OSFS{}, live2)
	env.Tx.Replacements = append(env.Tx.Replacements, Replacement{
		LivePath: live2,
		TempPath: temp2,
		OldHash:  sha256.Sum256([]byte(constLive2)),
		NewHash:  sha256.Sum256([]byte(constCand2)),
		Mode:     mode,
	})

	rec := &recordingFS{DurableFS: env.Publisher.FS}
	env.Publisher.FS = rec
	return multiEnv{stagedEnv: env, LivePath2: live2, TempPath2: temp2}
}

// TestApplyUnderLockRenamesAreContiguous proves the atomicity fix: the
// temp→live renames of a multi-replacement transaction are back-to-back FS
// operations. No journal write (CreateTemp/Rename onto the journal), no
// directory fsync, and no temp preparation may occur between the first and
// last live rename, because each interleaved durable step widens the window
// in which a concurrent reader sees a mixed old/new file set.
func TestApplyUnderLockRenamesAreContiguous(t *testing.T) {
	env := newMultiEnv(t)

	if _, err := env.Publisher.ApplyUnderLock(context.Background(), env.Tx); err != nil {
		t.Fatalf("ApplyUnderLock: %v", err)
	}

	ops := env.Publisher.FS.(*recordingFS).opsCopy()
	var liveRenames []int
	for i, op := range ops {
		if !strings.HasPrefix(op, "Rename:") {
			continue
		}
		dst := strings.TrimPrefix(op, "Rename:")
		if dst == env.LivePath || dst == env.LivePath2 {
			liveRenames = append(liveRenames, i)
		}
	}
	if len(liveRenames) != 2 {
		t.Fatalf("expected 2 live renames, got %d in ops %v", len(liveRenames), ops)
	}
	first, last := liveRenames[0], liveRenames[len(liveRenames)-1]
	if last != first+1 {
		t.Fatalf("live renames are not contiguous: ops between them: %v",
			ops[first+1:last])
	}

	// Durability is preserved: the live directory is fsync'd after the batch.
	sawLiveDirSync := false
	for _, op := range ops[last+1:] {
		if op == "SyncDir:"+filepath.Dir(env.LivePath) {
			sawLiveDirSync = true
		}
	}
	if !sawLiveDirSync {
		t.Fatalf("no SyncDir of the live directory after the rename batch: %v", ops)
	}

	// Both live files carry the candidate content after the batch.
	for _, p := range []struct{ path, want string }{
		{env.LivePath, constCandidate},
		{env.LivePath2, "polytoken:\n  model: codex/gpt\n"},
	} {
		got, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatalf("read %s: %v", p.path, err)
		}
		if string(got) != p.want {
			t.Fatalf("live file %s = %q, want %q", p.path, got, p.want)
		}
	}
}

// TestApplyUnderLockRejectsStaleLiveBytesBeforeRename proves publication refuses
// to overwrite bytes changed after preparation captured OldHash.
func TestApplyUnderLockRejectsStaleLiveBytesBeforeRename(t *testing.T) {
	env := newStagedEnv(t, "")
	newer := []byte("concurrent newer config\n")
	if err := os.WriteFile(env.LivePath, newer, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := env.Publisher.ApplyUnderLock(context.Background(), env.Tx); err == nil {
		t.Fatal("expected stale live bytes to reject publication")
	}
	got, err := os.ReadFile(env.LivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newer) {
		t.Fatalf("stale publication replaced newer live bytes: got %q want %q", got, newer)
	}
}

// TestApplyUnderLockJournalDurableBeforeFirstRename pins the crash-safety
// ordering the batching must not regress: the durable journal (CreateTemp in
// the journal directory followed by a Rename onto the journal path) completes
// before the first temp→live rename.
func TestApplyUnderLockJournalDurableBeforeFirstRename(t *testing.T) {
	env := newMultiEnv(t)

	if _, err := env.Publisher.ApplyUnderLock(context.Background(), env.Tx); err != nil {
		t.Fatalf("ApplyUnderLock: %v", err)
	}

	ops := env.Publisher.FS.(*recordingFS).opsCopy()
	journalRename, firstLive := -1, -1
	for i, op := range ops {
		if strings.HasPrefix(op, "Rename:") {
			dst := strings.TrimPrefix(op, "Rename:")
			switch dst {
			case env.Publisher.JournalPath:
				if journalRename == -1 {
					journalRename = i
				}
			case env.LivePath, env.LivePath2:
				if firstLive == -1 {
					firstLive = i
				}
			}
		}
	}
	if journalRename == -1 || firstLive == -1 {
		t.Fatalf("missing journal or live rename in ops %v", ops)
	}
	if journalRename > firstLive {
		t.Fatalf("journal rename (%d) not before first live rename (%d): %v",
			journalRename, firstLive, ops)
	}
}

// TestApplyUnderLockBatchRecoversFromInterruptedRename proves the batch keeps
// the existing crash semantics: when the rename batch is interrupted (fault at
// the second rename), recovery converges via restore-from-backup to a coherent
// pre-apply file set for BOTH files, with no mixed state left behind.
func TestApplyUnderLockBatchRecoversFromInterruptedRename(t *testing.T) {
	env := newMultiEnv(t)
	trace := &faultTrace{mu: newChanMutex()}
	renames := 0
	env.Publisher.Fault = func(step string) error {
		trace.fire(step)
		if step == stepRename {
			renames++
			if renames == 2 {
				return ErrInjected
			}
		}
		return nil
	}

	if _, err := env.Publisher.ApplyUnderLock(context.Background(), env.Tx); err == nil {
		t.Fatal("expected injected fault at second rename to fail ApplyUnderLock")
	}

	// Rewire to a passthrough fault and recover exactly as the coordinator
	// does after an interrupted apply.
	env.Publisher.Fault = singleShotFault(trace, "")
	rec, _, err := env.Publisher.Recover(context.Background(), env.Prior)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rec.Revision != env.Prior.Revision+1 {
		t.Fatalf("recovered revision = %d, want %d", rec.Revision, env.Prior.Revision+1)
	}

	// Both live files are back to the pre-apply content — no mixed state.
	for _, p := range []struct{ path, want string }{
		{env.LivePath, constLive},
		{env.LivePath2, "polytoken:\n  model: zai/glm\n"},
	} {
		got, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatalf("read %s: %v", p.path, err)
		}
		if string(got) != p.want {
			t.Fatalf("live file %s = %q, want restored %q", p.path, got, p.want)
		}
	}
	if _, err := os.Stat(env.Publisher.JournalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still present after recovery: %v", err)
	}
}
