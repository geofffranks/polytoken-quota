package policy

// signal_gate configuration tests (approved amendment: gating keys off the
// use-it-or-lose-it routing signal): the strict sub-decode (unknown keys,
// wrong types, duplicate keys, explicit nulls, non-mapping shapes,
// out-of-range thresholds), the documented defaults for an absent block or
// omitted keys, the legacy-mode rejection, and import parity (a non-default
// signal_gate survives a render/load round trip while the default is
// omitted). All fixtures are synthetic; no live files.

import (
	"math"
	"strings"
	"testing"
)

// providerOnlyWithSignalGate renders a minimal provider-only policy whose
// first provider carries the given quota block body.
func providerOnlyWithSignalGate(quotaBody string) string {
	return "version: 1\n" +
		"mode: provider-only\n" +
		"providers:\n" +
		"  codex:\n" +
		"    quota:\n" +
		quotaBody +
		"global:\n" +
		"  root: /home/user/.config/polytoken\n"
}

func loadSignalGateConfig(t *testing.T, quotaBody string) *QuotaConfig {
	t.Helper()
	d, err := Load(writeTemp(t, providerOnlyWithSignalGate(quotaBody)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, ok := d.Providers["codex"]
	if !ok || m.Quota == nil {
		t.Fatalf("codex mapping with quota block missing: %+v", d.Providers)
	}
	return m.Quota
}

// TestSignalGateConfigValidation proves the signal_gate sub-decode is strict,
// the documented defaults apply for an absent block or omitted keys, and an
// invalid configuration is a load error — never a silent default.
func TestSignalGateConfigValidation(t *testing.T) {
	cases := []struct {
		name      string
		quotaBody string
		want      SignalGateConfig
		wantErr   string
	}{
		{
			name:      "absent block defaults on at the exhaustion line",
			quotaBody: "      adapter: codex\n",
			want:      DefaultSignalGate(),
		},
		{
			name:      "empty block defaults on at the exhaustion line",
			quotaBody: "      adapter: codex\n      signal_gate: {}\n",
			want:      DefaultSignalGate(),
		},
		{
			name:      "null block defaults on at the exhaustion line",
			quotaBody: "      adapter: codex\n      signal_gate:\n",
			want:      DefaultSignalGate(),
		},
		{
			name:      "omitted keys default individually",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: -0.25}\n",
			want:      SignalGateConfig{Threshold: -0.25},
		},
		{
			name:      "explicit disabled keeps default threshold",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: false}\n",
			want:      SignalGateConfig{Disabled: true, Threshold: 0.0},
		},
		{
			name:      "zero threshold is the documented default",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: true, threshold: 0}\n",
			want:      SignalGateConfig{Threshold: 0.0},
		},
		{
			name:      "negative threshold is a valid margin override",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: true, threshold: -0.5}\n",
			want:      SignalGateConfig{Threshold: -0.5},
		},
		{
			name:      "unknown key rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: true, hysteresis: 2}\n",
			wantErr:   "policy: signal_gate: unknown key (want enabled or threshold)",
		},
		{
			name:      "non-boolean enabled rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: \"true\"}\n",
			wantErr:   "policy: signal_gate: enabled must be a boolean",
		},
		{
			name:      "explicit null enabled rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: ~}\n",
			wantErr:   "policy: signal_gate: enabled must be a boolean",
		},
		{
			name:      "non-numeric threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: high}\n",
			wantErr:   "policy: signal_gate: threshold must be a non-positive number",
		},
		{
			name:      "boolean threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: true}\n",
			wantErr:   "policy: signal_gate: threshold must be a non-positive number",
		},
		{
			name:      "explicit null threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: null}\n",
			wantErr:   "policy: signal_gate: threshold must be a non-positive number",
		},
		{
			name:      "duplicate enabled key rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {enabled: true, enabled: false}\n",
			wantErr:   "policy: signal_gate: duplicate enabled key",
		},
		{
			name:      "duplicate threshold key rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: -1.0, threshold: -1.5}\n",
			wantErr:   "policy: signal_gate: duplicate threshold key",
		},
		{
			name:      "non-mapping block rejected",
			quotaBody: "      adapter: codex\n      signal_gate: 5\n",
			wantErr:   "policy: signal_gate must be a mapping",
		},
		{
			name:      "positive threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: 0.5}\n",
			wantErr:   "signal_gate threshold must be finite and non-positive",
		},
		{
			name:      "nan threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: .nan}\n",
			wantErr:   "signal_gate threshold must be finite and non-positive",
		},
		{
			name:      "infinite threshold rejected",
			quotaBody: "      adapter: codex\n      signal_gate: {threshold: -.inf}\n",
			wantErr:   "signal_gate threshold must be finite and non-positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr == "" {
				qc := loadSignalGateConfig(t, tc.quotaBody)
				if qc.SignalGate != tc.want {
					t.Fatalf("SignalGate = %+v, want %+v", qc.SignalGate, tc.want)
				}
				return
			}
			_, err := Load(writeTemp(t, providerOnlyWithSignalGate(tc.quotaBody)))
			if err == nil {
				t.Fatalf("Load succeeded, want error %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %q", err, tc.wantErr)
			}
		})
	}
}

