// Package state holds the durable quota/availability state types, the event
// state machine, and the sanitized JSON store for the polytoken-quota
// reconciler.
//
// Quota and availability are independent axes: a provider recovery never clears
// quota state and a quota reset never clears availability state. Each axis
// tracks its own accepted timestamp and arrival sequence so stale, duplicate,
// and equal-timestamp events are ordered deterministically.
package state

import "time"

// Mode is the reconciler-internal effective operating mode derived from a
// provider's independent quota and availability axes. It is never persisted to
// Polytoken directly (Polytoken has no reserve field); the reconciler maps
// reserve to a stable partition ordering.
type Mode string

// Effective modes.
const (
	ModeNormal   Mode = "normal"
	ModeReserve  Mode = "reserve"
	ModeDisabled Mode = "disabled"
)

// Quota is the managed quota level for a provider.
type Quota string

// Quota levels recognized by the reconciler.
const (
	QuotaNormal    Quota = "normal"
	QuotaLow       Quota = "low"
	QuotaExhausted Quota = "exhausted"
)

// Availability is the managed availability state for a provider.
type Availability string

// Availability states recognized by the reconciler.
const (
	Available   Availability = "available"
	Unavailable Availability = "unavailable"
)

// Arrival is per-event arrival metadata assigned by the coordinator when an
// event enters the state machine. Sequence increases monotonically across a
// process and is the deterministic tiebreaker when event timestamps are absent
// or equal.
type Arrival struct {
	Sequence   uint64
	ReceivedAt time.Time
}

// Diagnostic records bounded, sanitized diagnostic metadata. It never carries
// credentials, account names, or unmanaged source fields.
type Diagnostic struct {
	Code     string
	Provider string
	Summary  string
	At       time.Time
}

// ProviderPatch is a partial update to a single provider's managed axes. Each
// pointer field is nil when left unchanged. It is the manual-override input for
// SetProvider (the `state set` command).
type ProviderPatch struct {
	Quota        *Quota
	Availability *Availability
}

// Selector identifies the provider(s) a ClearProvider applies to. When All is
// true the Provider field is ignored.
type Selector struct {
	Provider string
	All      bool
}

// ProviderState is the durable observed state of a single provider. Quota and
// Availability are independent axes; each records the timestamp and arrival
// sequence of the event that last mutated it, so stale and duplicate events are
// rejected per axis without touching the other.
type ProviderState struct {
	Quota               Quota
	Availability        Availability
	QuotaAt             time.Time
	AvailabilityAt      time.Time
	QuotaArrival        uint64
	AvailabilityArrival uint64
}

// ApplyFailure describes a target that could not be fully reconciled. It is the
// structured pending/recovered error surfaced by status and doctor. It carries
// only sanitized, bounded diagnostic fields — never credentials, account names,
// auth blocks, or unmanaged Polytoken source content.
type ApplyFailure struct {
	TargetID               string
	Stage                  string
	File                   string
	Chain                  string
	Summary                string
	Remediation            string
	LastSuccessfulRevision uint64
	AttemptedRevision      uint64
	LastSuccessfulAt       time.Time
	AttemptedAt            time.Time
	ResolvedAt             time.Time
	Reproduces             bool
	LiveStatus             string
}

// TargetState records the last attempted and applied reconciliation outcome for
// a single target. Pending is non-nil while the target remains on its
// last-known-good files with an unresolved apply failure.
type TargetState struct {
	AttemptedRevision uint64
	AppliedRevision   uint64
	AttemptedAt       time.Time
	AppliedAt         time.Time
	Pending           *ApplyFailure
}

// State is the complete durable observed state persisted to state.json. It
// never contains provider credentials, account names, auth blocks, or unmanaged
// Polytoken source fields.
type State struct {
	Schema        int
	Revision      uint64
	Providers     map[string]ProviderState
	Targets       map[string]TargetState
	RefreshFailed []Diagnostic
	Recovered     []ApplyFailure
}

// Store is the sanitized, atomic persistence layer for State. Save prunes
// recovered errors older than RecoveredRetention (by Now) before writing, and
// atomically replaces the state file with mode 0600.
type Store struct {
	Path               string
	Now                func() time.Time
	RecoveredRetention time.Duration
}
