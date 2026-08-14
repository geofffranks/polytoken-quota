// anthropic.go implements the Anthropic API quota adapter for pay-as-you-go
// API customers (NOT Claude subscription plans, which expose no usable API).
//
// A metered API has no token allowance to run out of — the real "quota" is
// the money the operator is willing to spend. The adapter therefore measures
// month-to-date organization spend via the Admin API cost report
// (GET /v1/organizations/cost_report) against a user-defined monthly budget
// (the mapping's `monthly_budget_usd` in desired.yaml) and reports it as a
// single "monthly" window: spend/budget as used/limit, resetting at the first
// of the next month (UTC).
//
// Spend visibility is coarse: the cost report accepts only 1d (UTC) buckets
// and omits the still-open day until the UTC day closes, so the reported
// month-to-date spend can trail reality by up to a day's usage. The reported
// number is a lower bound; operators should set the budget with a heavy day's
// margin below their true ceiling.
//
// Two alternatives were evaluated and rejected:
//   - the anthropic-ratelimit-* response headers: per-minute buckets that
//     refill in ~60 seconds — the wrong timescale for session-start routing
//     (a polled snapshot is stale within a minute and almost always reads
//     "fully available");
//   - the free /v1/messages/count_tokens endpoint as a header probe: it
//     returns no ratelimit headers at all.
//
// Credentials are transient: the ADMIN API key (sk-ant-admin…, created in
// Console → Settings → Admin keys; a standard sk-ant-api key gets 401 here)
// is read from ANTHROPIC_ADMIN_API_KEY, attached to the immediate request(s),
// and discarded. No keys, auth headers, account/workspace identifiers, or raw
// response bodies are ever persisted, logged, or included in a snapshot or
// error. All errors are sanitized via SanitizeError.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicCostReportBase = "https://api.anthropic.com/v1/organizations/cost_report"
	// anthropicProviderName is the evidence-registry key and adapter identifier.
	anthropicProviderName = "anthropic"
	// anthropicAdminKeyEnv is the canonical env var the transient Admin API
	// key resolves from (matching the official docs' example variable).
	anthropicAdminKeyEnv = "ANTHROPIC_ADMIN_API_KEY"
	// anthropicVersion is the required anthropic-version header value.
	anthropicVersion = "2023-06-01"
	// anthropicMaxPages bounds cost-report pagination so a misbehaving server
	// can never hold a poll open indefinitely.
	anthropicMaxPages = 40
	// anthropicCentsPerDollar converts cost-report amounts to dollars: the
	// report's `amount` is a decimal string in cents (hundredths of USD), so
	// treating it as dollars would overstate spend 100×.
	anthropicCentsPerDollar = 100.0
)

// AnthropicSource polls the Anthropic Admin cost report behind the evidence
// gate and reports month-to-date spend against the configured monthly budget.
// It satisfies the QuotaSource interface. The HTTP transport is injectable for
// tests; the Admin key is resolved transiently and never retained.
type AnthropicSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials CredentialResolver
	Evidence    *EvidenceRegistry
	// MonthlyBudgetUSD is the spend ceiling treated as this provider's quota.
	// It must be positive; the policy loader enforces this for anthropic
	// mappings, and Fetch fails closed when it is unset.
	MonthlyBudgetUSD float64
	Now              func() time.Time
}

// AnthropicEvidence returns the sanitized contract evidence for the Anthropic
// adapter. Register it in an EvidenceRegistry so the gate recognizes
// "anthropic" as fresh. The dates are release-owned and do not renew on
// construction. ReviewBy is three months after the recording date (quarterly
// review): the cost-report schema is documented but the amount-unit semantics
// were confirmed against a live organization, so it reviews on the faster
// cadence.
func AnthropicEvidence(_ time.Time) Evidence {
	return Evidence{
		Provider:    anthropicProviderName,
		Endpoint:    anthropicCostReportBase,
		Method:      http.MethodGet,
		AuthType:    "admin-api-key",
		SchemaNote:  "envelope {data:[{starting_at,ending_at,results:[{amount,currency,...}]}],has_more,next_page}; amount is a decimal string in cents; USD only; 1d buckets only, open day absent until UTC day close",
		FixturePath: "contract/testdata/quota/anthropic/midmonth.json",
		RecordedAt:  evidenceRecordedAt(),
		ReviewBy:    evidenceRecordedAt().AddDate(0, 3, 0), // quarterly review per evidence policy
	}
}

// NewAnthropicSource constructs an AnthropicSource consulting the supplied
// release-owned evidence registry. budgetUSD is the mapping's
// monthly_budget_usd. Construction never registers or refreshes evidence. If
// reg is nil, an empty registry is created and the source is unsupported.
func NewAnthropicSource(mappingID string, client *BoundedClient, creds CredentialResolver, budgetUSD float64, reg *EvidenceRegistry, now time.Time) *AnthropicSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	return &AnthropicSource{
		mappingID:        mappingID,
		Client:           client,
		Credentials:      creds,
		Evidence:         reg,
		MonthlyBudgetUSD: budgetUSD,
		Now:              func() time.Time { return now },
	}
}

