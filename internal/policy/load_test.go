package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTemp writes content to a temp desired.yaml and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func loadedFixture(t *testing.T) Desired {
	t.Helper()
	d, err := Load("testdata/synthetic_desired.yaml")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return d
}

func sliceEq(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLoadSyntheticFixture is the Task 4 schema happy path: version, explicit
// single top-level provider mapping keyed by provider ID with exact concrete model
// enumeration, the
// global target with chains, a registered project, a suffixed definition chain
// that resolves to a managed base, and operational bounds.
func TestLoadSyntheticFixture(t *testing.T) {
	d, err := Load("testdata/synthetic_desired.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Version != 1 {
		t.Fatalf("version=%d", d.Version)
	}
	codex, ok := d.Providers["codex"]
	if !ok {
		t.Fatal("missing codex mapping")
	}
	for _, base := range []string{"codex/gpt-5.6-sol", "codex/gpt-5.6-luna"} {
		mb, ok := codex.Models[base]
		if !ok {
			t.Fatalf("missing model %q", base)
		}
		if !mb.Enabled || mb.HadEnabledKey {
			t.Fatalf("model %q baseline=%+v want enabled/no-key", base, mb)
		}
	}
	zai := d.Providers["zai"]
	if _, ok := zai.Models["zai/glm-5.2"]; !ok {
		t.Fatal("missing zai/glm-5.2")
	}
	if !d.Global.Global || d.Global.Root != "/home/user/.config/polytoken" {
		t.Fatalf("global=%+v", d.Global)
	}
	if !sliceEq(d.Global.Full, "codex/gpt-5.6-sol", "zai/glm-5.2") {
		t.Fatalf("global.full=%v", d.Global.Full)
	}
	if !sliceEq(d.Global.Classifier, "codex/gpt-5.6-sol") {
		t.Fatalf("global.classifier=%v", d.Global.Classifier)
	}
	if len(d.Global.Definitions) != 1 || d.Global.Definitions[0].Path != "agents/research.md" {
		t.Fatalf("global.definitions=%+v", d.Global.Definitions)
	}
	// The suffixed entry must have resolved to its managed base at load time;
	// the exact spelling is preserved on the chain.
	if !sliceEq(d.Global.Definitions[0].Chain, "codex/gpt-5.6-sol(medium)", "zai/glm-5.2") {
		t.Fatalf("definition chain=%v", d.Global.Definitions[0].Chain)
	}
	if len(d.Projects) != 1 || d.Projects[0].ID != "demo" || d.Projects[0].Global {
		t.Fatalf("projects=%+v", d.Projects)
	}
	if d.Operational.ValidationTimeout != 30*time.Second ||
		d.Operational.LockWait != 10*time.Second ||
		d.Operational.RecoveredRetention != 168*time.Hour ||
		d.Operational.BackupCount != 5 {
		t.Fatalf("operational=%+v", d.Operational)
	}
}

// TestParseModelRefGrammar pins the canonical model-reference grammar: bare
// names and exactly-one trailing "(suffix)" are valid; unbalanced, empty, or
// trailing-junk spellings are rejected. Policy loading and reconciliation both
// use this parser, so acceptance can never diverge between them.
func TestParseModelRefGrammar(t *testing.T) {
	valid := map[string][2]string{
		"codex/gpt":              {"codex/gpt", ""},
		"codex/gpt-5.6-sol(med)": {"codex/gpt-5.6-sol", "med"},
	}
	for entry, want := range valid {
		base, suffix, err := ParseModelRef(entry)
		if err != nil || base != want[0] || suffix != want[1] {
			t.Fatalf("ParseModelRef(%q) = %q,%q,%v want %q,%q", entry, base, suffix, err, want[0], want[1])
		}
	}
	invalid := []string{"", "model(", "model)", "model()", "model(foo)junk", "(foo)", "model(f(o)o)"}
	for _, entry := range invalid {
		if _, _, err := ParseModelRef(entry); err == nil {
			t.Fatalf("ParseModelRef(%q) accepted malformed spelling", entry)
		}
	}
}

// TestValidateChainRejectsMalformedSuffix proves a chain entry with a
// malformed reasoning suffix fails policy validation instead of being
// truncated at the first parenthesis and described as validated.
func TestValidateChainRejectsMalformedSuffix(t *testing.T) {
	owner := map[string]MappingID{"codex/gpt": "codex"}
	if err := validateChain(owner, Chain{"codex/gpt(high)junk"}); err == nil {
		t.Fatal("malformed suffix accepted by validateChain")
	}
	if err := validateChain(owner, Chain{"codex/gpt(high)"}); err != nil {
		t.Fatalf("valid suffix rejected: %v", err)
	}
}

// TestLoadRequiresConcreteUnambiguousModels covers the Task 4 blueprint rejection
// cases plus version, intra-mapping duplicate, and non-concrete (glob) names.
func TestLoadRequiresConcreteUnambiguousModels(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"empty enumeration", "version: 1\nproviders: {codex: {models: []}}", "models"},
		{"conflict", "version: 1\nproviders: {a: {models: [codex/m]}, b: {models: [codex/m]}}", "conflicting"},
		{"unresolved chain", "version: 1\nglobal: {full: [unknown/model]}", "unresolved"},
		{"duplicate within mapping", "version: 1\nproviders: {a: {models: [codex/m, codex/m]}}", "duplicate"},
		{"non-concrete glob", "version: 1\nproviders: {a: {models: [codex/*]}}", "non-concrete"},
		{"unsupported version", "version: 2\nproviders: {a: {models: [codex/m]}}", "version"},
		{"unresolved project chain", "version: 1\nproviders: {a: {models: [codex/m]}}\nprojects: [{id: p, full: [codex/missing]}]", "unresolved"},
		{"unresolved definition chain", "version: 1\nproviders: {a: {models: [codex/m]}}\nglobal: {definitions: [{path: a.md, chain: [codex/ghost]}]}", "unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
		})
	}
}

