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
// CodexBar→Polytoken provider mapping with exact concrete model enumeration, the
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
	if !sliceEq(codex.CodexBarProviders, "codex") || !sliceEq(codex.PolytokenProviders, "codex") {
		t.Fatalf("codex providers cb=%v pt=%v", codex.CodexBarProviders, codex.PolytokenProviders)
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
	if !sliceEq(zai.CodexBarProviders, "z.ai") || !sliceEq(zai.PolytokenProviders, "zai") {
		t.Fatalf("zai providers cb=%v pt=%v", zai.CodexBarProviders, zai.PolytokenProviders)
	}
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
		{"empty enumeration", "version: 1\nproviders: {codex: {codexbar_providers: [codex], models: []}}", "models"},
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

// TestLoadRejectsAmbiguousProviders proves two mappings claiming the same CodExBar
// (or Polytoken) provider are rejected as ambiguous.
func TestLoadRejectsAmbiguousProviders(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"codexbar shared", "version: 1\nproviders:\n  a: {codexbar_providers: [codex], models: [codex/m1]}\n  b: {codexbar_providers: [codex], models: [codex/m2]}", "ambiguous"},
		{"polytoken shared", "version: 1\nproviders:\n  a: {polytoken_providers: [codex], models: [codex/m1]}\n  b: {polytoken_providers: [codex], models: [codex/m2]}", "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

// TestLoadBaselineEnabledState proves the optional baseline enabled state and the
// HadEnabledKey origin tracking: a bare name defaults to enabled with no key,
// while an explicit enabled:false records HadEnabledKey true.
func TestLoadBaselineEnabledState(t *testing.T) {
	yaml := "version: 1\nproviders:\n  a:\n    codexbar_providers: [codex]\n    models:\n      - codex/on\n      - codex/off: {enabled: false}\n"
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

// TestResolveCodexBar proves event-provider resolution: known CodExBar IDs resolve
// to their mapping, unknown IDs are rejected.
func TestResolveCodexBar(t *testing.T) {
	d := loadedFixture(t)
	if id, err := d.ResolveCodexBar("codex"); err != nil || id != "codex" {
		t.Fatalf("ResolveCodexBar(codex)=%q err=%v", id, err)
	}
	if id, err := d.ResolveCodexBar("z.ai"); err != nil || id != "zai" {
		t.Fatalf("ResolveCodexBar(z.ai)=%q err=%v", id, err)
	}
	if _, err := d.ResolveCodexBar("unknown"); err == nil {
		t.Fatal("accepted unknown codexbar provider")
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
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models: [codex/gpt-5]
    quota:
      adapter: codex
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
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models: [codex/gpt-5]
    quota:
      adapter: codex
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
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models: [codex/gpt-5]
    quota:
      adapter: codex
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
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models: [codex/m]
    quota:
      adapter: codex
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

// TestLoadRoutingQuotaDefaults proves omitted quota fields get their defaults
// (freshness 30m) and that quota enabled without a schedule keeps Schedule nil.
func TestLoadRoutingQuotaDefaults(t *testing.T) {
	yaml := `version: 1
providers:
  codex:
    codexbar_providers: [codex]
    models: [codex/m]
    quota:
      adapter: codex
`
	d, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Routing.Enabled {
		t.Fatal("routing should default to disabled")
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
// quota sections loads with routing disabled and nil quota (backward compat).
func TestLoadRoutingQuotaBackwardCompat(t *testing.T) {
	d, err := Load("testdata/synthetic_desired.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Routing.Enabled {
		t.Fatal("routing should be disabled for legacy fixture")
	}
	for id, m := range d.Providers {
		if m.Quota != nil {
			t.Fatalf("mapping %q has unexpected quota config %+v", id, m.Quota)
		}
	}
}

func TestLoadRejectsNonFiniteAnthropicBudget(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		t.Run(value, func(t *testing.T) {
			yaml := "version: 1\nproviders:\n  anthropic:\n    codexbar_providers: [anthropic]\n    models: [anthropic/claude]\n    quota:\n      adapter: anthropic\n      monthly_budget_usd: " + value + "\n"
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
			yaml := "version: 1\nproviders:\n  codex:\n    codexbar_providers: [codex]\n    models: [codex/m]\n    quota:\n      adapter: codex\n      schedule:\n        " + tc.schedule + "\n"
			_, err := Load(writeTemp(t, yaml))
			if err == nil {
				t.Fatal("expected schedule rejection, got nil error")
			}
		})
	}
}
