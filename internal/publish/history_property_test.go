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

// These histories use private synthetic staging roots and observe files only at
// operation boundaries; they do not promise multi-file atomic reader snapshots.
func TestGeneratedPublishFaultRecoveryHistories(t *testing.T) {
	steps := []string{stepBackup, stepJournalFsync, stepTempWrite, stepTempFsync, stepRename, stepDirFsync, stepProgress, stepStatePublish, stepStateFsync, stepJournalRemove}
	config := readHistoryConfig(t, "PUBLISH_HISTORY_MODE", "PUBLISH_HISTORY_SEED", "PUBLISH_HISTORY_CASES", 10, 80, 500)
	logHistoryRun(t, config)
	for _, caseSeed := range historySeeds(config) {
		t.Run(fmt.Sprintf("seed-%d", caseSeed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(caseSeed))
			env := newStagedEnv(t, "")
			env.Publisher.Backups.Limit = 1
			oracleBytes := []byte(constLive)
			committedRevision := env.Prior.Revision
			transactions := 1 + rng.Intn(5)
			caseIndex := int(caseSeed - config.Seed)
			step := steps[caseIndex%len(steps)]
			if config.Mode == "full" && caseIndex >= len(steps) {
				step = steps[rng.Intn(len(steps))]
			}
			for n := 0; n < transactions; n++ {
				candidate := []byte(fmt.Sprintf("synthetic revision %d seed %d\n", n+1, caseSeed))
				if err := os.WriteFile(env.LivePath, oracleBytes, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(env.TempPath, candidate, 0o600); err != nil {
					t.Fatal(err)
				}
				prior := env.committedState(t)
				if prior.Revision != committedRevision {
					t.Fatalf("before transaction %d revision=%d want %d", n, prior.Revision, committedRevision)
				}
				next := cloneState(prior)
				next.Revision++
				oldHash, newHash := sha256.Sum256(oracleBytes), sha256.Sum256(candidate)
				mode := fileMode(OSFS{}, env.LivePath)
				tx := Transaction{Prior: prior, Next: next, TargetID: "global", ManagedRoot: filepath.Dir(env.LivePath), Replacements: []Replacement{{LivePath: env.LivePath, TempPath: env.TempPath, OldHash: oldHash, NewHash: newHash, Mode: mode}}}
				trace := &faultTrace{mu: newChanMutex()}
				env.Publisher.Fault = singleShotFault(trace, step)
				if _, err := env.Publisher.Apply(context.Background(), tx); err == nil {
					t.Fatalf("transaction %d: expected injected %s failure", n, step)
				}
				journaled := step != stepBackup && step != stepJournalFsync
				wantBytes := oracleBytes
				wantRevision := committedRevision
				wantApplied := false
				if journaled {
					wantRevision = next.Revision
					if step == stepDirFsync || step == stepProgress || step == stepStatePublish || step == stepStateFsync || step == stepJournalRemove {
						wantBytes, wantApplied = candidate, true
					}
				}
				if journaled {
					j, ok, err := readJournal(OSFS{}, env.Publisher.JournalPath)
					if err != nil || !ok {
						t.Fatalf("transaction %d fault %s: journal ok=%v err=%v", n, step, ok, err)
					}
					backups := backupPaths(t, env.Publisher.Backups.Root)
					if len(backups) != 1 || j.Replacements[0].BackupPath != backups[0] {
						t.Fatalf("transaction %d fault %s: journal backup %q, retained %v", n, step, j.Replacements[0].BackupPath, backups)
					}
					backup, err := os.ReadFile(j.Replacements[0].BackupPath)
					if err != nil || string(backup) != string(oracleBytes) {
						t.Fatalf("transaction %d fault %s: backup %q err=%v want %q", n, step, backup, err, oracleBytes)
					}
				} else if _, err := os.Stat(env.Publisher.JournalPath); !os.IsNotExist(err) {
					t.Fatalf("transaction %d pre-journal fault %s left journal: %v", n, step, err)
				}
				env.rewiredForRecover()
				final, report, err := env.Publisher.Recover(context.Background(), prior)
				if err != nil {
					t.Fatalf("transaction %d recover after %s: %v", n, step, err)
				}
				wantAction := ActionNoop
				if journaled {
					wantAction = ActionRestore
					if wantApplied {
						wantAction = ActionRollForward
					}
				}
				if report.Action != wantAction || final.Revision != wantRevision {
					t.Fatalf("transaction %d fault %s: action/revision=%s/%d want %s/%d", n, step, report.Action, final.Revision, wantAction, wantRevision)
				}
				got, err := os.ReadFile(env.LivePath)
				if err != nil || string(got) != string(wantBytes) {
					t.Fatalf("transaction %d fault %s: live=%q err=%v want %q", n, step, got, err, wantBytes)
				}
				persisted := env.committedState(t)
				if persisted.Revision != wantRevision {
					t.Fatalf("transaction %d persisted revision=%d want %d", n, persisted.Revision, wantRevision)
				}
				if journaled {
					target := persisted.Targets["global"]
					wantAppliedRevision := prior.Targets["global"].AppliedRevision
					if wantApplied {
						wantAppliedRevision = next.Revision
					}
					if target.AttemptedRevision != next.Revision || target.AppliedRevision != wantAppliedRevision || (target.Pending != nil) == wantApplied {
						t.Fatalf("transaction %d fault %s action %s outcome=%+v; want applied revision=%d pending=%v", n, step, report.Action, target, wantAppliedRevision, !wantApplied)
					}
					committedRevision = next.Revision
					if wantApplied {
						oracleBytes = candidate
					} else {
						oracleBytes = append([]byte(nil), oracleBytes...)
					}
				}
				if len(backupPaths(t, env.Publisher.Backups.Root)) > 1 {
					t.Fatalf("transaction %d exceeded backup retention", n)
				}
				if _, again, err := env.Publisher.Recover(context.Background(), final); err != nil || again.Action != ActionNoop {
					t.Fatalf("transaction %d recovery retry=%+v err=%v, want no-op", n, again, err)
				}
			}
		})
	}
}
