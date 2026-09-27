package groupsafety

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestReadLayerReadsOnlyAllowlistedPaths proves ReadLayer gathers exactly one
// root's config.yaml plus the policy allowlisted facets/subagents *.md trees:
// decoy configs in unregistered directories, non-definition files, backups,
// and symlinked definitions are never read, and a project registered at the
// project directory resolves through the shared canonical-root rule.
func TestReadLayerReadsOnlyAllowlistedPaths(t *testing.T) {
	root := t.TempDir()
	rootConfig := `version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
modelgroups:
  g:
    - gp/g1
  polytoken:default_model_full: gp/g1
`
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.yaml", rootConfig)
	write("facets/p.md", "---\nname: p\npolytoken:\n  model: gp/g1\n---\n\nBody.\n")
	write("facets/sub/inner.md", "---\nname: inner\npolytoken:\n  model: mg:g\n  fallback_models:\n    - gp/g1\n---\n\nBody.\n")
	write("subagents/s.md", "---\nname: s\npolytoken:\n  model: gp/g1\n---\n\nBody.\n")
	write("facets/notes.txt", "not a definition")
	write("facets/x.md.bak", "---\nname: x\npolytoken:\n  model: gp/g1\n---\n\nBackup body.\n")
	// A decoy config in an unregistered subdirectory with poison content: if
	// the reader ever scanned beyond the root config.yaml, this would fail the
	// parse.
	write("other/config.yaml", "version: 3\n  bad: [")

	if err := os.Symlink(filepath.Join(root, "facets", "p.md"), filepath.Join(root, "facets", "linked.md")); err != nil {
		t.Fatal(err)
	}

	layer, err := ReadLayer("global", root, true)
	if err != nil {
		t.Fatalf("ReadLayer: %v", err)
	}
	if string(layer.Config) != rootConfig {
		t.Fatalf("layer config is not the root config.yaml: %.80s", layer.Config)
	}
	var paths []string
	for _, def := range layer.Definitions {
		paths = append(paths, def.Path)
	}
	want := []string{"facets/p.md", "facets/sub/inner.md", "subagents/s.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("definitions = %v, want %v", paths, want)
	}
	if got := layer.Definitions[1]; got.Model != "mg:g" || !reflect.DeepEqual(got.Fallbacks, []string{"gp/g1"}) {
		t.Fatalf("nested definition = %+v", got)
	}

	t.Run("project registration at the project directory", func(t *testing.T) {
		proj := filepath.Join(root, "proj")
		write("proj/.polytoken/config.yaml", "version: 4\nmodelgroups:\n  g:\n    - gp/g1\n")
		write("proj/.polytoken/subagents/pj.md", "---\nname: pj\npolytoken:\n  model: gp/g1\n---\n\nBody.\n")
		layer, err := ReadLayer("proj", proj, false)
		if err != nil {
			t.Fatalf("ReadLayer project: %v", err)
		}
		if !strings.Contains(string(layer.Config), "modelgroups") {
			t.Fatalf("project layer config not read from .polytoken: %.80s", layer.Config)
		}
		if len(layer.Definitions) != 1 || layer.Definitions[0].Path != "subagents/pj.md" {
			t.Fatalf("project definitions = %+v", layer.Definitions)
		}
	})

	t.Run("missing config is an error", func(t *testing.T) {
		empty := t.TempDir()
		if _, err := ReadLayer("gone", empty, true); err == nil {
			t.Fatalf("ReadLayer accepted a root without config.yaml")
		}
	})
}
