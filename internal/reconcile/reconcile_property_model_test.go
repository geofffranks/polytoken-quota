package reconcile

// Independent reconciliation model for the property suite. Everything in this
// file is written from the reconcile.go contract comments and the documented
// state/quota truth tables; production code under test — EffectiveMode,
// MappingMode, resolveSurvivors, applyRoutingOverlay, quota normalization — is
// never called to compute an expectation. Only domain types and constants are
// shared (policy.Desired, state.Mode, EmptyChainError, ...), which the plan
// explicitly permits.
//
// The model computes the exact surviving occurrences of a desired chain,
// including duplicates, and assembles the complete expected Plan (edits in
// Build's documented order, skipped fields, typed empty-chain error) so the
// properties can compare whole plans rather than fragments.

import (
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ---------------------------------------------------------------------------
// Axes and mode derivation (documented truth tables)

// propNormalizeAxes resolves sparse (empty-string) legacy axes to their
// documented healthy baselines, exactly as the state contract describes.
func propNormalizeAxes(ps state.ProviderState) (state.Quota, state.Availability) {
	q, av := ps.Quota, ps.Availability
	if q == "" {
		q = state.QuotaNormal
	}
	if av == "" {
		av = state.Available
	}
	return q, av
}

func propValidQuota(q state.Quota) bool {
	switch q {
	case state.QuotaNormal, state.QuotaLow, state.QuotaExhausted:
		return true
	}
	return false
}

func propValidAvailability(a state.Availability) bool {
	switch a {
	case state.Available, state.Unavailable:
		return true
	}
	return false
}

// propWindowRemaining reports the remaining fraction (0..1) of one snapshot
// window and whether the window is usable at all: a used/limit pair with a
// positive limit, else a usage_percent. Values outside [0,1] clamp.
func propWindowRemaining(w quota.QuotaWindow) (float64, bool) {
	switch {
	case w.Used != nil && w.Limit != nil && *w.Limit > 0:
		r := (*w.Limit - *w.Used) / *w.Limit
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r, true
	case w.UsagePercent != nil:
		r := (100 - *w.UsagePercent) / 100
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r, true
	default:
		return 0, false
	}
}

// propMinRemaining returns the minimum remaining fraction across usable
// windows, or (0, false) when no window is usable.
func propMinRemaining(ps state.ProviderState) (float64, bool) {
	if ps.QuotaSnapshot == nil {
		return 0, false
	}
	minSet, minVal := false, 0.0
	for _, w := range ps.QuotaSnapshot.Windows {
		r, ok := propWindowRemaining(w)
		if !ok {
			continue
		}
		if !minSet || r < minVal {
			minSet, minVal = true, r
		}
	}
	return minVal, minSet
}

// propSnapshotUnavailable is the documented fail-closed poll boundary: the
// snapshot must be explicitly available and report a positive usable remaining
// fraction. A nil snapshot (no poll yet) never crosses the boundary on its own.
func propSnapshotUnavailable(ps state.ProviderState) bool {
	if ps.QuotaSnapshot == nil {
		return false
	}
	if ps.QuotaSnapshot.Availability != quota.QuotaAvailable {
		return true
	}
	rem, ok := propMinRemaining(ps)
	return !ok || rem <= 0
}

// propEffectiveMode is the documented EffectiveMode truth table: manual
// disable, unavailable, or exhausted disable; unrecognized non-empty axes fail
// closed after sparse normalization; available+low is reserve; only
// available+normal is normal. The production helper checks manual/unavailable/
// exhausted before axis validity, but every one of those orders yields
// disabled, so the collapsed order here is observationally identical.
func propEffectiveMode(ps state.ProviderState) state.Mode {
	q, av := propNormalizeAxes(ps)
	if ps.ManualDisabled || av == state.Unavailable || q == state.QuotaExhausted {
		return state.ModeDisabled
	}
	if !propValidQuota(q) || !propValidAvailability(av) {
		return state.ModeDisabled
	}
	if av == state.Available && q == state.QuotaLow {
		return state.ModeReserve
	}
	return state.ModeNormal
}

// propQuotaGateExempt reports the documented `quota_gate: {enabled: false}`
// shape: a quota section must exist and its resolved gate must be disabled.
func propQuotaGateExempt(m policy.Mapping) bool {
	return m.Quota != nil && m.Quota.Gate.Disabled
}

// propMappingMode is the per-cause mode derivation: absent mapping or absent
// observation is healthy; a quota-gate-exempt provider clamps only quota-derived
// causes to reserve while manual disables and corrupted axes fail closed; a
// non-exempt provider uses the EffectiveMode table with the snapshot boundary
// overriding to disabled.
func propMappingMode(d policy.Desired, s state.State, id policy.MappingID) state.Mode {
	m, ok := d.Providers[id]
	if !ok {
		return state.ModeNormal
	}
	ps, seen := s.Providers[string(id)]
	if !seen {
		return state.ModeNormal
	}
	if propQuotaGateExempt(m) {
		if ps.ManualDisabled {
			return state.ModeDisabled
		}
		q, av := propNormalizeAxes(ps)
		if !propValidQuota(q) || !propValidAvailability(av) {
			return state.ModeDisabled
		}
		if propSnapshotUnavailable(ps) {
			return state.ModeReserve
		}
		if av == state.Unavailable || q == state.QuotaExhausted || q == state.QuotaLow {
			return state.ModeReserve
		}
		return state.ModeNormal
	}
	mode := propEffectiveMode(ps)
	if propSnapshotUnavailable(ps) {
		return state.ModeDisabled
	}
	return mode
}

// ---------------------------------------------------------------------------
// Survivor resolution (exact occurrences, duplicates preserved)

// propSurvEntry is one surviving chain occurrence: its exact desired spelling
// and the mapping that owns its base.
type propSurvEntry struct {
	spelling string
	mid      policy.MappingID
}

// propBalanceGroup resolves a mapping's effective balance group with the
// documented "default" fallback (no quota section or empty group).
func propBalanceGroup(d policy.Desired, id policy.MappingID) string {
	m, ok := d.Providers[id]
	if !ok || m.Quota == nil || m.Quota.BalanceGroup == "" {
		return "default"
	}
	return m.Quota.BalanceGroup
}

// propBaseOf splits a valid generated spelling into its base: the portion
// before an optional reasoning suffix. Generated spellings are valid by
// construction ("base" or "base(suffix)" ending at the final byte), so the
// strict invalid-grammar refusals of the canonical parser are not modeled
// here; they belong to policy loading, not reconciliation.
func propBaseOf(spelling string) string {
	for i := 0; i < len(spelling); i++ {
		if spelling[i] == '(' {
			return spelling[:i]
		}
	}
	return spelling
}

// propResolveOwner finds the unique mapping owning base. The generated worlds
// never double-assign a base, so a miss here is a generator defect, not a
// modeled outcome; the caller fails the property.
func propResolveOwner(d policy.Desired, base string) (policy.MappingID, bool) {
	found, count := policy.MappingID(""), 0
	for id, m := range d.Providers {
		if _, ok := m.Models[base]; ok {
			found, count = id, count+1
		}
	}
	if count != 1 {
		return "", false
	}
	return found, true
}

// propOverlaySlotGroups returns, for one survivor partition, the slot indices
// of ranked entries keyed by balance group — the routing overlay's reorder
// scope: only ranked entries, only within the same group, never across
// partitions.
func propOverlaySlotGroups(d policy.Desired, part []propSurvEntry, ranks RankLookup) map[string][]int {
	groups := make(map[string][]int)
	for i, e := range part {
		if _, ok := ranks[e.mid]; !ok {
			continue
		}
		g := propBalanceGroup(d, e.mid)
		groups[g] = append(groups[g], i)
	}
	return groups
}

// propOverlay reorders ranked same-group entries within one partition by
// global rank, stably. Unranked entries, single-entry groups, and equal ranks
// keep their exact positions; membership never changes.
func propOverlay(d policy.Desired, part []propSurvEntry, ranks RankLookup) []propSurvEntry {
	if len(part) < 2 || len(ranks) == 0 {
		return part
	}
	out := append([]propSurvEntry(nil), part...)
	for _, slots := range propOverlaySlotGroups(d, part, ranks) {
		if len(slots) < 2 {
			continue
		}
		vals := make([]propSurvEntry, 0, len(slots))
		for _, i := range slots {
			vals = append(vals, part[i])
		}
		sort.SliceStable(vals, func(a, b int) bool { return ranks[vals[a].mid] < ranks[vals[b].mid] })
		for k, slot := range slots {
			out[slot] = vals[k]
		}
	}
	return out
}

// propSurvivors computes the exact surviving occurrences of a chain: baseline-
// disabled and mode-disabled entries drop, duplicate occurrences are preserved
// in place, survivors stable-partition normal before reserve preserving desired
// order, and the routing overlay reorders within each partition independently.
func propSurvivors(d policy.Desired, s state.State, chain policy.Chain, ranks RankLookup, routingOn bool) ([]propSurvEntry, bool) {
	var normal, reserve []propSurvEntry
	for _, entry := range chain {
		base := propBaseOf(entry)
		mid, ok := propResolveOwner(d, base)
		if !ok {
			return nil, false // generator defect: unowned or ambiguous base
		}
		if baseline, ok := d.Providers[mid].Models[base]; ok && !baseline.Enabled {
			continue // baseline-disabled: unusable even when healthy
		}
		e := propSurvEntry{spelling: entry, mid: mid}
		switch propMappingMode(d, s, mid) {
		case state.ModeDisabled:
			continue
		case state.ModeReserve:
			reserve = append(reserve, e)
		default:
			normal = append(normal, e)
		}
	}
	if routingOn && len(ranks) > 0 {
		normal = propOverlay(d, normal, ranks)
		reserve = propOverlay(d, reserve, ranks)
	}
	return append(normal, reserve...), true
}

// ---------------------------------------------------------------------------
// Whole-plan expectation

// propExpectation is the complete expected outcome of one Build call: the
// whole plan (empty edits and skipped entries whenever err is set, mirroring
// Build's discard-on-error) and the typed error when a required chain has no
// survivor.
type propExpectation struct {
	plan Plan
	err  *EmptyChainError
}

// propTierField lists the scalar config fields Build manages, in its
// documented evaluation order.
var propTierFields = []struct {
	field string
	path  []string
}{
	{"defaults.full", []string{"defaults", "full"}},
	{"defaults.mini", []string{"defaults", "mini"}},
	{"defaults.nano", []string{"defaults", "nano"}},
	{"autonomous_permission_matcher.classifier_model", []string{"autonomous_permission_matcher", "classifier_model"}},
}

func propTierChain(target policy.Target, i int) policy.Chain {
	switch i {
	case 0:
		return target.Full
	case 1:
		return target.Mini
	case 2:
		return target.Nano
	default:
		return target.Classifier
	}
}

// propExpectedPlan assembles the exact expected plan for one target: managed
// tier scalars carry the first survivor, modelgroup-stamped targets report the
// managed tier fields as skipped, definitions carry primary + fallback (or a
// removal), and global targets carry the models block for every managed base
// in sorted order with baseline honored unless the mode is disabled. On the
// first empty required chain the expectation is the empty plan plus the typed
// error naming target, file, and field.
func propExpectedPlan(d policy.Desired, s state.State, target policy.Target, ranks RankLookup) (propExpectation, bool) {
	routingOn := d.Routing.Enabled
	survivors := func(chain policy.Chain) ([]propSurvEntry, bool) {
		return propSurvivors(d, s, chain, ranks, routingOn)
	}
	plan := Plan{TargetID: target.ID, Revision: s.Revision}

	for i, tf := range propTierFields {
		chain := propTierChain(target, i)
		if len(chain) == 0 {
			continue // unmanaged for this target
		}
		if target.UsesModelGroups {
			plan.Skipped = append(plan.Skipped, SkippedEdit{Field: tf.field, Reason: modelGroupsSkipReason})
			continue
		}
		sv, ok := survivors(chain)
		if !ok {
			return propExpectation{}, false
		}
		if len(sv) == 0 {
			return propExpectation{
				plan: Plan{TargetID: target.ID, Revision: s.Revision},
				err:  &EmptyChainError{TargetID: target.ID, File: configFile, Field: tf.field},
			}, true
		}
		v := sv[0].spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: configFile, Path: tf.path, Scalar: &v})
	}

	for _, def := range target.Definitions {
		if len(def.Chain) == 0 {
			continue
		}
		sv, ok := survivors(def.Chain)
		if !ok {
			return propExpectation{}, false
		}
		if len(sv) == 0 {
			return propExpectation{
				plan: Plan{TargetID: target.ID, Revision: s.Revision},
				err:  &EmptyChainError{TargetID: target.ID, File: def.Path, Field: "polytoken.model"},
			}, true
		}
		primary := sv[0].spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: def.Path, Path: []string{"polytoken", "model"}, Scalar: &primary})
		rest := make([]string, 0, len(sv)-1)
		for _, e := range sv[1:] {
			rest = append(rest, e.spelling)
		}
		fb := FieldEdit{File: def.Path, Path: []string{"polytoken", "fallback_models"}}
		if len(rest) > 0 {
			fb.Sequence = rest
		} else {
			fb.Remove = true
		}
		plan.Edits = append(plan.Edits, fb)
	}

	if target.Global {
		ids := make([]string, 0, len(d.Providers))
		for id := range d.Providers {
			ids = append(ids, string(id))
		}
		sort.Strings(ids)
		for _, idStr := range ids {
			id := policy.MappingID(idStr)
			mapping := d.Providers[id]
			mode := propMappingMode(d, s, id)
			bases := make([]string, 0, len(mapping.Models))
			for base := range mapping.Models {
				bases = append(bases, base)
			}
			sort.Strings(bases)
			for _, base := range bases {
				enabled := false
				if mode != state.ModeDisabled {
					enabled = mapping.Models[base].Enabled
				}
				plan.Edits = append(plan.Edits, FieldEdit{
					File:    configFile,
					Path:    []string{"models", base, "enabled"},
					Enabled: &enabled,
				})
			}
		}
	}

	return propExpectation{plan: plan}, true
}
