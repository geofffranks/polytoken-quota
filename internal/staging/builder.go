// Package staging materializes complete, isolated Polytoken validation staging
// roots for the polytoken-quota reconciler. For each target it folds the real
// global source layer together with the registered project layer into one
// private configuration directory, copies every effective startup definition,
// applies the reconciler's managed edits inside staging only, and creates a
// co-located neutral working directory with no .polytoken.
//
// Live source files are never mutated: edits are translated to document.Edit
// calls and applied to staged copies. The staged candidate is the sole transient
// exception to the "no secrets persisted" rule: auth values are either replaced
// with schema-valid inert placeholders (AuthInert) or retained with restrictive
// permissions and guaranteed deletion (AuthTransientSource). AuthUndecided fails
// closed. Staging roots are deleted on every exit path — success, render error,
// cancellation, and timeout.
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/geofffranks/codexbar-hooks/internal/document"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"gopkg.in/yaml.v3"
)

// AuthMode selects how secret-bearing auth values are handled in staging. The
// coordinator (Task 12) selects exactly one from contract evidence; this
// package implements all three branches.
type AuthMode uint8

const (
	// AuthUndecided is the zero value and fail-closed default: Build returns an
	// error rather than producing a candidate with ambiguous auth handling.
	AuthUndecided AuthMode = iota
	// AuthInert replaces secret-bearing auth values with schema-valid inert
	// placeholders, so no source secret is ever present in staging.
	AuthInert
	// AuthTransientSource retains the source auth values verbatim, relying on
	// restrictive file permissions (0600) and guaranteed cleanup.
	AuthTransientSource
)

// Candidate is a materialized validation staging root. ConfigDir is the complete
// standalone configuration directory (passed to --config-dir); WorkingDir is the
// co-located neutral directory with no .polytoken (passed to --working-dir).
// Root is the parent cleaned up alongside both. Cleanup removes Root and is
// idempotent.
type Candidate struct {
	Root          string
	ConfigDir     string
	WorkingDir    string
	UserConfigDir string
	TargetID      string
	cleanup       func() error
}

// Cleanup removes the staging root and neutral working directory. It is
// idempotent: subsequent calls return the first call's result without redoing
// work.
func (c Candidate) Cleanup() error {
	if c.cleanup == nil {
		return nil
	}
	return c.cleanup()
}

// WithoutCleanup returns a copy of the candidate whose Cleanup is a no-op. It
// is used by callers that validate the candidate through a validator (e.g.
// validate.Runner) which calls Cleanup on completion, but then need the staged
// files to remain on disk for a subsequent publish step. The caller retains the
// original candidate (or the returned copy's Root) and is responsible for
// removing the staging root once publish has consumed the staged files.
func (c Candidate) WithoutCleanup() Candidate {
	out := c
	out.cleanup = nil
	return out
}

// Layer is one source layer's materialized contents: the config.yaml bytes and
// every other file under the layer's configuration root, keyed by forward-slash
// relative path. Bytes are returned verbatim; no environment expansion occurs.
type Layer struct {
	Config []byte
	Files  map[string][]byte
}

// SourceMaterializer abstracts reading the canonical global and project source
// layers. Production uses real filesystem reads via FSMaterializer; tests use
// fixture-backed implementations. Global returns the global layer; Project
// returns the registered project layer for res and ok=true, or ok=false (with an
// empty layer) for the global target, which has no project override.
type SourceMaterializer interface {
	Global(ctx context.Context) (Layer, error)
	Project(ctx context.Context, res target.Resolved) (Layer, bool, error)
}

// Builder materializes isolated validation staging roots. TempRoot is the parent
// utility temporary directory; AuthMode selects the auth branch; Sources reads
// the canonical source layers.
type Builder struct {
	TempRoot string
	AuthMode AuthMode
	Sources  SourceMaterializer
}

