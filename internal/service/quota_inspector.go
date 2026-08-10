package service

// Quota probe construction for the doctor. buildDoctorQuotaProbes builds
// sanitized doctor.QuotaProbe values from the preloaded observed state, the
// desired policy (for freshness TTL and adapter name), and the evidence gate
// (for adapter support), then delegates to the pure doctor.QuotaFindings.
// It also checks the write-ahead journal to detect an interrupted quota-check
// reconcile. It is read-only: it never mutates state, policy, or files, and it
// never loads state or policy.

import (
	"os"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// doctorQuotaInputs carries the preloaded inputs needed to build quota probes
// for the doctor. It is pure data: the caller (Coordinator.Doctor) assembles it
// from the shared DiagnosticSnapshot so no duplicate loads occur.
type doctorQuotaInputs struct {
	observed    state.State
	desired     policy.Desired
	now         time.Time
	evidence    *quota.EvidenceRegistry
	journalPath string
}

// buildDoctorQuotaProbes builds probes from the preloaded observed state +
// desired policy + evidence gate. It mirrors the prior quotaDoctorInspector
// aggregation logic (CodexBar alias grouping, freshness TTL, adapter support)
// without re-loading state or policy.
func buildDoctorQuotaProbes(in doctorQuotaInputs) ([]doctor.QuotaProbe, bool) {
	// Build freshness TTL and adapter lookups from policy, keyed by mapping ID.
	// QuotaPoller observations use mapping IDs even when a mapping has one or
	// more CodexBar provider aliases.
	type qcfg struct {
		ttl     time.Duration
		adapter string
	}
	configs := map[string]qcfg{}
	aliases := make(map[string][]string)
	if len(in.desired.Providers) > 0 {
		aliases = make(map[string][]string, len(in.desired.Providers))
		for mappingID, m := range in.desired.Providers {
			if m.Quota == nil {
				continue
			}
			id := string(mappingID)
			configs[id] = qcfg{ttl: m.Quota.FreshnessTTL, adapter: m.Quota.Adapter}
			aliases[id] = m.CodexBarProviders
		}
	}
	sorted := aggregateProviderNames(aliases, in.observed.Providers)
	sort.Strings(sorted)

	probes := make([]doctor.QuotaProbe, 0, len(sorted))
	for _, name := range sorted {
		ps := in.observed.Providers[name]
		cfg, configured := configs[name]
		if backing, ok := aliases[name]; ok && len(backing) > 0 {
			ps = aggregateMappingState(backing, in.observed.Providers)
			if hasMissingAlias(backing, in.observed.Providers) {
				if allAliasesMissing(backing, in.observed.Providers) {
					if legacy, exists := in.observed.Providers[name]; exists {
						// Preserve compatibility when no configured alias has any
						// observation to aggregate.
						ps = legacy
					}
				} else if legacy, exists := in.observed.Providers[name]; exists {
					// Present aliases own quota safety; legacy state may only
					// contribute non-safety metadata in a mixed view.
					ps.Routing = legacy.Routing
				}
			} else if legacy, exists := in.observed.Providers[name]; exists {
				ps.Routing = legacy.Routing
			}
		}
		if ps.Availability == state.Unavailable && ps.QuotaSnapshot == nil {
			ps.QuotaSnapshot = &quota.QuotaSnapshot{MappingID: name, Availability: quota.QuotaUnknown, Status: quota.SourcePartial}
		}
		support := adapterSupport(cfg.adapter, in.now, in.evidence)
		probes = append(probes, doctor.QuotaProbe{
			Provider:       name,
			HasQuotaConfig: configured,
			FreshnessTTL:   cfg.ttl,
			Snapshot:       ps.QuotaSnapshot,
			Attempt:        ps.QuotaAttempt,
			Supported:      support.Supported,
			SupportReason:  support.Reason,
		})
	}

	reconcilePending := false
	if in.journalPath != "" {
		if _, err := os.Stat(in.journalPath); err == nil {
			reconcilePending = true
		}
	}
	return probes, reconcilePending
}

// adapterSupport checks the evidence gate for a named adapter using the same
// registry owned by the production poller. A nil registry is empty and therefore
// fail-closed; this function never registers or refreshes evidence.
func adapterSupport(adapter string, now time.Time, reg *quota.EvidenceRegistry) quota.SupportStatus {
	if adapter == "" {
		return quota.SupportStatus{}
	}
	if reg == nil {
		reg = quota.NewEvidenceRegistry()
	}
	if adapter != "codex" && adapter != "zai" && adapter != "anthropic" {
		return quota.SupportStatus{
			Supported: false,
			Reason:    "unknown quota adapter " + adapter + "; record evidence before enabling",
		}
	}
	return quota.SupportFromEvidence(reg.Status(adapter, now))
}
