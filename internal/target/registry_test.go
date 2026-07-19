package target

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
)

// TestResolveRejectsTraversalAndSymlink is the Task 4 blueprint contract test:
// traversal outside the root and symlinked managed files are rejected by default.
func TestResolveRejectsTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "agent.md")
	if err := os.Symlink(filepath.Join(outside, "agent.md"), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.md", link} {
		_, err := Resolve(policy.Target{ID: "p", Root: root, Definitions: []policy.Definition{{Path: path}}})
		if err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

// TestResolveGlobalTarget proves one global target resolves with canonical root
// containment and its managed definition file collected as a canonical path.
func TestResolveGlobalTarget(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agents", "agent.md")
	if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("---\npolytoken.model: codex/gpt-5.6-sol\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(policy.Target{ID: "global", Root: root, Global: true, Definitions: []policy.Definition{{Path: rel}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Global || res.ID != "global" || res.CanonicalRoot == "" {
		t.Fatalf("resolved=%+v", res)
	}
	if len(res.DefinitionFiles) != 1 || filepath.Base(res.DefinitionFiles[0]) != "agent.md" {
		t.Fatalf("files=%+v", res.DefinitionFiles)
	}
	if !withinRoot(t, res.CanonicalRoot, res.DefinitionFiles[0]) {
		t.Fatalf("file %q not within root %q", res.DefinitionFiles[0], res.CanonicalRoot)
	}
}

// TestResolveDuplicateFiles proves duplicate managed definition files are rejected.
func TestResolveDuplicateFiles(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent.md")
	if err := os.WriteFile(agent, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(policy.Target{ID: "p", Root: root, Definitions: []policy.Definition{
		{Path: "agent.md"}, {Path: "agent.md"},
	}})
	if err == nil {
		t.Fatal("accepted duplicate definition file")
	}
}

// TestResolveRejectsOutsideRoot proves an absolute definition path outside the
// canonical root is rejected (root containment after canonicalization).
func TestResolveRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	other := filepath.Join(outside, "agent.md")
	if err := os.WriteFile(other, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(policy.Target{ID: "p", Root: root, Definitions: []policy.Definition{{Path: other}}})
	if err == nil {
		t.Fatal("accepted absolute path outside root")
	}
}

// TestResolveExplicitProjectsOnly proves only the definitions explicitly
// registered on the target are collected; Resolve never scans the root.
func TestResolveExplicitProjectsOnly(t *testing.T) {
	root := t.TempDir()
	registered := filepath.Join(root, "registered.md")
	// An unregistered model-bearing file that must NOT be collected.
	extra := filepath.Join(root, "extra", "unmanaged.md")
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registered, []byte("polytoken.model: a/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("polytoken.model: a/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(policy.Target{ID: "p", Root: root, Definitions: []policy.Definition{{Path: "registered.md"}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.DefinitionFiles) != 1 || filepath.Base(res.DefinitionFiles[0]) != "registered.md" {
		t.Fatalf("files=%+v want only registered.md", res.DefinitionFiles)
	}
}

// TestDiscoverProposesModelBearingFiles proves Discover proposes only files
// containing polytoken.model or polytoken.fallback_models within one registered
// root, without adopting them.
func TestDiscoverProposesModelBearingFiles(t *testing.T) {
	root := t.TempDir()
	m1 := filepath.Join(root, "agents", "agent.md")
	m2 := filepath.Join(root, "helpers", "sub.md")
	plain := filepath.Join(root, "README.md")
	for _, p := range []string{m1, m2, plain} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(m1, []byte("---\npolytoken.model: codex/gpt-5.6-sol\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m2, []byte("polytoken.fallback_models: [zai/glm-5.2]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("# project readme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"agents/agent.md", "helpers/sub.md"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestDiscoverSkipsSymlinkedDirs proves Discover never follows symlinks out of the
// registered root, so it cannot scan arbitrary workspace paths.
func TestDiscoverSkipsSymlinkedDirs(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	leaked := filepath.Join(outside, "leaked.md")
	if err := os.WriteFile(leaked, []byte("polytoken.model: x/y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("discover followed symlink out of root: %v", got)
	}
}

// withinRoot reports whether file is contained under root.
func withinRoot(t *testing.T, root, file string) bool {
	t.Helper()
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return false
	}
	return rel != ".." && !(len(rel) >= 3 && rel[0:3] == "../")
}
