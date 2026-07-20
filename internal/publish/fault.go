package publish

import "errors"

// faultStep names a durable publication step that can fail under test. These
// mirror the steps enumerated in the Task 11 brief.
const (
	stepBackup       = "backup"
	stepJournalFsync = "journal-fsync"
	stepTempWrite    = "temp-write"
	stepTempFsync    = "temp-fsync"
	stepRename       = "rename"
	stepDirFsync     = "dir-fsync"
	stepProgress     = "progress"
	stepStatePublish = "state-publish"
	// stepStateFsync is the durability boundary inside state.Store.Save: the
	// state temp file has been written but not yet fsync'd/renamed. A fault here
	// proves the journal survives (state not committed) and Recover converges.
	stepStateFsync    = "state-fsync"
	stepJournalRemove = "journal-remove"
	// stepJournalWrite covers journal file create/write/rename (not a named
	// fault step in the brief, but classified for consistent diagnosis).
	stepJournalWrite = "journal-write"
)

// FaultHook is consulted by Publisher at each named durable step when non-nil.
// Returning a non-nil error aborts Apply at exactly that step, leaving the
// system in the corresponding half-applied state for Recover to repair. It is a
// test-only seam; production leaves it nil.
type FaultHook func(step string) error

// ErrInjected is returned when the fault hook injects a failure.
var ErrInjected = errors.New("publish: injected failure")
