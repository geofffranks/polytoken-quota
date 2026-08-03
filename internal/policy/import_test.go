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

	"github.com/geofffranks/codexbar-hooks/internal/state"
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
// `sync --from-polytoken`.
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
	err := newWriter(path).CreateAtomic(context.Background(), Desired{})
	if !errors.Is(err, ErrDesiredExists) {
		t.Fatalf("err=%v want ErrDesiredExists", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing desired.yaml changed")
	}
	if !strings.Contains(err.Error(), "use sync --from-polytoken") {
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
	if err := NewWriter(path).CreateAtomic(context.Background(), d); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%#o want 0600", info.Mode().Perm())
	}
	if err := NewWriter(path).CreateAtomic(context.Background(), Desired{}); !errors.Is(err, ErrDesiredExists) {
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
