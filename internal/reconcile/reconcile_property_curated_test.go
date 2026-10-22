package reconcile

// Curated deterministic cases for the reconcile property suite: fixed
// combinations chosen to demonstrate each documented behavior class exactly
// once in readable form — typed empty-chain errors (including the
// discard-on-error and model-group skip interplay), unmanaged chains and
// project scope, model-group skips alongside definition and models-block
// edits, the sorted global models block honoring baseline and mode, preserved
// duplicate occurrences, corrupt/sparse/manual observation classes, poll
// snapshot boundaries, routing overlay slot invariants, and editor
// convergence. Every case also compares the whole plan against the
// independent model, so the focused assertions document intent while the
// oracle guards exactness.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ---------------------------------------------------------------------------
// Small curated-world builders

func propMapping(enabled map[string]bool, quotaCfg *policy.QuotaConfig) policy.Mapping {
	models := make(map[string]policy.ModelBaseline, len(enabled))
	for base, on := range enabled {
		models[base] = policy.ModelBaseline{Enabled: on, HadEnabledKey: true}
	}
	return policy.Mapping{Models: models, Quota: quotaCfg}
}

func propQuotaCfg(group string, exempt bool) *policy.QuotaConfig {
	return &policy.QuotaConfig{
		Adapter:      "codex",
		FreshnessTTL: 30 * time.Minute,
		Weight:       1,
		BalanceGroup: group,
		Gate:         policy.QuotaGateConfig{Disabled: exempt},
	}
}

func propSetAxes(s *state.State, id string, q state.Quota, a state.Availability, manual bool) {
	if s.Providers == nil {
		s.Providers = map[string]state.ProviderState{}
	}
	s.Providers[id] = state.ProviderState{
		Quota: q, Availability: a, ManualDisabled: manual,
		QuotaAt: propBaseTime, QuotaSnapshot: propFreshHalfSnapshot(),
	}
}

func propFreshHalfSnapshot() *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
		Windows: []quota.QuotaWindow{{Used: propFloat(50), Limit: propFloat(100)}}}
}

// propDefProjection extracts the polytoken.model scalar and the
// fallback_models projection for one definition file.
func propDefProjection(t *testing.T, plan Plan, file string) (string, []string, bool) {
	t.Helper()
	primary := ""
	var fallback []string
	has := false
	for _, fe := range plan.Edits {
		if fe.File != file || len(fe.Path) != 2 || fe.Path[0] != "polytoken" {
			continue
		}
		switch fe.Path[1] {
		case "model":
			if fe.Scalar == nil {
				t.Fatalf("definition %s: polytoken.model edit is not scalar", file)
			}
			primary = *fe.Scalar
		case "fallback_models":
			if fe.Remove {
				has = false
				fallback = nil
			} else {
				fallback, has = fe.Sequence, true
			}
		}
	}
	return primary, fallback, has
}

// propEnabledMap extracts the models-block enabled edits keyed by base.
func propEnabledMap(t *testing.T, plan Plan) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, fe := range plan.Edits {
		if fe.File == configFile && len(fe.Path) == 3 && fe.Path[0] == "models" && fe.Path[2] == "enabled" && fe.Enabled != nil {
			out[fe.Path[1]] = *fe.Enabled
		}
	}
	return out
}

func propMustBuild(t *testing.T, w propWorld) Plan {
	t.Helper()
	plan, err := Build(w.desired, w.observed, w.target, w.ranks)
	if err != nil {
		t.Fatalf("curated world unexpectedly rejected: %v", err)
	}
	propComparePlan(t, w, plan, err)
	return plan
}

