package target

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/geofffranks/codexbar-hooks/internal/document"
)

// legacyModelMarkers identify a definition file as model-bearing when it uses
// dotted-key spellings. Real Polytoken definitions use the nested frontmatter
// form (a `polytoken:` mapping containing `model:` / `fallback_models:`),
// which is detected by parsing the frontmatter; the dotted substrings are kept
// as a permissive legacy fallback. This is a proposal step, and exact
// parsing/ownership happens later.
var legacyModelMarkers = []string{"polytoken.model", "polytoken.fallback_models"}

// Discover proposes model-bearing definition files within a single registered
// target root. It returns relative (forward-slash) paths of files whose YAML
// frontmatter declares polytoken.model or polytoken.fallback_models (nested
// form), or that contain the dotted-key spellings.
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
		if isModelBearing(data) {
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

// discoverWire mirrors the managed slice of a Polytoken definition's
// frontmatter: a nested `polytoken` mapping with `model` and/or
// `fallback_models`. Unknown sibling keys are ignored.
type discoverWire struct {
	Polytoken *struct {
		Model          string   `yaml:"model"`
		FallbackModels []string `yaml:"fallback_models"`
	} `yaml:"polytoken"`
}

// isModelBearing reports whether data is a candidate managed definition:
// either its YAML frontmatter carries a nested polytoken.model /
// polytoken.fallback_models declaration, or it contains a dotted-key legacy
// spelling. Malformed frontmatter never fails discovery — this is a proposal
// step, so an unparseable file is simply not proposed.
func isModelBearing(data []byte) bool {
	// Cheap pre-filter before any parsing.
	if !strings.Contains(string(data), "polytoken") {
		return false
	}
	if containsAny(data, legacyModelMarkers) {
		return true
	}
	block, ok := document.Frontmatter(data)
	if !ok {
		return false
	}
	var w discoverWire
	if err := yaml.Unmarshal(block, &w); err != nil {
		return false
	}
	return w.Polytoken != nil && (w.Polytoken.Model != "" || len(w.Polytoken.FallbackModels) > 0)
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
