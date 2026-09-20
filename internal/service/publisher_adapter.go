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

// BackupLimitSetter is implemented by publishers whose backup retention can be
// retargeted from the loaded policy immediately before an apply. The
// Coordinator type-asserts this at its single apply site so runtime retention
// always tracks operational.backup_count (omitted → policy default 1,
// explicit N → N).
type BackupLimitSetter interface {
	// SetBackupLimit applies the retention count to the publisher. Values
	// below 1 are clamped to 1: the backup store's prune step no-ops at
	// limit <= 0, which would silently mean unbounded retention.
	SetBackupLimit(count int)
}

// SetBackupLimit retargets the underlying publisher's per-file backup retention
// to the policy-loaded operational.backup_count. Pointer receiver: the
// Coordinator holds the adapter by pointer (see newCoordinator), so the
// mutation is visible to the next ApplyUnderLock under the same held
// transaction lock — a value receiver here would silently no-op.
func (a *PublisherAdapter) SetBackupLimit(count int) {
	if count < 1 {
		count = 1
	}
	a.Publisher.Backups.Limit = count
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

// Compile-time assertions that PublisherAdapter satisfies the service.Publisher
// interface by value and by pointer (the pointer form is what production wires,
// so SetBackupLimit is reachable through the Publisher interface), and that the
// pointer adapter implements BackupLimitSetter.
var _ Publisher = PublisherAdapter{}
var _ Publisher = (*PublisherAdapter)(nil)
var _ BackupLimitSetter = (*PublisherAdapter)(nil)
