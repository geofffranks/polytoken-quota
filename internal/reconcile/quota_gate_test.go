package reconcile

// quota_gate exemption tests: MappingMode is the single per-cause mode
// derivation, so these tables pin both the exemption's clamped causes
// (AC.6's fail-closed + sparse matrix over it) and the legacy chain projection
// (AC.3: an exempt exhausted mapping's models stay in managed chains in the
// reserve partition; a manual disable still drops them).

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func fptr(v float64) *float64 { return &v }

// snapshotWith builds a poll snapshot reporting the given availability and
// remaining fraction (used/limit on a single window).
func snapshotWith(availability quota.QuotaAvailability, used float64) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		CheckedAt:    time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Availability: availability,
		Status:       quota.SourceFresh,
		Windows:      []quota.QuotaWindow{{Used: fptr(used), Limit: fptr(100)}},
	}
}

// TestMappingModeQuotaGateExemption is the fail-closed + sparse matrix over
// the exemption (AC.6): exhausted/unavailable durable axes and the poll
// snapshot boundary clamp to reserve; manual disables and corrupted
// (non-empty unrecognized) axes still fail closed; sparse (empty-string) axes
// stay healthy exactly as EffectiveMode classifies them. Non-exempt rows pin
// the unchanged baseline.
func TestMappingModeQuotaGateExemption(t *testing.T) {
	cases := []struct {
		name         string
		exempt       bool
		quota        state.Quota
		availability state.Availability
		manual       bool
		snapshot     *quota.QuotaSnapshot
		want         state.Mode
	}{
		{name: "exhausted durable axis clamps to reserve", exempt: true, quota: state.QuotaExhausted, availability: state.Available, want: state.ModeReserve},
		{name: "unavailable durable axis clamps to reserve", exempt: true, quota: state.QuotaNormal, availability: state.Unavailable, want: state.ModeReserve},
		{name: "low quota keeps the reserve classification", exempt: true, quota: state.QuotaLow, availability: state.Available, want: state.ModeReserve},
		{name: "healthy axes stay normal", exempt: true, quota: state.QuotaNormal, availability: state.Available, want: state.ModeNormal},
		{name: "sparse axes stay healthy", exempt: true, want: state.ModeNormal},
		{name: "sparse availability with low quota stays reserve", exempt: true, quota: state.QuotaLow, want: state.ModeReserve},
		{name: "manual disable still disables", exempt: true, quota: state.QuotaNormal, availability: state.Available, manual: true, want: state.ModeDisabled},
		{name: "corrupted quota axis fails closed", exempt: true, quota: state.Quota("bogus"), availability: state.Available, want: state.ModeDisabled},
		{name: "corrupted availability axis fails closed", exempt: true, quota: state.QuotaNormal, availability: state.Availability("bogus"), want: state.ModeDisabled},
		{name: "corrupted plus exhausted fails closed", exempt: true, quota: state.Quota("bogus"), availability: state.Unavailable, want: state.ModeDisabled},
		{name: "manual plus corrupted fails closed", exempt: true, quota: state.Quota("bogus"), availability: state.Available, manual: true, want: state.ModeDisabled},
		{name: "exhausted poll snapshot clamps to reserve", exempt: true, quota: state.QuotaNormal, availability: state.Available, snapshot: snapshotWith(quota.QuotaUnavailable, 100), want: state.ModeReserve},
		{name: "unavailable snapshot with zero remaining clamps to reserve", exempt: true, quota: state.QuotaNormal, availability: state.Available, snapshot: snapshotWith(quota.QuotaUnavailable, 100), want: state.ModeReserve},
		{name: "available snapshot with zero remaining clamps to reserve", exempt: true, quota: state.QuotaNormal, availability: state.Available, snapshot: snapshotWith(quota.QuotaAvailable, 100), want: state.ModeReserve},
		{name: "exhausted durable axis and snapshot both clamp to reserve", exempt: true, quota: state.QuotaExhausted, availability: state.Available, snapshot: snapshotWith(quota.QuotaUnavailable, 100), want: state.ModeReserve},
		{name: "corrupted axis still fails closed across an exhausted snapshot", exempt: true, quota: state.Quota("bogus"), availability: state.Available, snapshot: snapshotWith(quota.QuotaUnavailable, 100), want: state.ModeDisabled},
		{name: "manual disable wins over a healthy snapshot", exempt: true, quota: state.QuotaNormal, availability: state.Available, manual: true, snapshot: snapshotWith(quota.QuotaAvailable, 10), want: state.ModeDisabled},
		{name: "non-exempt exhausted stays disabled (baseline unchanged)", exempt: false, quota: state.QuotaExhausted, availability: state.Available, want: state.ModeDisabled},
		{name: "non-exempt exhausted snapshot stays disabled (baseline unchanged)", exempt: false, quota: state.QuotaNormal, availability: state.Available, snapshot: snapshotWith(quota.QuotaUnavailable, 100), want: state.ModeDisabled},
		{name: "non-exempt sparse axes stay healthy (baseline unchanged)", exempt: false, want: state.ModeNormal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
				"p": {},
			}}
			if tc.exempt {
				desired.Providers["p"] = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex", Gate: policy.QuotaGateConfig{Disabled: true}}}
			}
			observed := state.State{Providers: map[string]state.ProviderState{
				"p": {
					Quota: tc.quota, Availability: tc.availability,
					ManualDisabled: tc.manual, QuotaSnapshot: tc.snapshot,
				},
			}}
			if got := MappingMode(desired, observed, "p"); got != tc.want {
				t.Fatalf("MappingMode=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestMappingModeAbsentProviderStaysHealthy pins the envelope: a mapping
// absent from the desired policy, or a provider absent from the observed
// state, is healthy regardless of the exemption (nothing is observed to
// gate on).
func TestMappingModeAbsentProviderStaysHealthy(t *testing.T) {
	exemptDesired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"p": {Quota: &policy.QuotaConfig{Adapter: "codex", Gate: policy.QuotaGateConfig{Disabled: true}}},
	}}
	var empty state.State
	if got := MappingMode(exemptDesired, empty, "p"); got != state.ModeNormal {
		t.Fatalf("unobserved exempt provider = %v, want normal", got)
	}
	if got := MappingMode(policy.Desired{Providers: map[policy.MappingID]policy.Mapping{}}, empty, "p"); got != state.ModeNormal {
		t.Fatalf("unmanaged mapping = %v, want normal", got)
	}
}

// exemptLegacyFixture builds a legacy-mode desired policy where exempt mapping
// "codex" carries a quota section with the opt-out, and healthy mapping "zai"
// provides a normal-partition survivor for partition assertions.
func exemptLegacyFixture() (policy.Desired, policy.Target) {
	desired := policy.Desired{
		Version: 1,
		Mode:    policy.ModeLegacy,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				Models: map[string]policy.ModelBaseline{"codex/g1": {Enabled: true}},
				Quota:  &policy.QuotaConfig{Adapter: "codex", Gate: policy.QuotaGateConfig{Disabled: true}},
			},
			"zai": {
				Models: map[string]policy.ModelBaseline{"zai/z1": {Enabled: true}},
			},
		},
	}
	target := policy.Target{
		ID:          "t",
		Root:        "/r",
		Global:      true,
		Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"codex/g1", "zai/z1"}}},
	}
	return desired, target
}

