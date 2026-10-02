package service

// quota_gate exemption tests over the provider-only gate planner and the
// verbose/check provider projection. AC.1: an exempt out-of-quota provider
// never plans a disable, a pre-held quota claim is restored or released, and
// the gate summary names the action; a manual disable still plans the disable.
// AC.2(c): the provider projection names the clamped out-of-quota state with
// gating suspended rather than the plain "quota low (reserve)" wording, while
// a genuine low reserve keeps the plain wording. All inputs are synthetic; no
// filesystem, state store, or clock is involved.

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func fptrService(v float64) *float64 { return &v }

// quotaGateSnapshot builds a fresh poll snapshot stamped at now, with the
// given availability and used/limit on a single promo-shaped 48h window
// (used=100 is kwh_remaining 0), as a NeuralWatt promo weekend would report
// while the plan's monthly energy is exhausted.
func quotaGateSnapshot(now time.Time, availability quota.QuotaAvailability, used float64) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		CheckedAt:    now,
		Availability: availability,
		Status:       quota.SourceFresh,
		Windows: []quota.QuotaWindow{{
			Used: fptrService(used), Limit: fptrService(100),
			ResetAt: tptr64(now.Add(24 * time.Hour)), Period: durptr64(48 * time.Hour),
		}},
	}
}

// TestPlanProviderGateQuotaGateExemption is AC.1 as a table over the pure
// planner, mirroring the keep_enabled fixtures: the exemption plans no
// disable for out-of-quota observations, restores or releases a pre-held
// quota claim, and keeps manual disables and the non-exempt baseline exactly
// as they were.
func TestPlanProviderGateQuotaGateExemption(t *testing.T) {
	const rev = 9
	configTrue := []byte("providers:\n  gp:\n    enabled: true\n")
	configFalse := []byte("providers:\n  gp:\n    enabled: false\n")
	verdicts := map[string]routing.SignalGateVerdict{} // the signal axis stays quiet

	exhaustedAxes := state.ProviderState{Quota: state.QuotaExhausted, Availability: state.Available, QuotaSnapshot: quotaGateSnapshot(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), quota.QuotaUnavailable, 100)}
	reserveClaim := state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 4}

	cases := []struct {
		name string
		// inputs
		exempt  bool
		ps      state.ProviderState
		cfg     []byte
		own     map[string]state.ProviderOwnership
		// expectations
		wantDisable, wantRestore, wantConflict, claimReleased bool
		wantClaim           *state.ProviderOwnership
		wantAction          string
	}{
		{
			name:       "exempt provider with a zero-remaining snapshot plans no disable",
			exempt:     true,
			ps:         exhaustedAxes,
			cfg:        configTrue,
			wantAction: GateActionUnchanged,
		},
		{
			name:        "exempt provider's held reserve claim is restored when the exemption lands",
			exempt:      true,
			ps:          exhaustedAxes,
			cfg:         configFalse,
			own:         map[string]state.ProviderOwnership{"gp": reserveClaim},
			wantRestore: true, claimReleased: true,
			wantAction: GateActionRestored,
		},
		{
			name:   "claim released without edits when the operator already restored the baseline",
			exempt: true,
			ps:     exhaustedAxes,
			cfg:    configTrue,
			own:    map[string]state.ProviderOwnership{"gp": reserveClaim},
			claimReleased: true,
			wantAction:    GateActionReleased,
		},
		{
			name:   "operator divergence from a held claim still reports conflict",
			exempt: true,
			ps:     exhaustedAxes,
			cfg:    configTrue,
			own: map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 4}},
			wantConflict: true,
			wantClaim:    &state.ProviderOwnership{BaselinePresent: false, Owned: true, Axis: state.OwnershipAxisReserve, Conflict: true, EngagedRevision: 4},
			wantAction:   GateActionConflict,
		},
		{
			name:        "manual disable still plans the disable for an exempt provider",
			exempt:      true,
			ps:          state.ProviderState{Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
			cfg:         configTrue,
			wantDisable: true,
			wantClaim:   &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisDisabled, EngagedRevision: rev},
			wantAction:  GateActionDisabled,
		},
		{
			name:       "low-quota exempt provider is not gated off either",
			exempt:     true,
			ps:         state.ProviderState{Quota: state.QuotaLow, Availability: state.Available},
			cfg:        configTrue,
			wantAction: GateActionUnchanged,
		},
		{
			name:       "healthy exempt provider stays unchanged",
			exempt:     true,
			ps:         state.ProviderState{Quota: state.QuotaNormal, Availability: state.Available},
			cfg:        configTrue,
			wantAction: GateActionUnchanged,
		},
		{
			name:        "non-exempt exhausted provider still plans the disable (baseline unchanged)",
			exempt:      false,
			ps:          exhaustedAxes,
			cfg:         configTrue,
			wantDisable: true,
			wantClaim:   &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisDisabled, EngagedRevision: rev},
			wantAction:  GateActionDisabled,
		},
		{
			name:   "non-exempt held reserve claim stays held (baseline unchanged)",
			exempt: false,
			ps:     state.ProviderState{Quota: state.QuotaLow, Availability: state.Available},
			cfg:    configFalse,
			own:    map[string]state.ProviderOwnership{"gp": reserveClaim},
			wantClaim:  &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 4},
			wantAction: GateActionHeld,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := policy.Mapping{}
			if tc.exempt {
				m = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex", Gate: policy.QuotaGateConfig{Disabled: true}}}
			}
			desired := policy.Desired{
				Mode:      policy.ModeProviderOnly,
				Providers: map[policy.MappingID]policy.Mapping{"gp": m},
			}
			observed := state.State{Revision: 8, Providers: map[string]state.ProviderState{"gp": tc.ps}, ProviderOwnership: tc.own}

			plan, err := planProviderGate(desired, observed, tc.cfg, time.Time{}, verdicts, rev)
			if err != nil {
				t.Fatalf("planProviderGate: %v", err)
			}
			if tc.wantDisable != (len(plan.Disables) == 1) {
				t.Fatalf("disables=%v want disable=%v", plan.Disables, tc.wantDisable)
			}
			if tc.wantRestore != (len(plan.Restores) == 1) {
				t.Fatalf("restores=%v want restore=%v", plan.Restores, tc.wantRestore)
			}
			if tc.wantConflict != (len(plan.Conflicts) == 1) {
				t.Fatalf("conflicts=%v want conflict=%v", plan.Conflicts, tc.wantConflict)
			}
			if tc.claimReleased {
				if _, held := plan.PublishedOwnership["gp"]; held {
					t.Fatalf("claim not released: %+v", plan.PublishedOwnership)
				}
			}
			if tc.wantClaim != nil {
				got, ok := plan.PublishedOwnership["gp"]
				if !ok || got != *tc.wantClaim {
					t.Fatalf("published ownership=%+v want %+v", got, *tc.wantClaim)
				}
			}
			if len(plan.GateSummary) != 1 {
				t.Fatalf("gate summary rows=%d want 1: %+v", len(plan.GateSummary), plan.GateSummary)
			}
			if row := plan.GateSummary[0]; row.Action != tc.wantAction {
				t.Fatalf("summary action=%q want %q (row %+v)", row.Action, tc.wantAction, row)
			}
		})
	}
}

