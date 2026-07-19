// Package target resolves registered Polytoken reconciliation targets and proposes
// model-bearing definition files within a single registered root.
//
// Resolve canonicalizes a target's root and validates that every managed
// definition file stays inside it, rejecting path traversal and symlinked managed
// files by default. Discover proposes files containing polytoken.model or
// polytoken.fallback_models; it walks only the root it is given, never scans
// arbitrary workspace roots, and never adopts files automatically.
package target

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
)

// Resolved is a canonicalized, validated target. DefinitionFiles holds the
// canonical absolute paths of the managed definition files, all guaranteed to be
// contained under CanonicalRoot.
type Resolved struct {
	ID              string
	CanonicalRoot   string
	Global          bool
	DefinitionFiles []string
}

// ErrSymlinkManagedFile signals a managed definition path that is a symlink,
// rejected by default to prevent writes escaping the registered root.
var ErrSymlinkManagedFile = errors.New("target: managed definition file is a symlink")

// Resolve canonicalizes the target root and validates every managed definition
// path. It rejects an empty root, path traversal outside the root, symlinks (by
// default), paths that canonicalize outside the root, and duplicate definition
// files.
func Resolve(in policy.Target) (Resolved, error) {
	if in.Root == "" {
		return Resolved{}, errors.New("target: empty root")
	}
	root, err := filepath.Abs(filepath.Clean(in.Root))
	if err != nil {
		return Resolved{}, fmt.Errorf("target: canonicalize root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("target: resolve root %q: %w", in.Root, err)
	}

	out := Resolved{ID: in.ID, CanonicalRoot: realRoot, Global: in.Global}
	seen := map[string]bool{}
	for _, def := range in.Definitions {
		canon, err := resolveDefinition(realRoot, def.Path)
		if err != nil {
			if in.ID != "" {
				return Resolved{}, fmt.Errorf("target: %s: %w", in.ID, err)
			}
			return Resolved{}, err
		}
		if seen[canon] {
			return Resolved{}, fmt.Errorf("target: duplicate definition file %q", canon)
		}
		seen[canon] = true
		out.DefinitionFiles = append(out.DefinitionFiles, canon)
	}
	sort.Strings(out.DefinitionFiles)
	return out, nil
}

func resolveDefinition(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty definition path")
	}
	// Reject traversal that escapes the root before touching the filesystem.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path traversal outside root: %q", rel)
	}
	abs := rel
	if !filepath.IsAbs(rel) {
		abs = filepath.Join(root, rel)
	}
	// Symlink rejection (default): Lstat catches a symlink at the entry itself.
	li, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", rel, err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %q", ErrSymlinkManagedFile, rel)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", rel, err)
	}
	if !isWithin(root, real) {
		return "", fmt.Errorf("path outside root: %q", rel)
	}
	return real, nil
}

// isWithin reports whether path is contained under root after canonicalization.
func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
