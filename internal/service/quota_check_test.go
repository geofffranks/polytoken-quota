package service

// QuotaCheck transaction tests (Task 8b). These verify the locked transaction
// flow for the quota check polling path: poll-only observation persistence,
// provider isolation (a failed provider never blocks a valid one), last-good
// snapshot preservation on failure, --reconcile ordering, --provider filtering,
// unsupported adapters, exit-code semantics, no-mutation on rejection, and
// key crash boundaries.
//
// A fake QuotaPoller injects preset snapshots so the transaction logic is
// exercised without the network. The coordinator spy records the operation
// order into Trace via its Tracer.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// fakePoller is a test QuotaPoller that returns preset snapshots keyed by
// mapping ID. It records the provider filter and desired it received. When a
// provider filter is set, only that mapping's result is returned (mirroring the
// production poller's filter so the coordinator logic is exercised correctly).
type fakePoller struct {
	results    map[string]quota.QuotaSnapshot
	gotProv    string
	gotDesired policy.Desired
	pollErr    error
}

func (f *fakePoller) Poll(_ context.Context, desired policy.Desired, provider string, _ time.Time) (map[string]quota.QuotaSnapshot, error) {
	f.gotProv = provider
	f.gotDesired = desired
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	out := make(map[string]quota.QuotaSnapshot, len(f.results))
	for k, v := range f.results {
		if provider != "" && k != provider {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// quotaCheckSpy extends coordinatorSpy with a desired policy carrying quota
// configs and a fake poller. The desired has two quota-enabled mappings.
func newQuotaCheckSpy() *coordinatorSpy {
	spy := newCoordinatorSpy()
	spy.Coordinator.QuotaPoller = &fakePoller{results: map[string]quota.QuotaSnapshot{}}
	return spy
}

// quotaDesired returns a desired policy with two quota-enabled mappings backed
// by real CodExBar provider IDs. LoadPolicy on the spy returns this.
func quotaDesired() policy.Desired {
	return policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				CodexBarProviders:  []string{"codex"},
				PolytokenProviders: []string{"codex"},
				Quota:              &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: 30 * time.Minute},
				Models:             map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
			},
			"zai": {
				CodexBarProviders:  []string{"zai"},
				PolytokenProviders: []string{"zai"},
				Quota:              &policy.QuotaConfig{Adapter: "zai", FreshnessTTL: 30 * time.Minute},
				Models:             map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}},
			},
		},
	}
}

// freshSnap builds a successful snapshot for a mapping with one window.
func freshSnap(mappingID string, used float64) quota.QuotaSnapshot {
	limit := 100.0
	return quota.QuotaSnapshot{
		MappingID:    mappingID,
		CheckedAt:    time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows:      []quota.QuotaWindow{{Name: "daily", Used: &used, Limit: &limit}},
	}
}

// failedSnap builds a failed snapshot for a mapping.
func failedSnap(mappingID, reason string) quota.QuotaSnapshot {
	return quota.QuotaSnapshot{
		MappingID:    mappingID,
		Availability: quota.QuotaUnknown,
		Status:       quota.SourceFailed,
		Error:        reason,
	}
}

func pollerOf(spy *coordinatorSpy) *fakePoller {
	return spy.Coordinator.QuotaPoller.(*fakePoller)
}

// TestQuotaCheckPollOnlyUpdatesBothProviders verifies a clean poll updates both
// providers' QuotaSnapshot and QuotaAttempt and bumps the revision.
func TestQuotaCheckPollOnlyUpdatesBothProviders(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 20)
	p.results["zai"] = freshSnap("zai", 40)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)

	if !out.Accepted || out.Revision != 2 || out.Problem {
		t.Fatalf("outcome: %+v", out)
	}
	saved := spy.LastSaved
	if saved.Providers["codex"].QuotaSnapshot == nil || saved.Providers["codex"].QuotaAttempt == nil {
		t.Fatalf("codex missing snapshot/attempt: %+v", saved.Providers["codex"])
	}
	if saved.Providers["codex"].QuotaSnapshot.Status != quota.SourceFresh {
		t.Fatalf("codex snapshot status=%s", saved.Providers["codex"].QuotaSnapshot.Status)
	}
	if saved.Providers["zai"].QuotaSnapshot == nil || saved.Providers["zai"].QuotaSnapshot.Status != quota.SourceFresh {
		t.Fatalf("zai snapshot missing/not fresh: %+v", saved.Providers["zai"])
	}
	if spy.StateSaves != 1 {
		t.Fatalf("state saves=%d want 1", spy.StateSaves)
	}
}