// TestProviderProjectionNamesSuspendedQuotaGating is AC.2(c): the
// verbose/check provider projection (ProjectProviders) names an exempt
// provider's clamped out-of-quota state with gating suspended — never the
// misleading plain "quota low (reserve)" — while genuine low reserves (exempt
// or not) keep the plain wording and the mode itself stays a truthful
// reserve.
func TestProviderProjectionNamesSuspendedQuotaGating(t *testing.T) {
	exemptQuota := &policy.QuotaConfig{Adapter: "codex", Gate: policy.QuotaGateConfig{Disabled: true}}
	cases := []struct {
		name        string
		quota       *policy.QuotaConfig
		ps          state.ProviderState
		wantMode    state.Mode
		wantReason  string
	}{
		{
			name:       "exempt exhausted axes",
			quota:      exemptQuota,
			ps:         state.ProviderState{Quota: state.QuotaExhausted, Availability: state.Available},
			wantMode:   state.ModeReserve,
			wantReason: "out of quota; quota gating suspended",
		},
		{
			name:       "exempt unavailable axes",
			quota:      exemptQuota,
			ps:         state.ProviderState{Quota: state.QuotaNormal, Availability: state.Unavailable},
			wantMode:   state.ModeReserve,
			wantReason: "provider unavailable; quota gating suspended",
		},
		{
			name:  "exempt exhausted poll snapshot with healthy axes",
			quota: exemptQuota,
			ps:    state.ProviderState{Quota: state.QuotaNormal, Availability: state.Available, QuotaSnapshot: quotaGateSnapshot(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), quota.QuotaUnavailable, 100)},
			wantMode:   state.ModeReserve,
			wantReason: "out of quota; quota gating suspended",
		},
		{
			name:       "exempt low quota keeps the plain reserve wording",
			quota:      exemptQuota,
			ps:         state.ProviderState{Quota: state.QuotaLow, Availability: state.Available},
			wantMode:   state.ModeReserve,
			wantReason: "quota low (reserve)",
		},
		{
			name:       "non-exempt exhausted keeps the disabled wording",
			quota:      &policy.QuotaConfig{Adapter: "codex"},
			ps:         state.ProviderState{Quota: state.QuotaExhausted, Availability: state.Available},
			wantMode:   state.ModeDisabled,
			wantReason: "quota exhausted",
		},
		{
			name:       "non-exempt low keeps the plain reserve wording",
			quota:      &policy.QuotaConfig{Adapter: "codex"},
			ps:         state.ProviderState{Quota: state.QuotaLow, Availability: state.Available},
			wantMode:   state.ModeReserve,
			wantReason: "quota low (reserve)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := policy.Desired{
				Mode:      policy.ModeProviderOnly,
				Providers: map[policy.MappingID]policy.Mapping{"gp": {Quota: tc.quota}},
			}
			observed := state.State{Providers: map[string]state.ProviderState{"gp": tc.ps}}
			providers := ProjectProviders(desired, observed)
			if len(providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(providers))
			}
			got := providers[0]
			if got.Mode != tc.wantMode || got.Reason != tc.wantReason {
				t.Fatalf("projection = (mode %v, reason %q), want (mode %v, reason %q)", got.Mode, got.Reason, tc.wantMode, tc.wantReason)
			}
		})
	}
}