// TestLoadBaselineEnabledState proves the optional baseline enabled state and the
// HadEnabledKey origin tracking: a bare name defaults to enabled with no key,
// while an explicit enabled:false records HadEnabledKey true.
func TestLoadBaselineEnabledState(t *testing.T) {
	yaml := "version: 1\nproviders:\n  a:\n    models:\n      - codex/on\n      - codex/off: {enabled: false}\n"
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	on := d.Providers["a"].Models["codex/on"]
	if !on.Enabled || on.HadEnabledKey {
		t.Fatalf("on=%+v want enabled/no-key", on)
	}
	off := d.Providers["a"].Models["codex/off"]
	if off.Enabled || !off.HadEnabledKey {
		t.Fatalf("off=%+v want disabled/key", off)
	}
}

// TestResolveModel proves exact resolution and that similarity-based guessing is
// never performed.
func TestResolveModel(t *testing.T) {
	d := loadedFixture(t)
	if id, err := d.ResolveModel("codex/gpt-5.6-sol"); err != nil || id != "codex" {
		t.Fatalf("ResolveModel(gpt-5.6-sol)=%q err=%v", id, err)
	}
	if id, err := d.ResolveModel("zai/glm-5.2"); err != nil || id != "zai" {
		t.Fatalf("ResolveModel(glm-5.2)=%q err=%v", id, err)
	}
	// Similarity guessing must be rejected: a prefix of a real model is unknown.
	if _, err := d.ResolveModel("codex/gpt-5.6"); err == nil {
		t.Fatal("accepted similar model name (guessed)")
	}
	// Unknown base rejected.
	if _, err := d.ResolveModel("minime/gemma"); err == nil {
		t.Fatal("accepted unknown model")
	}
}

// TestLoadOperationalBounds proves operational durations must be positive and the
// backup count must be at least one, while an omitted section falls back to sane
// defaults.
func TestLoadOperationalBounds(t *testing.T) {
	t.Run("zero timeout rejected", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {validation_timeout: 0s}"
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Fatal("accepted zero timeout")
		}
	})
	t.Run("zero backup count rejected", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {backup_count: 0}"
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Fatal("accepted zero backup count")
		}
	})
	t.Run("backup count omitted in partial section defaults to 5", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {validation_timeout: 60s}"
		d, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if d.Operational.ValidationTimeout != 60*time.Second {
			t.Fatalf("validation_timeout=%v want 60s", d.Operational.ValidationTimeout)
		}
		if d.Operational.BackupCount != 5 {
			t.Fatalf("backup_count=%d want default 5", d.Operational.BackupCount)
		}
	})
	t.Run("backup count explicit value honored", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {backup_count: 3}"
		d, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if d.Operational.BackupCount != 3 {
			t.Fatalf("backup_count=%d want 3", d.Operational.BackupCount)
		}
	})
	t.Run("negative backup count rejected", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {backup_count: -1}"
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Fatal("accepted negative backup count")
		}
	})
	t.Run("bad duration rejected", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}\noperational: {lock_wait: not-a-duration}"
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Fatal("accepted bad duration")
		}
	})
	t.Run("defaults when omitted", func(t *testing.T) {
		yaml := "version: 1\nproviders: {a: {models: [codex/m]}}"
		d, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if d.Operational != defaultOperational {
			t.Fatalf("operational=%+v want default %+v", d.Operational, defaultOperational)
		}
	})
}

