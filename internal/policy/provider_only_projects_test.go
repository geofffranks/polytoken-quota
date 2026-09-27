package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providerOnlyProjectsFixture is a valid provider-only policy with two
// registered project roots carrying exactly id and root.
const providerOnlyProjectsFixture = `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
projects:
  - id: proj-a
    root: /home/user/proj-a
  - id: proj-b
    root: /home/user/proj-b
`

// TestProviderOnlyAcceptsRegisteredProjectRoots proves provider-only policy
// may register project roots for read-only global+project safety assessment:
// each entry carries exactly id and root, loads as an id/root-only target, and
// preserves document order.
func TestProviderOnlyAcceptsRegisteredProjectRoots(t *testing.T) {
	d, err := Load(writeTemp(t, providerOnlyProjectsFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.ProviderOnly() {
		t.Fatalf("mode=%q want provider-only", d.Mode)
	}
	if len(d.Projects) != 2 {
		t.Fatalf("projects=%d want 2 registered roots", len(d.Projects))
	}
	want := []Target{
		{ID: "proj-a", Root: "/home/user/proj-a"},
		{ID: "proj-b", Root: "/home/user/proj-b"},
	}
	for i, w := range want {
		got := d.Projects[i]
		if got.ID != w.ID || got.Root != w.Root || got.Global {
			t.Fatalf("project %d = %+v, want %+v", i, got, w)
		}
		if len(got.Definitions) != 0 || len(got.Full) != 0 || len(got.Mini) != 0 ||
			len(got.Nano) != 0 || len(got.Classifier) != 0 {
			t.Fatalf("project %q adopted legacy fields: %+v", got.ID, got)
		}
	}
}

// TestProviderOnlyRejectsUnsupportedProjectFields proves a provider-only
// project entry accepts exactly id and root: unsupported keys, empty ids or
// roots, and duplicate ids are load errors — never silently stripped or
// tolerated.
func TestProviderOnlyRejectsUnsupportedProjectFields(t *testing.T) {
	base := `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
projects:
%s
`
	cases := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{
			name: "unsupported provider key",
			entry: `  - id: proj
    root: /home/user/proj
    provider: codex
`,
			wantErr: `field "provider" is not supported in provider-only mode`,
		},
		{
			name: "unsupported model key",
			entry: `  - id: proj
    root: /home/user/proj
    model: codex/gpt-5.6-sol
`,
			wantErr: `field "model" is not supported in provider-only mode`,
		},
		{
			name: "mini chain",
			entry: `  - id: proj
    root: /home/user/proj
    mini: [codex/gpt-5.6-sol]
`,
			wantErr: `field "mini" is not supported in provider-only mode`,
		},
		{
			name: "empty id",
			entry: `  - id: ""
    root: /home/user/proj
`,
			wantErr: `require a non-empty id`,
		},
		{
			name: "empty root",
			entry: `  - id: proj
    root: ""
`,
			wantErr: `require a non-empty root`,
		},
		{
			name: "duplicate id",
			entry: `  - id: proj
    root: /home/user/proj-a
  - id: proj
    root: /home/user/proj-b
`,
			wantErr: `project "proj" is registered more than once`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, strings.ReplaceAll(base, "%s", tc.entry)))
			if err == nil {
				t.Fatalf("Load accepted an unsupported provider-only project entry")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestProviderOnlyMarshalRoundTripWithProjects proves a provider-only policy
// with registered project roots serializes back to a document Load accepts as
// the same policy: id/root-only projects, no legacy fields emitted.
func TestProviderOnlyMarshalRoundTripWithProjects(t *testing.T) {
	d, err := Load(writeTemp(t, providerOnlyProjectsFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := marshalDesired(d)
	if err != nil {
		t.Fatalf("marshalDesired: %v", err)
	}
	red, err := Load(writeTemp(t, string(out)))
	if err != nil {
		t.Fatalf("Load(marshaled): %v\nmarshaled:\n%s", err, out)
	}
	if len(red.Projects) != 2 {
		t.Fatalf("round-tripped projects=%d want 2", len(red.Projects))
	}
	for i, p := range red.Projects {
		if p.ID != d.Projects[i].ID || p.Root != d.Projects[i].Root || p.Global {
			t.Fatalf("round-tripped project %d = %+v, want %+v", i, p, d.Projects[i])
		}
		if len(p.Definitions) != 0 || len(p.Full) != 0 || len(p.Mini) != 0 ||
			len(p.Nano) != 0 || len(p.Classifier) != 0 {
			t.Fatalf("round-tripped project %q adopted legacy fields: %+v", p.ID, p)
		}
	}
	if strings.Contains(string(out), "full:") || strings.Contains(string(out), "definitions:") {
		t.Fatalf("marshaled document emitted legacy fields:\n%s", out)
	}
}

// TestProviderOnlyInitPreservesRegisteredProjectRoots proves the provider-only
// init proposal preserves explicitly registered project roots as id/root-only
// targets — and nothing else: no chains, no definitions, and no adoption of
// roots the reader does not report as registered.
func TestProviderOnlyInitPreservesRegisteredProjectRoots(t *testing.T) {
	global := SourceSet{
		ID: "global", Root: "/home/user/.config/polytoken", Global: true,
		Config: SourceConfig{Providers: []SourceMapping{{ID: "codex"}}},
	}
	registered := []SourceSet{
		{ID: "proj-b", Root: "/home/user/proj-b"},
		{ID: "proj-a", Root: "/home/user/proj-a", Definitions: []SourceDefinition{{Path: "facets/reader.md", Model: "codex/gpt-5.6-sol"}}},
	}
	d, err := InitProviderOnly(context.Background(), staticReader{global: global, projects: registered})
	if err != nil {
		t.Fatalf("InitProviderOnly: %v", err)
	}
	if !d.ProviderOnly() {
		t.Fatalf("mode=%q want provider-only", d.Mode)
	}
	if len(d.Projects) != 2 {
		t.Fatalf("projects=%+v want exactly the two registered roots", d.Projects)
	}
	if d.Projects[0].ID != "proj-a" || d.Projects[1].ID != "proj-b" {
		t.Fatalf("projects not sorted by id: %+v", d.Projects)
	}
	for _, p := range d.Projects {
		if p.Global || len(p.Definitions) != 0 || len(p.Full) != 0 || len(p.Mini) != 0 ||
			len(p.Nano) != 0 || len(p.Classifier) != 0 {
			t.Fatalf("registered project %q carried legacy fields: %+v", p.ID, p)
		}
	}
}

// TestProviderOnlyInitRejectsUnusableRegisteredProject proves the init
// proposal stops on a registered project it cannot carry: an empty id is an
// error, never a silently dropped or fabricated root.
func TestProviderOnlyInitRejectsUnusableRegisteredProject(t *testing.T) {
	global := SourceSet{
		ID: "global", Root: "/home/user/.config/polytoken", Global: true,
		Config: SourceConfig{Providers: []SourceMapping{{ID: "codex"}}},
	}
	_, err := InitProviderOnly(context.Background(), staticReader{
		global:   global,
		projects: []SourceSet{{ID: "", Root: "/home/user/proj"}},
	})
	if err == nil || !strings.Contains(err.Error(), "without an id") {
		t.Fatalf("err=%v want registered-project-without-id rejection", err)
	}
}

// TestFilesystemSourceReaderProjectsWithoutPolicyReadsNone proves registered
// projects come only from the registered policy: with no policy path there is
// nothing to read and no discovery runs.
func TestFilesystemSourceReaderProjectsWithoutPolicyReadsNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("providers:\n  codex: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := FilesystemSourceReader{GlobalDir: dir}
	projects, err := reader.Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects=%+v want none without a registered policy", projects)
	}
}
