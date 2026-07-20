package service

// Dependency interfaces for the Coordinator. Every one is injected for
// testability: production wires concrete adapters (state.Store, the staging
// Builder, the validate Runner, etc.) while tests inject recording spies. The
// Coordinator owns the single locked transaction path and never re-enters a
// dependency's own locking — Recover and Apply here publish per-target files and
// recover prior journals under the Coordinator's already-held lock.

import (
	"context"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/publish"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"github.com/geofffranks/codexbar-hooks/internal/validate"
)

// Tracer is the observability seam the Coordinator emits each transaction step
// through. It is nil-safe in production (no-op) and the spy records the exact
// operation order in tests. Internal steps that are not natural dependency calls
// (accept-revision, record-pending, desired-exists) and per-kind granularity
// are observable only through this seam.
type Tracer interface {
	Step(string)
}

// Clock returns the current time, used for state timestamps.
type Clock interface {
	Now() time.Time
}

// PolicyLoader loads the validated desired policy and reports whether desired.yaml
// already exists (the strict create-only Init guard).
type PolicyLoader interface {
	LoadPolicy() (policy.Desired, error)
	DesiredExists() bool
}

// StateStore loads and saves the durable observed state.
type StateStore interface {
	LoadState() (state.State, error)
	Save(state.State) error
}

// RegisteredTarget pairs a policy target with its resolved, canonicalized form.
// The reconciler renders from the policy target; staging and publication use the
// resolved root and definition files.
type RegisteredTarget struct {
	Policy   policy.Target
	Resolved target.Resolved
}

// TargetRegistry resolves the registered targets (global + projects) from the
// desired policy, canonicalizing roots and validating definition containment.
type TargetRegistry interface {
	ResolveTargets(desired policy.Desired) ([]RegisteredTarget, error)
}

// Reconciler builds a reconciliation plan for one target. It wraps
// reconcile.Build, which is a pure function of desired policy plus observed state.
type Reconciler interface {
	Build(desired policy.Desired, observed state.State, t policy.Target) (reconcile.Plan, error)
}

// Stager materializes an isolated validation staging candidate for one target.
// It wraps staging.Builder.Build.
type Stager interface {
	Stage(ctx context.Context, res target.Resolved, plan reconcile.Plan) (staging.Candidate, error)
}

// Validator runs the bounded startup-equivalent validation against one staged
// candidate. It wraps validate.Runner.Validate.
type Validator interface {
	Validate(ctx context.Context, c staging.Candidate, timeout time.Duration) validate.Result
}

// Publisher recovers any prior unfinished apply journal and publishes one
// target's validated candidate files. The Coordinator holds the advisory lock
// and calls Recover once, then Apply per valid target; neither re-locks.
type Publisher interface {
	Recover(ctx context.Context, prior state.State) (state.State, error)
	Apply(ctx context.Context, tx publish.Transaction) (state.State, error)
}