// TestQuotaCheckIndependentFailurePreservesLastGood verifies a failed provider
// never blocks a valid one: the successful provider's snapshot is updated while
// the failed provider's QuotaSnapshot is PRESERVED and only QuotaAttempt
// reflects the failure. Exit code is pending (Problem=true).
func TestQuotaCheckFailureRecordsEvent(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = failedSnap("codex", "Bearer SECRET account=alice")
	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
	if !out.Accepted || !out.Problem {
		t.Fatalf("out=%+v", out)
	}
	found := false
	for _, e := range spy.LastSaved.EventHistory.Events {
		if e.Category == state.EventQuotaFailure && e.Result == state.EventFailed {
			found = true
			if strings.Contains(e.Reason, "SECRET") {
				t.Fatal("unsanitized failure")
			}
		}
	}
	if !found {
		t.Fatalf("events=%+v", spy.LastSaved.EventHistory)
	}
}

func TestQuotaCheckIndependentFailurePreservesLastGood(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)

	// Seed prior state: codex has a last-good snapshot at revision 1. The seeded
	// publisher returns this state from Recover (the transact path uses the
	// recovered state, not the raw LoadState).
	priorGood := freshSnap("codex", 10)
	seeded := state.State{
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
		Targets: map[string]state.TargetState{},
	}
	spy.Coordinator.State = seededStateStore{state: seeded, spy: spy}
	spy.Coordinator.Publish = seededPublisher{state: seeded}
	// This poll: codex FAILS, zai succeeds.
	p.results["codex"] = failedSnap("codex", "codex: server error (HTTP 500)")
	p.results["zai"] = freshSnap("zai", 30)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)

	if !out.Accepted || !out.Problem {
		t.Fatalf("outcome should be accepted+problem: %+v", out)
	}
	if out.Revision != 2 {
		t.Fatalf("revision=%d want 2", out.Revision)
	}
	saved := spy.LastSaved
	// zai succeeded → snapshot updated.
	if saved.Providers["zai"].QuotaSnapshot == nil || saved.Providers["zai"].QuotaSnapshot.Status != quota.SourceFresh {
		t.Fatalf("zai should have a fresh snapshot: %+v", saved.Providers["zai"])
	}
	// codex failed → QuotaSnapshot PRESERVED (last-good), QuotaAttempt reflects failure.
	if saved.Providers["codex"].QuotaSnapshot == nil ||
		saved.Providers["codex"].QuotaSnapshot.Status != quota.SourceFresh ||
		saved.Providers["codex"].QuotaSnapshot.Windows[0].Used == nil ||
		*saved.Providers["codex"].QuotaSnapshot.Windows[0].Used != 10 {
		t.Fatalf("codex last-good snapshot not preserved: %+v", saved.Providers["codex"].QuotaSnapshot)
	}
	if saved.Providers["codex"].QuotaAttempt == nil || saved.Providers["codex"].QuotaAttempt.Status != quota.SourceFailed {
		t.Fatalf("codex attempt should be failed: %+v", saved.Providers["codex"].QuotaAttempt)
	}
}