// MappingID returns the provider mapping this source serves.
func (a *AnthropicSource) MappingID() string { return a.mappingID }

// now returns the current observation time, defaulting to time.Now when unset.
func (a *AnthropicSource) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Status reports whether this source is supported. It consults the evidence
// registry and is fail-closed: absent, expired, or incomplete evidence yields
// an unsupported status with a sanitized remediation reason. A missing budget
// is likewise unsupported: there is nothing to measure spend against.
func (a *AnthropicSource) Status() SupportStatus {
	if a.MonthlyBudgetUSD <= 0 || math.IsNaN(a.MonthlyBudgetUSD) || math.IsInf(a.MonthlyBudgetUSD, 0) {
		return SupportStatus{
			Supported: false,
			Reason:    "anthropic adapter requires monthly_budget_usd in the mapping's quota config",
		}
	}
	return SupportFromEvidence(a.evidenceStatus())
}

func (a *AnthropicSource) evidenceStatus() EvidenceStatus {
	if a.Evidence == nil {
		return EvidenceStatus{
			State:  EvidenceAbsent,
			Reason: "provider " + anthropicProviderName + " has no recorded contract evidence; record evidence before enabling",
		}
	}
	return a.Evidence.Status(anthropicProviderName, a.now())
}

// Fetch retrieves the month-to-date spend snapshot. It fails closed when the
// evidence gate (or budget) is unsupported without making any request,
// resolves the Admin key transiently, pages through the current month's cost
// report with bounded pagination, and returns one "monthly" window of
// spend-vs-budget. Errors never carry API keys, auth headers, or raw bodies.
func (a *AnthropicSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := a.Status()
	if !st.Supported {
		return a.fail(st.Reason), errors.New(st.Reason)
	}

	adminKey, err := a.resolveCredentials()
	if err != nil {
		msg := SanitizeError(err)
		return a.fail(msg), errors.New(msg)
	}

	spend, partial, ferr := a.fetchMonthToDateSpend(ctx, adminKey)
	if ferr != nil {
		msg := SanitizeError(ferr)
		return a.fail(msg), errors.New(msg)
	}

	window := a.budgetWindow(spend)
	status := SourceFresh
	if partial {
		status = SourcePartial
	}
	windows := []QuotaWindow{window}
	return QuotaSnapshot{
		MappingID:    a.mappingID,
		CheckedAt:    a.now(),
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
	}, nil
}

// budgetWindow builds the single monthly spend-vs-budget window. Spend can
// legitimately exceed the budget (the budget is advisory, not enforced by
// Anthropic), so Used is not clamped — but UsagePercent is capped at 100,
// which routing treats as exhausted.
func (a *AnthropicSource) budgetWindow(spendUSD float64) QuotaWindow {
	limit := a.MonthlyBudgetUSD
	used := spendUSD
	pct := clampRange(used/limit*100, 0, 100)
	now := a.now()
	start := monthStart(now)
	reset := nextMonthStart(now)
	period := reset.Sub(start)
	return QuotaWindow{
		Name:         "monthly",
		Used:         &used,
		Limit:        &limit,
		UsagePercent: &pct,
		ResetAt:      &reset,
		Period:       &period,
	}
}

// fail returns a sanitized failed snapshot for this source's mapping.
func (a *AnthropicSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{
		MappingID:    a.mappingID,
		Availability: QuotaUnknown,
		Status:       SourceFailed,
		Error:        reason,
	}
}

// resolveCredentials reads the Admin API key transiently from
// ANTHROPIC_ADMIN_API_KEY. The key is used for the immediate request(s) only.
// Errors are generic and never include the key value.
func (a *AnthropicSource) resolveCredentials() (string, error) {
	raw, err := a.Credentials.Resolve(CredentialRef{Kind: CredentialEnv, Locator: anthropicAdminKeyEnv})
	if err != nil {
		return "", errors.New("anthropic: could not resolve " + anthropicAdminKeyEnv)
	}
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errors.New("anthropic: " + anthropicAdminKeyEnv + " is empty")
	}
	return key, nil
}

// fetchMonthToDateSpend pages through the cost report from the first of the
// current month (UTC) and sums the USD amounts. Pagination is bounded by
// anthropicMaxPages. partial reports skipped entries (non-USD currencies or
// malformed amounts) — the sum is then a lower bound, never an invented value.
func (a *AnthropicSource) fetchMonthToDateSpend(ctx context.Context, adminKey string) (spend float64, partial bool, err error) {
	page := ""
	for i := 0; i < anthropicMaxPages; i++ {
		body, herr := a.fetchCostPage(ctx, adminKey, page)
		if herr != nil {
			return 0, false, herr
		}
		pageSpend, pagePartial, next, perr := parseAnthropicCostPage(body)
		if perr != nil {
			return 0, false, perr
		}
		spend += pageSpend
		partial = partial || pagePartial
		if next == "" {
			return spend, partial, nil
		}
		page = next
	}
	return 0, false, errors.New("anthropic: cost report pagination exceeded the page bound")
}

