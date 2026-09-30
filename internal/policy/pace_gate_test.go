package policy

// pace_gate configuration tests (pace gating AC.5): the strict sub-decode
// (unknown keys, wrong types, duplicate keys, explicit nulls, non-mapping
// shapes, out-of-range thresholds), the documented defaults for an absent
// block or omitted keys, the legacy-mode rejection, and import parity (a
// non-default pace_gate survives a render/load round trip while the default is
// omitted). All fixtures are synthetic; no live files.

import (
	"math"
	"strings"
	"testing"
)

// providerOnlyWithPaceGate renders a minimal provider-only policy whose first
// provider carries the given quota block body.
func providerOnlyWithPaceGate(quotaBody string) string {
	return "version: 1\n" +
		"mode: provider-only\n" +
		"providers:\n" +
		"  codex:\n" +
		"    quota:\n" +
		quotaBody +
		"global:\n" +
		"  root: /home/user/.config/polytoken\n"
}

func loadPaceGateConfig(t *testing.T, quotaBody string) *QuotaConfig {
	t.Helper()
	d, err := Load(writeTemp(t, providerOnlyWithPaceGate(quotaBody)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, ok := d.Providers["codex"]
	if !ok || m.Quota == nil {
		t.Fatalf("codex mapping with quota block missing: %+v", d.Providers)
	}
	return m.Quota
}

// TestPaceGateConfigValidation proves the pace_gate sub-decode is strict, the
// documented defaults apply for an absent block or omitted keys, and an
// invalid configuration is a load error — never a silent default.
func TestPaceGateConfigValidation(t *testing.T) {
	cases := []struct {
		name      string
		quotaBody string
		want      PaceGateConfig
		wantErr   string
	}{
		{
			name:      "absent block defaults on at 1.0",
			quotaBody: "      adapter: codex\n",
			want:      DefaultPaceGate(),
		},
		{
			name:      "empty block defaults on at 1.0",
			quotaBody: "      adapter: codex\n      pace_gate: {}\n",
			want:      DefaultPaceGate(),
		},
		{
			name:      "null block defaults on at 1.0",
			quotaBody: "      adapter: codex\n      pace_gate:\n",
			want:      DefaultPaceGate(),
		},
		{
			name:      "omitted keys default individually",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: 1.25}\n",
			want:      PaceGateConfig{Enabled: true, Threshold: 1.25},
		},
		{
			name:      "explicit disabled keeps default threshold",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: false}\n",
			want:      PaceGateConfig{Enabled: false, Threshold: 1.0},
		},
		{
			name:      "sub-unity threshold is a valid override",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: true, threshold: 0.8}\n",
			want:      PaceGateConfig{Enabled: true, Threshold: 0.8},
		},
		{
			name:      "unknown key rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: true, hysteresis: 2}\n",
			wantErr:   "policy: pace_gate: unknown key (want enabled or threshold)",
		},
		{
			name:      "non-boolean enabled rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: \"true\"}\n",
			wantErr:   "policy: pace_gate: enabled must be a boolean",
		},
		{
			name:      "explicit null enabled rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: ~}\n",
			wantErr:   "policy: pace_gate: enabled must be a boolean",
		},
		{
			name:      "non-numeric threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: high}\n",
			wantErr:   "policy: pace_gate: threshold must be a positive number",
		},
		{
			name:      "boolean threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: true}\n",
			wantErr:   "policy: pace_gate: threshold must be a positive number",
		},
		{
			name:      "explicit null threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: null}\n",
			wantErr:   "policy: pace_gate: threshold must be a positive number",
		},
		{
			name:      "duplicate enabled key rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {enabled: true, enabled: false}\n",
			wantErr:   "policy: pace_gate: duplicate enabled key",
		},
		{
			name:      "duplicate threshold key rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: 1.0, threshold: 1.5}\n",
			wantErr:   "policy: pace_gate: duplicate threshold key",
		},
		{
			name:      "non-mapping block rejected",
			quotaBody: "      adapter: codex\n      pace_gate: 5\n",
			wantErr:   "policy: pace_gate must be a mapping",
		},
		{
			name:      "zero threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: 0}\n",
			wantErr:   "pace_gate threshold must be finite and positive",
		},
		{
			name:      "negative threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: -0.5}\n",
			wantErr:   "pace_gate threshold must be finite and positive",
		},
		{
			name:      "nan threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: .nan}\n",
			wantErr:   "pace_gate threshold must be finite and positive",
		},
		{
			name:      "infinite threshold rejected",
			quotaBody: "      adapter: codex\n      pace_gate: {threshold: .inf}\n",
			wantErr:   "pace_gate threshold must be finite and positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr == "" {
				qc := loadPaceGateConfig(t, tc.quotaBody)
				if qc.PaceGate != tc.want {
					t.Fatalf("PaceGate = %+v, want %+v", qc.PaceGate, tc.want)
				}
				return
			}
			_, err := Load(writeTemp(t, providerOnlyWithPaceGate(tc.quotaBody)))
			if err == nil {
				t.Fatalf("Load succeeded, want error %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %q", err, tc.wantErr)
			}
		})
	}
}

