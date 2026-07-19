// Package doctor produces health and drift diagnostics for the reconciler.
//
// This file declares only the minimal Report type needed to compile the CLI
// shell. Task 13 fills in the findings, severity, and the real Actionable logic.
package doctor

// Report is a doctor health report. Stub: empty now; Task 13 adds findings and
// severity.
type Report struct{}

// Actionable reports whether the report requires user action.
// Stub: always false until Task 13 populates findings.
func (Report) Actionable() bool { return false }
