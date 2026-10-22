package reconcile

// Rapid property suite for the reconciler. Worlds are generated within the
// plan's bounded caps (at most 5 provider mappings with 1-3 managed bases
// each, chains of at most 8 entries including deliberate duplicates, at most 2
// definitions, and small fixed observation/rank/suffix pools) and compared
// against the independent model in reconcile_property_model_test.go.
//
// Three properties run here:
//
//   - the whole plan (edits, skipped fields, typed empty-chain error, revision,
//     target identity) matches the model exactly, Build is deterministic, and
//     no input is mutated;
//   - the routing overlay reorders only ranked same-group slots within each
//     mode partition, so an order-preserving relabeling of ranks changes
//     nothing and routing-off equals an empty rank lookup;
//   - successful plans applied through the actual document editors to
//     synthetic config/definition documents converge byte-for-byte on a second
//     application while every managed value matches the model semantically.
//
// Generated worlds are valid by construction (unique model ownership, chain
// entries that resolve to a managed base, parseable spellings), so any
// rejection other than the documented typed empty-chain error fails the
// property.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/document"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"pgregory.net/rapid"
	"gopkg.in/yaml.v3"
)

// propBaseTime anchors the synthetic snapshot timestamps (fixed clock; Build
// itself reads no time).
var propBaseTime = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// propTB is the minimal test surface the shared property helpers need; both
// *testing.T (curated cases) and *rapid.T (generated properties) satisfy it.
type propTB interface {
	Helper()
	Fatalf(format string, args ...interface{})
}

func propFloat(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// Generated worlds

// propWorld is one generated reconciliation scenario plus the owned-base pool
// used to build synthetic documents.
type propWorld struct {
	desired  policy.Desired
	observed state.State
	target   policy.Target
	ranks    RankLookup
	bases    []string // every owned base, sorted
}

var (
	propGenGroup      = rapid.SampledFrom([]string{"", "default", "g1", "g2"})
	propGenSuffix     = rapid.SampledFrom([]string{"med", "high"})
	propGenTargetID   = rapid.SampledFrom([]string{"tgt-one", "proj-alpha"})
	propGenQuotaAxis  = rapid.SampledFrom([]state.Quota{state.QuotaNormal, state.QuotaLow, state.QuotaExhausted, "", state.Quota("corrupt-quota")})
	propGenAvailAxis  = rapid.SampledFrom([]state.Availability{state.Available, state.Unavailable, "", state.Availability("corrupt-avail")})
	propGenRankValue  = rapid.SampledFrom([]int{0, 1, 1, 2, 5})
	propGenSnapshotID = rapid.SampledFrom([]string{"none", "fresh-half", "fresh-zero", "overdrawn", "unavailable", "windowless", "empty-avail"})
)

// propDrawSnapshot builds one bounded snapshot class: no poll, fresh with
// positive remaining, exactly zero remaining, clamped-negative remaining,
// explicitly unavailable, available but windowless (no usable remaining), and
// a snapshot whose availability spelling crosses the boundary.
func propDrawSnapshot(t *rapid.T) *quota.QuotaSnapshot {
	switch propGenSnapshotID.Draw(t, "snapshotClass") {
	case "none":
		return nil
	case "fresh-half":
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{Used: propFloat(50), Limit: propFloat(100)}}}
	case "fresh-zero":
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{Used: propFloat(100), Limit: propFloat(100)}}}
	case "overdrawn":
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{Used: propFloat(120), Limit: propFloat(100)}}}
	case "unavailable":
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaUnavailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{Used: propFloat(0), Limit: propFloat(100)}}}
	case "windowless":
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Availability: quota.QuotaAvailable, Status: quota.SourceFresh}
	default: // empty-avail
		return &quota.QuotaSnapshot{CheckedAt: propBaseTime, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{Used: propFloat(50), Limit: propFloat(100)}}}
	}
}

// propDrawProviderState draws one observed provider entry: axes from the
// sparse/healthy/degraded/corrupt pool, an optional manual disable, and a
// bounded snapshot class.
func propDrawProviderState(t *rapid.T) state.ProviderState {
	return state.ProviderState{
		Quota:          propGenQuotaAxis.Draw(t, "quotaAxis"),
		Availability:   propGenAvailAxis.Draw(t, "availAxis"),
		ManualDisabled: rapid.Bool().Draw(t, "manual"),
		QuotaAt:        propBaseTime,
		QuotaSnapshot:  propDrawSnapshot(t),
	}
}

