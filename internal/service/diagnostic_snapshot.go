package service

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ErrorScope classifies a non-fatal diagnostic projection error.
type ErrorScope string

const (
	ErrorScopeProvider ErrorScope = "provider"
	ErrorScopeRoute    ErrorScope = "route"
)

// DiagnosticError is safe display context for one independently failed item.
type DiagnosticError struct {
	Scope      ErrorScope `json:"scope"`
	MappingID  string     `json:"mapping_id,omitempty"`
	TargetID   string     `json:"target_id,omitempty"`
	SourcePath string     `json:"source_path,omitempty"`
	Summary    string     `json:"summary"`
}

// DiagnosticSnapshot is the canonical read-only projection backing all
// diagnostic selectors. Its slices are private; selectors return deep copies.
type DiagnosticSnapshot struct {
	asOf           time.Time
	routingEnabled bool
	providers      []ProviderProjection
	ranks          []RankEntryReport
	routes         []RouteProjection
	providerErrors []DiagnosticError
	routeErrors    []DiagnosticError
	revision       uint64
	targets        []TargetStatus
	pending        int
	drift          bool
	legacyQuota    []QuotaSnapshotReport
	problem        bool
	fatalError     string
	// observed carries the raw observed state loaded exactly once, so downstream
	// consumers (doctor) can read pending targets and recovered history without a
	// duplicate load.
	observed state.State
	// desired carries the raw desired policy loaded exactly once, so downstream
	// consumers can build quota probes without a duplicate load.
	desired policy.Desired
	// policyErr and policyMissing classify the single policy load so doctor can
	// surface a configuration finding without another filesystem probe.
	policyErr     error
	policyMissing bool
	// resolveErr is the target resolution error (nil on success). It lets the
	// doctor surface a configuration finding without re-resolving targets.
	resolveErr error
	// stateErr is the state load error (nil on success). It lets the doctor
	// surface a state-unreadable finding without re-loading state.
	stateErr error
}

// BuildDiagnosticSnapshot performs the only shared reads for a diagnostic
// invocation. The injected clock is sampled exactly once and that AsOf is used
// by every time-sensitive projection.
func (c *Coordinator) BuildDiagnosticSnapshot(_ context.Context) DiagnosticSnapshot {
	snapshot := DiagnosticSnapshot{asOf: c.now()}
	if c.Policy == nil {
		snapshot.fatalError = "load policy failed"
		return snapshot
	}
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		snapshot.fatalError = "load policy failed"
		snapshot.policyErr = err
		snapshot.policyMissing = errors.Is(err, fs.ErrNotExist)
		return snapshot
	}
	snapshot.desired = desired
	if c.State == nil {
		snapshot.fatalError = "load state failed"
		return snapshot
	}
	observed, err := c.State.LoadState()
	if err != nil {
		snapshot.fatalError = "load state failed"
		snapshot.stateErr = err
		return snapshot
	}
	snapshot.observed = observed
	if c.Targets == nil {
		snapshot.fatalError = "resolve targets failed"
		return snapshot
	}
	targets, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		snapshot.fatalError = "resolve targets failed"
		snapshot.resolveErr = err
		return snapshot
	}

	snapshot.routingEnabled = desired.Routing.Enabled
	snapshot.revision = observed.Revision
	snapshot.targets, snapshot.pending, snapshot.drift = projectLegacyTargets(observed)
	snapshot.legacyQuota, snapshot.problem = projectLegacyQuota(desired, observed, snapshot.asOf)
	ranks, ranking := ComputeRanking(desired, observed, snapshot.asOf)
	snapshot.ranks = completeRankProjection(desired, ranking.Entries)
	snapshot.providers, snapshot.providerErrors = projectProviders(desired, observed, snapshot.asOf)
	snapshot.routes, snapshot.routeErrors = projectRoutes(desired, observed, targets, ranks)
	return snapshot
}