// TestLoadRoutingQuota proves the routing/quota sections parse correctly:
// routing enabled flag, per-provider quota config (adapter, freshness, balance
// group, weight, off-peak schedule).
func TestLoadPeakScheduleForSingaporeBusinessHours(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/gpt-5]
    quota:
      schedule:
        timezone: Asia/Singapore
        peak:
          - days: [mon, tue, wed, thu, fri]
            start: "14:00"
            end: "18:00"
global:
  id: global
  root: /tmp/polytoken
  full: [codex/gpt-5]
`
	got, err := loadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("load peak schedule: %v", err)
	}
	if got.Providers["codex"].Quota == nil || got.Providers["codex"].Quota.Schedule == nil {
		t.Fatal("peak schedule was not loaded")
	}
	schedule := got.Providers["codex"].Quota.Schedule
	peak := time.Date(2026, 1, 5, 15, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	outside := time.Date(2026, 1, 5, 13, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	if schedule.IsOffPeak(peak) {
		t.Fatal("15:00 Singapore time should be peak, not off-peak")
	}
	if !schedule.IsOffPeak(outside) {
		t.Fatal("13:00 Singapore time should be off-peak")
	}
}

func TestLoadRejectsLegacyOffPeakScheduleKey(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/gpt-5]
    quota:
      schedule:
        timezone: UTC
        off_peak:
          - days: [mon]
            start: "00:00"
            end: "08:00"
global:
  id: global
  root: /tmp/polytoken
  full: [codex/gpt-5]
`
	_, err := loadBytes([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "peak") {
		t.Fatalf("load error = %v, want peak migration error", err)
	}
}

func TestLoadRejectsBothPeakAndOffPeakScheduleKeys(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/gpt-5]
    quota:
      schedule:
        timezone: UTC
        peak: []
        off_peak: []
global:
  id: global
  root: /tmp/polytoken
  full: [codex/gpt-5]
