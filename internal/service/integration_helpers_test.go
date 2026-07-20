package service

// Shared test helpers for the Task 14 integration suites in the service
// package. These provide a real advisory-flock-backed Locker (mirroring the
// production lock so integration tests exercise genuine OS serialization) and a
// small SHA-256 helper, without depending on unexported publish helpers.

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
)

// realFlock is a real advisory exclusive file lock backed by flock(2), mirroring
// the production lock so the integration suites exercise genuine OS-level
// serialization. It is a test double only in that it lives here; the syscalls are
// real. On any supported platform (darwin/linux) flock(2) LOCK_EX is available.
type realFlock struct {
	path string
}

func (l realFlock) Lock(context.Context) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// BLOCKING exclusive acquire; tests run few short-lived transactions.
	return realFlockAcquire(f)
}

// sha256sum returns the SHA-256 of s as a fixed-size array (the digest type used
// by publish.Replacement).
func sha256sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}
