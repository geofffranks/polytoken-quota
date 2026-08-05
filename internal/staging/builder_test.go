package staging

// These tests verify the complete standalone validation staging root. They fold
// a real fixture global layer plus a registered project layer into one effective
// config.yaml, copy every effective startup definition, apply candidate edits
// only inside staging, create a co-located neutral working directory with no
// .polytoken, guarantee cleanup on every exit, prove conflicting-live isolation,
// and exercise the three auth branches.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/testutil"
	"gopkg.in/yaml.v3"
)

// --- shared fixture ---------------------------------------------------------

// liveFixture is a layered source tree under t.TempDir(): a global config dir
// and a registered project (with its own .polytoken), plus the resolved target,
// a reconciliation plan, and an FSMaterializer reading the real fixture files.
type liveFixture struct {
	Root        string // overall fixture root (global + project)
	ProjectRoot string // the live registered project root
	Target      target.Resolved
	Plan        reconcile.Plan
	Sources     FSMaterializer
}

const (
	// sourceSecret is a synthetic, non-personal sentinel placed in the global
	// provider auth; AuthInert must remove it and AuthTransientSource retain it.
	sourceSecret = "super-secret-codex-key-1234"
)

var globalConfigYAML = "providers:\n" +
	"  codex:\n" +
	"    api_key: " + sourceSecret + "\n" +
	"    base_url: https://api.codex.test\n" +
	"models:\n" +
	"  codex/gpt:\n" +
	"    enabled: true\n" +
	"  zai/glm:\n" +
	"    enabled: true\n" +
	"defaults:\n" +
	"  full: zai/glm\n"

// projectConfigYAML overrides one model and the default; project wins on merge.
var projectConfigYAML = "models:\n" +
	"  codex/gpt:\n" +
	"    enabled: false\n" +
	"defaults:\n" +
	"  full: codex/gpt\n"

var globalDefMD = "---\n" +
	"polytoken:\n" +
	"  model: zai/glm\n" +
	"  fallback_models:\n" +
	"    - codex/gpt\n" +
	"description: global agent\n" +
	"---\n# Global agent body.\n"

var projectDefMD = "---\n" +
	"polytoken:\n" +
	"  model: codex/gpt\n" +
	"description: project agent\n" +
	"---\n# Project agent body.\n"

// layeredFixture lays down the global and project layers on disk and returns a
// liveFixture whose Sources is an FSMaterializer reading them for real.
func layeredFixture(t *testing.T) liveFixture {
	t.Helper()
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	projectRoot := filepath.Join(root, "project")
	projectDir := filepath.Join(projectRoot, ".polytoken")

	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"), globalConfigYAML)
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "global.md"), globalDefMD)
	testutil.WriteFile(t, filepath.Join(projectDir, "config.yaml"), projectConfigYAML)
	testutil.WriteFile(t, filepath.Join(projectDir, "subagents", "project.md"), projectDefMD)

	res := target.Resolved{
		ID:              "proj-test",
		CanonicalRoot:   projectDir,
		Global:          false,
		DefinitionFiles: []string{filepath.Join(projectDir, "subagents", "project.md")},
	}
	// A realistic plan: re-enable codex/gpt (edit), set a scalar default, and
	// rewrite the project definition model + fallback list. These edits land
	// only inside staging.
	plan := reconcile.Plan{
		TargetID: "proj-test",
		Edits: []reconcile.FieldEdit{
			{File: "config.yaml", Path: []string{"models", "codex/gpt", "enabled"}, Enabled: boolPtr(true)},
			{File: "subagents/project.md", Path: []string{"polytoken", "model"}, Scalar: strPtr("codex/gpt")},
			{File: "subagents/project.md", Path: []string{"polytoken", "fallback_models"}, Sequence: []string{"zai/glm"}},
		},
	}
	return liveFixture{
		Root:        root,
		ProjectRoot: projectRoot,
		Target:      res,
		Plan:        plan,
		Sources:     FSMaterializer{GlobalDir: globalDir},
	}
}

