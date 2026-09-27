package notice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderProviderNoticeContainsOnlyProviderStatus(t *testing.T) {
	got, err := RenderProvider(12, publishedAt, []ProviderState{{ID: "zeta", Enabled: false}, {ID: "alpha", Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["revision"] != float64(12) || doc["provider_only"] != true {
		t.Fatalf("provider notice metadata = %v", doc)
	}
	providers, ok := doc["providers"].([]any)
	if !ok || len(providers) != 2 || providers[0].(map[string]any)["id"] != "alpha" || providers[1].(map[string]any)["id"] != "zeta" {
		t.Fatalf("providers not sorted: %s", got)
	}
	for _, forbidden := range []string{"targets", "disabled_models", "chain", "fallback", "model"} {
		if _, exists := doc[forbidden]; exists || strings.Contains(string(got), `"`+forbidden+`"`) {
			t.Fatalf("provider notice contains route/model claim %q: %s", forbidden, got)
		}
	}
}
