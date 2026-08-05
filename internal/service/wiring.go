package service

// Production wiring adapters: concrete implementations of the Coordinator's
// injected dependency interfaces, backed by the real policy loader, the target
// resolver, the reconcile builder, and the staging/validate runners. These let
// main.go wire a fully functional *Coordinator without nil dependencies. They
// hold no business logic; each delegates to the package it wraps.

import (
	"context"
	"os"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// FilePolicyLoader is the production PolicyLoader: it loads and validates
// desired.yaml at DesiredPath and reports whether it exists.
type FilePolicyLoader struct {
	Path string
}

// LoadPolicy loads and validates desired.yaml.
func (l FilePolicyLoader) LoadPolicy() (policy.Desired, error) {
	return policy.Load(l.Path)
}

// DesiredExists reports whether desired.yaml is present.
func (l FilePolicyLoader) DesiredExists() bool {
	_, err := os.Stat(l.Path)
	return err == nil
}

// DesiredPath returns the path to desired.yaml. It lets the routing enable/
// disable commands locate the file for byte-preserving editing without the
// Coordinator needing a separate path field.
func (l FilePolicyLoader) DesiredPath() string { return l.Path }

// StoreState adapts the concrete state.Store (Load/Save) to the StateStore
// interface (LoadState/Save). The concrete store's Load returns a fresh empty
// state on a missing file, so LoadState inherits that behavior.
type StoreState struct {
	Store state.Store
}

// LoadState loads the durable observed state; a missing file yields an empty
// state (not an error).
func (s StoreState) LoadState() (state.State, error) {
	return s.Store.Load()
}

// Save durably persists the observed state.
func (s StoreState) Save(st state.State) error {
	return s.Store.Save(st)
}

// reconcileBuilder adapts reconcile.Build to the Reconciler interface.
type reconcileBuilder struct{}

// Build delegates to reconcile.Build.
func (reconcileBuilder) Build(desired policy.Desired, observed state.State, t policy.Target, ranks reconcile.RankLookup) (reconcile.Plan, error) {
	return reconcile.Build(desired, observed, t, ranks)
}

// StagingStager adapts staging.Builder to the Stager interface.
type StagingStager struct {
	Builder staging.Builder
}

// Stage delegates to staging.Builder.Build.
func (s StagingStager) Stage(ctx context.Context, res target.Resolved, plan reconcile.Plan) (staging.Candidate, error) {
	return s.Builder.Build(ctx, res, plan)
}

// ValidateRunner adapts validate.Runner to the Validator interface.
type ValidateRunner struct {
	Runner validate.Runner
}

// Validate delegates to validate.Runner.Validate against a copy of the candidate
// whose Cleanup is detached. validate.Runner.Validate calls Cleanup on
// completion, but the Coordinator needs the staged files to remain on disk for
// the subsequent publish step (buildTransaction reads them and applyOne renames
// them). The Coordinator owns the candidate's lifecycle after publish.
func (v ValidateRunner) Validate(ctx context.Context, c staging.Candidate, timeout time.Duration) validate.Result {
	return v.Runner.Validate(ctx, c.WithoutCleanup(), timeout)
}

// realTargetRegistry is the production TargetRegistry: it resolves the global
// and registered project targets from the desired policy, canonicalizing roots
// and validating definition containment.
type realTargetRegistry struct{}

// ResolveTargets resolves the global target plus every registered project target.
func (realTargetRegistry) ResolveTargets(desired policy.Desired) ([]RegisteredTarget, error) {
	out := make([]RegisteredTarget, 0, 1+len(desired.Projects))
	res, err := target.Resolve(desired.Global)
	if err != nil {
		return nil, err
	}
	out = append(out, RegisteredTarget{Policy: desired.Global, Resolved: res})
	for i := range desired.Projects {
		res, err := target.Resolve(desired.Projects[i])
		if err != nil {
			return nil, err
		}
		out = append(out, RegisteredTarget{Policy: desired.Projects[i], Resolved: res})
	}
	return out, nil
}

// NewTargetRegistry returns the production TargetRegistry.
func NewTargetRegistry() TargetRegistry { return realTargetRegistry{} }

// NewReconciler returns the production Reconciler backed by reconcile.Build.
func NewReconciler() Reconciler { return reconcileBuilder{} }