// TestQuotaCheckReconcileOrdering verifies the reconcile flow follows
// lock → recover → poll → stage → validate → publish → save-state ordering.
func TestQuotaCheckReconcileOrdering(t *testing.T) {
	spy := newQuotaCheckSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 20)
	p.results["zai"] = freshSnap("zai", 30)

	spy.Coordinator.QuotaCheck(context.Background(), "", true)

	// Verify the relative ordering of key milestones.
	trace := spy.Trace
	idxOf := func(needle string) int {
		for i, s := range trace {
			if s == needle {
				return i
			}
		}
		return -1
	}
	pollIdx := idxOf("poll")
	stageIdx := -1
	validateIdx := -1
	publishIdx := -1
	saveIdx := idxOf("save-state")
	for i, s := range trace {
		if strings.HasPrefix(s, "stage:") && stageIdx == -1 {
			stageIdx = i
		}
		if strings.HasPrefix(s, "validate:") && validateIdx == -1 {
			validateIdx = i
		}
		if strings.HasPrefix(s, "publish:") && publishIdx == -1 {
			publishIdx = i
		}
	}
	milestones := []struct {
		name string
		idx  int
	}{
		{"lock", idxOf("lock")}, {"recover", idxOf("recover")}, {"poll", pollIdx},
		{"stage", stageIdx}, {"validate", validateIdx}, {"publish", publishIdx}, {"save-state", saveIdx},
	}
	for _, m := range milestones {
		if m.idx < 0 {
			t.Fatalf("milestone %s missing from trace: %v", m.name, trace)
		}
	}
	for i := 1; i < len(milestones); i++ {
		if milestones[i].idx <= milestones[i-1].idx {
			t.Fatalf("%s (idx %d) not after %s (idx %d): trace=%v",
				milestones[i].name, milestones[i].idx, milestones[i-1].name, milestones[i-1].idx, trace)
		}
	}
}

// TestQuotaCheckExitCodes verifies exit-code semantics: 0 clean, 2 problem, 1
// rejection (lock failure).
func TestQuotaCheckExitCodes(t *testing.T) {
	t.Run("clean_exit0", func(t *testing.T) {
		spy := newQuotaCheckSpy()
		spy.Coordinator.Policy = quotaCheckPolicyLoader{}
		p := pollerOf(spy)
		p.results["codex"] = freshSnap("codex", 20)
		out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
		if MutationExitCodeOf(out) != 0 {
			t.Fatalf("exit=%d want 0", MutationExitCodeOf(out))
		}
	})
	t.Run("problem_exit2", func(t *testing.T) {
		spy := newQuotaCheckSpy()
		spy.Coordinator.Policy = quotaCheckPolicyLoader{}
		p := pollerOf(spy)
		p.results["codex"] = failedSnap("codex", "boom")
		out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
		if MutationExitCodeOf(out) != 2 {
			t.Fatalf("exit=%d want 2", MutationExitCodeOf(out))
		}
	})
	t.Run("rejection_exit1", func(t *testing.T) {
		spy := newQuotaCheckSpy()
		spy.Coordinator.Policy = quotaCheckPolicyLoader{}
		p := pollerOf(spy)
		p.pollErr = errors.New("catastrophic poll failure")
		out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
		if MutationExitCodeOf(out) != 1 {
			t.Fatalf("exit=%d want 1", MutationExitCodeOf(out))
		}
		if spy.StateSaves != 0 {
			t.Fatalf("rejected invocation saved state: %d", spy.StateSaves)
		}
	})
}

// TestQuotaCheckProviderFilter verifies --provider restricts polling to one
// mapping.
func TestQuotaCheckProviderFilter(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 20)
	p.results["zai"] = freshSnap("zai", 30)

	spy.Coordinator.QuotaCheck(context.Background(), "zai", false)

	if p.gotProv != "zai" {
		t.Fatalf("poller got provider=%q want zai", p.gotProv)
	}
	saved := spy.LastSaved
	// Only zai should have an attempt; codex should be untouched (no attempt).
	if saved.Providers["codex"].QuotaAttempt != nil {
		t.Fatalf("codex should not have been polled: %+v", saved.Providers["codex"])
	}
	if saved.Providers["zai"].QuotaAttempt == nil {
		t.Fatalf("zai should have been polled: %+v", saved.Providers["zai"])
	}
}

// TestQuotaCheckUnsupportedAdapter verifies a provider with a failed (e.g.
// unsupported) snapshot yields Status=failed and exit 2, with no network request
// (the fake returns a failed snapshot directly).
func TestQuotaCheckUnsupportedAdapter(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = failedSnap("codex", "provider codex has no recorded contract evidence; record evidence before enabling")
	p.results["zai"] = freshSnap("zai", 30)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)

	if !out.Accepted || !out.Problem {
		t.Fatalf("outcome should be accepted+problem: %+v", out)
	}
	if MutationExitCodeOf(out) != 2 {
		t.Fatalf("exit=%d want 2", MutationExitCodeOf(out))
	}
	saved := spy.LastSaved
	if saved.Providers["codex"].QuotaAttempt == nil || saved.Providers["codex"].QuotaAttempt.Status != quota.SourceFailed {
		t.Fatalf("codex attempt should be failed: %+v", saved.Providers["codex"].QuotaAttempt)
	}
}

