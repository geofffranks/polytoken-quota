package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// coordinatorNilClock returns the fixture coordinator with Clock explicitly
// nil, so every clock read falls back to real time.Now() via Coordinator.now.
func coordinatorNilClock(f *gateFixture, pub Publisher) *Coordinator {
	c := f.coordinatorWith(pub)
	c.Clock = nil
	return c
}

// TestProviderGateReconcileWithoutClock drives the full reconcile path through
// the gate fixture with Coordinator.Clock left at its zero value (nil). The
// provider gate used to call c.Clock.Now() directly (provider_gate.go:216,
// "signal-verdicts" step), which panicked on the nil interface; the rest of the
// coordinator already falls back to time.Now() inside Coordinator.now()
// (coordinator.go). The gate must reuse that fallback.
//
// Expected outcomes are freshness-INDEPENDENT: nothing here keys on historical
// timestamps in the seeded durable state (the fixture state carries no expiry,
// so seed values alone determine the verdict regardless of the real wall clock
// the nil-clock fallback returns).
//
// Fixtures are complete standalone private synthetic staging roots under
// t.TempDir(); nothing touches live configuration, accounts, credentials, or
// daemons.
func TestProviderGateReconcileWithoutClock(t *testing.T) {
	// A low quota observation drives the classic reserve gate: the reconciler
	// should gate gp off (enabled: true -> enabled: false) and publish it.
	seed := map[string]state.ProviderState{
		"gp": {Quota: state.QuotaLow, Availability: state.Available},
	}

	t.Run("normal publishes the gated-off edit and commits state", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		before := f.readGlobalConfig()
		f.seedState(1, seed, nil)

		out := coordinatorNilClock(f, PublisherAdapter{Publisher: f.publisher()}).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		after := f.readGlobalConfig()
		want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		if after != want {
			t.Fatalf("config bytes wrong:\n--- got ---\n%s\n--- want ---\n%s", after, want)
		}
		// Pertinent durable-state assertion: the same seed as the existing
		// baseline must yield the same ownership claim from the nil-clock path.
		st := f.loadState()
		own, ok := st.ProviderOwnership["gp"]
		if !ok || !own.Owned || !own.BaselinePresent || !own.BaselineValue {
			t.Fatalf("ownership=%+v want the gate claim recorded without a clock", own)
		}
		if got := len(st.ReconcileHistory.Records); got != 1 {
			t.Fatalf("history records=%d want 1", got)
		}
		if f.journalExists() {
			t.Fatal("journal left behind after a committed publish")
		}
		if _, err := os.Stat(f.desired.Operational.NoticePath); err != nil {
			t.Fatalf("provider notice not published after accepted reconcile: %v", err)
		}
	})

	t.Run("dry-run edits nothing, publishes nothing, and does not save state", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		before := f.readGlobalConfig()
		f.seedState(9, seed, nil)
		counting := &gateCountingPublisher{PublisherAdapter: PublisherAdapter{Publisher: f.publisher()}}

		out := coordinatorNilClock(f, counting).Reconcile(context.Background(), true, false, false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if out.Revision != 9 {
			t.Fatalf("dry-run advanced the revision to %d", out.Revision)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatalf("dry-run edited the live config:\n%s", got)
		}
		if counting.applies != 0 {
			t.Fatalf("dry-run published %d times", counting.applies)
		}
		if f.journalExists() {
			t.Fatal("dry-run wrote a journal")
		}
		st := f.loadState()
		if st.Revision != 9 || len(st.ReconcileHistory.Records) != 0 {
			t.Fatalf("dry-run mutated state: revision=%d history=%d", st.Revision, len(st.ReconcileHistory.Records))
		}
		if len(st.ProviderOwnership) != 0 {
			t.Fatalf("dry-run recorded ownership claims: %+v", st.ProviderOwnership)
		}
		// The evaluation itself is real: every registered root was staged.
		joined := strings.Join(f.runner.calls, "\n")
		if !strings.Contains(joined, "quota-stage-global") {
			t.Fatalf("dry-run did not stage/validate the global root:\n%s", joined)
		}
	})

	t.Run("gate did not consult the injected clock interface for time", func(t *testing.T) {
		// Publication adapter sanity: the nil-clock normal path must delegate to
		// the real publisher exactly once per accepted transaction.
		f := newGateFixture(t, []string{"gp"}, nil)
		f.seedState(3, seed, nil)
		counting := &gateCountingPublisher{PublisherAdapter: PublisherAdapter{Publisher: f.publisher()}}
		before := f.readGlobalConfig()

		out := coordinatorNilClock(f, counting).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if counting.applies != 1 {
			t.Fatalf("normal path applied %d transactions, want exactly 1", counting.applies)
		}
		want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		if got := f.readGlobalConfig(); got != want {
			t.Fatalf("config bytes wrong without clock:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
	})
}
