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
	Status(context.Context, bool) MergedStatusReport
	Doctor(context.Context, bool) doctor.Report
}

// SnapshotBuilder builds the shared read-only diagnostic snapshot backing the
// routing selectors. The production implementation is *Coordinator. The CLI's
// bare `routing` and `routing explain` commands use the snapshot's view methods.
type SnapshotBuilder interface {
	BuildDiagnosticSnapshot(context.Context) DiagnosticSnapshot
}

// TargetStatus is one target's attempted/applied revision in a status report.
type TargetStatus struct {
	TargetID          string `json:"target_id"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	AppliedRevision   uint64 `json:"applied_revision"`
	Pending           bool   `json:"pending"`
}

// providerReason names a provider's decisive condition for the provider
// projection's Reason field.
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

// Compile-time assertions that *Coordinator implements both Mutator (via its
// InitWithOptions/Reconcile/Disable/Enable/Reset/QuotaCheck methods) and Diagnoser.
var (
	_ Diagnoser = (*Coordinator)(nil)
)

// Status renders the merged status view: consolidated provider status, raw
// quota window numbers, routes with skip reasons, pending targets, and one
// global last-checked timestamp. It is read-only: it never mutates state or
// targets. A load or resolution error yields a report carrying only the
// sanitized error string rather than panicking.
func (c *Coordinator) Status(ctx context.Context, _ bool) MergedStatusReport {
	return c.BuildDiagnosticSnapshot(ctx).MergedStatusView()
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
		Observed:         snapshot.ObservedState(),
		DesiredRaw:       snapshot.DesiredRaw(),
		DesiredProviders: desiredProviderIDs(snapshot),
		Now:              func() time.Time { return asOf },
	}

	// Configuration findings come from the preloaded snapshot's captured load
	// errors — no duplicate LoadPolicy or ResolveTargets. Publication uses the
	// JournalPath directly.
	deps.Policy = &preloadedPolicyInspector{snapshot: snapshot}
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
	report.AsOf = asOf

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

func desiredProviderIDs(snapshot DiagnosticSnapshot) map[string]struct{} {
	if snapshot.PolicyError() != nil {
		return nil
	}
	desired := snapshot.DesiredPolicy()
	ids := make(map[string]struct{}, len(desired.Providers))
	for id := range desired.Providers {
		ids[string(id)] = struct{}{}
	}
	return ids
}

// preloadedPolicyInspector surfaces a policy-schema finding from the snapshot's
// captured policy load error without re-loading policy. When policy loaded
// cleanly it contributes no findings (the snapshot's LoadPolicy already
// validated the schema).
type preloadedPolicyInspector struct {
	snapshot DiagnosticSnapshot
}

func (p *preloadedPolicyInspector) Findings(context.Context) []doctor.Finding {
	if p.snapshot.PolicyError() == nil {
		return nil
	}
	if p.snapshot.policyMissing {
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
		Remediation: "fix desired.yaml (or regenerate with `polytoken-quota init --force`)",
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
