package service

import (
	"context"
	"fmt"

	"github.com/geofffranks/polytoken-quota/internal/selection"
)

// SelectionSnapshot performs the only business reads a selection invocation
// needs and returns the selection runner's own snapshot type directly: the
// desired policy, the durable observed state, and the single clock sample
// both are read against. Producing selection.Snapshot here makes *Coordinator
// satisfy selection.SnapshotSource itself, so no mirrored type or field-copy
// adapter exists. It is deliberately narrower than DiagnosticSnapshot:
// selection reads business inputs only and never resolves targets, so target
// diagnostic failures cannot affect a selection.
//
// The reads are intentionally lock-free. Every writer commits whole files by
// rename under the mutation lock (Coordinator.transact), so each read
// observes one complete version; selection never writes, and the reconcile
// computations that need a mutually consistent desired/state pair re-read
// both inside that locked path.
//
// The injected clock is sampled exactly once. A missing state file is not an
// error (empty state, matching LoadState); a missing, unreadable, or invalid
// desired.yaml and an unreadable state file are fatal — selection is a
// read-only consumer of both, and there is no safe result without them.
// Target resolution is never attempted, so target diagnostic failures cannot
// reach or stop a selection.
func (c *Coordinator) SelectionSnapshot(_ context.Context) (selection.Snapshot, error) {
	inputs := selection.Snapshot{AsOf: c.now()}
	if c.Policy == nil {
		return selection.Snapshot{}, fmt.Errorf("load policy failed")
	}
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return selection.Snapshot{}, fmt.Errorf("load policy failed: %w", err)
	}
	if desired.ProviderOnly() {
		// Selection ranks enumerated models and projects managed chains —
		// exactly the fields a provider-only policy rejects. Return the clear
		// unsupported result instead of silently selecting over an empty graph.
		// The error wraps both sentinels: the service-level unsupported
		// contract and the selection runner's dedicated fatal classification.
		return selection.Snapshot{}, fmt.Errorf("%w (%w)", providerOnlyUnsupported("select/select-eval",
			"model selection projects managed model chains, which a provider-only policy does not define"), selection.ErrUnsupportedPolicyMode)
	}
	inputs.Desired = desired
	if c.State == nil {
		return selection.Snapshot{}, fmt.Errorf("load state failed")
	}
	observed, err := c.State.LoadState()
	if err != nil {
		return selection.Snapshot{}, fmt.Errorf("load state failed: %w", err)
	}
	inputs.State = observed
	return inputs, nil
}
