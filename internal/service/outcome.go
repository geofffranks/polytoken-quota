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
}

// Outcome is the result of a mutation operation. Accepted is false when the
// operation was rejected (e.g. init refused to overwrite desired.yaml).
type Outcome struct {
	Accepted bool
	Revision uint64
	Targets  []TargetOutcome
	Error    error
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