// propDrawChain draws a desired chain from the owned-base pool with optional
// reasoning suffixes and deliberate duplicate occurrences.
func propDrawChain(t *rapid.T, bases []string, min, max int) policy.Chain {
	spelling := rapid.Custom(func(t *rapid.T) string {
		base := bases[rapid.IntRange(0, len(bases)-1).Draw(t, "baseIdx")]
		if rapid.Bool().Draw(t, "suffixed") {
			return base + "(" + propGenSuffix.Draw(t, "suffix") + ")"
		}
		return base
	})
	return rapid.SliceOfN(spelling, min, max).Draw(t, "chain")
}

// genPropProviders draws 1-5 mappings owning 1-3 bases each with independent
// baselines and optional quota sections (balance groups and quota-gate
// exemption), returning the desired map and the sorted owned-base pool.
func genPropProviders(t *rapid.T) (map[policy.MappingID]policy.Mapping, []string, []policy.MappingID) {
	n := rapid.IntRange(1, 5).Draw(t, "providers")
	providers := make(map[policy.MappingID]policy.Mapping, n)
	var bases []string
	var ids []policy.MappingID
	for i := 0; i < n; i++ {
		id := policy.MappingID(fmt.Sprintf("p%d", i))
		ids = append(ids, id)
		nb := rapid.IntRange(1, 3).Draw(t, "basesPerProvider")
		models := make(map[string]policy.ModelBaseline, nb)
		for j := 0; j < nb; j++ {
			base := fmt.Sprintf("p%d/m%d", i, j)
			bases = append(bases, base)
			models[base] = policy.ModelBaseline{Enabled: rapid.Bool().Draw(t, "baseline"), HadEnabledKey: true}
		}
		m := policy.Mapping{Models: models}
		if rapid.Bool().Draw(t, "hasQuota") {
			m.Quota = &policy.QuotaConfig{
				Adapter:      "codex",
				FreshnessTTL: 30 * time.Minute,
				Weight:       1,
				BalanceGroup: propGenGroup.Draw(t, "balanceGroup"),
				Gate:         policy.QuotaGateConfig{Disabled: rapid.Bool().Draw(t, "quotaGateExempt")},
			}
		}
		providers[id] = m
	}
	sort.Strings(bases)
	return providers, bases, ids
}

// genPropObserved draws the observed state: every provider is tracked with
// high probability; the revision is stamped on the state.
func genPropObserved(t *rapid.T, ids []policy.MappingID, revision uint64) state.State {
	s := state.State{Revision: revision}
	for _, id := range ids {
		if rapid.Bool().Draw(t, "tracked") {
			if s.Providers == nil {
				s.Providers = map[string]state.ProviderState{}
			}
			s.Providers[string(id)] = propDrawProviderState(t)
		}
	}
	return s
}

// genPropWorld draws one complete scenario. Chains live on the target's tier
// fields and definitions; empty and singleton chains occur naturally.
func genPropWorld(t *rapid.T) propWorld {
	providers, bases, ids := genPropProviders(t)
	desired := policy.Desired{Version: 1, Providers: providers, Routing: policy.RoutingConfig{Enabled: rapid.Bool().Draw(t, "routing")}}

	var ranks RankLookup
	if desired.Routing.Enabled {
		ranks = RankLookup{}
		for _, id := range ids {
			if rapid.Bool().Draw(t, "ranked") {
				ranks[id] = propGenRankValue.Draw(t, "rank")
			}
		}
	}

	observed := genPropObserved(t, ids, rapid.Uint64().Draw(t, "revision"))

	target := policy.Target{
		ID:              propGenTargetID.Draw(t, "targetID"),
		Root:            "/synthetic/root",
		Global:          rapid.Bool().Draw(t, "global"),
		UsesModelGroups: rapid.Bool().Draw(t, "modelGroups"),
	}
	target.Full = propDrawChain(t, bases, 0, 8)
	target.Mini = propDrawChain(t, bases, 0, 8)
	target.Nano = propDrawChain(t, bases, 0, 8)
	target.Classifier = propDrawChain(t, bases, 0, 8)
	for k := 0; k < rapid.IntRange(0, 2).Draw(t, "definitions"); k++ {
		target.Definitions = append(target.Definitions, policy.Definition{
			Path:  fmt.Sprintf("agent-%d.md", k),
			Chain: propDrawChain(t, bases, 0, 8),
		})
	}

	return propWorld{desired: desired, observed: observed, target: target, ranks: ranks, bases: bases}
}

