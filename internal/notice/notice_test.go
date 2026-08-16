package notice

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var publishedAt = time.Date(2026, 8, 16, 2, 0, 5, 0, time.UTC)

// TestRenderDeterministicDocument covers the primary render: a global target
// with chains (suffix-stripped to registry keys), a definition target with
// file/facet derivation, changed fields, and disabled models — asserting the
// exact byte-stable document.
func TestRenderDeterministicDocument(t *testing.T) {
	in := Input{
		Revision:       43,
		PublishedAt:    publishedAt,
		RoutingEnabled: true,
		Targets: []Target{
			{
				ID:   "global",
				Kind: "global",
				Chains: []Chain{
					{Name: "full", Models: []string{"codex/gpt-5.6-luna", "codex/gpt-5.6-sol(medium)"}},
					{Name: "mini", Models: []string{"minime/gemma-3-27b"}},
				},
				ChangedFields: [][]string{
					{"models", "codex/gpt-5.6-sol", "enabled"},
					{"defaults", "full"},
				},
			},
			{
				ID:   "work-api",
				Kind: "definition",
				File: "subagents/work-api.md",
				Chain: []string{
					"zai/glm-4.6",
					"codex/gpt-5.6-luna",
				},
				ChangedFields: [][]string{
					{"polytoken", "model"},
				},
			},
		},
		KnownModels: map[string]bool{
			"codex/gpt-5.6-luna":   true,
			"codex/gpt-5.6-sol":    true,
			"minime/gemma-3-27b":   true,
			"zai/glm-4.6":          true,
			"zai/glm-5.2":          true,
			"anthropic/sonnet-4-6": true,
		},
		DisabledModels: []string{"zai/glm-5.2", "codex/gpt-5.6-sol"},
	}

	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	want := `{
  "schema": 1,
  "revision": 43,
  "published_at": "2026-08-16T02:00:05Z",
  "routing_enabled": true,
  "targets": [
    {
      "id": "global",
      "kind": "global",
      "chains": [
        {"name": "full", "models": ["codex/gpt-5.6-luna", "codex/gpt-5.6-sol"]},
        {"name": "mini", "models": ["minime/gemma-3-27b"]}
      ],
      "changed_fields": [
        ["models", "codex/gpt-5.6-sol", "enabled"],
        ["defaults", "full"]
      ]
    },
    {
      "id": "work-api",
      "kind": "definition",
      "file": "subagents/work-api.md",
      "facet": "work-api",
      "chain": ["zai/glm-4.6", "codex/gpt-5.6-luna"],
      "changed_fields": [["polytoken", "model"]]
    }
  ],
  "disabled_models": ["codex/gpt-5.6-sol", "zai/glm-5.2"]
}`
	if normJSON(t, got) != normJSON(t, []byte(want)) {
		t.Fatalf("Render document mismatch:\ngot:  %s\nwant: %s", got, want)
	}
}

