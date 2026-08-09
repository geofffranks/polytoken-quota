package target

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
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
	if len(res.Definitions) != 1 || res.Definitions[0].PolicyPath != "agents/agent.md" {
		t.Fatalf("definitions=%+v", res.Definitions)
	}
	if !withinRoot(t, res.CanonicalRoot, res.Definitions[0].canonicalPath) {
		t.Fatalf("definition not within resolved root")
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
	if len(res.Definitions) != 1 || res.Definitions[0].PolicyPath != "registered.md" {
		t.Fatalf("definitions=%+v want only registered.md", res.Definitions)
	}
}

// TestResolvedDefinitionsRetainPathIdentity proves each explicitly registered
// definition keeps its target, normalized policy path, exact desired chain, and
// an internal canonical path approved by containment checks.
func TestResolvedDefinitionsRetainPathIdentity(t *testing.T) {
	secretRoot := filepath.Join(t.TempDir(), "home", "alice", ".config", "CANARY-secret-root")
	definitionPath := filepath.Join(secretRoot, "subagents", "nested", "agent.md")
	if err := os.MkdirAll(filepath.Dir(definitionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definitionPath, []byte("---\nname: Build Agent\npolytoken:\n  model: codex/gpt\n  fallback_models: [zai/glm]\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chain := policy.Chain{"codex/gpt(medium)", "zai/glm"}
	res, err := Resolve(policy.Target{ID: "project-a", Root: secretRoot, Definitions: []policy.Definition{{
		Path: filepath.Join("subagents", "nested", "..", "nested", "agent.md"), Chain: chain,
	}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Definitions) != 1 {
		t.Fatalf("definitions=%+v", res.Definitions)
	}
	got := res.Definitions[0]
	if got.TargetID != "project-a" || got.PolicyPath != "subagents/nested/agent.md" {
		t.Fatalf("public identity=%+v", got)
	}
	if got.canonicalPath != definitionPath {
		t.Fatalf("canonical path=%q want %q", got.canonicalPath, definitionPath)
	}
	if len(got.Chain) != 2 || got.Chain[0] != chain[0] || got.Chain[1] != chain[1] {
		t.Fatalf("chain=%v want %v", got.Chain, chain)
	}
	chain[0] = "mutated/input"
	if got.Chain[0] != "codex/gpt(medium)" {
		t.Fatalf("resolved chain aliases policy input: %v", got.Chain)
	}
	metadata, err := ReadDefinitionMetadata(got)
	if err != nil {
		t.Fatalf("ReadDefinitionMetadata: %v", err)
	}
	if metadata.Name != "Build Agent" || metadata.Model != "codex/gpt" || len(metadata.FallbackModels) != 1 || metadata.FallbackModels[0] != "zai/glm" {
		t.Fatalf("metadata=%+v", metadata)
	}
}

// TestResolveDoesNotParseDefinitionMetadata keeps exact path validation separate
// from diagnostic metadata reads. Malformed frontmatter is a metadata error, not
// a reason to reject an otherwise valid registered reconciliation target.
func TestResolveDoesNotParseDefinitionMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte("---\nname: [invalid\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(policy.Target{ID: "project-a", Root: root, Definitions: []policy.Definition{{Path: "agent.md"}}})
	if err != nil {
		t.Fatalf("Resolve parsed frontmatter: %v", err)
	}
	if _, err := ReadDefinitionMetadata(resolved.Definitions[0]); err == nil {
		t.Fatal("malformed frontmatter accepted by metadata reader")
	}
}

// TestRoutingDefinitionsExplicitPathsOnly proves resolution reads only exact
// registered files and still rejects traversal and symlink escapes. Model-bearing
// siblings and sibling project roots are never discovered.
func TestRoutingDefinitionsExplicitPathsOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "home", "registered", ".polytoken")
	outside := filepath.Join(base, "home", "unregistered-project", ".polytoken")
	for _, path := range []string{
		filepath.Join(root, "subagents", "registered.md"),
		filepath.Join(root, "subagents", "unregistered.md"),
		filepath.Join(outside, "subagents", "sibling.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: hidden unless registered\npolytoken:\n  model: codex/gpt\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Resolve(policy.Target{ID: "registered", Root: root, Definitions: []policy.Definition{{Path: "subagents/registered.md", Chain: policy.Chain{"codex/gpt"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definitions) != 1 || res.Definitions[0].PolicyPath != "subagents/registered.md" {
		t.Fatalf("resolved unregistered definitions: %+v", res.Definitions)
	}

	link := filepath.Join(root, "subagents", "linked.md")
	if err := os.Symlink(filepath.Join(outside, "subagents", "sibling.md"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, path := range []string{"../unregistered-project/.polytoken/subagents/sibling.md", "subagents/linked.md"} {
		if _, err := Resolve(policy.Target{ID: "registered", Root: root, Definitions: []policy.Definition{{Path: path}}}); err == nil {
			t.Fatalf("accepted unsafe definition %q", path)
		}
	}
}

// TestDiscoverProposesModelBearingFiles proves Discover proposes files whose
// frontmatter declares polytoken.model or polytoken.fallback_models in the
// nested form real Polytoken definitions use, plus dotted-key legacy
// spellings, within one registered root, without adopting them.
func TestDiscoverProposesModelBearingFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		// Nested frontmatter — the shape Polytoken actually loads.
		"agents/agent.md": "---\npolytoken:\n  model: codex/gpt-5.6-sol\n---\n# Agent\n",
		// Nested fallback_models without model.
		"helpers/sub.md": "---\ndescription: helper\npolytoken:\n  fallback_models:\n    - zai/glm-5.2\n---\nbody\n",
		// Dotted legacy spelling, still proposed.
		"legacy/dotted.md": "---\npolytoken.model: codex/gpt-5.6-sol\n---\n",
		// Negative: plain file, no polytoken at all.
		"README.md": "# project readme\n",
		// Negative: polytoken mapping without managed model fields.
		"agents/toolonly.md": "---\npolytoken:\n  tools: [file_read]\n---\nbody\n",
		// Negative: prose mention of the word polytoken in the body only.
		"docs/notes.md": "---\ntitle: notes\n---\npolytoken is a harness; model: not frontmatter\n",
		// Negative: malformed frontmatter YAML must not fail discovery.
		"broken/bad.md": "---\npolytoken: [unclosed\n  model: x\n---\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"agents/agent.md", "helpers/sub.md", "legacy/dotted.md"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
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
