package service

// Signal-gate unit tests (approved amendment: gating keys off the
// use-it-or-lose-it routing signal): the pure all-hot pool rule table and the
// planner's signal decision table (engage, hold, transfer, pool-skip release,
// conflict, degraded-evidence release). All inputs are synthetic values; no
// filesystem, state store, or clock is involved in the pure table.

import (
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func signalVerdict(gated bool) routing.SignalGateVerdict {
	s := -0.72
	return routing.SignalGateVerdict{MappingID: "", Signal: &s, Gated: gated, Reason: "test"}
}

// TestSignalPoolRuleTable proves the all-hot pool escape hatch: a pool is
// skipped exactly when its proposed signal-gate set covers every member that
// would otherwise remain enabled (operator-held and reserve/disabled-gated
// members never count as enabled), and signal-held claims in a skipped pool
// are released unless another axis still gates them. The quota exemption
// column carries the quota-gate exemption set: it lifts only the mode switch
// in coverage, and only the clamped reserve in release, so an exempt member
// never masks a signal-hot sibling's gate and its own claim still releases
// while an operator disable keeps holding.
func TestSignalPoolRuleTable(t *testing.T) {
	hot := map[string]routing.SignalGateVerdict{
		"a": signalVerdict(true),
		"b": signalVerdict(false), // surplus / uncomputable signal
		"c": signalVerdict(true),
		"d": signalVerdict(true),
	}
	reserve := state.ModeReserve
	disabled := state.ModeDisabled
	cases := []struct {
		name         string
		enrolled     []string
		groups       map[string]string // provider -> balance group
		liveEnabled  map[string]bool   // absent = live enabled
		modes        map[string]state.Mode
		signalHeld   []string
		exempt       []string
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
			name:        "uncomputable-signal member keeps the pool open (no skip)",
			enrolled:    []string{"a", "b"},
			wantSkipped: nil,
			wantPools:   nil,
		},
		{
			name:         "singleton pool is never signal-gated and releases its held claim",
			enrolled:     []string{"a"},
			signalHeld:   []string{"a"},
			wantSkipped:  []string{"a"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:         "all-hot pool releases signal-held claims",
			enrolled:     []string{"a", "c"},
			signalHeld:   []string{"a"},
			wantSkipped:  []string{"a", "c"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:         "reserve-mode signal claim in a skipped pool stays held",
			enrolled:     []string{"a", "c"},
			modes:        map[string]state.Mode{"a": reserve},
			signalHeld:   []string{"a"},
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
		// --- quota-gate exemption cases (AC.5) ---
		{
			name:        "exhausted exempt member counts as enabled so the signal-hot sibling still gates",
			enrolled:    []string{"a", "b"},
			modes:       map[string]state.Mode{"b": reserve},
			exempt:      []string{"b"},
			wantSkipped: nil,
			wantPools:   nil,
		},
		{
			name:         "exempt singleton with a clamped reserve behaves exactly like the healthy singleton",
			enrolled:     []string{"a"},
			modes:        map[string]state.Mode{"a": reserve},
			signalHeld:   []string{"a"},
			exempt:       []string{"a"},
			wantSkipped:  []string{"a"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:         "a skipped pool releases the exempt member's own held signal claim",
			enrolled:     []string{"a", "c"},
			modes:        map[string]state.Mode{"a": reserve},
			signalHeld:   []string{"a"},
			exempt:       []string{"a"},
			wantSkipped:  []string{"a", "c"},
			wantReleased: []string{"a"},
			wantPools:    []string{"default"},
		},
		{
			name:        "operator-held exempt member does not count as enabled and the hot sibling is still spared",
			enrolled:    []string{"a", "b"},
			liveEnabled: map[string]bool{"b": false},
			exempt:      []string{"b"},
			wantSkipped: []string{"a"},
			wantPools:   []string{"default"},
		},
		{
			name:         "operator-disabled exempt member keeps its held claim when the pool skips",
			enrolled:     []string{"a", "c"},
			liveEnabled:  map[string]bool{"c": false},
			modes:        map[string]state.Mode{"c": disabled},
			signalHeld:   []string{"c"},
			exempt:       []string{"c"},
			wantSkipped:  []string{"a", "c"},
			wantReleased: nil,
			wantPools:    []string{"default"},
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
			for _, id := range tc.signalHeld {
				held[id] = true
			}
			exempt := map[string]bool{}
			for _, id := range tc.exempt {
				exempt[id] = true
			}
			dec := signalPoolRule(desired, hot, live, modes, held, exempt)
			if len(dec.SkipSignal) != len(tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", keysOf(dec.SkipSignal), tc.wantSkipped)
			}
			for _, id := range tc.wantSkipped {
				if !dec.SkipSignal[id] {
					t.Fatalf("skipped = %v, want %v to be skipped", keysOf(dec.SkipSignal), tc.wantSkipped)
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

// TestSignalGateConfigOfDefaults proves the planner resolves a missing quota
// section (or signal_gate block) to the documented defaults, and honors an
// explicit disabled gate.
func TestSignalGateConfigOfDefaults(t *testing.T) {
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
		"plain":    {},
		"off":      {Quota: &policy.QuotaConfig{Adapter: "codex", SignalGate: policy.SignalGateConfig{Disabled: true, Threshold: 0}}},
		"lenient":  {Quota: &policy.QuotaConfig{Adapter: "codex", SignalGate: policy.SignalGateConfig{Threshold: -0.5}}},
		"handmade": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	if enabled, threshold := signalGateConfigOf(desired, "plain"); !enabled || threshold != policy.DefaultSignalGateThreshold {
		t.Fatalf("plain = %v/%v, want on at the documented default", enabled, threshold)
	}
	if enabled, _ := signalGateConfigOf(desired, "off"); enabled {
		t.Fatalf("explicit disabled gate resolved as enabled")
	}
	if _, threshold := signalGateConfigOf(desired, "lenient"); threshold != -0.5 {
		t.Fatalf("lenient threshold = %v, want -0.5", threshold)
	}
	if enabled, threshold := signalGateConfigOf(desired, "handmade"); !enabled || threshold != policy.DefaultSignalGateThreshold {
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

// TestPlanProviderGateSignalDecisionTable proves the planner's signal
// precedence: engage (disable + signal-attributed claim), hold (intact claim,
// no bytes), transfer (a recovered reserve claim moves to the signal axis in
// place, preserving the operator baseline), pool-skip release (through the
// normal restore branch), conflict (operator re-enable), and
// degraded-evidence release (the verdict no longer gates).
func TestPlanProviderGateSignalDecisionTable(t *testing.T) {
	const rev = 9
	hot := map[string]routing.SignalGateVerdict{
		"gp": {Signal: fptr64(-0.72), Gated: true, Reason: "signal -0.72 <= threshold +0.00"},
		"pp": {Signal: fptr64(1.60), Gated: false, Reason: "signal +1.60 above threshold +0.00"},
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
		verdicts map[string]routing.SignalGateVerdict
		// expectations
		disable, restore, conflict bool
		claim                      *state.ProviderOwnership // expected published claim
		claimGone                  bool
		wantAction                 string
	}{
		{
			name:  "fresh projected-exhaustion signal engages and claims the baseline",
			modes: normal, cfg: configTrue, verdicts: hot,
			disable: true, claim: &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: rev},
			wantAction: GateActionDisabled,
		},
		{
			name:  "intact signal claim holds without editing",
			modes: normal, cfg: configFalse,
			own:        map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 4}},
			verdicts:   hot,
			claim:      &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 4},
			wantAction: GateActionHeld,
		},
		{
			name:  "recovered reserve claim transfers to the signal axis in place",
			modes: normal, cfg: configFalse,
			own:        map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisReserve, EngagedRevision: 4}},
			verdicts:   hot,
			claim:      &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: rev},
			wantAction: GateActionHeld,
		},
		{
			name:  "operator re-enable over a signal claim conflicts",
			modes: normal, cfg: configTrue,
			own:        map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 4}},
			verdicts:   hot,
			conflict:   true,
			wantAction: GateActionConflict,
		},
		{
			name:  "surplus signal releases the signal claim by restoring the baseline",
			modes: normal, cfg: configFalse,
			own:      map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 4}},
			verdicts: map[string]routing.SignalGateVerdict{"gp": hot["pp"]},
			restore:  true, claimGone: true,
			wantAction: GateActionRestored,
		},
		{
			name:  "missing verdict never gates",
			modes: normal, cfg: configTrue, verdicts: map[string]routing.SignalGateVerdict{},
			wantAction: GateActionUnchanged,
		},
		{
			name:  "operator-held field claims nothing even when the signal gates",
			modes: normal, cfg: configFalse, verdicts: hot,
			wantAction: GateActionUnchanged,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case enrolls a second live provider with a not-gated
			// verdict: a lone enrolled provider is a singleton pool, and the
			// pool rule deliberately never signal-gates singletons.
			desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{
				"gp": {},
				"zz": {},
			}}
			modes := map[string]state.ProviderState{"zz": {Quota: state.QuotaNormal, Availability: state.Available}}
			for id, ps := range tc.modes {
				modes[id] = ps
			}
			verdicts := map[string]routing.SignalGateVerdict{"zz": hot["pp"]}
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

// TestProviderProjectionCarriesGateAttribution proves the diagnostic provider
// projection surfaces the gate record: a signal claim carries axis, observed
// signal, threshold, and engaged revision; a legacy claim (no axis) keeps
// today's presentation with no gate field.
func TestProviderProjectionCarriesGateAttribution(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	snapshot := &quota.QuotaSnapshot{
		CheckedAt: asOf.Add(-time.Minute),
		Windows: []quota.QuotaWindow{{
			Used: fptr64(68), Limit: fptr64(100),
			ResetAt: tptr64(asOf.Add(24 * time.Hour)), Period: durptr64(48 * time.Hour),
		}},
	}
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{"gp": {}}}
	observed := state.State{
		Providers: map[string]state.ProviderState{"gp": {QuotaSnapshot: snapshot}},
		ProviderOwnership: map[string]state.ProviderOwnership{
			"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: -0.25, EngagedRevision: 7},
		},
	}
	providers, errs := projectProviders(desired, observed, asOf)
	if len(errs) != 0 {
		t.Fatalf("projection errors: %v", errs)
	}
	gate := providers[0].Gate
	if gate == nil || gate.Axis != state.OwnershipAxisSignal || gate.EngagedRevision != 7 {
		t.Fatalf("gate = %+v, want signal attribution at revision 7", gate)
	}
	if gate.Signal == nil || routing.SignalFormat(*gate.Signal) != "-0.72" {
		t.Fatalf("gate signal = %+v, want -0.72", gate.Signal)
	}
	if gate.Threshold == nil || *gate.Threshold != -0.25 {
		t.Fatalf("gate threshold = %+v, want -0.25", gate.Threshold)
	}

	// Legacy claim: no axis, no gate field.
	observed.ProviderOwnership["gp"] = state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true}
	providers, _ = projectProviders(desired, observed, asOf)
	if providers[0].Gate != nil {
		t.Fatalf("legacy claim must not carry a gate field: %+v", providers[0].Gate)
	}
}

