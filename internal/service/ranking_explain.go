package service

// Read-only routing/quota diagnostic methods for the Coordinator: RankingExplain
// (the routing explain command) and QuotaStatus (the quota status command). Both
// are pure projections over loaded policy + observed state and never mutate
// anything. Their report DTOs live here (not cli) so *Coordinator can implement
// the Diagnoser surface directly without an import cycle.

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/quota"
	"github.com/geofffranks/codexbar-hooks/internal/routing"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// RankEntryReport is one provider's position in the routing ranking, for the
// explain command. It mirrors routing.RankEntry with JSON tags. It carries only
// sanitized fields — never credentials, account IDs, or raw bodies.
type RankEntryReport struct {
	MappingID   string `json:"mapping_id"`
	Rank        int    `json:"rank"`
	OffPeak     bool   `json:"off_peak"`
	Eligible    bool   `json:"eligible"`
	Explanation string `json:"explanation"`
}

// RankingReport is the result of the routing explain command.
type RankingReport struct {
	Enabled  bool              `json:"enabled"`
	Now      time.Time         `json:"now"`
	Entries  []RankEntryReport `json:"entries"`
	Error    string            `json:"error,omitempty"`
	Advisory string            `json:"advisory,omitempty"`
}

// RankingExplain computes the routing ranking from the loaded desired policy and
// observed state and returns it for display. It is read-only: it never mutates
// state, policy, or targets. A load error yields a report carrying only the
// sanitized error string. The injected clock supplies "now".
func (c *Coordinator) RankingExplain(_ context.Context) RankingReport {
	report := RankingReport{Now: c.now()}
	if c.Policy == nil || c.State == nil {
		report.Error = "policy or state unavailable"
		return report
	}
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		report.Error = sanitizeMsg("load policy: ", err)
		return report
	}
	observed, err := c.State.LoadState()
	if err != nil {
		report.Error = sanitizeMsg("load state: ", err)
		return report
	}
	report.Enabled = desired.Routing.Enabled
	_, result := ComputeRanking(desired, observed, c.now())
	report.Entries = make([]RankEntryReport, 0, len(result.Entries))
	for _, e := range result.Entries {
		report.Entries = append(report.Entries, RankEntryReport{
			MappingID:   e.MappingID,
			Rank:        e.Rank,
			OffPeak:     e.OffPeak,
			Eligible:    e.Eligible,
			Explanation: e.Explanation,
		})
	}
	return report
}

