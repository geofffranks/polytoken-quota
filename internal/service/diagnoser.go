package service

// Diagnostic types for the Coordinator. The Diagnoser interface and its
// StatusReport DTO live in the service package (not cli) so the production
// implementation — *Coordinator — can implement Diagnoser directly and satisfy
// a compile-time interface assertion. cli imports service for these types; the
// reverse import (service→cli) would be a cycle, so the DTOs are defined here.

import (
	"context"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
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
	Provider       string             `json:"provider"`
	Quota          state.Quota        `json:"quota"`
	Availability   state.Availability `json:"availability"`
	Mode           state.Mode         `json:"mode"`
	ManualDisabled bool               `json:"manual_disabled"`
	Reason         string             `json:"reason"`
	LastEvent      string             `json:"last_event,omitempty"`
}

func providerReason(ps state.ProviderState) string {
	if ps.ManualDisabled {
		return "manual_disabled"
	}
	if ps.Availability == state.Unavailable {
		return "unavailable"
	}
	if ps.Quota == state.QuotaExhausted {
		return "quota_exhausted"
	}
	if ps.Quota == state.QuotaLow {
		return "quota_low"
	}
	return "normal"
}

// TargetStatus is one target's attempted/applied revision in a status report.
type TargetStatus struct {
	TargetID          string `json:"target_id"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	AppliedRevision   uint64 `json:"applied_revision"`
	Pending           bool   `json:"pending"`
}

// ChainOrderReport carries the desired vs effective model order for one managed
// chain, so status can show how routing has reordered survivors.
type ChainOrderReport struct {
	TargetID  string   `json:"target_id"`
	Chain     string   `json:"chain"`
	Desired   []string `json:"desired"`
	Effective []string `json:"effective"`
}

// StatusReport is the result of the status command. It carries the provider
// axes, effective modes, last events, current revision, per-target
// attempted/applied revisions, concise pending/drift summary, routing ranking
// and effective order (when routing is enabled), per-provider quota state, and
// the unconditional running-session advisory.
type StatusReport struct {
	JSON            bool                  `json:"-"`
	Revision        uint64                `json:"revision"`
	Providers       []ProviderStatus      `json:"providers,omitempty"`
	Targets         []TargetStatus        `json:"targets,omitempty"`
	Pending         int                   `json:"pending"`
	Drift           bool                  `json:"drift"`
	RoutingEnabled  bool                  `json:"routing_enabled"`
	Ranking         []RankEntryReport     `json:"ranking,omitempty"`
	EffectiveOrders []ChainOrderReport    `json:"effective_orders,omitempty"`
	Quota           []QuotaSnapshotReport `json:"quota,omitempty"`
	Problem         bool                  `json:"problem"`
	// Error carries a sanitized diagnostic when the report could not be
	// produced at all (state unreadable / no store). Callers must treat a
	// non-empty Error as a failed diagnostic, never as a clean report.
	Error                  string `json:"error,omitempty"`
	RunningSessionAdvisory string `json:"running_session_advisory"`
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
func (c *Coordinator) Status(ctx context.Context, _ bool) StatusReport {
	snapshot := c.BuildDiagnosticSnapshot(ctx)
	view := snapshot.StatusView()
	report := StatusReport{
		Revision: snapshot.revision, Targets: append([]TargetStatus(nil), snapshot.targets...),
		Pending: snapshot.pending, Drift: snapshot.drift, Problem: snapshot.problem,
		Quota: cloneLegacyQuota(snapshot.legacyQuota), Error: view.Error,
	}
	for _, provider := range view.Providers {
		report.Providers = append(report.Providers, ProviderStatus{
			Provider: provider.MappingID, Quota: state.Quota(provider.QuotaClass),
			Availability: provider.Availability, Mode: provider.EffectiveMode,
			ManualDisabled: provider.ManualDisabled, Reason: provider.Reason,
		})
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
	// Wire the quota/routing inspector when the diagnostic state store is
	// available (it needs to load observed quota snapshots/attempts). The
	// policy loader is shared with the transaction path; the journal path
	// enables the interrupted-reconcile check.
	if c.DiagnosticState.Path != "" {
		var evidence *quota.EvidenceRegistry
		if provider, ok := c.QuotaPoller.(quotaEvidenceProvider); ok {
			evidence = provider.EvidenceRegistry()
		}
		deps.Quota = quotaDoctorInspector{
			state:       c.DiagnosticState,
			policy:      c.Policy,
			journalPath: c.JournalPath,
			now:         c.now,
			evidence:    evidence,
		}
	}
	return doctor.Run(ctx, deps)
}
