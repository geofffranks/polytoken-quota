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
//     it to be off (reserve/disabled gating).
//   - Conflict records a detected mismatch between the owned expectation and
//     the live field (e.g. an operator edit) that must be reported, never
//     overwritten, while the claim is held.
type ProviderOwnership struct {
	BaselinePresent bool
	BaselineValue   bool
	Owned           bool
	Conflict        bool
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

// sanitizeProviderOwnership re-sanitizes the ownership map's provider-ID keys
// immediately before persisting: control characters are stripped and keys are
// bounded so a stale or hand-edited state file cannot carry hostile key bytes
// into the next process lifetime. Values are plain booleans and need no
// sanitization. It never mutates the input state.
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
