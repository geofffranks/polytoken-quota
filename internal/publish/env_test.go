package publish

// Shared test fixtures and environments for the publish package tests. All
// fixtures are synthetic and non-personal; no real secrets or live config are
// embedded.

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/testutil"
)

// stagedEnv is a fully-wired publication environment under t.TempDir(): one
// managed live file, a staged candidate temp file, a backups directory, a
// journal path, a state store, and a Publisher ready to Apply/Recover.
type stagedEnv struct {
	Publisher Publisher
	Tx        Transaction
	Prior     state.State
	Trace     *faultTrace
	LivePath  string
	TempPath  string
	StatePath string
}

// faultTrace records the ordered fault-step decisions so tests can assert the
// terminal invariant (state durable before journal removal) and that each named
// step was reached.
type faultTrace struct {
	fired []string
	mu    chanMutex
}

type chanMutex struct{ ch chan struct{} }

func newChanMutex() chanMutex {
	m := chanMutex{ch: make(chan struct{}, 1)}
	m.ch <- struct{}{}
	return m
}
func (m chanMutex) lock()   { <-m.ch }
func (m chanMutex) unlock() { m.ch <- struct{}{} }

func (t *faultTrace) fire(step string) {
	t.mu.lock()
	t.fired = append(t.fired, step)
	t.mu.unlock()
}
func (t *faultTrace) firedCopy() []string {
	t.mu.lock()
	defer t.mu.unlock()
	out := make([]string, len(t.fired))
	copy(out, t.fired)
	return out
}

// constLive is the pre-apply live content of the single managed file.
const constLive = "model: zai/glm\nfallback_models:\n  - codex/gpt\n"
const constCandidate = "model: codex/gpt\nfallback_models:\n  - minime/gemma\n"

// newStagedEnv builds a stagedEnv with a single managed file. fault is the
// durable step to fail once ("" disables).
func newStagedEnv(t *testing.T, fault string) stagedEnv {
	t.Helper()
	root := t.TempDir()
	liveDir := filepath.Join(root, "managed")
	livePath := filepath.Join(liveDir, "config.yaml")
	testutil.WriteFile(t, livePath, constLive)

	// Staged candidate temp file (same directory → same filesystem).
	tempPath := filepath.Join(liveDir, ".candidate-config.yaml")
	testutil.WriteFile(t, tempPath, constCandidate)

	journalPath := filepath.Join(root, "journal", "apply.json")
	backupsRoot := filepath.Join(root, "backups")
	statePath := filepath.Join(root, "state.json")
	clock := fixedClock(t)

	store := state.Store{Path: statePath, Now: clock, RecoveredRetention: 24 * time.Hour}
	prior := state.State{
		Schema:    1,
		Revision:  10,
		Providers: map[string]state.ProviderState{},
		Targets: map[string]state.TargetState{
			"global": {AppliedRevision: 9, AttemptedRevision: 9, AppliedAt: clock(), AttemptedAt: clock()},
		},
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

	next := cloneState(prior)
	next.Revision = 11
	next.Targets["global"] = state.TargetState{
		AppliedRevision:   11,
		AttemptedRevision: 11,
		AppliedAt:         clock(),
		AttemptedAt:       clock(),
	}

	trace := &faultTrace{mu: newChanMutex()}
	mode := fileMode(OSFS{}, livePath)

	tx := Transaction{
		Prior:    prior,
		Next:     next,
		TargetID: "global",
		Replacements: []Replacement{{
			LivePath: livePath,
			TempPath: tempPath,
			OldHash:  sha256.Sum256([]byte(constLive)),
			NewHash:  sha256.Sum256([]byte(constCandidate)),
			Mode:     mode,
		}},
	}

	pub := Publisher{
		Locker:      newFileLock(filepath.Join(root, "lock", "apply.lock")),
		State:       store,
		FS:          OSFS{},
		JournalPath: journalPath,
		Backups:     BackupStore{Root: backupsRoot, Limit: 3},
		ManagedRoot: liveDir,
		Clock:       clock,
		Fault:       singleShotFault(trace, fault),
	}
	return stagedEnv{
		Publisher: pub,
		Tx:        tx,
		Prior:     prior,
		Trace:     trace,
		LivePath:  livePath,
		TempPath:  tempPath,
		StatePath: statePath,
	}
}

// singleShotFault returns a FaultHook that fails once when the named step is
// first consulted, recording every decision in trace. After the one failure it
// passes through.
func singleShotFault(trace *faultTrace, fault string) FaultHook {
	fired := make(map[string]bool)
	return func(step string) error {
		trace.fire(step)
		if fault != "" && step == fault && !fired[step] {
			fired[step] = true
			return ErrInjected
		}
		return nil
	}
}

// fixedClock returns a deterministic clock pinned to a stable instant.
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var n uint64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

// rewiredStore returns a fresh non-failing Publisher based on env with the
// fault hook reset to a passthrough recorder.
func (e *stagedEnv) rewiredForRecover() {
	e.Publisher.Fault = singleShotFault(e.Trace, "")
}

// rewireForRecover is the t.Helper-taking alias used by some tests.
func (e *stagedEnv) rewireForRecover(t *testing.T) {
	t.Helper()
	e.rewiredForRecover()
}

// committedState loads the on-disk state.json the Publisher wrote, proving the
// terminal invariant (state durable before journal removal).
func (e *stagedEnv) committedState(t *testing.T) state.State {
	t.Helper()
	s, err := e.Publisher.State.Load()
	if err != nil {
		t.Fatalf("load committed state: %v", err)
	}
	return s
}

func hashHex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}
