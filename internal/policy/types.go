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
	"math"
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

	// KeepEnabled spares the provider from automatic signal gating: the gate
	// never plans a disable for it, whatever its signal shows. It constrains
	// disabling only — a keep_enabled provider that is currently gated off is
	// re-enabled on the next reconcile pass, regardless of signal recovery,
	// and an explicit operator `routing disable`/`enable` overrides it.
	KeepEnabled bool

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

	// UsesModelGroups records that the target's composed config surface — the
	// live global config.yaml or any registered project layer's config.yaml —
	// defines a top-level modelgroups key. It is not authored in desired.yaml:
	// the service coordinator detects it on the registered roots before every
	// legacy transaction and stamps it here (see service detectModelGroups).
	// Polytoken rejects a version-4 config that combines a legacy tier default
	// with an explicit model-group definition, and the staged merge composes
	// the layers, so a stamped target's tier-default fields stay
	// operator-owned: the reconciler proposes no defaults edits for it and
	// reports the skipped fields on the plan.
	UsesModelGroups bool
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

// DefaultSignalGateThreshold is the use-it-or-lose-it signal value at which
// signal gating engages when a quota block omits signal_gate.threshold. Zero
// is the projected-exhaustion line ("this provider runs out before reset");
// negative thresholds demand overdraw margin. It is the documented default of
// that key, not a magic number: Load applies it here and the gate surfaces it
// in diagnostics, so one documented value governs every threshold decision.
const DefaultSignalGateThreshold = 0.0

// SignalGateConfig is the resolved signal-gating configuration for one
// provider mapping (provider-only mode). Gating engages when the provider's
// observed use-it-or-lose-it routing signal falls to or below Threshold on a
// fresh quota snapshot. The zero value IS the documented default (gating on
// at DefaultSignalGateThreshold): Disabled is false and Threshold is zero,
// the projected-exhaustion line. This makes an explicitly disabled gate
// ({Disabled: true}) representable next to an absent signal_gate block,
// which Load resolves to the zero value.
type SignalGateConfig struct {
	// Disabled records an explicit `signal_gate: {enabled: false}`. The wire
	// grammar stays positive ("enabled"); the resolved form is inverted so
	// the struct's zero value means "as documented" with no ambiguity
	// between "absent" and "explicitly off".
	Disabled  bool
	Threshold float64
}

// DefaultSignalGate returns the documented default signal-gating
// configuration: gating on at DefaultSignalGateThreshold. It is the zero
// value of SignalGateConfig.
func DefaultSignalGate() SignalGateConfig {
	return SignalGateConfig{}
}

// Resolved returns p with the documented defaults applied: a threshold that
// is not finite and non-positive falls back to the default. Load rejects
// out-of-range explicit thresholds before this point; the fallback exists for
// hand-built configs, never to launder invalid input.
func (p SignalGateConfig) Resolved() SignalGateConfig {
	if p.Threshold > 0 || math.IsNaN(p.Threshold) || math.IsInf(p.Threshold, 0) {
		p.Threshold = DefaultSignalGateThreshold
	}
	return p
}

// IsDefault reports whether p is exactly the documented default configuration
// (the only shape the rendered policy omits).
func (p SignalGateConfig) IsDefault() bool {
	return !p.Disabled && p.Threshold == DefaultSignalGateThreshold
}

// QuotaConfig holds per-provider quota/routing configuration (additive on
// Mapping). When a mapping omits its quota section, the mapping's Quota pointer
// is nil and routing treats it as unrankable (it keeps its position, never
// reordered). The defaults below match the routing package's own defaults:
// FreshnessTTL 30m, BalanceGroup "default", Weight 1 when zero/empty.
type QuotaConfig struct {
	// Adapter is the quota adapter name ("codex" | "zai" | "anthropic" |
	// "anthropic-subscription" | "neuralwatt" | "opencode-go").
	// It is not user-configurable:
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
	// SignalGate is the resolved signal-gating configuration (additive). It
	// is consumed only by the provider-only gate; legacy mode keeps
	// rank-demotion-only semantics. Load resolves an absent signal_gate
	// block to DefaultSignalGate, so the in-memory config always carries the
	// effective values.
	SignalGate SignalGateConfig
}

// RoutingConfig holds the top-level routing enablement (additive on Desired).
// Load resolves an omitted routing section to enabled; `routing: {enabled:
// false}` is the explicit opt-out. Note the struct zero value is disabled —
// only Load applies the enabled default, so code constructing Desired by hand
// must set Routing explicitly where it means "as loaded".
type RoutingConfig struct {
	Enabled bool
}