// TestLegacyExemptExhaustedKeepsModelsInChainReserve is AC.3: with the
// exemption, an exhausted quota observation keeps the mapping's models in
// managed chains, placed in the reserve partition (after normal survivors),
// and Build keeps the configured model baseline enabled. A manual disable
// still drops them and forces the baseline off.
func TestLegacyExemptExhaustedKeepsModelsInChainReserve(t *testing.T) {
	desired, target := exemptLegacyFixture()
	observed := state.State{Providers: map[string]state.ProviderState{
		"codex": {Quota: state.QuotaExhausted, Availability: state.Available},
		"zai":   {Quota: state.QuotaNormal, Availability: state.Available},
	}}

	order, err := EffectiveOrder(desired, observed, target.Definitions[0].Chain, nil)
	if err != nil {
		t.Fatalf("EffectiveOrder: %v", err)
	}
	if len(order) != 2 || order[0] != "zai/z1" || order[1] != "codex/g1" {
		t.Fatalf("effective order = %v, want the exempt exhausted mapping in the reserve partition after the healthy one", order)
	}

	plan, err := Build(desired, observed, target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	enabledByBase := map[string]bool{}
	for _, e := range plan.Edits {
		if len(e.Path) == 3 && e.Path[0] == "models" && e.Enabled != nil {
			enabledByBase[e.Path[1]] = *e.Enabled
		}
	}
	if !enabledByBase["codex/g1"] {
		t.Fatalf("exempt exhausted mapping's model baseline = false, want the configured true baseline: %v", enabledByBase)
	}

	// Manual disable still drops the models and forces the baseline off.
	observed.Providers["codex"] = state.ProviderState{
		Quota: state.QuotaExhausted, Availability: state.Available, ManualDisabled: true,
	}
	order, err = EffectiveOrder(desired, observed, target.Definitions[0].Chain, nil)
	if err != nil {
		t.Fatalf("EffectiveOrder (manual): %v", err)
	}
	if len(order) != 1 || order[0] != "zai/z1" {
		t.Fatalf("manual-disabled exempt mapping order = %v, want the model dropped", order)
	}
	plan, err = Build(desired, observed, target, nil)
	if err != nil {
		t.Fatalf("Build (manual): %v", err)
	}
	for _, e := range plan.Edits {
		if len(e.Path) == 3 && e.Path[0] == "models" && e.Path[1] == "codex/g1" {
			if e.Enabled == nil || *e.Enabled {
				t.Fatalf("manual-disabled exempt mapping baseline edit = %+v, want enabled=false", e)
			}
		}
	}
}