// propCheckSurvival builds the single-provider world and asserts whether the
// provider's model survives the chain (non-empty polytoken.model) and whether
// its models-block flag matches: dead rows must fail with the typed
// no-survivor error, an empty plan, and enabled=false.
func propCheckSurvival(t *testing.T, w propWorld, alive bool) {
	t.Helper()
	plan, err := Build(w.desired, w.observed, w.target, w.ranks)
	if err != nil {
		var ec EmptyChainError
		if !errors.As(err, &ec) {
			t.Fatalf("unexpected rejection: %v", err)
		}
		propComparePlan(t, w, plan, err)
		if alive {
			t.Fatalf("expected a survivor, got %v", err)
		}
		if len(plan.Edits) != 0 || len(plan.Skipped) != 0 {
			t.Fatalf("dead row must produce no edits: %+#v", plan)
		}
		if got := propEnabledMap(t, plan)["p0/m"]; got {
			t.Fatalf("dead provider left enabled")
		}
		return
	}
	propComparePlan(t, w, plan, err)
	if !alive {
		t.Fatalf("expected the typed no-survivor error, chain survived")
	}
	primary, _, _ := propDefProjection(t, plan, "agent.md")
	if primary == "" {
		t.Fatalf("expected a survivor, definition edit missing")
	}
	if got := propEnabledMap(t, plan)["p0/m"]; got != alive {
		t.Fatalf("models enabled = %t, want %t", got, alive)
	}
}

// ---------------------------------------------------------------------------
// Typed empty-chain errors

// TestReconcileTypedErrorDiscardsPlan pins: a no-survivor required chain
// returns the typed error naming target, file, and field; the returned plan is
// empty (no edits, no skipped entries — even skips accumulated before the
// failure are discarded); and managed scalar fields are evaluated before
// definitions, so the first failing tier field wins.
func TestReconcileTypedErrorDiscardsPlan(t *testing.T) {
	// A: scalar chain fails first and names the tier field.
	w := propWorld{
		desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"p0": propMapping(map[string]bool{"p0/m": true}, nil),
		}},
		observed: func() state.State {
			s := state.State{Revision: 41}
			propSetAxes(&s, "p0", state.QuotaNormal, state.Unavailable, false)
			return s
		}(),
		target: policy.Target{
			ID:         "tgt-a",
			Root:       "/synthetic/root",
			Global:     true,
			Full:       policy.Chain{"p0/m"},
			Mini:       policy.Chain{"p0/m"},
			Classifier: policy.Chain{"p0/m"},
			Definitions: []policy.Definition{
				{Path: "agent.md", Chain: policy.Chain{"p0/m"}},
			},
		},
	}
	plan, err := Build(w.desired, w.observed, w.target, nil)
	propComparePlan(t, w, plan, err)
	var ec *EmptyChainError
	if err == nil || !asEmptyChain(err, &ec) {
		t.Fatalf("expected EmptyChainError, got %v", err)
	}
	if ec.TargetID != "tgt-a" || ec.File != configFile || ec.Field != "defaults.full" {
		t.Fatalf("typed error names the wrong chain: %+#v", ec)
	}
	if len(plan.Edits) != 0 || len(plan.Skipped) != 0 || plan.Revision != 41 || plan.TargetID != "tgt-a" {
		t.Fatalf("error plan is not the empty stamped plan: %+#v", plan)
	}

	// B: a modelgroup-stamped target accumulates skips, but a failing
	// definition discards them and names the definition file/field.
	wB := w
	wB.target = policy.Target{
		ID: "tgt-b", Root: "/synthetic/root", Global: true, UsesModelGroups: true,
		Full: policy.Chain{"p0/m"},
		Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p0/m"}}},
	}
	planB, errB := Build(wB.desired, wB.observed, wB.target, nil)
	propComparePlan(t, wB, planB, errB)
	if errB == nil || !asEmptyChain(errB, &ec) {
		t.Fatalf("expected EmptyChainError for definition chain, got %v", errB)
	}
	if ec.File != "agent.md" || ec.Field != "polytoken.model" {
		t.Fatalf("definition failure names wrong file/field: %+#v", ec)
	}
	if len(planB.Skipped) != 0 || len(planB.Edits) != 0 {
		t.Fatalf("error plan must discard skipped entries: %+#v", planB.Skipped)
	}
}

func asEmptyChain(err error, target **EmptyChainError) bool {
	ec, ok := err.(EmptyChainError)
	if ok {
		*target = &ec
		return true
	}
	wrapped, ok := err.(*EmptyChainError)
	if ok {
		*target = wrapped
	}
	return ok
}

