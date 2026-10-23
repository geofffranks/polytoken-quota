package publish

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic filesystem history; validates recovery state, not atomic reader snapshots.
func TestGeneratedMultiFilePublishRecoveryHistories(t *testing.T) {
	steps := []string{stepBackup, stepJournalFsync, stepTempWrite, stepTempFsync, stepRename, stepDirFsync, stepProgress, stepStatePublish, stepStateFsync, stepJournalRemove}
	config := readHistoryConfig(t, "PUBLISH_HISTORY_MODE", "PUBLISH_MULTI_SEED", "PUBLISH_MULTI_CASES", 10, 48, 500)
	logHistoryRun(t, config)
	for _, caseSeed := range historySeeds(config) {
		t.Run(fmt.Sprintf("seed-%d", caseSeed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(caseSeed))
			env := newMultiEnv(t)
			env.Publisher.Backups.Limit = 2
			paths, temps := []string{env.LivePath, env.LivePath2}, []string{env.TempPath, env.TempPath2}
			oracle := [][]byte{[]byte(constLive), []byte("polytoken:\n  model: zai/glm\n")}
			revision := env.Prior.Revision
			transactions := len(steps) + rng.Intn(len(steps))
			for n := 0; n < transactions; n++ {
				step := steps[n%len(steps)]
				failAt := 1
				if step == stepRename {
					failAt = 2
				}
				candidates := [][]byte{[]byte(fmt.Sprintf("synthetic primary seed=%d txn=%d nonce=%d\n", caseSeed, n, rng.Intn(1<<20))), []byte(fmt.Sprintf("synthetic secondary seed=%d txn=%d nonce=%d\n", caseSeed, n, rng.Intn(1<<20)))}
				for i := range paths {
					if err := os.WriteFile(paths[i], oracle[i], 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(temps[i], candidates[i], 0o600); err != nil {
						t.Fatal(err)
					}
				}
				prior := env.committedState(t)
				if prior.Revision != revision {
					t.Fatalf("before transaction %d revision=%d want %d", n, prior.Revision, revision)
				}
				next := cloneState(prior)
				next.Revision++
				replacements := make([]Replacement, len(paths))
				for i := range paths {
					replacements[i] = Replacement{LivePath: paths[i], TempPath: temps[i], OldHash: sha256.Sum256(oracle[i]), NewHash: sha256.Sum256(candidates[i]), Mode: fileMode(OSFS{}, paths[i])}
				}
				tx := Transaction{Prior: prior, Next: next, TargetID: "global", ManagedRoot: filepath.Dir(env.LivePath), Replacements: replacements}
				trace := &faultTrace{mu: newChanMutex()}
				occurrences := 0
				env.Publisher.Fault = func(got string) error {
					trace.fire(got)
					if got == step {
						occurrences++
						if occurrences == failAt {
							return ErrInjected
						}
					}
					return nil
				}
				if _, err := env.Publisher.Apply(context.Background(), tx); err == nil {
					t.Fatalf("transaction %d: expected injected %s failure", n, step)
				}
				if occurrences < failAt {
					t.Fatalf("fault %s occurrence %d not reached", step, failAt)
				}
				journaled := step != stepBackup && step != stepJournalFsync
				rollForward := step == stepDirFsync || step == stepProgress || step == stepStatePublish || step == stepStateFsync || step == stepJournalRemove
				wantRevision := revision
				wantBytes := oracle
				wantAction := ActionNoop
				if journaled {
					wantRevision = next.Revision
					wantAction = ActionRestore
					if rollForward {
						wantAction = ActionRollForward
						wantBytes = candidates
					}
					j, ok, err := readJournal(OSFS{}, env.Publisher.JournalPath)
					if err != nil || !ok {
						t.Fatalf("transaction %d fault %s journal ok=%v err=%v", n, step, ok, err)
					}
					for _, r := range j.Replacements {
						backup, readErr := os.ReadFile(r.BackupPath)
						ix := indexOfPath(paths, r.LivePath)
						if readErr != nil || ix < 0 || string(backup) != string(oracle[ix]) {
							t.Fatalf("backup for %s=%q err=%v", r.LivePath, backup, readErr)
						}
					}
				} else if _, err := os.Stat(env.Publisher.JournalPath); !os.IsNotExist(err) {
					t.Fatalf("pre-journal fault left journal: %v", err)
				}
				env.Publisher.Fault = singleShotFault(trace, "")
				final, report, err := env.Publisher.Recover(context.Background(), prior)
				if err != nil {
					t.Fatalf("Recover after %s: %v", step, err)
				}
				if report.Action != wantAction || final.Revision != wantRevision {
					t.Fatalf("fault %s action/revision=%s/%d want %s/%d", step, report.Action, final.Revision, wantAction, wantRevision)
				}
				for i, path := range paths {
					got, err := os.ReadFile(path)
					if err != nil || string(got) != string(wantBytes[i]) {
						t.Fatalf("fault %s file %d=%q err=%v", step, i, got, err)
					}
				}
				persisted := env.committedState(t)
				if persisted.Revision != wantRevision {
					t.Fatalf("persisted revision=%d want %d", persisted.Revision, wantRevision)
				}
				if journaled {
					target := final.Targets["global"]
					applied := prior.Targets["global"].AppliedRevision
					if rollForward {
						applied = next.Revision
					}
					if target.AttemptedRevision != next.Revision || target.AppliedRevision != applied || (target.Pending != nil) == rollForward {
						t.Fatalf("target=%+v applied=%d rollForward=%v", target, applied, rollForward)
					}
					revision = next.Revision
					if rollForward {
						oracle = candidates
					}
				}
				if len(backupPaths(t, env.Publisher.Backups.Root)) > 4 {
					t.Fatal("backup retention limit exceeded")
				}
				if _, again, err := env.Publisher.Recover(context.Background(), final); err != nil || again.Action != ActionNoop {
					t.Fatalf("repeated recovery=%+v err=%v", again, err)
				}
			}
		})
	}
}

func indexOfPath(paths []string, path string) int {
	for i, candidate := range paths {
		if candidate == path {
			return i
		}
	}
	return -1
}
