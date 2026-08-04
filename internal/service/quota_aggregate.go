package service

import (
	"github.com/geofffranks/codexbar-hooks/internal/quota"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// aggregateMappingState projects the CodexBar provider states backing one
// mapping into the state used by read-only diagnostics. It mirrors ranking's
// fail-closed grouping: the worst effective mode and unavailable availability
// win, while snapshots use the most depleted observation. Attempts preserve a
// failed attempt when any backing provider failed; otherwise the latest attempt
// is selected deterministically.
func aggregateMappingState(codexBarProviders []string, providers map[string]state.ProviderState) state.ProviderState {
	var out state.ProviderState
	_, out.QuotaSnapshot = aggregateMappingObs(codexBarProviders, providers)
	var chosenAttempt *quota.QuotaSnapshot
	var chosenProvider string
	for _, cb := range codexBarProviders {
		ps, ok := providers[cb]
		if !ok {
			// A configured alias with no observation is unknown, so aggregate
			// availability must fail closed rather than use a partial view.
			out.Availability = state.Unavailable
			continue
		}
		if state.EffectiveMode(ps) == state.ModeDisabled {
			out.Quota = state.QuotaExhausted
		} else if out.Quota != state.QuotaExhausted && state.EffectiveMode(ps) == state.ModeReserve {
			out.Quota = state.QuotaLow
		}
		if ps.Availability == state.Unavailable || ps.QuotaSnapshot == nil || (ps.QuotaSnapshot.Availability != quota.QuotaAvailable || ps.QuotaSnapshot.EffectiveRemaining() == nil) {
			// Unknown availability, missing snapshots, and snapshots without an
			// effective remaining signal are unsafe even when another alias is usable.
			out.Availability = state.Unavailable
		} else if out.Availability == "" {
			out.Availability = state.Available
		}
		if ps.QuotaAttempt == nil {
			continue
		}
		if chosenAttempt == nil ||
			(ps.QuotaAttempt.Status == quota.SourceFailed && chosenAttempt.Status != quota.SourceFailed) ||
			(ps.QuotaAttempt.Status == chosenAttempt.Status && (ps.QuotaAttempt.CheckedAt.After(chosenAttempt.CheckedAt) ||
				(ps.QuotaAttempt.CheckedAt.Equal(chosenAttempt.CheckedAt) && cb < chosenProvider))) {
			chosenAttempt = ps.QuotaAttempt
			chosenProvider = cb
		}
	}
	out.QuotaAttempt = chosenAttempt
	if out.QuotaSnapshot != nil {
		copy := *out.QuotaSnapshot
		if out.Availability == state.Unavailable {
			copy.Availability = quota.QuotaUnknown
		}
		out.QuotaSnapshot = &copy
	}
	return out
}

func hasMissingAlias(aliases []string, providers map[string]state.ProviderState) bool {
	for _, alias := range aliases {
		if _, ok := providers[alias]; !ok {
			return true
		}
	}
	return false
}

func allAliasesMissing(aliases []string, providers map[string]state.ProviderState) bool {
	for _, alias := range aliases {
		if _, ok := providers[alias]; ok {
			return false
		}
	}
	return true
}

func aggregateProviderNames(desiredProviders map[string][]string, observed map[string]state.ProviderState) []string {
	covered := make(map[string]bool)
	for id, aliases := range desiredProviders {
		_ = id
		for _, alias := range aliases {
			covered[alias] = true
		}
	}
	names := make(map[string]bool, len(desiredProviders)+len(observed))
	for id := range desiredProviders {
		names[id] = true
	}
	for name, ps := range observed {
		if !covered[name] && (ps.QuotaSnapshot != nil || ps.QuotaAttempt != nil) {
			names[name] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	return out
}
