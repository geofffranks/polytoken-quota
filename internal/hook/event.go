// Package hook decodes CodexBar hook events delivered on stdin.
//
// Decode pins the CodexBar 0.44.0 hook event contract: a single bounded JSON
// object describing a quota or provider event, cross-checked against the
// CODEXBAR_* environment snapshot CodexBar sets alongside it. Account is parsed
// from the wire but discarded before the normalized Event is returned, so it can
// never reach persistence or logging.
package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// defaultMaxBytes matches CodexBar's stdin payload cap.
const defaultMaxBytes int64 = 4096

// Supported CODEXBAR_* environment keys. Identity keys cross-check the JSON
// payload; the remainder are optional fallbacks for fields JSON omits. Account
// is intentionally absent here: it is read from the wire only to be discarded.
const (
	envEvent        = "CODEXBAR_EVENT"
	envProvider     = "CODEXBAR_PROVIDER"
	envTimestamp    = "CODEXBAR_TIMESTAMP"
	envWindow       = "CODEXBAR_WINDOW"
	envUsagePercent = "CODEXBAR_USAGE_PERCENT"
	envUsed         = "CODEXBAR_USED"
	envLimit        = "CODEXBAR_LIMIT"
	envResetAt      = "CODEXBAR_RESET_AT"
	envStatus       = "CODEXBAR_STATUS"
)

// Type is the CodexBar hook event type. The raw values are the stable event
// names used in config, env vars, and the JSON payload.
type Type string

// The six CodexBar 0.44.0 hook event types.
const (
	QuotaLow            Type = "quota_low"
	QuotaReached        Type = "quota_reached"
	QuotaReset          Type = "quota_reset"
	ProviderUnavailable Type = "provider_unavailable"
	ProviderRecovered   Type = "provider_recovered"
	RefreshFailed       Type = "refresh_failed"
)

// Event is a decoded, normalized CodexBar hook event. The optional pointer
// fields are populated only when CodexBar includes them on the wire or via env
// fallback. Account is deliberately absent: it is decode-only and discarded.
type Event struct {
	Type         Type
	Provider     string
	Window       *string
	UsagePercent *float64
	Used         *float64
	Limit        *float64
	ResetAt      *time.Time
	Status       *string
	Timestamp    time.Time
}

// wireEvent mirrors the CodexBar JSON payload. camelCase tags match the contract
// keys; Account is parsed here only to be dropped before normalization.
type wireEvent struct {
	Event        Type       `json:"event"`
	Provider     string     `json:"provider"`
	Account      *string    `json:"account"`
	Window       *string    `json:"window"`
	UsagePercent *float64   `json:"usagePercent"`
	Used         *float64   `json:"used"`
	Limit        *float64   `json:"limit"`
	ResetAt      *time.Time `json:"resetAt"`
	Status       *string    `json:"status"`
	Timestamp    time.Time  `json:"timestamp"`
}

