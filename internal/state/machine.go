package state

import (
	"errors"
	"fmt"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/hook"
)

// EffectiveMode derives the reconciler-internal effective mode from a provider's
// independent quota and availability axes:
//
//	unavailable OR exhausted => disabled
//	available  AND low       => reserve
//	available  AND normal    => normal
func EffectiveMode(ps ProviderState) Mode {
	if ps.ManualDisabled || ps.Availability == Unavailable || ps.Quota == QuotaExhausted {
		return ModeDisabled
	}
	if ps.Availability == Available && ps.Quota == QuotaLow {
		return ModeReserve
	}
	return ModeNormal
}

// axis identifies which independent condition an event mutates.
type axis int

const (
	axisNone axis = iota
	axisQuota
	axisAvailability
)

func eventAxis(t hook.Type) axis {
	switch t {
	case hook.QuotaLow, hook.QuotaReached, hook.QuotaReset:
		return axisQuota
	case hook.ProviderUnavailable, hook.ProviderRecovered:
		return axisAvailability
	}
	return axisNone
}

func axisName(a axis) string {
	switch a {
	case axisQuota:
		return "quota"
	case axisAvailability:
		return "availability"
	}
	return "unknown"
}

// compareArrival orders a candidate event against the last accepted state for
// its axis. It returns >0 when the candidate is newer and should be accepted, <0
// when it is stale and must be ignored, and 0 when it is an exact duplicate.
// Usable (non-zero) timestamps dominate; equal or absent timestamps fall back to
// the arrival sequence.
func compareArrival(newTS time.Time, newSeq uint64, oldTS time.Time, oldSeq uint64) int {
	newOK := !newTS.IsZero()
	oldOK := !oldTS.IsZero()
	switch {
	case newOK && oldOK:
		switch {
		case newTS.After(oldTS):
			return 1
		case newTS.Before(oldTS):
			return -1
		}
		// Equal usable timestamps: fall through to the sequence tiebreak.
	case newOK && !oldOK:
		return 1
	case !newOK && oldOK:
		return -1
	}
	// Equal timestamps or both unusable: tiebreak by arrival sequence.
	switch {
	case newSeq > oldSeq:
		return 1
	case newSeq < oldSeq:
		return -1
	}
	return 0
}

// ApplyEvent applies a single validated hook event to the state machine and
// returns the resulting state, whether the event was accepted, a diagnostic
// (populated for stale outcomes), and an error.
//
// Quota and availability are independent axes: an event mutates only its own
// axis, so a recovery never clears quota and a reset never clears availability.
// An event whose timestamp (or, when equal or absent, arrival sequence) is not
// newer than the last accepted value for its axis is stale and ignored with a
// diagnostic. Duplicate events are idempotent. refresh_failed records bounded
// diagnostic metadata only and never changes model eligibility. ApplyEvent
// never mutates the input state.
func ApplyEvent(s State, e hook.Event, a Arrival) (State, bool, Diagnostic, error) {
	switch e.Type {
	case hook.QuotaLow, hook.QuotaReached, hook.QuotaReset, hook.ProviderUnavailable, hook.ProviderRecovered:
		return applyAxisEvent(s, e, a)
	case hook.RefreshFailed:
		return applyRefreshFailed(s, e)
	default:
		return s, false, Diagnostic{}, fmt.Errorf("state: unknown event type %q", e.Type)
	}
}

// applyAxisEvent handles the four quota/availability transition events on their
// own independent axis.
func applyAxisEvent(s State, e hook.Event, a Arrival) (State, bool, Diagnostic, error) {
	ax := eventAxis(e.Type)

	providers := make(map[string]ProviderState, len(s.Providers)+1)
	for k, v := range s.Providers {
		providers[k] = v
	}
	ps, ok := providers[e.Provider]
	if !ok {
		ps = ProviderState{Quota: QuotaNormal, Availability: Available}
	}

	var oldTS time.Time
	var oldSeq uint64
	if ax == axisQuota {
		oldTS, oldSeq = ps.QuotaAt, ps.QuotaArrival
	} else {
		oldTS, oldSeq = ps.AvailabilityAt, ps.AvailabilityArrival
	}

	if cmp := compareArrival(e.Timestamp, a.Sequence, oldTS, oldSeq); cmp < 0 {
		diag := Diagnostic{
			Code:     "stale",
			Provider: e.Provider,
			Summary:  fmt.Sprintf("ignored stale %s event older than last accepted %s state", e.Type, axisName(ax)),
			At:       e.Timestamp,
		}
		return s, false, diag, nil
	}

	switch ax {
	case axisQuota:
		switch e.Type {
		case hook.QuotaLow:
			ps.Quota = QuotaLow
		case hook.QuotaReached:
			ps.Quota = QuotaExhausted
		case hook.QuotaReset:
			ps.Quota = QuotaNormal
		}
		ps.QuotaAt = e.Timestamp
		ps.QuotaArrival = a.Sequence
	case axisAvailability:
		switch e.Type {
		case hook.ProviderUnavailable:
			ps.Availability = Unavailable
		case hook.ProviderRecovered:
			ps.Availability = Available
		}
		ps.AvailabilityAt = e.Timestamp
		ps.AvailabilityArrival = a.Sequence
	}
	providers[e.Provider] = ps

	next := s
	next.Providers = providers
	return next, true, Diagnostic{}, nil
}

// applyRefreshFailed records the latest refresh_failed diagnostic for the event's
// provider (at most one entry per provider) without changing eligibility. It is
// always accepted.
func applyRefreshFailed(s State, e hook.Event) (State, bool, Diagnostic, error) {
	summary := ""
	if e.Status != nil {
		summary = *e.Status
	}
	diag := Diagnostic{
		Code:     string(e.Type),
		Provider: e.Provider,
		Summary:  summary,
		At:       e.Timestamp,
	}
	refresh := make([]Diagnostic, 0, len(s.RefreshFailed)+1)
	for _, d := range s.RefreshFailed {
		if d.Provider == e.Provider {
			continue // drop the stale entry for this provider
		}
		refresh = append(refresh, d)
	}
	refresh = append(refresh, diag)
	next := s
	next.RefreshFailed = refresh
	return next, true, Diagnostic{}, nil
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
	return updateManualDisable(s, provider, true, at)
}

// EnableProvider clears one provider's manual disable without changing its
// automatic quota or availability observations.
func EnableProvider(s State, provider string, at time.Time) (State, error) {
	if provider == "" {
		return s, errors.New("state: enable requires a provider")
	}
	return updateManualDisable(s, provider, false, at)
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

func updateManualDisable(s State, provider string, disabled bool, at time.Time) (State, error) {
	providers := make(map[string]ProviderState, len(s.Providers)+1)
	for k, v := range s.Providers {
		providers[k] = v
	}
	ps := providers[provider]
	if ps.Quota == "" {
		ps.Quota = QuotaNormal
	}
	if ps.Availability == "" {
		ps.Availability = Available
	}
	ps.ManualDisabled = disabled
	ps.ManualDisabledAt = at
	providers[provider] = ps
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