// genPropRoutingWorld draws a routing-focused scenario: routing on, 2-4
// single-base mappings so chains always span several mappings, at least two
// ranked mappings, and non-trivial chains on the definition (the projection
// that reveals the full survivor order).
func genPropRoutingWorld(t *rapid.T) propWorld {
	providers, bases, ids := genPropRoutingProviders(t)
	desired := policy.Desired{Version: 1, Providers: providers, Routing: policy.RoutingConfig{Enabled: true}}

	ranks := RankLookup{}
	rankedCount := 0
	for _, id := range ids {
		if rapid.Bool().Draw(t, "ranked") {
			ranks[id] = propGenRankValue.Draw(t, "rank")
			rankedCount++
		}
	}
	for i := 0; rankedCount < 2 && i < len(ids); i++ {
		if _, ok := ranks[ids[i]]; !ok {
			ranks[ids[i]] = propGenRankValue.Draw(t, "forcedRank")
			rankedCount++
		}
	}

	observed := genPropObserved(t, ids, rapid.Uint64().Draw(t, "revision"))

	target := policy.Target{ID: propGenTargetID.Draw(t, "targetID"), Root: "/synthetic/root"}
	for k := 0; k < rapid.IntRange(1, 2).Draw(t, "definitions"); k++ {
		target.Definitions = append(target.Definitions, policy.Definition{
			Path:  fmt.Sprintf("agent-%d.md", k),
			Chain: propDrawChain(t, bases, 2, 6),
		})
	}

	return propWorld{desired: desired, observed: observed, target: target, ranks: ranks, bases: bases}
}

// genPropRoutingProviders draws 2-4 single-base mappings with balance groups
// weighted toward reuse (so same-group reorders are common) and occasional
// quota-gate exemption interacting with degraded observations.
func genPropRoutingProviders(t *rapid.T) (map[policy.MappingID]policy.Mapping, []string, []policy.MappingID) {
	n := rapid.IntRange(2, 4).Draw(t, "providers")
	providers := make(map[policy.MappingID]policy.Mapping, n)
	var bases []string
	var mids []policy.MappingID
	groups := []string{"default", "g1", "g1", "g2"}
	for i := 0; i < n; i++ {
		id := policy.MappingID(fmt.Sprintf("p%d", i))
		mids = append(mids, id)
		base := fmt.Sprintf("p%d/m", i)
		bases = append(bases, base)
		m := policy.Mapping{
			Models: map[string]policy.ModelBaseline{base: {Enabled: true, HadEnabledKey: true}},
			Quota: &policy.QuotaConfig{
				Adapter:      "codex",
				FreshnessTTL: 30 * time.Minute,
				Weight:       1,
				BalanceGroup: groups[i%len(groups)],
				Gate:         policy.QuotaGateConfig{Disabled: rapid.Bool().Draw(t, "quotaGateExempt")},
			},
		}
		providers[id] = m
	}
	return providers, bases, mids
}

// ---------------------------------------------------------------------------
// Input snapshots (alias-based false-agreement guard)

// propInputs is a deep copy of everything Build receives.
type propInputs struct {
	desired  policy.Desired
	observed state.State
	target   policy.Target
	ranks    RankLookup
}

func propCloneDesired(d policy.Desired) policy.Desired {
	out := d
	out.Providers = make(map[policy.MappingID]policy.Mapping, len(d.Providers))
	for id, m := range d.Providers {
		models := make(map[string]policy.ModelBaseline, len(m.Models))
		for base, b := range m.Models {
			models[base] = b
		}
		m.Models = models
		if m.Quota != nil {
			q := *m.Quota
			m.Quota = &q
		}
		out.Providers[id] = m
	}
	return out
}