// Build materializes a complete standalone staging root for res, applies plan's
// managed edits inside staging only, and returns a Candidate whose WorkingDir is
// neutral (no .polytoken) and never the live project root. On any failure
// (render error, cancellation, timeout) the partial staging root is removed
// before the error is returned.
func (b Builder) Build(ctx context.Context, res target.Resolved, plan reconcile.Plan) (Candidate, error) {
	if b.AuthMode == AuthUndecided {
		return Candidate{}, errors.New("staging: auth mode is undecided")
	}
	if b.Sources == nil {
		return Candidate{}, errors.New("staging: no source materializer configured")
	}
	if b.TempRoot == "" {
		return Candidate{}, errors.New("staging: no temp root configured")
	}
	if err := ctx.Err(); err != nil {
		return Candidate{}, err
	}

	root := stageRoot(b.TempRoot, res.ID)
	configDir := filepath.Join(root, configSubdir)
	userConfigDir := filepath.Join(root, userConfigSubdir)
	workDir := filepath.Join(root, workSubdir)
	if err := os.MkdirAll(configDir, dirPerm); err != nil {
		return Candidate{}, fmt.Errorf("staging: create config dir: %w", err)
	}
	cleanup := newCleanup(root)
	if err := b.stage(ctx, configDir, userConfigDir, workDir, res, plan); err != nil {
		_ = cleanup()
		return Candidate{}, err
	}
	return Candidate{
		Root:          root,
		ConfigDir:     configDir,
		WorkingDir:    workDir,
		UserConfigDir: userConfigDir,
		TargetID:      res.ID,
		cleanup:       cleanup,
	}, nil
}

