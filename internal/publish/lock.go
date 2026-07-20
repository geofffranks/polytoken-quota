package publish

import "context"

// Locker serializes mutating invocations. Lock blocks until the advisory lock is
// acquired or ctx is cancelled, returning an unlock function that releases the
// lock. The returned error wraps ctx.Err() (context.DeadlineExceeded or
// context.Canceled) when the wait is bounded out.
type Locker interface {
	Lock(context.Context) (func() error, error)
}