// TestSignalGateRejectedInLegacyMode proves a legacy policy cannot carry
// signal_gate: the key is a provider-only gating concept and legacy mode keeps
// rank-demotion-only semantics, so accepting it there could only ever
// silently ignore operator intent.
func TestSignalGateRejectedInLegacyMode(t *testing.T) {
	legacy := "version: 1\n" +
		"providers:\n" +
		"  codex:\n" +
		"    quota:\n" +
		"      signal_gate: {enabled: true}\n" +
		"    models: [codex/g1]\n"
	_, err := Load(writeTemp(t, legacy))
	if err == nil {
		t.Fatalf("legacy signal_gate load succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "signal_gate is only valid in provider-only mode") {
		t.Fatalf("error %q does not name the legacy rejection", err)
	}
}

// TestSignalGateImportRoundTrip proves import parity: a non-default
// signal_gate survives marshalDesired + Load unchanged, and the documented
// default is omitted from the rendered policy (loading back identically).
func TestSignalGateImportRoundTrip(t *testing.T) {
	d := Desired{
		Version:   1,
		Mode:      ModeProviderOnly,
		Selection: defaultSelection(), // Load always resolves the selection defaults
		Providers: map[MappingID]Mapping{
			"codex": {Quota: &QuotaConfig{Adapter: "codex", SignalGate: SignalGateConfig{Disabled: true, Threshold: -0.25}}},
			"zai":   {Quota: &QuotaConfig{Adapter: "zai"}},
		},
		Global: Target{ID: "global", Root: "/home/user/.config/polytoken", Global: true},
	}
	rendered, err := marshalDesired(d)
	if err != nil {
		t.Fatalf("marshalDesired: %v", err)
	}
	if !strings.Contains(string(rendered), "signal_gate:") {
		t.Fatalf("rendered policy dropped the non-default signal_gate:\n%s", rendered)
	}
	if strings.Count(string(rendered), "signal_gate:") != 1 {
		t.Fatalf("rendered policy emitted the default signal_gate too:\n%s", rendered)
	}
	loaded, err := Load(writeTemp(t, string(rendered)))
	if err != nil {
		t.Fatalf("Load rendered: %v", err)
	}
	if got := loaded.Providers["codex"].Quota.SignalGate; got != (SignalGateConfig{Disabled: true, Threshold: -0.25}) {
		t.Fatalf("codex SignalGate round trip = %+v", got)
	}
	if got := loaded.Providers["zai"].Quota.SignalGate; got != DefaultSignalGate() {
		t.Fatalf("zai SignalGate round trip = %+v, want the documented default", got)
	}
	// Guard the test's own NaN comparison assumptions: IsDefault must reject
	// every non-finite threshold so a hand-built config never renders.
	nan := SignalGateConfig{Threshold: math.NaN()}
	if nan.IsDefault() {
		t.Fatalf("NaN threshold must never compare as the default")
	}
}