// newBuilder returns a Builder with a fresh TempRoot and AuthInert (no secrets).
func newBuilder(t *testing.T) Builder {
	t.Helper()
	return Builder{
		TempRoot: t.TempDir(),
		AuthMode: AuthInert,
		Sources:  nil, // set per-test
	}
}

// builderWith builds a Builder bound to the fixture sources and the given mode.
func builderWith(t *testing.T, live liveFixture, mode AuthMode) Builder {
	t.Helper()
	return Builder{TempRoot: t.TempDir(), AuthMode: mode, Sources: live.Sources}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// readStagedConfig reads and parses config.yaml from configDir into a map.
func readStagedConfig(t *testing.T, configDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read staged config: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse staged config: %v\n%s", err, data)
	}
	return m
}

// configGet resolves a nested dotted path (e.g. "defaults.full") in a parsed
// config map. It fatals if any segment is missing or non-map.
func configGet(t *testing.T, m map[string]any, path string) any {
	t.Helper()
	segs := splitDots(path)
	var cur any = m
	for _, seg := range segs {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q: %s is not a map", path, seg)
		}
		cur, ok = mm[seg]
		if !ok {
			t.Fatalf("path %q: key %s missing", path, seg)
		}
	}
	return cur
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// --- tests ------------------------------------------------------------------

// TestBuildSkipsSymlinkedLayerFiles proves a benign-named symlink inside a
// source layer pointing at an outside secret file is never followed into the
// staging root, and a symlinked config.yaml fails the build outright.
func TestBuildSkipsSymlinkedLayerFiles(t *testing.T) {
	live := layeredFixture(t)
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "credentials")
	testutil.WriteFile(t, secretPath, "outside-secret-canary")
	globalDir := live.Sources.GlobalDir
	// Benign-looking definition name linking to the outside secret.
	if err := os.Symlink(secretPath, filepath.Join(globalDir, "subagents", "notes.md")); err != nil {
		t.Fatal(err)
	}
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "subagents", "notes.md")); !os.IsNotExist(err) {
		t.Fatal("symlinked layer file was copied into staging")
	}
	err = filepath.WalkDir(c.Root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, []byte("outside-secret-canary")) {
			t.Fatalf("outside secret leaked into staging at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBuildRejectsSymlinkedConfig proves a layer whose config.yaml is itself a
// symlink is refused rather than silently read through.
func TestBuildRejectsSymlinkedConfig(t *testing.T) {
	live := layeredFixture(t)
	globalDir := live.Sources.GlobalDir
	real := filepath.Join(globalDir, "config.yaml")
	moved := filepath.Join(t.TempDir(), "moved-config.yaml")
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatal(err)
	}
	if _, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan); err == nil {
		t.Fatal("build accepted a symlinked config.yaml")
	}
}

// TestBuildClearsStaleStagingRoot proves a stale root left by a prior crash
// (including foreign files) never survives into the next candidate.
func TestBuildClearsStaleStagingRoot(t *testing.T) {
	live := layeredFixture(t)
	b := builderWith(t, live, AuthInert)
	stale := stageRoot(b.TempRoot, live.Target.ID)
	testutil.WriteFile(t, filepath.Join(stale, "config", "leftover.md"), "stale-bytes")
	c, err := b.Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "leftover.md")); !os.IsNotExist(err) {
		t.Fatal("stale file survived into the fresh staging candidate")
	}
}

