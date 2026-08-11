// Package state holds the durable quota/availability state types, the event
// state machine, and the sanitized JSON store for the polytoken-quota
// reconciler.
//
// Quota and availability are independent axes: a provider recovery never clears
// quota state and a quota reset never clears availability state. Each axis
// tracks its own accepted timestamp and arrival sequence so stale, duplicate,
// and equal-timestamp events are ordered deterministically.
package state

import (
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// CurrentSchema is the on-disk state schema version this build reads and writes.
// Load accepts older known schemas (0 through 3) by migrating them in memory to
// CurrentSchema and rejects any newer, unknown schema by failing closed. Save
// always persists CurrentSchema.
const CurrentSchema = 4

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
	ManualDisabled      bool
	ManualDisabledAt    time.Time
	QuotaAt             time.Time
	AvailabilityAt      time.Time
	QuotaArrival        uint64
	AvailabilityArrival uint64

	// Additive (v2) quota-polling fields. Both snapshots are nil until the
	// first poll. QuotaSnapshot is the last successful/usable observation;
	// QuotaAttempt is the latest attempt including failures. The event-derived
	// Quota/Availability axes above are unchanged and remain authoritative for
	// the state machine.
	QuotaSnapshot *quota.QuotaSnapshot
	QuotaAttempt  *quota.QuotaSnapshot
	// ResetCredits is the independent Codex reset-credit sub-observation and
	// ordinary usage-summary provenance added in schema v3.
	ResetCredits quota.ResetCreditState
	// Routing records the last routing-policy decision metadata for this
	// provider. It is a value type (always present, zero-valued until ranked).
	Routing ProviderRouting
}

// ProviderRouting records routing-policy decision metadata for a single
// provider. It carries only sanitized, non-secret bookkeeping.
type ProviderRouting struct {
	LastRank            int // last computed global rank (0-based)
	LastDecisionAt      time.Time
	LastAppliedRevision uint64
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

	// Additive (v2) routing/usage history. Both are nil until the first
	// computation/observation.
	RoutingHistory *RoutingHistory
	UsageHistory   *UsageHistory

	// ReconcileHistory is the additive schema-v4 bounded record of qualifying
	// reconciles, newest revision first.
	ReconcileHistory ReconcileHistory
}

// RoutingHistory records the last good global provider ranking computed by the
// routing policy. It carries only sanitized mapping IDs — never credentials.
type RoutingHistory struct {
	LastGoodGlobalRank []string // mapping IDs in rank order
	ComputedAt         time.Time
}

// UsageSample is one week's normalized usage share aggregated across polls. The
// map keys are mapping IDs; values are normalized shares.
type UsageSample struct {
	WeekStart   time.Time
	Totals      map[string]float64
	SampleCount int
}

// UsageHistory is the bounded rolling usage history (current week plus four
// prior weeks — at most five entries). Save prunes to that bound.
type UsageHistory struct {
	Weeks []UsageSample
}

// Store is the sanitized, atomic persistence layer for State. Save prunes
// recovered errors older than RecoveredRetention (by Now) before writing, and
// atomically replaces the state file with mode 0600. The write is crash-safe:
// the temp file is fsync'd before the rename and the parent directory is fsync'd
// after, so the committed state.json is durably atomic.
type Store struct {
	Path               string
	Now                func() time.Time
	RecoveredRetention time.Duration
	// Fault, when non-nil, is consulted after the state temp file is written but
	// before it is fsync'd. It is a test-only seam letting a caller simulate a
	// crash between the write and the fsync so the publisher's durability
	// ordering (state durable before journal removal) is exercisable. Production
	// leaves it nil, in which case Save is fully durable.
	Fault func() error
}