// fetchCostPage performs one bounded HTTPS cost-report request.
func (a *AnthropicSource) fetchCostPage(ctx context.Context, adminKey, page string) ([]byte, error) {
	q := url.Values{}
	q.Set("starting_at", monthStart(a.now()).Format(time.RFC3339))
	if page != "" {
		q.Set("page", page)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicCostReportBase+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", adminKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("anthropic: auth failure (HTTP %d); check %s (must be an ADMIN key from Console → Settings → Admin keys, not a standard API key)", resp.StatusCode, anthropicAdminKeyEnv)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("anthropic: cost report rate limited (HTTP 429); retry on the next scheduled check")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("anthropic: server error (HTTP %d)", resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		return nil, errors.New("anthropic: empty cost report response body")
	}
	return resp.Body, nil
}

// --- Response parsing (lenient, per-entry) ----------------------------------

// anthropicCostEnvelope is the cost report page envelope.
type anthropicCostEnvelope struct {
	Data        []anthropicCostBucket `json:"data"`
	HasMore     bool                  `json:"has_more"`
	NextPage    *string               `json:"next_page"`
	dataSet     bool
	hasMoreSet  bool
	dataNull    bool
	hasMoreNull bool
}

func (e *anthropicCostEnvelope) UnmarshalJSON(data []byte) error {
	type plain anthropicCostEnvelope
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["data"]; ok {
		decoded.dataSet = true
		decoded.dataNull = string(raw) == "null"
	}
	if raw, ok := fields["has_more"]; ok {
		decoded.hasMoreSet = true
		decoded.hasMoreNull = string(raw) == "null"
	}
	*e = anthropicCostEnvelope(decoded)
	return nil
}

// anthropicCostBucket is one time bucket with its cost line items.
type anthropicCostBucket struct {
	Results     []json.RawMessage `json:"results"`
	resultsSet  bool
	resultsNull bool
}

func (b *anthropicCostBucket) UnmarshalJSON(data []byte) error {
	type plain anthropicCostBucket
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["results"]; ok {
		decoded.resultsSet = true
		decoded.resultsNull = string(raw) == "null"
	}
	*b = anthropicCostBucket(decoded)
	return nil
}

// anthropicCostItem is one cost line item. Amount is a decimal string; the
// currency is currently always "USD" per the documented contract.
type anthropicCostItem struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// parseAnthropicCostPage sums the USD amounts of one cost-report page.
// Individual line items are decoded independently: a malformed item or an
// unexpected currency is skipped and partial is set true rather than failing
// the page, so the returned sum is a defensible lower bound. An empty data
// array is valid (no spend recorded yet, e.g. early in the month or within
// the report's 1-2h lag).
func parseAnthropicCostPage(body []byte) (spend float64, partial bool, nextPage string, err error) {
	var env anthropicCostEnvelope
	if uerr := json.Unmarshal(body, &env); uerr != nil || !env.dataSet || env.dataNull || !env.hasMoreSet || env.hasMoreNull {
		return 0, false, "", errors.New("anthropic: invalid cost report body (missing required envelope fields)")
	}
	for _, bucket := range env.Data {
		if !bucket.resultsSet || bucket.resultsNull {
			return 0, false, "", errors.New("anthropic: invalid cost report body (missing bucket results)")
		}
		for _, raw := range bucket.Results {
			var item anthropicCostItem
			if uerr := json.Unmarshal(raw, &item); uerr != nil {
				partial = true
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(item.Currency), "USD") {
				partial = true // never guess an exchange rate
				continue
			}
			cents, perr := parseCostAmount(item.Amount)
			if perr != nil {
				partial = true
				continue
			}
			spend += cents / anthropicCentsPerDollar
		}
	}
	if env.HasMore {
		if env.NextPage == nil || strings.TrimSpace(*env.NextPage) == "" {
			return 0, false, "", errors.New("anthropic: invalid cost report pagination (has_more without next_page)")
		}
		nextPage = strings.TrimSpace(*env.NextPage)
	}
	return spend, partial, nextPage, nil
}

// parseCostAmount parses a decimal-string amount in cents. Negative and
// non-finite values are rejected (credits/refunds are not modeled; skipping
// them can only overstate spend, which understates remaining headroom — the
// fail-safe direction — and callers mark the snapshot partial).
func parseCostAmount(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New("malformed amount")
	}
	if v < 0 {
		return 0, errors.New("negative amount")
	}
	return finiteOrErr(v)
}

// monthStart returns the first instant of t's month in UTC.
func monthStart(t time.Time) time.Time {
	tt := t.UTC()
	return time.Date(tt.Year(), tt.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// nextMonthStart returns the first instant of the month after t's month (UTC)
// — the moment the budget window resets.
func nextMonthStart(t time.Time) time.Time {
	return monthStart(t).AddDate(0, 1, 0)
}

// Compile-time assertion that AnthropicSource satisfies the QuotaSource interface.
var _ QuotaSource = (*AnthropicSource)(nil)
