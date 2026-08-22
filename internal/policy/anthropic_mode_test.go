package policy

import (
	"strings"
	"testing"
	"time"
)

// TestLoadAnthropicQuotaMode covers `providers.anthropic.quota.mode`:
// subscription selects the anthropic-subscription adapter and forbids
// monthly_budget_usd; api (the default) keeps requiring it.
func TestLoadAnthropicQuotaMode(t *testing.T) {
	base := "version: 1\nproviders:\n  anthropic:\n    models: [anthropic/claude]\n"
	cases := []struct {
		name    string
		suffix  string
		wantErr string
	}{
		{name: "subscription", suffix: "    quota:\n      mode: subscription\n"},
		{name: "subscription_with_freshness", suffix: "    quota:\n      mode: subscription\n      freshness_ttl: 10m\n"},
		{name: "subscription_with_budget", suffix: "    quota:\n      mode: subscription\n      monthly_budget_usd: 250\n", wantErr: "does not use monthly_budget_usd"},
		{name: "api_explicit_with_budget", suffix: "    quota:\n      mode: api\n      monthly_budget_usd: 250\n"},
		{name: "api_explicit_without_budget", suffix: "    quota:\n      mode: api\n", wantErr: "requires monthly_budget_usd"},
		{name: "unknown_mode", suffix: "    quota:\n      mode: console\n      monthly_budget_usd: 250\n", wantErr: "unknown quota mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load(writeTemp(t, base+tc.suffix))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			q := d.Providers["anthropic"].Quota
			if q == nil {
				t.Fatal("missing anthropic quota config")
			}
			wantAdapter := "anthropic"
			if strings.Contains(tc.name, "subscription") {
				wantAdapter = "anthropic-subscription"
			}
			if q.Adapter != wantAdapter {
				t.Fatalf("adapter=%q want %q", q.Adapter, wantAdapter)
			}
			if q.FreshnessTTL <= 0 || q.FreshnessTTL > time.Hour {
				t.Fatalf("freshness=%v", q.FreshnessTTL)
			}
		})
	}
}

// TestLoadQuotaModeOnlyForAnthropic proves mode on another provider rejects.
func TestLoadQuotaModeOnlyForAnthropic(t *testing.T) {
	yaml := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\n    quota:\n      mode: subscription\n"
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "mode is only valid for the anthropic provider") {
		t.Fatalf("err=%v want mode-rejection", err)
	}
}
