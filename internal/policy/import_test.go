package policy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// --- test doubles -----------------------------------------------------------

// staticReader is a fixture SourceReader returning fixed global/project source
// sets. It replaces filesystem reads so the proposal/import logic is tested
// against controlled, deterministic inputs.
type staticReader struct {
	global   SourceSet
	projects []SourceSet
}

func (r staticReader) Global(context.Context) (SourceSet, error) { return r.global, nil }
func (r staticReader) Projects(context.Context) ([]SourceSet, error) {
	return r.projects, nil
}

// fixtureDefinitions is a registry of named fixture definition files. "model.md"
// carries a live polytoken.model; "polytoken-only.md" has a polytoken block but no
// model field (and is therefore not model-bearing); "drift.md" references a model
// outside the provider graph (ambiguous managed drift).
var fixtureDefinitions = map[string]SourceDefinition{
	"model.md":          {Path: "model.md", Model: "codex/gpt-5.6-sol"},
	"polytoken-only.md": {Path: "polytoken-only.md"},
	"fallback.md":       {Path: "fallback.md", FallbackModels: []string{"codex/gpt-5.6-sol"}},
	"drift.md":          {Path: "drift.md", Model: "codex/unmapped"},
}

// globalMapping is the single concrete provider mapping shared by the fixtures.
func globalMapping(enabled bool) SourceMapping {
	return SourceMapping{
		ID:                 "codex",
		CodexBarProviders:  []string{"codex"},
		PolytokenProviders: []string{"codex"},
		Models:             map[string]ModelBaseline{"codex/gpt-5.6-sol": {Enabled: enabled}},
	}
}

// fixtureSources builds a SourceReader whose global target manages the named
// fixture definition files under a single concrete codex mapping.
func fixtureSources(names ...string) staticReader {
	defs := make([]SourceDefinition, 0, len(names))
	for _, n := range names {
		defs = append(defs, fixtureDefinitions[n])
	}
	return staticReader{
		global: SourceSet{
			ID:          "global",
			Root:        "/home/user/.config/polytoken",
			Global:      true,
			Config:      SourceConfig{Providers: []SourceMapping{globalMapping(true)}},
			Definitions: defs,
		},
	}
}

// importSources builds sources for the guarded-import table. The degraded flag
// reflects a degraded provider's disabled model baseline (the import guard is
// driven by observed state, not the source); ambiguous adds an off-graph managed
// reference (ambiguous drift).
func importSources(degraded, ambiguous bool) staticReader {
	r := fixtureSources()
	if degraded {
		r.global.Config.Providers = []SourceMapping{globalMapping(false)}
	}
	if ambiguous {
		r.global.Definitions = append(r.global.Definitions, fixtureDefinitions["drift.md"])
	}
	return r
}

func driftSources() staticReader { return importSources(false, true) }

// fixtureState returns observed provider state; when degraded the codex provider
// is exhausted (effective mode disabled).
func fixtureState(degraded bool) state.State {
	if !degraded {
		return state.State{}
	}
	return state.State{Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaExhausted, Availability: state.Available},
	}}
}

func normalState() state.State {
	return state.State{Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaNormal, Availability: state.Available},
	}}
}

func desiredFixture() Desired {
	return Desired{Version: 1, Providers: map[MappingID]Mapping{
		"codex": {Models: map[string]ModelBaseline{"codex/gpt-5.6-sol": {Enabled: true}}},
	}}
}

func writeDesired(t *testing.T, _ string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n# pre-existing policy\n"), 0o600); err != nil {
		t.Fatalf("write desired: %v", err)
	}
	return path
}

func newWriter(path string) Writer { return NewWriter(path) }

