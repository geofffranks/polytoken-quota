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

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// ResolvedDefinition is one explicitly registered definition after exact path
// validation. Public identity is limited to the target ID and normalized,
// slash-separated policy-relative path. canonicalPath is retained internally for
// exact approved-file reads and is never exposed through DTOs or errors.
type ResolvedDefinition struct {
	TargetID   string
	PolicyPath string
	Chain      policy.Chain

	canonicalPath string
}

// Resolved is a canonicalized, validated target. Definitions keeps each managed
// definition's approved canonical path together with its public target/path
// identity and exact desired chain; callers never reconstruct paths.
type Resolved struct {
	ID            string
	CanonicalRoot string
	Global        bool
	Definitions   []ResolvedDefinition
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
		return Resolved{}, errors.New("target: resolve root failed")
	}

	out := Resolved{ID: in.ID, CanonicalRoot: realRoot, Global: in.Global}
	seen := map[string]bool{}
	for _, def := range in.Definitions {
		policyPath, canon, err := resolveDefinition(realRoot, def.Path)
		if err != nil {
			return Resolved{}, definitionError(in.ID, def.Path, err)
		}
		if seen[canon] {
			return Resolved{}, definitionError(in.ID, policyPath, errors.New("duplicate definition file"))
		}
		seen[canon] = true
		out.Definitions = append(out.Definitions, ResolvedDefinition{
			TargetID:      in.ID,
			PolicyPath:    policyPath,
			Chain:         append(policy.Chain(nil), def.Chain...),
			canonicalPath: canon,
		})
	}
	sort.Slice(out.Definitions, func(i, j int) bool {
		return out.Definitions[i].PolicyPath < out.Definitions[j].PolicyPath
	})
	return out, nil
}

func resolveDefinition(root, rel string) (string, string, error) {
	if rel == "" {
		return "", "", errors.New("empty definition path")
	}
	if filepath.IsAbs(rel) {
		return "", "", errors.New("absolute definition path is not policy-relative")
	}
	// Reject traversal that escapes the root before touching the filesystem.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", "", errors.New("path traversal outside root")
	}
	policyPath := filepath.ToSlash(cleaned)
	abs := filepath.Join(root, cleaned)
	// Symlink rejection (default): Lstat catches a symlink at the entry itself.
	li, err := os.Lstat(abs)
	if err != nil {
		return "", "", errors.New("stat failed")
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return "", "", ErrSymlinkManagedFile
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", errors.New("resolve failed")
	}
	if !isWithin(root, real) {
		return "", "", errors.New("path outside root")
	}
	return policyPath, real, nil
}

func definitionError(targetID, policyPath string, err error) error {
	location := safeLocation(targetID, policyPath)
	if location == "" {
		return fmt.Errorf("target: definition: %w", err)
	}
	return fmt.Errorf("target: definition %s: %w", location, err)
}

func safeLocation(targetID, policyPath string) string {
	targetID = sanitizeLabel(targetID)
	policyPath = filepath.ToSlash(filepath.Clean(policyPath))
	if filepath.IsAbs(policyPath) || policyPath == "." || policyPath == ".." || strings.HasPrefix(policyPath, "../") {
		policyPath = "<invalid>"
	} else {
		policyPath = sanitizeLabel(policyPath)
	}
	switch {
	case targetID != "" && policyPath != "":
		return targetID + ":" + policyPath
	case targetID != "":
		return targetID
	default:
		return policyPath
	}
}

// isWithin reports whether path is contained under root after canonicalization.
func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