// TestReconcileUnmanagedChainsAndProjectScope pins: empty chains are
// unmanaged (no edits, no skips, no error); project targets never emit
// models-block edits; and a fully empty project plan is the zero-edit stamped
// plan.
func TestReconcileUnmanagedChainsAndProjectScope(t *testing.T) {
	providers := map[policy.MappingID]policy.Mapping{
		"p0": propMapping(map[string]bool{"p0/m": true}, nil),
		"p1": propMapping(map[string]bool{"p1/m": false}, nil),
	}
	observed := state.State{Revision: 9}

	// A: global target with no chains at all still carries the models block.
	wGlobal := propWorld{
		desired:  policy.Desired{Version: 1, Providers: providers},
		observed: observed,
		target:   policy.Target{ID: "g", Root: "/synthetic/root", Global: true},
	}
	plan := propMustBuild(t, wGlobal)
	if len(plan.Edits) != 2 {
		t.Fatalf("expected exactly the two models-block edits, got %+#v", plan.Edits)
	}
	if got := propEnabledMap(t, plan); !got["p0/m"] || got["p1/m"] {
		t.Fatalf("models block must honor baselines when healthy: %v", got)
	}

	// B: project target with managed chains emits no models edits.
	wProject := propWorld{
		desired:  policy.Desired{Version: 1, Providers: providers},
		observed: observed,
		target: policy.Target{
			ID: "proj", Root: "/synthetic/root",
			Full: policy.Chain{"p0/m", "p1/m"},
			Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p1/m", "p0/m"}}},
		},
	}
	planProject := propMustBuild(t, wProject)
	for _, fe := range planProject.Edits {
		if len(fe.Path) == 3 && fe.Path[0] == "models" {
			t.Fatalf("project target emitted a models-block edit: %+#v", fe)
		}
	}
	// Baseline-disabled p1/m is not a survivor anywhere.
	if primary, fallback, _ := propDefProjection(t, planProject, "agent.md"); primary != "p0/m" || len(fallback) != 0 {
		t.Fatalf("baseline-disabled model survived: primary=%q fallback=%v", primary, fallback)
	}

	// C: project target with no chains at all is a zero-edit stamped plan.
	wEmpty := propWorld{
		desired:  policy.Desired{Version: 1, Providers: providers},
		observed: observed,
		target:   policy.Target{ID: "proj-empty", Root: "/synthetic/root"},
	}
	planEmpty := propMustBuild(t, wEmpty)
	if len(planEmpty.Edits) != 0 || len(planEmpty.Skipped) != 0 || planEmpty.Revision != 9 {
		t.Fatalf("expected zero-edit plan, got %+#v", planEmpty)
	}
}

// TestReconcileModelGroupsSkipsTiersKeepsRest pins the modelgroups stamp: the
// managed tier fields are skipped with the sanitized reason in order, while
// definition edits and the global models block are unaffected.
func TestReconcileModelGroupsSkipsTiersKeepsRest(t *testing.T) {
	w := propWorld{
		desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"p0": propMapping(map[string]bool{"p0/m": true}, nil),
		}},
		observed: state.State{Revision: 3},
		target: policy.Target{
			ID: "g", Root: "/synthetic/root", Global: true, UsesModelGroups: true,
			Full:  policy.Chain{"p0/m"},
			Mini:  policy.Chain{},
			Nano:  policy.Chain{"p0/m"},
			Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p0/m"}}},
		},
	}
	plan := propMustBuild(t, w)
	wantSkipped := []SkippedEdit{
		{Field: "defaults.full", Reason: modelGroupsSkipReason},
		{Field: "defaults.nano", Reason: modelGroupsSkipReason},
	}
	if !reflect.DeepEqual(plan.Skipped, wantSkipped) {
		t.Fatalf("skipped = %+#v, want %+#v (empty chain stays unmanaged)", plan.Skipped, wantSkipped)
	}
	primary, _, _ := propDefProjection(t, plan, "agent.md")
	if primary != "p0/m" {
		t.Fatalf("modelgroups suppressed the definition edit: primary=%q", primary)
	}
	if got := propEnabledMap(t, plan); !got["p0/m"] {
		t.Fatalf("modelgroups suppressed the models block: %v", got)
	}
	for _, fe := range plan.Edits {
		if fe.File == configFile && len(fe.Path) == 2 {
			t.Fatalf("tier field escaped the skip: %+#v", fe)
		}
	}
}

