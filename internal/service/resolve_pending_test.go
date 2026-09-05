package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// staleSyntheticTargets seeds persisted state with stale synthetic
// resolution-failure pendings from older revisions: quota-reconcile from a
// failed `check --reconcile` resolution and manual-resolution from a failed
// manual-command resolution.
func staleSyntheticTargets() map[string]state.TargetState {
	stale := time.Date(2026, 9, 5, 8, 5, 21, 0, time.UTC)
	synthetic := func(id string) state.TargetState {
		return state.TargetState{Pending: &state.ApplyFailure{
			TargetID:          id,
			Stage:             "resolve_targets",
			Summary:           "target: definition lappie:facets/app-engineering.md: stat failed",
			AttemptedRevision: 38,
			AttemptedAt:       stale,
			LiveStatus:        "last-known-good",
		}}
	}
	return map[string]state.TargetState{
		"quota-reconcile":   synthetic("quota-reconcile"),
		"manual-resolution": synthetic("manual-resolution"),
	}
}

func seededState() state.State {
	return state.State{Revision: 41, Providers: map[string]state.ProviderState{}, Targets: staleSyntheticTargets()}
}

func assertSyntheticPendingsRetired(t *testing.T, saved state.State) {
	t.Helper()
	for _, id := range []string{"quota-reconcile", "manual-resolution"} {
		if ts, ok := saved.Targets[id]; ok {
			t.Fatalf("stale synthetic target %q survived a successful run: %+v", id, ts)
		}
	}
}

// TestReconcileSuccessRetiresStaleSyntheticPendings proves a reconcile whose
// target resolution succeeds retires stale quota-reconcile and
// manual-resolution pendings so doctor stops reporting fixed resolution errors.
func TestReconcileSuccessRetiresStaleSyntheticPendings(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.recovered = seededState()
	out := spy.Coordinator.Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v", out)
	}
	assertSyntheticPendingsRetired(t, spy.LastSaved)
}

// TestManualDisableSuccessRetiresStaleSyntheticPendings proves a manual
// command whose target resolution succeeds retires stale synthetic pendings.
func TestManualDisableSuccessRetiresStaleSyntheticPendings(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.recovered = seededState()
	out := spy.Coordinator.Disable(context.Background(), "codex-mapping")
	if !out.Accepted {
		t.Fatalf("out=%+v", out)
	}
	assertSyntheticPendingsRetired(t, spy.LastSaved)
}

// TestQuotaCheckReconcileSuccessRetiresStaleSyntheticPendings proves the
// check --reconcile path retires stale synthetic pendings once target
// resolution succeeds — the ghost that kept a fixed stat failed visible.
func TestQuotaCheckReconcileSuccessRetiresStaleSyntheticPendings(t *testing.T) {
	spy := newQuotaCheckSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	p := pollerOf(spy)
	p.results["codex"] = freshSnap("codex", 20)
	p.results["zai"] = freshSnap("zai", 40)
	spy.recovered = seededState()

	out := spy.Coordinator.QuotaCheck(context.Background(), "", true)

	if !out.Accepted {
		t.Fatalf("out=%+v", out)
	}
	assertSyntheticPendingsRetired(t, spy.LastSaved)
}

// TestQuotaCheckResolutionFailureReplacesStaleSyntheticPendings proves a new
// resolution failure records the quota-reconcile pending and retires the other
// synthetic id, so at most the latest resolution failure is reported.
func TestQuotaCheckResolutionFailureReplacesStaleSyntheticPendings(t *testing.T) {
	spy := newQuotaCheckSpy()
	spy.Coordinator.Policy = quotaCheckPolicyLoader{}
	spy.resolveErr = errors.New("target: definition lappie:facets/app-engineering.md: stat failed")
	spy.recovered = seededState()

	out := spy.Coordinator.QuotaCheck(context.Background(), "", true)

	if !out.Accepted {
		t.Fatalf("out=%+v", out)
	}
	pending := spy.LastSaved.Targets["quota-reconcile"]
	if pending.Pending == nil || pending.Pending.Stage != "resolve_targets" {
		t.Fatalf("saved pending=%+v", pending)
	}
	if _, ok := spy.LastSaved.Targets["manual-resolution"]; ok {
		t.Fatalf("stale manual-resolution survived a new resolution failure: %+v", spy.LastSaved.Targets)
	}
}
