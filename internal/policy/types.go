// Package policy defines the durable desired-state policy for the polytoken-quota
// reconciler: the provider mappings keyed by mapping ID, their exact managed
// model enumeration, the registered targets and desired chains, and bounded
// operational settings.
//
// Load parses desired.yaml into a validated Desired. ResolveModel answers graph
// queries deterministically — exact match only, never similarity-based guessing. These types are the in-memory representation consumed
// by reconcile, import, and the coordinator.
package policy

import (
	"time"

	"github.com/geofffranks/polytoken-quota/internal/routing"
)

// MappingID names a provider mapping. It is the desired.yaml `providers` map key.
type MappingID string

// ModelBaseline records a managed model's durable baseline enabled state.
//
// Enabled is the desired on/off value restored when the provider is healthy.
// HadEnabledKey records whether an explicit `enabled` key existed in the source: a
// model listed as a bare name has HadEnabledKey false (and Enabled true), while a
// model listed with `enabled` has HadEnabledKey true. The byte-preserving editor
// (Task 7) uses HadEnabledKey to remove a transient `enabled` key it inserted when
// restoring a model that never had one.
type ModelBaseline struct {
	Enabled       bool `yaml:"enabled"`
	HadEnabledKey bool `yaml:"had_enabled_key"`
}

// Mapping enumerates the exact concrete base models managed by a provider
// mapping. The model map is keyed by concrete base model name (e.g.
// "codex/gpt-5.6-sol"). The mapping's provider identity is its top-level
// Desired.Providers key.
type Mapping struct {
	Models map[string]ModelBaseline

	// Quota is the optional per-provider quota/routing configuration. It is nil
	// when the mapping's desired.yaml entry omits a quota section (routing
	// disabled for that mapping). When present it carries the routing-relevant
	// metadata (adapter, freshness, balance group, weight, off-peak schedule).
	Quota *QuotaConfig
}

// Chain is an ordered list of model preference entries. Entries may carry
// reasoning suffixes (e.g. "codex/gpt-5.6-sol(medium)"); the reconciler normalizes
// to the base model (the portion before any `(`) for provider-mode matching while
// preserving the exact spelling on output.
type Chain []string

// Definition is one managed facet/subagent definition file within a target,
// together with its desired model chain.
type Definition struct {
	Path  string
	Chain Chain
}

// Target is one reconciliation target: the global user target or a registered
// project. Root is the canonical Polytoken configuration root; Definitions
// enumerates exactly which facet/subagent files are managed. Global distinguishes
// the single global target from project targets.
type Target struct {
	ID          string
	Root        string
	Global      bool
	Definitions []Definition
	Full        Chain
	Mini        Chain
	Nano        Chain
	Classifier  Chain
}

// OnChange bounds for operator-configured host-side actions. The count and
// timeout ceilings bound the worst-case post-commit work a desired.yaml edit
// can introduce.
const (
	MaxOnChangeActions            = 16
	DefaultOnChangeTimeoutSeconds = 10
	MaxOnChangeTimeoutSeconds     = 60
)

// OnChangeAction is one operator-configured executable executed by the host
// binary after a reconciled revision changed managed fields. Run must be an
// absolute path; Args and Env are literal values passed through verbatim (no
// shell, no notice-content interpolation).
type OnChangeAction struct {
	Run            string
	Args           []string
	Env            map[string]string
	TimeoutSeconds int
}

// Operational holds bounded operational settings. A valid policy requires every
// duration to be positive and BackupCount to be at least one.
type Operational struct {
	ValidationTimeout  time.Duration
	LockWait           time.Duration
	RecoveredRetention time.Duration
	BackupCount        int
	// NoticePath overrides the reconciliation-notice file location. When empty,
	// notice.ResolvePath defaults to ~/.local/polytoken-quota/notice.json.
	NoticePath string
	// OnChange lists opt-in host-side actions executed after a committed
	// revision changed managed fields. Empty by default: nothing executes.
	OnChange []OnChangeAction
}

// QuotaConfig holds per-provider quota/routing configuration (additive on
// Mapping). When a mapping omits its quota section, the mapping's Quota pointer
// is nil and routing treats it as unrankable (it keeps its position, never
// reordered). The defaults below match the routing package's own defaults:
// FreshnessTTL 30m, BalanceGroup "default", Weight 1 when zero/empty.
type QuotaConfig struct {
	// Adapter is the quota adapter name ("codex" | "zai" | "anthropic" |
	// "anthropic-subscription" | "neuralwatt"). It is not user-configurable:
	// Load derives it from the provider mapping key (and, for anthropic, the
	// quota `mode` — `subscription` selects the anthropic-subscription
	// adapter) and validates it is a known adapter.
	Adapter      string
	FreshnessTTL time.Duration // default 30m when zero
	BalanceGroup string        // default "default"
	Weight       int           // default 1
	// MonthlyBudgetUSD is the user-defined monthly spend ceiling treated as
	// the provider's quota. Required (and only meaningful) for the anthropic
	// adapter: a pay-as-you-go API has no token allowance, so the budget IS
	// the quota. Neuralwatt reads its provider-reported balance directly.
	MonthlyBudgetUSD float64
	Schedule         *routing.Schedule // nil = never off-peak
}

// RoutingConfig holds the top-level routing enablement (additive on Desired).
// Load resolves an omitted routing section to enabled; `routing: {enabled:
// false}` is the explicit opt-out. Note the struct zero value is disabled —
// only Load applies the enabled default, so code constructing Desired by hand
// must set Routing explicitly where it means "as loaded".
type RoutingConfig struct {
	Enabled bool
}

// Desired is the fully validated in-memory desired policy produced by Load.
type Desired struct {
	Version     int
	Providers   map[MappingID]Mapping
	Global      Target
	Projects    []Target
	Operational Operational

	// Routing holds the top-level routing enablement. Load defaults it to
	// enabled when the routing section is omitted; explicit
	// `routing: {enabled: false}` disables quota-based reordering (managed
	// fields then reconcile to their configured chain positions only).
	Routing RoutingConfig
}