func propCloneObserved(s state.State) state.State {
	out := s
	out.Providers = make(map[string]state.ProviderState, len(s.Providers))
	for id, ps := range s.Providers {
		if ps.QuotaSnapshot != nil {
			snap := *ps.QuotaSnapshot
			snap.Windows = append([]quota.QuotaWindow(nil), ps.QuotaSnapshot.Windows...)
			for i, w := range snap.Windows {
				if w.Used != nil {
					v := *w.Used
					snap.Windows[i].Used = &v
				}
				if w.Limit != nil {
					v := *w.Limit
					snap.Windows[i].Limit = &v
				}
				if w.UsagePercent != nil {
					v := *w.UsagePercent
					snap.Windows[i].UsagePercent = &v
				}
				if w.ResetAt != nil {
					v := *w.ResetAt
					snap.Windows[i].ResetAt = &v
				}
			}
			ps.QuotaSnapshot = &snap
		}
		out.Providers[id] = ps
	}
	return out
}

func propCloneTarget(t policy.Target) policy.Target {
	out := t
	out.Full = append(policy.Chain(nil), t.Full...)
	out.Mini = append(policy.Chain(nil), t.Mini...)
	out.Nano = append(policy.Chain(nil), t.Nano...)
	out.Classifier = append(policy.Chain(nil), t.Classifier...)
	out.Definitions = append([]policy.Definition(nil), t.Definitions...)
	for i := range out.Definitions {
		out.Definitions[i].Chain = append(policy.Chain(nil), t.Definitions[i].Chain...)
	}
	return out
}

func propCloneRanks(r RankLookup) RankLookup {
	out := make(RankLookup, len(r))
	for id, rank := range r {
		out[id] = rank
	}
	return out
}

func propSnapshotInputs(w propWorld) propInputs {
	return propInputs{
		desired:  propCloneDesired(w.desired),
		observed: propCloneObserved(w.observed),
		target:   propCloneTarget(w.target),
		ranks:    propCloneRanks(w.ranks),
	}
}

// ---------------------------------------------------------------------------
// Exact plan comparison

func propWorldString(w propWorld) string {
	return fmt.Sprintf("desired: %+#v\nobserved: %+#v\ntarget: %+#v\nranks: %v",
		w.desired, w.observed, w.target, w.ranks)
}

// propComparePlan asserts the production plan equals the independent model's
// expectation exactly: identity fields, the full edit list in documented
// order, skipped fields, and the typed empty-chain error. Any rejection of a
// generated valid world other than that typed error fails here.
func propComparePlan(t propTB, w propWorld, plan Plan, err error) {
	t.Helper()
	exp, ok := propExpectedPlan(w.desired, w.observed, w.target, w.ranks)
	if !ok {
		t.Fatalf("generator produced an unowned or ambiguous base:\n%s", propWorldString(w))
	}
	if exp.err == nil {
		if err != nil {
			t.Fatalf("valid world unexpectedly rejected: %v\nworld:\n%s", err, propWorldString(w))
		}
	} else {
		// Build returns the error as a value, so the As target is the value
		// type; a **EmptyChainError target would never match.
		var ec EmptyChainError
		if err == nil || !errors.As(err, &ec) {
			t.Fatalf("expected EmptyChainError %+#v, got %v\nworld:\n%s", *exp.err, err, propWorldString(w))
		}
		if ec.TargetID != exp.err.TargetID || ec.File != exp.err.File || ec.Field != exp.err.Field {
			t.Fatalf("EmptyChainError mismatch: got %+#v want %+#v\nworld:\n%s", ec, *exp.err, propWorldString(w))
		}
	}
	if plan.TargetID != exp.plan.TargetID || plan.Revision != exp.plan.Revision || plan.ProviderOnly != exp.plan.ProviderOnly {
		t.Fatalf("plan identity mismatch: got %+#v want target=%q revision=%d providerOnly=%v",
			plan, exp.plan.TargetID, exp.plan.Revision, exp.plan.ProviderOnly)
	}
	if !reflect.DeepEqual(plan.Edits, exp.plan.Edits) {
		t.Fatalf("edits mismatch:\n got: %+#v\nwant: %+#v\nworld:\n%s", plan.Edits, exp.plan.Edits, propWorldString(w))
	}
	if !reflect.DeepEqual(plan.Skipped, exp.plan.Skipped) {
		t.Fatalf("skipped mismatch:\n got: %+#v\nwant: %+#v\nworld:\n%s", plan.Skipped, exp.plan.Skipped, propWorldString(w))
	}
}