// Decode reads exactly one bounded JSON object from r, validates it against the
// CodexBar 0.44.0 contract, cross-checks the identity fields against env, fills
// omitted optional fields from env, and returns the normalized Event.
//
// JSON is authoritative: identity (event/provider/timestamp) must be present and
// valid in the JSON, and env values for those fields must agree when present.
// Optional fields fall back to env only when JSON omits them. All numeric fields
// must be finite; usagePercent must lie in [0,1]. On any error a zero-value
// Event and non-nil error are returned so no valid Event escapes a bad decode.
func Decode(r io.Reader, env map[string]string, maxBytes int64) (Event, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	// Bound the read: read at most maxBytes+1 so an over-long payload is
	// detectable. json.Unmarshal enforces exactly one top-level value.
	buf, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return Event{}, fmt.Errorf("hook: read input: %w", err)
	}
	if int64(len(buf)) > maxBytes {
		return Event{}, fmt.Errorf("hook: input exceeds %d bytes", maxBytes)
	}

	var w wireEvent
	if err := json.Unmarshal(buf, &w); err != nil {
		return Event{}, fmt.Errorf("hook: parse json: %w", err)
	}

	// Identity: JSON authoritative and required.
	if !validType(w.Event) {
		return Event{}, fmt.Errorf("hook: missing or unknown event %q", w.Event)
	}
	if w.Provider == "" {
		return Event{}, errors.New("hook: missing provider")
	}
	if w.Timestamp.IsZero() {
		return Event{}, errors.New("hook: missing timestamp")
	}

	// Identity cross-check: env values must agree when present.
	if v, ok := env[envEvent]; ok && Type(v) != w.Event {
		return Event{}, fmt.Errorf("hook: event identity mismatch (json %q, env %q)", w.Event, v)
	}
	if v, ok := env[envProvider]; ok && v != w.Provider {
		return Event{}, fmt.Errorf("hook: provider identity mismatch (json %q, env %q)", w.Provider, v)
	}
	if v, ok := env[envTimestamp]; ok {
		envTS, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return Event{}, fmt.Errorf("hook: invalid env timestamp %q: %w", v, err)
		}
		if !w.Timestamp.Equal(envTS) {
			return Event{}, fmt.Errorf("hook: timestamp identity mismatch (json %s, env %s)", w.Timestamp, envTS)
		}
	}

	e := Event{
		Type:         w.Event,
		Provider:     w.Provider,
		Timestamp:    w.Timestamp,
		Window:       w.Window,
		UsagePercent: w.UsagePercent,
		Used:         w.Used,
		Limit:        w.Limit,
		ResetAt:      w.ResetAt,
		Status:       w.Status,
	}

	// Optional env fallback: only for fields JSON omitted (nil).
	if e.Window == nil {
		if v, ok := env[envWindow]; ok {
			e.Window = &v
		}
	}
	if e.Status == nil {
		if v, ok := env[envStatus]; ok {
			e.Status = &v
		}
	}
	if e.UsagePercent == nil {
		if p, err := envFloat(env, envUsagePercent); err != nil {
			return Event{}, err
		} else {
			e.UsagePercent = p
		}
	}
	if e.Used == nil {
		if p, err := envFloat(env, envUsed); err != nil {
			return Event{}, err
		} else {
			e.Used = p
		}
	}
	if e.Limit == nil {
		if p, err := envFloat(env, envLimit); err != nil {
			return Event{}, err
		} else {
			e.Limit = p
		}
	}
	if e.ResetAt == nil {
		if t, err := envTime(env, envResetAt); err != nil {
			return Event{}, err
		} else {
			e.ResetAt = t
		}
	}

	// Finite-number guard (catches env "NaN"/"Inf" which ParseFloat accepts).
	for _, p := range []*float64{e.UsagePercent, e.Used, e.Limit} {
		if p != nil && !finite(*p) {
			return Event{}, fmt.Errorf("hook: non-finite number %v", *p)
		}
	}
	if e.UsagePercent != nil && (*e.UsagePercent < 0 || *e.UsagePercent > 1) {
		return Event{}, fmt.Errorf("hook: usagePercent %v out of range [0,1]", *e.UsagePercent)
	}

	return e, nil
}

// validType reports whether t is one of the six CodexBar event types.
func validType(t Type) bool {
	switch t {
	case QuotaLow, QuotaReached, QuotaReset,
		ProviderUnavailable, ProviderRecovered, RefreshFailed:
		return true
	}
	return false
}

// finite reports whether f is neither NaN nor an infinity.
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// envFloat parses an optional env float. It returns (nil, nil) when the key is
// absent.
func envFloat(env map[string]string, key string) (*float64, error) {
	v, ok := env[key]
	if !ok {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("hook: invalid %s %q: %w", key, v, err)
	}
	return &f, nil
}

// envTime parses an optional env timestamp (RFC 3339). It returns (nil, nil)
// when the key is absent.
func envTime(env map[string]string, key string) (*time.Time, error) {
	v, ok := env[key]
	if !ok {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("hook: invalid %s %q: %w", key, v, err)
	}
	return &t, nil
}