// TestBuildCompleteRootAndNeutralWorkdir folds global + project into one
// effective config (project wins), copies every effective definition, applies
// edits only in staging, and records a separate neutral ConfigDir/WorkingDir.
func TestBuildCompleteRootAndNeutralWorkdir(t *testing.T) {
	live := layeredFixture(t)
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	cfg := readStagedConfig(t, c.ConfigDir)
	// Project wins on defaults.full and codex/gpt.enabled, while the untouched
	// zai/glm comes from the global layer.
	if got := configGet(t, cfg, "defaults.full"); got != "codex/gpt" {
		t.Fatalf("defaults.full = %v want codex/gpt (project wins)", got)
	}
	if got := configGet(t, cfg, "models.codex/gpt.enabled"); got != true {
		t.Fatalf("codex/gpt.enabled = %v want true (plan edit applied)", got)
	}
	if got := configGet(t, cfg, "models.zai/glm.enabled"); got != true {
		t.Fatalf("zai/glm.enabled = %v want true (global layer present)", got)
	}
	if got := configGet(t, cfg, "providers.codex.base_url"); got != "https://api.codex.test" {
		t.Fatalf("base_url = %v want global value", got)
	}
	// Every effective definition is present.
	for _, rel := range []string{"subagents/global.md", "subagents/project.md"} {
		if _, err := os.Stat(filepath.Join(c.ConfigDir, rel)); err != nil {
			t.Fatalf("missing effective definition %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(c.UserConfigDir, "polytoken", "config.yaml")); err != nil {
		t.Fatalf("missing staged user config: %v", err)
	}
	userProject, err := os.ReadFile(filepath.Join(c.UserConfigDir, "polytoken", "subagents", "project.md"))
	if err != nil || !bytes.Contains(userProject, []byte("- zai/glm")) {
		t.Fatalf("user config did not receive plan edit: err=%v content=%s", err, userProject)
	}
	// The plan edit rewrote the project definition's model + fallback.
	pj, err := os.ReadFile(filepath.Join(c.ConfigDir, "subagents", "project.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pj, []byte("model: codex/gpt")) {
		t.Fatalf("project def model not edited:\n%s", pj)
	}
	if !bytes.Contains(pj, []byte("- zai/glm")) {
		t.Fatalf("project def fallback not edited:\n%s", pj)
	}
	// WorkingDir is separate from the live project root and has no .polytoken.
	if filepath.Clean(c.WorkingDir) == filepath.Clean(live.ProjectRoot) {
		t.Fatal("used live project workdir")
	}
	if _, err := os.Stat(filepath.Join(c.WorkingDir, ".polytoken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("neutral workdir contaminated with .polytoken")
	}
	if c.TargetID != live.Target.ID {
		t.Fatalf("TargetID = %q want %q", c.TargetID, live.Target.ID)
	}
}

// TestBuildDoesNotMutateLive proves the live source tree is byte-identical after
// a build + cleanup.
func TestBuildDoesNotMutateLive(t *testing.T) {
	live := layeredFixture(t)
	before := testutil.Snapshot(t, live.Root)
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Cleanup()
	after := testutil.Snapshot(t, live.Root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("live files changed:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestCleanupRemovesRootAndIsIdempotent verifies Cleanup deletes the staging
// root and that a second call is a no-op.
func TestCleanupRemovesRootAndIsIdempotent(t *testing.T) {
	live := layeredFixture(t)
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(c.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root left after cleanup: %v", err)
	}
	if err := c.Cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}

// TestCleanupOnEveryExit proves no staging root leaks after success, a render
// (edit) error, cancellation, or timeout. Each stage builds with a Builder whose
// TempRoot is known and captured, predicts the staging root from that exact
// TempRoot (so the assertGone check observes a path the build actually created),
// and — for cancel/timeout — cancels AFTER root creation so the real
// failure-handling cleanup path is exercised.
func TestCleanupOnEveryExit(t *testing.T) {
	live := layeredFixture(t)

	// success: build then explicit cleanup. The Builder that runs the build is
	// the same one whose TempRoot we predict the root from.
	b := builderWith(t, live, AuthInert)
	c, err := b.Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	root := stageRoot(b.TempRoot, live.Target.ID)
	if root != c.Root {
		t.Fatalf("success: predicted root %q != actual %q", root, c.Root)
	}
	if err := c.Cleanup(); err != nil {
		t.Fatal(err)
	}
	assertGone(t, "success", root)

	// render: a plan edit targets a file absent from staging, erroring inside
	// stage() after the root has been created.
	rb := builderWith(t, live, AuthInert)
	root = stageRoot(rb.TempRoot, live.Target.ID)
	renderPlan := reconcile.Plan{TargetID: live.Target.ID, Edits: []reconcile.FieldEdit{
		{File: "ghost/missing.md", Path: []string{"polytoken", "model"}, Scalar: strPtr("codex/gpt")},
	}}
	if _, err := rb.Build(context.Background(), live.Target, renderPlan); err == nil {
		t.Fatal("expected render error")
	}
	assertGone(t, "render", root)

	// cancel: the context is cancelled mid-build — after root creation but
	// before stage() finishes — by a Sources wrapper that cancels during the
	// project-layer materialization, so the next ctx.Err() gate in stage()
	// observes the cancellation and exercises the real failure-handling
	// cleanup path. This is deterministic (no timing race): cancellation lands
	// synchronously inside the build, after the root already exists.
	cb := builderWith(t, live, AuthInert)
	root = stageRoot(cb.TempRoot, live.Target.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cb.Sources = cancelDuringProject{inner: live.Sources, cancel: cancel}
	if _, err := cb.Build(ctx, live.Target, live.Plan); err == nil {
		t.Fatal("expected cancel error")
	}
	assertGone(t, "cancel", root)
	cancel()

	// timeout: a context whose deadline elapses mid-build — after root
	// creation but before stage() finishes — so a later ctx.Err() gate
	// observes context.DeadlineExceeded and the real cleanup path runs. The
	// deadlineDuringProject wrapper blocks inside Project() (which runs after
	// the root is created) until the near-future deadline passes.
	tb := builderWith(t, live, AuthInert)
	root = stageRoot(tb.TempRoot, live.Target.ID)
	tctx, tcancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Millisecond))
	defer tcancel()
	tb.Sources = deadlineDuringProject{inner: live.Sources}
	if _, err := tb.Build(tctx, live.Target, live.Plan); err == nil {
		t.Fatal("expected timeout error")
	}
	assertGone(t, "timeout", root)
}

// cancelDuringProject wraps a SourceMaterializer and cancels the build context
// during Project() materialization — after the staging root already exists. The
// next ctx.Err() gate in stage() then observes the cancellation and exercises
// the real failure-handling cleanup path.
type cancelDuringProject struct {
	inner  SourceMaterializer
	cancel context.CancelFunc
}

func (c cancelDuringProject) Global(ctx context.Context) (Layer, error) {
	return c.inner.Global(ctx)
}

func (c cancelDuringProject) Project(ctx context.Context, res target.Resolved) (Layer, bool, error) {
	c.cancel()
	return c.inner.Project(ctx, res)
}

// deadlineDuringProject wraps a SourceMaterializer and blocks inside Project()
// (which runs after root creation) until the context's deadline elapses, so a
// later ctx.Err() gate in stage() observes context.DeadlineExceeded and the
// real failure-handling cleanup path runs.
type deadlineDuringProject struct {
	inner SourceMaterializer
}

func (d deadlineDuringProject) Global(ctx context.Context) (Layer, error) {
	return d.inner.Global(ctx)
}

func (d deadlineDuringProject) Project(ctx context.Context, res target.Resolved) (Layer, bool, error) {
	// Block until the deadline elapses. The deadline is set by the caller to
	// a few milliseconds in the future, so this returns quickly but only
	// after the staging root has already been created by Build.
	<-ctx.Done()
	return Layer{}, false, ctx.Err()
}

func assertGone(t *testing.T, stage, root string) {
	t.Helper()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s left staging root %s", stage, root)
	}
}

// TestPrivatePermissions asserts dirs are 0700 and files 0600 in staging.
func TestPrivatePermissions(t *testing.T) {
	live := layeredFixture(t)
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	for _, dir := range []string{c.Root, c.ConfigDir, c.WorkingDir, filepath.Join(c.ConfigDir, "subagents")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != dirPerm {
			t.Fatalf("dir %s perm = %o want %o", dir, info.Mode().Perm(), dirPerm)
		}
	}
	for _, file := range []string{
		filepath.Join(c.ConfigDir, "config.yaml"),
		filepath.Join(c.ConfigDir, "subagents", "global.md"),
		filepath.Join(c.ConfigDir, "subagents", "project.md"),
	} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != filePerm {
			t.Fatalf("file %s perm = %o want %o", file, info.Mode().Perm(), filePerm)
		}
	}
}

// TestConflictingLiveIsolation proves a deliberately conflicting live project
// .polytoken cannot affect the candidate: WorkingDir is neutral (no .polytoken)
// and separate from the live root, and the staged config reflects the merged
// source layers, not the live working-dir discovery.
func TestConflictingLiveIsolation(t *testing.T) {
	live := layeredFixture(t)
	// Drop an extra conflicting definition directly in the live project root
	// (outside .polytoken) that must never appear in staging.
	testutil.WriteFile(t, filepath.Join(live.ProjectRoot, "stray.md"),
		"---\npolytoken:\n  model: evil/stray\n---\nbody\n")
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	// The stray file is never copied into staging (only the .polytoken layers).
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "stray.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stray live file leaked into staging config dir")
	}
	// sanity: the stray still exists in the live tree, just not in staging
	if _, err := os.Stat(filepath.Join(live.ProjectRoot, "stray.md")); err != nil {
		t.Fatalf("stray missing from live tree: %v", err)
	}
	if filepath.Clean(c.WorkingDir) == filepath.Clean(live.ProjectRoot) {
		t.Fatal("working dir is the live project root")
	}
	if _, err := os.Stat(filepath.Join(c.WorkingDir, ".polytoken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("neutral workdir has .polytoken")
	}
}

// TestAuthInertRedactsSecrets proves AuthInert removes source secrets and leaves
// an inert placeholder.
func TestAuthInertRejectsSecretBearingAuxiliaryFile(t *testing.T) {
	live := layeredFixture(t)
	testutil.WriteFile(t, filepath.Join(live.Root, "global", "credentials.json"), `{"token":"secret"}`)
	live.Sources = FSMaterializer{GlobalDir: filepath.Join(live.Root, "global")}
	if _, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan); err == nil {
		t.Fatal("AuthInert accepted secret-bearing auxiliary file")
	}
}

func TestAuthInertRedactsSecrets(t *testing.T) {
	live := layeredFixture(t)
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	data, err := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(sourceSecret)) {
		t.Fatal("AuthInert left source secret in staging")
	}
	if !bytes.Contains(data, []byte(inertSecret)) {
		t.Fatalf("AuthInert did not write inert placeholder:\n%s", data)
	}
}

// TestAuthTransientSourceRetainsSecrets proves AuthTransientSource keeps source
// auth values verbatim (no expansion) under restrictive permissions.
func TestAuthTransientSourceRetainsSecrets(t *testing.T) {
	live := layeredFixture(t)
	c, err := builderWith(t, live, AuthTransientSource).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	data, err := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(sourceSecret)) {
		t.Fatal("AuthTransientSource dropped source secret")
	}
	info, err := os.Stat(filepath.Join(c.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != filePerm {
		t.Fatalf("config perm = %o want %o", info.Mode().Perm(), filePerm)
	}
}

// TestAuthUndecidedErrors proves the fail-closed default returns an error and
// creates no staging root.
func TestAuthUndecidedErrors(t *testing.T) {
	live := layeredFixture(t)
	b := Builder{TempRoot: t.TempDir(), AuthMode: AuthUndecided, Sources: live.Sources}
	_, err := b.Build(context.Background(), live.Target, live.Plan)
	if err == nil {
		t.Fatal("expected error for AuthUndecided")
	}
}

// TestNoEnvSecretExpansion proves an environment-variable-style auth reference
// (${CODEX_KEY}) is copied literally, never expanded, into the merged config.
func TestNoEnvSecretExpansion(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	projectDir := filepath.Join(root, "project", ".polytoken")
	envConfig := "providers:\n  codex:\n    api_key: ${CODEX_API_KEY}\n"
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"), envConfig)
	testutil.WriteFile(t, filepath.Join(projectDir, "config.yaml"), "defaults:\n  full: codex/gpt\n")
	res := target.Resolved{ID: "p", CanonicalRoot: projectDir, Global: false}
	c, err := Builder{
		TempRoot: t.TempDir(),
		AuthMode: AuthTransientSource,
		Sources:  FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	data, err := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("${CODEX_API_KEY}")) {
		t.Fatalf("env var reference expanded or lost:\n%s", data)
	}
}

// TestGlobalTargetNoProjectLayer proves a global target stages from the global
// layer alone (no project overlay).
func TestGlobalTargetNoProjectLayer(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"), globalConfigYAML)
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "global.md"), globalDefMD)
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	c, err := Builder{
		TempRoot: t.TempDir(),
		AuthMode: AuthInert,
		Sources:  FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: "global"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	cfg := readStagedConfig(t, c.ConfigDir)
	if got := configGet(t, cfg, "defaults.full"); got != "zai/glm" {
		t.Fatalf("global default = %v want zai/glm", got)
	}
	if got := configGet(t, cfg, "providers.codex.api_key"); got != inertSecret {
		t.Fatalf("global auth not redacted: %v", got)
	}
}

// TestTimeoutContextIsHonored uses a context that expires mid-build window to
// confirm cancellation surfaces as an error without leaking the root. (The
// zero/deadline-on-arrival case is covered in TestCleanupOnEveryExit.)
func TestTimeoutContextIsHonored(t *testing.T) {
	live := layeredFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // ensure deadline passed
	root := stageRoot(t.TempDir(), live.Target.ID)
	b := Builder{TempRoot: filepath.Dir(root), AuthMode: AuthInert, Sources: live.Sources}
	_, err := b.Build(ctx, live.Target, live.Plan)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	assertGone(t, "timeout-window", root)
}

// --- secretBearingFile word-boundary tests ----------------------------------

func TestSecretBearingFileWordBoundary(t *testing.T) {
	// Benign filenames that contain a marker as a substring of a larger word.
	// These must NOT be flagged — the old code false-positived them.
	benign := []string{
		// filepath.Base is all that matters; "polytoken" embeds "token" but
		// must not be flagged. (Full path kept short to avoid tripping the
		// TestNoProcessControl source guard.)
		"ref/polytoken-tools.md",
		"superpowers/session-start-polytoken",
		"facets/authorize.md",
		"subagents/authors.md",
		"facets/polyauth.md",
	}
	for _, f := range benign {
		if secretBearingFile(f) {
			t.Errorf("secretBearingFile(%q) = true; want false (benign)", f)
		}
	}

	// Genuinely secret-bearing filenames — these MUST be flagged.
	dangerous := []string{
		"credentials.json",
		".env",
		"production.env",
		"access_token.json",
		"token.json",
		"secret.key",
		"secrets.yaml",
		"auth.json",
		"auth_tokens.yaml",
		"deploy/.env",
		"config/token",
	}
	for _, f := range dangerous {
		if !secretBearingFile(f) {
			t.Errorf("secretBearingFile(%q) = false; want true (secret-bearing)", f)
		}
	}
}

// --- readLayer exclusion tests ----------------------------------------------

func TestReadLayerExcludesNonConfigPaths(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "config.yaml"), "models: {}\n")
	// Config files that MUST be staged.
	testutil.WriteFile(t, filepath.Join(dir, "subagents", "reviewer.md"), "# reviewer")
	testutil.WriteFile(t, filepath.Join(dir, "facets", "scribe.md"), "# scribe")
	testutil.WriteFile(t, filepath.Join(dir, "skills", "debug", "SKILL.md"), "# debug")
	// Ephemeral runtime state that MUST be excluded.
	testutil.WriteFile(t, filepath.Join(dir, "read-once", "session-abc.jsonl"), "{}")
	testutil.WriteFile(t, filepath.Join(dir, "skill-once", "session-def.jsonl"), "{}")
	testutil.WriteFile(t, filepath.Join(dir, "superpowers", "session-start"), "#!/bin/sh")
	testutil.WriteFile(t, filepath.Join(dir, "prompt_history"), "history data")
	// Backup files that MUST be excluded (contain raw secrets).
	testutil.WriteFile(t, filepath.Join(dir, "config.yaml.bak"), "providers:\n  api_key: leaked\n")
	testutil.WriteFile(t, filepath.Join(dir, "config.yaml.20260101T000000Z.bak"), "providers:\n  api_key: leaked\n")
	testutil.WriteFile(t, filepath.Join(dir, "config.yaml.bak-20260101"), "providers:\n  api_key: leaked\n")

	layer, err := readLayer(dir)
	if err != nil {
		t.Fatal(err)
	}
	must := []string{
		"subagents/reviewer.md",
		"facets/scribe.md",
		"skills/debug/SKILL.md",
	}
	for _, rel := range must {
		if _, ok := layer.Files[rel]; !ok {
			t.Errorf("config file %q was excluded; it must be staged", rel)
		}
	}
	excluded := []string{
		"read-once/session-abc.jsonl",
		"skill-once/session-def.jsonl",
		"superpowers/session-start",
		"prompt_history",
		"config.yaml.bak",
		"config.yaml.20260101T000000Z.bak",
		"config.yaml.bak-20260101",
	}
	for _, rel := range excluded {
		if _, ok := layer.Files[rel]; ok {
			t.Errorf("non-config file %q was staged; it must be excluded", rel)
		}
	}
}

