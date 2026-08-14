package state

import (
	"errors"
	"fmt"
	"time"
)

// EffectiveMode derives the reconciler-internal effective mode from a provider's
// independent quota and availability axes:
//
//	unavailable OR exhausted     => disabled
//	available  AND low           => reserve
//	available  AND normal        => normal
//	unknown/corrupted axis value => disabled (fail closed)
//
// Zero values ("" on either axis) are legacy/sparse states normalized to the
// healthy baseline for that axis, matching how manual-disable updates seed
// untracked providers. Any other unrecognized value is a corrupted observation
// and must never enable a provider.
func EffectiveMode(ps ProviderState) Mode {
	q, av := ps.Quota, ps.Availability
	if q == "" {
		q = QuotaNormal
	}
	if av == "" {
		av = Available
	}
	if ps.ManualDisabled || av == Unavailable || q == QuotaExhausted {
		return ModeDisabled
	}
	if !validQuota(q) || !validAvailability(av) {
		return ModeDisabled
	}
	if av == Available && q == QuotaLow {
		return ModeReserve
	}
	return ModeNormal
}

// SetProvider applies an authoritative manual override to a single provider
// (the `state set` primitive). Each non-nil patch field is written directly with
// the given timestamp, bypassing staleness checks. An empty patch or empty
// provider is rejected.
func SetProvider(s State, provider string, patch ProviderPatch, at time.Time) (State, error) {
	if provider == "" {
		return s, errors.New("state: set requires a provider")
	}
	if patch.Quota == nil && patch.Availability == nil {
		return s, errors.New("state: set requires at least one of quota or availability")
	}
	if patch.Quota != nil && !validQuota(*patch.Quota) {
		return s, fmt.Errorf("state: invalid quota %q", *patch.Quota)
	}
	if patch.Availability != nil && !validAvailability(*patch.Availability) {
		return s, fmt.Errorf("state: invalid availability %q", *patch.Availability)
	}

	providers := make(map[string]ProviderState, len(s.Providers)+1)
	for k, v := range s.Providers {
		providers[k] = v
	}
	ps, ok := providers[provider]
	if !ok {
		ps = ProviderState{Quota: QuotaNormal, Availability: Available}
	}
	if patch.Quota != nil {
		ps.Quota = *patch.Quota
		ps.QuotaAt = at
	}
	if patch.Availability != nil {
		ps.Availability = *patch.Availability
		ps.AvailabilityAt = at
	}
	providers[provider] = ps

	next := s
	next.Providers = providers
	return next, nil
}

// ManualDisableChanged reports whether applying the requested manual-disable
// value changes any alias in the batch.
func ManualDisableChanged(s State, providers []string, disabled bool) bool {
	for _, provider := range providers {
		if s.Providers[provider].ManualDisabled != disabled {
			return true
		}
	}
	return false
}

// ClearChanged reports whether clearing the selected provider axes changes any
// tracked provider.
func ClearChanged(s State, sel Selector) bool {
	for provider, ps := range s.Providers {
		if (sel.All || provider == sel.Provider) && (ps.Quota != QuotaNormal || ps.Availability != Available) {
			return true
		}
	}
	return false
}

// SetChanged reports whether any requested patch field differs from the current
// provider state.
func SetChanged(s State, provider string, patch ProviderPatch) bool {
	ps := s.Providers[provider]
	if patch.Quota != nil && ps.Quota != *patch.Quota {
		return true
	}
	if patch.Availability != nil && ps.Availability != *patch.Availability {
		return true
	}
	return false
}

// ClearProvider resets the provider(s) identified by the selector to the healthy
// baseline (quota normal, availability available) using the given timestamp. It
// is the repair primitive behind `state clear`/`--all`. Clearing an untracked
// provider is a no-op.
func ClearProvider(s State, sel Selector, at time.Time) (State, error) {
	if !sel.All && sel.Provider == "" {
		return s, errors.New("state: clear requires a provider or --all")
	}
	providers := make(map[string]ProviderState, len(s.Providers))
	for k, v := range s.Providers {
		if sel.All || k == sel.Provider {
			v.Quota = QuotaNormal
			v.Availability = Available
			v.QuotaAt = at
			v.AvailabilityAt = at
		}
		providers[k] = v
	}
	next := s
	next.Providers = providers
	return next, nil
}

// DisableProvider marks one provider as manually disabled without changing its
// automatic quota or availability observations.
func DisableProvider(s State, provider string, at time.Time) (State, error) {
	if provider == "" {
		return s, errors.New("state: disable requires a provider")
	}
	return SetManualDisabled(s, []string{provider}, true, at)
}

// EnableProvider clears one provider's manual disable without changing its
// automatic quota or availability observations.
func EnableProvider(s State, provider string, at time.Time) (State, error) {
	if provider == "" {
		return s, errors.New("state: enable requires a provider")
	}
	return SetManualDisabled(s, []string{provider}, false, at)
}

// SetManualDisabled updates every supplied provider mapping ID as one immutable
// state transition, preserving all automatic quota and availability observations.
func SetManualDisabled(s State, providers []string, disabled bool, at time.Time) (State, error) {
	nextProviders := make(map[string]ProviderState, len(s.Providers)+len(providers))
	for k, v := range s.Providers {
		nextProviders[k] = v
	}
	for _, provider := range providers {
		if provider == "" {
			return s, errors.New("state: manual routing control requires non-empty provider mapping IDs")
		}
		ps := nextProviders[provider]
		if ps.Quota == "" {
			ps.Quota = QuotaNormal
		}
		if ps.Availability == "" {
			ps.Availability = Available
		}
		ps.ManualDisabled = disabled
		ps.ManualDisabledAt = at
		nextProviders[provider] = ps
	}
	next := s
	next.Providers = nextProviders
	return next, nil
}

// ResetManualDisables clears manual disables for every tracked provider while
// preserving automatic quota and availability observations and metadata.
func ResetManualDisables(s State, at time.Time) (State, error) {
	providers := make(map[string]ProviderState, len(s.Providers))
	for provider, ps := range s.Providers {
		if ps.ManualDisabled {
			ps.ManualDisabled = false
			ps.ManualDisabledAt = at
		}
		providers[provider] = ps
	}
	next := s
	next.Providers = providers
	return next, nil
}

func validQuota(q Quota) bool {
	switch q {
	case QuotaNormal, QuotaLow, QuotaExhausted:
		return true
	}
	return false
}

func validAvailability(a Availability) bool {
	switch a {
	case Available, Unavailable:
		return true
	}
	return false
}
