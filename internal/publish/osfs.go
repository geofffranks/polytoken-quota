package publish

import (
	"io/fs"
	"os"
	"path/filepath"
)

// OSFS is the production DurableFS backed by the real os package. All durable
// steps — temp write, fsync, atomic rename, parent-dir fsync — go through real
// syscalls. It is the zero value of OSFS.
type OSFS struct{}

// MkdirAll creates path (and parents) with perm.
func (OSFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

// Stat reports the file info for name.
func (OSFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// ReadFile reads name in full.
func (OSFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// WriteFile writes data to name with perm (not necessarily durable; pair with
// an explicit temp + Fsync + Rename for crash-safe writes).
func (OSFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// CreateTemp creates and opens a new temp file in dir with pattern, returning
// the open file.
func (OSFS) CreateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// Open opens name for read/write and returns the open file.
func (OSFS) Open(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR, 0)
}

// Fsync flushes f's data and metadata to stable storage.
func (OSFS) Fsync(f *os.File) error { return f.Sync() }

// Rename atomically renames oldpath to newpath within the same directory.
func (OSFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// SyncDir opens dir and fsyncs it, making directory-entry changes (rename,
// create, unlink) durable. The open file descriptor is closed before returning.
func (OSFS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// RemoveAll removes path and any children.
func (OSFS) RemoveAll(path string) error { return os.RemoveAll(path) }

// parentDir returns the directory of path, cleaned. errStep lives in helpers.go.
func parentDir(path string) string { return filepath.Dir(filepath.Clean(path)) }
