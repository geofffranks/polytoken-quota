package service

// Publisher adapter. The concrete publish.Publisher.Recover returns
// (state.State, RecoveryReport, error), but the service.Publisher interface's
// Recover returns (state.State, error). This adapter bridges them so the
// Coordinator can use the real Publisher. The RecoveryReport is dropped because
// the Coordinator only needs the recovered committed state for staleness
// detection and the revision counter.

import (
	"context"

	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// PublisherAdapter adapts a concrete publish.Publisher to the service.Publisher
// interface. Recover delegates to the concrete Recover and discards the
// RecoveryReport; ApplyUnderLock passes straight through.
type PublisherAdapter struct {
	Publisher publish.Publisher
}

// Recover delegates to the concrete publish.Publisher.Recover, dropping the
// RecoveryReport so the return satisfies the service.Publisher interface.
func (a PublisherAdapter) Recover(ctx context.Context, prior state.State) (state.State, error) {
	recovered, _, err := a.Publisher.Recover(ctx, prior)
	return recovered, err
}

// ApplyUnderLock delegates to the concrete publish.Publisher.ApplyUnderLock.
// The caller (the Coordinator) already holds the advisory lock, so this must
// not re-acquire it.
func (a PublisherAdapter) ApplyUnderLock(ctx context.Context, tx publish.Transaction) (state.State, error) {
	return a.Publisher.ApplyUnderLock(ctx, tx)
}

// Compile-time assertion that PublisherAdapter satisfies the service.Publisher
// interface.
var _ Publisher = PublisherAdapter{}
