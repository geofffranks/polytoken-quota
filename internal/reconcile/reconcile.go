// Package reconcile is the pure desired-chain reconciler for the polytoken-quota
// utility. It transforms a desired policy plus observed provider state into an
// abstract Plan of managed FieldEdits. It is a pure function of its inputs: it
// never touches bytes, files, or the filesystem. The Plan is consumed by staging
// (which materializes candidate files) and the coordinator.
//
// For each desired chain the reconciler normalizes entries into a base model plus an
// optional reasoning suffix, resolves the base to exactly one provider mapping,
// drops entries whose provider is disabled, stable-partitions the survivors
// (normal first, then reserve), and projects them onto managed fields. A disabled
// provider forces its models to enabled:false; a healthy provider restores each
// model's desired baseline. A required chain with no survivor is a typed render
// failure that produces no edits.
package reconcile

import (
	"fmt"
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// configFile names the managed config.yaml document (relative to a target root) that
// holds the global scalar fields and per-model enable flags.
const configFile = "config.yaml"

// ModelRef is a normalized desired chain entry. Spelling is the exact desired
// preference (including any reasoning suffix); Base is the portion used for
// provider-mode matching; Suffix is the reasoning level without its parentheses
// (empty when none).
type ModelRef struct {
	Spelling string
	Base     string
	Suffix   string
}

// FieldEdit is one abstract managed-field change addressed at the key level. File is
// the facet/subagent definition path for list edits, or configFile for global
// config.yaml edits. Path addresses a key sequence (e.g. ["defaults","full"] or
// ["models","codex/gpt-5.6-sol","enabled"]). Exactly one of Scalar, Sequence, or
// Enabled carries the value; Remove signals that the key should be removed.
type FieldEdit struct {
	File     string
	Path     []string
	Scalar   *string
	Sequence []string
	Enabled  *bool
	Remove   bool
}

// Plan is the reconciled set of managed edits for one target, stamped with the
// observed state revision it was computed from.
type Plan struct {
	TargetID string
	Revision uint64
	Edits    []FieldEdit
}

// EmptyChainError is the typed render failure returned when a required chain has no
// survivor after disabled filtering. No live edit is produced for the target.
type EmptyChainError struct {
	TargetID string
	File     string
	Field    string
}

func (e EmptyChainError) Error() string {
	return "required chain has no survivor: " + e.TargetID + ":" + e.Field
}

// ParseModelRef splits a desired chain entry into its base model and an optional
// reasoning suffix. "codex/gpt-5.6-sol(medium)" yields base "codex/gpt-5.6-sol" and
// suffix "medium"; a bare entry yields Base == Spelling with an empty suffix.
// The grammar is the single canonical one in policy.ParseModelRef, so policy
// loading and reconciliation can never disagree about which spellings are valid.
func ParseModelRef(entry string) (ModelRef, error) {
	base, suffix, err := policy.ParseModelRef(entry)
	if err != nil {
		return ModelRef{}, fmt.Errorf("reconcile: %w", err)
	}
	return ModelRef{Spelling: entry, Base: base, Suffix: suffix}, nil
}

// survivor is a desired chain entry that survived disabled filtering, with its
// resolved provider mapping and effective mode for partitioning.
type survivor struct {
	ref  ModelRef
	mid  policy.MappingID
	mode state.Mode
}

// modeRank orders effective modes from least to most degraded: normal < reserve <
// disabled. Unknown values rank as normal.
func modeRank(m state.Mode) int {
	switch m {
	case state.ModeDisabled:
		return 2
	case state.ModeReserve:
		return 1
	default:
		return 0
	}
}

// MappingMode derives a mapping's effective mode from the observed state keyed
// by the mapping ID. A provider absent from the observed state is healthy.
func MappingMode(d policy.Desired, s state.State, id policy.MappingID) state.Mode {
	if _, ok := d.Providers[id]; !ok {
		return state.ModeNormal
	}
	ps, seen := s.Providers[string(id)]
	if !seen {
		return state.ModeNormal
	}
	mode := state.EffectiveMode(ps)
	// A successful quota poll is an additional, fail-closed availability
	// boundary. The state axes may still reflect their last hook event while the
	// latest snapshot reports explicit exhaustion/unavailability.
	if ps.QuotaSnapshot != nil {
		rem := ps.QuotaSnapshot.EffectiveRemaining()
		if ps.QuotaSnapshot.Availability != quota.QuotaAvailable || rem == nil || *rem <= 0 {
			return state.ModeDisabled
		}
	}
	return mode
}

// RankLookup maps a provider mapping ID to its global rank (0 = best). A mapping
// absent from the lookup has no rank and preserves its position. It is the
// routing overlay's only input beyond the desired policy: when routing is
// disabled (the default) or the lookup is nil, reconciliation is byte-for-byte
// identical to the pre-routing behavior.
type RankLookup map[policy.MappingID]int

// Build reconciles one target's desired chains against the observed provider state
// and returns the abstract managed edits. It is deterministic and never mutates its
// inputs. A non-empty chain whose survivors are all disabled yields an
// EmptyChainError and a plan with no edits; an empty desired chain is treated as
// unmanaged and skipped.
//
// When desired.Routing.Enabled is true and ranks is non-empty, the routing overlay
// reorders survivors within each (normal/reserve) partition by global rank, but
// only among mappings that share a balance group and both carry a rank. It never
// adds, removes, or resurrects survivors, and it never mutates the desired policy.
func Build(desired policy.Desired, observed state.State, target policy.Target, ranks RankLookup) (Plan, error) {
	plan := Plan{TargetID: target.ID, Revision: observed.Revision}

	// survivors resolves, filters, stable-partitions, and (optionally) overlays a
	// desired chain: disabled entries are dropped, normal survivors precede
	// reserve survivors, and desired relative order is preserved within each
	// partition. When routing is enabled and a rank lookup is provided the
	// routing overlay reorders entries within each partition by global rank
	// (same balance group, both ranked only); entries that do not meet the
	// reorder criteria keep their original relative order.
	survivors := func(c policy.Chain) ([]survivor, error) {
		return resolveSurvivors(desired, observed, c, ranks)
	}

	// Scalar config fields: write only the first survivor, never a fallback.
	for _, sp := range []struct {
		field string
		path  []string
		chain policy.Chain
	}{
		{"defaults.full", []string{"defaults", "full"}, target.Full},
		{"defaults.mini", []string{"defaults", "mini"}, target.Mini},
		{"defaults.nano", []string{"defaults", "nano"}, target.Nano},
		{"autonomous_permission_matcher.classifier_model", []string{"autonomous_permission_matcher", "classifier_model"}, target.Classifier},
	} {
		if len(sp.chain) == 0 {
			continue // unmanaged for this target
		}
		sv, err := survivors(sp.chain)
		if err != nil {
			return emptyPlan(target, observed), err
		}
		if len(sv) == 0 {
			return emptyPlan(target, observed), EmptyChainError{TargetID: target.ID, File: configFile, Field: sp.field}
		}
		v := sv[0].ref.Spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: configFile, Path: sp.path, Scalar: &v})
	}

	// Facet/subagent definitions: first survivor -> polytoken.model, rest ->
	// polytoken.fallback_models; with no remaining survivor the fallback list is
	// cleared so a disabled fallback cannot linger.
	for _, def := range target.Definitions {
		if len(def.Chain) == 0 {
			continue
		}
		sv, err := survivors(def.Chain)
		if err != nil {
			return emptyPlan(target, observed), err
		}
		if len(sv) == 0 {
			return emptyPlan(target, observed), EmptyChainError{TargetID: target.ID, File: def.Path, Field: "polytoken.model"}
		}
		primary := sv[0].ref.Spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: def.Path, Path: []string{"polytoken", "model"}, Scalar: &primary})
		rest := make([]string, 0, len(sv)-1)
		for _, s2 := range sv[1:] {
			rest = append(rest, s2.ref.Spelling)
		}
		fb := FieldEdit{File: def.Path, Path: []string{"polytoken", "fallback_models"}}
		if len(rest) > 0 {
			fb.Sequence = rest
		} else {
			fb.Remove = true
		}
		plan.Edits = append(plan.Edits, fb)
	}

	// Baseline enable state for every managed model, deterministically ordered.
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, idStr := range ids {
		id := policy.MappingID(idStr)
		mapping := desired.Providers[id]
		mode := MappingMode(desired, observed, id)
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

	return plan, nil
}

