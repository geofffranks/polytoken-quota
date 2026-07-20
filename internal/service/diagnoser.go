package service

// Diagnostic types for the Coordinator. The Diagnoser interface and its
// StatusReport DTO live in the service package (not cli) so the production
// implementation — *Coordinator — can implement Diagnoser directly and satisfy
// a compile-time interface assertion. cli imports service for these types; the
// reverse import (service→cli) would be a cycle, so the DTOs are defined here.

import (
	"context"

	"github.com/geofffranks/codexbar-hooks/internal/doctor"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// Diagnoser performs the read-only diagnostic operations surfaced by the CLI.
// The production implementation is *Coordinator.
type Diagnoser interface {
	Status(context.Context, bool) StatusReport
	Doctor(context.Context, bool) doctor.Report
}

// ProviderStatus is one provider's quota/availability/mode axis in a status
// report.
type ProviderStatus struct {
	Provider     string             `json:"provider"`
	Quota        state.Quota        `json:"quota"`
	Availability state.Availability `json:"availability"`
	Mode         state.Mode         `json:"mode"`
	LastEvent    string             `json:"last_event,omitempty"`
}

// TargetStatus is one target's attempted/applied revision in a status report.
type TargetStatus struct {
	TargetID          string `json:"target_id"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	AppliedRevision   uint64 `json:"applied_revision"`
	Pending           bool   `json:"pending"`
}

// StatusReport is the result of the status command. It carries the provider
// axes, effective modes, last events, current revision, per-target
// attempted/applied revisions, concise pending/drift summary, and the
// unconditional running-session advisory.
type StatusReport struct {
	JSON                   bool             `json:"-"`
	Revision               uint64           `json:"revision"`
	Providers              []ProviderStatus `json:"providers,omitempty"`
	Targets                []TargetStatus   `json:"targets,omitempty"`
	Pending                int              `json:"pending"`
	Drift                  bool             `json:"drift"`
	RunningSessionAdvisory string           `json:"running_session_advisory"`
}

// Compile-time assertions that *Coordinator implements both Mutator (via its
// Init/HandleEvent/Reconcile/Sync/Set/Clear methods) and Diagnoser.
var (
	_ Diagnoser = (*Coordinator)(nil)
)

// Status renders the current observed state, policy providers, and per-target
// reconciliation outcome into a StatusReport. It is read-only: it never mutates
// state or targets. A load or resolution error yields a report carrying only the
// error-safe fields (empty providers/targets) rather than panicking.
func (c *Coordinator) Status(_ context.Context, _ bool) StatusReport {
	report := StatusReport{}
	if c.State == nil {
		return report
	}
	observed, err := c.State.LoadState()
	if err != nil {
		return report
	}
	report.Revision = observed.Revision

	// Provider axes in stable (sorted) order so output is deterministic.
	for _, name := range sortedProviderNames(observed.Providers) {
		ps := observed.Providers[name]
		report.Providers = append(report.Providers, ProviderStatus{
			Provider:     name,
			Quota:        ps.Quota,
			Availability: ps.Availability,
			Mode:         state.EffectiveMode(ps),
		})
	}

	// Per-target attempted/applied revision and pending flag.
	for _, id := range sortedTargetIDs(observed.Targets) {
		ts := observed.Targets[id]
		report.Targets = append(report.Targets, TargetStatus{
			TargetID:          id,
			AttemptedRevision: ts.AttemptedRevision,
			AppliedRevision:   ts.AppliedRevision,
			Pending:           ts.Pending != nil,
		})
		if ts.Pending != nil {
			report.Pending++
		}
	}

	// Drift: any target whose attempted revision exceeds its applied revision is
	// not yet fully reconciled with the live files.
	for _, ts := range observed.Targets {
		if ts.AttemptedRevision > ts.AppliedRevision {
			report.Drift = true
			break
		}
	}
	return report
}

// Doctor collects health and drift diagnostics by delegating to doctor.Run with
// the Coordinator's real dependencies. The state store is always wired; the
// optional inspectors (policy/target/live/publish) are nil-safe — each
// contributes no findings when unset, so Doctor never panics even before full
// inspector wiring. It is read-only.
func (c *Coordinator) Doctor(ctx context.Context, _ bool) doctor.Report {
	deps := doctor.Dependencies{}
	// Only wire the state store when it has a real path; a zero-valued store
	// (Path=="") would surface a spurious state-unreadable finding.
	if c.DiagnosticState.Path != "" {
		deps.State = c.DiagnosticState
	}
	if c.DoctorInspectors.Policy != nil {
		deps.Policy = c.DoctorInspectors.Policy
	}
	if c.DoctorInspectors.Targets != nil {
		deps.Targets = c.DoctorInspectors.Targets
	}
	if c.DoctorInspectors.Validator != nil {
		deps.Validator = c.DoctorInspectors.Validator
	}
	if c.DoctorInspectors.Publisher != nil {
		deps.Publisher = c.DoctorInspectors.Publisher
	}
	return doctor.Run(ctx, deps)
}
