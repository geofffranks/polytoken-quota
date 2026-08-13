// neuralwatt.go implements the Neuralwatt Cloud quota/balance adapter.
//
// Neuralwatt exposes a read-only GET /v1/quota endpoint intended for provider
// routers. The response may describe a key allowance, a subscription allowance,
// or a prepaid/PAYG USD balance. The adapter selects the first present boundary
// in that order; a present but unusable boundary fails closed rather than falling
// back to a weaker signal.
//
// Credentials are transient: NEURALWATT_API_KEY is resolved for the immediate
// request, attached as a Bearer header, and discarded. No key, account identity,
// raw response, or provider-controlled message is persisted or returned.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	neuralwattQuotaEndpoint = "https://api.neuralwatt.com/v1/quota"
	neuralwattProviderName  = "neuralwatt"
	neuralwattAPIKeyEnv     = "NEURALWATT_API_KEY"
)

// NeuralwattSource polls Neuralwatt's quota endpoint behind the evidence gate.
type NeuralwattSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials CredentialResolver
	Evidence    *EvidenceRegistry
	Now         func() time.Time
}

// NeuralwattEvidence returns the reviewed, sanitized Neuralwatt contract record.
func NeuralwattEvidence(_ time.Time) Evidence {
	return Evidence{
		Provider:    neuralwattProviderName,
		Endpoint:    neuralwattQuotaEndpoint,
		Method:      http.MethodGet,
		AuthType:    "bearer-api-key",
		SchemaNote:  "balance credits, optional key.allowance, optional subscription allowance, usage/limits metadata may be null; blocked/overage states fail closed",
		FixturePath: "contract/testdata/quota/neuralwatt/quota.json",
		RecordedAt:  neuralwattEvidenceRecordedAt(),
		ReviewBy:    neuralwattEvidenceReviewBy(),
	}
}

// neuralwattEvidenceRecordedAt and neuralwattEvidenceReviewBy are release-owned
// dates. They change only when the endpoint contract and fixture are re-reviewed;
// constructing a poller must not renew evidence automatically.
func neuralwattEvidenceRecordedAt() time.Time {
	return time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
}

func neuralwattEvidenceReviewBy() time.Time {
	return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
}

// NewNeuralwattSource constructs a source using the supplied evidence registry.
// Construction never registers or refreshes evidence.
func NewNeuralwattSource(mappingID string, client *BoundedClient, creds CredentialResolver, reg *EvidenceRegistry, now time.Time) *NeuralwattSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	return &NeuralwattSource{
		mappingID:   mappingID,
		Client:      client,
		Credentials: creds,
		Evidence:    reg,
		Now:         func() time.Time { return now },
	}
}

func (n *NeuralwattSource) MappingID() string { return n.mappingID }

func cleanNeuralwattKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}

func (n *NeuralwattSource) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *NeuralwattSource) Status() SupportStatus {
	if n.Evidence == nil {
		return SupportStatus{Reason: "provider neuralwatt has no recorded contract evidence; record evidence before enabling"}
	}
	return SupportFromEvidence(n.Evidence.Status(neuralwattProviderName, n.now()))
}