// TestMergedStatusSignalReasonLeads proves a signal-held gate names itself
// ahead of the ranking explanation in the merged status REASON, and the gate
// attribution rides the row for status --json.
func TestMergedStatusSignalReasonLeads(t *testing.T) {
	snap := DiagnosticSnapshot{
		providers: []ProviderProjection{{
			MappingID: "gp",
			Gate: &GateReport{
				Axis: state.OwnershipAxisSignal, Signal: fptr64(-0.72), Threshold: fptr64(0), EngagedRevision: 7,
			},
		}},
		ranks: []RankEntryReport{{MappingID: "gp", Rank: 0, Eligible: true, Explanation: "peak, signal -0.72"}},
	}
	report := snap.MergedStatusView()
	if len(report.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(report.Providers))
	}
	row := report.Providers[0]
	if row.Status != StatusGated {
		t.Fatalf("status = %q, want %q for a signal-held gate", row.Status, StatusGated)
	}
	if !strings.HasPrefix(row.Reason, "signal-gated (-0.72 <= +0.00); ") {
		t.Fatalf("reason = %q, want the signal attribution to lead", row.Reason)
	}
	if !strings.HasSuffix(row.Reason, "peak, signal -0.72") {
		t.Fatalf("reason = %q, want the ranking explanation to follow", row.Reason)
	}
	if row.Gate == nil || row.Gate.Axis != state.OwnershipAxisSignal || row.Gate.EngagedRevision != 7 {
		t.Fatalf("gate = %+v, want the signal attribution carried", row.Gate)
	}
}