// ---------------------------------------------------------------------------
// Properties

// TestPropertyReconcilePlanMatchesIndependentModel is the main generated
// property: exact whole-plan agreement, determinism across repeated builds,
// and input immutability under deep snapshots.
func TestPropertyReconcilePlanMatchesIndependentModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := genPropWorld(t)
		pre := propSnapshotInputs(w)

		plan, err := Build(w.desired, w.observed, w.target, w.ranks)
		propComparePlan(t, w, plan, err)

		plan2, err2 := Build(w.desired, w.observed, w.target, w.ranks)
		if fmt.Sprint(err) != fmt.Sprint(err2) || !reflect.DeepEqual(plan, plan2) {
			t.Fatalf("Build nondeterministic: %+#v/%v vs %+#v/%v", plan, err, plan2, err2)
		}

		post := propSnapshotInputs(w)
		if !reflect.DeepEqual(pre, post) {
			t.Fatalf("Build mutated its inputs\nbefore: %+#v\nafter:  %+#v", pre, post)
		}
	})
}

// propRelabelRanksOrderPreserving maps each distinct rank value to its index
// among the sorted distinct values. All comparisons (and ties) between ranks
// are preserved, so any plan that depends only on rank order must be
// unchanged.
func propRelabelRanksOrderPreserving(ranks RankLookup) RankLookup {
	seen := map[int]bool{}
	var vals []int
	for _, r := range ranks {
		if !seen[r] {
			seen[r] = true
			vals = append(vals, r)
		}
	}
	sort.Ints(vals)
	idx := make(map[int]int, len(vals))
	for i, v := range vals {
		idx[v] = i
	}
	out := make(RankLookup, len(ranks))
	for id, r := range ranks {
		out[id] = idx[r]
	}
	return out
}

// TestPropertyReconcileRoutingOverlayWithinPartitions pins the overlay's
// scope: an order-preserving rank relabeling changes nothing, and routing-off
// is indistinguishable from an empty rank lookup. The exact plan comparison
// against the model (which reorders only ranked same-group slots within each
// partition) covers slot membership and inter-group positions on every case.
func TestPropertyReconcileRoutingOverlayWithinPartitions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := genPropRoutingWorld(t)

		plan, err := Build(w.desired, w.observed, w.target, w.ranks)
		propComparePlan(t, w, plan, err)

		relabelled := propRelabelRanksOrderPreserving(w.ranks)
		plan2, err2 := Build(w.desired, w.observed, w.target, relabelled)
		if fmt.Sprint(err) != fmt.Sprint(err2) || !reflect.DeepEqual(plan, plan2) {
			t.Fatalf("overlay depends on rank magnitudes:\n got: %+#v/%v\nwant: %+#v/%v\nworld:\n%s",
				plan, err, plan2, err2, propWorldString(w))
		}

		off := w.desired
		off.Routing = policy.RoutingConfig{}
		planOff, errOff := Build(off, w.observed, w.target, nil)
		planEmpty, errEmpty := Build(w.desired, w.observed, w.target, RankLookup{})
		if fmt.Sprint(errOff) != fmt.Sprint(errEmpty) || !reflect.DeepEqual(planOff, planEmpty) {
			t.Fatalf("routing-off and empty-ranks diverge:\n off: %+#v/%v\nempty: %+#v/%v", planOff, errOff, planEmpty, errEmpty)
		}
	})
}

// ---------------------------------------------------------------------------
// Editor application: convergence and semantic correctness

// propDocEdit maps one abstract FieldEdit onto the document editor's Edit
// value carrier (the same one-value-carrier rule the staging layer applies).
func propDocEdit(fe FieldEdit) document.Edit {
	e := document.Edit{Path: fe.Path}
	switch {
	case fe.Remove:
		e.Remove = true
	case fe.Scalar != nil:
		e.Kind = document.Scalar
		e.Scalar = fe.Scalar
	case len(fe.Sequence) > 0:
		e.Kind = document.Sequence
		e.Sequence = fe.Sequence
	case fe.Enabled != nil:
		e.Kind = document.Boolean
		e.Bool = fe.Enabled
	}
	return e
}