// DocumentedJevModel is the versioned assessment model pinned for JEV. It is
// the documented default of the selection.jev `model` key: an omitted key (or
// an omitted selection section) resolves to this version, and the rendered
// policy omits the key whenever it matches, so the documented value — not a
// per-file literal — is the single source of the pin.
const DocumentedJevModel = "jev-1.13.0"

// DefaultJevTimeout bounds one JEV assessment when selection.jev omits
// timeout. It must stay positive: Load rejects an explicit non-positive
// timeout, so every resolved JevSelectionConfig carries a positive bound.
// It is the single source of that bound; the selection package's
// DefaultAssessTimeout delegates to it, so the two layers cannot drift.
const DefaultJevTimeout = 10 * time.Second

// DefaultQuotaFreshness is the freshness TTL a quota mapping must satisfy
// when its configuration omits freshness_ttl. It matches the routing
// package's default and is the single source of that bound: Load applies it
// here, and the selection package falls back to it for mappings whose
// configured TTL is non-positive, so one documented value governs every
// freshness decision.
const DefaultQuotaFreshness = 30 * time.Minute

// SelectionConfig holds quota-aware model-selection settings (additive on
// Desired). The whole section is optional: Load resolves an omitted selection
// section — or any omitted key within it — to the documented defaults (JEV
// disabled, DefaultJevTimeout).
type SelectionConfig struct {
	// Jev holds the JEV assessment enablement. The struct zero value is
	// disabled with no timeout; only Load and propose apply DefaultJevTimeout,
	// so code constructing Desired by hand must set Selection explicitly where
	// it means "as loaded" (the same contract as Routing).
	Jev JevSelectionConfig
}

// JevSelectionConfig is the resolved selection.jev configuration. Enabled
// defaults to false: quota-aware model selection is strictly opt-in. When
// enabled, the assessment runs against Model — defaulted to the documented
// DocumentedJevModel pin when the key is omitted — and is bounded by Timeout,
// which Load guarantees to be positive (DefaultJevTimeout when the key is
// omitted).
type JevSelectionConfig struct {
	Enabled bool
	// Model is the versioned assessment model pin. Load validates an explicit
	// pin against the documented jev-X.Y.Z grammar and defaults the omitted
	// key to DocumentedJevModel. Empty only in a hand-constructed zero value;
	// Load and propose always resolve it.
	Model string
	// Timeout is positive after Load; DefaultJevTimeout when the key is
	// omitted.
	Timeout time.Duration
}

// PolicyMode names the desired.yaml policy mode. The zero value (and the
// explicit "legacy" spelling) is the original chain-managing policy: provider
// mappings enumerate concrete models and targets carry desired chains and
// definitions. ModeProviderOnly is the strictly opt-in provider-only policy: it
// enrolls Polytoken provider IDs with optional quota adapter configuration and
// a global target, and rejects every legacy target/model field.
type PolicyMode string

const (
	// ModeLegacy is the original policy mode. desired.yaml files without a
	// mode key load as ModeLegacy; the explicit spelling is accepted so a
	// legacy policy can document its mode.
	ModeLegacy PolicyMode = "legacy"
	// ModeProviderOnly selects the provider-only policy: enrolled Polytoken
	// provider IDs, optional per-provider quota adapter configuration, a
	// global target, and registered project roots (id and root only) for
	// read-only safety assessment. Model enumeration, chain definitions, and
	// the routing/selection sections are rejected in this mode, as is any
	// project field beyond id and root.
	ModeProviderOnly PolicyMode = "provider-only"
)

// ProviderOnly reports whether the policy is the opt-in provider-only mode.
// Callers branch explicitly on this — a provider-only Desired carries no model
// enumeration or chains, so any legacy chain projection over it would be a
// silent no-op by accident.
func (d Desired) ProviderOnly() bool { return d.Mode == ModeProviderOnly }

// Desired is the fully validated in-memory desired policy produced by Load.
type Desired struct {
	Version     int
	Providers   map[MappingID]Mapping
	Global      Target
	Projects    []Target
	Operational Operational

	// Mode records the policy mode. In ModeProviderOnly the Providers map
	// carries the enrolled Polytoken provider IDs (Models is nil, Quota holds
	// the explicit quota adapter configuration when enrolled with one), Global
	// carries only the target identity and root, and Projects carries only the
	// registered project identity and root for each entry.
	Mode PolicyMode

	// Routing holds the top-level routing enablement. Load defaults it to
	// enabled when the routing section is omitted; explicit
	// `routing: {enabled: false}` disables quota-based reordering (managed
	// fields then reconcile to their configured chain positions only).
	Routing RoutingConfig

	// Selection holds the optional quota-aware model-selection settings. Load
	// defaults it to JEV disabled with a DefaultJevTimeout bound when the
	// selection section is omitted.
	Selection SelectionConfig
}
