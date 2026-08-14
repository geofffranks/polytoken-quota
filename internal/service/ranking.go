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
// snapshots → routing.ProviderObs, then calls routing.Rank. Returns the RankLookup (mapping ID → global rank) for
// reconcile.Build, plus the full RankingResult for explain/status.
//
// Only mappings carrying a Quota config participate in routing. A mapping's
// observed mode is the worst (most degraded) effective mode across its CodExBar
// providers — the same aggregation reconcile uses — and its observed snapshot is
// the most depleted among those providers (fail-closed). When usage history is
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
			continue // no quota config: not a routing participant
		}
		policies = append(policies, routing.ProviderPolicy{
			MappingID:    idStr,
			BalanceGroup: m.Quota.BalanceGroup,
			Schedule:     m.Quota.Schedule,
			FreshnessTTL: m.Quota.FreshnessTTL,
			Weight:       m.Quota.Weight,
		})
		mode, snap := aggregateMappingObs(m.CodexBarProviders, observed.Providers)
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

// aggregateMappingObs derives one mapping's observed mode and snapshot from the
// CodExBar providers that back it. The mode is the worst (most degraded)
// effective mode; the snapshot is the most depleted among the providers that
// carry one (minimum effective remaining; nil-remaining is treated as least
// depleted since it carries no depletion signal). Ties break by earliest
// CheckedAt then CodExBar provider ID, both deterministic.
func aggregateMappingObs(codexBarProviders []string, providers map[string]state.ProviderState) (state.Mode, *quota.QuotaSnapshot) {
	worst := state.ModeNormal
	var chosen *quota.QuotaSnapshot
	var chosenCB string
	for _, cb := range codexBarProviders {
		ps, ok := providers[cb]
		if !ok {
			// A configured alias without an observation makes this mapping
			// unrankable; disabled is the existing fail-closed ranking mode.
			worst = state.ModeDisabled
			continue
		}
		if mode := state.EffectiveMode(ps); modeSeverity(mode) > modeSeverity(worst) {
			worst = mode
		}
		if ps.QuotaSnapshot == nil {
			// A configured alias present without a usable snapshot is unsafe even
			// when another alias has usable headroom.
			worst = state.ModeDisabled
			continue
		}
		// An unsafe alias must not be discarded by most-depleted selection: the
		// aggregate is fail-closed even when another alias has usable headroom.
		if ps.QuotaSnapshot.Availability == quota.QuotaUnknown || ps.QuotaSnapshot.EffectiveRemaining() == nil {
			worst = state.ModeDisabled
		}
		if chosen == nil || moreDepleted(ps.QuotaSnapshot, chosen) ||
			(equalDepletion(ps.QuotaSnapshot, chosen) && (ps.QuotaSnapshot.CheckedAt.Before(chosen.CheckedAt) ||
				(ps.QuotaSnapshot.CheckedAt.Equal(chosen.CheckedAt) && cb < chosenCB))) {
			chosen = ps.QuotaSnapshot
			chosenCB = cb
		}
	}
	return worst, chosen
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
