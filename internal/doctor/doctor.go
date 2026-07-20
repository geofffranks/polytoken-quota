// Package doctor produces health and drift diagnostics for the reconciler.
//
// Run collects findings from four injected inspectors (policy, targets, live
// validation, and publication), surfaces every persisted pending target error
// from the observed state, and ages recovered history by the configured
// retention. Recovered-only history is informational and never actionable: if
// no finding requires user action, the report's Actionable method returns false
// and the caller exits 0.
//
// doctor performs no process control — it never inspects, restarts, signals, or
// kills a live daemon. Its live validation dependency reports current
// config-validate and startup-equivalent results only.
package doctor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// Severity classifies a finding's urgency. Info findings are informational and
// never actionable; Warning and Error findings require user action.
type Severity string

// Severity levels.
const (
	Info    Severity = "info"
	Warning Severity = "warning"
	Error   Severity = "error"
)

// Finding is one health or drift diagnostic. Code is a stable machine-readable
// identifier; Message is a human-readable description with full persisted-error
// detail (stage, revisions, timestamps, sanitized summary, reproducibility, and
// live status). TargetID, File, and Chain locate the finding when known;
// Remediation suggests the next step.
type Finding struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	TargetID    string   `json:"target_id"`
	File        string   `json:"file"`
	Chain       string   `json:"chain"`
	Remediation string   `json:"remediation"`
	Severity    Severity `json:"severity"`
}

// Report is a doctor health report. Findings holds every static, live, pending,
// and recovered-adjacent diagnostic; Recovered holds aged recovered-error
// history within the retention window.
type Report struct {
	Findings  []Finding            `json:"findings"`
	Recovered []state.ApplyFailure `json:"recovered"`
}

// Actionable reports whether the report contains any Warning or Error finding
// that requires user action. Recovered-only history is informational and never
// actionable: a report whose only content is recovered errors returns false.
func (r Report) Actionable() bool {
	return slices.ContainsFunc(r.Findings, func(f Finding) bool {
		return f.Severity == Warning || f.Severity == Error
	})
}

// PolicyInspector reports static policy/schema/mapping findings: schema errors,
// unknown or ambiguous provider/model mappings, and enumeration staleness or
// new references versus the live configuration.
type PolicyInspector interface {
	Findings(ctx context.Context) []Finding
}

// TargetInspector reports per-target findings: managed-field drift, uncovered
// model references, empty current chains, and desired chains that cannot
// survive each mapped provider's loss.
type TargetInspector interface {
	Findings(ctx context.Context) []Finding
}

// LiveValidator reports current config-validate and startup-equivalent results
// against live files, plus symlink/path problems in managed definition files.
type LiveValidator interface {
	Findings(ctx context.Context) []Finding
}

// PublishInspector reports journal completeness, backup health, and permission
// problems in the utility root.
type PublishInspector interface {
	Findings(ctx context.Context) []Finding
}

// Dependencies are the injected collaborators Run consults. Every dependency is
// nil-safe: a nil inspector contributes no findings. Now supplies the clock for
// recovered-error aging; when nil Run uses time.Now.
type Dependencies struct {
	Policy    PolicyInspector
	State     state.Store
	Targets   TargetInspector
	Validator LiveValidator
	Publisher PublishInspector
	Now       func() time.Time
}

// Run collects findings from every dependency, surfaces pending target errors
// from persisted state, and ages recovered history by the configured retention.
// It is read-only: it never mutates inputs or persisted state. Recovered errors
// older than the retention window are pruned and absent from the returned
// report.
func Run(ctx context.Context, deps Dependencies) Report {
	var findings []Finding

	if deps.Policy != nil {
		findings = append(findings, deps.Policy.Findings(ctx)...)
	}
	if deps.Targets != nil {
		findings = append(findings, deps.Targets.Findings(ctx)...)
	}
	if deps.Validator != nil {
		findings = append(findings, deps.Validator.Findings(ctx)...)
	}
	if deps.Publisher != nil {
		findings = append(findings, deps.Publisher.Findings(ctx)...)
	}

	// Surface every persisted pending target error as an actionable finding.
	st, loadErr := deps.State.Load()
	if loadErr != nil {
		findings = append(findings, Finding{
			Code:        "state-unreadable",
			Severity:    Error,
			Message:     "could not read state.json: " + loadErr.Error(),
			Remediation: "check state.json format and permissions",
		})
	}
	for id, ts := range st.Targets {
		if ts.Pending != nil {
			findings = append(findings, pendingTargetFinding(id, ts))
		}
	}

	// Age recovered history by the configured retention window.
	now := time.Now()
	if deps.Now != nil {
		now = deps.Now()
	}
	retention := deps.State.RecoveredRetention
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	pruned := state.PruneRecovered(st, now, retention)

	return Report{Findings: findings, Recovered: pruned.Recovered}
}

// pendingTargetFinding builds a target-pending finding from a persisted
// ApplyFailure. The Message carries the full persisted-error detail: last
// successful and latest attempted revision/timestamp, failure stage, affected
// chain/file, sanitized command error, current reproducibility, and live status.
func pendingTargetFinding(id string, ts state.TargetState) Finding {
	af := ts.Pending
	msg := fmt.Sprintf(
		"target %s pending: stage=%s attempted_revision=%d attempted_at=%s last_successful_revision=%d last_successful_at=%s summary=%q reproduces=%v live_status=%s",
		id, af.Stage, af.AttemptedRevision, af.AttemptedAt, af.LastSuccessfulRevision, af.LastSuccessfulAt,
		af.Summary, af.Reproduces, af.LiveStatus,
	)
	remediation := af.Remediation
	if remediation == "" {
		remediation = "resolve the pending error and re-run reconcile"
	}
	return Finding{
		Code:        "target-pending",
		Message:     msg,
		TargetID:    af.TargetID,
		File:        af.File,
		Chain:       af.Chain,
		Remediation: remediation,
		Severity:    Error,
	}
}
