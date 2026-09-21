package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// newSelectionInitFixture wires a Coordinator whose policy layer is real
// (FilePolicyLoader + policy.NewWriter over a temp desired.yaml) while the
// remaining dependencies stay spy stubs. desired == nil models the
// first-init-absent case.
func newSelectionInitFixture(t *testing.T, desired []byte) (*Coordinator, string) {
	t.Helper()
	desiredPath := filepath.Join(t.TempDir(), "desired.yaml")
	if desired != nil {
		if err := os.WriteFile(desiredPath, desired, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spy := newCoordinatorSpy()
	spy.Coordinator.Policy = FilePolicyLoader{Path: desiredPath}
	spy.Coordinator.PolicyWriter = policy.NewWriter(desiredPath)
	spy.Coordinator.Sources = testSourceReader{}
	return &spy.Coordinator, desiredPath
}

// selectionInitDoc is a valid existing desired.yaml carrying an explicit
// operator selection section, including the documented versioned model pin.
var selectionInitDoc = []byte(`version: 1
providers:
  codex:
    models: [codex/gpt]
selection:
  jev:
    enabled: true
    model: jev-1.13.0
    timeout: 45s
`)

// TestForcedInitImportsExistingSelectionSection proves forced init imports the
// existing file's selection section into the replacement: the managed fields
// are re-adopted from source while the operator's selection.jev intent
// survives byte-for-byte into the rewritten policy.
func TestForcedInitImportsExistingSelectionSection(t *testing.T) {
	coord, desiredPath := newSelectionInitFixture(t, selectionInitDoc)
	out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v want accepted", out)
	}
	data, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := policy.Load(desiredPath)
	if err != nil {
		t.Fatalf("reloaded replacement: %v", err)
	}
	if !loaded.Selection.Jev.Enabled {
		t.Fatal("forced init lost selection.jev.enabled=true")
	}
	if loaded.Selection.Jev.Model != policy.DocumentedJevModel {
		t.Fatalf("model = %q, want documented pin %q", loaded.Selection.Jev.Model, policy.DocumentedJevModel)
	}
	if loaded.Selection.Jev.Timeout != 45*time.Second {
		t.Fatalf("timeout = %s, want 45s", loaded.Selection.Jev.Timeout)
	}
	if !bytes.Contains(data, []byte("selection:")) || !bytes.Contains(data, []byte("timeout: 45s")) {
		t.Fatalf("replacement bytes lost the selection section:\n%s", data)
	}
	// The managed fields were genuinely re-adopted from the source reader.
	if _, ok := loaded.Providers["codex"]; !ok {
		t.Fatalf("replacement should re-import the codex mapping, got %+v", loaded.Providers)
	}
}

// TestForcedInitWithoutSelectionStaysDefault proves an existing policy without
// a selection section yields a replacement at the documented defaults, with no
// selection section synthesized into the bytes.
func TestForcedInitWithoutSelectionStaysDefault(t *testing.T) {
	coord, desiredPath := newSelectionInitFixture(t, validInitDesired())
	out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v want accepted", out)
	}
	data, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := policy.Load(desiredPath)
	if err != nil {
		t.Fatalf("reloaded replacement: %v", err)
	}
	if loaded.Selection != (policy.SelectionConfig{
		Jev:  policy.JevSelectionConfig{Enabled: false, Model: policy.DocumentedJevModel, Timeout: policy.DefaultJevTimeout},
		Laya: policy.LayaSelectionConfig{Enabled: false, Timeout: policy.DefaultLayaTimeout},
	}) {
		t.Fatalf("selection = %+v, want defaults", loaded.Selection)
	}
	if bytes.Contains(data, []byte("selection:")) {
		t.Fatalf("default selection should not be written, got:\n%s", data)
	}
}

// TestFirstInitAbsentHasNoSelectionSection proves plain init on an absent
// desired.yaml creates a policy at the documented selection defaults without
// writing the section.
func TestFirstInitAbsentHasNoSelectionSection(t *testing.T) {
	coord, desiredPath := newSelectionInitFixture(t, nil)
	out := coord.InitWithOptions(context.Background(), InitOptions{})
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v want accepted", out)
	}
	data, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("selection:")) {
		t.Fatalf("first init should not write a selection section, got:\n%s", data)
	}
	loaded, err := policy.Load(desiredPath)
	if err != nil {
		t.Fatalf("loaded created file: %v", err)
	}
	if loaded.Selection.Jev.Enabled || loaded.Selection.Jev.Model != policy.DocumentedJevModel || loaded.Selection.Jev.Timeout != policy.DefaultJevTimeout {
		t.Fatalf("selection = %+v, want disabled at documented pin and default timeout", loaded.Selection.Jev)
	}
}

// TestInitUnreadableExistingWithSelectionAborts proves an unreadable existing
// desired.yaml aborts forced init safely: a deterministically injected
// permission failure rejects the transaction during the init preflight —
// before any writer runs — and the durable bytes stay untouched.
func TestInitUnreadableExistingWithSelectionAborts(t *testing.T) {
	coord, desiredPath := newSelectionInitFixture(t, selectionInitDoc)
	coord.Policy = initPolicyLoader{FilePolicyLoader: FilePolicyLoader{Path: desiredPath}, err: os.ErrPermission}
	before, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
	if out.Accepted || out.Error == nil {
		t.Fatalf("out=%+v want rejection", out)
	}
	if !errors.Is(out.Error, os.ErrPermission) {
		t.Fatalf("error %v should wrap the injected permission failure", out.Error)
	}
	after, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected init mutated durable bytes:\nbefore=%q\nafter =%q", before, after)
	}
}

// TestInitCorruptExistingWithSelectionAborts proves a corrupt existing
// desired.yaml — even one carrying a selection section — aborts init under
// every mode and leaves the durable bytes untouched.
func TestInitCorruptExistingWithSelectionAborts(t *testing.T) {
	corrupt := append(bytes.Clone(selectionInitDoc), []byte("  jev:\n    timeout: [unclosed\n")...)
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "force"}[force], func(t *testing.T) {
			coord, desiredPath := newSelectionInitFixture(t, corrupt)
			before, err := os.ReadFile(desiredPath)
			if err != nil {
				t.Fatal(err)
			}
			out := coord.InitWithOptions(context.Background(), InitOptions{Force: force})
			if out.Accepted || out.Error == nil {
				t.Fatalf("out=%+v want rejection", out)
			}
			after, err := os.ReadFile(desiredPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("rejected init mutated durable bytes:\nbefore=%q\nafter =%q", before, after)
			}
		})
	}
}
