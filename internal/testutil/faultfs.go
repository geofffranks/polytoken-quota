package testutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FaultFS is a thin filesystem wrapper that can inject failures for testing
// error and cleanup paths. It wraps the os operations a producer of staging or
// validation artifacts performs. When FailAfter is non-zero, the Nth operation
// after FailAfter calls returns the injected error instead of touching the
// filesystem. Operations are counted across all methods.
//
// Callers wire it in by routing their filesystem access through FaultFS methods
// rather than the os package directly; production code keeps using os directly.
type FaultFS struct {
	// FailAfter is the number of permitted operations before injection begins.
	// A value of 0 (the zero value) disables injection.
	FailAfter int

	mu    sync.Mutex
	count int
}

// ErrFaultInjected is returned when FaultFS injects a failure.
var ErrFaultInjected = errors.New("faultfs: injected failure")

func (f *FaultFS) trip() error {
	if f == nil || f.FailAfter == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.count > f.FailAfter {
		return ErrFaultInjected
	}
	return nil
}

// MkdirAll creates path (and parents) with perm, or injects a failure.
func (f *FaultFS) MkdirAll(path string, perm os.FileMode) error {
	if err := f.trip(); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

// WriteFile writes data to name with perm, or injects a failure.
func (f *FaultFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if err := f.trip(); err != nil {
		return err
	}
	return os.WriteFile(name, data, perm)
}

// ReadFile reads name, or injects a failure.
func (f *FaultFS) ReadFile(name string) ([]byte, error) {
	if err := f.trip(); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// RemoveAll removes path, or injects a failure.
func (f *FaultFS) RemoveAll(path string) error {
	if err := f.trip(); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// Stat reports the fs.FileInfo for name, or injects a failure.
func (f *FaultFS) Stat(name string) (fs.FileInfo, error) {
	if err := f.trip(); err != nil {
		return nil, err
	}
	return os.Stat(name)
}

// CopyTree copies every regular file under src into dst, recreating the
// directory layout. It fails the copy (and returns the error) on the first
// injected fault or I/O error. Used to set up staging roots under a FaultFS.
func (f *FaultFS) CopyTree(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return f.MkdirAll(target, DirPerm)
		}
		data, rerr := f.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return f.WriteFile(target, data, FilePerm)
	})
}