func (n *NeuralwattSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := n.Status()
	if !st.Supported {
		return n.fail(st.Reason), errors.New(st.Reason)
	}
	if n.Client == nil || n.Credentials == nil {
		msg := "neuralwatt: adapter is not configured"
		return n.fail(msg), errors.New(msg)
	}
	key, err := n.Credentials.Resolve(CredentialRef{Kind: CredentialEnv, Locator: neuralwattAPIKeyEnv})
	key = cleanNeuralwattKey(key)
	if err != nil || key == "" {
		msg := "neuralwatt: could not resolve NEURALWATT_API_KEY"
		return n.fail(msg), errors.New(msg)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, neuralwattQuotaEndpoint, nil)
	if err != nil {
		msg := SanitizeError(err)
		return n.fail(msg), errors.New(msg)
	}
	req.Header.Set("Authorization", "Bearer "+cleanNeuralwattKey(key))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")
	resp, err := n.Client.Do(req)
	if err != nil {
		msg := SanitizeError(err)
		return n.fail(msg), errors.New(msg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("neuralwatt: server error (HTTP %d)", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			msg = "neuralwatt: authentication failed; check NEURALWATT_API_KEY"
		case http.StatusTooManyRequests:
			msg = "neuralwatt: quota endpoint rate limited (HTTP 429); retry on the next scheduled check"
		}
		return n.fail(msg), errors.New(msg)
	}
	if len(resp.Body) == 0 {
		msg := "neuralwatt: empty response body"
		return n.fail(msg), errors.New(msg)
	}
	checkedAt := n.now()
	observedAt, windows, unavailable, partial, err := parseNeuralwattQuota(resp.Body, checkedAt)
	if err != nil {
		msg := SanitizeError(err)
		return n.fail(msg), errors.New(msg)
	}
	status := SourceFresh
	if partial {
		status = SourcePartial
	}
	availability := determineAvailability(windows)
	if unavailable {
		availability = QuotaUnavailable
	}
	return QuotaSnapshot{
		MappingID:    n.mappingID,
		CheckedAt:    observedAt,
		Windows:      windows,
		Availability: availability,
		Status:       status,
	}, nil
}

func (n *NeuralwattSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{MappingID: n.mappingID, Availability: QuotaUnknown, Status: SourceFailed, Error: reason}
}

type neuralwattQuotaResponse struct {
	SnapshotAt   string                  `json:"snapshot_at"`
	Balance      *neuralwattBalance      `json:"balance"`
	Subscription *neuralwattSubscription `json:"subscription"`
	Key          *neuralwattKey          `json:"key"`
}

type neuralwattBalance struct {
	Remaining *float64 `json:"credits_remaining_usd"`
	Total     *float64 `json:"total_credits_usd"`
	Used      *float64 `json:"credits_used_usd"`
}

type neuralwattKey struct {
	Allowance *neuralwattAllowance `json:"allowance"`
}

type neuralwattAllowance struct {
	Limit     *float64 `json:"limit_usd"`
	SpentUSD  *float64 `json:"spent_usd"`
	Spent     *float64 `json:"spent"`
	Remaining *float64 `json:"remaining_usd"`
	Blocked   *bool    `json:"blocked"`
}

type neuralwattSubscription struct {
	KWHIncluded  *float64 `json:"kwh_included"`
	KWHUsed      *float64 `json:"kwh_used"`
	KWHRemaining *float64 `json:"kwh_remaining"`
	PeriodEnd    string   `json:"current_period_end"`
	InOverage    *bool    `json:"in_overage"`
}

func parseNeuralwattQuota(body []byte, checkedAt time.Time) (time.Time, []QuotaWindow, bool, bool, error) {
	var response neuralwattQuotaResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return time.Time{}, nil, false, false, errors.New("neuralwatt: invalid response body (could not decode JSON)")
	}
	if response.SnapshotAt == "" {
		return time.Time{}, nil, false, false, errors.New("neuralwatt: response is missing snapshot_at")
	}
	observedAt, err := time.Parse(time.RFC3339, response.SnapshotAt)
	if err != nil || observedAt.After(checkedAt.Add(5*time.Minute)) {
		return time.Time{}, nil, false, false, errors.New("neuralwatt: response has an invalid snapshot_at")
	}
	if response.Key != nil && response.Key.Allowance != nil {
		windows, unavailable, partial, err := neuralwattAllowanceWindow(*response.Key.Allowance)
		return observedAt, windows, unavailable, partial, err
	}
	if response.Subscription != nil {
		windows, unavailable, partial, err := neuralwattSubscriptionWindow(*response.Subscription)
		if err != nil {
			return time.Time{}, nil, false, false, err
		}
		if len(windows) == 0 && !unavailable {
			return time.Time{}, nil, false, false, errors.New("neuralwatt: subscription is missing a usable allowance")
		}
		return observedAt, windows, unavailable, partial, nil
	}
	if response.Balance == nil {
		return time.Time{}, nil, false, false, errors.New("neuralwatt: response has no usable allowance or balance")
	}
	window, unavailable, partial, err := neuralwattBalanceWindow(*response.Balance)
	if err != nil {
		return time.Time{}, nil, false, false, err
	}
	if window == nil && !unavailable {
		return time.Time{}, nil, false, false, errors.New("neuralwatt: response has no usable balance limit")
	}
	if window == nil {
		return observedAt, nil, unavailable, partial, nil
	}
	return observedAt, []QuotaWindow{*window}, unavailable, partial, nil
}