func completeRankProjection(desired policy.Desired, ranked []routing.RankEntry) []RankEntryReport {
	out := make([]RankEntryReport, 0, len(desired.Providers))
	seen := make(map[string]bool, len(ranked))
	for _, entry := range ranked {
		seen[entry.MappingID] = true
		out = append(out, RankEntryReport{
			MappingID: entry.MappingID, Rank: entry.Rank, OffPeak: entry.OffPeak,
			Eligible: entry.Eligible, Explanation: entry.Explanation,
		})
	}
	missing := make([]string, 0)
	for id := range desired.Providers {
		if !seen[string(id)] {
			missing = append(missing, string(id))
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		out = append(out, RankEntryReport{MappingID: id, Rank: len(out), Explanation: "quota routing not configured"})
	}
	return out
}

// StatusView selects provider information only. Route-local failures are
// deliberately invisible to this selector.
func (s DiagnosticSnapshot) StatusView() StatusViewReport {
	report := StatusViewReport{AsOf: s.asOf, Error: s.fatalError}
	if s.fatalError != "" {
		return report
	}
	report.Providers = cloneProviders(s.providers)
	report.Errors = cloneDiagnosticErrors(s.providerErrors)
	return report
}

// RoutingView selects effective chains and route-local failures only.
func (s DiagnosticSnapshot) RoutingView() RoutingReport {
	report := RoutingReport{AsOf: s.asOf, Error: s.fatalError}
	if s.fatalError != "" {
		return report
	}
	report.RoutingEnabled = s.routingEnabled
	report.Routes = cloneRoutes(s.routes, false)
	report.Errors = cloneDiagnosticErrors(s.routeErrors)
	report.Partial = len(report.Errors) > 0
	return report
}

// RoutingExplainView adds policy enablement, every rank explanation, and desired
// chains to the same effective route projection.
func (s DiagnosticSnapshot) RoutingExplainView() RoutingExplainReport {
	report := RoutingExplainReport{AsOf: s.asOf, Error: s.fatalError}
	if s.fatalError != "" {
		return report
	}
	report.RoutingEnabled = s.routingEnabled
	report.Ranks = append([]RankEntryReport(nil), s.ranks...)
	report.Routes = cloneRoutes(s.routes, true)
	report.Errors = cloneDiagnosticErrors(s.routeErrors)
	report.Partial = len(report.Errors) > 0
	return report
}

func cloneDiagnosticErrors(in []DiagnosticError) []DiagnosticError {
	return append([]DiagnosticError(nil), in...)
}

// ObservedState returns the raw observed state loaded exactly once during
// snapshot construction. Downstream consumers (doctor) use it to surface
// pending targets and recovered history without a duplicate state load.
func (s DiagnosticSnapshot) ObservedState() state.State {
	return s.observed
}

// DesiredPolicy returns the raw desired policy loaded exactly once during
// snapshot construction. Downstream consumers (doctor) use it to build quota
// probes without a duplicate policy load.
func (s DiagnosticSnapshot) DesiredPolicy() policy.Desired {
	return s.desired
}

// AsOf returns the single clock sample used by every time-sensitive projection.
func (s DiagnosticSnapshot) AsOf() time.Time {
	return s.asOf
}

// PolicyError returns the policy load error (nil on success) captured once
// during snapshot construction, so the doctor can surface a configuration
// finding without re-loading policy.
func (s DiagnosticSnapshot) PolicyError() error {
	return s.policyErr
}

// ResolveError returns the target resolution error (nil on success) captured
// once during snapshot construction, so the doctor can surface a configuration
// finding without re-resolving targets.
func (s DiagnosticSnapshot) ResolveError() error {
	return s.resolveErr
}

// StateError returns the state load error (nil on success) captured once
// during snapshot construction, so the doctor can surface a state-unreadable
// finding without re-loading state.
func (s DiagnosticSnapshot) StateError() error {
	return s.stateErr
}
