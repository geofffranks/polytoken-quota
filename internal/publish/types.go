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
	Replacements  []Replacement
	Intended      state.TargetState
}

// Transaction is the input to Publisher.Apply. Prior is the committed observed
// state before this mutation; Next is the accepted state to commit; TargetID is
// the target being applied; Replacements are the validated candidate file
// writes.
type Transaction struct {
	Prior        state.State
	Next         state.State
	TargetID     string
	Replacements []Replacement
}

// RecoveryReport summarizes a single recovery invocation.
type RecoveryReport struct {
	Action   string // "noop", "roll-forward", or "restore"
	TargetID string
}

// Recovery action names.
const (
	ActionRollForward = "roll-forward"
	ActionRestore     = "restore"
	ActionNoop        = "noop"
)

// DurableFS abstracts the fsync + atomic-rename primitives used by publication
// so failures can be injected at every durable step in tests. Production uses
// OSFS, which calls the real os package; tests use FaultFS to inject errors.
//
// Each method that writes durable state is counted by FaultFS so tests can fail
// a specific step. WriteTemp writes data to a fresh temp file in the same
// directory as dst, returns the temp path, and is the file later renamed. Fsync
// flushes a path's data to stable storage. Rename is the atomic rename. SyncDir
// fsyncs the parent directory so the rename entry is durable. RemoveAll deletes
// a path recursively. MkdirAll creates directories. Stat reports file info.
// ReadFile and WriteFile read/write whole files.
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
	// RenameAtCount is a hook used by the faulting filesystem to classify the
	// rename step; it is a no-op for OSFS.
}
