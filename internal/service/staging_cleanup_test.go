package service

// Task 14 staging-cleanup coverage. assertNoStagingRoots (used by
// TestNoSecretCanaryPersists) is exercised here across the four exit paths a
// staging root can meet: success (explicit cleanup), validation failure (the
// validate.Runner cleans up the candidate on every exit), cancellation, and
// timeout. The staging package's TestCleanupOnEveryExit covers the builder's
// own failure paths; this file proves the property through the validate Runner
// (the Coordinator's validation seam) and the success path, asserting no
// quota-stage-* root survives under the staging temp area.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"github.com/geofffranks/codexbar-hooks/internal/testutil"
	"github.com/geofffranks/codexbar-hooks/internal/validate"
)

// noStagingSourceFixture lays down a global layer for staging builds.
func noStagingSourceFixture(t *testing.T) (string, target.Resolved) {
	t.Helper()
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  codex/gpt:\n    enabled: true\ndefaults:\n  full: codex/gpt\n")
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true,
		DefinitionFiles: []string{filepath.Join(globalDir, "subagents", "agent.md")}}
	return globalDir, res
}

// buildCandidate builds an AuthInert candidate under stageTmp and returns it.
func buildCandidate(t *testing.T, globalDir, stageTmp string, res target.Resolved) staging.Candidate {
	t.Helper()
	c, err := staging.Builder{
		TempRoot: stageTmp, AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: res.ID})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return c
}

// assertNoStagingUnderRoot fails if any quota-stage-* directory remains under
// stageTmp. This is the deliverable's assertNoStagingRoots check applied to the
// staging temp area after each exit path.
func assertNoStagingUnderRoot(t *testing.T, stageTmp string) {
	t.Helper()
	_ = filepath.Walk(stageTmp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasPrefix(info.Name(), "quota-stage-") {
			t.Errorf("transient staging root survived: %s", path)
		}
		return nil
	})
}

// TestAssertNoStagingRootsAfterExits proves no staging root survives after
// success, validation failure, cancellation, and timeout. Each path builds a
// candidate under a shared stageTmp and exits via the real cleanup path, then
// asserts the temp area holds no quota-stage-* root.
func TestAssertNoStagingRootsAfterExits(t *testing.T) {
	globalDir, res := noStagingSourceFixture(t)

	// success: build then explicit Cleanup.
	stageTmp := t.TempDir()
	c := buildCandidate(t, globalDir, stageTmp, res)
	if err := c.Cleanup(); err != nil {
		t.Fatal(err)
	}
	assertNoStagingUnderRoot(t, stageTmp)

	// validation-failure: the validate.Runner cleans up the candidate on every
	// exit path (success or failure). A failing runner must still remove it.
	stageTmp2 := t.TempDir()
	c2 := buildCandidate(t, globalDir, stageTmp2, res)
	runner := validate.Runner{
		Binary:   "polytoken",
		Commands: failRunner{},
	}
	if result := runner.Validate(context.Background(), c2, time.Second); result.StartupValid {
		t.Fatal("expected validation failure")
	}
	assertNoStagingUnderRoot(t, stageTmp2)

	// cancellation: a cancelled context aborts staging after root creation; the
	// builder removes the partial root.
	stageTmp3 := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (staging.Builder{
		TempRoot: stageTmp3, AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(ctx, res, reconcile.Plan{TargetID: res.ID})); err == nil {
		t.Fatal("expected cancelled build to fail")
	}
	assertNoStagingUnderRoot(t, stageTmp3)

	// timeout: an already-elapsed deadline aborts staging; the builder removes
	// the partial root.
	stageTmp4 := t.TempDir()
	tctx, tcancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer tcancel()
	time.Sleep(2 * time.Millisecond)
	if _, err := (staging.Builder{
		TempRoot: stageTmp4, AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(tctx, res, reconcile.Plan{TargetID: res.ID})); err == nil {
		t.Fatal("expected timed-out build to fail")
	}
	assertNoStagingUnderRoot(t, stageTmp4)
}

// failRunner is a validate.CommandRunner that always fails, exercising the
// Runner's candidate-cleanup-on-failure path.
type failRunner struct{}

func (failRunner) Run(context.Context, string, []string, int64) ([]byte, []byte, int, error) {
	return []byte(""), []byte("synthetic failure"), 1, nil
}
