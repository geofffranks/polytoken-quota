package service

import (
	"errors"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
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

// ProviderProjection is one exact mapping-level diagnostic projection.
type ProviderProjection struct {
	MappingID      string              `json:"mapping_id"`
	Adapter        string              `json:"adapter,omitempty"`
	QuotaClass     quota.QuotaClass    `json:"quota_class"`
	Availability   state.Availability  `json:"availability"`
	EffectiveMode  state.Mode          `json:"effective_mode"`
	ManualDisabled bool                `json:"manual_disabled"`
	Reason         string              `json:"reason"`
	CheckedAt      time.Time           `json:"checked_at,omitempty"`
	Freshness      Freshness           `json:"freshness"`
	Windows        []QuotaWindowReport `json:"windows,omitempty"`
	NextResetAt    *time.Time          `json:"next_reset_at,omitempty"`
	LatestAttempt  *QuotaAttemptReport `json:"latest_attempt,omitempty"`
	Usage          *UsageSummaryReport `json:"usage,omitempty"`
	ResetCredits   *ResetCreditReport  `json:"reset_credits,omitempty"`
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
	for id, mapping := range desired.Providers {
		if mapping.Quota == nil {
			continue
		}
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	providers := make([]ProviderProjection, 0, len(ids))
	var projectionErrors []DiagnosticError
	for _, id := range ids {
		mapping := desired.Providers[policy.MappingID(id)]
		ps := mappingProviderState(id, mapping, observed.Providers)
		entry := ProviderProjection{
			MappingID: id, Availability: ps.Availability, EffectiveMode: state.EffectiveMode(ps),
			ManualDisabled: ps.ManualDisabled, Reason: providerReason(ps),
			Freshness: FreshnessMissing, QuotaClass: quota.ClassUnknown,
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
			entry.NextResetAt = nextResetAfter(ps.QuotaSnapshot.Windows, asOf)
		}
		if ps.QuotaAttempt != nil {
			entry.LatestAttempt = &QuotaAttemptReport{
				Status: ps.QuotaAttempt.Status, CheckedAt: ps.QuotaAttempt.CheckedAt,
				Error: quota.SanitizeError(errors.New(ps.QuotaAttempt.Error)),
			}
		}
		entry.Usage = usageSummaryReport(ps.ResetCredits.UsageSummary)
		entry.ResetCredits = resetCreditReport(ps.ResetCredits, asOf)
		providers = append(providers, entry)
	}
	return providers, projectionErrors
}

func mappingProviderState(id string, mapping policy.Mapping, observed map[string]state.ProviderState) state.ProviderState {
	if len(mapping.CodexBarProviders) == 0 {
		return observed[id]
	}
	ps := aggregateMappingState(mapping.CodexBarProviders, observed)
	ps.ResetCredits = selectResetCreditState(mapping.CodexBarProviders, observed)
	return ps
}

func selectResetCreditState(aliases []string, providers map[string]state.ProviderState) quota.ResetCreditState {
	var selected quota.ResetCreditState
	var selectedAt time.Time
	var selectedAlias string
	for _, alias := range aliases {
		candidate, ok := providers[alias]
		if !ok {
			continue
		}
		at := resetCreditStateAt(candidate.ResetCredits)
		if selectedAlias == "" || at.After(selectedAt) || (at.Equal(selectedAt) && alias < selectedAlias) {
			selected = candidate.ResetCredits
			selectedAt = at
			selectedAlias = alias
		}
	}
	return selected
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

func nextResetAfter(windows []quota.QuotaWindow, asOf time.Time) *time.Time {
	var earliest *time.Time
	for _, window := range windows {
		if window.ResetAt == nil || !window.ResetAt.After(asOf) {
			continue
		}
		if earliest == nil || window.ResetAt.Before(*earliest) {
			earliest = cloneTime(window.ResetAt)
		}
	}
	return earliest
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
		if in[i].LatestAttempt != nil {
			attempt := *in[i].LatestAttempt
			out[i].LatestAttempt = &attempt
		}
		out[i].Usage = cloneUsage(in[i].Usage)
		out[i].ResetCredits = cloneResetCredits(in[i].ResetCredits)
	}
	return out
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
