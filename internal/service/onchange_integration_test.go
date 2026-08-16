package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// memStore is a concurrency-safe in-memory StateStore that returns the latest
// saved state on load (unlike coordinatorSpy, which always returns fresh
// state).
type memStore struct {
	mu sync.Mutex
	s  state.State
}

func (m *memStore) LoadState() (state.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, nil
}

func (m *memStore) Save(s state.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = s
	return nil
}

// logLock records lock/unlock ordering into a file it shares with the action
// script, proving the action executes between lock holds (outside the lock).
type logLock struct{ path string }

func (l logLock) Lock(context.Context) (func() error, error) {
	appendLine(l.path, "lock")
	return func() error {
		appendLine(l.path, "unlock")
		return nil
	}, nil
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func writeShellAction(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write action: %v", err)
	}
	return path
}

func onChangeFixture(t *testing.T, run string) (policy.Desired, []RegisteredTarget, []TargetOutcome) {
	t.Helper()
	desired := fixtureDesired()
	desired.Operational.NoticePath = filepath.Join(t.TempDir(), "notice", "notice.json")
	desired.Operational.OnChange = []policy.OnChangeAction{{Run: run, TimeoutSeconds: 5}}
	rt := RegisteredTarget{Policy: desired.Global}
	outcomes := []TargetOutcome{{
		TargetID: targetID(rt),
		Prepare: &PrepareResult{
			PlanComputed: true,
			ChangedFiles: map[string]bool{"config.yaml": true},
			ChangedEdits: []reconcile.FieldEdit{
				{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("codex/gpt-5.6-luna")},
			},
		},
	}}
	return desired, []RegisteredTarget{rt}, outcomes
}

// TestOnChangeRunsOutsideLockOncePerRevision: after a proven-change
// publication, actions execute exactly once for the revision, between the
// reserve unlock and the record lock — never while the lock is held.
func TestOnChangeRunsOutsideLockOncePerRevision(t *testing.T) {
	dir := t.TempDir()
	orderLog := filepath.Join(dir, "order.log")
	action := writeShellAction(t, dir, "act.sh", "printf 'action\\n' >> "+orderLog+"\n")

	desired, targets, outcomes := onChangeFixture(t, action)
	spy := newCoordinatorSpy()
	spy.Coordinator.Lock = logLock{path: orderLog}
	store := &memStore{s: state.State{Revision: 5, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
	spy.Coordinator.State = store

	st := store.s
	if c := &spy.Coordinator; c.notifyTargets(desired, &st, targets, outcomes) {
		t.Fatalf("publication should not record a failure")
	}
	if spy.Coordinator.pendingChange == nil {
		t.Fatalf("pendingChange not stashed after proven-change publication")
	}
	if _, err := os.Stat(desired.Operational.NoticePath); err != nil {
		t.Fatalf("notice not published: %v", err)
	}

	spy.Coordinator.runPendingOnChange(context.Background())

	lines := strings.Split(strings.TrimSpace(readTestFile(t, orderLog)), "\n")
	// Success path: reserve (lock/unlock), action, and no further locking —
	// nothing to record when every action succeeded.
	wantOrder := []string{"lock", "unlock", "action"}
	if len(lines) != len(wantOrder) {
		t.Fatalf("order = %v, want exactly %v", lines, wantOrder)
	}
	for i := range wantOrder {
		if lines[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", lines, wantOrder)
		}
	}
	if got := strings.Count(strings.Join(lines, "\n"), "action"); got != 1 {
		t.Fatalf("action ran %d times, want exactly 1", got)
	}
	loaded, _ := store.LoadState()
	if loaded.OnChangeExecutedRevision != 5 {
		t.Fatalf("OnChangeExecutedRevision = %d, want 5", loaded.OnChangeExecutedRevision)
	}

	// A second invocation for the same revision must skip (at-most-once).
	spy.Coordinator.pendingChange = &pendingChange{revision: 5, notice: []byte("{}"), actions: desired.Operational.OnChange}
	spy.Coordinator.runPendingOnChange(context.Background())
	if got := strings.Count(readTestFile(t, orderLog), "action"); got != 1 {
		t.Fatalf("action re-ran for the same revision (%d occurrences)", got)
	}
}

// TestOnChangeFailureRecordsEvent: a failing action produces a sanitized
// on-change-failed notice event in state; nothing panics and the marker
// revision is still recorded.
func TestOnChangeFailureRecordsEvent(t *testing.T) {
	dir := t.TempDir()
	action := writeShellAction(t, dir, "fail.sh", "echo 'boom sk-ant-secret' >&2\nexit 3\n")

	desired, targets, outcomes := onChangeFixture(t, action)
	spy := newCoordinatorSpy()
	store := &memStore{s: state.State{Revision: 7, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
	spy.Coordinator.State = store

	st := store.s
	(&spy.Coordinator).notifyTargets(desired, &st, targets, outcomes)
	spy.Coordinator.runPendingOnChange(context.Background())

	loaded, _ := store.LoadState()
	if loaded.OnChangeExecutedRevision != 7 {
		t.Fatalf("OnChangeExecutedRevision = %d, want 7", loaded.OnChangeExecutedRevision)
	}
	var failed *state.EventRecord
	for i := range loaded.EventHistory.Events {
		e := loaded.EventHistory.Events[i]
		if e.Category == state.EventNotice && e.Action == "on-change-failed" {
			failed = &loaded.EventHistory.Events[i]
		}
	}
	if failed == nil {
		t.Fatalf("no on-change-failed event recorded: %+v", loaded.EventHistory.Events)
	}
	if failed.Result != state.EventFailed || !strings.Contains(failed.Reason, "fail.sh") {
		t.Fatalf("event = %+v", failed)
	}
	if strings.Contains(failed.Reason, "sk-ant-secret") {
		t.Fatalf("event reason leaks action stderr verbatim: %q", failed.Reason)
	}
}

// TestOnChangeNoActionsConfigured: with no on_change configured, publication
// stashes nothing and runPendingOnChange is a no-op.
func TestOnChangeNoActionsConfigured(t *testing.T) {
	desired, targets, outcomes := onChangeFixture(t, "/no/such/action")
	desired.Operational.OnChange = nil

	spy := newCoordinatorSpy()
	store := &memStore{s: state.State{Revision: 3, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
	spy.Coordinator.State = store

	st := store.s
	(&spy.Coordinator).notifyTargets(desired, &st, targets, outcomes)
	if spy.Coordinator.pendingChange != nil {
		t.Fatalf("pendingChange stashed with no actions configured")
	}
	spy.Coordinator.runPendingOnChange(context.Background()) // must be a silent no-op
	loaded, _ := store.LoadState()
	if loaded.OnChangeExecutedRevision != 0 {
		t.Fatalf("OnChangeExecutedRevision = %d, want 0", loaded.OnChangeExecutedRevision)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