// emptyPlan returns a no-edit plan carrying only the target identity and observed
// revision, used when a chain fails to render.
func emptyPlan(target policy.Target, observed state.State) Plan {
	return Plan{TargetID: target.ID, Revision: observed.Revision}
}

// balanceGroupOf resolves a mapping's effective balance group. A mapping without a
// quota section (or an empty balance_group) defaults to "default", matching the
// routing package's convention so two unconfigured mappings compare as the same
// group.
func balanceGroupOf(d policy.Desired, id policy.MappingID) string {
	m, ok := d.Providers[id]
	if !ok || m.Quota == nil || m.Quota.BalanceGroup == "" {
		return "default"
	}
	return m.Quota.BalanceGroup
}

// resolveSurvivors resolves, filters (disabled dropped), stable-partitions (normal then reserve),
// and applies the routing overlay to a desired chain. Both Build and EffectiveOrder call this
// so their ordering logic never diverges.
func resolveSurvivors(desired policy.Desired, observed state.State, chain policy.Chain, ranks RankLookup) ([]survivor, error) {
	var reserve []survivor
	normal := make([]survivor, 0, len(chain))
	for _, entry := range chain {
		ref, err := ParseModelRef(entry)
		if err != nil {
			return nil, err
		}
		mid, err := desired.ResolveModel(ref.Base)
		if err != nil {
			return nil, err
		}
		// A baseline-disabled model is unusable: Build writes enabled=false
		// for it into the candidate's models block, and polytoken's registry
		// rejects any subagent reference to a disabled model (the
		// disabled-fallback contract fixture). Drop it exactly like a
		// mode-disabled mapping so a survivor is always a model the same
		// candidate enables (pq-m4k8).
		if mapping, ok := desired.Providers[mid]; ok {
			if baseline, ok := mapping.Models[ref.Base]; ok && !baseline.Enabled {
				continue
			}
		}
		switch mode := MappingMode(desired, observed, mid); mode {
		case state.ModeDisabled:
			continue
		case state.ModeReserve:
			reserve = append(reserve, survivor{ref: ref, mid: mid, mode: mode})
		default:
			normal = append(normal, survivor{ref: ref, mid: mid, mode: mode})
		}
	}
	if desired.Routing.Enabled {
		applyRoutingOverlay(normal, desired, ranks)
		applyRoutingOverlay(reserve, desired, ranks)
	}
	return append(normal, reserve...), nil
}

