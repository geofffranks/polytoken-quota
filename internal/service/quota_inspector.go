package service

// Production quota/routing inspector for doctor.Run. quotaDoctorInspector
// implements doctor.QuotaInspector by building sanitized doctor.QuotaProbe
// values from the observed state, the desired policy (for freshness TTL and
// adapter name), and the evidence gate (for adapter support), then delegating
// to the pure doctor.QuotaFindings. It also checks the write-ahead journal to
// detect an interrupted quota-check reconcile. It is read-only: it never
// mutates state, policy, or files.

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/doctor"
	"github.com/geofffranks/codexbar-hooks/internal/quota"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// quotaDoctorInspector implements doctor.QuotaInspector. It needs the concrete
// state store (for observed quota snapshots/attempts), the policy loader (for
// freshness TTL and adapter names), the journal path (for reconcile-pending),
// and a clock.
type quotaDoctorInspector struct {
	state       state.Store
	policy      PolicyLoader
	journalPath string
	now         func() time.Time
	evidence    *quota.EvidenceRegistry
}

// Findings builds probes from state + policy + evidence gate and delegates to
// doctor.QuotaFindings. A state-load error yields no findings (doctor.Run
// surfaces its own state-unreadable finding).
func (q quotaDoctorInspector) Findings(_ context.Context) []doctor.Finding {
	observed, err := q.state.Load()
	if err != nil {
		return nil
	}

	now := time.Now()
	if q.now != nil {
		now = q.now()
	}

	// Build freshness TTL and adapter lookups from policy, keyed by mapping ID.
	// QuotaPoller observations use mapping IDs even when a mapping has one or
	// more CodexBar provider aliases.
	type qcfg struct {
		ttl     time.Duration
		adapter string
	}
	configs := map[string]qcfg{}
	aliases := make(map[string][]string)
	if q.policy != nil {
		if desired, err := q.policy.LoadPolicy(); err == nil {
			aliases = make(map[string][]string, len(desired.Providers))
			for mappingID, m := range desired.Providers {
				if m.Quota == nil {
					continue
				}
				id := string(mappingID)
				configs[id] = qcfg{ttl: m.Quota.FreshnessTTL, adapter: m.Quota.Adapter}
				aliases[id] = m.CodexBarProviders
			}
		}
	}
	sorted := aggregateProviderNames(aliases, observed.Providers)
	sort.Strings(sorted)

	probes := make([]doctor.QuotaProbe, 0, len(sorted))
	for _, name := range sorted {
		ps := observed.Providers[name]
		cfg, configured := configs[name]
		if backing, ok := aliases[name]; ok && len(backing) > 0 {
			ps = aggregateMappingState(backing, observed.Providers)
			if hasMissingAlias(backing, observed.Providers) {
				if allAliasesMissing(backing, observed.Providers) {
					if legacy, exists := observed.Providers[name]; exists {
						// Preserve compatibility when no configured alias has any
						// observation to aggregate.
						ps = legacy
					}
				} else if legacy, exists := observed.Providers[name]; exists {
					// Present aliases own quota safety; legacy state may only
					// contribute non-safety metadata in a mixed view.
					ps.Routing = legacy.Routing
				}
			} else if legacy, exists := observed.Providers[name]; exists {
				ps.Routing = legacy.Routing
			}
		}
		if ps.Availability == state.Unavailable && ps.QuotaSnapshot == nil {
			ps.QuotaSnapshot = &quota.QuotaSnapshot{MappingID: name, Availability: quota.QuotaUnknown, Status: quota.SourcePartial}
		}
		support := adapterSupport(cfg.adapter, now, q.evidence)
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
	if q.journalPath != "" {
		if _, err := os.Stat(q.journalPath); err == nil {
			reconcilePending = true
		}
	}

	return doctor.QuotaFindings(probes, reconcilePending, now)
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
	if adapter != "codex" && adapter != "zai" {
		return quota.SupportStatus{
			Supported: false,
			Reason:    "unknown quota adapter " + adapter + "; record evidence before enabling",
		}
	}
	return quota.SupportFromEvidence(reg.Status(adapter, now))
}
