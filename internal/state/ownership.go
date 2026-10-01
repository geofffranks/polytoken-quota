package state

import "unicode"

// Provider ownership metadata: the durable record of the reconciler's
// ownership claim over one enrolled global provider's `enabled` field. This is
// the additive schema prerequisite for provider-only gating; it records
// boolean facts only and never carries credentials, account names, raw config,
// or free text.

// OwnershipKeyBytes bounds a persisted provider-ownership map key (a provider
// ID) after control-character stripping. Enrollment keys are short config
// identifiers; the bound only defends against a hand-edited state file.
const OwnershipKeyBytes = 128

// ProviderOwnership is the sanitized ownership record for a single enrolled
// provider ID:
//   - BaselinePresent records whether the operator's `providers.<id>.enabled`
//     key existed when the baseline was captured; BaselineValue records its
//     value when present. Absent keys and explicit false are distinct so
//     normal mode can restore the exact original shape.
//   - Owned records that the reconciler wrote the field off and still expects
//     it to be off (reserve/disabled/pace gating).
//   - Conflict records a detected mismatch between the owned expectation and
//     the live field (e.g. an operator edit) that must be reported, never
//     overwritten, while the claim is held.
//   - Axis names the gating axis that engaged the claim: one of the
//     OwnershipAxis constants, or the empty string for a legacy claim (a
//     record written before axis attribution). Threshold and EngagedRevision
//     carry the pace threshold and the revision at which a pace claim was
//     engaged; they are inert on non-pace claims.
type ProviderOwnership struct {
	BaselinePresent bool
	BaselineValue   bool
	Owned           bool
	Conflict        bool
	Axis            string
	Threshold       float64
	EngagedRevision uint64
}

// Ownership gate axes. The empty string is the legacy-claim axis: a claim
// recorded before attribution, owned expected-off with no axis semantics
// (never signal-held in the pool rule, never blocks recovery).
const (
	OwnershipAxisLegacy   = ""
	OwnershipAxisSignal   = "signal"
	OwnershipAxisReserve  = "reserve"
	OwnershipAxisDisabled = "disabled"
)

// ValidOwnershipAxis reports whether axis is a representable ownership axis.
// Load maps any other value to the legacy axis, so a hand-edited state file
// can never smuggle an unbounded string into gate decisions.
func ValidOwnershipAxis(axis string) bool {
	switch axis {
	case OwnershipAxisLegacy, OwnershipAxisSignal, OwnershipAxisReserve, OwnershipAxisDisabled:
		return true
	}
	return false
}

// SignalHeld reports whether the record is a claim currently held by the
// signal axis (as opposed to reserve/disabled gating or a legacy claim).
func (o ProviderOwnership) SignalHeld() bool {
	return o.Owned && o.Axis == OwnershipAxisSignal
}

// OwnershipOf returns the ownership record for provider id and whether one
// exists. A nil map reads as no ownership records.
func (s State) OwnershipOf(id string) (ProviderOwnership, bool) {
	o, ok := s.ProviderOwnership[id]
	return o, ok
}

// WithOwnership returns a copy of s with the ownership record for provider id
// set to o. The input state is never mutated; a nil map is initialized lazily
// in the copy.
func (s State) WithOwnership(id string, o ProviderOwnership) State {
	next := s
	if next.ProviderOwnership == nil {
		next.ProviderOwnership = map[string]ProviderOwnership{}
	} else {
		next.ProviderOwnership = CloneProviderOwnership(next.ProviderOwnership)
	}
	next.ProviderOwnership[id] = o
	return next
}

// CloneProviderOwnership returns a deep copy of m (nil stays nil) so derived
// states never share the ownership map with their input.
func CloneProviderOwnership(m map[string]ProviderOwnership) map[string]ProviderOwnership {
	if m == nil {
		return nil
	}
	out := make(map[string]ProviderOwnership, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalizeProviderOwnershipAxes maps every ownership record's axis to a
// representable value immediately after load and immediately before persist:
// an unknown or out-of-enum axis value falls back to the legacy axis (with its
// inert attribution fields cleared), so a hand-edited state file can never
// carry an unbounded axis string into gate decisions and every persisted
// record carries canonical axis values. It never mutates the input state.
func normalizeProviderOwnershipAxes(s State) State {
	if len(s.ProviderOwnership) == 0 {
		return s
	}
	changed := false
	out := make(map[string]ProviderOwnership, len(s.ProviderOwnership))
	for k, v := range s.ProviderOwnership {
		if ValidOwnershipAxis(v.Axis) {
			out[k] = v
			continue
		}
		// Unknown axis: legacy-claim semantics. The record still means
		// "owned expected-off" (recovery unaffected), but it is never
		// pace-held and carries no pace attribution.
		v.Axis = OwnershipAxisLegacy
		v.Threshold = 0
		v.EngagedRevision = 0
		out[k] = v
		changed = true
	}
	if !changed {
		return s
	}
	next := s
	next.ProviderOwnership = out
	return next
}

// sanitizeProviderOwnership re-sanitizes the ownership map's provider-ID keys
// immediately before persisting: control characters are stripped and keys are
// bounded so a stale or hand-edited state file cannot carry hostile key bytes
// into the next process lifetime. Values need no byte sanitization: their
// fields are booleans, bounded numbers, and an axis enum that
// normalizeProviderOwnershipAxes constrains to known values. Neither helper
// ever mutates the input state.
func sanitizeProviderOwnership(s State) State {
	if len(s.ProviderOwnership) == 0 {
		return s
	}
	out := make(map[string]ProviderOwnership, len(s.ProviderOwnership))
	changed := false
	for k, v := range s.ProviderOwnership {
		cleaned := sanitizeOwnershipKey(k)
		if cleaned != k {
			changed = true
		}
		out[cleaned] = v
	}
	if !changed {
		return s
	}
	next := s
	next.ProviderOwnership = out
	return next
}

// sanitizeOwnershipKey strips control characters from a provider-ID key and
// bounds its length.
func sanitizeOwnershipKey(k string) string {
	cleaned := k
	if hasControl(cleaned) {
		runes := []rune(cleaned)
		kept := make([]rune, 0, len(runes))
		for _, r := range runes {
			if r == 0 || unicode.IsControl(r) {
				continue
			}
			kept = append(kept, r)
		}
		cleaned = string(kept)
	}
	return truncateUTF8(cleaned, OwnershipKeyBytes)
}
