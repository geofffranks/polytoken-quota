package service

import (
	"context"
	"fmt"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// SelectionInputs is the narrow read-only business snapshot the selection
// runner consumes: the desired policy, the durable observed state, and the
// single clock sample both are read against. It is deliberately narrower than
// DiagnosticSnapshot: selection reads business inputs only and never resolves
// targets, so target diagnostic failures cannot affect a selection.
type SelectionInputs struct {
	Desired policy.Desired
	State   state.State
	AsOf    time.Time
}

// SelectionInputs performs the only business reads a selection invocation
// needs. The injected clock is sampled exactly once. A missing state file is
// not an error (empty state, matching LoadState); a missing, unreadable, or
// invalid desired.yaml and an unreadable state file are fatal — selection is
// a read-only consumer of both, and there is no safe result without them.
// Target resolution is never attempted, so target diagnostic failures cannot
// reach or stop a selection.
func (c *Coordinator) SelectionInputs(_ context.Context) (SelectionInputs, error) {
	inputs := SelectionInputs{AsOf: c.now()}
	if c.Policy == nil {
		return SelectionInputs{}, fmt.Errorf("load policy failed")
	}
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return SelectionInputs{}, fmt.Errorf("load policy failed: %w", err)
	}
	inputs.Desired = desired
	if c.State == nil {
		return SelectionInputs{}, fmt.Errorf("load state failed")
	}
	observed, err := c.State.LoadState()
	if err != nil {
		return SelectionInputs{}, fmt.Errorf("load state failed: %w", err)
	}
	inputs.State = observed
	return inputs, nil
}