// propApplyPlan groups the plan's edits per file in plan order and runs them
// through the actual document editors against synthetic documents. The input
// map is copied; the editors must never need its bytes anyway.
func propApplyPlan(t propTB, plan Plan, docs map[string][]byte) map[string][]byte {
	t.Helper()
	byFile := make(map[string][]document.Edit)
	var order []string
	for _, fe := range plan.Edits {
		if _, ok := byFile[fe.File]; !ok {
			order = append(order, fe.File)
		}
		byFile[fe.File] = append(byFile[fe.File], propDocEdit(fe))
	}
	out := make(map[string][]byte, len(docs))
	for file, raw := range docs {
		out[file] = raw
	}
	for _, file := range order {
		raw, ok := out[file]
		if !ok {
			t.Fatalf("plan references a file with no synthetic document: %q", file)
		}
		var res []byte
		var err error
		if file == configFile {
			res, err = document.EditYAML(raw, byFile[file])
		} else {
			res, err = document.EditFrontmatter(raw, byFile[file])
		}
		if err != nil {
			t.Fatalf("editor refused a valid plan for %s: %v\nraw: %q", file, err, raw)
		}
		out[file] = res
	}
	return out
}

// propSyntheticDocs builds block-style config.yaml (placeholders for every
// tier field, a models block mixing explicit enabled keys with models that
// lack one, and an unrelated key) plus one frontmatter document per
// definition path with mixed LF/CRLF endings.
func propSyntheticDocs(t *rapid.T, w propWorld) map[string][]byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("# synthetic config (property fixture)\n")
	b.WriteString("defaults:\n  full: placeholder-full\n  mini: placeholder-mini\n  nano: placeholder-nano\n")
	b.WriteString("autonomous_permission_matcher:\n  classifier_model: placeholder-classifier\n")
	b.WriteString("models:\n")
	for _, base := range w.bases {
		if rapid.Bool().Draw(t, "docEnabledKey") {
			fmt.Fprintf(&b, "  %s:\n    enabled: %t\n", base, rapid.Bool().Draw(t, "docEnabledVal"))
		} else {
			fmt.Fprintf(&b, "  %s:\n    reasoning: high\n", base)
		}
	}
	b.WriteString("unrelated:\n  keep: unchanged\n")
	docs := map[string][]byte{configFile: []byte(b.String())}

	for k := range w.target.Definitions {
		eol := "\n"
		if rapid.Bool().Draw(t, "docCRLF") {
			eol = "\r\n"
		}
		lines := []string{"---", "# synthetic facet frontmatter", "polytoken:", "  model: placeholder-model"}
		if rapid.Bool().Draw(t, "docHasFallback") {
			lines = append(lines, "  fallback_models:", "    - stale/old-1")
		}
		lines = append(lines, "description: \"property fixture\"", "---", "", "Unmanaged prose body.", "")
		path := fmt.Sprintf("agent-%d.md", k)
		docs[path] = []byte(strings.Join(lines, eol))
	}
	return docs
}

// propDecodeYAMLMap decodes managed-document YAML into generic maps for
// semantic value checks (test-owned decoding, not a production helper).
func propDecodeYAMLMap(t propTB, raw []byte, label string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: produced unparseable YAML: %v\nraw: %q", label, err, raw)
	}
	return m
}

// propFrontmatterOf returns the YAML between the constructed document's
// opening and closing "---" lines. The fixture guarantees both exist and that
// the body never contains a delimiter line.
func propFrontmatterOf(t propTB, raw []byte, label string) map[string]interface{} {
	t.Helper()
	lines := strings.Split(string(raw), "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return propDecodeYAMLMap(t, []byte(strings.Join(lines[1:i], "\n")), label)
		}
	}
	t.Fatalf("%s: synthetic document lost its frontmatter close:\n%q", label, raw)
	return nil
}

func propLookupPath(t propTB, doc map[string]interface{}, path []string, label string) interface{} {
	t.Helper()
	var cur interface{} = doc
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: expected %v to exist, found non-map at %q", label, path, key)
		}
		if cur, ok = m[key]; !ok {
			t.Fatalf("%s: expected key %v missing at %q", label, path, key)
		}
	}
	return cur
}

// propPathExists reports whether the full key path exists in the decoded
// document.
func propPathExists(doc map[string]interface{}, path []string) bool {
	var cur interface{} = doc
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return false
		}
		if cur, ok = m[key]; !ok {
			return false
		}
	}
	return true
}