// TestQuotaCheckNoMutationOnError verifies a rejected invocation changes
// nothing.
func TestQuotaCheckNoMutationOnError(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = failingPolicyLoader{}
	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
	if out.Accepted || spy.StateSaves != 0 {
		t.Fatalf("rejected invocation should not mutate: out=%+v saves=%d", out, spy.StateSaves)
	}
}

// TestQuotaCheckNilPollerRejected verifies a missing poller is rejected without
// mutation.
func TestQuotaCheckNilPollerRejected(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	spy.Coordinator.QuotaPoller = nil
	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
	if out.Accepted || out.Error == nil || spy.StateSaves != 0 {
		t.Fatalf("nil poller should be rejected: out=%+v", out)
	}
}

// TestQuotaCheckCrashMatrixAfterPoll verifies that a crash (Store.Fault) after
// the poll but before the final state save leaves no torn state: the last-good
// QuotaSnapshot is preserved and no partial state is committed.
func TestQuotaCheckCrashMatrixAfterPoll(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	priorGood := freshSnap("codex", 10)
	base := state.State{
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
		Targets: map[string]state.TargetState{},
	}
	crashStore := &crashingStateStore{base: base, fault: true}
	spy.Coordinator.State = crashStore
	spy.Coordinator.Publish = seededPublisher{state: base}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 25)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)

	// Poll succeeded, but persistence failed, so the transaction is rejected.
	if out.Accepted || !out.DurabilityFailure || out.Error == nil {
		t.Fatalf("expected rejected durability outcome with save error: %+v", out)
	}
	// No state committed: the on-disk base is unchanged.
	if crashStore.committed != nil {
		t.Fatalf("crash should not commit state: %+v", crashStore.committed)
	}
	// The base state's last-good snapshot is intact (no torn write).
	if crashStore.base.Providers["codex"].QuotaSnapshot == nil ||
		*crashStore.base.Providers["codex"].QuotaSnapshot.Windows[0].Used != 10 {
		t.Fatalf("last-good snapshot corrupted by crash: %+v", crashStore.base.Providers["codex"].QuotaSnapshot)
	}
}

// TestApplyQuotaObservationsDoesNotAliasPriorState is the regression guard for
// the state-aliasing finding (Fix 2): applyQuotaObservations must allocate a
// fresh Providers map so the prior (recovered) state's map is never mutated.
// Previously next shared observed's Providers map.
func TestApplyQuotaObservationsDoesNotAliasPriorState(t *testing.T) {
	priorGood := freshSnap("codex", 10)
	observed := state.State{
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
	}
	desired := quotaDesired()
	attempts := map[string]quota.QuotaSnapshot{"codex": freshSnap("codex", 25)}

	next := applyQuotaObservations(observed, desired, attempts)

	// The prior state must be untouched: its provider entry still holds the old
	// snapshot (used=10), proving no map aliasing between observed and next.
	if used, ok := snapshotUsed(observed.Providers["codex"].QuotaSnapshot); !ok || used != 10 {
		t.Fatalf("prior observed state mutated (aliasing): %+v", observed.Providers["codex"].QuotaSnapshot)
	}
	// next carries the fresh observation (used=25) on its own independent map.
	if used, ok := snapshotUsed(next.Providers["codex"].QuotaSnapshot); !ok || used != 25 {
		t.Fatalf("next state missing fresh observation: %+v", next.Providers["codex"].QuotaSnapshot)
	}
}

