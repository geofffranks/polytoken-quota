package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree lays out files under dir and returns dir.
func writeTree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestCanonicalProjectRootKeepsRootWithConfigYAML proves a project root that is
// already a Polytoken configuration directory is returned unchanged.
func TestCanonicalProjectRootKeepsRootWithConfigYAML(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"config.yaml": "defaults: {}\n",
	})
	got, err := CanonicalProjectRoot(root)
	if err != nil {
		t.Fatalf("CanonicalProjectRoot: %v", err)
	}
	if got != root {
		t.Fatalf("CanonicalProjectRoot=%q want unchanged %q", got, root)
	}
}

// TestCanonicalProjectRootAppendsPolytokenSubdir proves a project directory
// root gets its .polytoken subdirectory appended when that subdirectory holds
// the configuration.
func TestCanonicalProjectRootAppendsPolytokenSubdir(t *testing.T) {
	proj := writeTree(t, t.TempDir(), map[string]string{
		".polytoken/config.yaml": "defaults: {}\n",
	})
	got, err := CanonicalProjectRoot(proj)
	if err != nil {
		t.Fatalf("CanonicalProjectRoot: %v", err)
	}
	if want := filepath.Join(proj, ".polytoken"); got != want {
		t.Fatalf("CanonicalProjectRoot=%q want %q", got, want)
	}
}

// TestCanonicalProjectRootErrorIsPathFree proves the error explains the missing
// configuration directory without echoing the root path.
func TestCanonicalProjectRootErrorIsPathFree(t *testing.T) {
	root := t.TempDir() // no config.yaml anywhere
	_, err := CanonicalProjectRoot(root)
	if err == nil {
		t.Fatal("configless project root accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not a Polytoken configuration directory") {
		t.Fatalf("error lacks classification: %q", msg)
	}
	if strings.Contains(msg, root) {
		t.Fatalf("error leaked root path %q: %q", root, msg)
	}
}

// TestProjectsReaderUsesAppendedPolytokenRoot proves the project source reader
// resolves a registered project-directory root to its .polytoken configuration
// layer and reads config from there.
func TestProjectsReaderUsesAppendedPolytokenRoot(t *testing.T) {
	globalDir := writeTree(t, t.TempDir(), map[string]string{
		"config.yaml": "defaults: {}\n",
	})
	proj := writeTree(t, t.TempDir(), map[string]string{
		".polytoken/config.yaml": "defaults:\n  full: codex/gpt\n",
	})
	desiredPath := filepath.Join(t.TempDir(), "desired.yaml")
	desired := "version: 1\nprojects:\n  - id: p\n    root: " + proj + "\n"
	if err := os.WriteFile(desiredPath, []byte(desired), 0o600); err != nil {
		t.Fatal(err)
	}
	sets, err := (FilesystemSourceReader{GlobalDir: globalDir, DesiredPath: desiredPath}).Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("sets=%+v", sets)
	}
	if want := filepath.Join(proj, ".polytoken"); sets[0].Root != want {
		t.Fatalf("set root=%q want %q", sets[0].Root, want)
	}
	if len(sets[0].Config.Full) != 1 || sets[0].Config.Full[0] != "codex/gpt" {
		t.Fatalf("config read from wrong layer: %+v", sets[0].Config)
	}
}