`
	_, err := loadBytes([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("load error = %v, want both-keys error", err)
	}
}

func TestLoadRoutingQuota(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
    quota:
      freshness_ttl: 45m
      schedule:
        timezone: America/Los_Angeles
        peak:
          - days: [mon, tue, wed, thu, fri]
            start: "08:00"
            end: "24:00"
      balance_group: primary
      weight: 3
routing:
  enabled: true
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Routing.Enabled {
		t.Fatal("routing should be enabled")
	}
	q := d.Providers["codex"].Quota
	if q == nil {
		t.Fatal("missing codex quota config")
	}
	if q.Adapter != "codex" {
		t.Fatalf("adapter=%q", q.Adapter)
	}
	if q.FreshnessTTL != 45*time.Minute {
		t.Fatalf("freshness=%v", q.FreshnessTTL)
	}
	if q.BalanceGroup != "primary" {
		t.Fatalf("balance_group=%q", q.BalanceGroup)
	}
	if q.Weight != 3 {
		t.Fatalf("weight=%d", q.Weight)
	}
	if q.Schedule == nil {
		t.Fatal("missing schedule")
	}
}

// TestLoadRoutingDefaultsEnabled proves a desired.yaml without a routing
// section loads with routing enabled (the default since config simplification).
func TestLoadRoutingDefaultsEnabled(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Routing.Enabled {
		t.Fatal("routing should default to enabled when the section is omitted")
	}
}

// TestLoadRoutingExplicitDisable proves an explicit routing section still
// controls enablement, including opting out.
func TestLoadRoutingExplicitDisable(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
routing:
  enabled: false
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Routing.Enabled {
		t.Fatal("explicit routing.enabled=false should disable routing")
	}
}

// TestLoadRoutingQuotaDefaults proves omitted quota fields get their defaults
// (freshness 30m) and that quota enabled without a schedule keeps Schedule nil.
func TestLoadRoutingQuotaDefaults(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
    quota: {}
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Routing.Enabled {
		t.Fatal("routing should default to enabled")
	}
	q := d.Providers["codex"].Quota
	if q == nil {
		t.Fatal("missing codex quota config")
	}
	if q.FreshnessTTL != 30*time.Minute {
		t.Fatalf("freshness default=%v want 30m", q.FreshnessTTL)
	}
	if q.BalanceGroup != "default" {
		t.Fatalf("balance_group default=%q want %q", q.BalanceGroup, "default")
	}
	if q.Weight != 1 {
		t.Fatalf("weight default=%d want 1", q.Weight)
	}
	if q.Schedule != nil {
		t.Fatalf("schedule should be nil when omitted, got %+v", q.Schedule)
	}
}

// TestLoadRoutingQuotaBackwardCompat proves a desired.yaml without routing or
// quota sections still loads, with routing enabled by default and default quota
// configuration for recognized non-Anthropic providers.
func TestLoadRoutingQuotaBackwardCompat(t *testing.T) {
	d, err := Load("testdata/synthetic_desired.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Routing.Enabled {
		t.Fatal("routing should be enabled by default for the legacy fixture")
	}
	for _, id := range []MappingID{"codex", "zai"} {
		q := d.Providers[id].Quota
		if q == nil || q.Adapter != string(id) || q.FreshnessTTL != defaultQuotaFreshness || q.BalanceGroup != "default" || q.Weight != 1 || q.Schedule != nil {
			t.Fatalf("mapping %q quota=%+v, want normalized defaults", id, q)
		}
	}
}

func TestLoadOmittedQuotaDefaults(t *testing.T) {
	for _, id := range []string{"codex", "zai", "neuralwatt"} {
		t.Run(id, func(t *testing.T) {
			yaml := "version: 1\nproviders:\n  " + id + ":\n    models: [" + id + "/model]\n"
			d, err := Load(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			q := d.Providers[MappingID(id)].Quota
			if q == nil || q.Adapter != id || q.FreshnessTTL != defaultQuotaFreshness || q.BalanceGroup != "default" || q.Weight != 1 {
				t.Fatalf("quota=%+v, want normalized defaults", q)
			}
		})
	}
}

func TestLoadEmptyQuotaDefaults(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/model]
    quota: {}
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	q := d.Providers["codex"].Quota
	if q == nil || q.Adapter != "codex" || q.FreshnessTTL != defaultQuotaFreshness || q.BalanceGroup != "default" || q.Weight != 1 {
		t.Fatalf("quota=%+v, want normalized defaults", q)
	}
}

func TestLoadAnthropicQuotaSemantics(t *testing.T) {
	base := "version: 1\nproviders:\n  anthropic:\n    models: [anthropic/model]\n"
	cases := []struct {
		name    string
		suffix  string
		wantErr string
		wantNil bool
	}{
		{name: "omitted", wantNil: true},
		{name: "empty", suffix: "    quota: {}\n", wantNil: true},
		{name: "partial", suffix: "    quota:\n      freshness_ttl: 10m\n", wantErr: "requires monthly_budget_usd"},
		{name: "zero", suffix: "    quota:\n      monthly_budget_usd: 0\n", wantErr: "must be finite and positive"},
		{name: "negative", suffix: "    quota:\n      monthly_budget_usd: -1\n", wantErr: "must be finite and positive"},
		{name: "positive", suffix: "    quota:\n      monthly_budget_usd: 250\n", wantErr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load(writeTemp(t, base+tc.suffix))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			q := d.Providers["anthropic"].Quota
			if (q == nil) != tc.wantNil {
				t.Fatalf("quota=%+v, want nil=%v", q, tc.wantNil)
			}
			if q != nil && (q.MonthlyBudgetUSD != 250 || q.FreshnessTTL != defaultQuotaFreshness || q.BalanceGroup != "default" || q.Weight != 1) {
				t.Fatalf("quota=%+v, want positive-budget defaults", q)
			}
		})
	}
}

// TestLoadQuotaAdapterFromMappingKey proves the provider mapping key selects
// the quota adapter: a quota block needs no adapter field.
func TestLoadQuotaAdapterFromMappingKey(t *testing.T) {
	yaml := `version: 1
providers:
  zai:
    models: [zai/glm-4.5]
    quota: {}
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	q := d.Providers["zai"].Quota
	if q == nil {
		t.Fatal("missing zai quota config")
	}
	if q.Adapter != "zai" {
		t.Fatalf("adapter=%q want mapping key %q", q.Adapter, "zai")
	}
}

