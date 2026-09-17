package service

// Verbose threading tests: the reconcile Verbose flag must reach per-target
// processing so decision traces populate for EVERY target outcome — applied
// and pending alike (the historical defect hardcoded verbose=false, so
// --verbose printed "(no trace data)" for all targets).

import (
	"context"
	"testing"
)

// TestVerbosePopulatesTraceForAppliedAndPending proves --verbose populates
// Trace for applied targets (real publish path) and pending targets
// (validation failure), and that quiet runs still carry no trace.
func TestVerbosePopulatesTraceForAppliedAndPending(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		coord := newHarnessWithRunner(t, fakeCommandRunner{})
		out := coord.Reconcile(context.Background(), false, false, true)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("expected an accepted applied reconcile, got accepted=%v pending=%d err=%v", out.Accepted, out.PendingCount(), out.Error)
		}
		for _, o := range out.Targets {
			if o.Trace == nil {
				t.Fatalf("verbose applied target %q carried no trace", o.TargetID)
			}
		}
	})

	t.Run("pending", func(t *testing.T) {
		coord := newHarnessWithRunner(t, doctorFailingRunner{})
		out := coord.Reconcile(context.Background(), false, false, true)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("expected one pending target, got accepted=%v pending=%d err=%v", out.Accepted, out.PendingCount(), out.Error)
		}
		for _, o := range out.Targets {
			if o.Pending != nil && o.Trace == nil {
				t.Fatalf("verbose pending target %q carried no trace", o.TargetID)
			}
		}
	})

	t.Run("dry_run_pending", func(t *testing.T) {
		coord := newHarnessWithRunner(t, doctorFailingRunner{})
		out := coord.Reconcile(context.Background(), true, false, true)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("expected one pending dry-run target, got accepted=%v pending=%d err=%v", out.Accepted, out.PendingCount(), out.Error)
		}
		for _, o := range out.Targets {
			if o.Pending != nil && o.Trace == nil {
				t.Fatalf("verbose dry-run pending target %q carried no trace", o.TargetID)
			}
		}
	})

	t.Run("quiet_has_no_trace", func(t *testing.T) {
		coord := newHarnessWithRunner(t, fakeCommandRunner{})
		out := coord.Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("expected an accepted applied reconcile, got accepted=%v pending=%d err=%v", out.Accepted, out.PendingCount(), out.Error)
		}
		for _, o := range out.Targets {
			if o.Trace != nil {
				t.Fatalf("quiet applied target %q carried a trace", o.TargetID)
			}
		}
	})
}