// propPlaceholder names the untouched-region sentinel for tier field i.
func propPlaceholder(i int) string {
	return []string{"placeholder-full", "placeholder-mini", "placeholder-nano", "placeholder-classifier"}[i]
}

// propAssertDocSemantics walks the model's expected edits over the applied
// documents: every scalar/boolean/sequence/removal landed exactly, and every
// tier field the plan deliberately did not manage still holds its placeholder
// (whole-document normalization would fail this).
func propAssertDocSemantics(t propTB, w propWorld, docs map[string][]byte) {
	t.Helper()
	exp, ok := propExpectedPlan(w.desired, w.observed, w.target, w.ranks)
	if !ok || exp.err != nil {
		t.Fatalf("editor property invoked on a non-success expectation (ok=%v err=%v)", ok, exp.err)
	}
	for _, fe := range exp.plan.Edits {
		label := fe.File + ":" + strings.Join(fe.Path, ".")
		var doc map[string]interface{}
		if fe.File == configFile {
			doc = propDecodeYAMLMap(t, docs[fe.File], label)
		} else {
			doc = propFrontmatterOf(t, docs[fe.File], label)
		}
		switch {
		case fe.Scalar != nil:
			if got := propLookupPath(t, doc, fe.Path, label); got != *fe.Scalar {
				t.Fatalf("%s: scalar %v: got %+#v want %q", label, fe.Path, got, *fe.Scalar)
			}
		case fe.Enabled != nil:
			if got := propLookupPath(t, doc, fe.Path, label); got != *fe.Enabled {
				t.Fatalf("%s: boolean %v: got %+#v want %t", label, fe.Path, got, *fe.Enabled)
			}
		case len(fe.Sequence) > 0:
			raw, ok := propLookupPath(t, doc, fe.Path, label).([]interface{})
			if !ok || len(raw) != len(fe.Sequence) {
				t.Fatalf("%s: sequence %v: got %+#v want %v", label, fe.Path, propLookupPath(t, doc, fe.Path, label), fe.Sequence)
			}
			for i, item := range raw {
				if s, _ := item.(string); s != fe.Sequence[i] {
					t.Fatalf("%s: sequence %v[%d]: got %+#v want %q", label, fe.Path, i, item, fe.Sequence[i])
				}
			}
		case fe.Remove:
			if propPathExists(doc, fe.Path) {
				t.Fatalf("%s: key %v should have been removed, still present as %+#v", label, fe.Path, propLookupPath(t, doc, fe.Path, label))
			}
		}
	}
	// Untouched-region sentinels: unmanaged tier fields keep their placeholders.
	cfg := propDecodeYAMLMap(t, docs[configFile], "config-sentinels")
	for i := range propTierFields {
		managed := len(propTierChain(w.target, i)) > 0 && !w.target.UsesModelGroups
		if managed {
			continue
		}
		tf := propTierFields[i]
		if got := propLookupPath(t, cfg, tf.path, "sentinel "+tf.field); got != propPlaceholder(i) {
			t.Fatalf("unmanaged %s changed: got %+#v want %q", tf.field, got, propPlaceholder(i))
		}
	}
}

// TestPropertyReconcileEditorsConvergeSemantically applies every successful
// generated plan through the real document editors: a second application with
// unchanged inputs must leave identical bytes, and every managed value (and
// every deliberately unmanaged region) must match the independent model.
func TestPropertyReconcileEditorsConvergeSemantically(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := genPropWorld(t)
		plan, err := Build(w.desired, w.observed, w.target, w.ranks)
		if err != nil {
			var ec EmptyChainError
			if !errors.As(err, &ec) {
				t.Fatalf("valid world unexpectedly rejected: %v\nworld:\n%s", err, propWorldString(w))
			}
			return // typed empty-chain failure produces no edits to apply
		}
		propComparePlan(t, w, plan, err)

		docs := propSyntheticDocs(t, w)
		once := propApplyPlan(t, plan, docs)
		twice := propApplyPlan(t, plan, once)
		for file, raw := range once {
			if !bytes.Equal(raw, twice[file]) {
				t.Fatalf("second application changed %s\nonce:  %q\ntwice: %q", file, raw, twice[file])
			}
		}
		propAssertDocSemantics(t, w, once)
	})
}