// TestLoadQuotaRejectsUnknownMappingKey proves a quota block under a mapping
// key that is not a known adapter name fails policy load with the valid names.
func TestLoadQuotaRejectsUnknownMappingKey(t *testing.T) {
	yaml := `version: 1
providers:
  codex2:
    models: [codex/gpt-5]
    quota: {}
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected rejection of unknown adapter mapping key, got nil")
	}
	for _, want := range []string{"codex2", "codex", "zai", "anthropic", "neuralwatt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestLoadQuotaUnknownKeyWithoutQuotaBlock proves a mapping without a quota
// block may use any key: only quota-participating mappings must name an
// adapter.
func TestLoadQuotaUnknownKeyWithoutQuotaBlock(t *testing.T) {
	yaml := `version: 1
providers:
  legacyrelay:
    models: [codex/gpt-5]
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Providers["legacyrelay"].Quota != nil {
		t.Fatal("unexpected quota config for mapping without quota block")
	}
}

// TestLoadAnthropicBudgetRequired proves empty Anthropic quota is accepted as
// visible but unpollable, while a positive monthly budget enables polling.
func TestLoadAnthropicBudgetRequired(t *testing.T) {
	base := "version: 1\nproviders:\n  anthropic:\n    models: [anthropic/claude]\n"
	d, err := Load(writeTemp(t, base+"    quota: {}\n"))
	if err != nil {
		t.Fatalf("Load empty quota: %v", err)
	}
	if d.Providers["anthropic"].Quota != nil {
		t.Fatalf("empty Anthropic quota=%+v, want unpollable nil config", d.Providers["anthropic"].Quota)
	}
	d, err = Load(writeTemp(t, base+"    quota:\n      monthly_budget_usd: 250\n"))
	if err != nil {
		t.Fatalf("Load with budget: %v", err)
	}
	q := d.Providers["anthropic"].Quota
	if q == nil || q.Adapter != "anthropic" || q.MonthlyBudgetUSD != 250 {
		t.Fatalf("anthropic quota=%+v", q)
	}
}

// TestLoadQuotaLegacyAdapterKeyIgnored proves a leftover `adapter` key in a
// quota block is ignored: it can neither select nor override the adapter
// derived from the mapping key.
func TestLoadQuotaLegacyAdapterKeyIgnored(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
    quota:
      adapter: zai
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q := d.Providers["codex"].Quota; q == nil || q.Adapter != "codex" {
		t.Fatalf("adapter=%+v want mapping key codex despite ignored legacy key", q)
	}
}

// TestLoadQuotaPeakWindowCrossingMidnightRejected proves a peak window whose
// start is not before its end rejects policy load: the desired.yaml grammar
// cannot express a midnight-crossing peak window.
func TestLoadQuotaPeakWindowCrossingMidnightRejected(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    models: [codex/m]
    quota:
      schedule:
        timezone: UTC
        peak:
          - days: [mon]
            start: "22:00"
            end: "06:00"
`
	_, err := Load(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected cross-midnight peak window to be rejected")
	}
	if !strings.Contains(err.Error(), "cross midnight") {
		t.Fatalf("error %q does not mention cross midnight", err.Error())
	}
}

func TestLoadRejectsNonFiniteAnthropicBudget(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		t.Run(value, func(t *testing.T) {
			yaml := "version: 1\nproviders:\n  anthropic:\n    models: [anthropic/claude]\n    quota:\n      monthly_budget_usd: " + value + "\n"
			if _, err := Load(writeTemp(t, yaml)); err == nil {
				t.Fatalf("Load accepted non-finite budget %s", value)
			}
		})
	}
}

// TestLoadRoutingQuotaInvalidSchedule proves a bad schedule (timezone/day/time)
// rejects policy loading rather than being silently accepted.
func TestLoadRoutingQuotaInvalidSchedule(t *testing.T) {
	for _, tc := range []struct{ name, schedule string }{
		{
			name:     "bad timezone",
			schedule: "timezone: Not/A/Zone\n        off_peak: []",
		},
		{
			name:     "bad day",
			schedule: "timezone: UTC\n        off_peak:\n          - days: [funday]\n            start: \"09:00\"\n            end: \"17:00\"",
		},
		{
			name:     "bad time",
			schedule: "timezone: UTC\n        off_peak:\n          - days: [mon]\n            start: \"99:00\"\n            end: \"17:00\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\n    quota:\n      schedule:\n        " + tc.schedule + "\n"
			_, err := Load(writeTemp(t, yaml))
			if err == nil {
				t.Fatal("expected schedule rejection, got nil error")
			}
		})
	}
}
