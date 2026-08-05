package publish

import (
	"encoding/json"
	"fmt"
	"os"
)

// journalFile is the on-disk representation of Journal. It is a separate type so
// the durable (marshaled) form is stable and clearly never carries file content
// — only schema, revisions, target id, hashes, paths, modes, progress, and the
// intended target outcome.
type journalFile struct {
	Schema        int                  `json:"schema"`
	PriorRevision uint64               `json:"prior_revision"`
	NextRevision  uint64               `json:"next_revision"`
	TargetID      string               `json:"target_id"`
	ManagedRoot   string               `json:"managed_root,omitempty"`
	Replacements  []journalReplacement `json:"replacements"`
	Intended      intendedTarget       `json:"intended"`
}

type journalReplacement struct {
	LivePath   string `json:"live_path"`
	TempPath   string `json:"temp_path"`
	BackupPath string `json:"backup_path"`
	OldHash    string `json:"old_hash"` // hex sha-256
	NewHash    string `json:"new_hash"` // hex sha-256
	Mode       uint32 `json:"mode"`
	Applied    bool   `json:"applied"`
}

// intendedTarget is the durable projection of state.TargetState. It carries no
// secrets — only revision/timestamp/pending-outcome fields needed to reconstruct
// the committed target outcome on roll-forward.
type intendedTarget struct {
	AttemptedRevision uint64 `json:"attempted_revision"`
	AppliedRevision   uint64 `json:"applied_revision"`
	AttemptedAtUnix   int64  `json:"attempted_at_unix"`
	AppliedAtUnix     int64  `json:"applied_at_unix"`
	Pending           *bool  `json:"pending,omitempty"` // presence + value; nil=false
	Stage             string `json:"stage,omitempty"`
	Summary           string `json:"summary,omitempty"`
	LiveStatus        string `json:"live_status,omitempty"`
}

// writeJournal durably persists j to path via the durable FS. The bytes are
// fsync'd before the function returns, so the journal is stable before any
// live-file rename. It stores only hashes and paths — never file content,
// credentials, or raw config. The FaultHook (if any) is consulted at the
// journal-fsync step before the temp file is fsynced.
func writeJournal(fs DurableFS, path string, j Journal, hook FaultHook) error {
	if fs == nil {
		fs = OSFS{}
	}
	jf := toJournalFile(j)
	data, err := json.MarshalIndent(jf, "", "  ")
	if err != nil {
		return fmt.Errorf("publish: encode journal: %w", err)
	}
	dir := parentDir(path)
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return errStep(stepJournalWrite, err)
	}
	f, err := fs.CreateTemp(dir, ".journal-*.json.tmp")
	if err != nil {
		return errStep(stepTempWrite, err)
	}
	tmpName := f.Name()
	cleanup := func() { _ = fs.RemoveAll(tmpName) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return errStep(stepJournalWrite, err)
	}
	// Fault the journal fsync here: the durable guarantee is the temp fsync
	// before rename. If the hook fails at journal-fsync, the journal is not yet
	// durable and Apply aborts before touching any live file.
	if hook != nil {
		if err := hook(stepJournalFsync); err != nil {
			_ = f.Close()
			cleanup()
			return err
		}
	}
	if err := fs.Fsync(f); err != nil {
		_ = f.Close()
		cleanup()
		return errStep(stepJournalFsync, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return errStep(stepJournalFsync, err)
	}
	if err := fs.Rename(tmpName, path); err != nil {
		cleanup()
		return errStep(stepJournalWrite, err)
	}
	if err := fs.SyncDir(dir); err != nil {
		return errStep(stepJournalFsync, err)
	}
	return nil
}

// readJournal loads and validates the journal at path. A missing file returns
// ok=false with a nil error (no journal → no recovery). A corrupt or
// schema-incompatible file returns an error.
func readJournal(fs DurableFS, path string) (Journal, bool, error) {
	if fs == nil {
		fs = OSFS{}
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Journal{}, false, nil
		}
		return Journal{}, false, fmt.Errorf("publish: read journal: %w", err)
	}
	var jf journalFile
	if err := json.Unmarshal(data, &jf); err != nil {
		return Journal{}, false, fmt.Errorf("publish: parse journal: %w", err)
	}
	if jf.Schema != JournalSchema {
		return Journal{}, false, fmt.Errorf("publish: journal schema %d != %d", jf.Schema, JournalSchema)
	}
	return fromJournalFile(jf), true, nil
}

// removeJournal deletes the journal path, returning any error.
func removeJournal(fs DurableFS, path string) error {
	if fs == nil {
		fs = OSFS{}
	}
	return fs.RemoveAll(path)
}

func toJournalFile(j Journal) journalFile {
	jf := journalFile{
		Schema:        ifZero(j.Schema, JournalSchema),
		PriorRevision: j.PriorRevision,
		NextRevision:  j.NextRevision,
		TargetID:      j.TargetID,
		ManagedRoot:   j.ManagedRoot,
		Intended:      toIntended(j.Intended),
	}
	for _, r := range j.Replacements {
		jf.Replacements = append(jf.Replacements, journalReplacement{
			LivePath:   r.LivePath,
			TempPath:   r.TempPath,
			BackupPath: r.BackupPath,
			OldHash:    hexEncode(r.OldHash[:]),
			NewHash:    hexEncode(r.NewHash[:]),
			Mode:       uint32(r.Mode.Perm()),
			Applied:    r.Applied,
		})
	}
	return jf
}

func fromJournalFile(jf journalFile) Journal {
	j := Journal{
		Schema:        jf.Schema,
		PriorRevision: jf.PriorRevision,
		NextRevision:  jf.NextRevision,
		TargetID:      jf.TargetID,
		ManagedRoot:   jf.ManagedRoot,
		Intended:      fromIntended(jf.Intended),
	}
	for _, r := range jf.Replacements {
		rep := Replacement{
			LivePath:   r.LivePath,
			TempPath:   r.TempPath,
			BackupPath: r.BackupPath,
			Mode:       parseMode(r.Mode),
			Applied:    r.Applied,
		}
		copy(rep.OldHash[:], hexDecode(r.OldHash))
		copy(rep.NewHash[:], hexDecode(r.NewHash))
		j.Replacements = append(j.Replacements, rep)
	}
	return j
}

func ifZero(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}
