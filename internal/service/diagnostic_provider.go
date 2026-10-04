package service

import (
	"errors"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Freshness classifies the last successful quota observation at AsOf.
type Freshness string

const (
	FreshnessMissing Freshness = "missing"
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
)

// QuotaWindowReport is one complete window projection.
type QuotaWindowReport struct {
	Name         string     `json:"name"`
	Used         *float64   `json:"used,omitempty"`
	Limit        *float64   `json:"limit,omitempty"`
	UsagePercent *float64   `json:"usage_percent,omitempty"`
	Remaining    *float64   `json:"remaining,omitempty"`
	ResetAt      *time.Time `json:"reset_at,omitempty"`
}

// QuotaAttemptReport is the sanitized latest quota attempt.
type QuotaAttemptReport struct {
	Status    quota.SourceStatus `json:"status"`
	Error     string             `json:"error,omitempty"`
	CheckedAt time.Time          `json:"checked_at,omitempty"`
}

// UsageCreditsReport is ordinary usage-credit state, separate from reset credits.
type UsageCreditsReport struct {
	HasCredits *bool   `json:"has_credits,omitempty"`
	Unlimited  *bool   `json:"unlimited,omitempty"`
	Balance    *string `json:"balance,omitempty"`
}

// SpendControlReport is ordinary monthly spend-control state.
type SpendControlReport struct {
	Limit     *float64   `json:"limit,omitempty"`
	Used      *float64   `json:"used,omitempty"`
	Remaining *float64   `json:"remaining,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// UsageSummaryReport records ordinary credit/spend provenance.
type UsageSummaryReport struct {
	ObservedAt   time.Time           `json:"observed_at"`
	Credits      *UsageCreditsReport `json:"credits,omitempty"`
	SpendControl *SpendControlReport `json:"spend_control,omitempty"`
}

// ResetCreditInventoryReport is one copied successful inventory observation.
type ResetCreditInventoryReport struct {
	ServerAvailableCount int          `json:"server_available_count"`
	UsableCount          int          `json:"usable_count"`
	AvailableExpiries    []*time.Time `json:"available_expiries,omitempty"`
	DiscrepancyCount     int          `json:"discrepancy_count"`
	SkippedCount         int          `json:"skipped_count"`
	ObservedAt           time.Time    `json:"observed_at"`
}

// ResetCreditAttemptReport is the copied latest endpoint attempt.
type ResetCreditAttemptReport struct {
	Status    quota.CreditAttemptStatus   `json:"status"`
	At        time.Time                   `json:"at"`
	Inventory *ResetCreditInventoryReport `json:"inventory,omitempty"`
	Error     string                      `json:"error,omitempty"`
}

// ResetCreditReport preserves success/attempt provenance and provides an AsOf
// summary whose count/expiries exclude expired items without changing history.
type ResetCreditReport struct {
	LastSuccess          *ResetCreditInventoryReport `json:"last_success,omitempty"`
	LatestAttempt        *ResetCreditAttemptReport   `json:"latest_attempt,omitempty"`
	Status               quota.CreditAttemptStatus   `json:"status,omitempty"`
	ServerAvailableCount int                         `json:"server_available_count"`
	UsableCount          *int                        `json:"usable_count,omitempty"`
	AvailableExpiries    []*time.Time                `json:"available_expiries,omitempty"`
	DiscrepancyCount     int                         `json:"discrepancy_count"`
	SkippedCount         int                         `json:"skipped_count"`
}

// GateReport is the sanitized gate attribution for one provider mapping: the
// gating axis that holds its `enabled` field off, and — for signal claims —
// the observed use-it-or-lose-it signal, the engaged threshold, and the
// revision that engaged the claim. It is diagnostic surfacing only; it never
// changes the snapshot's fail-closed aggregation semantics.
type GateReport struct {
	Axis            string   `json:"axis"`
	Signal          *float64 `json:"signal,omitempty"`
	Threshold       *float64 `json:"threshold,omitempty"`
	EngagedRevision uint64   `json:"engaged_revision,omitempty"`
	// Conflict records a detected operator divergence from the held claim:
	// the live enabled field is operator-controlled, so the provider is not
	// actually held off. Status presentation must not render such a claim as
	// an active gate.
	Conflict bool `json:"conflict,omitempty"`
}

// ProviderProjection is one exact mapping-level diagnostic projection. Every
// configured mapping is projected; mappings without a pollable quota config use
// their observed state and remain visible without fabricated quota data.
type ProviderProjection struct {
	MappingID    string             `json:"mapping_id"`
	Adapter      string             `json:"adapter,omitempty"`
	QuotaClass   quota.QuotaClass   `json:"quota_class"`
	Availability state.Availability `json:"availability"`
	// Quota is the row's aggregated quota axis, exposed so presentation can
	// name a quota-exhausted cause instead of the generic disabled text.
	Quota          state.Quota `json:"quota,omitempty"`
	EffectiveMode  state.Mode  `json:"effective_mode"`
	ManualDisabled bool        `json:"manual_disabled"`
	Reason         string      `json:"reason"`
	CheckedAt      time.Time   `json:"checked_at,omitempty"`
	Freshness      Freshness   `json:"freshness"`
	// Signal is the provider's current use-it-or-lose-it pace, computed from
	// the saved quota snapshot at the projection's AsOf via
	// routing.ComputeSignal — the same formula the ranking and the signal gate
	// use, evaluated now. Diagnostic-only: it is computed whether or not any
	// gate engages, and is nil (omitted) when no snapshot exists or no window
	// qualifies, never fabricated as zero.
	Signal        *float64            `json:"signal,omitempty"`
	Windows       []QuotaWindowReport `json:"windows,omitempty"`
	NextResetAt   *time.Time          `json:"next_reset_at,omitempty"`
	LatestAttempt *QuotaAttemptReport `json:"latest_attempt,omitempty"`
	Usage         *UsageSummaryReport `json:"usage,omitempty"`
	ResetCredits  *ResetCreditReport  `json:"reset_credits,omitempty"`
	// Gate carries the provider-only gate attribution; nil when quota holds
	// no claim over this provider's enabled field (including legacy claims
	// recorded before axis attribution, which keep today's presentation).
	Gate *GateReport `json:"gate,omitempty"`
	// Condition names the snapshot's sanitized out-of-quota condition when
	// the adapter failed closed on the observation, whether or not usable
	// numbers were retained alongside it; empty otherwise.
	Condition string `json:"condition,omitempty"`
	// SnapshotAvailability is the stored quota snapshot's own availability
	// (available/unavailable/unknown), read from the raw pre-aggregation
	// snapshot. Distinct from Availability above: the fail-closed row axis
	// has no unknown member and is forced to unavailable for every
	// windowless snapshot, losing the distinction the QUOTA fallback needs.
	SnapshotAvailability quota.QuotaAvailability `json:"snapshot_availability,omitempty"`
	// QuotaGateSuspended records that the provider's quota section declares
	// `quota_gate: {enabled: false}`: the raw EffectiveMode/Reason above stay
	// exactly as observed (exemption-blind), and this flag names the policy
	// fact that automatic quota gating will not act on them.
	QuotaGateSuspended bool `json:"quota_gate_suspended,omitempty"`
}

// StatusViewReport is the provider-only status selector.
type StatusViewReport struct {
	AsOf      time.Time            `json:"as_of"`
	Providers []ProviderProjection `json:"providers,omitempty"`
	Errors    []DiagnosticError    `json:"errors,omitempty"`
	Error     string               `json:"error,omitempty"`
}

func projectProviders(desired policy.Desired, observed state.State, asOf time.Time) ([]ProviderProjection, []DiagnosticError) {
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	providers := make([]ProviderProjection, 0, len(ids))
	var projectionErrors []DiagnosticError
	for _, id := range ids {
		mapping := desired.Providers[policy.MappingID(id)]
		ps := mappingProviderState(id, mapping, observed.Providers)
		entry := ProviderProjection{
			MappingID: id, Availability: ps.Availability, Quota: ps.Quota, EffectiveMode: state.EffectiveMode(ps),
			ManualDisabled: ps.ManualDisabled, Reason: providerReason(ps),
			Freshness: FreshnessMissing, QuotaClass: quota.ClassUnknown,
			QuotaGateSuspended: reconcile.QuotaGateExempt(mapping),
		}
		// Condition and snapshot availability come from the raw
		// pre-aggregation snapshot: aggregateMappingState rewrites the
		// aggregated copy's availability to unknown for every fail-closed
		// snapshot, erasing the unavailable-vs-unknown distinction the
		// status view renders.
		if raw := observed.Providers[id]; raw.QuotaSnapshot != nil {
			entry.SnapshotAvailability = raw.QuotaSnapshot.Availability
			entry.Condition = quota.NormalizeQuotaCondition(raw.QuotaSnapshot.Condition)
		}
		var ttl time.Duration
		if mapping.Quota != nil {
			entry.Adapter = mapping.Quota.Adapter
			ttl = mapping.Quota.FreshnessTTL
		}
		if ps.QuotaSnapshot != nil {
			entry.CheckedAt = ps.QuotaSnapshot.CheckedAt
			entry.QuotaClass = ps.QuotaSnapshot.Class()
			entry.Freshness = classifyFreshness(ps.QuotaSnapshot.CheckedAt, ttl, asOf)
			entry.Windows = windowsReport(ps.QuotaSnapshot)
			entry.NextResetAt = quota.NextQuotaResetAt(ps.QuotaSnapshot.Windows, asOf)
			if signal, ok := routing.ComputeSignal(ps.QuotaSnapshot, asOf); ok {
				s := signal
				entry.Signal = &s
			}
		}
		if ps.QuotaAttempt != nil {
			entry.LatestAttempt = &QuotaAttemptReport{
				Status: ps.QuotaAttempt.Status, CheckedAt: ps.QuotaAttempt.CheckedAt,
				Error: quota.SanitizeError(errors.New(ps.QuotaAttempt.Error)),
			}
		}
		entry.Usage = usageSummaryReport(ps.ResetCredits.UsageSummary)
		entry.ResetCredits = resetCreditReport(ps.ResetCredits, asOf)
		if record, ok := observed.OwnershipOf(id); ok && record.Owned && record.Axis != "" {
			g := &GateReport{Axis: record.Axis, EngagedRevision: record.EngagedRevision, Conflict: record.Conflict}
			if record.Axis == state.OwnershipAxisSignal {
				if ps.QuotaSnapshot != nil {
					if signal, ok := routing.ComputeSignal(ps.QuotaSnapshot, asOf); ok {
						s := signal
						g.Signal = &s
					}
				}
				t := record.Threshold
				g.Threshold = &t
			}
			entry.Gate = g
		}
		providers = append(providers, entry)
	}
	return providers, projectionErrors
}

func mappingProviderState(id string, mapping policy.Mapping, observed map[string]state.ProviderState) state.ProviderState {
	if mapping.Quota == nil {
		return observed[id]
	}
	return aggregateMappingState(id, observed)
}

func resetCreditStateAt(s quota.ResetCreditState) time.Time {
	if s.LatestAttempt != nil {
		return s.LatestAttempt.At
	}
	if s.LastSuccess != nil {
		return s.LastSuccess.ObservedAt
	}
	if s.UsageSummary != nil {
		return s.UsageSummary.ObservedAt
	}
	return time.Time{}
}

func classifyFreshness(checkedAt time.Time, ttl time.Duration, asOf time.Time) Freshness {
	if checkedAt.IsZero() {
		return FreshnessMissing
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if asOf.Sub(checkedAt) > ttl {
		return FreshnessStale
	}
	return FreshnessFresh
}

func windowsReport(s *quota.QuotaSnapshot) []QuotaWindowReport {
	if s == nil || len(s.Windows) == 0 {
		return nil
	}
	out := make([]QuotaWindowReport, 0, len(s.Windows))
	for _, window := range s.Windows {
		out = append(out, QuotaWindowReport{
			Name: window.Name, Used: cloneFloat(window.Used), Limit: cloneFloat(window.Limit),
			UsagePercent: cloneFloat(window.UsagePercent), Remaining: cloneFloat(window.Remaining()),
			ResetAt: cloneTime(window.ResetAt),
		})
	}
	return out
}

func usageSummaryReport(summary *quota.CodexUsageSummary) *UsageSummaryReport {
	if summary == nil {
		return nil
	}
	out := &UsageSummaryReport{ObservedAt: summary.ObservedAt}
	if summary.Credits != nil {
		out.Credits = &UsageCreditsReport{
			HasCredits: cloneBool(summary.Credits.HasCredits), Unlimited: cloneBool(summary.Credits.Unlimited),
			Balance: cloneString(summary.Credits.Balance),
		}
	}
	if summary.SpendControl != nil {
		out.SpendControl = &SpendControlReport{
			Limit: cloneFloat(summary.SpendControl.Limit), Used: cloneFloat(summary.SpendControl.Used),
			Remaining: cloneFloat(summary.SpendControl.Remaining), ResetAt: cloneTime(summary.SpendControl.ResetAt),
		}
	}
	return out
}

func resetCreditReport(credits quota.ResetCreditState, asOf time.Time) *ResetCreditReport {
	if credits.LastSuccess == nil && credits.LatestAttempt == nil {
		return nil
	}
	out := &ResetCreditReport{
		LastSuccess:   resetInventoryReport(credits.LastSuccess),
		LatestAttempt: resetAttemptReport(credits.LatestAttempt),
	}
	if credits.LatestAttempt != nil {
		out.Status = credits.LatestAttempt.Status
	}
	if credits.LastSuccess != nil {
		out.ServerAvailableCount = credits.LastSuccess.ServerAvailableCount
		out.UsableCount = credits.UsableCountAt(asOf)
		out.DiscrepancyCount = credits.LastSuccess.DiscrepancyCount
		out.SkippedCount = credits.LastSuccess.SkippedCount
		for _, expiry := range credits.LastSuccess.AvailableExpiries {
			if expiry == nil || expiry.After(asOf) {
				out.AvailableExpiries = append(out.AvailableExpiries, cloneTime(expiry))
			}
		}
	}
	return out
}

func resetInventoryReport(in *quota.ResetCreditInventory) *ResetCreditInventoryReport {
	if in == nil {
		return nil
	}
	out := &ResetCreditInventoryReport{
		ServerAvailableCount: in.ServerAvailableCount, UsableCount: in.UsableCount,
		DiscrepancyCount: in.DiscrepancyCount, SkippedCount: in.SkippedCount, ObservedAt: in.ObservedAt,
	}
	for _, expiry := range in.AvailableExpiries {
		out.AvailableExpiries = append(out.AvailableExpiries, cloneTime(expiry))
	}
	return out
}

func resetAttemptReport(in *quota.ResetCreditAttempt) *ResetCreditAttemptReport {
	if in == nil {
		return nil
	}
	return &ResetCreditAttemptReport{
		Status: in.Status, At: in.At, Inventory: resetInventoryReport(in.Inventory),
		Error: quota.SanitizeError(errors.New(in.Error)),
	}
}

func cloneProviders(in []ProviderProjection) []ProviderProjection {
	out := make([]ProviderProjection, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Windows = cloneWindows(in[i].Windows)
		out[i].NextResetAt = cloneTime(in[i].NextResetAt)
		out[i].Signal = cloneFloat(in[i].Signal)
		if in[i].LatestAttempt != nil {
			attempt := *in[i].LatestAttempt
			out[i].LatestAttempt = &attempt
		}
		out[i].Usage = cloneUsage(in[i].Usage)
		out[i].ResetCredits = cloneResetCredits(in[i].ResetCredits)
		out[i].Gate = cloneGateReport(in[i].Gate)
	}
	return out
}

// cloneGateReport deep-copies a gate attribution report (nil stays nil).
func cloneGateReport(in *GateReport) *GateReport {
	if in == nil {
		return nil
	}
	out := *in
	out.Signal = cloneFloat(in.Signal)
	out.Threshold = cloneFloat(in.Threshold)
	return &out
}

func cloneWindows(in []QuotaWindowReport) []QuotaWindowReport {
	out := make([]QuotaWindowReport, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Used = cloneFloat(in[i].Used)
		out[i].Limit = cloneFloat(in[i].Limit)
		out[i].UsagePercent = cloneFloat(in[i].UsagePercent)
		out[i].Remaining = cloneFloat(in[i].Remaining)
		out[i].ResetAt = cloneTime(in[i].ResetAt)
	}
	return out
}

func cloneUsage(in *UsageSummaryReport) *UsageSummaryReport {
	if in == nil {
		return nil
	}
	out := *in
	if in.Credits != nil {
		credits := *in.Credits
		credits.HasCredits = cloneBool(in.Credits.HasCredits)
		credits.Unlimited = cloneBool(in.Credits.Unlimited)
		credits.Balance = cloneString(in.Credits.Balance)
		out.Credits = &credits
	}
	if in.SpendControl != nil {
		spend := *in.SpendControl
		spend.Limit = cloneFloat(in.SpendControl.Limit)
		spend.Used = cloneFloat(in.SpendControl.Used)
		spend.Remaining = cloneFloat(in.SpendControl.Remaining)
		spend.ResetAt = cloneTime(in.SpendControl.ResetAt)
		out.SpendControl = &spend
	}
	return &out
}

func cloneResetCredits(in *ResetCreditReport) *ResetCreditReport {
	if in == nil {
		return nil
	}
	out := *in
	out.LastSuccess = cloneResetInventory(in.LastSuccess)
	out.LatestAttempt = cloneResetAttempt(in.LatestAttempt)
	out.UsableCount = cloneInt(in.UsableCount)
	out.AvailableExpiries = cloneTimes(in.AvailableExpiries)
	return &out
}

func cloneResetInventory(in *ResetCreditInventoryReport) *ResetCreditInventoryReport {
	if in == nil {
		return nil
	}
	out := *in
	out.AvailableExpiries = cloneTimes(in.AvailableExpiries)
	return &out
}

func cloneResetAttempt(in *ResetCreditAttemptReport) *ResetCreditAttemptReport {
	if in == nil {
		return nil
	}
	out := *in
	out.Inventory = cloneResetInventory(in.Inventory)
	return &out
}

func cloneTimes(in []*time.Time) []*time.Time {
	out := make([]*time.Time, len(in))
	for i := range in {
		out[i] = cloneTime(in[i])
	}
	return out
}
func cloneTime(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
func cloneFloat(in *float64) *float64 {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
func cloneString(in *string) *string {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
