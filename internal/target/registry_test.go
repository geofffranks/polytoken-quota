package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// TestResolveRejectsTraversalAndSymlink is the Task 4 blueprint contract test:
// traversal outside the root and symlinked managed files are rejected by default.
func TestResolveRejectsTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeRootConfig(t, root)
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
	writeRootConfig(t, root)
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
	writeRootConfig(t, root)
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
	writeRootConfig(t, root)
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
	writeRootConfig(t, secretRoot)
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
	writeRootConfig(t, root)
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

func TestReadDefinitionMetadataRejectsPostResolveSymlinkSwap(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, root, outside string)
	}{
		{
			name: "file",
			swap: func(t *testing.T, root, outside string) {
				t.Helper()
				managed := filepath.Join(root, "subagents", "agent.md")
				if err := os.Remove(managed); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "agent.md"), managed); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			},
		},
		{
			name: "parent directory",
			swap: func(t *testing.T, root, outside string) {
				t.Helper()
				parent := filepath.Join(root, "subagents")
				if err := os.RemoveAll(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "home", "victim", ".config", "CANARY-ROOT")
			outside := filepath.Join(base, "outside", "CANARY-OUTSIDE")
			for _, file := range []string{filepath.Join(root, "config.yaml"), filepath.Join(root, "subagents", "agent.md"), filepath.Join(outside, "agent.md")} {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				name := "Approved Agent"
				if strings.HasPrefix(file, outside) {
					name = "CANARY-OUTSIDE-METADATA"
				}
				if err := os.WriteFile(file, []byte("---\nname: "+name+"\n---\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			resolved, err := Resolve(policy.Target{ID: "target-a", Root: root, Definitions: []policy.Definition{{Path: "subagents/agent.md"}}})
			if err != nil {
				t.Fatal(err)
			}

			tc.swap(t, root, outside)
			_, err = ReadDefinitionMetadata(resolved.Definitions[0])
			if err == nil {
				t.Fatal("metadata reader followed post-resolution symlink swap")
			}
			message := err.Error()
			if !strings.Contains(message, "target-a") || !strings.Contains(message, "subagents/agent.md") {
				t.Fatalf("error lacks safe location: %q", message)
			}
			for _, forbidden := range []string{root, outside, "CANARY-ROOT", "CANARY-OUTSIDE", "CANARY-OUTSIDE-METADATA"} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("error leaked %q: %q", forbidden, message)
				}
			}
		})
	}
}

func TestResolveRootErrorsIncludeSanitizedTargetIdentity(t *testing.T) {
	assertPrivateRootError := func(t *testing.T, err error, classification string, forbidden ...string) {
		t.Helper()
		if err == nil {
			t.Fatal("invalid root accepted")
		}
		if !strings.Contains(err.Error(), "target-a") || !strings.Contains(err.Error(), classification) {
			t.Fatalf("root error lacks safe identity/classification: %q", err)
		}
		for _, value := range forbidden {
			if strings.Contains(err.Error(), value) {
				t.Fatalf("root error leaked root material %q: %q", value, err)
			}
		}
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			t.Fatalf("root error wraps path-bearing OS error: %v", pathErr)
		}
	}

	_, err := Resolve(policy.Target{ID: "target-a"})
	assertPrivateRootError(t, err, "empty root")

	missingRoot := filepath.Join(t.TempDir(), "CANARY-MISSING-ROOT")
	_, err = Resolve(policy.Target{ID: "target-a", Root: missingRoot})
	assertPrivateRootError(t, err, "resolve root failed", missingRoot, "CANARY-MISSING-ROOT")

	t.Run("absolute canonicalization", func(t *testing.T) {
		removedWorkingDir := t.TempDir()
		t.Chdir(removedWorkingDir)
		if err := os.Remove(removedWorkingDir); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(policy.Target{ID: "target-a", Root: "CANARY-RELATIVE-ROOT"})
		assertPrivateRootError(t, err, "canonicalize root failed", "CANARY-RELATIVE-ROOT")
	})
}

func TestDefinitionIdentityPreservesMarkerSubstrings(t *testing.T) {
	root := t.TempDir()
	writeRootConfig(t, root)
	definitionPath := filepath.Join(root, "secretariat", "tokenizer.md")
	if err := os.MkdirAll(filepath.Dir(definitionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definitionPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(policy.Target{ID: "tokenizer=primary", Root: root, Definitions: []policy.Definition{{Path: "secretariat/tokenizer.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ReadDefinitionMetadata(resolved.Definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "tokenizer" {
		t.Fatalf("path fallback=%q want tokenizer", metadata.Name)
	}

	if err := os.Remove(definitionPath); err != nil {
		t.Fatal(err)
	}
	_, err = ReadDefinitionMetadata(resolved.Definitions[0])
	if err == nil || !strings.Contains(err.Error(), "tokenizer=primary:secretariat/tokenizer.md") {
		t.Fatalf("identity marker substrings were redacted: %v", err)
	}
}

func TestDefinitionDisplayNameRedactsSecretMaterial(t *testing.T) {
	root := t.TempDir()
	writeRootConfig(t, root)
	definitionPath := filepath.Join(root, "agent.md")
	if err := os.WriteFile(definitionPath, []byte("---\nname: 'Agent token=CANARY-SECRET'\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(policy.Target{ID: "target-a", Root: root, Definitions: []policy.Definition{{Path: "agent.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ReadDefinitionMetadata(resolved.Definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "Agent" || strings.Contains(metadata.Name, "CANARY-SECRET") {
		t.Fatalf("display name was not safely redacted: %q", metadata.Name)
	}
}

func TestDefinitionPathFallbackIsNeverEmpty(t *testing.T) {
	root := t.TempDir()
	writeRootConfig(t, root)
	definitionPath := filepath.Join(root, ".md")
	if err := os.WriteFile(definitionPath, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(policy.Target{ID: "target-a", Root: root, Definitions: []policy.Definition{{Path: ".md"}}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ReadDefinitionMetadata(resolved.Definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name == "" {
		t.Fatal("valid policy path produced an empty fallback")
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
		filepath.Join(root, "config.yaml"),
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

// writeRootConfig marks root as a Polytoken configuration directory so
// project-target fixtures satisfy the root canonicalization contract.
func writeRootConfig(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("defaults: {}\n"), 0o600); err != nil {
		t.Fatal(err)
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
