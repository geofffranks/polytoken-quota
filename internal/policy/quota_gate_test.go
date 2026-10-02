package policy

// quota_gate configuration tests: the strict sub-decode (unknown keys, wrong
// types, duplicate keys, explicit nulls, non-mapping shapes), the documented
// default for an absent block or omitted keys, the deliberate mode-reach
// divergence from signal_gate (accepted in BOTH policy modes wherever a quota
// section is valid), and import parity (a non-default quota_gate survives a
// render/load round trip while the default is omitted). All fixtures are
// synthetic; no live files.

import (
	"strings"
	"testing"
)

// TestQuotaGateConfigValidation proves the quota_gate sub-decode is strict,
// the documented default applies for an absent block or omitted keys, and an
// invalid configuration is a load error — never a silent default.
func TestQuotaGateConfigValidation(t *testing.T) {
	cases := []struct {
		name      string
		quotaBody string
		want      QuotaGateConfig
		wantErr   string
	}{
		{
			name:      "absent block defaults to gating on",
			quotaBody: "      adapter: codex\n",
			want:      DefaultQuotaGate(),
		},
		{
			name:      "empty block defaults to gating on",
			quotaBody: "      adapter: codex\n      quota_gate: {}\n",
			want:      DefaultQuotaGate(),
		},
		{
			name:      "null block defaults to gating on",
			quotaBody: "      adapter: codex\n      quota_gate:\n",
			want:      DefaultQuotaGate(),
		},
		{
			name:      "explicit enabled true is the documented default",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: true}\n",
			want:      DefaultQuotaGate(),
		},
		{
			name:      "explicit disabled suspends quota gating",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: false}\n",
			want:      QuotaGateConfig{Disabled: true},
		},
		{
			name:      "unknown key rejected",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: true, threshold: -1}\n",
			wantErr:   "policy: quota_gate: unknown key (want enabled)",
		},
		{
			name:      "non-boolean enabled rejected",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: \"false\"}\n",
			wantErr:   "policy: quota_gate: enabled must be a boolean",
		},
		{
			name:      "explicit null enabled rejected",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: ~}\n",
			wantErr:   "policy: quota_gate: enabled must be a boolean",
		},
		{
			name:      "duplicate enabled key rejected",
			quotaBody: "      adapter: codex\n      quota_gate: {enabled: true, enabled: false}\n",
			wantErr:   "policy: quota_gate: duplicate enabled key",
		},
		{
			name:      "non-mapping block rejected",
			quotaBody: "      adapter: codex\n      quota_gate: 5\n",
			wantErr:   "policy: quota_gate must be a mapping",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr == "" {
				qc := loadSignalGateConfig(t, tc.quotaBody)
				if qc.Gate != tc.want {
					t.Fatalf("Gate = %+v, want %+v", qc.Gate, tc.want)
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

// TestQuotaGateAcceptedInBothModes proves the deliberate divergence from the
// signal_gate precedent: the key loads under a provider-only enrollment AND
// under a legacy adapter-keyed mapping's quota section, resolving to the
// documented default when absent and to the opt-out when explicitly disabled.
func TestQuotaGateAcceptedInBothModes(t *testing.T) {
	t.Run("provider-only enrollment", func(t *testing.T) {
		qc := loadSignalGateConfig(t, "      adapter: codex\n      quota_gate: {enabled: false}\n")
		if !qc.Gate.Resolved().Disabled || qc.Gate.IsDefault() {
			t.Fatalf("provider-only Gate = %+v, want the explicit opt-out", qc.Gate)
		}
	})

	t.Run("legacy adapter-keyed mapping", func(t *testing.T) {
		legacy := "version: 1\n" +
			"providers:\n" +
			"  codex:\n" +
			"    quota:\n" +
			"      quota_gate: {enabled: false}\n" +
			"    models: [codex/g1]\n"
		d, err := Load(writeTemp(t, legacy))
		if err != nil {
			t.Fatalf("legacy quota_gate load: %v", err)
		}
		m, ok := d.Providers["codex"]
		if !ok || m.Quota == nil {
			t.Fatalf("codex mapping with quota block missing: %+v", d.Providers)
		}
		if !m.Quota.Gate.Resolved().Disabled {
			t.Fatalf("legacy Gate = %+v, want the explicit opt-out", m.Quota.Gate)
		}
	})

	t.Run("legacy absent block defaults", func(t *testing.T) {
		legacy := "version: 1\n" +
			"providers:\n" +
			"  codex:\n" +
			"    quota:\n" +
			"      balance_group: pool-a\n" +
			"    models: [codex/g1]\n"
		d, err := Load(writeTemp(t, legacy))
		if err != nil {
			t.Fatalf("legacy load: %v", err)
		}
		if got := d.Providers["codex"].Quota.Gate; got != DefaultQuotaGate() {
			t.Fatalf("legacy Gate = %+v, want the documented default", got)
		}
	})
}

// TestQuotaGateImportRoundTrip proves import parity: a non-default quota_gate
// survives marshalDesired + Load unchanged, and the documented default is
// omitted from the rendered policy (loading back identically).
func TestQuotaGateImportRoundTrip(t *testing.T) {
	d := Desired{
		Version:   1,
		Mode:      ModeProviderOnly,
		Selection: defaultSelection(), // Load always resolves the selection defaults
		Providers: map[MappingID]Mapping{
			"codex": {Quota: &QuotaConfig{Adapter: "codex", Gate: QuotaGateConfig{Disabled: true}}},
			"zai":   {Quota: &QuotaConfig{Adapter: "zai"}},
		},
		Global: Target{ID: "global", Root: "/home/user/.config/polytoken", Global: true},
	}
	rendered, err := marshalDesired(d)
	if err != nil {
		t.Fatalf("marshalDesired: %v", err)
	}
	if !strings.Contains(string(rendered), "quota_gate:") {
		t.Fatalf("rendered policy dropped the non-default quota_gate:\n%s", rendered)
	}
	if strings.Count(string(rendered), "quota_gate:") != 1 {
		t.Fatalf("rendered policy emitted the default quota_gate too:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "enabled: false") {
		t.Fatalf("rendered quota_gate must carry its enabled key:\n%s", rendered)
	}
	loaded, err := Load(writeTemp(t, string(rendered)))
	if err != nil {
		t.Fatalf("Load rendered: %v", err)
	}
	if got := loaded.Providers["codex"].Quota.Gate; got != (QuotaGateConfig{Disabled: true}) {
		t.Fatalf("codex Gate round trip = %+v", got)
	}
	if got := loaded.Providers["zai"].Quota.Gate; got != DefaultQuotaGate() {
		t.Fatalf("zai Gate round trip = %+v, want the documented default", got)
	}
}
