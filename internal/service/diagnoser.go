package service

// Diagnostic types for the Coordinator. The Diagnoser interface and its
// StatusReport DTO live in the service package (not cli) so the production
// implementation — *Coordinator — can implement Diagnoser directly and satisfy
// a compile-time interface assertion. cli imports service for these types; the
// reverse import (service→cli) would be a cycle, so the DTOs are defined here.

import (
	"context"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
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
func (c *Coordinator) Status(_ context.Context, _ bool) StatusReport {
	report := StatusReport{}
	if c.State == nil {
		report.Error = "no state store configured"
		return report
	}
	observed, err := c.State.LoadState()
	if err != nil {
		// A malformed or unreadable state file must never render as a clean
		// "in sync" report; surface the sanitized failure so the CLI can exit
		// non-zero.
		report.Error = sanitizeFailure(err.Error())
		return report
	}
	report.Revision = observed.Revision

	// Provider axes in stable (sorted) order so output is deterministic.
	for _, name := range sortedProviderNames(observed.Providers) {
		ps := observed.Providers[name]
		report.Providers = append(report.Providers, ProviderStatus{
			Provider:       name,
			Quota:          ps.Quota,
			Availability:   ps.Availability,
			Mode:           state.EffectiveMode(ps),
			ManualDisabled: ps.ManualDisabled,
			Reason:         providerReason(ps),
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

	// Quota projections are composed from the same read-only service method used
	// by `quota status`, ensuring status and quota status cannot diverge.
	quotaReport := c.QuotaStatus(context.Background())
	report.Quota = quotaReport.Providers
	report.Problem = quotaReport.Problem

	// Routing projections are only shown when policy loading succeeds. Disabled
	// routing is explicit and intentionally carries no ranking entries.
	if c.Policy != nil {
		if desired, perr := c.Policy.LoadPolicy(); perr == nil {
			report.RoutingEnabled = desired.Routing.Enabled
			if desired.Routing.Enabled {
				rankLookup, ranking := ComputeRanking(desired, observed, c.now())
				for _, entry := range ranking.Entries {
					report.Ranking = append(report.Ranking, RankEntryReport{
						MappingID:   entry.MappingID,
						Rank:        entry.Rank,
						OffPeak:     entry.OffPeak,
						Eligible:    entry.Eligible,
						Explanation: entry.Explanation,
					})
				}
				appendChainOrders := func(target policy.Target) {
					chains := []struct {
						name  string
						chain policy.Chain
					}{
						{"full", target.Full}, {"mini", target.Mini},
						{"nano", target.Nano}, {"classifier", target.Classifier},
					}
					for _, ch := range chains {
						if len(ch.chain) == 0 {
							continue
						}
						effective, err := reconcile.EffectiveOrder(desired, observed, ch.chain, rankLookup)
						if err != nil {
							continue
						}
						report.EffectiveOrders = append(report.EffectiveOrders, ChainOrderReport{
							TargetID: target.ID, Chain: ch.name,
							Desired: append([]string(nil), ch.chain...), Effective: effective,
						})
					}
				}
				appendChainOrders(desired.Global)
				for _, target := range desired.Projects {
					appendChainOrders(target)
				}
			}
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
