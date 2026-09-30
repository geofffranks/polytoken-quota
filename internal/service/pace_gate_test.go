package service

// Pace-gate unit tests (pace gating AC.2): the pure all-hot pool rule table
// and the planner's pace decision table (engage, hold, transfer, pool-skip
// release, conflict, degraded-evidence release). All inputs are synthetic
// values; no filesystem, state store, or clock is involved in the pure table.

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func paceVerdict(gated bool) routing.PaceGateVerdict {
	p := 1.36
	return routing.PaceGateVerdict{MappingID: "", Pace: &p, Gated: gated, Reason: "test"}
}

// TestPacePoolRuleTable proves the all-hot pool escape hatch: a pool is
// skipped exactly when its proposed pace-gate set covers every member that
// would otherwise remain enabled (operator-held and reserve/disabled-gated
// members never count as enabled), and pace-held claims in a skipped pool are
// released unless another axis still gates them.
func TestPacePoolRuleTable(t *testing.T) {
	hot := map[string]routing.PaceGateVerdict{
		"a": paceVerdict(true),
		"b": paceVerdict(false), // under threshold / uncomputable pace
		"c": paceVerdict(true),
		"d": paceVerdict(true),
	}
	reserve := state.ModeReserve
	disabled := state.ModeDisabled
	cases := []struct {
		name         string
		enrolled     []string
		groups       map[string]string // provider -> balance group
		liveEnabled  map[string]bool   // absent = live enabled
		modes        map[string]state.Mode
		paceHeld     []string
		wantSkipped  []string
		wantReleased []string
		wantPools    []string
	}{
		{
			name:        "operator-disabled member does not count as enabled",
			enrolled:    []string{"a", "b"},
			liveEnabled: map[string]bool{"b": false},
			wantSkipped: []string{"a"},
			wantPools:   []string{"default"},
		},
		{
			name:        "reserve-held member stays gated and does not count as enabled",
			enrolled:    []string{"a", "b"},
			modes:       map[string]state.Mode{"b": reserve},
			wantSkipped: []string{"a"},
			wantPools:   []string{"default"},
		},
		{
			name:        "uncomputable-pace member keeps the pool open (no skip)",
			enrolled:    []string{"a", "b"},
			wantSkipped: nil,
			wantPools:   nil,
		},
		{
			name:         "singleton pool is never pace-gated and releases its held claim",
			enrolled:     []string{"a"},
			paceHeld:     []string{"a"},
			wantSkipped:  []string{"a"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:         "all-hot pool releases pace-held claims",
			enrolled:     []string{"a", "c"},
			paceHeld:     []string{"a"},
			wantSkipped:  []string{"a", "c"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:         "reserve-mode pace claim in a skipped pool stays held",
			enrolled:     []string{"a", "c"},
			modes:        map[string]state.Mode{"a": reserve},
			paceHeld:     []string{"a"},
			wantSkipped:  []string{"a", "c"},
			wantReleased: nil,
			wantPools:    []string{"default"},
		},
		{
			name:        "separate pools decide independently (default pool stays open, singleton pool-2 skips)",
			enrolled:    []string{"a", "b", "c"},
			groups:      map[string]string{"c": "pool-2"},
			liveEnabled: map[string]bool{"c": true},
			wantSkipped: []string{"c"},
			wantPools:   []string{"pool-2"},
		},
		{
			name:        "disabled-mode member does not count as enabled either",
			enrolled:    []string{"a", "d"},
			modes:       map[string]state.Mode{"d": disabled},
			wantSkipped: []string{"a", "d"},
			wantPools:   []string{"default"},
		},
		{
			name:        "no gated member never skips",
			enrolled:    []string{"b"},
			wantSkipped: nil,
			wantPools:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := policy.Desired{
				Mode:      policy.ModeProviderOnly,
				Providers: map[policy.MappingID]policy.Mapping{},
			}
			for _, id := range tc.enrolled {
				m := policy.Mapping{}
				if g, ok := tc.groups[id]; ok {
					m.Quota = &policy.QuotaConfig{Adapter: "codex", BalanceGroup: g}
				}
				desired.Providers[policy.MappingID(id)] = m
			}
			live := map[string]bool{}
			for _, id := range tc.enrolled {
				live[id] = true // absent from tc.liveEnabled means live enabled
				if v, ok := tc.liveEnabled[id]; ok {
					live[id] = v
				}
			}
			modes := map[string]state.Mode{}
			for _, id := range tc.enrolled {
				modes[id] = tc.modes[id] // absent = normal
			}
			held := map[string]bool{}
			for _, id := range tc.paceHeld {
				held[id] = true
			}
			dec := pacePoolRule(desired, hot, live, modes, held)
			if len(dec.SkipPace) != len(tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", keysOf(dec.SkipPace), tc.wantSkipped)
			}
			for _, id := range tc.wantSkipped {
				if !dec.SkipPace[id] {
					t.Fatalf("skipped = %v, want %v to be skipped", keysOf(dec.SkipPace), tc.wantSkipped)
				}
			}
			if len(dec.Release) != len(tc.wantReleased) {
				t.Fatalf("released = %v, want %v", keysOf(dec.Release), tc.wantReleased)
			}
			for _, id := range tc.wantReleased {
				if !dec.Release[id] {
					t.Fatalf("released = %v, want %v to be released", keysOf(dec.Release), tc.wantReleased)
				}
			}
			if len(dec.Pools) != len(tc.wantPools) {
				t.Fatalf("pools = %v, want %v", dec.Pools, tc.wantPools)
			}
			for i, g := range tc.wantPools {
				if dec.Pools[i] != g {
					t.Fatalf("pools = %v, want %v", dec.Pools, tc.wantPools)
				}
			}
		})
	}
}

// TestPaceGateConfigOfDefaults proves the planner resolves a missing quota
// section (or pace_gate block) to the documented defaults, and honors an
// explicit disabled gate.
func TestPaceGateConfigOfDefaults(t *testing.T) {
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
		"plain":    {},
		"off":      {Quota: &policy.QuotaConfig{Adapter: "codex", PaceGate: policy.PaceGateConfig{Enabled: false, Threshold: 1.0}}},
		"lenient":  {Quota: &policy.QuotaConfig{Adapter: "codex", PaceGate: policy.PaceGateConfig{Enabled: true, Threshold: 1.5}}},
		"handmade": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	if enabled, threshold := paceGateConfigOf(desired, "plain"); !enabled || threshold != policy.DefaultPaceGateThreshold {
		t.Fatalf("plain = %v/%v, want on at the documented default", enabled, threshold)
	}
	if enabled, _ := paceGateConfigOf(desired, "off"); enabled {
		t.Fatalf("explicit disabled gate resolved as enabled")
	}
	if _, threshold := paceGateConfigOf(desired, "lenient"); threshold != 1.5 {
		t.Fatalf("lenient threshold = %v, want 1.5", threshold)
	}
	if enabled, threshold := paceGateConfigOf(desired, "handmade"); !enabled || threshold != policy.DefaultPaceGateThreshold {
		t.Fatalf("handmade = %v/%v, want the documented defaults", enabled, threshold)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPlanProviderGatePaceDecisionTable proves the planner's pace precedence:
// engage (disable + pace-attributed claim), hold (intact claim, no bytes),
// transfer (a recovered reserve claim moves to the pace axis in place,
// preserving the operator baseline), pool-skip release (through the normal
// restore branch), conflict (operator re-enable), and degraded-evidence
// release (the verdict no longer gates).
func TestPlanProviderGatePaceDecisionTable(t *testing.T) {
	const rev = 9
	hot := map[string]routing.PaceGateVerdict{
		"gp": {Pace: fptr64(1.36), Gated: true, Reason: "pace 136% >= threshold 100%"},
		"pp": {Pace: fptr64(0.4), Gated: false, Reason: "pace 40% under threshold 100%"},
	}
	normal := map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}}
	configTrue := []byte("providers:\n  gp:\n    enabled: true\n  zz:\n    enabled: true\n")
	configFalse := []byte("providers:\n  gp:\n    enabled: false\n  zz:\n    enabled: true\n")

	cases := []struct {
		name string
		// inputs
		modes    map[string]state.ProviderState
		cfg      []byte
		own      map[string]state.ProviderOwnership
		verdicts map[string]routing.PaceGateVerdict
		// expectations
		disable, restore, conflict bool
		claim                      *state.ProviderOwnership // expected published claim
		claimGone                  bool
		wantAction                 string
	}{
		{
			name: "fresh over-threshold pace engages and claims the baseline",
			modes: normal, cfg: configTrue, verdicts: hot,
			disable: true, claim: &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: rev},
			wantAction: GateActionDisabled,
		},
		{
			name: "intact pace claim holds without editing",
			modes: normal, cfg: configFalse,
			own:      map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: 4}},
			verdicts: hot,
			claim:    &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: 4},
			wantAction: GateActionHeld,
		},
		{
			name: "recovered reserve claim transfers to the pace axis in place",
			modes: normal, cfg: configFalse,
			own:      map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 4}},
			verdicts: hot,
			claim:    &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: rev},
			wantAction: GateActionHeld,
		},
		{
			name: "operator re-enable over a pace claim conflicts",
			modes: normal, cfg: configTrue,
			own:      map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: 4}},
			verdicts: hot,
			conflict: true,
			wantAction: GateActionConflict,
		},
		{
			name: "under-threshold evidence releases the pace claim by restoring the baseline",
			modes: normal, cfg: configFalse,
			own:      map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: 4}},
			verdicts: map[string]routing.PaceGateVerdict{"gp": hot["pp"]},
			restore:  true, claimGone: true,
			wantAction: GateActionRestored,
		},
		{
			name: "missing verdict never gates",
			modes: normal, cfg: configTrue, verdicts: map[string]routing.PaceGateVerdict{},
			wantAction: GateActionUnchanged,
		},
		{
			name: "operator-held field claims nothing even when pace is hot",
			modes: normal, cfg: configFalse, verdicts: hot,
			wantAction: GateActionUnchanged,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case enrolls a second live provider with a not-gated
			// verdict: a lone enrolled provider is a singleton pool, and the
			// pool rule deliberately never pace-gates singletons.
			desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
				"gp": {},
				"zz": {},
			}}
			modes := map[string]state.ProviderState{"zz": {Quota: state.QuotaNormal, Availability: state.Available}}
			for id, ps := range tc.modes {
				modes[id] = ps
			}
			verdicts := map[string]routing.PaceGateVerdict{"zz": hot["pp"]}
			for id, v := range tc.verdicts {
				verdicts[id] = v
			}
			observed := state.State{Revision: 8, Providers: modes, ProviderOwnership: tc.own}
			plan, err := planProviderGate(desired, observed, tc.cfg, time.Time{}, verdicts, rev)
			if err != nil {
				t.Fatalf("planProviderGate: %v", err)
			}
			if tc.disable != (len(plan.Disables) == 1) {
				t.Fatalf("disables=%v want disable=%v", plan.Disables, tc.disable)
			}
			if tc.restore != (len(plan.Restores) == 1) {
				t.Fatalf("restores=%v want restore=%v", plan.Restores, tc.restore)
			}
			if tc.conflict != (len(plan.Conflicts) == 1) {
				t.Fatalf("conflicts=%v want conflict=%v", plan.Conflicts, tc.conflict)
			}
			if tc.claim != nil {
				got, ok := plan.PublishedOwnership["gp"]
				if !ok || got != *tc.claim {
					t.Fatalf("published ownership=%+v want %+v", got, *tc.claim)
				}
				// A change to a PRE-EXISTING claim (the transfer) must ride a
				// refusal so conflict markers stay consistent; a brand-new
				// claim must never persist without its bytes.
				if _, existed := tc.own["gp"]; existed {
					if _, held := plan.RefusalOwnership["gp"]; !held {
						t.Fatalf("attribution change must ride a refusal too: %+v", plan.RefusalOwnership)
					}
				} else if _, held := plan.RefusalOwnership["gp"]; held {
					t.Fatalf("refusal ownership carries a new claim: %+v", plan.RefusalOwnership)
				}
			}
			if tc.claimGone {
				if _, held := plan.PublishedOwnership["gp"]; held {
					t.Fatalf("claim not released: %+v", plan.PublishedOwnership)
				}
			}
			if len(plan.GateSummary) != 2 {
				t.Fatalf("gate summary rows=%d want 2: %+v", len(plan.GateSummary), plan.GateSummary)
			}
			var row *ProviderGateSummary
			for i := range plan.GateSummary {
				if plan.GateSummary[i].Provider == "gp" {
					row = &plan.GateSummary[i]
				}
			}
			if row == nil {
				t.Fatalf("gp summary row missing: %+v", plan.GateSummary)
			}
			if row.Action != tc.wantAction {
				t.Fatalf("summary action=%q want %q (row %+v)", row.Action, tc.wantAction, row)
			}
		})
	}
}

// fptr64 is a local float pointer helper (routing's fptr is test-package
// private).
func fptr64(v float64) *float64 { return &v }