// TestReconcileGlobalModelsBlockSortedHonorsBaselineAndMode pins the sorted
// models block: ids and bases in string order, enabled=false for any disabled
// mode (manual, observation, or snapshot boundary), baseline otherwise.
func TestReconcileGlobalModelsBlockSortedHonorsBaselineAndMode(t *testing.T) {
	quotaLow := propQuotaCfg("g1", false)
	quotaExempt := propQuotaCfg("g1", true)
	w := propWorld{
		desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"p0": propMapping(map[string]bool{"p0/b": true, "p0/a": false}, nil),
			"p1": propMapping(map[string]bool{"p1/m": true}, quotaLow),
			"p2": propMapping(map[string]bool{"p2/m": true}, nil),
			"p3": propMapping(map[string]bool{"p3/m": true}, quotaExempt),
		}},
		observed: func() state.State {
			s := state.State{Revision: 17}
			propSetAxes(&s, "p1", state.QuotaLow, state.Available, false)            // reserve
			propSetAxes(&s, "p2", state.QuotaNormal, state.Available, true)          // manual disable
			propSetAxes(&s, "p3", state.QuotaExhausted, state.Available, false)      // exempt exhausted -> reserve
			return s
		}(),
		target: policy.Target{ID: "g", Root: "/synthetic/root", Global: true},
	}
	plan := propMustBuild(t, w)
	want := []FieldEdit{
		{File: configFile, Path: []string{"models", "p0/a", "enabled"}, Enabled: boolPtr(false)}, // baseline disabled
		{File: configFile, Path: []string{"models", "p0/b", "enabled"}, Enabled: boolPtr(true)},
		{File: configFile, Path: []string{"models", "p1/m", "enabled"}, Enabled: boolPtr(true)},  // reserve keeps baseline
		{File: configFile, Path: []string{"models", "p2/m", "enabled"}, Enabled: boolPtr(false)}, // manual disable
		{File: configFile, Path: []string{"models", "p3/m", "enabled"}, Enabled: boolPtr(true)},  // exempt exhausted clamps to reserve
	}
	if !reflect.DeepEqual(plan.Edits, want) {
		t.Fatalf("models block mismatch:\n got: %+#v\nwant: %+#v", plan.Edits, want)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestReconcileDuplicateOccurrencesSurviveInPlace pins the duplicate contract:
// the survivor projection preserves repeated occurrences in desired order (the
// scalar still surfaces only the first), and dropping one occurrence of a
// duplicated baseline-disabled entry drops them all.
func TestReconcileDuplicateOccurrencesSurviveInPlace(t *testing.T) {
	w := propWorld{
		desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"p0": propMapping(map[string]bool{"p0/m": true}, nil),
			"p1": propMapping(map[string]bool{"p1/m": true, "p1/g": false}, nil),
		}},
		observed: state.State{Revision: 5},
		target: policy.Target{
			ID: "d", Root: "/synthetic/root",
			Full:        policy.Chain{"p0/m", "p1/m", "p0/m(high)", "p1/m", "p1/g(med)"},
			Definitions: []policy.Definition{{
				Path:  "agent.md",
				Chain: policy.Chain{"p0/m", "p1/m", "p0/m(high)", "p1/m", "p1/g(med)"},
			}},
		},
	}
	plan := propMustBuild(t, w)
	primary, fallback, has := propDefProjection(t, plan, "agent.md")
	if primary != "p0/m" || !has {
		t.Fatalf("primary = %q (has=%t)", primary, has)
	}
	// The exact surviving occurrences, primary included, preserve duplicates
	// in desired order: p1/g is baseline-disabled and dropped entirely.
	wantProjection := []string{"p0/m", "p1/m", "p0/m(high)", "p1/m"}
	if got := append([]string{primary}, fallback...); !reflect.DeepEqual(got, wantProjection) {
		t.Fatalf("projection = %v, want duplicated occurrences preserved in place: %v", got, wantProjection)
	}
	if scalar := propScalarOf(t, plan, configFile, "defaults", "full"); scalar != "p0/m" {
		t.Fatalf("scalar first survivor = %q", scalar)
	}
}