// TestQuotaCheckReconcileCrashAtSaveBoundaryRealStore proves the most
// safety-critical boundary for the reconcile flow using the REAL state.Store.Fault
// seam (which fires after the temp write but before the durable rename — the
// canonical torn-write hazard): when the save faults AFTER ApplyUnderLock has
// completed (publish ran) but BEFORE the final durable rename, no torn state is
// committed. The on-disk state.json stays at the prior revision with the last-good
// snapshot intact, and a subsequent clean QuotaCheck recovers consistently.
func TestQuotaCheckReconcileCrashAtSaveBoundaryRealStore(t *testing.T) {
	priorGood := freshSnap("codex", 10)
	prior := state.State{
		Schema:   1,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
		Targets: map[string]state.TargetState{},
	}
	store := seedStateStore(t, prior, true)

	spy := newQuotaCheckSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	spy.Coordinator.State = StoreState{Store: store}
	spy.Coordinator.Publish = seededPublisher{state: prior}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 25)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", true)

	// The poll and publish ran, but the final state Save faulted, so the transaction is rejected.
	if out.Accepted || out.Error == nil {
		t.Fatalf("expected rejected outcome with save fault error: %+v", out)
	}
	// Publish completed before the fault (the pipeline reached the publish step).
	if count(spy.Trace, "publish:global") == 0 {
		t.Fatalf("publish should have run before the save fault: %v", spy.Trace)
	}
	// The on-disk state is unchanged: no torn write. The Fault seam cleaned up the
	// temp file and the rename never happened, so state.json still holds prior.
	got := loadStateStore(t, store.Path)
	if got.Revision != 1 {
		t.Fatalf("on-disk revision=%d want 1 (fault must not commit an uncommitted save)", got.Revision)
	}
	if used, ok := snapshotUsed(got.Providers["codex"].QuotaSnapshot); !ok || used != 10 {
		t.Fatalf("last-good snapshot not preserved across crash: %+v", got.Providers["codex"].QuotaSnapshot)
	}
	// The fresh observation (used=25) did NOT leak to disk (no torn state).
	if used, ok := snapshotUsed(got.Providers["codex"].QuotaSnapshot); ok && used == 25 {
		t.Fatalf("uncommitted observation leaked to disk: %+v", got.Providers["codex"].QuotaSnapshot)
	}

	// A subsequent clean QuotaCheck (fault cleared) recovers consistently: the
	// revision advances from the durable prior and the fresh observation persists.
	clean := state.Store{Path: store.Path, Now: store.Now, RecoveredRetention: store.RecoveredRetention}
	recoverSpy := newQuotaCheckSpy()
	recoverSpy.Coordinator.Policy = quotaCheckPolicyLoader{}
	recoverSpy.Coordinator.State = StoreState{Store: clean}
	recoverSpy.Coordinator.Publish = seededPublisher{state: prior}
	rp := pollerOf(recoverSpy)
	rp.results["codex"] = freshSnap("codex", 25)

	recovered := recoverSpy.Coordinator.QuotaCheck(context.Background(), "", false)

	if !recovered.Accepted || recovered.Error != nil || recovered.Revision != 2 {
		t.Fatalf("recovery run failed: %+v", recovered)
	}
	recoveredOnDisk := loadStateStore(t, store.Path)
	if recoveredOnDisk.Revision != 2 {
		t.Fatalf("recovered on-disk revision=%d want 2", recoveredOnDisk.Revision)
	}
	if used, ok := snapshotUsed(recoveredOnDisk.Providers["codex"].QuotaSnapshot); !ok || used != 25 {
		t.Fatalf("recovered state missing fresh snapshot: %+v", recoveredOnDisk.Providers["codex"])
	}
}

