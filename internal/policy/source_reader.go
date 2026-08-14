package policy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/geofffranks/polytoken-quota/internal/document"
)

// FilesystemSourceReader reads the managed subset of Polytoken's filesystem
// configuration. It never returns credentials or unrelated config fields.
type FilesystemSourceReader struct {
	GlobalDir   string
	DesiredPath string
}

func (r FilesystemSourceReader) Global(ctx context.Context) (SourceSet, error) {
	return r.readSet(ctx, r.GlobalDir, "global", true, true)
}

func (r FilesystemSourceReader) Projects(ctx context.Context) ([]SourceSet, error) {
	if r.DesiredPath == "" {
		return nil, errors.New("policy: source reader requires desired policy for registered projects")
	}
	d, err := Load(r.DesiredPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("policy: read registered projects: %w", err)
	}
	out := make([]SourceSet, 0, len(d.Projects))
	for _, p := range d.Projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		set, err := r.readSet(ctx, p.Root, p.ID, false, true)
		if err != nil {
			return nil, err
		}
		out = append(out, set)
	}
	return out, nil
}

func (r FilesystemSourceReader) readSet(ctx context.Context, root, id string, global, readConfig bool) (SourceSet, error) {
	if err := ctx.Err(); err != nil {
		return SourceSet{}, err
	}
	if root == "" {
		return SourceSet{}, errors.New("policy: source root is empty")
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return SourceSet{}, fmt.Errorf("policy: source root: %w", err)
	}
	if _, err := os.Stat(root); err != nil {
		return SourceSet{}, fmt.Errorf("policy: source root %q: %w", root, err)
	}
	set := SourceSet{ID: id, Root: root, Global: global}
	if readConfig {
		set.Config, err = readManagedConfig(filepath.Join(root, "config.yaml"))
		if err != nil {
			return SourceSet{}, err
		}
	}
	paths, err := discoverManagedFiles(root)
	if err != nil {
		return SourceSet{}, fmt.Errorf("policy: discover %s source: %w", id, err)
	}
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return SourceSet{}, fmt.Errorf("policy: read definition %q: %w", rel, err)
		}
		def, ok, err := readManagedDefinition(data)
		if err != nil {
			return SourceSet{}, fmt.Errorf("policy: read definition %q: %w", rel, err)
		}
		if ok {
			def.Path = rel
			set.Definitions = append(set.Definitions, def)
		}
	}
	return set, nil
}

type sourceConfigWire struct {
	Providers map[string]struct{} `yaml:"providers"`
	Models    map[string]struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"models"`
	Defaults struct {
		Full string `yaml:"full"`
		Mini string `yaml:"mini"`
		Nano string `yaml:"nano"`
	} `yaml:"defaults"`
	Autonomous struct {
		Classifier string `yaml:"classifier_model"`
	} `yaml:"autonomous_permission_matcher"`
}

func readManagedConfig(path string) (SourceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceConfig{}, fmt.Errorf("policy: read config %q: %w", path, err)
	}
	var w sourceConfigWire
	if err := yaml.Unmarshal(data, &w); err != nil {
		return SourceConfig{}, fmt.Errorf("policy: parse config %q: %w", path, err)
	}
	ids := make([]string, 0, len(w.Providers))
	for id := range w.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := SourceConfig{
		Full: ChainIf(w.Defaults.Full), Mini: ChainIf(w.Defaults.Mini), Nano: ChainIf(w.Defaults.Nano),
		Classifier: ChainIf(w.Autonomous.Classifier),
	}
	for _, id := range ids {
		models := map[string]ModelBaseline{}
		prefix := id + "/"
		for name, entry := range w.Models {
			if len(prefix) > len(name) || name[:len(prefix)] != prefix {
				continue
			}
			mb := ModelBaseline{Enabled: true}
			if entry.Enabled != nil {
				mb.Enabled, mb.HadEnabledKey = *entry.Enabled, true
			}
			models[name] = mb
		}
		out.Providers = append(out.Providers, SourceMapping{ID: id, Models: models})
	}
	return out, nil
}

func discoverManagedFiles(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				if isIgnoredDir(filepath.ToSlash(rel)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if isIgnoredFile(relSlash) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, ok, err := readManagedDefinition(data); err != nil {
			return err
		} else if ok {
			found = append(found, relSlash)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// ignoredDirs are top-level directories under the config root that hold ephemeral
// runtime state, not Polytoken configuration. They must never contribute
// definition files. This MUST stay in sync with staging.excludedDirs.
var ignoredDirs = map[string]bool{
	"read-once":   true,
	"skill-once":  true,
	"superpowers": true,
}

func isIgnoredDir(rel string) bool {
	top := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		top = rel[:i]
	}
	return ignoredDirs[top]
}

// isIgnoredFile reports whether a file (forward-slash relative path) is a backup
// copy or ephemeral artifact that must not be treated as a managed definition.
// Backup copies are especially dangerous: they carry stale managed fields that
// would pollute the reconciliation plan and then fail in staging (the file is
// excluded from staging but the plan still references it).
// This MUST stay in sync with staging.shouldExcludeFile.
func isIgnoredFile(rel string) bool {
	if isIgnoredDir(rel) {
		return true
	}
	base := filepath.Base(rel)
	if base == "prompt_history" {
		return true
	}
	if strings.HasSuffix(base, ".bak") || strings.Contains(base, ".bak-") {
		return true
	}
	return false
}

func ChainIf(s string) Chain {
	if s == "" {
		return nil
	}
	return Chain{s}
}

type sourceDefinitionWire struct {
	Polytoken *struct {
		Model          string   `yaml:"model"`
		FallbackModels []string `yaml:"fallback_models"`
	} `yaml:"polytoken"`
}

func readManagedDefinition(data []byte) (SourceDefinition, bool, error) {
	// Use the shared document locator so discovery/sync agree byte-for-byte
	// with EditFrontmatter about what counts as frontmatter (BOM prefixes and
	// trailing whitespace on the fences included) — a definition the editor
	// can manage must never be invisible to source reading.
	block, ok := document.Frontmatter(data)
	if !ok {
		return SourceDefinition{}, false, nil
	}
	var w sourceDefinitionWire
	if err := yaml.Unmarshal(block, &w); err != nil {
		return SourceDefinition{}, false, err
	}
	if w.Polytoken == nil || (w.Polytoken.Model == "" && len(w.Polytoken.FallbackModels) == 0) {
		return SourceDefinition{}, false, nil
	}
	return SourceDefinition{Model: w.Polytoken.Model, FallbackModels: append([]string(nil), w.Polytoken.FallbackModels...)}, true, nil
}