func neuralwattAllowanceWindow(a neuralwattAllowance) ([]QuotaWindow, bool, bool, error) {
	if a.Blocked == nil {
		return nil, false, false, errors.New("neuralwatt: key allowance is missing blocked state")
	}
	if *a.Blocked {
		return nil, true, false, nil
	}
	if a.Limit == nil || a.Remaining == nil || !finiteNonNegative(*a.Limit) || !finiteNonNegative(*a.Remaining) || *a.Limit <= 0 {
		return nil, false, false, errors.New("neuralwatt: key allowance is missing a valid limit or remaining value")
	}
	if *a.Remaining > *a.Limit {
		return nil, false, false, errors.New("neuralwatt: key allowance remaining exceeds limit")
	}
	used := *a.Limit - *a.Remaining
	derivedUsed := used
	if a.SpentUSD != nil {
		used = *a.SpentUSD
	} else if a.Spent != nil {
		used = *a.Spent
	}
	if !finiteNonNegative(used) || used > *a.Limit || !neuralwattValuesAgree(used, derivedUsed, *a.Limit) {
		return nil, false, false, errors.New("neuralwatt: key allowance spent value is inconsistent with its limit and remaining value")
	}
	return []QuotaWindow{neuralwattWindow("key_allowance", used, *a.Limit)}, *a.Remaining <= 0, false, nil
}

func neuralwattSubscriptionWindow(s neuralwattSubscription) ([]QuotaWindow, bool, bool, error) {
	if s.InOverage == nil {
		return nil, false, false, errors.New("neuralwatt: subscription is missing overage state")
	}
	if *s.InOverage {
		return nil, true, false, nil
	}
	if s.KWHIncluded == nil || s.KWHRemaining == nil || !finiteNonNegative(*s.KWHIncluded) || !finiteNonNegative(*s.KWHRemaining) || *s.KWHIncluded <= 0 {
		return nil, false, false, nil
	}
	if *s.KWHRemaining > *s.KWHIncluded {
		return nil, false, false, errors.New("neuralwatt: subscription remaining exceeds included allowance")
	}
	used := *s.KWHIncluded - *s.KWHRemaining
	derivedUsed := used
	if s.KWHUsed != nil {
		used = *s.KWHUsed
	}
	if !finiteNonNegative(used) || used > *s.KWHIncluded || !neuralwattValuesAgree(used, derivedUsed, *s.KWHIncluded) {
		return nil, false, false, errors.New("neuralwatt: subscription used value is inconsistent with its allowance")
	}
	window := neuralwattWindow("subscription_kwh", used, *s.KWHIncluded)
	partial := false
	if s.PeriodEnd != "" {
		reset, err := time.Parse(time.RFC3339, s.PeriodEnd)
		if err != nil {
			partial = true
		} else {
			window.ResetAt = &reset
		}
	}
	return []QuotaWindow{window}, *s.KWHRemaining <= 0, partial, nil
}

func neuralwattBalanceWindow(b neuralwattBalance) (*QuotaWindow, bool, bool, error) {
	if b.Remaining == nil || !finiteNonNegative(*b.Remaining) {
		return nil, false, false, errors.New("neuralwatt: balance is missing a valid credits_remaining_usd value")
	}
	if *b.Remaining == 0 && (b.Total == nil || *b.Total <= 0) {
		return nil, true, false, nil
	}
	if b.Total == nil || !finiteNonNegative(*b.Total) || *b.Total <= 0 || *b.Remaining > *b.Total {
		return nil, false, false, errors.New("neuralwatt: balance is missing a valid total credits limit")
	}
	used := *b.Total - *b.Remaining
	derivedUsed := used
	if b.Used != nil {
		used = *b.Used
	}
	if !finiteNonNegative(used) || used > *b.Total || !neuralwattValuesAgree(used, derivedUsed, *b.Total) {
		return nil, false, false, errors.New("neuralwatt: balance used value is inconsistent with its limit and remaining value")
	}
	returnWindow := neuralwattWindow("balance_usd", used, *b.Total)
	return &returnWindow, *b.Remaining <= 0, false, nil
}

func neuralwattWindow(name string, used, limit float64) QuotaWindow {
	percent := clampRange(used/limit*100, 0, 100)
	return QuotaWindow{Name: name, Used: &used, Limit: &limit, UsagePercent: &percent}
}

func neuralwattValuesAgree(a, b, limit float64) bool {
	if a == b {
		return true
	}
	tolerance := math.Max(0.01, math.Abs(limit)*1e-6)
	return math.Abs(a-b) <= tolerance
}

func finite(v float64) bool            { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func finiteNonNegative(v float64) bool { return finite(v) && v >= 0 }

var _ QuotaSource = (*NeuralwattSource)(nil)