// stage fills configDir with the merged effective config and definitions,
// applies the plan edits, and creates the neutral workDir. Any error leaves the
// caller responsible for removing root.
func (b Builder) stage(ctx context.Context, configDir, userConfigDir, workDir string, res target.Resolved, plan reconcile.Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	global, err := b.Sources.Global(ctx)
	if err != nil {
		return fmt.Errorf("global layer: %w", err)
	}
	var project Layer
	if !res.Global {
		project, _, err = b.Sources.Project(ctx, res)
		if err != nil {
			return fmt.Errorf("project layer: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfgBytes, err := buildEffectiveConfig(global.Config, project.Config, b.AuthMode)
	if err != nil {
		return fmt.Errorf("merge config: %w", err)
	}
	if err := writeStaged(filepath.Join(configDir, stagedConfigFile), cfgBytes); err != nil {
		return err
	}
	if err := writeStaged(filepath.Join(userConfigDir, "polytoken", stagedConfigFile), cfgBytes); err != nil {
		return err
	}
	for rel, data := range mergeFiles(global.Files, project.Files) {
		if b.AuthMode == AuthInert && secretBearingFile(rel) {
			return fmt.Errorf("staging: refusing secret-bearing auxiliary file %q in AuthInert mode", rel)
		}
		if err := writeStaged(filepath.Join(configDir, filepath.FromSlash(rel)), data); err != nil {
			return err
		}
		if err := writeStaged(filepath.Join(userConfigDir, "polytoken", filepath.FromSlash(rel)), data); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := applyPlanEdits(configDir, plan); err != nil {
		return fmt.Errorf("apply edits: %w", err)
	}
	if err := applyPlanEdits(filepath.Join(userConfigDir, "polytoken"), plan); err != nil {
		return fmt.Errorf("apply user edits: %w", err)
	}
	if err := os.MkdirAll(workDir, dirPerm); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}
	// Defensive: the freshly created neutral workdir must never carry a
	// .polytoken entry that could contaminate validation via discovery.
	if _, err := os.Stat(filepath.Join(workDir, ".polytoken")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("neutral workdir contaminated with .polytoken")
	}
	return nil
}

// applyPlanEdits translates the reconcile.FieldEdits into document.Edit calls
// and applies them to the staged copies only. Config edits target config.yaml
// via EditYAML; definition edits target their frontmatter via EditFrontmatter.
func applyPlanEdits(configDir string, plan reconcile.Plan) error {
	byFile := make(map[string][]document.Edit)
	var order []string
	for _, fe := range plan.Edits {
		if _, ok := byFile[fe.File]; !ok {
			order = append(order, fe.File)
		}
		byFile[fe.File] = append(byFile[fe.File], fieldToEdit(fe))
	}
	for _, file := range order {
		edits := byFile[file]
		stagedPath := filepath.Join(configDir, filepath.FromSlash(file))
		raw, err := os.ReadFile(stagedPath)
		if err != nil {
			return fmt.Errorf("read staged %s: %w", file, err)
		}
		var out []byte
		if file == stagedConfigFile {
			out, err = document.EditYAML(raw, edits)
		} else {
			out, err = document.EditFrontmatter(raw, edits)
		}
		if err != nil {
			return fmt.Errorf("edit staged %s: %w", file, err)
		}
		if err := os.WriteFile(stagedPath, out, filePerm); err != nil {
			return fmt.Errorf("write staged %s: %w", file, err)
		}
	}
	return nil
}

// fieldToEdit maps one reconcile.FieldEdit to one document.Edit. Exactly one
// value carrier survives; Remove wins when set.
func fieldToEdit(fe reconcile.FieldEdit) document.Edit {
	e := document.Edit{Path: fe.Path}
	switch {
	case fe.Remove:
		e.Remove = true
	case fe.Scalar != nil:
		e.Kind = document.Scalar
		e.Scalar = fe.Scalar
	case len(fe.Sequence) > 0:
		e.Kind = document.Sequence
		e.Sequence = fe.Sequence
	case fe.Enabled != nil:
		e.Kind = document.Boolean
		e.Bool = fe.Enabled
	}
	return e
}

// buildEffectiveConfig deep-merges the global and project config layers (project
// wins), applies the auth branch, and marshals the result. Parsing through a map
// guarantees no environment-variable secret expansion: values stay literal. No
// source secret is written when mode is AuthInert.
func buildEffectiveConfig(global, project []byte, mode AuthMode) ([]byte, error) {
	g, err := decodeConfig(global)
	if err != nil {
		return nil, fmt.Errorf("parse global: %w", err)
	}
	p, err := decodeConfig(project)
	if err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	merged := deepMerge(g, p)
	if mode == AuthInert {
		redactSecrets(merged)
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal effective config: %w", err)
	}
	return out, nil
}

// decodeConfig parses YAML bytes into a generic map. Empty input yields an empty
// map. yaml.v3 decodes nested mappings as map[string]interface{}.
func decodeConfig(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// deepMerge returns a new map that overlays src onto dst, recursing into nested
// maps (project wins at every level). Scalars, sequences, and non-map values
// from src replace dst's. Neither input is mutated.
func deepMerge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if existing, ok := out[k]; ok {
			if em, eok := existing.(map[string]any); eok {
				if vm, vok := v.(map[string]any); vok {
					out[k] = deepMerge(em, vm)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

// mergeFiles overlays project files onto global files (project wins on a shared
// relative path). Neither input is mutated.
func mergeFiles(global, project map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(global)+len(project))
	for k, v := range global {
		out[k] = v
	}
	for k, v := range project {
		out[k] = v
	}
	return out
}

// authValueKeys names leaf keys whose scalar values are treated as secret-bearing
// auth and redacted under AuthInert. Matching is case-insensitive on the final
// path segment.
var authValueKeys = map[string]bool{
	// "key" covers the real Polytoken provider auth shape
	// (providers.<id>.auth.key). Redacting every leaf named key is
	// deliberately broad: staging is validation-only, and over-redacting a
	// non-secret string is harmless while under-redacting leaks a credential.
	"key":           true,
	"api_key":       true,
	"api-key":       true,
	"apikey":        true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"access_token":  true,
	"auth_token":    true,
	"refresh_token": true,
	"client_secret": true,
	"private_key":   true,
	"authorization": true,
	"credentials":   true,
}

// inertSecret is the schema-valid placeholder substituted for redacted secrets.
// It is a plain non-empty string, acceptable for any string-typed auth field.
const inertSecret = "inert-validation-placeholder"

// redactSecrets walks m recursively and replaces the scalar value of any
// auth-bearing key with the inert placeholder. Nested maps are descended; map
// values are updated in place.
func redactSecrets(m map[string]any) {
	for k, v := range m {
		if vm, ok := v.(map[string]any); ok {
			redactSecrets(vm)
			continue
		}
		if authValueKeys[strings.ToLower(k)] {
			m[k] = inertSecret
		}
	}
}

// FSMaterializer is the production SourceMaterializer backed by the real
// filesystem. GlobalDir is the canonical global Polytoken configuration
// directory.
type FSMaterializer struct {
	GlobalDir string
}

// Global reads the global layer from GlobalDir.
func (m FSMaterializer) Global(ctx context.Context) (Layer, error) {
	if err := ctx.Err(); err != nil {
		return Layer{}, err
	}
	if m.GlobalDir == "" {
		return Layer{}, errors.New("staging: empty global dir")
	}
	return readLayer(m.GlobalDir)
}

// Project reads the project layer from res.CanonicalRoot, or returns ok=false
// for the global target.
func (m FSMaterializer) Project(ctx context.Context, res target.Resolved) (Layer, bool, error) {
	if err := ctx.Err(); err != nil {
		return Layer{}, false, err
	}
	if res.Global {
		return Layer{}, false, nil
	}
	layer, err := readLayer(res.CanonicalRoot)
	if err != nil {
		return Layer{}, false, err
	}
	return layer, true, nil
}

// readLayer reads config.yaml plus every other regular file under dir, keyed by
// forward-slash relative path. config.yaml at the dir root is returned as
// Config; a nested file named config.yaml is treated as an ordinary file.
// Ephemeral runtime directories (read-once, skill-once, superpowers), the
// prompt_history file, and backup copies (*.bak) are excluded — they are not
// configuration and backup files may carry raw secrets that bypass AuthInert
// redaction.
func readLayer(dir string) (Layer, error) {
	cfg, err := os.ReadFile(filepath.Join(dir, stagedConfigFile))
	if err != nil {
		return Layer{}, fmt.Errorf("read config: %w", err)
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return rerr
			}
			if isExcludedDir(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == stagedConfigFile && filepath.Dir(path) == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		if shouldExcludeFile(relSlash) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		files[relSlash] = data
		return nil
	})
	if err != nil {
		return Layer{}, fmt.Errorf("walk %s: %w", dir, err)
	}
	return Layer{Config: cfg, Files: files}, nil
}

// excludedDirs lists top-level directory names under the config root that hold
// ephemeral runtime state rather than Polytoken configuration. Walking into
// these is skipped entirely.
var excludedDirs = map[string]bool{
	"read-once":   true, // session-tracking JSONL
	"skill-once":  true, // session-tracking JSONL
	"superpowers": true, // runtime state scripts
}

// isExcludedDir reports whether rel (a forward-slash path relative to the config
// root) is inside a top-level excluded directory.
func isExcludedDir(rel string) bool {
	top := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		top = rel[:i]
	}
	return excludedDirs[top]
}

// shouldExcludeFile reports whether a file (forward-slash path relative to the
// config root) should be skipped because it is ephemeral runtime state or a
// backup copy, not live configuration.
func shouldExcludeFile(rel string) bool {
	if isExcludedDir(rel) {
		return true
	}
	base := filepath.Base(rel)
	if base == "prompt_history" {
		return true
	}
	// Backup copies contain raw config with real secrets and bypass AuthInert
	// redaction — never stage them.
	if strings.HasSuffix(base, ".bak") {
		return true
	}
	if strings.Contains(base, ".bak-") {
		return true
	}
	return false
}

func secretBearingFile(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	// Distinctive markers — substring match is safe and precise.
	for _, marker := range []string{".env", "credential", "secret"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	// Short markers that embed in benign words (e.g. "token" in "polytoken",
	// "auth" in "authorize") — require word boundaries so only standalone
	// occurrences match.
	for _, marker := range []string{"token", "auth"} {
		if wordBoundaryContains(base, marker) {
			return true
		}
	}
	return false
}

// wordBoundaryContains reports whether s contains word as a token bounded by
// non-alphabetic characters (or the start/end of the string). This prevents
// "token" from matching inside "polytoken" while still matching "access_token"
// or "token.json". s is assumed already lowercased.
func wordBoundaryContains(s, word string) bool {
	idx := 0
	for {
		pos := strings.Index(s[idx:], word)
		if pos < 0 {
			return false
		}
		absPos := idx + pos
		end := absPos + len(word)
		leftOK := absPos == 0 || !isLowerAlpha(s[absPos-1])
		rightOK := end == len(s) || !isLowerAlpha(s[end])
		if leftOK && rightOK {
			return true
		}
		idx = absPos + 1
	}
}

func isLowerAlpha(b byte) bool {
	return b >= 'a' && b <= 'z'
}

// writeStaged writes data to path, creating parent directories with dirPerm and
// the file with filePerm.
func writeStaged(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// newCleanup returns an idempotent remover for root.
func newCleanup(root string) func() error {
	var once sync.Once
	var firstErr error
	return func() error {
		once.Do(func() {
			firstErr = os.RemoveAll(root)
		})
		return firstErr
	}
}

// stageRoot returns the deterministic staging root path under tempRoot for the
// given target id. The coordinator serializes builds per target under an
// advisory lock, so deterministic naming is safe and lets callers verify cleanup
// on every exit path.
func stageRoot(tempRoot, id string) string {
	return filepath.Join(tempRoot, "quota-stage-"+sanitizeID(id))
}

// sanitizeID reduces a target id to filesystem-safe characters.
func sanitizeID(id string) string {
	if id == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		return "default"
	}
	return s
}

const (
	configSubdir     = "config"
	userConfigSubdir = "user-config"
	workSubdir       = "work"
	stagedConfigFile = "config.yaml"
	dirPerm          = 0o700
	filePerm         = 0o600
)
