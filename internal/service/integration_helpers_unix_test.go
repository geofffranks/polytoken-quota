//go:build !windows

package service

import (
	"os"
	"syscall"
)

// realFlockAcquire takes an exclusive flock on the open file f and returns an
// unlock function that releases it and closes f. It blocks until the lock is
// acquired.
func realFlockAcquire(f *os.File) (func() error, error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}
