// Package quota holds the normalized quota observation types that provider
// adapters populate and the routing policy consumes. These types never carry
// credentials, account names, auth headers, or raw provider response bodies —
// only sanitized observations.
//
// The package is a leaf dependency: it imports only the standard library and
// must never import the state package (the dependency is one-directional,
// state -> quota).
package quota

import (
	"regexp"
	"time"
)

// QuotaAvailability is the provider's observed availability.
type QuotaAvailability string

const (
	QuotaAvailable   QuotaAvailability = "available"
	QuotaUnavailable QuotaAvailability = "unavailable"
	QuotaUnknown     QuotaAvailability = "unknown"
)

// SourceStatus describes how trustworthy a snapshot's source is.
type SourceStatus string

const (
	SourceFresh   SourceStatus = "fresh"
	SourcePartial SourceStatus = "partial"
	SourceFailed  SourceStatus = "failed"
)

// QuotaWindow is a single usage window within a snapshot. Pointer fields are
// nil when the provider did not report that value; reset times are never
// invented.
type QuotaWindow struct {
	Name         string
	Used         *float64
	Limit        *float64
	UsagePercent *float64
	ResetAt      *time.Time
}

// QuotaSnapshot is the aggregate observation for one provider mapping. It
// carries only sanitized observations and a sanitized error summary (never raw
// HTTP bodies, URLs with credentials, auth headers, or account IDs).
type QuotaSnapshot struct {
	MappingID    string
	CheckedAt    time.Time
	Windows      []QuotaWindow
	Availability QuotaAvailability
	Status       SourceStatus
	Error        string
}

// QuotaClass is the derived quota bucket the routing policy consumes. Unknown
// never drives a demotion on its own.
type QuotaClass string

const (
	ClassNormal    QuotaClass = "normal"
	ClassLow       QuotaClass = "low"
	ClassExhausted QuotaClass = "exhausted"
	ClassUnknown   QuotaClass = "unknown"
)

// lowRemainingThreshold is the remaining fraction at or above which a window is
// still "normal"; below it the window is "low". Exactly 1/3 per the spec.
const lowRemainingThreshold = 1.0 / 3.0

// Remaining returns the clamped remaining fraction (0.0–1.0) for this window,
// preferring a trustworthy used/limit fraction and falling back to
// usage_percent. It returns nil when the window reports neither. This is the
// single source of truth for the per-window remaining fraction; both the
// routing policy and the status display derive from it so they cannot diverge.
func (w QuotaWindow) Remaining() *float64 {
	if w.Used != nil && w.Limit != nil && *w.Limit > 0 {
		return clamp01((*w.Limit - *w.Used) / *w.Limit)
	}
	if w.UsagePercent != nil {
		return clamp01((100 - *w.UsagePercent) / 100)
	}
	return nil
}

// clamp01 bounds v to the [0, 1] range and returns a pointer to the result.
func clamp01(v float64) *float64 {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return &v
}

// EffectiveRemaining returns the minimum remaining fraction (0.0–1.0) among
// usable windows — those that report a trustworthy used/limit fraction or a
// usage_percent. It returns nil when no window is usable.
func (s QuotaSnapshot) EffectiveRemaining() *float64 {
	var min *float64
	for _, w := range s.Windows {
		rem := w.Remaining()
		if rem == nil {
			continue
		}
		if min == nil || *rem < *min {
			r := *rem
			min = &r
		}
	}
	return min
}

// NextResetAt returns the earliest future ResetAt among windows that report one.
// "Future" is relative to CheckedAt (the observation time); a ResetAt already in
// the past at observation time is ignored. It returns nil when no window reports
// a reset time.
func (s QuotaSnapshot) NextResetAt() *time.Time {
	var earliest *time.Time
	for _, w := range s.Windows {
		if w.ResetAt == nil {
			continue
		}
		rt := *w.ResetAt
		if !s.CheckedAt.IsZero() && !rt.After(s.CheckedAt) {
			continue // not future relative to the observation
		}
		if earliest == nil || rt.Before(*earliest) {
			t := rt
			earliest = &t
		}
	}
	return earliest
}

// Class derives the quota bucket from the snapshot's usable windows: exhausted
// at zero remaining, low below ~33% (exactly 1/3), normal otherwise. It returns
// ClassUnknown when no usable limit is reported — in that case the class never
// drives a demotion.
func (s QuotaSnapshot) Class() QuotaClass {
	rem := s.EffectiveRemaining()
	if rem == nil {
		return ClassUnknown
	}
	switch {
	case *rem <= 0:
		return ClassExhausted
	case *rem < lowRemainingThreshold:
		return ClassLow
	default:
		return ClassNormal
	}
}

// maxErrorSummary bounds the length of a sanitized error summary.
const maxErrorSummary = 512

// secretPatterns strips known secret-bearing substrings from an error string.
// Order matters: URLs with embedded credentials are reduced to their scheme
// first, then bearer tokens and key/value secrets are redacted.
var (
	reURLCreds = regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`)
	reBearer   = regexp.MustCompile(`(?i)\b(bearer)\s+[^\s]+`)
	reSecretKV = regexp.MustCompile(`(?i)(api[_-]?key|apikey|token|secret|password|passwd|account|acct)(\s*[=:]\s*)?[^\s]+`)
)

// sanitize strips known secret patterns from s and bounds its length. It never
// preserves credentials, bearer tokens, or account-like substrings.
func sanitize(s string) string {
	s = reURLCreds.ReplaceAllString(s, "$1")
	s = reBearer.ReplaceAllString(s, "[redacted]")
	s = reSecretKV.ReplaceAllString(s, "[redacted]")
	if len(s) > maxErrorSummary {
		// Keep the UTF-8 output within the byte bound while reserving space for
		// the three-byte ellipsis. Walk runes so truncation never splits UTF-8.
		limit := maxErrorSummary - len("…")
		end := 0
		for _, r := range s {
			n := len(string(r))
			if end+n > limit {
				break
			}
			end += n
		}
		s = s[:end] + "…"
	}
	return s
}

// SanitizeError returns a bounded, generic summary of err with known secret
// patterns (URLs with embedded credentials, bearer tokens, account-like and
// key=value secrets) stripped. A nil error returns the empty string.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error())
}

// SanitizeText returns s with known secret patterns stripped and its length
// bounded. It is the string equivalent of SanitizeError for pre-extracted
// strings (e.g. persisted snapshot errors read from state.json) that must be
// sanitized a second time for defense in depth.
func SanitizeText(s string) string {
	return sanitize(s)
}
