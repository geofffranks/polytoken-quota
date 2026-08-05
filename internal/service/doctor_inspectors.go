package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// Production doctor inspectors. Each is read-only and side-effect free: doctor
// must never stage, validate, publish, or otherwise mutate anything. Live
// candidate validation intentionally stays out of doctor — it runs during
// reconcile (including --dry-run), whose pending outcomes doctor already
// surfaces from persisted state.

// PolicyDoctorInspector reports desired-policy load/validation failures
// (missing file, schema errors, unresolved or ambiguous mappings) as findings.
type PolicyDoctorInspector struct {
	Loader PolicyLoader
}

// Findings implements the doctor policy inspector.
func (i PolicyDoctorInspector) Findings(context.Context) []doctor.Finding {
	if i.Loader == nil {
		return nil
	}
	if !i.Loader.DesiredExists() {
		return []doctor.Finding{{
			Code:        "policy-schema",
			Message:     "desired.yaml does not exist",
			Remediation: "run `polytoken-quota init` to create the initial policy",
			Severity:    doctor.Error,
		}}
	}
	if _, err := i.Loader.LoadPolicy(); err != nil {
		return []doctor.Finding{{
			Code:        "policy-schema",
			Message:     fmt.Sprintf("desired.yaml failed validation: %s", quota.SanitizeText(err.Error())),
			Remediation: "fix desired.yaml (or regenerate with `polytoken-quota sync --from-polytoken`)",
			Severity:    doctor.Error,
		}}
	}
	return nil
}

// TargetDoctorInspector resolves every registered target and reports
// resolution failures: path traversal, symlinked managed definitions, missing
// definition files, and unresolvable roots.
type TargetDoctorInspector struct {
	Loader  PolicyLoader
	Targets TargetRegistry
}

// Findings implements the doctor target inspector.
func (i TargetDoctorInspector) Findings(context.Context) []doctor.Finding {
	if i.Loader == nil || i.Targets == nil || !i.Loader.DesiredExists() {
		return nil
	}
	desired, err := i.Loader.LoadPolicy()
	if err != nil {
		return nil // the policy inspector owns load failures
	}
	if _, err := i.Targets.ResolveTargets(desired); err != nil {
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
	return nil
}

// PublishDoctorInspector reports an interrupted publication: a leftover
// write-ahead journal means the last apply did not complete and the next
// mutation will run recovery.
type PublishDoctorInspector struct {
	JournalPath string
}

// Findings implements the doctor publish inspector.
func (i PublishDoctorInspector) Findings(context.Context) []doctor.Finding {
	if i.JournalPath == "" {
		return nil
	}
	if _, err := os.Stat(i.JournalPath); err != nil {
		return nil // no journal (or unreadable path) → nothing to report here
	}
	return []doctor.Finding{{
		Code:        "journal-incomplete",
		Message:     "a write-ahead apply journal is present: the last publication did not complete",
		Remediation: "run `polytoken-quota reconcile` to recover the interrupted transaction",
		Severity:    doctor.Warning,
	}}
}
