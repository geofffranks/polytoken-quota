// Package publish implements crash-consistent per-target publication for the
// polytoken-quota reconciler. It writes validated candidate bytes to live
// Polytoken files through a write-ahead journal: lock, recover any prior
// journal, create bounded mode-preserving backups, write the journal, apply
// each file via a same-filesystem temp file + atomic rename, publish state.json
// last as the commit record, and finally remove the journal.
//
// The journal records only hashes and paths — never file content, credentials,
// or raw config. On the next invocation Recover compares the journal, live-file
// hashes, backups, and committed state revision and either rolls forward
// (publishes the recorded state), restores the whole target from backup, or
// performs a no-op coherent commit. Every recovery path ends by atomically
// publishing state.json with the accepted event state and final per-target
// outcomes before the journal is removed.
package publish

import (
	"io/fs"
	"os"

	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// JournalSchema is the on-disk journal schema version. It is bumped only on
// incompatible journal-format changes; recovery refuses an unknown schema.
const JournalSchema = 1

// Replacement describes one managed file replacement inside a transaction. The
// live file is replaced atomically by renaming TempPath over LivePath; OldHash
// and NewHash are SHA-256 digests of the prior and intended contents; Mode is
// the source file mode to preserve; Applied is journal progress.
type Replacement struct {
	LivePath   string
	TempPath   string
	BackupPath string
	OldHash    [32]byte
	NewHash    [32]byte
	Mode       fs.FileMode
	Applied    bool
}

// Journal is the write-ahead record for one target transaction. It carries
// schema, next/prior state revisions, target id, every intended replacement,
// per-file applied progress, and the intended target outcome. It stores only
// hashes and paths — never file content, credentials, or raw config.
type Journal struct {
	Schema        int
	PriorRevision uint64
	NextRevision  uint64
	TargetID      string
	// ManagedRoot is the canonical target root the transaction's live paths
	// were validated against. Recovery re-validates every journal path
	// against it before reading or writing anything.
	ManagedRoot  string
	Replacements []Replacement
	Intended     state.TargetState
}

// Transaction is the input to Publisher.Apply. Prior is the committed observed
// state before this mutation; Next is the accepted state to commit; TargetID is
// the target being applied; Replacements are the validated candidate file
// writes. ManagedRoot is the canonical root of the target being applied: every
// replacement's live path must stay within it. When empty, the Publisher-level
// ManagedRoot (if any) applies instead.
type Transaction struct {
	Prior        state.State
	Next         state.State
	TargetID     string
	ManagedRoot  string
	Replacements []Replacement
}

// RecoveryReport summarizes a single recovery invocation. CleanupError is a
// non-fatal diagnostic: the committed state is already durable, but a stale
// journal (or backup) could not be removed and will be retried/ignored on the
// next recovery. It never fails the recovery itself.
type RecoveryReport struct {
	Action       string // "noop", "roll-forward", "restore", or "corrupt"
	TargetID     string
	CleanupError string
}

// Recovery action names.
const (
	ActionRollForward = "roll-forward"
	ActionRestore     = "restore"
	ActionNoop        = "noop"
	// ActionCorrupt marks a recovery that could not proceed because the journal
	// was unparseable or was written for a different base state. The journal is
	// left in place for an operator to inspect; the accepted event is preserved
	// at the prior committed revision.
	ActionCorrupt = "corrupt"
)

// DurableFS abstracts the fsync + atomic-rename primitives used by publication.
// Production uses OSFS, which performs the real os/syscalls so durability is
// exercised against the kernel. Test injection does not use a fake filesystem:
// the Publisher consults a FaultHook (see fault.go) at each named durable step,
// while OSFS still performs the real operation. CreateTemp opens a fresh temp
// file in the same directory as the target for an atomic rename; Open re-opens
// an already-written temp so it can be Fsync'd before the rename; SyncDir fsyncs
// a directory so a rename/create/unlink entry is durable. MkdirAll, Stat,
// ReadFile, WriteFile, and RemoveAll are the usual whole-file/directory helpers.
type DurableFS interface {
	MkdirAll(path string, perm os.FileMode) error
	Stat(path string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	// WriteFile writes data to name with perm.
	WriteFile(name string, data []byte, perm os.FileMode) error
	// CreateTemp creates and opens a new temp file in dir with the given prefix
	// pattern, and returns the open *os.File. Used for same-filesystem temp
	// writes before an atomic rename.
	CreateTemp(dir, pattern string) (*os.File, error)
	// Open opens an existing file for read/write and returns the open *os.File.
	// Used to fsync an already-written temp file before rename.
	Open(name string) (*os.File, error)
	// Fsync flushes the given open file to stable storage.
	Fsync(f *os.File) error
	// Rename atomically renames oldpath to newpath (same directory).
	Rename(oldpath, newpath string) error
	// SyncDir opens dir and fsyncs it, making directory-entry changes durable.
	SyncDir(dir string) error
	// RemoveAll removes path and any children.
	RemoveAll(path string) error
}
