package service

// T2b: the persisted Remediation of pre-publication pendings. Every
// stage-stage pending persists a non-empty, sanitized, staging-specific hint
// (D6); render, publish, and resolve_targets pendings persist the generic hint
// with no staging-specific text. Doctor backfills empty remediation only at
// display time (internal/doctor), so these assertions target the PERSISTED
// ApplyFailure.Remediation field, read back from the saved state.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// stagingHintMarker is text unique to the staging-specific remediation hint;
// it must never appear in non-stage pendings.
const stagingHintMarker = "staging temp directory"

// pendingFor returns the persisted pending failure for one target from the
// last saved state.
func pendingFor(t *testing.T, spy *coordinatorSpy, id string) *state.ApplyFailure {
	t.Helper()
	ts, ok := spy.LastSaved.Targets[id]
	if !ok || ts.Pending == nil {
		t.Fatalf("no persisted pending for target %q in saved state", id)
	}
	return ts.Pending
}

func TestStagePendingCarriesStagingRemediation(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Stage = failingStage{}
	spy.Coordinator.Reconcile(context.Background(), false, false, false)
	p := pendingFor(t, spy, "global")
	if p.Stage != "stage" {
		t.Fatalf("pending stage = %q, want stage", p.Stage)
	}
	if p.Remediation == "" {
		t.Fatal("stage pending persisted an empty Remediation")
	}
	if !strings.Contains(p.Remediation, stagingHintMarker) {
		t.Fatalf("stage pending remediation lacks the staging-specific hint: %q", p.Remediation)
	}
	// The hint is sanitized clean: re-sanitizing is a no-op.
	if got := validate.DefaultSanitize([]byte(p.Remediation)); got != p.Remediation {
		t.Fatalf("remediation is not sanitize-clean: %q -> %q", p.Remediation, got)
	}
}

func TestRenderPendingCarriesGenericRemediation(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Builder = failingRender{}
	spy.Coordinator.Reconcile(context.Background(), false, false, false)
	p := pendingFor(t, spy, "global")
	if p.Stage != "render" {
		t.Fatalf("pending stage = %q, want render", p.Stage)
	}
	assertGenericRemediation(t, p.Remediation)
}

func TestPublishPendingCarriesGenericRemediation(t *testing.T) {
	spy := newCoordinatorSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Publish = failingApply{inner: spy}
	spy.Coordinator.Reconcile(context.Background(), false, false, false)
	p := pendingFor(t, spy, "global")
	if p.Stage != "publish" {
		t.Fatalf("pending stage = %q, want publish", p.Stage)
	}
	assertGenericRemediation(t, p.Remediation)
}

func TestResolveTargetsPendingCarriesGenericRemediation(t *testing.T) {
	spy := newCoordinatorSpy()
	spy.resolveErr = errors.New("synthetic resolution failure")
	out := spy.Coordinator.Disable(context.Background(), "codex-mapping")
	if out.Accepted && out.Error == nil {
		t.Fatalf("expected the resolution failure to surface: %+v", out)
	}
	p := pendingFor(t, spy, pendingTargetManual)
	if p.Stage != "resolve_targets" {
		t.Fatalf("pending stage = %q, want resolve_targets", p.Stage)
	}
	assertGenericRemediation(t, p.Remediation)
}

// assertGenericRemediation pins the generic hint: exactly doctor's backfill
// text, with no staging-specific advice.
func assertGenericRemediation(t *testing.T, remediation string) {
	t.Helper()
	if remediation != "resolve the pending error and re-run reconcile" {
		t.Fatalf("generic remediation = %q, want the doctor-backfill text", remediation)
	}
	if strings.Contains(remediation, stagingHintMarker) || strings.Contains(remediation, "staging") {
		t.Fatalf("non-stage pending carries staging-specific advice: %q", remediation)
	}
}

// --- forcing doubles ---------------------------------------------------------

// failingStage forces a stage-stage failure in the spy pipeline.
type failingStage struct{}

func (failingStage) Stage(context.Context, target.Resolved, reconcile.Plan, *reconcile.Plan) (staging.Candidate, error) {
	return staging.Candidate{}, errors.New("synthetic stage failure: cannot write staged config")
}

// failingRender forces a render-stage failure in the spy pipeline.
type failingRender struct{}

func (failingRender) Build(policy.Desired, state.State, policy.Target, reconcile.RankLookup) (reconcile.Plan, error) {
	return reconcile.Plan{}, errors.New("synthetic render failure")
}

// failingApply forces a publish-stage failure while delegating recovery to the
// spy so the transaction still has observed state.
type failingApply struct{ inner Publisher }

func (f failingApply) Recover(ctx context.Context, prior state.State) (state.State, error) {
	return f.inner.Recover(ctx, prior)
}

func (f failingApply) ApplyUnderLock(context.Context, publish.Transaction) (state.State, error) {
	return state.State{}, errors.New("synthetic publish failure")
}
