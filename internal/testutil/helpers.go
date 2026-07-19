// Package testutil provides shared, synthetic-data test helpers for the
// polytoken-quota reconciler: filesystem fixture writers, directory snapshots,
// a deterministic fake clock, a command spy, and a fault-injecting filesystem.
//
// Every helper writes only below the caller-supplied root (typically
// t.TempDir()). Fixtures are synthetic and non-personal; no real secrets,
// provider keys, or live configuration are ever embedded.
package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// DirPerm and FilePerm are the restrictive permissions used for staging and
// fixture files: private directories (0700) and private files (0600).
const (
	DirPerm  os.FileMode = 0o700
	FilePerm os.FileMode = 0o600
)

// WriteFile writes content to path, creating any missing parent directories
// with DirPerm (0700) and the file itself with FilePerm (0600). It fails the
// test on any I/O error. Use it to lay down synthetic fixtures below
// t.TempDir().
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), DirPerm); err != nil {
		t.Fatalf("testutil: mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), FilePerm); err != nil {
		t.Fatalf("testutil: write %s: %v", path, err)
	}
}

// FileSnapshot captures a file's content and permission bits at a moment in
// time, for before/after byte-identity comparisons.
type FileSnapshot struct {
	Content []byte
	Mode    os.FileMode
}

// Equal reports whether two snapshots have identical content and mode.
func (s FileSnapshot) Equal(o FileSnapshot) bool {
	if len(s.Content) != len(o.Content) {
		return false
	}
	for i := range s.Content {
		if s.Content[i] != o.Content[i] {
			return false
		}
	}
	return s.Mode.Perm() == o.Mode.Perm()
}

// Snapshot walks root and returns a map of forward-slash-relative path to
// FileSnapshot for every regular file beneath it. It follows symlinks for
// reading only via os.ReadFile (which errors on dangling links). It fails the
// test on any walk or read error. Use it to prove source directories are
// byte-identical before and after an operation.
func Snapshot(t *testing.T, root string) map[string]FileSnapshot {
	t.Helper()
	out := make(map[string]FileSnapshot)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = FileSnapshot{Content: data, Mode: info.Mode()}
		return nil
	})
	if err != nil {
		t.Fatalf("testutil: snapshot %s: %v", root, err)
	}
	return out
}