// TestRenderUnresolvableModelEmitsNull: a chain entry whose base model is not
// a known managed model renders as JSON null, both in chains and disabled
// lists, without failing the render.
func TestRenderUnresolvableModelEmitsNull(t *testing.T) {
	in := Input{
		Revision:       7,
		PublishedAt:    publishedAt,
		RoutingEnabled: false,
		Targets: []Target{
			{
				ID:    "global",
				Kind:  "global",
				Chains: []Chain{{Name: "full", Models: []string{"unknown/model-x", "codex/ok"}}},
			},
		},
		KnownModels:    map[string]bool{"codex/ok": true},
		DisabledModels: []string{"unknown/model-x"},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc struct {
		Targets []struct {
			Chains []struct {
				Models []*string `json:"models"`
			} `json:"chains"`
		} `json:"targets"`
		DisabledModels []*string `json:"disabled_models"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Targets) != 1 || len(doc.Targets[0].Chains) != 1 {
		t.Fatalf("unexpected target/chains shape: %s", got)
	}
	models := doc.Targets[0].Chains[0].Models
	if len(models) != 2 || models[0] != nil {
		t.Fatalf("expected first chain entry null, got %v", models)
	}
	if models[1] == nil || *models[1] != "codex/ok" {
		t.Fatalf("expected second entry codex/ok, got %v", models)
	}
	if len(doc.DisabledModels) != 1 || doc.DisabledModels[0] != nil {
		t.Fatalf("expected disabled entry null, got %v", doc.DisabledModels)
	}
}

// TestRenderOrderingAndOmission: global target first then definitions by ID,
// disabled models deduped and sorted, empty chains and empty-chain definition
// targets omitted.
func TestRenderOrderingAndOmission(t *testing.T) {
	in := Input{
		Revision:       9,
		PublishedAt:    publishedAt,
		RoutingEnabled: true,
		Targets: []Target{
			{ID: "zeta-def", Kind: "definition", File: "facets/zeta-def.md", Chain: []string{"codex/ok"}},
			{ID: "alpha-def", Kind: "definition", File: "subagents/alpha-def.md", Chain: []string{"codex/ok"}},
			{
				ID:    "global",
				Kind:  "global",
				Chains: []Chain{
					{Name: "full", Models: []string{"codex/ok"}},
					{Name: "mini", Models: nil},
				},
			},
			{ID: "empty-def", Kind: "definition", File: "subagents/empty-def.md"},
		},
		KnownModels:    map[string]bool{"codex/ok": true, "codex/a": true, "codex/b": true},
		DisabledModels: []string{"codex/b", "codex/a", "codex/b"},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc struct {
		Targets []struct {
			ID     string `json:"id"`
			Chains []struct {
				Name string `json:"name"`
			} `json:"chains"`
		} `json:"targets"`
		DisabledModels []string `json:"disabled_models"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var ids []string
	for _, t2 := range doc.Targets {
		ids = append(ids, t2.ID)
	}
	wantIDs := []string{"global", "alpha-def", "zeta-def"}
	if len(ids) != len(wantIDs) {
		t.Fatalf("targets = %v, want %v (empty-def omitted)", ids, wantIDs)
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Fatalf("target order = %v, want %v", ids, wantIDs)
		}
	}
	if len(doc.Targets[0].Chains) != 1 || doc.Targets[0].Chains[0].Name != "full" {
		t.Fatalf("empty mini chain must be omitted, got %+v", doc.Targets[0].Chains)
	}
	if len(doc.DisabledModels) != 2 || doc.DisabledModels[0] != "codex/a" || doc.DisabledModels[1] != "codex/b" {
		t.Fatalf("disabled_models = %v, want deduped sorted [codex/a codex/b]", doc.DisabledModels)
	}
}

// TestRenderSurfaceIsExactlyTheSchema: the document exposes exactly the
// specified top-level keys and carries nothing beyond managed-field facts —
// no environment, credentials, or arbitrary values can appear.
func TestRenderSurfaceIsExactlyTheSchema(t *testing.T) {
	got, err := Render(Input{
		Revision:    1,
		PublishedAt: publishedAt,
		Targets:     []Target{{ID: "global", Kind: "global", Chains: []Chain{{Name: "full", Models: []string{"codex/ok"}}}}},
		KnownModels: map[string]bool{"codex/ok": true},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{"schema", "revision", "published_at", "routing_enabled", "targets", "disabled_models"}
	if len(doc) != len(wantKeys) {
		t.Fatalf("top-level keys = %d (%v), want exactly %v", len(doc), doc, wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Fatalf("missing top-level key %q in %s", k, got)
		}
	}
	// Canary: values that must never appear anywhere in the document.
	for _, canary := range []string{"sk-ant-", "Bearer ", "api_key", "password"} {
		if strings.Contains(string(got), canary) {
			t.Fatalf("document contains canary %q", canary)
		}
	}
}

func normJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("normJSON: %v (%s)", err, b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normJSON marshal: %v", err)
	}
	return string(out)
}
