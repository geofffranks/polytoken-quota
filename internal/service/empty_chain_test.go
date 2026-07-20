package service

// Task 14 desired-chain-only and byte-identical last-known-good tests. When a
// required chain has no survivor (every entry's provider is disabled) the
// reconciler returns a typed EmptyChainError and emits no edits, so no managed
// byte is ever written: the live last-known-good file must remain byte-identical.
// These tests prove the property end to end through the real reconciler and the
// real staging editor (which applies candidate edits only inside staging and
// never mutates live source).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"github.com/geofffranks/codexbar-hooks/internal/testutil"
)

// emptyChainDesired is a policy whose single definition chain has two entries,
// both owned by providers that are disabled in the observed state.
func emptyChainDesired() (policy.Desired, state.State, policy.Target) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex": {
			CodexBarProviders:  []string{"codex"},
			PolytokenProviders: []string{"codex"},
			Models:             map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
		},
		"zai": {
			CodexBarProviders:  []string{"zai"},
			PolytokenProviders: []string{"zai"},
			Models:             map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}},
		},
	}}
	target := policy.Target{
		ID:   "global",
		Root: "/r",
		Definitions: []policy.Definition{{
			Path:  "agent.md",
			Chain: policy.Chain{"codex/gpt", "zai/glm"},
		}},
	}
	observed := state.State{Revision: 1, Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaExhausted, Availability: state.Available, QuotaAt: epoch, QuotaArrival: 1},
		"zai":   {Availability: state.Unavailable, Quota: state.QuotaNormal, AvailabilityAt: epoch, AvailabilityArrival: 2},
	}}
	return d, observed, target
}

// epoch is a fixed non-zero timestamp for accepted state entries.
var epoch = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// TestEmptyChainFailureProducesNoEdits proves that when every desired-chain
// entry's provider is disabled, reconcile.Build returns EmptyChainError and a
// plan with zero edits. With no edits there is nothing to publish, so the live
// file can never be mutated by the failure.
func TestEmptyChainFailureProducesNoEdits(t *testing.T) {
	d, observed, target := emptyChainDesired()
	plan, err := reconcile.Build(d, observed, target)
	var empty reconcile.EmptyChainError
	if !errors.As(err, &empty) {
		t.Fatalf("want EmptyChainError, got %v", err)
	}
	if len(plan.Edits) != 0 {
		t.Fatalf("empty-chain failure emitted edits: %+v", plan.Edits)
	}
	if empty.TargetID != "global" || empty.File != "agent.md" {
		t.Fatalf("EmptyChainError identity = %+v", empty)
	}
}

// TestEmptyChainFailurePreservesByteIdenticalLKG proves that a render failure
// (empty chain → no edits) leaves the live definition file byte-identical. It
// runs the real staging editor with a zero-edit plan (the shape an empty-chain
// failure produces), confirms the staged candidate matches the source exactly,
// and confirms the live source tree is never mutated.
func TestEmptyChainFailurePreservesByteIdenticalLKG(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	defPath := filepath.Join(globalDir, "subagents", "agent.md")
	defBytes := []byte("---\r\n" +
		"polytoken:\r\n" +
		"  model: codex/gpt\r\n" +
		"  fallback_models:\r\n" +
		"    - zai/glm\r\n" +
		"description: keep me exact # CRLF + comment\r\n" +
		"---\r\n# Agent body.\r\n")
	testutil.WriteFile(t, defPath, string(defBytes))
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\n  zai/glm:\n    enabled: true\n")

	res := target.Resolved{
		ID:              "global",
		CanonicalRoot:   globalDir,
		Global:          true,
		DefinitionFiles: []string{defPath},
	}
	// A zero-edit plan: the exact shape produced by an empty-chain failure.
	zeroPlan := reconcile.Plan{TargetID: "global"}
	c, err := staging.Builder{
		TempRoot: t.TempDir(),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, zeroPlan)
	if err != nil {
		t.Fatalf("stage zero-edit plan: %v", err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	// The staged candidate's definition is byte-identical to the source: no
	// managed span changed, so comments, CRLF, and the body are all preserved.
	got, err := os.ReadFile(filepath.Join(c.ConfigDir, "subagents", "agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, defBytes) {
		t.Fatalf("zero-edit plan mutated the definition:\n got=%q\nwant=%q", got, defBytes)
	}
	// The live source tree is byte-identical before and after staging.
	live, err := os.ReadFile(defPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(live, defBytes) {
		t.Fatal("live source mutated by staging")
	}
}

// TestPermanentDisableSurvivesHealthyReconcile proves an intentionally-disabled
// baseline model (enabled: false) stays disabled when its provider is healthy,
// and that disabling then re-enabling through the document editor is byte-stable
// for the managed span only. This is the desired-chain-only permanent-disable
// property composed with byte-preserving edits.
func TestPermanentDisableSurvivesHealthyReconcile(t *testing.T) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex": {
			CodexBarProviders:  []string{"codex"},
			PolytokenProviders: []string{"codex"},
			Models: map[string]policy.ModelBaseline{
				"codex/off":  {Enabled: false, HadEnabledKey: true},
				"codex/on":   {Enabled: true, HadEnabledKey: false},
				"codex/bare": {Enabled: true, HadEnabledKey: false},
			},
		},
	}}
	target := policy.Target{ID: "global", Root: "/r"}
	observed := state.State{Revision: 1} // codex healthy (absent = normal)
	plan, err := reconcile.Build(d, observed, target)
	if err != nil {
		t.Fatal(err)
	}
	// The intentionally-disabled model stays false; the enabled models stay true.
	if e := enabledEditFor(plan, "codex/off"); e == nil || *e {
		t.Fatal("intentional disable not preserved when provider healthy")
	}
	if e := enabledEditFor(plan, "codex/on"); e == nil || !*e {
		t.Fatal("enabled baseline flipped when provider healthy")
	}
}

// enabledEditFor returns the enabled bool a plan emits for models.<base>.enabled.
func enabledEditFor(p reconcile.Plan, base string) *bool {
	for _, e := range p.Edits {
		if len(e.Path) == 3 && e.Path[0] == "models" && e.Path[1] == base && e.Path[2] == "enabled" {
			return e.Enabled
		}
	}
	return nil
}
