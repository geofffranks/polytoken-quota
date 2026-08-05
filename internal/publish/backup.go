package publish

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// BackupStore manages bounded pre-apply last-known-good snapshots. It preserves
// source file modes and enforces a retention limit by deleting the oldest
// backups beyond the limit. Each live file gets one numbered backup per apply;
// older numbered backups beyond Limit for that live file are pruned.
type BackupStore struct {
	Root  string // directory holding numbered backups
	Limit int    // max backups to keep per live file
}

// Snapshot copies src (a live file about to be replaced) to a fresh numbered
// backup under Root and returns the backup path. The backup preserves the
// source file mode and is written durably (same-directory temp + fsync +
// atomic rename + directory fsync), so a crash after the journal becomes
// durable can never leave a torn backup that a later restore depends on.
// After copying, backups for the same source beyond Limit are pruned
// oldest-first.
func (b BackupStore) Snapshot(src string) (string, error) {
	if b.Root == "" {
		return "", fmt.Errorf("publish: empty backup root")
	}
	if err := os.MkdirAll(b.Root, 0o700); err != nil {
		return "", errStep(stepBackup, err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", errStep(stepBackup, err)
	}
	mode := info.Mode().Perm()
	data, err := os.ReadFile(src)
	if err != nil {
		return "", errStep(stepBackup, err)
	}
	dst := b.freshPath(src)
	if err := writeFileDurable(dst, data, mode); err != nil {
		return "", errStep(stepBackup, err)
	}
	if err := b.prune(src); err != nil {
		return "", errStep(stepBackup, err)
	}
	return dst, nil
}

// Restore copies the backup at backup back over its original live path, then
// removes the backup. The live file is written durably with the backup's
// preserved mode via a same-directory temp + atomic rename, and the
// destination is refused when it is a symlink (the restore target must be the
// managed file itself, never a redirection).
func (b BackupStore) Restore(backup, live string) error {
	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("publish: read backup %s: %w", backup, err)
	}
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(backup); err == nil {
		mode = info.Mode().Perm()
	}
	if info, err := os.Lstat(live); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("publish: restore %s: %w", live, ErrPathEscape)
	}
	if err := writeFileDurable(live, data, mode); err != nil {
		return fmt.Errorf("publish: restore %s: %w", live, err)
	}
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("publish: remove restored backup %s: %w", backup, err)
	}
	return nil
}

// writeFileDurable writes data to path atomically and durably: a temp file in
// path's directory is written, fsync'd, chmod'd to mode, renamed over path,
// and the parent directory is fsync'd so the entry survives a crash.
func writeFileDurable(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".backup-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// freshPath returns a unique numbered backup path for src. It derives a stable
// sanitized base from src and appends a monotonically increasing sequence
// number (one greater than the highest existing sequence for that base), so
// pruned gaps are never reused and retention ordering is stable.
func (b BackupStore) freshPath(src string) string {
	base := sanitizeBase(src)
	next := b.nextSeq(base)
	return filepath.Join(b.Root, fmt.Sprintf("%s.%d", base, next))
}

// nextSeq returns one greater than the highest existing sequence number for
// base, or 1 if none exist.
func (b BackupStore) nextSeq(base string) int {
	entries, err := os.ReadDir(b.Root)
	if err != nil {
		return 1
	}
	prefix := base + "."
	highest := 0
	for _, e := range entries {
		name := e.Name()
		if !startsWith(name, prefix) {
			continue
		}
		if n, ok := parseSeq(name[len(prefix):]); ok && n > highest {
			highest = n
		}
	}
	return highest + 1
}

// prune keeps at most Limit backups for the same source base, deleting the
// oldest (lowest sequence number) beyond the limit.
func (b BackupStore) prune(src string) error {
	if b.Limit <= 0 {
		return nil
	}
	base := sanitizeBase(src)
	entries, err := os.ReadDir(b.Root)
	if err != nil {
		return err
	}
	var seqs []seqEntry
	prefix := base + "."
	for _, e := range entries {
		name := e.Name()
		if !startsWith(name, prefix) {
			continue
		}
		n, ok := parseSeq(name[len(prefix):])
		if !ok {
			continue
		}
		seqs = append(seqs, seqEntry{path: filepath.Join(b.Root, name), seq: n})
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i].seq > seqs[j].seq })
	for i := b.Limit; i < len(seqs); i++ {
		// Newest are first; drop the tail (oldest).
		if err := os.Remove(seqs[i].path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

type seqEntry struct {
	path string
	seq  int
}

func parseSeq(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// sanitizeBase reduces an absolute live path to a filesystem-safe backup base
// name, preserving enough structure to stay readable while staying under
// Root. Path separators become '+'. The readable form alone is not injective
// (distinct paths can sanitize to the same bytes, which would let retention
// pruning for one source delete another source's backups), so a short digest
// of the exact cleaned path is appended to make the base unique per source.
func sanitizeBase(p string) string {
	clean := filepath.Clean(p)
	var b []byte
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		switch {
		case c == '/' || c == '\\':
			b = append(b, '+')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-' || c == '_' || c == '.':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	s := string(b)
	if s == "" {
		s = "backup"
	}
	// Bound the readable prefix so the final name (plus digest and sequence
	// number) stays within common 255-byte filename limits for deep paths.
	// The digest keeps uniqueness even when the prefix is truncated.
	const maxReadable = 180
	if len(s) > maxReadable {
		s = s[len(s)-maxReadable:]
	}
	sum := sha256.Sum256([]byte(clean))
	return s + "~" + hexEncode(sum[:6])
}