// TestQuotaCheckPollOnlyCrashAtSaveBoundaryRealStore proves the poll-only path
// leaves no partial observation when the REAL state.Store.Fault fires at the save
// boundary: the on-disk state stays at the prior revision with the prior snapshot,
// and a clean re-run recovers.
func TestQuotaCheckPollOnlyCrashAtSaveBoundaryRealStore(t *testing.T) {
	priorGood := freshSnap("codex", 10)
	prior := state.State{
		Schema:   1,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
		Targets: map[string]state.TargetState{},
	}
	store := seedStateStore(t, prior, true)

	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	spy.Coordinator.State = StoreState{Store: store}
	spy.Coordinator.Publish = seededPublisher{state: prior}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 25)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)

	if out.Accepted || !out.DurabilityFailure || out.Error == nil {
		t.Fatalf("expected rejected durability outcome with save fault error: %+v", out)
	}
	got := loadStateStore(t, store.Path)
	if got.Revision != 1 {
		t.Fatalf("on-disk revision=%d want 1 (partial observation must not persist)", got.Revision)
	}
	if used, ok := snapshotUsed(got.Providers["codex"].QuotaSnapshot); !ok || used != 10 {
		t.Fatalf("prior snapshot not intact after poll-only crash: %+v", got.Providers["codex"].QuotaSnapshot)
	}

	// Clean re-run recovers and persists the fresh observation.
	clean := state.Store{Path: store.Path, Now: store.Now, RecoveredRetention: store.RecoveredRetention}
	recoverSpy := newQuotaCheckSpy()
	recoverSpy.Coordinator.Policy = quotaCheckPolicyLoader{}
	recoverSpy.Coordinator.State = StoreState{Store: clean}
	recoverSpy.Coordinator.Publish = seededPublisher{state: prior}
	rp := pollerOf(recoverSpy)
	rp.results["codex"] = freshSnap("codex", 25)

	recovered := recoverSpy.Coordinator.QuotaCheck(context.Background(), "", false)
	if !recovered.Accepted || recovered.Error != nil || recovered.Revision != 2 {
		t.Fatalf("recovery run failed: %+v", recovered)
	}
	recoveredOnDisk := loadStateStore(t, store.Path)
	if used, ok := snapshotUsed(recoveredOnDisk.Providers["codex"].QuotaSnapshot); !ok || used != 25 {
		t.Fatalf("recovered state missing fresh snapshot: %+v", recoveredOnDisk.Providers["codex"])
	}
}

// TestQuotaCheckCrashAfterPollBeforeSaveRecoversPrePollState covers the boundary
// after the poll but before the final durable save: observations are computed in
// memory (poll returned used=25) but, because the REAL state.Store.Fault fires,
// never persist. A subsequent Recover — loading the durable state and replaying
// through the publisher — yields exactly the pre-poll state, not a hybrid of old
// and new (both snapshot and attempt stay at the pre-poll value).
func TestQuotaCheckCrashAfterPollBeforeSaveRecoversPrePollState(t *testing.T) {
	priorGood := freshSnap("codex", 10)
	prior := state.State{
		Schema:   1,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: &priorGood, QuotaAttempt: &priorGood},
		},
		Targets: map[string]state.TargetState{},
	}
	store := seedStateStore(t, prior, true)
	pub := seededPublisher{state: prior}

	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	spy.Coordinator.State = StoreState{Store: store}
	spy.Coordinator.Publish = pub
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 25)

	out := spy.Coordinator.QuotaCheck(context.Background(), "", false)
	if out.Accepted || out.Error == nil {
		t.Fatalf("expected rejected outcome with save fault: %+v", out)
	}

	// The transact recover sequence (load durable state, then publisher Recover)
	// must yield exactly the pre-poll state: no in-memory observation persisted.
	loaded, err := state.Store{Path: store.Path}.Load()
	if err != nil {
		t.Fatalf("load after fault: %v", err)
	}
	if loaded.Revision != prior.Revision {
		t.Fatalf("durable revision=%d want pre-poll %d", loaded.Revision, prior.Revision)
	}
	recovered, err := pub.Recover(context.Background(), loaded)
	if err != nil {
		t.Fatalf("publisher recover after fault: %v", err)
	}
	if recovered.Revision != prior.Revision {
		t.Fatalf("recovered revision=%d want pre-poll %d", recovered.Revision, prior.Revision)
	}
	if used, ok := snapshotUsed(recovered.Providers["codex"].QuotaSnapshot); !ok || used != 10 {
		t.Fatalf("recovered snapshot should be pre-poll (used=10), got %+v", recovered.Providers["codex"].QuotaSnapshot)
	}
	if used, ok := snapshotUsed(recovered.Providers["codex"].QuotaAttempt); !ok || used != 10 {
		t.Fatalf("recovered attempt should be pre-poll (used=10), got %+v", recovered.Providers["codex"].QuotaAttempt)
	}
}

// --- test helpers ---

// quotaCheckPolicyLoader returns the two-mapping quota desired.
type quotaCheckPolicyLoader struct{}

