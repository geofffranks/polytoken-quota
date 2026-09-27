package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providerOnlyFixture is a minimal valid provider-only policy: two enrolled
// Polytoken provider IDs, explicit quota adapter configuration on one of them,
// and a global target root.
const providerOnlyFixture = `version: 1
mode: provider-only
providers:
  codex:
    quota:
      adapter: codex
  team-llm: {}
global:
  root: /home/user/.config/polytoken
`

// TestProviderOnlyRejectsLegacyChainsAndModels proves the opt-in provider-only
// mode rejects every conflicting legacy target/model field: enumerated models
// under a provider, desired chains on the global target, definition chains,
// registered projects, and the routing/selection sections. Each rejection must
// be a load error — never a silently tolerated or stripped field.
func TestProviderOnlyRejectsLegacyChainsAndModels(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "models under provider",
			doc: `version: 1
mode: provider-only
providers:
  codex:
    models: [codex/gpt-5.6-sol]
global:
  root: /home/user/.config/polytoken
`,
			wantErr: `must not enumerate models`,
		},
		{
			name: "full chain on global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
  full: [codex/gpt-5.6-sol]
`,
			wantErr: `chain "full" is a legacy field`,
		},
		{
			name: "mini chain on global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
  mini: [codex/gpt-5.6-sol]
`,
			wantErr: `chain "mini" is a legacy field`,
		},
		{
			name: "nano chain on global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
  nano: [codex/gpt-5.6-sol]
`,
			wantErr: `chain "nano" is a legacy field`,
		},
		{
			name: "classifier chain on global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
  classifier: [codex/gpt-5.6-sol]
`,
			wantErr: `chain "classifier" is a legacy field`,
		},
		{
			name: "definitions on global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
  definitions:
    - path: facets/reader.md
      chain: [codex/gpt-5.6-sol]
`,
			wantErr: `definitions are legacy fields`,
		},
		{
			name: "registered projects",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
projects:
  - id: proj
    root: /home/user/proj
`,
			wantErr: `must not register projects`,
		},
		{
			name: "routing section",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
routing:
  enabled: true
`,
			wantErr: `must not set routing`,
		},
		{
			name: "selection section",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
selection:
  jev:
    enabled: true
`,
			wantErr: `must not set selection`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.doc))
			if err == nil {
				t.Fatalf("Load accepted a provider-only policy with legacy fields")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestProviderOnlyLoad proves the minimal provider-only document loads into a
// Desired whose providers carry enrolled IDs (optionally with explicit quota
// adapter configuration) and whose global target carries only identity and root.
func TestProviderOnlyLoad(t *testing.T) {
	d, err := Load(writeTemp(t, providerOnlyFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.ProviderOnly() || d.Mode != ModeProviderOnly {
		t.Fatalf("mode=%q want provider-only", d.Mode)
	}
	if len(d.Providers) != 2 {
		t.Fatalf("providers=%d want 2 enrolled IDs", len(d.Providers))
	}
	if m := d.Providers["codex"]; m.Quota == nil || m.Quota.Adapter != "codex" || len(m.Models) != 0 {
		t.Fatalf("codex mapping=%+v want quota adapter codex and no models", m)
	}
	if m := d.Providers["team-llm"]; m.Quota != nil || len(m.Models) != 0 {
		t.Fatalf("team-llm mapping=%+v want no quota and no models", m)
	}
	if d.Global.Root != "/home/user/.config/polytoken" || !d.Global.Global || len(d.Global.Definitions) != 0 {
		t.Fatalf("global target=%+v", d.Global)
	}
	if len(d.Projects) != 0 {
		t.Fatalf("projects=%d want none in provider-only mode", len(d.Projects))
	}
	// The resolved defaults still apply for operational settings.
	if d.Operational.BackupCount != 1 {
		t.Fatalf("backup_count=%d want default 1", d.Operational.BackupCount)
	}
}

// TestProviderOnlyQuotaAdapterRules pins the explicit quota adapter grammar:
// the adapter is required whenever a quota section is present, must be a known
// adapter, and the anthropic budget/mode rules carry over unchanged.
func TestProviderOnlyQuotaAdapterRules(t *testing.T) {
	base := `version: 1
mode: provider-only
providers:
  team-llm:
    quota:
%s
global:
  root: /home/user/.config/polytoken
`
	cases := []struct {
		name    string
		quota   string
		wantErr string // empty means load must succeed
	}{
		{name: "missing adapter", quota: "      freshness_ttl: 15m", wantErr: "requires an explicit adapter"},
		{name: "empty adapter", quota: "      adapter: \"\"", wantErr: "requires an explicit adapter"},
		{name: "unknown adapter", quota: "      adapter: not-an-adapter", wantErr: "unknown quota adapter"},
		{name: "codex adapter", quota: "      adapter: codex"},
		{
			name: "anthropic api requires budget",
			quota: `      adapter: anthropic
      monthly_budget_usd: 120
`,
		},
		{
			name: "anthropic api without budget rejected",
			quota: `      adapter: anthropic
`,
			wantErr: "requires monthly_budget_usd",
		},
		{
			name: "anthropic subscription without budget",
			quota: `      adapter: anthropic
      mode: subscription
`,
		},
		{
			name: "anthropic subscription with budget rejected",
			quota: `      adapter: anthropic
      mode: subscription
      monthly_budget_usd: 10
`,
			wantErr: "does not use monthly_budget_usd",
		},
		{
			name: "anthropic-subscription adapter with budget rejected",
			quota: `      adapter: anthropic-subscription
      monthly_budget_usd: 10
`,
			wantErr: "does not use monthly_budget_usd",
		},
		{
			name: "anthropic-subscription adapter with mode rejected",
			quota: `      adapter: anthropic-subscription
      mode: subscription
`,
			wantErr: "always subscription",
		},
		{
			name: "mode on non-anthropic rejected",
			quota: `      adapter: codex
      mode: subscription
`,
			wantErr: "only valid for the anthropic provider",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load(writeTemp(t, fmt.Sprintf(base, tc.quota)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load accepted invalid provider-only quota")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			_ = d
		})
	}
}

// TestProviderOnlyRequiresProvidersGlobalRootAndKnownMode pins the structural
// requirements: at least one enrolled provider, a global target with a root,
// and no mode value other than legacy/provider-only.
func TestProviderOnlyRequiresProvidersGlobalRootAndKnownMode(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "no providers",
			doc: `version: 1
mode: provider-only
global:
  root: /home/user/.config/polytoken
`,
			wantErr: "must enroll at least one provider",
		},
		{
			name: "no global target",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
`,
			wantErr: "requires a global target",
		},
		{
			name: "global without root",
			doc: `version: 1
mode: provider-only
providers:
  codex: {}
global:
  id: global
`,
			wantErr: "requires a global target root",
		},
		{
			name: "unknown mode",
			doc: `version: 1
mode: provideronely
providers:
  codex: {}
global:
  root: /home/user/.config/polytoken
`,
			wantErr: "unknown mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.doc))
			if err == nil {
				t.Fatalf("Load accepted invalid provider-only policy")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLegacyPolicyStillLoads proves both legacy spellings — no mode key and
// explicit mode: legacy — keep loading with the full legacy behavior: model
// enumeration, chains, projects, routing and selection sections.
func TestLegacyPolicyStillLoads(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "absent mode", mode: ""},
		{name: "explicit legacy mode", mode: "mode: legacy\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `version: 1
` + tc.mode + `providers:
  codex:
    models: [codex/gpt-5.6-sol]
global:
  full: [codex/gpt-5.6-sol]
  definitions:
    - path: facets/reader.md
      chain: [codex/gpt-5.6-sol]
projects:
  - id: proj
    root: /home/user/proj
    mini: [codex/gpt-5.6-sol]
`
			d, err := Load(writeTemp(t, doc))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if d.ProviderOnly() || d.Mode != ModeLegacy {
				t.Fatalf("mode=%q want legacy", d.Mode)
			}
			if len(d.Providers["codex"].Models) != 1 || len(d.Global.Full) != 1 || len(d.Global.Definitions) != 1 || len(d.Projects) != 1 {
				t.Fatalf("legacy structure lost: %+v", d)
			}
			if d.Routing.Enabled != true {
				t.Fatalf("routing default changed: %+v", d.Routing)
			}
		})
	}
}

// TestProviderOnlyMarshalRoundTrip proves a provider-only Desired serializes
// back to a document Load accepts as the same policy: mode, enrolled IDs,
// explicit quota adapter configuration, and the global root — with no model
// enumeration emitted.
func TestProviderOnlyMarshalRoundTrip(t *testing.T) {
	d, err := Load(writeTemp(t, providerOnlyFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := marshalDesired(d)
	if err != nil {
		t.Fatalf("marshalDesired: %v", err)
	}
	if strings.Contains(string(data), "models:") {
		t.Fatalf("provider-only serialization emitted models: %s", data)
	}
	redesired, err := loadBytes(data)
	if err != nil {
		t.Fatalf("re-read marshaled policy: %v\n%s", err, data)
	}
	if !redesired.ProviderOnly() || len(redesired.Providers) != 2 || redesired.Global.Root != d.Global.Root {
		t.Fatalf("round trip mismatch: %+v", redesired)
	}
	if q := redesired.Providers["codex"].Quota; q == nil || q.Adapter != "codex" {
		t.Fatalf("quota adapter lost in round trip: %+v", q)
	}
}

// TestProviderOnlyInitDoesNotAdoptGroupsModelsOrDefinitions proves the
// provider-only init proposal enrolls only the live provider IDs: a version-4
// style global config carrying a modelgroups section, enumerated models, and
// model-bearing definition files contributes no groups, models, chains, or
// definitions to the proposal.
func TestProviderOnlyInitDoesNotAdoptGroupsModelsOrDefinitions(t *testing.T) {
	global := staticReader{
		global: SourceSet{
			ID:     "global",
			Root:   "/home/user/.config/polytoken",
			Global: true,
			Config: SourceConfig{
				Providers: []SourceMapping{
					{ID: "codex", Models: map[string]ModelBaseline{"codex/gpt-5.6-sol": {Enabled: true}}},
					{ID: "zai", Models: map[string]ModelBaseline{"zai/glm": {Enabled: false}}},
				},
				Full: Chain{"codex/gpt-5.6-sol"},
			},
			Definitions: []SourceDefinition{{Path: "facets/reader.md", Model: "codex/gpt-5.6-sol"}},
		},
	}
	d, err := InitProviderOnly(context.Background(), global)
	if err != nil {
		t.Fatalf("InitProviderOnly: %v", err)
	}
	if !d.ProviderOnly() {
		t.Fatalf("mode=%q want provider-only", d.Mode)
	}
	if len(d.Providers) != 2 {
		t.Fatalf("providers=%v want exactly the enrolled IDs codex and zai", d.Providers)
	}
	for id, m := range d.Providers {
		if len(m.Models) != 0 {
			t.Fatalf("provider %q adopted models: %+v", id, m.Models)
		}
		if _, isLegacyAdapter := map[string]bool{"codex": true, "zai": true, "anthropic": true, "neuralwatt": true}[string(id)]; !isLegacyAdapter {
			continue // provider IDs are not adapter names in this mode
		}
	}
	if d.Global.Full != nil || d.Global.Mini != nil || d.Global.Nano != nil || d.Global.Classifier != nil {
		t.Fatalf("global target adopted chains: %+v", d.Global)
	}
	if len(d.Global.Definitions) != 0 {
		t.Fatalf("global target adopted definitions: %+v", d.Global.Definitions)
	}
	if len(d.Projects) != 0 {
		t.Fatalf("projects adopted: %+v", d.Projects)
	}
	if d.Global.Root != "/home/user/.config/polytoken" {
		t.Fatalf("global root=%q", d.Global.Root)
	}
}

// TestProviderOnlyInitRejectsEmptyGlobalConfig proves the provider-only init
// proposal refuses to fabricate a policy with nothing enrolled: an empty
// global provider list is an error, never a silently empty policy.
func TestProviderOnlyInitRejectsEmptyGlobalConfig(t *testing.T) {
	_, err := InitProviderOnly(context.Background(), staticReader{
		global: SourceSet{ID: "global", Root: "/home/user/.config/polytoken", Global: true},
	})
	if err == nil || !strings.Contains(err.Error(), "no providers to enroll") {
		t.Fatalf("err=%v want no-providers rejection", err)
	}
}

// TestProviderOnlyInitReadsRealV4ConfigLayout proves the production source
// reader + provider-only init combination ignores the modelgroups key of a
// version-4 style config.yaml on disk: only the provider IDs under `providers:`
// are enrolled. This is the synthetic fixture form of the group non-adoption
// contract (no live binary or config involved).
func TestProviderOnlyInitReadsRealV4ConfigLayout(t *testing.T) {
	root := t.TempDir()
	config := `providers:
  codex:
    base_url: http://127.0.0.1:9
  team-llm:
    base_url: http://127.0.0.1:9
models:
  codex/gpt-5.6-sol:
    enabled: true
defaults:
  full: codex/gpt-5.6-sol
modelgroups:
  mg:failover:
    - codex/gpt-5.6-sol
    - team-llm/glm
`
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	reader := FilesystemSourceReader{GlobalDir: root}
	d, err := InitProviderOnly(context.Background(), reader)
	if err != nil {
		t.Fatalf("InitProviderOnly: %v", err)
	}
	got := make([]string, 0, len(d.Providers))
	for id := range d.Providers {
		got = append(got, string(id))
	}
	if len(got) != 2 || (got[0] != "codex" && got[1] != "codex") || (got[0] != "team-llm" && got[1] != "team-llm") {
		t.Fatalf("enrolled=%v want codex and team-llm only", got)
	}
	for id, m := range d.Providers {
		if len(m.Models) != 0 || m.Quota != nil {
			t.Fatalf("provider %q adopted quota/models: %+v", id, m)
		}
	}
	if d.Global.Root != root {
		t.Fatalf("global root=%q want %q", d.Global.Root, root)
	}
}