// propScalarOf extracts a scalar config edit value by path.
func propScalarOf(t *testing.T, plan Plan, file string, path ...string) string {
	t.Helper()
	for _, fe := range plan.Edits {
		if fe.File == file && fe.Scalar != nil && reflect.DeepEqual(fe.Path, path) {
			return *fe.Scalar
		}
	}
	t.Fatalf("no scalar edit for %s:%v in %+#v", file, path, plan.Edits)
	return ""
}

// TestReconcileObservationClassesMatrix pins the observation classes end to
// end through one target: corrupt axes drop the provider and disable its
// models, sparse axes stay healthy, manual disables drop even when exempt, and
// the quota-gate exemption clamps quota-derived causes to reserve.
func TestReconcileObservationClassesMatrix(t *testing.T) {
	cases := []struct {
		name      string
		exempt    bool
		quota     state.Quota
		avail     state.Availability
		manual    bool
		wantAlive bool
	}{
		{"healthy", false, state.QuotaNormal, state.Available, false, true},
		{"sparse axes stay healthy", false, "", "", false, true},
		{"low is reserve", false, state.QuotaLow, state.Available, false, true},
		{"exhausted drops", false, state.QuotaExhausted, state.Available, false, false},
		{"unavailable drops", false, state.QuotaNormal, state.Unavailable, false, false},
		{"corrupt quota drops", false, state.Quota("garbage"), state.Available, false, false},
		{"corrupt availability drops", false, state.QuotaNormal, state.Availability("garbage"), false, false},
		{"manual disable drops", false, state.QuotaNormal, state.Available, true, false},
		{"exempt healthy", true, state.QuotaNormal, state.Available, false, true},
		{"exempt exhausted clamps to reserve", true, state.QuotaExhausted, state.Available, false, true},
		{"exempt unavailable clamps to reserve", true, state.QuotaNormal, state.Unavailable, false, true},
		{"exempt low is reserve", true, state.QuotaLow, state.Available, false, true},
		{"exempt sparse stays healthy", true, "", "", false, true},
		{"exempt manual still drops", true, state.QuotaNormal, state.Available, true, false},
		{"exempt corrupt still drops", true, state.Quota("garbage"), state.Available, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := propWorld{
				desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
					"p0": propMapping(map[string]bool{"p0/m": true}, propQuotaCfg("default", tc.exempt)),
				}},
				observed: func() state.State {
					s := state.State{Revision: 11}
					propSetAxes(&s, "p0", tc.quota, tc.avail, tc.manual)
					return s
				}(),
				target: policy.Target{
					ID: "m", Root: "/synthetic/root", Global: true,
					Full:        policy.Chain{"p0/m"},
					Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p0/m"}}},
				},
			}
			propCheckSurvival(t, w, tc.wantAlive)
		})
	}
}

// TestReconcileSnapshotBoundaryMatrix pins the fail-closed poll boundary: any
// snapshot that is not explicitly available with a positive usable remaining
// fraction disables a non-exempt provider and clamps an exempt one to reserve.
func TestReconcileSnapshotBoundaryMatrix(t *testing.T) {
	snapshots := map[string]*quota.QuotaSnapshot{
		"no poll":       nil,
		"fresh":         {CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Used: propFloat(50), Limit: propFloat(100)}}},
		"zero remaining": {CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Used: propFloat(100), Limit: propFloat(100)}}},
		"overdrawn":     {CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Used: propFloat(120), Limit: propFloat(100)}}},
		"unavailable":   {CheckedAt: propBaseTime, Availability: quota.QuotaUnavailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Used: propFloat(0), Limit: propFloat(100)}}},
		"windowless":    {CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh},
		"empty avail":   {CheckedAt: propBaseTime, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Used: propFloat(50), Limit: propFloat(100)}}},
	}
	for name, snap := range snapshots {
		for _, exempt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exempt=%t", name, exempt), func(t *testing.T) {
				w := propWorld{
					desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
						"p0": propMapping(map[string]bool{"p0/m": true}, propQuotaCfg("default", exempt)),
					}},
					observed: func() state.State {
						s := state.State{Revision: 2}
						propSetAxes(&s, "p0", state.QuotaNormal, state.Available, false)
						ps := s.Providers["p0"]
						ps.QuotaSnapshot = snap
						s.Providers["p0"] = ps
						return s
					}(),
					target: policy.Target{
						ID: "s", Root: "/synthetic/root", Global: true,
						Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p0/m"}}},
					},
				}
				wantAlive := name == "no poll" || name == "fresh" || exempt
				propCheckSurvival(t, w, wantAlive)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Routing overlay slot invariants

