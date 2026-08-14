package service

import (
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// aggregateMappingState projects one mapping-ID state entry into the state used
// by read-only diagnostics. Missing or unsafe observations fail closed.
func aggregateMappingState(mappingID string, providers map[string]state.ProviderState) state.ProviderState {
	ps, ok := providers[mappingID]
	if !ok {
		return state.ProviderState{Availability: state.Unavailable, Quota: state.QuotaExhausted}
	}
	out := ps
	if ps.Availability == state.Unavailable || ps.QuotaSnapshot == nil ||
		ps.QuotaSnapshot.Availability != quota.QuotaAvailable || ps.QuotaSnapshot.EffectiveRemaining() == nil {
		out.Availability = state.Unavailable
		if out.QuotaSnapshot != nil {
			copy := *out.QuotaSnapshot
			copy.Availability = quota.QuotaUnknown
			out.QuotaSnapshot = &copy
		}
	}
	return out
}

func aggregateProviderNames(desiredProviders map[string][]string, observed map[string]state.ProviderState) []string {
	names := make(map[string]bool, len(desiredProviders)+len(observed))
	for id := range desiredProviders {
		names[id] = true
	}
	for name, ps := range observed {
		if _, configured := desiredProviders[name]; !configured && (ps.QuotaSnapshot != nil || ps.QuotaAttempt != nil) {
			names[name] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	return out
}
