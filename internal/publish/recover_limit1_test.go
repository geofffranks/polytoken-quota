package publish

// AC4 regression test: an interrupted apply at retention limit 1 still
// recovers. The single retained backup must be exactly the journal-referenced
// one (retention keeps the newest and prunes oldest-first, so wiring a smaller
// limit can never drop the backup recovery needs), restore must succeed from
// it, and the live file must converge back to its pre-apply content.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestInterruptedApplyAtLimitOneRecoversViaJournalBackup(t *testing.T) {
	// Fault after the journal is durable but before the live rename: recovery
	// must take the restore-from-backup path.
	env := faultEnv(t, "rename")
	// Retention limit 1: the pre-apply snapshot is the only backup retained.
	env.Publisher.Backups.Limit = 1
	if _, err := env.Publisher.Apply(context.Background(), env.Tx); err == nil {
		t.Fatal("expected Apply to fail at the injected rename step")
	}

	// Retention at limit 1 kept exactly one backup, and it is precisely the
	// one the journal references — the wiring change to a smaller default must
	// never drop the journal-referenced (newest) backup.
	remaining := backupPaths(t, env.Publisher.Backups.Root)
	if len(remaining) != 1 {
		t.Fatalf("backups retained at limit 1 = %d (%v), want 1", len(remaining), remaining)
	}
	j, ok, err := readJournal(OSFS{}, env.Publisher.JournalPath)
	if err != nil || !ok {
		t.Fatalf("read journal after fault: ok=%v err=%v", ok, err)
	}
	if len(j.Replacements) != 1 {
		t.Fatalf("journal replacements=%d want 1", len(j.Replacements))
	}
	if j.Replacements[0].BackupPath != remaining[0] {
		t.Fatalf("journal-referenced backup %s was dropped; only %s remains",
			j.Replacements[0].BackupPath, remaining[0])
	}

	// Recover restores the live file from that single retained backup and
	// converges to the pending last-known-good state.
	env.rewiredForRecover()
	final, report, err := env.Publisher.Recover(context.Background(), env.Prior)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Action != ActionRestore {
		t.Fatalf("recovery action=%s want restore", report.Action)
	}
	got, err := os.ReadFile(env.LivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != constLive {
		t.Fatalf("live file not restored from the single retained backup:\n%s", got)
	}
	if ts := final.Targets["global"]; ts.Pending == nil {
		t.Fatalf("recovered target not marked pending: %+v", ts)
	}
	// The consumed backup is gone and the journal was removed after the
	// restored state was durably committed.
	if left := backupPaths(t, env.Publisher.Backups.Root); len(left) != 0 {
		t.Fatalf("restored backup not consumed: %v", left)
	}
	if _, err := os.Stat(env.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal not removed after restore: %v", err)
	}
}

// backupPaths lists the full paths of every backup file under root, sorted.
func backupPaths(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, filepath.Join(root, e.Name()))
	}
	sort.Strings(paths)
	return paths
}
