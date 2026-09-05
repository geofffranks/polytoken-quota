package target

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// projectConfigFixture lays out a project directory whose Polytoken
// configuration lives in .polytoken, with one facet definition inside it.
func projectConfigFixture(t *testing.T) (proj, configDir string) {
	t.Helper()
	proj = t.TempDir()
	configDir = filepath.Join(proj, ".polytoken")
	facet := filepath.Join(configDir, "facets", "app-engineering.md")
	if err := os.MkdirAll(filepath.Dir(facet), 0o700); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"config.yaml":               "defaults: {}\n",
		"facets/app-engineering.md": "---\npolytoken:\n  model: codex/gpt\n---\nbody\n",
	} {
		if err := os.WriteFile(filepath.Join(configDir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return proj, configDir
}

// TestResolveAppendsPolytokenForProjectDirRoot proves a project target whose
// root is the project directory resolves to the project's .polytoken
// configuration directory, with definitions relative to that directory.
func TestResolveAppendsPolytokenForProjectDirRoot(t *testing.T) {
	proj, configDir := projectConfigFixture(t)
	res, err := Resolve(policy.Target{ID: "lappie", Root: proj, Definitions: []policy.Definition{
		{Path: "facets/app-engineering.md", Chain: policy.Chain{"codex/gpt"}},
	}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.CanonicalRoot != configDir {
		t.Fatalf("CanonicalRoot=%q want %q", res.CanonicalRoot, configDir)
	}
	if len(res.Definitions) != 1 || res.Definitions[0].PolicyPath != "facets/app-engineering.md" {
		t.Fatalf("definitions=%+v", res.Definitions)
	}
}

// TestResolveRejectsProjectDirRootWithoutConfigDir proves a project root that
// is neither a configuration directory nor holds a .polytoken configuration
// directory is rejected with a path-free, actionable classification.
func TestResolveRejectsProjectDirRootWithoutConfigDir(t *testing.T) {
	proj := t.TempDir() // no config.yaml anywhere
	_, err := Resolve(policy.Target{ID: "lappie", Root: proj})
	if err == nil {
		t.Fatal("configless project root accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not a Polytoken configuration directory") || !strings.Contains(msg, "lappie") {
		t.Fatalf("error lacks actionable classification: %q", msg)
	}
	if strings.Contains(msg, proj) {
		t.Fatalf("error leaked root path %q: %q", proj, msg)
	}
}

// TestResolveGlobalRootNotCanonicalized proves global targets keep their
// documented root semantics: the root is the configuration directory and is
// never rewritten, even when a .polytoken subdirectory exists.
func TestResolveGlobalRootNotCanonicalized(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".polytoken"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(policy.Target{ID: "global", Root: root, Global: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.CanonicalRoot != root {
		t.Fatalf("CanonicalRoot=%q want unchanged %q", res.CanonicalRoot, root)
	}
}

// TestResolveDefinitionNotExistSuggestsPrefixDrop proves a missing definition
// that would exist without a redundant .polytoken/ prefix reports the mismatch
// using only policy-relative paths.
func TestResolveDefinitionNotExistSuggestsPrefixDrop(t *testing.T) {
	_, configDir := projectConfigFixture(t)
	_, err := Resolve(policy.Target{ID: "lappie", Root: configDir, Definitions: []policy.Definition{
		{Path: ".polytoken/facets/app-engineering.md", Chain: policy.Chain{"codex/gpt"}},
	}})
	if err == nil {
		t.Fatal("redundant .polytoken/-prefixed definition accepted")
	}
	msg := err.Error()
	for _, want := range []string{"does not exist", "facets/app-engineering.md", ".polytoken/"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, configDir) {
		t.Fatalf("error leaked config dir path %q: %q", configDir, msg)
	}
}

// TestResolveDefinitionNotExistPlainWording proves a missing definition with no
// prefix confusion says so plainly instead of the opaque stat failed.
func TestResolveDefinitionNotExistPlainWording(t *testing.T) {
	_, configDir := projectConfigFixture(t)
	_, err := Resolve(policy.Target{ID: "lappie", Root: configDir, Definitions: []policy.Definition{
		{Path: "facets/absent.md", Chain: policy.Chain{"codex/gpt"}},
	}})
	if err == nil {
		t.Fatal("missing definition accepted")
	}
	if !strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "stat failed") {
		t.Fatalf("error wording=%q", err)
	}
}
