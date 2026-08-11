package service

import (
	"errors"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func projectLegacyTargets(observed state.State) ([]TargetStatus, int, bool) {
	ids := sortedTargetIDs(observed.Targets)
	targets := make([]TargetStatus, 0, len(ids))
	pending := 0
	drift := false
	for _, id := range ids {
		ts := observed.Targets[id]
		targets = append(targets, TargetStatus{
			TargetID: id, AttemptedRevision: ts.AttemptedRevision,
			AppliedRevision: ts.AppliedRevision, Pending: ts.Pending != nil,
		})
		if ts.Pending != nil {
			pending++
		}
		if ts.AttemptedRevision > ts.AppliedRevision {
			drift = true
		}
	}
	return targets, pending, drift
}

func projectLegacyQuota(desired policy.Desired, observed state.State, asOf time.Time) ([]QuotaSnapshotReport, bool) {
	configured := make(map[string][]string, len(desired.Providers))
	freshness := make(map[string]time.Duration, len(desired.Providers))
	for id, mapping := range desired.Providers {
		if mapping.Quota == nil {
			continue
		}
		configured[string(id)] = append([]string(nil), mapping.CodexBarProviders...)
		freshness[string(id)] = mapping.Quota.FreshnessTTL
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]QuotaSnapshotReport, 0, len(names))
	problem := false
	for _, name := range names {
		ps := observed.Providers[name]
		if aliases, ok := configured[name]; ok {
			if len(aliases) > 0 {
				ps = aggregateMappingState(aliases, observed.Providers)
				if hasMissingAlias(aliases, observed.Providers) {
					if allAliasesMissing(aliases, observed.Providers) {
						if legacy, exists := observed.Providers[name]; exists {
							ps = legacy
						}
					} else if legacy, exists := observed.Providers[name]; exists {
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
				Status: ps.QuotaAttempt.Status, CheckedAt: ps.QuotaAttempt.CheckedAt,
				Error: quota.SanitizeError(errors.New(ps.QuotaAttempt.Error)),
			}
		}
		entry.LastRank = ps.Routing.LastRank
		entry.LastDecisionAt = ps.Routing.LastDecisionAt
		if providerHasProblem(ps, freshness[name], asOf) {
			problem = true
		}
		out = append(out, entry)
	}
	return out, problem
}

func cloneLegacyQuota(in []QuotaSnapshotReport) []QuotaSnapshotReport {
	out := make([]QuotaSnapshotReport, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Windows = cloneWindows(in[i].Windows)
		if in[i].Attempt != nil {
			attempt := *in[i].Attempt
			out[i].Attempt = &attempt
		}
	}
	return out
}
