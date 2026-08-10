package service

import (
	"context"
	"os"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
)

// Production doctor inspectors. Each is read-only and side-effect free: doctor
// must never stage, validate, publish, or otherwise mutate anything. Live
// candidate validation intentionally stays out of doctor — it runs during
// reconcile (including --dry-run), whose pending outcomes doctor already
// surfaces from persisted state.

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
