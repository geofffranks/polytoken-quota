//go:build !windows

package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// fileLock is an advisory exclusive file lock backed by flock(2) via
// syscall.Flock. It serializes concurrent mutations with a bounded, cancellable
// wait. The lock file is created with mode 0600. All supported targets
// (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64) are Unix-like, so a
// single implementation guarded by `//go:build !windows` suffices.
type fileLock struct {
	path    string
	poll    time.Duration // interval between lock attempts while waiting
	maxWait time.Duration // hard cap on total wait when ctx has no deadline

	mu     sync.Mutex
	locked bool
	f      *os.File
}

// NewFileLock returns a Locker backed by an advisory flock at path. It is the
// production locker constructor used by the CLI wiring and by tests.
func NewFileLock(path string) Locker {
	return newFileLock(path)
}

// newFileLock returns a Locker backed by an advisory flock at path.
func newFileLock(path string) Locker {
	return &fileLock{path: path, poll: 5 * time.Millisecond, maxWait: 30 * time.Second}
}

// Lock acquires an exclusive advisory lock within ctx. It polls flock(2) with a
// short backoff until the lock is free, ctx is cancelled, or the wait cap is
// reached (whichever comes first). The returned unlock function releases the
// lock and closes the lock file; it is safe to call multiple times.
func (l *fileLock) Lock(ctx context.Context) (func() error, error) {
	// Ensure the lock directory exists and create the lock file with mode 0600.
	// Re-opening each Lock keeps the holder deterministic across processes.
	dir := parentDir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("publish: lock dir: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("publish: open lock: %w", err)
	}

	deadline, hasDeadline := ctx.Deadline()
	maxDeadline := time.Now().Add(l.maxWait)
	if !hasDeadline || deadline.After(maxDeadline) {
		deadline = maxDeadline
		hasDeadline = true
	}

	if err := l.acquire(ctx, f, deadline, hasDeadline); err != nil {
		_ = f.Close()
		// Preserve the underlying context error in the chain so callers can use
		// errors.Is(err, context.DeadlineExceeded|context.Canceled).
		return nil, fmt.Errorf("publish: lock %s: %w", l.path, err)
	}

	l.mu.Lock()
	l.locked = true
	l.f = f
	l.mu.Unlock()

	var once sync.Once
	var firstErr error
	return func() error {
		once.Do(func() {
			l.mu.Lock()
			f := l.f
			l.f = nil
			l.locked = false
			l.mu.Unlock()
			if f == nil {
				return
			}
			// Releasing an flock by closing the fd is the documented, portable
			// way; flock locks are associated with the open file description.
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			firstErr = f.Close()
		})
		return firstErr
	}, nil
}

// acquire polls an exclusive non-blocking flock until it succeeds, ctx expires,
// or deadline passes.
func (l *fileLock) acquire(ctx context.Context, f *os.File, deadline time.Time, hasDeadline bool) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("flock: %w", err)
		}
		if hasDeadline {
			now := time.Now()
			if !now.Before(deadline) {
				return context.DeadlineExceeded
			}
		}
		// Wait one poll interval or until the deadline, whichever is sooner.
		wait := l.poll
		if hasDeadline {
			remaining := time.Until(deadline)
			if remaining < wait {
				wait = remaining
			}
			if wait <= 0 {
				return context.DeadlineExceeded
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