// TestProviderEditsCarrySignalNoticeReason proves a committed signal disable's
// edit carries the sanitized signal reason for the notice, and reserve edits
// carry none.
func TestProviderEditsCarrySignalNoticeReason(t *testing.T) {
	off := false
	outcomes := []TargetOutcome{{
		Prepare: &PrepareResult{ChangedEdits: []reconcile.FieldEdit{
			{File: "config.yaml", Path: []string{"providers", "gp", "enabled"}, Enabled: &off},
			{File: "config.yaml", Path: []string{"providers", "pp", "enabled"}, Enabled: &off},
		}},
		ProviderGates: []ProviderGateSummary{
			{Provider: "gp", Action: GateActionDisabled, Axis: state.OwnershipAxisSignal, Signal: fptr64(-0.72), Threshold: fptr64(0)},
			{Provider: "pp", Action: GateActionDisabled, Axis: state.OwnershipAxisReserve},
		},
	}}
	edits := providerEdits(outcomes)
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want 2", len(edits))
	}
	for _, e := range edits {
		switch e.id {
		case "gp":
			if e.reason != "signal-gated (-0.72 <= +0.00)" {
				t.Fatalf("gp reason = %q, want the signal attribution", e.reason)
			}
		case "pp":
			if e.reason != "" {
				t.Fatalf("pp reason = %q, want none for the reserve axis", e.reason)
			}
		}
	}
}

// tptr64/durptr64 are local pointer helpers (routing's are test-package
// private).
func tptr64(t time.Time) *time.Time           { return &t }
func durptr64(d time.Duration) *time.Duration { return &d }