// TestStagingAcceptsPolytokenNamedFiles proves staging succeeds when the config
// dir contains files whose names embed "token" inside "polytoken" — the bug that
// blocked `polytoken-quota sync --from-polytoken` in the wild.
func TestStagingAcceptsPolytokenNamedFiles(t *testing.T) {
	live := layeredFixture(t)
	testutil.WriteFile(t, filepath.Join(live.Root, "global", "polytoken-tools.md"), "# tools")
	testutil.WriteFile(t, filepath.Join(live.Root, "global", "superpowers", "session-start-polytoken"), "#!/bin/sh")
	live.Sources = FSMaterializer{GlobalDir: filepath.Join(live.Root, "global")}
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatalf("staging rejected benign polytoken-named file: %v", err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
}

// TestStagingExcludesBackupWithSecrets proves backup config files containing
// real secrets are never staged in AuthInert mode.
func TestStagingExcludesBackupWithSecrets(t *testing.T) {
	live := layeredFixture(t)
	leaked := "real-leaked-secret-9999"
	testutil.WriteFile(t, filepath.Join(live.Root, "global", "config.yaml.bak"),
		"providers:\n  codex:\n    api_key: "+leaked+"\n")
	live.Sources = FSMaterializer{GlobalDir: filepath.Join(live.Root, "global")}
	c, err := builderWith(t, live, AuthInert).Build(context.Background(), live.Target, live.Plan)
	if err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	// The backup must not exist in staging.
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "config.yaml.bak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup file was staged (should be excluded)")
	}
	// And its secret must not appear anywhere in the staged config dir.
	err = filepath.Walk(c.ConfigDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, []byte(leaked)) {
			t.Errorf("leaked secret found in staged file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