func (quotaCheckPolicyLoader) LoadPolicy() (policy.Desired, error) { return quotaDesired(), nil }
func (quotaCheckPolicyLoader) DesiredExists() bool                 { return true }

// failingPolicyLoader always fails to load.
type failingPolicyLoader struct{}

func (failingPolicyLoader) LoadPolicy() (policy.Desired, error) {
	return policy.Desired{}, errors.New("load policy failed")
}
func (failingPolicyLoader) DesiredExists() bool { return false }

// seededStateStore returns a preset base state on LoadState and delegates Save
// to the spy so LastSaved is captured.
type seededStateStore struct {
	state state.State
	spy   *coordinatorSpy
}

func (s seededStateStore) LoadState() (state.State, error) { return s.state, nil }
func (s seededStateStore) Save(st state.State) error {
	s.spy.LastSaved = st
	s.spy.StateSaves++
	return nil
}

// seededPublisher is a Publisher whose Recover returns a deep copy of the preset
// state (so the transact path sees the seeded observed state without the
// in-memory mutation corrupting the preset). ApplyUnderLock is a no-op.
type seededPublisher struct{ state state.State }

func (s seededPublisher) Recover(context.Context, state.State) (state.State, error) {
	return copyState(s.state), nil
}
func (s seededPublisher) ApplyUnderLock(context.Context, publish.Transaction) (state.State, error) {
	return s.state, nil
}

// crashingStateStore simulates a crash between the poll and the final state
// save: LoadState returns base, and Save fails (fault) without committing.
// committed stays nil until a successful Save. Recover returns a deep copy of
// base so the in-memory observation mutation never corrupts the on-disk base
// (production reads fresh from disk, so the map is never shared).
type crashingStateStore struct {
	base      state.State
	fault     bool
	committed *state.State
}

func (c *crashingStateStore) LoadState() (state.State, error) { return c.base, nil }
func (c *crashingStateStore) Save(st state.State) error {
	if c.fault {
		return errors.New("service: crash before state save (fault)")
	}
	c.committed = &st
	return nil
}

// copyState returns a deep-enough copy of st so mutating the Providers map of
// the copy does not affect st.
func copyState(st state.State) state.State {
	out := st
	if st.Providers != nil {
		out.Providers = make(map[string]state.ProviderState, len(st.Providers))
		for k, v := range st.Providers {
			out.Providers[k] = v
		}
	}
	return out
}

// MutationExitCodeOf mirrors the CLI exit-code mapping for service-level tests.
func MutationExitCodeOf(o Outcome) int {
	if !o.Accepted {
		return 1
	}
	if o.PendingCount() > 0 || o.Problem {
		return 2
	}
	return 0
}

// seedStateStore writes prior to a REAL state.Store at a fresh temp path via a
// clean durable save, then (when fault is set) arms Fault — the real torn-write
// seam that fires after the temp file is written but before the durable rename.
// Returns the armed store. The real seam is the whole point: the store writes a
// temp, fsync's, then renames; a non-nil Fault aborts between the temp write and
// the fsync/rename, cleaning up the temp so the durable state.json is untouched.
func seedStateStore(t *testing.T, prior state.State, fault bool) state.Store {
	t.Helper()
	store := state.Store{
		Path:               filepath.Join(t.TempDir(), "state.json"),
		Now:                func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
		RecoveredRetention: 24 * time.Hour,
	}
	if err := store.Save(prior); err != nil {
		t.Fatalf("seed state store: %v", err)
	}
	if fault {
		store.Fault = func() error { return errors.New("simulated crash before durable rename") }
	}
	return store
}

// loadStateStore reads the committed on-disk state at path.
func loadStateStore(t *testing.T, path string) state.State {
	t.Helper()
	got, err := state.Store{Path: path}.Load()
	if err != nil {
		t.Fatalf("load committed state %s: %v", path, err)
	}
	return got
}

// snapshotUsed returns the first window's used value and whether it is present.
func snapshotUsed(snap *quota.QuotaSnapshot) (float64, bool) {
	if snap == nil || len(snap.Windows) == 0 || snap.Windows[0].Used == nil {
		return 0, false
	}
	return *snap.Windows[0].Used, true
}
