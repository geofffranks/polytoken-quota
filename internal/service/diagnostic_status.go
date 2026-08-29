package service

// Merged status view: the single read surface behind `polytoken-quota status`.
// It consolidates the shared DiagnosticSnapshot's provider, route, and
// pending-target data into one DTO with the simplified presentation contract
// (consolidated provider STATUS, raw quota window numbers, one global
// last-checked timestamp). It is presentation-only: it never changes the
// snapshot's fail-closed aggregation semantics for other consumers.

import (
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Consolidated provider status values for the merged status view.
const (
	StatusDisabled    = "disabled"
	StatusEnabled     = "enabled"
	StatusUnavailable = "unavailable"
	StatusAvailable   = "available"
)

// MergedStatusProvider is one provider row: consolidated status, ranking
// metadata, raw quota window numbers, and the earliest upcoming reset.
type MergedStatusProvider struct {
	Provider    string              `json:"provider"`
	Status      string              `json:"status"`
	Rank        int                 `json:"rank"`
	OffPeak     bool                `json:"off_peak"`
	Eligible    bool                `json:"eligible"`
	Reason      string              `json:"reason"`
	Windows     []QuotaWindowReport `json:"windows,omitempty"`
	NextResetAt *time.Time          `json:"next_reset_at,omitempty"`
}

// MergedStatusRoute is one route with target/source provenance and its
// complete desired/effective chains. ProjectionError marks a route whose
// projection failed (Skipped is not synthesized for such routes).
type MergedStatusRoute struct {
	Name            string         `json:"name"`
	TargetID        string         `json:"target_id,omitempty"`
	SourcePath      string         `json:"source_path,omitempty"`
	Desired         []string       `json:"desired,omitempty"`
	Effective       []string       `json:"effective,omitempty"`
	Skipped         []SkippedModel `json:"skipped,omitempty"`
	ProjectionError bool           `json:"projection_error,omitempty"`
}

// SkippedModel is one desired model absent from the effective chain with the
// provider mapping's drop condition (manual disable / unavailable / quota
// exhausted, or the generic "disabled" fallback).
type SkippedModel struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// MergedStatusReport is the result of the merged status command. LastChecked
// is the max snapshot CheckedAt across providers (zero when never observed).
type MergedStatusReport struct {
	RoutingEnabled bool                   `json:"routing_enabled"`
	LastChecked    time.Time              `json:"last_checked,omitempty"`
	Providers      []MergedStatusProvider `json:"providers,omitempty"`
	Routes         []MergedStatusRoute    `json:"routes,omitempty"`
	PendingTargets []string               `json:"pending_targets,omitempty"`
	Problem        bool                   `json:"problem"`
	Errors         []DiagnosticError      `json:"errors,omitempty"`
	Error          string                 `json:"error,omitempty"`
}

// MergedStatusView consolidates the snapshot into the merged status report.
// Like the other selectors it is read-only and returns copies of slice data.
// A fatal error yields a report carrying only the sanitized error string.
func (s DiagnosticSnapshot) MergedStatusView() MergedStatusReport {
	report := MergedStatusReport{Error: s.fatalError}
	if s.fatalError != "" {
		return report
	}
	report.RoutingEnabled = s.routingEnabled
	report.Problem = s.problem
	report.PendingTargets = append([]string(nil), s.pendingTargets...)
	report.Errors = cloneDiagnosticErrors(s.providerErrors)
	report.Errors = append(report.Errors, cloneDiagnosticErrors(s.routeErrors)...)

	ranks := make(map[string]RankEntryReport, len(s.ranks))
	for _, rank := range s.ranks {
		ranks[rank.MappingID] = rank
	}
	for _, provider := range s.providers {
		rank := ranks[provider.MappingID]
		row := MergedStatusProvider{
			Provider: provider.MappingID,
			Status:   mergedProviderStatus(provider),
			Rank:     rank.Rank,
			OffPeak:  rank.OffPeak,
			Eligible: rank.Eligible,
			Reason:   rank.Explanation,
		}
		if provider.CheckedAt.After(report.LastChecked) {
			report.LastChecked = provider.CheckedAt
		}
		if len(provider.Windows) > 0 {
			row.Windows = cloneWindows(provider.Windows)
		}
		row.NextResetAt = cloneTime(provider.NextResetAt)
		report.Providers = append(report.Providers, row)
	}

	report.Routes = make([]MergedStatusRoute, 0, len(s.routes))
	routeFailed := failedRouteKeys(s.routeErrors)
	for _, route := range s.routes {
		row := MergedStatusRoute{
			Name: route.Name, TargetID: route.TargetID, SourcePath: route.SourcePath,
			Desired:   append([]string(nil), route.Desired...),
			Effective: append([]string(nil), route.Effective...),
		}
		row.ProjectionError = routeFailed[route.TargetID+"\x00"+route.SourcePath]
		if !row.ProjectionError {
			row.Skipped = skippedModels(s.desired, s.observed, route)
		}
		report.Routes = append(report.Routes, row)
	}
	return report
}

// failedRouteKeys indexes the route-scope projection errors by target and
// source path so a route projection failure suppresses skip synthesis.
func failedRouteKeys(errors []DiagnosticError) map[string]bool {
	failed := make(map[string]bool, len(errors))
	for _, e := range errors {
		if e.Scope == ErrorScopeRoute {
			failed[e.TargetID+"\x00"+e.SourcePath] = true
		}
	}
	return failed
}

// skippedModels diffs a route's desired chain against its effective chain and
// maps each skipped model to its provider mapping's drop condition. Entries
// are dropped only when their mapping's effective mode is disabled, so the
// condition mirrors EffectiveMode's three disabled-mode causes.
func skippedModels(desired policy.Desired, observed state.State, route RouteProjection) []SkippedModel {
	if len(route.Desired) == 0 {
		return nil
	}
	effective := make(map[string]bool, len(route.Effective))
	for _, entry := range route.Effective {
		effective[entry] = true
	}
	var skipped []SkippedModel
	for _, entry := range route.Desired {
		if effective[entry] {
			continue
		}
		reason := StatusDisabled
		if ref, err := reconcile.ParseModelRef(entry); err == nil {
			if mid, err := desired.ResolveModel(ref.Base); err == nil {
				if _, ok := observed.Providers[string(mid)]; ok {
					reason = dropCondition(aggregateMappingState(string(mid), observed.Providers))
				}
			}
		}
		skipped = append(skipped, SkippedModel{Model: entry, Reason: reason})
	}
	return skipped
}

// dropCondition names why a mode-disabled provider's models were dropped,
// mirroring state.EffectiveMode's disabled-mode causes. The generic
// "disabled" fallback covers corrupted axis values.
func dropCondition(ps state.ProviderState) string {
	switch {
	case ps.ManualDisabled:
		return "manual disable"
	case ps.Availability == state.Unavailable:
		return "unavailable"
	case ps.Quota == state.QuotaExhausted:
		return "quota exhausted"
	default:
		return StatusDisabled
	}
}

// mergedProviderStatus applies the consolidated status precedence:
//
//  1. disabled — manual disable wins over everything.
//  2. enabled — never observed: no quota snapshot and no attempt. This
//     deliberately overrides the fail-closed Unavailable/Exhausted
//     aggregation result for never-observed quota-mapped providers,
//     for presentation in this view only.
//  3. unavailable — the availability axis says unreachable, or the provider
//     has been observed (attempt recorded) but never produced a snapshot.
//  4. available — reachable with a quota observation.
func mergedProviderStatus(provider ProviderProjection) string {
	if provider.ManualDisabled {
		return StatusDisabled
	}
	if provider.Freshness == FreshnessMissing && provider.LatestAttempt == nil {
		return StatusEnabled
	}
	if provider.Availability == state.Unavailable {
		return StatusUnavailable
	}
	if !provider.CheckedAt.IsZero() {
		return StatusAvailable
	}
	return StatusUnavailable
}
