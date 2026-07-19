package contract

// These contract tests pin the complete standalone validation staging behavior
// against synthetic, non-personal fixture layers under
// contract/testdata/polytoken. They prove that global and project layers fold
// into one effective config (project wins), every effective definition is
// copied, candidate edits land only in staging, and a deliberately conflicting
// live project cannot contaminate the candidate through the neutral workdir.
//
// No live Polytoken binary is required; these are static fixture checks of the
// staging builder's isolation and merge contracts.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"gopkg.in/yaml.v3"
)

const polytokenFixtures = "polytoken"

// fixtureDir resolves a path under contract/testdata/polytoken.
func fixtureDir(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"testdata", polytokenFixtures}, parts...)...)
}

// loadConfig parses config.yaml under dir.
func loadConfig(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("read %s/config.yaml: %v", dir, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func configPath(t *testing.T, m map[string]any, path string) any {
	t.Helper()
	var cur any = m
	for _, seg := range splitPath(path) {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q not a map at %s", path, seg)
		}
		cur, ok = mm[seg]
		if !ok {
			t.Fatalf("path %q missing key %s", path, seg)
		}
	}
	return cur
}

func splitPath(s string) []string {
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

// TestContractStagingFoldsLayers builds a candidate from the global + registered
// project fixtures and asserts project precedence and effective definitions.
func TestContractStagingFoldsLayers(t *testing.T) {
	globalDir, _ := filepath.Abs(fixtureDir(t, "global"))
	projectDir, _ := filepath.Abs(fixtureDir(t, "project", ".polytoken"))
	res := target.Resolved{
		ID:             "project",
		CanonicalRoot:  projectDir,
		Global:         false,
		DefinitionFiles: []string{filepath.Join(projectDir, "subagents", "project.md")},
	}
	plan := reconcile.Plan{TargetID: "project"}
	b := staging.Builder{
		TempRoot: t.TempDir(),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}
	c, err := b.Build(context.Background(), res, plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	cfg := loadConfig(t, c.ConfigDir)
	// Project wins.
	if got := configPath(t, cfg, "defaults.full"); got != "codex/gpt" {
		t.Fatalf("defaults.full = %v want codex/gpt", got)
	}
	if got := configPath(t, cfg, "models.codex/gpt.enabled"); got != false {
		t.Fatalf("codex/gpt.enabled = %v want false (project override)", got)
	}
	// Global-only model retained.
	if got := configPath(t, cfg, "models.zai/glm.enabled"); got != true {
		t.Fatalf("zai/glm.enabled = %v want true (global)", got)
	}
	// Auth redacted (inert).
	if got := configPath(t, cfg, "providers.codex.api_key"); got == "synthetic-codex-key-do-not-use" {
		t.Fatal("source secret present in inert staging")
	}
	// Both effective definitions present.
	for _, rel := range []string{"subagents/global.md", "subagents/project.md"} {
		if _, err := os.Stat(filepath.Join(c.ConfigDir, rel)); err != nil {
			t.Fatalf("missing definition %s: %v", rel, err)
		}
	}
	// Neutral workdir.
	if _, err := os.Stat(filepath.Join(c.WorkingDir, ".polytoken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("neutral workdir has .polytoken")
	}
}

// TestContractStagingConflictingLiveIsolation proves the conflicting live
// project config/definitions cannot enter the candidate. The candidate's
// ConfigDir must not contain the conflicting values or definition, and the
// workdir must remain neutral even though a conflicting live root exists.
func TestContractStagingConflictingLiveIsolation(t *testing.T) {
	globalDir, _ := filepath.Abs(fixtureDir(t, "global"))
	projectDir, _ := filepath.Abs(fixtureDir(t, "project", ".polytoken"))
	conflictingRoot, _ := filepath.Abs(fixtureDir(t, "conflicting-live"))
	res := target.Resolved{
		ID:             "project",
		CanonicalRoot:  projectDir,
		Global:         false,
		DefinitionFiles: []string{filepath.Join(projectDir, "subagents", "project.md")},
	}
	b := staging.Builder{
		TempRoot: t.TempDir(),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}
	c, err := b.Build(context.Background(), res, reconcile.Plan{TargetID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	// The candidate config reflects the merged layers, NOT the conflicting live.
	cfg := loadConfig(t, c.ConfigDir)
	if got := configPath(t, cfg, "defaults.full"); got == "conflicting/evil" {
		t.Fatal("conflicting live value entered candidate config")
	}
	// The conflicting definition is absent from staging.
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "subagents", "conflicting.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflicting live definition leaked into staging")
	}
	// The conflicting live root still exists on disk (proof of isolation target).
	if _, err := os.Stat(filepath.Join(conflictingRoot, ".polytoken", "config.yaml")); err != nil {
		t.Fatalf("conflicting live fixture missing: %v", err)
	}
	// Working dir is neutral and never the conflicting root.
	if filepath.Clean(c.WorkingDir) == filepath.Clean(conflictingRoot) {
		t.Fatal("working dir is the conflicting live root")
	}
	// Staged config contains no conflicting secret/value at all.
	data, _ := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if bytes.Contains(data, []byte("conflicting/evil")) {
		t.Fatal("conflicting value present in staged config")
	}
}
