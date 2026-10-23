package policy

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/state"
	"pgregory.net/rapid"
)

// These properties exercise the in-memory Import proposal contract. Import uses
// a SourceReader and never writes desired.yaml; publication is a separate Writer.
func TestImportPropertyRoundTripsSyntheticSources(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		model := fmt.Sprintf("codex/model-%d", rapid.IntRange(0, 999).Draw(t, "model"))
		suffix := rapid.SampledFrom([]string{"", "(low)", "(medium)", "(high)"}).Draw(t, "suffix")
		projectCount := rapid.IntRange(1, 3).Draw(t, "project-count")
		global := SourceSet{ID: "global", Root: "/synthetic/global", Global: true, Config: SourceConfig{Providers: []SourceMapping{{ID: "codex", Models: map[string]ModelBaseline{model: {Enabled: true}}}}, Full: Chain{model + suffix}, Mini: Chain{model + "(mini)"}}, Definitions: []SourceDefinition{{Path: "agents/global.md", Model: model + suffix, FallbackModels: []string{model + "(fallback)"}}}}
		projects := make([]SourceSet, projectCount)
		wantProjects := make([]Target, projectCount)
		for i := range projects {
			id := fmt.Sprintf("project-%d-%d", i, rapid.IntRange(0, 9999).Draw(t, "project-id"))
			path := fmt.Sprintf("agents/project-%d-%d.md", i, rapid.IntRange(0, 9999).Draw(t, "project-path"))
			chain := Chain{model + suffix, model + "(project-fallback)"}
			projects[i] = SourceSet{ID: id, Root: "/synthetic/" + id, Config: SourceConfig{Nano: Chain{model + "(nano)"}}, Definitions: []SourceDefinition{{Path: path, FallbackModels: append(Chain(nil), chain...)}}}
			wantProjects[i] = Target{ID: id, Root: "/synthetic/" + id, Nano: Chain{model + "(nano)"}, Definitions: []Definition{{Path: path, Chain: chain}}}
		}
		d, report, err := Import(context.Background(), staticReader{global: global, projects: projects}, normalState(), false)
		if err != nil {
			t.Fatalf("Import valid synthetic sources: %v", err)
		}
		if len(report.Drift) != 0 || len(report.Uncovered) != 0 {
			t.Fatalf("valid sources reported issues: %+v", report)
		}
		if d.Global.Full[0] != model+suffix || d.Global.Definitions[0].Chain[0] != model+suffix {
			t.Fatalf("reasoning suffix changed: %+v", d.Global)
		}
		if !reflect.DeepEqual(d.Projects, wantProjects) {
			t.Fatalf("projects got %#v want %#v", d.Projects, wantProjects)
		}
		encoded, err := marshalDesired(d)
		if err != nil {
			t.Fatalf("marshal proposal: %v", err)
		}
		loaded, err := loadBytes(encoded)
		if err != nil {
			t.Fatalf("load proposal: %v\n%s", err, encoded)
		}
		if q := loaded.Providers["codex"].Quota; q == nil || q.Adapter != "codex" || q.FreshnessTTL != DefaultQuotaFreshness {
			t.Fatalf("implicit quota not resolved: %+v", q)
		}
		mapping := loaded.Providers["codex"]
		mapping.Quota = nil
		loaded.Providers["codex"] = mapping
		if !reflect.DeepEqual(loaded, d) {
			t.Fatalf("marshal/load changed proposal semantics")
		}
	})
}
func TestImportPropertyGuardRejectionReturnsNoProposal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		model := fmt.Sprintf("codex/model-%d", rapid.IntRange(0, 999).Draw(t, "model"))
		suffix := rapid.SampledFrom([]string{"", "(low)", "(medium)", "(high)"}).Draw(t, "suffix")
		global := SourceSet{ID: "global", Root: "/synthetic/global", Global: true, Config: SourceConfig{Providers: []SourceMapping{{ID: "codex", Models: map[string]ModelBaseline{model: {Enabled: true}}}}}, Definitions: []SourceDefinition{{Path: "agent.md", Model: model + suffix}}}
		cases := []struct {
			name   string
			reader staticReader
			state  state.State
		}{{name: "degraded", reader: staticReader{global: global}, state: fixtureState(true)}, {name: "off-graph", reader: staticReader{global: SourceSet{ID: global.ID, Root: global.Root, Global: global.Global, Config: global.Config, Definitions: append(append([]SourceDefinition(nil), global.Definitions...), SourceDefinition{Path: "drift.md", Model: "unmapped/model"})}}, state: normalState()}}
		for _, tc := range cases {
			got, report, err := Import(context.Background(), tc.reader, tc.state, false)
			if err == nil {
				t.Fatalf("%s Import unexpectedly succeeded", tc.name)
			}
			if !reflect.DeepEqual(got, Desired{}) {
				t.Fatalf("%s rejection returned proposal: %#v", tc.name, got)
			}
			if tc.name == "off-graph" && len(report.Drift) == 0 {
				t.Fatalf("off-graph rejection omitted drift: %+v", report)
			}
		}
	})
}
