// Package policy defines the durable desired-state policy for the polytoken-quota
// reconciler: the CodExBar→Polytoken provider mappings with their exact managed
// model enumeration, the registered targets and desired chains, and bounded
// operational settings.
//
// Load parses desired.yaml into a validated Desired. ResolveModel and
// ResolveCodexBar answer graph queries deterministically — exact match only, never
// similarity-based guessing. These types are the in-memory representation consumed
// by reconcile, import, and the coordinator.
package policy

import "time"

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

// Mapping binds one or more CodExBar provider IDs to one or more Polytoken
// provider IDs and enumerates the exact concrete base models managed by that
// binding. The model map is keyed by concrete base model name (e.g.
// "codex/gpt-5.6-sol"). CodExBar and Polytoken provider namespaces are separate;
// both lists are explicit so reconciliation is deterministic and offline.
type Mapping struct {
	CodexBarProviders  []string
	PolytokenProviders []string
	Models             map[string]ModelBaseline
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

// Operational holds bounded operational settings. A valid policy requires every
// duration to be positive and BackupCount to be at least one.
type Operational struct {
	ValidationTimeout  time.Duration
	LockWait           time.Duration
	RecoveredRetention time.Duration
	BackupCount        int
}

// Desired is the fully validated in-memory desired policy produced by Load.
type Desired struct {
	Version     int
	Providers   map[MappingID]Mapping
	Global      Target
	Projects    []Target
	Operational Operational
}
