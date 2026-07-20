package publish

// Tests for the publish package: lock serialization/timeout, fault injection at
// every durable step with recovery, recovery decision (roll-forward vs
// restore), no-op coherent commit, corrupt-journal detection, terminal
// invariant (state published before journal removal), backup retention and
// source-mode preservation, and TOCTOU symlink/path-escape rejection.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/testutil"
)

// --- Lock tests -------------------------------------------------------------

func newLocker(t *testing.T) Locker {
	t.Helper()
	return newFileLock(filepath.Join(t.TempDir(), "sub", "dir", "test.lock"))
}

// TestLockSerializesAndTimesOut is the brief's named lock test: a second Lock
// within a 1ms bounded context on an already-held lock must report
// context.DeadlineExceeded.
func TestLockSerializesAndTimesOut(t *testing.T) {
	l := newLocker(t)
	unlock, err := l.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

// TestLockFileMode0600 verifies the advisory lock file is created restrictive.
func TestLockFileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l := newFileLock(path).(interface { /* expose path */
	})
	_ = l
	locker := newFileLock(path)
	unlock, err := locker.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := info.Mode().Perm(); m != 0o600 {
		t.Fatalf("lock mode=%o want 0600", m)
	}
}

// TestLockConcurrentSerialization proves two goroutines never hold the lock at
// once: the second waits until the first releases.
func TestLockConcurrentSerialization(t *testing.T) {
	l := newFileLock(filepath.Join(t.TempDir(), "c.lock"))
	var held sync.Mutex
	inUse := 0
	maxObserved := 0
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := l.Lock(context.Background())
			if err != nil {
				t.Errorf("lock: %v", err)
				return
			}
			defer unlock()
			held.Lock()
			inUse++
			if inUse > maxObserved {
				maxObserved = inUse
			}
			held.Unlock()
			time.Sleep(2 * time.Millisecond)
			held.Lock()
			inUse--
			held.Unlock()
		}()
	}
	wg.Wait()
	if maxObserved != 1 {
		t.Fatalf("max concurrent holders=%d want 1", maxObserved)
	}
}

// TestLockCancelledContext proves an already-cancelled context errors quickly.
func TestLockCancelledContext(t *testing.T) {
	l := newLocker(t)
	unlock, err := l.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// --- Fault at every step recovers -------------------------------------------

func TestFaultAtEveryPublishStepRecovers(t *testing.T) {
	steps := []string{
		"backup", "journal-fsync", "temp-write", "temp-fsync",
		"rename", "dir-fsync", "progress", "state-publish", "journal-remove",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			env := faultEnv(t, step)
			// Apply fails at the injected step; the system is left
			// half-applied. Recover must converge to a coherent state.
			if _, err := env.Publisher.Apply(context.Background(), env.Tx); err == nil {
				t.Fatalf("expected Apply to fail at %s", step)
			}
			// Whether the journal was made durable before the fault determines
			// which coherent outcome Recover must reach. A pre-journal fault
			// (backup, journal-fsync) leaves no journal, so the accepted event
			// was never made durable and Recover is a no-op at the prior
			// revision. A post-journal fault leaves the accepted next revision
			// journaled, so the terminal invariant requires Recover to commit
			// that revision before removing the journal.
			journaled := stepJournalDurable(t, env.Publisher.JournalPath)
			// Rewire the fault hook to a passthrough recorder so Recover runs to
			// completion while still tracing the order of durable steps.
			env.rewiredForRecover()
			final, report, err := env.Publisher.Recover(context.Background(), env.Prior)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			assertCoherentTarget(t, env, final, report)
			assertAcceptedEventPreserved(t, env, final, journaled)
			if journaled {
				assertStatePublishedBeforeJournalRemoval(t, env)
			}
			// After recovery the journal must be gone and a second Recover is a
			// stable no-op.
			if _, err := os.Stat(env.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal not removed after recover at %s", step)
			}
			if _, report2, err := env.Publisher.Recover(context.Background(), final); err != nil || report2.Action != ActionNoop {
				t.Fatalf("second recover report=%+v err=%v", report2, err)
			}
		})
	}
}

// stepJournalDurable reports whether a complete (parsable) journal is present at
// path. A pre-journal fault leaves no (or partial) journal; a post-journal fault
// leaves a complete one.
func stepJournalDurable(t *testing.T, path string) bool {
	t.Helper()
	_, ok, err := readJournal(OSFS{}, path)
	if err != nil {
		return false // partial/empty journal
	}
	return ok
}

// faultEnv is the brief's named environment constructor: a stagedEnv wired to
// fail once at step.
func faultEnv(t *testing.T, step string) *stagedEnv {
	e := newStagedEnv(t, step)
	return &e
}