// definitionPaths collects every managed definition path across global and
// project targets, sorted.
func definitionPaths(d Desired) []string {
	var paths []string
	for _, def := range d.Global.Definitions {
		paths = append(paths, def.Path)
	}
	for _, p := range d.Projects {
		for _, def := range p.Definitions {
			paths = append(paths, def.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// concreteModels collects the concrete (non-empty, non-glob) enumerated model
// base names across every provider mapping, sorted.
func concreteModels(d Desired) []string {
	var models []string
	for _, m := range d.Providers {
		for base := range m.Models {
			if base != "" && !isGlob(base) {
				models = append(models, base)
			}
		}
	}
	sort.Strings(models)
	return models
}

// --- Task 8 tests -----------------------------------------------------------

// TestInitProposesOnlyModelBearingDefinitions proves Init proposes only
// definitions that carry polytoken.model/polytoken.fallback_models, materializes
// exact concrete model enumeration, and reports no uncovered references for a
// clean source set.
func TestInitProposesOnlyModelBearingDefinitions(t *testing.T) {
	d, r, err := Init(context.Background(), fixtureSources("model.md", "polytoken-only.md"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := definitionPaths(d); !slices.Equal(got, []string{"model.md"}) {
		t.Fatalf("definition paths got=%v want=[model.md]", got)
	}
	if len(concreteModels(d)) == 0 {
		t.Fatal("no explicit model enumeration")
	}
	if len(r.Uncovered) != 0 {
		t.Fatalf("uncovered=%v", r.Uncovered)
	}
}

// TestInitReportsInvalidReferences proves Init reports a model-bearing definition
// whose chain references a model outside the provider graph as an uncovered
// reference rather than silently proposing an unresolved chain.
func TestInitReportsInvalidReferences(t *testing.T) {
	_, r, err := Init(context.Background(), fixtureSources("drift.md"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(r.Uncovered) != 1 || r.Uncovered[0].File != "drift.md" {
		t.Fatalf("uncovered=%v", r.Uncovered)
	}
}

// TestWriterCreateAtomicRejectsExistingDesired proves exclusive create rejects an
// existing desired.yaml with ErrDesiredExists, preserves its bytes, and points to
// `polytoken-quota init --force`.
func TestFilesystemSourceReaderReadsManagedGlobalFields(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(`providers:
  codex:
    api_key: secret-must-not-be-copied
models:
  codex/gpt:
    enabled: false
defaults:
  full: codex/gpt
  mini: codex/gpt
autonomous_permission_matcher:
  classifier_model: codex/gpt
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "agent.md"), []byte(`---
polytoken:
  model: codex/gpt
  fallback_models:
    - codex/gpt
other: private
---
body
`), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := FilesystemSourceReader{GlobalDir: root}
	got, err := reader.Global(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Config.Providers) != 1 || got.Config.Providers[0].Models["codex/gpt"].Enabled {
		t.Fatalf("providers=%+v", got.Config.Providers)
	}
	if !reflect.DeepEqual(got.Config.Full, Chain{"codex/gpt"}) || !reflect.DeepEqual(got.Config.Classifier, Chain{"codex/gpt"}) {
		t.Fatalf("managed chains=%+v", got.Config)
	}
	if len(got.Definitions) != 1 || got.Definitions[0].Model != "codex/gpt" {
		t.Fatalf("definitions=%+v", got.Definitions)
	}
	if got.Definitions[0].Path != "agents/agent.md" {
		t.Fatalf("definition path=%q", got.Definitions[0].Path)
	}
}

// TestFilesystemSourceReaderAcceptsBOMAndWhitespaceFences proves definitions
// with a UTF-8 BOM prefix or trailing whitespace on the frontmatter fences are
// discovered: the document editor accepts them, so source reading must too, or
// an editable managed definition would be invisible to init/sync.
func TestFilesystemSourceReaderAcceptsBOMAndWhitespaceFences(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	bom := "\ufeff---\npolytoken:\n  model: codex/gpt\n---\nbody\n"
	ws := "---  \r\npolytoken:\r\n  model: zai/glm\r\n---\r\nbody\r\n"
	if err := os.WriteFile(filepath.Join(root, "agents", "bom.md"), []byte(bom), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "ws.md"), []byte(ws), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (FilesystemSourceReader{GlobalDir: root}).Global(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, d := range got.Definitions {
		found[d.Path] = d.Model
	}
	if found["agents/bom.md"] != "codex/gpt" {
		t.Fatalf("BOM-prefixed definition not read: %+v", got.Definitions)
	}
	if found["agents/ws.md"] != "zai/glm" {
		t.Fatalf("whitespace-fence definition not read: %+v", got.Definitions)
	}
}

func TestFilesystemSourceReaderUsesOnlyRegisteredProjects(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global")
	project := filepath.Join(root, "project")
	unregistered := filepath.Join(root, "unregistered")
	for _, dir := range []string{global, project, unregistered} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("models: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	desiredPath := filepath.Join(root, "desired.yaml")
	if err := os.WriteFile(desiredPath, []byte("version: 1\nproviders: {}\nglobal: {id: global, root: "+global+"}\nprojects:\n  - id: registered\n    root: "+project+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (FilesystemSourceReader{GlobalDir: global, DesiredPath: desiredPath}).Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "registered" || got[0].Root != project {
		t.Fatalf("projects=%+v", got)
	}
}

func TestFilesystemSourceReaderExcludesBackupAndEphemeralFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defContent := []byte("---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	// Real definition — must be discovered.
	if err := os.MkdirAll(filepath.Join(root, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "subagents", "agent.md"), defContent, 0o600); err != nil {
		t.Fatal(err)
	}
	// Backup copies with valid frontmatter — must NOT be discovered.
	for _, bak := range []string{"agent.md.bak", "agent.md.bak-20260802", "agent.md.20260802T000000Z.bak"} {
		if err := os.WriteFile(filepath.Join(root, "subagents", bak), defContent, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Definition-like file inside ephemeral dirs — must NOT be discovered.
	for _, dir := range []string{"read-once", "skill-once", "superpowers"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "fake.md"), defContent, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// prompt_history at root — must not be discovered.
	if err := os.WriteFile(filepath.Join(root, "prompt_history"), defContent, 0o600); err != nil {
		t.Fatal(err)
	}

	reader := FilesystemSourceReader{GlobalDir: root}
	got, err := reader.Global(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, d := range got.Definitions {
		paths = append(paths, d.Path)
	}
	if len(paths) != 1 || paths[0] != "subagents/agent.md" {
		t.Fatalf("expected only [subagents/agent.md], got %v", paths)
	}
}

func TestWriterCreateAtomicRejectsExistingDesired(t *testing.T) {
	path := writeDesired(t, "existing")
	before, _ := os.ReadFile(path)
	_, err := newWriter(path).CreateAtomic(context.Background(), Desired{})
	if !errors.Is(err, ErrDesiredExists) {
		t.Fatalf("err=%v want ErrDesiredExists", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing desired.yaml changed")
	}
	if !strings.Contains(err.Error(), "use polytoken-quota init --force") {
		t.Fatalf("error=%v", err)
	}
}

// TestWriterCreateAtomicCreatesExclusiveWithMode0600 proves a fresh exclusive
// create writes mode-0600 bytes and that a second create is rejected.
func TestWriterCreateAtomicCreatesExclusiveWithMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.yaml")
	d := Desired{
		Version: supportedVersion,
		Providers: map[MappingID]Mapping{
			"codex": {
				CodexBarProviders:  []string{"codex"},
				PolytokenProviders: []string{"codex"},
				Models:             map[string]ModelBaseline{"codex/gpt-5.6-sol": {Enabled: true}},
			},
		},
		Operational: defaultOperational,
	}
	if _, err := NewWriter(path).CreateAtomic(context.Background(), d); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%#o want 0600", info.Mode().Perm())
	}
	if _, err := NewWriter(path).CreateAtomic(context.Background(), Desired{}); !errors.Is(err, ErrDesiredExists) {
		t.Fatalf("second create err=%v want ErrDesiredExists", err)
	}
}

// TestImportGuards proves Import rejects a degraded provider and ambiguous drift
// unless --force, and that forced import warns temporary ordering may become
// durable intent.
func TestImportGuards(t *testing.T) {
	for _, tc := range []struct {
		degraded, ambiguous, force, wantErr bool
	}{
		{true, false, false, true},
		{false, true, false, true},
		{true, true, true, false},
	} {
		_, r, err := Import(context.Background(), importSources(tc.degraded, tc.ambiguous), fixtureState(tc.degraded), tc.force)
		if (err != nil) != tc.wantErr {
			t.Fatalf("case=%+v err=%v", tc, err)
		}
		if tc.force && !slices.ContainsFunc(r.Warnings, func(s string) bool {
			return strings.Contains(s, "temporary ordering")
		}) {
			t.Fatalf("case=%+v missing force warning", tc)
		}
	}
}

// TestManagedDriftNotAdopted proves a managed live difference (an off-graph
// managed reference) is reported as drift and never silently adopted: Import
// returns an empty Desired and an error when not forced.
func TestManagedDriftNotAdopted(t *testing.T) {
	before := desiredFixture()
	after, r, err := Import(context.Background(), driftSources(), normalState(), false)
	if err == nil || !reflect.DeepEqual(after, Desired{}) || len(r.Drift) == 0 {
		t.Fatalf("after=%+v report=%+v err=%v", after, r, err)
	}
	_ = before
}

// TestPolicyWriterCreateFaultMatrix proves exclusive creation has one atomic
// no-replace commit boundary. Every earlier fault leaves no destination or
// partial bytes; cleanup and directory-sync faults after link are committed
// warnings. The real filesystem still performs every non-faulted operation.
func TestPolicyWriterCreateFaultMatrix(t *testing.T) {
	desired := desiredFixture()
	want, err := marshalDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stage     string
		committed bool
		warning   bool
	}{
		{"open", false, false},
		{"write", false, false},
		{"chmod", false, false},
		{"file-fsync", false, false},
		{"close", false, false},
		{"link", false, false},
		{"unlink", true, true},
		{"dir-fsync", true, true},
		{"", true, false},
	} {
		name := tc.stage
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "desired.yaml")
			result, err := newWriterWithFS(path, &policyWriterFaultFS{fail: tc.stage}).CreateAtomic(context.Background(), desired)
			if tc.committed {
				if err != nil || !result.Committed || (result.Warning != nil) != tc.warning {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("published bytes=%q readErr=%v want=%q", got, readErr, want)
				}
				info, statErr := os.Stat(path)
				if statErr != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("mode=%v statErr=%v want=0600", infoMode(info), statErr)
				}
			} else {
				if err == nil || result.Committed {
					t.Fatalf("pre-commit stage result=%+v err=%v", result, err)
				}
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("destination visible after pre-commit fault: %v", statErr)
				}
			}
			if tc.stage != "unlink" {
				assertNoPolicyTemps(t, dir)
			}
		})
	}

	t.Run("existing-destination", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "desired.yaml")
		before := []byte("existing-policy-bytes\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := newWriterWithFS(path, &policyWriterFaultFS{}).CreateAtomic(context.Background(), desired)
		if !errors.Is(err, ErrDesiredExists) || result.Committed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("existing bytes changed: after=%q err=%v", after, readErr)
		}
		assertNoPolicyTemps(t, dir)
	})
}

// TestPolicyWriterReplaceFaultMatrix proves rename is replacement's commit
// boundary: every earlier failure preserves the exact old bytes, while cleanup
// and directory-fsync failures after rename report committed warnings.
func TestPolicyWriterReplaceFaultMatrix(t *testing.T) {
	desired := desiredFixture()
	want, err := marshalDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stage     string
		committed bool
		warning   bool
	}{
		{"open", false, false},
		{"write", false, false},
		{"chmod", false, false},
		{"file-fsync", false, false},
		{"close", false, false},
		{"rename", false, false},
		{"unlink", true, true},
		{"dir-fsync", true, true},
		{"", true, false},
	} {
		name := tc.stage
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "desired.yaml")
			before := []byte("version: 1\n# exact old policy bytes\n")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := newWriterWithFS(path, &policyWriterFaultFS{fail: tc.stage}).ReplaceAtomic(context.Background(), desired)
			if tc.committed {
				if err != nil || !result.Committed || (result.Warning != nil) != tc.warning {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("replacement bytes=%q readErr=%v want=%q", got, readErr, want)
				}
			} else {
				if err == nil || result.Committed {
					t.Fatalf("pre-commit stage result=%+v err=%v", result, err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, before) {
					t.Fatalf("old bytes changed: got=%q readErr=%v", got, readErr)
				}
			}
			assertNoPolicyTemps(t, dir)
		})
	}
}

// policyWriterFaultFS injects one stage failure while delegating all other
// operations to real os calls. It is deliberately narrow to the policy writer.
type policyWriterFaultFS struct{ fail string }

func (f *policyWriterFaultFS) CreateTemp(dir, pattern string) (policyWriterFile, error) {
	if f.fail == "open" {
		return nil, errors.New("injected open failure")
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &policyWriterFaultFile{File: file, fail: f.fail}, nil
}
func (f *policyWriterFaultFS) Link(oldpath, newpath string) error {
	if f.fail == "link" {
		return errors.New("injected link failure")
	}
	return os.Link(oldpath, newpath)
}
func (f *policyWriterFaultFS) Rename(oldpath, newpath string) error {
	if f.fail == "rename" {
		return errors.New("injected rename failure")
	}
	return os.Rename(oldpath, newpath)
}
func (f *policyWriterFaultFS) Remove(path string) error {
	if f.fail == "unlink" {
		return errors.New("injected unlink failure")
	}
	return os.Remove(path)
}
func (f *policyWriterFaultFS) SyncDir(dir string) error {
	if f.fail == "dir-fsync" {
		return errors.New("injected directory fsync failure")
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type policyWriterFaultFile struct {
	*os.File
	fail string
}

func (f *policyWriterFaultFile) Write(p []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("injected write failure")
	}
	return f.File.Write(p)
}
func (f *policyWriterFaultFile) Chmod(mode os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("injected chmod failure")
	}
	return f.File.Chmod(mode)
}
func (f *policyWriterFaultFile) Sync() error {
	if f.fail == "file-fsync" {
		return errors.New("injected file fsync failure")
	}
	return f.File.Sync()
}
func (f *policyWriterFaultFile) Close() error {
	err := f.File.Close()
	if f.fail == "close" {
		return errors.New("injected close failure")
	}
	return err
}

func assertNoPolicyTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".desired.yaml.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("policy temp files remain: %v", matches)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
