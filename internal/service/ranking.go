package service

// Ranking computation: builds the routing.RankingInput from the desired policy
// and observed state, calls routing.Rank, and produces the reconcile.RankLookup
// consumed by reconcile.Build. This is the service-layer wiring between the
// durable policy/state and the pure routing policy.

import (
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ComputeRanking builds the routing ranking from the desired policy and observed
// state. It maps provider quota configs → routing.ProviderPolicy, state
// snapshots → routing.ProviderObs, then calls routing.Rank. Returns the
// RankLookup (mapping ID → global rank) for reconcile.Build, plus the full
// RankingResult for explain/status.
//
// Only mappings carrying a Quota config participate in routing. A mapping's
// observed mode comes from the provider mapping's state entry — the same
// mapping-ID aggregation reconcile uses — and its observed snapshot is selected
// fail-closed. When usage history is
// absent or has no usable totals, the usage key is skipped for every group
// (routing treats absent shares as incomparable).
func ComputeRanking(desired policy.Desired, observed state.State, now time.Time) (reconcile.RankLookup, routing.RankingResult) {
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)

	policies := make([]routing.ProviderPolicy, 0, len(ids))
	obs := make([]routing.ProviderObs, 0, len(ids))
	for _, idStr := range ids {
		m := desired.Providers[policy.MappingID(idStr)]
		if m.Quota == nil {
			// Unpollable mappings stay visible in diagnostics but cannot affect
			// quota ranking or the routing overlay.
			continue
		}
		policies = append(policies, routing.ProviderPolicy{
			MappingID:    idStr,
			BalanceGroup: m.Quota.BalanceGroup,
			Schedule:     m.Quota.Schedule,
			FreshnessTTL: m.Quota.FreshnessTTL,
			Weight:       m.Quota.Weight,
		})
		mode, snap := aggregateMappingObs(idStr, observed.Providers)
		obs = append(obs, routing.ProviderObs{
			MappingID: idStr,
			Mode:      string(mode),
			Snapshot:  snap,
		})
	}

	result := routing.Rank(routing.RankingInput{
		Now:      now,
		Policies: policies,
		Obs:      obs,
	})

	lookup := make(reconcile.RankLookup, len(result.Entries))
	for _, e := range result.Entries {
		if e.Eligible {
			lookup[policy.MappingID(e.MappingID)] = e.Rank
		}
	}
	return lookup, result
}

// aggregateMappingObs derives one mapping's observed mode and snapshot from its
// single mapping-ID state entry. Missing or unsafe observations are fail-closed.
func aggregateMappingObs(mappingID string, providers map[string]state.ProviderState) (state.Mode, *quota.QuotaSnapshot) {
	ps, ok := providers[mappingID]
	if !ok {
		return state.ModeDisabled, nil
	}
	mode := state.EffectiveMode(ps)
	if ps.QuotaSnapshot == nil {
		return state.ModeDisabled, nil
	}
	if ps.QuotaSnapshot.Availability == quota.QuotaUnknown || ps.QuotaSnapshot.EffectiveRemaining() == nil {
		mode = state.ModeDisabled
	}
	return mode, ps.QuotaSnapshot
}

// modeSeverity orders modes from least to most degraded, matching reconcile's
// internal modeRank so aggregation is consistent with survivor partitioning.
func modeSeverity(m state.Mode) int {
	switch m {
	case state.ModeDisabled:
		return 2
	case state.ModeReserve:
		return 1
	default:
		return 0
	}
}

// moreDepleted reports whether a is strictly more depleted than b: a reports a
// usable remaining fraction and b does not, or both do and a's is smaller.
func moreDepleted(a, b *quota.QuotaSnapshot) bool {
	ar, br := a.EffectiveRemaining(), b.EffectiveRemaining()
	if ar != nil && br == nil {
		return true
	}
	if ar == nil || br == nil {
		return false
	}
	return *ar < *br
}

// equalDepletion reports whether a and b are equally depleted (both nil-remaining,
// or both usable with equal values).
func equalDepletion(a, b *quota.QuotaSnapshot) bool {
	ar, br := a.EffectiveRemaining(), b.EffectiveRemaining()
	if ar == nil && br == nil {
		return true
	}
	if ar == nil || br == nil {
		return false
	}
	return *ar == *br
}

// usageShares removed: pace projection uses only the current snapshot, not
// usage history. See docs/superpowers/pace-projection-routing/design_spec.md.
