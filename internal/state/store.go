package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PruneRecovered removes recovered errors whose ResolvedAt is at or before the
// retention cutoff (now - retention). It returns a state with a filtered
// Recovered slice and never mutates the input state.
func PruneRecovered(s State, now time.Time, retention time.Duration) State {
	cutoff := now.Add(-retention)
	kept := make([]ApplyFailure, 0, len(s.Recovered))
	for _, f := range s.Recovered {
		if f.ResolvedAt.After(cutoff) {
			kept = append(kept, f)
		}
	}
	next := s
	next.Recovered = kept
	return next
}

// Load reads and decodes the state file. A missing file returns a fresh, empty
// state with initialized maps and no error. Nil maps from a sparse file are
// normalized to empty maps so callers can assign without panicking.
func (st Store) Load() (State, error) {
	data, err := os.ReadFile(st.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return newState(), nil
		}
		return State{}, fmt.Errorf("state: read %s: %w", st.Path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("state: parse %s: %w", st.Path, err)
	}
	if s.Providers == nil {
		s.Providers = map[string]ProviderState{}
	}
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	return s, nil
}

// Save prunes recovered errors older than RecoveredRetention (by Now) and
// atomically writes the state with mode 0600. The write is atomic via a
// same-directory temporary file and rename, so a reader never observes a
// partial state file. The persisted JSON contains only sanitized state fields —
// never provider credentials, account names, auth blocks, or unmanaged source
// content.
func (st Store) Save(s State) error {
	s = PruneRecovered(s, st.now(), st.RecoveredRetention)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}
	dir := filepath.Dir(st.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: create dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmp, st.Path); err != nil {
		cleanup()
		return fmt.Errorf("state: rename temp: %w", err)
	}
	return nil
}

func (st Store) now() time.Time {
	if st.Now != nil {
		return st.Now()
	}
	return time.Now()
}

func newState() State {
	return State{
		Providers: map[string]ProviderState{},
		Targets:   map[string]TargetState{},
	}
}