// TestPaceGateRejectedInLegacyMode proves a legacy policy cannot carry
// pace_gate: the key is a provider-only gating concept and legacy mode keeps
// rank-demotion-only pace semantics, so accepting it there could only ever
// silently ignore operator intent.
func TestPaceGateRejectedInLegacyMode(t *testing.T) {
	legacy := "version: 1\n" +
		"providers:\n" +
		"  codex:\n" +
		"    quota:\n" +
		"      pace_gate: {enabled: true}\n" +
		"    models: [codex/g1]\n"
	_, err := Load(writeTemp(t, legacy))
	if err == nil {
		t.Fatalf("legacy pace_gate load succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "pace_gate is only valid in provider-only mode") {
		t.Fatalf("error %q does not name the legacy rejection", err)
	}
}

// TestPaceGateImportRoundTrip proves import parity: a non-default pace_gate
// survives marshalDesired + Load unchanged, and the documented default is
// omitted from the rendered policy (loading back identically).
func TestPaceGateImportRoundTrip(t *testing.T) {
	d := Desired{
		Version:   1,
		Mode:      ModeProviderOnly,
		Selection: defaultSelection(), // Load always resolves the selection defaults
		Providers: map[MappingID]Mapping{
			"codex": {Quota: &QuotaConfig{Adapter: "codex", PaceGate: PaceGateConfig{Enabled: false, Threshold: 1.25}}},
			"zai":   {Quota: &QuotaConfig{Adapter: "zai"}},
		},
		Global: Target{ID: "global", Root: "/home/user/.config/polytoken", Global: true},
	}
	rendered, err := marshalDesired(d)
	if err != nil {
		t.Fatalf("marshalDesired: %v", err)
	}
	if !strings.Contains(string(rendered), "pace_gate:") {
		t.Fatalf("rendered policy dropped the non-default pace_gate:\n%s", rendered)
	}
	if strings.Count(string(rendered), "pace_gate:") != 1 {
		t.Fatalf("rendered policy emitted the default pace_gate too:\n%s", rendered)
	}
	loaded, err := Load(writeTemp(t, string(rendered)))
	if err != nil {
		t.Fatalf("Load rendered: %v", err)
	}
	if got := loaded.Providers["codex"].Quota.PaceGate; got != (PaceGateConfig{Enabled: false, Threshold: 1.25}) {
		t.Fatalf("codex PaceGate round trip = %+v", got)
	}
	if got := loaded.Providers["zai"].Quota.PaceGate; got != DefaultPaceGate() {
		t.Fatalf("zai PaceGate round trip = %+v, want the documented default", got)
	}
	// Guard the test's own NaN comparison assumptions: IsDefault must reject
	// every non-finite threshold so a hand-built config never renders.
	nan := PaceGateConfig{Threshold: math.NaN()}
	if nan.IsDefault() {
		t.Fatalf("NaN threshold must never compare as the default")
	}
}