// TestAppendSignalGateEventsRecordsTransitions proves the durable event
// timeline records signal_gated where a signal claim appears and
// signal_recovered where it disappears, with the pass's sanitized reason, and
// records nothing when no signal claim changed.
func TestAppendSignalGateEventsRecordsTransitions(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	signalClaim := state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0, EngagedRevision: 8}
	observed := state.State{Revision: 8, ProviderOwnership: map[string]state.ProviderOwnership{"gp": signalClaim}}
	plan := providerGatePlan{
		PublishedOwnership: map[string]state.ProviderOwnership{}, // gp released this pass
		GateSummary: []ProviderGateSummary{
			{Provider: "gp", Action: GateActionRestored, Detail: "signal +1.60 above threshold +0.00"},
		},
	}
	next := appendSignalGateEvents(observed, observed, plan, 9, now)
	if len(next.EventHistory.Events) != 1 {
		t.Fatalf("events = %+v, want one signal_recovered", next.EventHistory.Events)
	}
	e := next.EventHistory.Events[0]
	if e.Action != "signal_recovered" || e.MappingID != "gp" || e.Revision != 9 || e.Result != state.EventChanged {
		t.Fatalf("event = %+v, want a signal_recovered transition for gp at revision 9", e)
	}
	if e.Reason != "signal +1.60 above threshold +0.00" {
		t.Fatalf("event reason = %q, want the recovery reason", e.Reason)
	}

	// Engage: the claim appears where none was held. The published map always
	// carries every claim kept this pass, so gp's claim rides along unchanged.
	plan2 := providerGatePlan{
		PublishedOwnership: map[string]state.ProviderOwnership{"gp": signalClaim, "pp": signalClaim},
		GateSummary: []ProviderGateSummary{
			{Provider: "pp", Action: GateActionDisabled, Axis: state.OwnershipAxisSignal, Detail: "signal -0.72 <= threshold +0.00"},
		},
	}
	applied2 := observed
	applied2.ProviderOwnership = plan2.PublishedOwnership
	next2 := appendSignalGateEvents(applied2, observed, plan2, 9, now)
	if len(next2.EventHistory.Events) != 1 {
		t.Fatalf("events = %+v, want one signal_gated", next2.EventHistory.Events)
	}
	e2 := next2.EventHistory.Events[0]
	if e2.Action != "signal_gated" || e2.MappingID != "pp" || e2.Reason != "signal -0.72 <= threshold +0.00" {
		t.Fatalf("event = %+v, want a signal_gated transition for pp with its reason", e2)
	}

	// Unchanged claims record nothing.
	plan3 := providerGatePlan{PublishedOwnership: map[string]state.ProviderOwnership{"gp": signalClaim}}
	applied3 := observed
	applied3.ProviderOwnership = plan3.PublishedOwnership
	next3 := appendSignalGateEvents(applied3, observed, plan3, 9, now)
	if len(next3.EventHistory.Events) != 0 {
		t.Fatalf("events = %+v, want none for an unchanged claim", next3.EventHistory.Events)
	}
}

// TestHistoryProviderDetailGateAttributionSanitized proves the record
// projection carries the gate axis through sanitization, and an unknown axis
// degrades to the legacy axis with its attribution cleared.
func TestHistoryProviderDetailGateAttributionSanitized(t *testing.T) {
	tpl := state.RecordTemplate{Revision: 3, Providers: []state.ProviderDetail{
		{MappingID: "gp", Mode: state.ModeNormal, GateAxis: state.OwnershipAxisSignal, GateThreshold: -0.25, GateEngagedRevision: 7},
		{MappingID: "pp", Mode: state.ModeReserve, GateAxis: state.OwnershipAxisReserve},
		{MappingID: "zz", Mode: state.ModeNormal, GateAxis: "bogus", GateThreshold: 9, GateEngagedRevision: 12},
	}}
	out := state.SanitizeRecordTemplate(tpl)
	if len(out.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(out.Providers))
	}
	if got := out.Providers[0]; got.GateAxis != state.OwnershipAxisSignal || got.GateThreshold != -0.25 || got.GateEngagedRevision != 7 {
		t.Fatalf("gp = %+v, want signal attribution preserved", got)
	}
	if got := out.Providers[1]; got.GateAxis != state.OwnershipAxisReserve || got.GateThreshold != 0 || got.GateEngagedRevision != 0 {
		t.Fatalf("pp = %+v, want reserve attribution without signal fields", got)
	}
	if got := out.Providers[2]; got.GateAxis != state.OwnershipAxisLegacy || got.GateThreshold != 0 || got.GateEngagedRevision != 0 {
		t.Fatalf("zz = %+v, want unknown axis degraded to legacy", got)
	}
}
