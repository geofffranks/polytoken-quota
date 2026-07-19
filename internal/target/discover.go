package target

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// modelMarkers identify a definition file as model-bearing for proposal purposes.
// A file containing either is a candidate for management; everything else is
// skipped. Matching is intentionally a simple substring scan: this is a proposal
// step, and exact parsing/ownership happens later.
var modelMarkers = []string{"polytoken.model", "polytoken.fallback_models"}

// Discover proposes model-bearing definition files within a single registered
// target root. It returns relative (forward-slash) paths of files containing
// polytoken.model or polytoken.fallback_models.
//
// Discover walks only the root it is given. It does not follow symlinks, so it
// cannot scan outside the root tree. It makes no changes and adopts nothing: only
// a registered root may be passed in, and callers must add proposals explicitly.
// There is no automatic adoption and no scanning of arbitrary workspace roots.
func Discover(root string) ([]string, error) {
	real, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("target: discover root %q: %w", root, err)
	}
	var found []string
	err = filepath.WalkDir(real, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Never follow symlinks; this keeps discovery inside the registered root.
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if containsAny(data, modelMarkers) {
			rel, err := filepath.Rel(real, path)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

func containsAny(data []byte, markers []string) bool {
	s := string(data)
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
