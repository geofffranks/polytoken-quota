package service

// Diagnostic types for the Coordinator. The Diagnoser interface and its
// StatusReport DTO live in the service package (not cli) so the production
// implementation — *Coordinator — can implement Diagnoser directly and satisfy
// a compile-time interface assertion. cli imports service for these types; the
// reverse import (service→cli) would be a cycle, so the DTOs are defined here.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// Diagnoser performs the read-only diagnostic operations surfaced by the CLI.
// The production implementation is *Coordinator.
type Diagnoser interface {
	Status(context.Context, bool) StatusReport
	Doctor(context.Context, bool) doctor.Report
}

// SnapshotBuilder builds the shared read-only diagnostic snapshot backing the
// routing selectors. The production implementation is *Coordinator. The CLI's
// bare `routing` and `routing explain` commands use the snapshot's view methods.
type SnapshotBuilder interface {
	BuildDiagnosticSnapshot(context.Context) DiagnosticSnapshot
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
// and effective order (when routing is enabled), and per-provider quota state.
type StatusReport struct {
	JSON            bool                  `json:"-"`
	AsOf            time.Time             `json:"as_of"`
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
	Error string `json:"error,omitempty"`
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
		AsOf: snapshot.asOf, Revision: snapshot.revision, Targets: append([]TargetStatus(nil), snapshot.targets...),
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

// Doctor collects health and drift diagnostics by building the shared
// DiagnosticSnapshot once and delegating to doctor.Run with preloaded data. It
// never independently loads policy, state, or targets: the snapshot's single
// read feeds every configuration, publication, and quota finding. Doctor is
// problem-only — manual disables and healthy providers are not surfaced (they
// remain visible through status mode/reason). It is read-only.
func (c *Coordinator) Doctor(ctx context.Context, _ bool) doctor.Report {
	snapshot := c.BuildDiagnosticSnapshot(ctx)
	asOf := snapshot.AsOf()

	deps := doctor.Dependencies{
		Observed: snapshot.ObservedState(),
		Now:      func() time.Time { return asOf },
	}

	// Configuration findings come from the preloaded snapshot's captured load
	// errors — no duplicate LoadPolicy or ResolveTargets. Publication uses the
	// JournalPath directly.
	deps.Policy = &preloadedPolicyInspector{snapshot: snapshot, loader: c.Policy}
	deps.Targets = &preloadedTargetInspector{snapshot: snapshot}
	deps.Publisher = PublishDoctorInspector{JournalPath: c.JournalPath}

	// Build quota probes from the preloaded snapshot data + evidence gate.
	var evidence *quota.EvidenceRegistry
	if provider, ok := c.QuotaPoller.(quotaEvidenceProvider); ok {
		evidence = provider.EvidenceRegistry()
	}
	probes, reconcilePending := buildDoctorQuotaProbes(doctorQuotaInputs{
		observed:    snapshot.ObservedState(),
		desired:     snapshot.DesiredPolicy(),
		now:         asOf,
		evidence:    evidence,
		journalPath: c.JournalPath,
	})
	deps.QuotaProbes = probes
	deps.ReconcilePending = reconcilePending

	report := doctor.Run(ctx, deps)

	// Surface a state-unreadable finding from the snapshot's captured state load
	// error — doctor must report this even though the preloaded observed state is
	// a zero value on failure. The message is a fixed literal (never the raw
	// error) to avoid leaking secret-bearing detail from a hand-edited state file.
	if snapshot.StateError() != nil {
		report.Findings = append([]doctor.Finding{{
			Code:        "state-unreadable",
			Message:     "could not read state file",
			Remediation: "check state.json format and permissions",
			Severity:    doctor.Error,
		}}, report.Findings...)
	}

	return report
}

// preloadedPolicyInspector surfaces a policy-schema finding from the snapshot's
// captured policy load error without re-loading policy. When policy loaded
// cleanly it contributes no findings (the snapshot's LoadPolicy already
// validated the schema).
type preloadedPolicyInspector struct {
	snapshot DiagnosticSnapshot
	loader   PolicyLoader
}

func (p *preloadedPolicyInspector) Findings(context.Context) []doctor.Finding {
	if p.snapshot.PolicyError() == nil {
		return nil
	}
	if p.loader != nil && !p.loader.DesiredExists() {
		return []doctor.Finding{{
			Code:        "policy-schema",
			Message:     "desired.yaml does not exist",
			Remediation: "run `polytoken-quota init` to create the initial policy",
			Severity:    doctor.Error,
		}}
	}
	return []doctor.Finding{{
		Code:        "policy-schema",
		Message:     fmt.Sprintf("desired.yaml failed validation: %s", quota.SanitizeText(p.snapshot.PolicyError().Error())),
		Remediation: "fix desired.yaml (or regenerate with `polytoken-quota sync --from-polytoken`)",
		Severity:    doctor.Error,
	}}
}

// preloadedTargetInspector surfaces a target-unresolvable finding from the
// snapshot's captured resolution error without re-resolving targets. When
// targets resolved cleanly it contributes no findings.
type preloadedTargetInspector struct {
	snapshot DiagnosticSnapshot
}

func (p *preloadedTargetInspector) Findings(context.Context) []doctor.Finding {
	if p.snapshot.ResolveError() == nil {
		return nil
	}
	err := p.snapshot.ResolveError()
	code := "target-unresolvable"
	if errors.Is(err, target.ErrSymlinkManagedFile) {
		code = "definition-symlink"
	}
	return []doctor.Finding{{
		Code:        code,
		Message:     fmt.Sprintf("registered target resolution failed: %s", quota.SanitizeText(err.Error())),
		Remediation: "fix the registered root/definition paths in desired.yaml (symlinked managed files are rejected)",
		Severity:    doctor.Error,
	}}
}
