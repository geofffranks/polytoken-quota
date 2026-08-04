// Package service holds the reconciler's service-level types. Outcome is the
// shared result every Mutator operation returns and that the CLI maps to a
// process exit code.
package service

import "github.com/geofffranks/codexbar-hooks/internal/state"

// TargetOutcome describes the result of attempting to reconcile a single target
// (the global target or a registered project target).
type TargetOutcome struct {
	TargetID          string
	AttemptedRevision uint64
	AppliedRevision   uint64
	// Pending is non-nil when the target could not be fully reconciled.
	Pending *state.ApplyFailure
	// StagingRoot is populated only when an explicitly requested dry-run retains
	// a failed staging candidate for diagnosis.
	StagingRoot string
}

// Outcome is the result of a mutation operation. Accepted is false when the
// operation was rejected (e.g. init refused to overwrite desired.yaml).
type Outcome struct {
	Accepted bool
	Revision uint64
	Targets  []TargetOutcome
	Error    error
	// Problem is true when an accepted observation transition left a pending
	// provider problem (a failed poll attempt) even when no target is pending.
	// The CLI maps it to exit code 2 alongside PendingCount. It is set only by
	// QuotaCheck; other mutators never set it, so their exit codes are unchanged.
	Problem bool
	// ProviderAttempts carries sanitized quota polling diagnostics for CLI reports.
	ProviderAttempts []QuotaAttemptDiagnostic
}

// PendingCount returns the number of targets that remain pending (not fully
// reconciled).
func (o Outcome) PendingCount() int {
	n := 0
	for _, t := range o.Targets {
		if t.Pending != nil {
			n++
		}
	}
	return n
}
