package service

// selection_inputs_test.go — the narrow business snapshot contract behind
// the selection runner: policy/state fatal, missing state file yields an
// empty state, the clock is sampled once, and target resolution is never
// attempted (a Coordinator with no target registry still snapshots).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// minimalDesiredDoc is a small valid desired.yaml for snapshot tests. The
// model names are synthetic, matching internal/policy test fixtures.
const minimalDesiredDoc = `version: 1
providers:
  codex:
    models:
      - codex/gpt-5.6-sol
global:
  id: global
  root: /home/user/.config/polytoken
  full: [codex/gpt-5.6-sol]
  mini: [codex/gpt-5.6-sol]
  nano: [codex/gpt-5.6-sol]
  classifier: [codex/gpt-5.6-sol]
operational:
  validation_timeout: 30s
  lock_wait: 10s
  recovered_retention: 168h
  backup_count: 5
`

type snapshotClock struct {
	calls int
	t     time.Time
}

func (c *snapshotClock) Now() time.Time {
	c.calls++
	return c.t
}

func newSnapshotCoordinator(t *testing.T, desired string) (*Coordinator, *snapshotClock) {
	t.Helper()
	home := t.TempDir()
	desiredPath := filepath.Join(home, "desired.yaml")
	if desired != "" {
		if err := os.WriteFile(desiredPath, []byte(desired), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clock := &snapshotClock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	coord := &Coordinator{
		Policy: FilePolicyLoader{Path: desiredPath},
		State:  StoreState{Store: state.Store{Path: filepath.Join(home, "state.json")}},
		Clock:  clock,
		// Targets deliberately nil: the narrow snapshot must never resolve
		// targets, so a missing registry cannot fail it.
	}
	return coord, clock
}

func TestSelectionInputsReadsDesiredStateAndSamplesClockOnce(t *testing.T) {
	coord, clock := newSnapshotCoordinator(t, minimalDesiredDoc)
	inputs, err := coord.SelectionInputs(context.Background())
	if err != nil {
		t.Fatalf("SelectionInputs: %v", err)
	}
	if len(inputs.Desired.Providers) != 1 {
		t.Fatalf("providers=%d, want the codex mapping", len(inputs.Desired.Providers))
	}
	if inputs.State.Providers == nil {
		t.Fatal("missing state file must yield an empty state, not an error")
	}
	if !inputs.AsOf.Equal(clock.t) {
		t.Fatalf("as_of=%s, want the injected clock %s", inputs.AsOf, clock.t)
	}
	if clock.calls != 1 {
		t.Fatalf("clock sampled %d times, want exactly once", clock.calls)
	}
}

func TestSelectionInputsPolicyFatal(t *testing.T) {
	coord, _ := newSnapshotCoordinator(t, "") // no desired.yaml
	if _, err := coord.SelectionInputs(context.Background()); err == nil {
		t.Fatal("a missing desired.yaml must be fatal")
	}

	home := t.TempDir()
	broken := filepath.Join(home, "desired.yaml")
	if err := os.WriteFile(broken, []byte("version: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coord2 := &Coordinator{
		Policy: FilePolicyLoader{Path: broken},
		State:  StoreState{Store: state.Store{Path: filepath.Join(home, "state.json")}},
	}
	if _, err := coord2.SelectionInputs(context.Background()); err == nil {
		t.Fatal("an invalid desired.yaml must be fatal")
	}
}

func TestSelectionInputsStateFatal(t *testing.T) {
	home := t.TempDir()
	desiredPath := filepath.Join(home, "desired.yaml")
	if err := os.WriteFile(desiredPath, []byte(minimalDesiredDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory at the state path makes LoadState fail without it being
	// a simple missing file.
	statePath := filepath.Join(home, "state.json")
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{
		Policy: FilePolicyLoader{Path: desiredPath},
		State:  StoreState{Store: state.Store{Path: statePath}},
	}
	if _, err := coord.SelectionInputs(context.Background()); err == nil {
		t.Fatal("an unreadable state file must be fatal")
	}
}

func TestSelectionInputsNilDependenciesFatal(t *testing.T) {
	if _, err := (&Coordinator{}).SelectionInputs(context.Background()); err == nil {
		t.Fatal("a coordinator without a policy loader must be fatal")
	}
	home := t.TempDir()
	desiredPath := filepath.Join(home, "desired.yaml")
	if err := os.WriteFile(desiredPath, []byte(minimalDesiredDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{Policy: FilePolicyLoader{Path: desiredPath}}
	if _, err := coord.SelectionInputs(context.Background()); err == nil {
		t.Fatal("a coordinator without a state store must be fatal")
	}
}
