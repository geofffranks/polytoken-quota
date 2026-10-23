package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This discovery test asserts final recovery, not a multi-file atomic snapshot.
func TestGeneratedMultiFileRenameRecoveryDiscovery(t *testing.T) {
	config := readHistoryConfig(t, "PUBLISH_HISTORY_MODE", "PUBLISH_MULTI_SEED", "PUBLISH_MULTI_CASES", 3, 20, 500)
	if os.Getenv("PUBLISH_MULTI_SEED") == "" {
		config.Seed = 101
	}
	logHistoryRun(t, config)
	for _, seed := range historySeeds(config) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			count := 2 + rng.Intn(2)
			cycles := 2 + rng.Intn(3)
			for cycle := 0; cycle < cycles; cycle++ {
				for failAt := 1; failAt <= count; failAt++ {
					runGeneratedRecoveryCase(t, rng, count, cycle, stepRename, failAt)
				}
				for _, step := range []string{stepDirFsync, stepProgress, stepStatePublish, stepStateFsync, stepJournalRemove} {
					runGeneratedRecoveryCase(t, rng, count, cycle, step, 1)
				}
			}
		})
	}
}

func runGeneratedRecoveryCase(t *testing.T, rng *rand.Rand, count, cycle int, faultStep string, failAt int) {
	t.Helper()
	env := newMultiEnv(t)
	paths := []string{env.LivePath, env.LivePath2}
	temps := []string{env.TempPath, env.TempPath2}
	priorBytes := [][]byte{[]byte(constLive), []byte("polytoken:\n  model: zai/glm\n")}
	for i := 2; i < count; i++ {
		path := filepath.Join(filepath.Dir(env.LivePath), "defs", fmt.Sprintf("extra-%d.yaml", i))
		temp := filepath.Join(filepath.Dir(env.LivePath), fmt.Sprintf(".candidate-extra-%d.yaml", i))
		old := []byte(fmt.Sprintf("synthetic old cycle=%d file=%d\n", cycle, i))
		if err := os.WriteFile(path, old, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(temp, []byte("placeholder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths, temps, priorBytes = append(paths, path), append(temps, temp), append(priorBytes, old)
	}
	candidates := make([][]byte, count)
	replacements := make([]Replacement, count)
	for i := range paths {
		candidates[i] = []byte(fmt.Sprintf("candidate seed=%d cycle=%d file=%d nonce=%d\n", rng.Int63(), cycle, i, rng.Intn(1<<20)))
		if err := os.WriteFile(paths[i], priorBytes[i], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(temps[i], candidates[i], 0o600); err != nil {
			t.Fatal(err)
		}
		replacements[i] = Replacement{LivePath: paths[i], TempPath: temps[i], OldHash: sha256.Sum256(priorBytes[i]), NewHash: sha256.Sum256(candidates[i]), Mode: fileMode(OSFS{}, paths[i])}
	}
	prior := env.committedState(t)
	next := cloneState(prior)
	next.Revision++
	tx := Transaction{Prior: prior, Next: next, TargetID: "global", ManagedRoot: filepath.Dir(env.LivePath), Replacements: replacements}
	seen := 0
	env.Publisher.Fault = func(got string) error {
		if got == faultStep {
			seen++
			if seen == failAt {
				return ErrInjected
			}
		}
		return nil
	}
	if _, err := env.Publisher.Apply(context.Background(), tx); err == nil {
		t.Fatalf("cycle=%d files=%d fault=%s occurrence=%d: expected failure", cycle, count, faultStep, failAt)
	}
	if seen < failAt {
		t.Fatalf("fault %s occurrence %d not reached", faultStep, failAt)
	}
	rollForward := faultStep != stepRename && faultStep != stepBackup && faultStep != stepJournalFsync && faultStep != stepTempWrite && faultStep != stepTempFsync
	wantAction, wantBytes, wantRevision := ActionRestore, priorBytes, prior.Revision+1
	if rollForward {
		wantAction, wantBytes = ActionRollForward, candidates
	}
	trace := &faultTrace{mu: newChanMutex()}
	env.Publisher.Fault = singleShotFault(trace, "")
	final, report, err := env.Publisher.Recover(context.Background(), prior)
	if err != nil {
		t.Fatalf("Recover after %s[%d]: %v", faultStep, failAt, err)
	}
	if report.Action != wantAction || final.Revision != wantRevision {
		t.Fatalf("fault %s[%d] action/revision=%s/%d want %s/%d", faultStep, failAt, report.Action, final.Revision, wantAction, wantRevision)
	}
	for i, path := range paths {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, wantBytes[i]) {
			t.Fatalf("fault %s[%d] file %d=%q err=%v", faultStep, failAt, i, got, err)
		}
	}
	persisted := env.committedState(t)
	if persisted.Revision != wantRevision {
		t.Fatalf("persisted revision=%d want %d", persisted.Revision, wantRevision)
	}
	target := final.Targets["global"]
	applied := prior.Targets["global"].AppliedRevision
	if rollForward {
		applied = next.Revision
	}
	if target.AttemptedRevision != next.Revision || target.AppliedRevision != applied || (target.Pending != nil) == rollForward {
		t.Fatalf("target=%+v applied=%d rollForward=%v", target, applied, rollForward)
	}
	again, repeated, err := env.Publisher.Recover(context.Background(), final)
	if err != nil || repeated.Action != ActionNoop || again.Revision != final.Revision {
		t.Fatalf("repeated recovery=%+v revision=%d err=%v", repeated, again.Revision, err)
	}
	secondCandidates := make([][]byte, count)
	secondReplacements := make([]Replacement, count)
	for i := range paths {
		secondCandidates[i] = []byte(fmt.Sprintf("next cycle seed=%d cycle=%d file=%d nonce=%d\n", rng.Int63(), cycle, i, rng.Intn(1<<20)))
		if err := os.WriteFile(temps[i], secondCandidates[i], 0o600); err != nil {
			t.Fatal(err)
		}
		secondReplacements[i] = Replacement{LivePath: paths[i], TempPath: temps[i], OldHash: sha256.Sum256(wantBytes[i]), NewHash: sha256.Sum256(secondCandidates[i]), Mode: fileMode(OSFS{}, paths[i])}
	}
	secondPrior := env.committedState(t)
	secondNext := cloneState(secondPrior)
	secondNext.Revision++
	secondTx := Transaction{Prior: secondPrior, Next: secondNext, TargetID: "global", ManagedRoot: filepath.Dir(env.LivePath), Replacements: secondReplacements}
	if _, err := env.Publisher.ApplyUnderLock(context.Background(), secondTx); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	secondFinal, secondReport, err := env.Publisher.Recover(context.Background(), secondNext)
	if err != nil || secondReport.Action != ActionNoop || secondFinal.Revision != secondNext.Revision {
		t.Fatalf("second recovery=%+v revision=%d err=%v", secondReport, secondFinal.Revision, err)
	}
	for i, path := range paths {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, secondCandidates[i]) {
			t.Fatalf("second cycle file %d=%q err=%v", i, got, err)
		}
	}
}

type renameBarrierFS struct {
	DurableFS
	livePath         string
	entered, release chan struct{}
	once             bool
}

func (b *renameBarrierFS) Rename(oldpath, newpath string) error {
	if err := b.DurableFS.Rename(oldpath, newpath); err != nil {
		return err
	}
	if newpath == b.livePath && !b.once {
		b.once = true
		close(b.entered)
		<-b.release
	}
	return nil
}

// Reader sees whole individual files but may observe a mixed multi-file pair.
func TestConcurrentReaderAtRenameBarrierDiscovery(t *testing.T) {
	env := newMultiEnv(t)
	barrier := &renameBarrierFS{DurableFS: OSFS{}, livePath: env.LivePath, entered: make(chan struct{}), release: make(chan struct{})}
	env.Publisher.FS = barrier
	applyDone := make(chan error, 1)
	go func() { _, err := env.Publisher.ApplyUnderLock(context.Background(), env.Tx); applyDone <- err }()
	select {
	case <-barrier.entered:
	case err := <-applyDone:
		t.Fatalf("apply stopped before barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rename barrier")
	}
	released := false
	defer func() {
		if !released {
			close(barrier.release)
			select {
			case <-applyDone:
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting after release")
			}
		}
	}()
	first, err := os.ReadFile(env.LivePath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(env.LivePath2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, []byte(constCandidate)) || !bytes.Equal(second, []byte("polytoken:\n  model: zai/glm\n")) {
		t.Fatalf("barrier files=(%q,%q), mixed set permitted", first, second)
	}
	if got := env.committedState(t).Revision; got != env.Prior.Revision {
		t.Fatalf("checkpoint revision=%d want %d", got, env.Prior.Revision)
	}
	close(barrier.release)
	released = true
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for apply")
	}
	if got := env.committedState(t).Revision; got != env.Tx.Next.Revision {
		t.Fatalf("final revision=%d want %d", got, env.Tx.Next.Revision)
	}
}
