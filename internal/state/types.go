// Package state holds the durable quota/availability state types and, in later
// tasks, the state machine and store. This file declares only the minimal types
// needed to compile the CLI shell; Task 5 fills in the machine logic and store.
package state

// Mode selects the reconciler operating mode. Stub: populated by later tasks.
type Mode string

// Quota is the managed quota level for a provider.
type Quota string

// Quota levels recognized by the reconciler.
const (
	QuotaLow       Quota = "low"
	QuotaNormal    Quota = "normal"
	QuotaExhausted Quota = "exhausted"
)

// Availability is the managed availability state for a provider.
type Availability string

// Availability states recognized by the reconciler.
const (
	Available   Availability = "available"
	Unavailable Availability = "unavailable"
)

// ProviderPatch is a partial update to a single provider's managed fields. Each
// pointer field is nil when left unchanged.
type ProviderPatch struct {
	Quota        *Quota
	Availability *Availability
}

// Selector identifies the provider(s) a Clear applies to. When All is true the
// Provider field is ignored.
type Selector struct {
	Provider string
	All      bool
}

// ApplyFailure describes a target that could not be fully reconciled.
// Stub: empty now; Task 5 adds ResolvedAt and other fields.
type ApplyFailure struct{}