// QuotaWindowReport is one window's sanitized quota observation.
type QuotaWindowReport struct {
	Name         string   `json:"name"`
	Used         *float64 `json:"used,omitempty"`
	Limit        *float64 `json:"limit,omitempty"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
	Remaining    *float64 `json:"remaining,omitempty"`
}

// QuotaSnapshotReport is the sanitized snapshot for one provider mapping: the
// last successful observation (QuotaSnapshot) and the last attempt
// (QuotaAttempt), plus routing metadata.
type QuotaSnapshotReport struct {
	MappingID      string                  `json:"mapping_id"`
	CheckedAt      time.Time               `json:"checked_at,omitempty"`
	Availability   quota.QuotaAvailability `json:"availability,omitempty"`
	Status         quota.SourceStatus      `json:"status,omitempty"`
	Windows        []QuotaWindowReport     `json:"windows,omitempty"`
	Attempt        *QuotaAttemptReport     `json:"attempt,omitempty"`
	LastRank       int                     `json:"last_rank,omitempty"`
	LastDecisionAt time.Time               `json:"last_decision_at,omitempty"`
}

// QuotaAttemptReport is the sanitized last attempt (success or failure).
type QuotaAttemptReport struct {
	Status    quota.SourceStatus `json:"status"`
	Error     string             `json:"error,omitempty"`
	CheckedAt time.Time          `json:"checked_at,omitempty"`
}

// QuotaStatusReport is the result of the quota status command. Problem is true
// when at least one provider has a pending problem (failed attempt or stale
// snapshot), which the CLI maps to exit code 2.
type QuotaStatusReport struct {
	Revision  uint64                `json:"revision"`
	Providers []QuotaSnapshotReport `json:"providers,omitempty"`
	Problem   bool                  `json:"problem"`
	Error     string                `json:"error,omitempty"`
	Advisory  string                `json:"advisory,omitempty"`
}

// QuotaStatus projects the observed state's per-provider quota snapshots,
// attempts, and routing metadata into a sanitized report. It is read-only. A
// provider has a pending problem when its last attempt failed or its snapshot is
// stale (older than its configured freshness TTL, when the TTL is known). The
// injected clock supplies "now".
func (c *Coordinator) QuotaStatus(_ context.Context) QuotaStatusReport {
	report := QuotaStatusReport{}
	if c.State == nil {
		report.Error = "state unavailable"
		return report
	}
	observed, err := c.State.LoadState()
	if err != nil {
		report.Error = sanitizeMsg("load state: ", err)
		return report
	}
	report.Revision = observed.Revision

	// Build a freshness TTL lookup from the policy, when available, so a stale
	// snapshot can be flagged. Also collect configured quota mappings so status
	// includes providers before their first poll. A missing policy or mapping is
	// non-fatal.
	freshness := map[string]time.Duration{}
	configured := map[string][]string{}
	if c.Policy != nil {
		if desired, err := c.Policy.LoadPolicy(); err == nil {
			for id, m := range desired.Providers {
				if m.Quota != nil {
					configured[string(id)] = m.CodexBarProviders
					if m.Quota.FreshnessTTL > 0 {
						freshness[string(id)] = m.Quota.FreshnessTTL
					}
				}
			}
		}
	}

	names := aggregateProviderNames(configured, observed.Providers)
	slices.Sort(names)

	now := c.now()
	for _, name := range names {
		ps := observed.Providers[name]
		if aliases, ok := configured[name]; ok {
			if len(aliases) == 0 {
				ps = observed.Providers[name]
			} else {
				ps = aggregateMappingState(aliases, observed.Providers)
				if hasMissingAlias(aliases, observed.Providers) {
					if allAliasesMissing(aliases, observed.Providers) {
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
		}
		entry := QuotaSnapshotReport{MappingID: name}
		if ps.Availability == state.Unavailable {
			entry.Availability = quota.QuotaUnavailable
		}
		if ps.QuotaSnapshot != nil {
			entry.CheckedAt = ps.QuotaSnapshot.CheckedAt
			entry.Availability = ps.QuotaSnapshot.Availability
			if ps.Availability == state.Unavailable {
				entry.Availability = quota.QuotaUnavailable
			}
			entry.Status = ps.QuotaSnapshot.Status
			entry.Windows = windowsReport(ps.QuotaSnapshot)
		}
		if ps.QuotaAttempt != nil {
			entry.Attempt = &QuotaAttemptReport{
				Status:    ps.QuotaAttempt.Status,
				Error:     quota.SanitizeError(errors.New(ps.QuotaAttempt.Error)),
				CheckedAt: ps.QuotaAttempt.CheckedAt,
			}
		}
		entry.LastRank = ps.Routing.LastRank
		entry.LastDecisionAt = ps.Routing.LastDecisionAt
		if providerHasProblem(ps, freshness[name], now) {
			report.Problem = true
		}
		report.Providers = append(report.Providers, entry)
	}
	return report
}

// windowsReport maps a snapshot's windows to sanitized reports, including the
// derived remaining fraction.
func windowsReport(s *quota.QuotaSnapshot) []QuotaWindowReport {
	if s == nil || len(s.Windows) == 0 {
		return nil
	}
	out := make([]QuotaWindowReport, 0, len(s.Windows))
	for _, w := range s.Windows {
		wr := QuotaWindowReport{
			Name:         w.Name,
			Used:         w.Used,
			Limit:        w.Limit,
			UsagePercent: w.UsagePercent,
		}
		// EffectiveRemaining is a per-snapshot minimum; show the window's own
		// remaining for display via the quota package's exported per-window method
		// (single source of truth, shared with the routing policy).
		wr.Remaining = w.Remaining()
		out = append(out, wr)
	}
	return out
}

// providerHasProblem reports whether a provider has a pending quota problem: a
// failed last attempt, or a stale (or missing) successful snapshot relative to
// its freshness TTL (when known).
func providerHasProblem(ps state.ProviderState, ttl time.Duration, now time.Time) bool {
	if ps.Availability == state.Unavailable {
		return true
	}
	if ps.QuotaSnapshot == nil && ps.QuotaAttempt == nil {
		return true
	}
	if ps.QuotaAttempt != nil && ps.QuotaAttempt.Status == quota.SourceFailed {
		return true
	}
	if ps.QuotaSnapshot != nil {
		snap := ps.QuotaSnapshot
		if snap.EffectiveRemaining() == nil || snap.Availability == quota.QuotaUnknown {
			return true
		}
		if ttl > 0 && now.Sub(snap.CheckedAt) > ttl {
			return true
		}
	}
	return false
}

// sanitizeMsg returns a bounded, generic error summary prefixed with ctx,
// stripping known secret patterns. It mirrors quota.SanitizeError.
func sanitizeMsg(ctx string, err error) string {
	if err == nil {
		return ""
	}
	return ctx + quota.SanitizeError(err)
}

// Compile-time assertions that *Coordinator satisfies the extended diagnostic
// surface used by the routing/quota CLI commands.
var (
	_ RankingExplainer = (*Coordinator)(nil)
	_ QuotaStater      = (*Coordinator)(nil)
	_ RoutingToggler   = (*Coordinator)(nil)
)

// RankingExplainer is the read-only surface for the routing explain command.
// The production implementation is *Coordinator; the CLI also accepts a test
// double.
type RankingExplainer interface {
	RankingExplain(context.Context) RankingReport
}

// QuotaStater is the read-only surface for the quota status command.
type QuotaStater interface {
	QuotaStatus(context.Context) QuotaStatusReport
}

// RoutingToggler mutates the desired policy's routing.enabled field. The
// production implementation operates on desired.yaml via the document editor.
type RoutingToggler interface {
	SetRoutingEnabled(ctx context.Context, enabled bool) error
}

// routing policy types referenced above are used.
var _ = routing.RankEntry{}
var _ = policy.MappingID("")