// applyRoutingOverlay reorders survivors in place by their global rank, but only
// among entries that share a balance group and both carry a rank in the lookup.
// It uses a stable sort so entries that do not meet the reorder criteria (absent
// rank, different balance group, or equal rank) keep their original relative
// order. It never adds or removes an entry.
func applyRoutingOverlay(sv []survivor, desired policy.Desired, ranks RankLookup) {
	if len(sv) < 2 || len(ranks) == 0 {
		return
	}
	// Reorder only ranked entries within each balance group. Ranked slots remain
	// fixed, so unrelated and inter-group entries retain their exact positions.
	groups := make(map[string][]int)
	for i, s := range sv {
		if _, ok := ranks[s.mid]; !ok {
			continue
		}
		g := balanceGroupOf(desired, s.mid)
		groups[g] = append(groups[g], i)
	}
	for _, slots := range groups {
		if len(slots) < 2 {
			continue
		}
		ordered := append([]survivor(nil), sv[slots[0]])
		ordered = ordered[:0]
		for _, i := range slots {
			ordered = append(ordered, sv[i])
		}
		sort.SliceStable(ordered, func(i, j int) bool { return ranks[ordered[i].mid] < ranks[ordered[j].mid] })
		for i, slot := range slots {
			sv[slot] = ordered[i]
		}
	}
}

// EffectiveOrder returns the effective (post-overlay) model ordering for a chain
// alongside the original desired order. It is a read-only projection: it performs
// the same resolve/filter/partition/overlay as Build but returns only the spellings
// in order, never mutating desired. When routing is disabled the effective order
// equals the desired-survivor order. Disabled entries (dropped before the overlay)
// never appear.
func EffectiveOrder(desired policy.Desired, observed state.State, chain policy.Chain, ranks RankLookup) ([]string, error) {
	sv, err := resolveSurvivors(desired, observed, chain, ranks)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(sv))
	for _, s := range sv {
		out = append(out, s.ref.Spelling)
	}
	return out, nil
}