func propOverlayWorld(t *testing.T, lowQuotaForG1 bool, ranks RankLookup, routingOn bool) propWorld {
	t.Helper()
	group := func() *policy.QuotaConfig { return propQuotaCfg("g1", false) }
	p0, p1 := group(), group()
	p2 := propQuotaCfg("g2", false)
	w := propWorld{
		desired: policy.Desired{
			Version: 1,
			Providers: map[policy.MappingID]policy.Mapping{
				"p0": propMapping(map[string]bool{"p0/m": true}, p0),
				"p1": propMapping(map[string]bool{"p1/m": true}, p1),
				"p2": propMapping(map[string]bool{"p2/m": true}, p2),
				"p3": propMapping(map[string]bool{"p3/m": true}, nil), // group "default"
			},
			Routing: policy.RoutingConfig{Enabled: routingOn},
		},
		observed: state.State{Revision: 6},
		target: policy.Target{
			ID: "o", Root: "/synthetic/root",
			Definitions: []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"p0/m", "p1/m", "p2/m", "p3/m"}}},
		},
		ranks: ranks,
		bases: []string{"p0/m", "p1/m", "p2/m", "p3/m"},
	}
	if lowQuotaForG1 {
		w.observed.Providers = map[string]state.ProviderState{
			"p0": {Quota: state.QuotaLow, Availability: state.Available, QuotaAt: propBaseTime, QuotaSnapshot: propFreshHalfSnapshot()},
			"p1": {Quota: state.QuotaLow, Availability: state.Available, QuotaAt: propBaseTime, QuotaSnapshot: propFreshHalfSnapshot()},
		}
	}
	return w
}

// TestReconcileOverlayCuratedSlotInvariants pins the overlay's slot rules on a
// fixed four-mapping chain: only ranked same-group slots reorder; unranked and
// inter-group entries keep exact positions; equal ranks and missing ranks keep
// desired order; the reserve partition overlays independently; and
// routing-off is byte-identical to an empty rank lookup.
func TestReconcileOverlayCuratedSlotInvariants(t *testing.T) {
	// projectedOf returns the full survivor projection (primary followed by
	// fallbacks) for the fixed definition chain.
	projectedOf := func(t *testing.T, w propWorld) []string {
		t.Helper()
		plan := propMustBuild(t, w)
		primary, fallback, _ := propDefProjection(t, plan, "agent.md")
		if primary == "" {
			t.Fatalf("definition edit missing")
		}
		return append([]string{primary}, fallback...)
	}

	// Inter-group and unranked entries keep their slot positions; only the
	// g1 slots (0,1) reorder by rank.
	w := propOverlayWorld(t, false, RankLookup{"p1": 0, "p0": 1, "p2": 3}, true)
	if got := projectedOf(t, w); !reflect.DeepEqual(got, []string{"p1/m", "p0/m", "p2/m", "p3/m"}) {
		t.Fatalf("g1 reorder disturbed other slots: %v", got)
	}

	// Equal ranks keep desired order (stable).
	wEq := propOverlayWorld(t, false, RankLookup{"p0": 1, "p1": 1}, true)
	if got := projectedOf(t, wEq); !reflect.DeepEqual(got, []string{"p0/m", "p1/m", "p2/m", "p3/m"}) {
		t.Fatalf("equal ranks reordered entries: %v", got)
	}

	// A single ranked mapping is not a group of two; nothing moves.
	wSingle := propOverlayWorld(t, false, RankLookup{"p0": 0}, true)
	if got := projectedOf(t, wSingle); !reflect.DeepEqual(got, []string{"p0/m", "p1/m", "p2/m", "p3/m"}) {
		t.Fatalf("lone ranked mapping moved: %v", got)
	}

	// The reserve partition overlays within itself, after the normals.
	wReserve := propOverlayWorld(t, true, RankLookup{"p1": 0, "p0": 1}, true)
	if got := projectedOf(t, wReserve); !reflect.DeepEqual(got, []string{"p2/m", "p3/m", "p1/m", "p0/m"}) {
		t.Fatalf("reserve partition overlay wrong: %v", got)
	}

	// Routing off equals empty ranks equals no overlay.
	wOff := propOverlayWorld(t, false, RankLookup{"p1": 0, "p0": 1}, false)
	wEmpty := wOff
	wEmpty.desired.Routing = policy.RoutingConfig{Enabled: true}
	wEmpty.ranks = RankLookup{}
	gotOff, gotEmpty := projectedOf(t, wOff), projectedOf(t, wEmpty)
	if !reflect.DeepEqual(gotOff, []string{"p0/m", "p1/m", "p2/m", "p3/m"}) || !reflect.DeepEqual(gotEmpty, gotOff) {
		t.Fatalf("routing-off/empty-ranks diverge or reordered: %v / %v", gotOff, gotEmpty)
	}
}