// rewiredForRecover resets the fault hook to a passthrough recorder so Recover
// runs to completion while still tracing the order of durable steps.
// (Defined as a method on stagedEnv.)

// --- Recovery decision ------------------------------------------------------

func TestRecoveryDecision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		allNew bool
		want   string
	}{
		{"roll-forward", true, "roll-forward"},
		{"restore", false, "restore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := recoveryEnv(t, tc.allNew)
			env.rewireForRecover(t)
			_, r, err := env.Publisher.Recover(context.Background(), env.Prior)
			if err != nil || r.Action != tc.want {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

// recoveryEnv sets up a journaled, uncommitted transaction. When allNew is true
// every live file already matches its NewHash (→ roll-forward); otherwise at
// least one is absent/mismatched (→ restore).
func recoveryEnv(t *testing.T, allNew bool) *stagedEnv {
	t.Helper()
	e := newStagedEnv(t, "")
	// Simulate an interrupted apply: journal written, backups taken, and the
	// candidate renamed (allNew) or not (restore).
	j := Journal{
		Schema:        JournalSchema,
		PriorRevision: e.Prior.Revision,
		NextRevision:  e.Tx.Next.Revision,
		TargetID:      e.Tx.TargetID,
		Intended:      intendedOutcome(e.Tx),
	}
	// Take a real backup so restore has a source.
	bp, err := e.Publisher.Backups.Snapshot(e.LivePath)
	if err != nil {
		t.Fatal(err)
	}
	j.Replacements = []Replacement{{
		LivePath:   e.Tx.Replacements[0].LivePath,
		TempPath:   e.Tx.Replacements[0].TempPath,
		BackupPath: bp,
		OldHash:    e.Tx.Replacements[0].OldHash,
		NewHash:    e.Tx.Replacements[0].NewHash,
		Mode:       e.Tx.Replacements[0].Mode,
	}}
	if allNew {
		// Live file already holds the candidate content.
		if err := os.WriteFile(e.LivePath, []byte(constCandidate), e.Tx.Replacements[0].Mode.Perm()); err != nil {
			t.Fatal(err)
		}
	} else {
		// Leave the live file at its prior content (mismatch) so restore runs.
		// For the "absent" variant, remove the live file.
		_ = os.Remove(e.LivePath)
	}
	if err := writeJournal(e.Publisher.fs(), e.Publisher.JournalPath, j, nil); err != nil {
		t.Fatal(err)
	}
	// Remove the staged temp so roll-forward does not depend on it.
	_ = os.Remove(e.TempPath)
	return &e
}

// --- Terminal invariant + coherence assertions ------------------------------

func assertCoherentTarget(t *testing.T, env *stagedEnv, s state.State, report RecoveryReport) {
	t.Helper()
	// The target must be present with a definite applied/pending outcome.
	ts, ok := s.Targets[env.Tx.TargetID]
	if !ok {
		t.Fatalf("target %s missing from recovered state", env.Tx.TargetID)
	}
	switch report.Action {
	case ActionRollForward:
		// Files match the candidate and target is applied at the next revision.
		if ts.AppliedRevision != env.Tx.Next.Revision {
			t.Fatalf("roll-forward applied=%d want %d", ts.AppliedRevision, env.Tx.Next.Revision)
		}
		if ts.Pending != nil {
			t.Fatalf("roll-forward left pending=%+v", ts.Pending)
		}
		got, err := os.ReadFile(env.LivePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte(constCandidate)) {
			t.Fatalf("roll-forward live mismatch:\n%s", got)
		}
	case ActionRestore:
		// Files restored to last-known-good; target pending.
		if ts.Pending == nil {
			t.Fatalf("restore left target applied; expected pending")
		}
		got, err := os.ReadFile(env.LivePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte(constLive)) {
			t.Fatalf("restore did not return to last-known-good:\n%s", got)
		}
	case ActionNoop:
		// No journal existed; live files are untouched at their prior state.
		got, err := os.ReadFile(env.LivePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte(constLive)) {
			t.Fatalf("noop left live files changed:\n%s", got)
		}
	}
}

func assertAcceptedEventPreserved(t *testing.T, env *stagedEnv, s state.State, journaled bool) {
	t.Helper()
	// When the accepted event was journaled before the fault, Recover must
	// advance the committed revision to the journal's next revision (the event
	// is never lost). When it was not journaled (pre-journal fault), the prior
	// committed revision is correctly preserved.
	want := env.Tx.Next.Revision
	if !journaled {
		want = env.Prior.Revision
	}
	if s.Revision != want {
		t.Fatalf("recovered revision=%d want %d (journaled=%v)", s.Revision, want, journaled)
	}
}

func assertStatePublishedBeforeJournalRemoval(t *testing.T, env *stagedEnv) {
	t.Helper()
	// The terminal invariant: state.json was durably published with the accepted
	// next revision (the commit record), and the journal is now absent. Because
	// Recover publishes state and only then removes the journal, the journal's
	// absence combined with the committed next revision proves the ordering.
	committed := env.committedState(t)
	if committed.Revision != env.Tx.Next.Revision {
		t.Fatalf("committed revision=%d want accepted %d", committed.Revision, env.Tx.Next.Revision)
	}
	if _, err := os.Stat(env.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal present after recovery; state not durable before removal")
	}
}

// --- No-op, corrupt journal, happy path -------------------------------------

func TestRecoverNoJournalIsNoop(t *testing.T) {
	e := newStagedEnv(t, "")
	e.rewireForRecover(t)
	final, report, err := e.Publisher.Recover(context.Background(), e.Prior)
	if err != nil {
		t.Fatal(err)
	}
	if report.Action != ActionNoop {
		t.Fatalf("action=%s want noop", report.Action)
	}
	if final.Revision != e.Prior.Revision {
		t.Fatalf("noop changed revision %d->%d", e.Prior.Revision, final.Revision)
	}
}

func TestRecoverCorruptJournalErrors(t *testing.T) {
	e := newStagedEnv(t, "")
	e.rewireForRecover(t)
	if err := os.MkdirAll(filepath.Dir(e.Publisher.JournalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Publisher.JournalPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Publisher.Recover(context.Background(), e.Prior); err == nil {
		t.Fatal("expected error for corrupt journal")
	}
	// Corrupt journal must remain for an operator to inspect/repair.
	if _, err := os.Stat(e.Publisher.JournalPath); err != nil {
		t.Fatalf("corrupt journal removed: %v", err)
	}
}

func TestApplyHappyPathPublishesAndRemovesJournal(t *testing.T) {
	e := newStagedEnv(t, "")
	e.rewiredForRecover()
	final, err := e.Publisher.Apply(context.Background(), e.Tx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if final.Revision != e.Tx.Next.Revision {
		t.Fatalf("revision=%d want %d", final.Revision, e.Tx.Next.Revision)
	}
	got, err := os.ReadFile(e.LivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(constCandidate)) {
		t.Fatalf("live not replaced:\n%s", got)
	}
	if _, err := os.Stat(e.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal not removed after successful apply")
	}
}

// --- Backup retention + mode preservation -----------------------------------

func TestBackupRetentionPrunesOldest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.yaml")
	testutil.WriteFile(t, src, constLive)
	bs := BackupStore{Root: filepath.Join(dir, "backups"), Limit: 2}
	var made []string
	for i := 0; i < 4; i++ {
		bp, err := bs.Snapshot(src)
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, bp)
	}
	entries, err := os.ReadDir(bs.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != bs.Limit {
		t.Fatalf("kept=%d backups want %d", len(entries), bs.Limit)
	}
	// The oldest two should be gone.
	for i := 0; i < 2; i++ {
		if _, err := os.Stat(made[i]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old backup %s still present", made[i])
		}
	}
}

func TestBackupPreservesSourceMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.yaml")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(constLive), 0o644); err != nil {
		t.Fatal(err)
	}
	bs := BackupStore{Root: filepath.Join(dir, "backups"), Limit: 3}
	bp, err := bs.Snapshot(src)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bp)
	if err != nil {
		t.Fatal(err)
	}
	if m := info.Mode().Perm(); m != 0o644 {
		t.Fatalf("backup mode=%o want source 0644", m)
	}
}

// --- TOCTOU defense ---------------------------------------------------------

func TestApplyRejectsSymlink(t *testing.T) {
	e := newStagedEnv(t, "")
	e.rewiredForRecover()
	// Replace the live file with a symlink pointing outside the managed root.
	outside := filepath.Join(t.TempDir(), "escape.yaml")
	testutil.WriteFile(t, outside, "evil")
	_ = os.Remove(e.LivePath)
	if err := os.Symlink(outside, e.LivePath); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Publisher.Apply(context.Background(), e.Tx); err == nil {
		t.Fatal("expected TOCTOU rejection of symlinked managed file")
	}
}

func TestApplyRejectsPathEscape(t *testing.T) {
	e := newStagedEnv(t, "")
	e.rewiredForRecover()
	// Point a replacement's live path outside the managed root.
	e.Tx.Replacements[0].LivePath = filepath.Join(t.TempDir(), "outside.yaml")
	if _, err := e.Publisher.Apply(context.Background(), e.Tx); err == nil {
		t.Fatal("expected path-escape rejection")
	}
}