// ---------------------------------------------------------------------------
// Curated editor convergence

// TestReconcileEditorConvergenceCurated applies one fixed successful plan
// through the actual editors to hand-built LF config and LF/CRLF definition
// documents: the managed values land exactly (including an inserted enabled
// key, a preserved reasoning suffix, and a removed fallback list), untouched
// regions keep their bytes, and a second application is byte-stable.
func TestReconcileEditorConvergenceCurated(t *testing.T) {
	w := propWorld{
		desired: policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
			"p0": propMapping(map[string]bool{"p0/m": true}, nil),
			"p1": propMapping(map[string]bool{"p1/m": false}, nil),
		}},
		observed: state.State{Revision: 8},
		target: policy.Target{
			ID: "e", Root: "/synthetic/root", Global: true,
			Full: policy.Chain{"p0/m(med)"},
			Definitions: []policy.Definition{
				{Path: "agent-lf.md", Chain: policy.Chain{"p0/m", "p1/m", "p0/m(high)"}},
				{Path: "agent-crlf.md", Chain: policy.Chain{"p0/m"}},
			},
		},
	}
	plan := propMustBuild(t, w)

	docs := map[string][]byte{
		configFile: []byte("# curated\n" +
			"defaults:\n  full: placeholder-full\n  mini: placeholder-mini\n  nano: placeholder-nano\n" +
			"autonomous_permission_matcher:\n  classifier_model: placeholder-classifier\n" +
			"models:\n  p0/m:\n    enabled: true\n  p1/m:\n    reasoning: high\n" +
			"unrelated:\n  keep: unchanged\n"),
		"agent-lf.md": []byte("---\npolytoken:\n  model: placeholder-model\n  fallback_models:\n    - stale/old\n---\n\nBody stays.\n"),
		"agent-crlf.md": []byte("---\r\npolytoken:\r\n  model: placeholder-model\r\n  fallback_models:\r\n    - stale/old\r\n---\r\n\r\nBody stays.\r\n"),
	}
	once := propApplyPlan(t, plan, docs)
	twice := propApplyPlan(t, plan, once)
	for file, raw := range once {
		if !bytes.Equal(raw, twice[file]) {
			t.Fatalf("second application changed %s\nonce:  %q\ntwice: %q", file, raw, twice[file])
		}
	}
	propAssertDocSemantics(t, w, once)

	cfg := string(once[configFile])
	if !strings.Contains(cfg, "full: p0/m(med)") || !strings.Contains(cfg, "enabled: false") {
		t.Fatalf("config edits missing:\n%s", cfg)
	}
	if !strings.Contains(cfg, "placeholder-nano") || !strings.Contains(cfg, "keep: unchanged") {
		t.Fatalf("untouched regions changed:\n%s", cfg)
	}
	if strings.Contains(cfg, "placeholder-full") {
		t.Fatalf("managed placeholder survived:\n%s", cfg)
	}
	if strings.Contains(string(once["agent-lf.md"]), "stale/old") {
		t.Fatalf("fallback removal did not land:\n%s", once["agent-lf.md"])
	}
	if !bytes.Contains(once["agent-crlf.md"], []byte("Body stays.\r\n")) {
		t.Fatalf("CRLF body changed:\n%s", once["agent-crlf.md"])
	}
}
