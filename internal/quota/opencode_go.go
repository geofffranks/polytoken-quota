// opencode_go.go implements the OpenCode Go quota adapter.
//
// OpenCode's Zen Go gateway exposes a read-only GET /v1/usage endpoint. The
// payload carries up to three named usage windows (rolling, weekly, monthly),
// each a {status, percent, resetsAt} object. `status` is advisory ("ok" or
// "rate-limited"); availability derives from `percent`, which is percent of
// the per-model cap USED — never dollars — so this adapter needs no user
// budget parameter. The percent-based ceiling ladder (5-hour rolling = 20%,
// weekly = 50%, monthly = 100% of a monthly per-model cap) fixes the window
// lengths below; prefer provider-reported window lengths if the payload ever
// exposes them.
//
// Credentials are transient: OPENCODE_API_KEY is resolved for the immediate
// request, attached as a Bearer header, and discarded. No key, account
// identity, raw response, or provider-controlled message is persisted or
// returned.
//
// The endpoint contract is derived from the provider's first-party
// open-source console code and is not officially documented: it is reviewed
// quarterly per the evidence policy and must be re-verified whenever the
// console implementation drifts.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// Credential-free probe verified 2026: GET on this endpoint returned HTTP
	// 401 with an AuthError envelope (not a 404), so the path exists.
	opencodeGoUsageEndpoint = "https://opencode.ai/zen/go/v1/usage"
	opencodeGoProviderName  = "opencode-go"
	opencodeGoAPIKeyEnv     = "OPENCODE_API_KEY"
)

// opencodeGoWindowOrder is the fixed, documented decode priority order. It is
// load-bearing: NextQuotaResetAt breaks period ties by slice order, so the
// shortest scoped window must decode first.
var opencodeGoWindowOrder = [3]string{"rolling", "weekly", "monthly"}

// Window length constants. The provider documents a limit ladder over a
// monthly per-model cap (5-hour rolling = 20%, weekly = 50%, monthly = 100%)
// but does not expose window lengths in the payload, so the lengths are
// fixed here: 5-hour rolling, 7-day weekly, and a 30-day monthly cycle
// (following the neuralwattQuotaCycle 30-day-cycle precedent). Prefer
// provider-reported window lengths if the payload ever exposes them.
const (
	opencodeGoRollingPeriod = 5 * time.Hour
	opencodeGoWeeklyPeriod  = 7 * 24 * time.Hour
	opencodeGoMonthlyPeriod = 30 * 24 * time.Hour
)

var opencodeGoWindowPeriods = map[string]time.Duration{
	"rolling": opencodeGoRollingPeriod,
	"weekly":  opencodeGoWeeklyPeriod,
	"monthly": opencodeGoMonthlyPeriod,
}

// OpenCodeGoSource polls OpenCode Go's usage endpoint behind the evidence
// gate.
type OpenCodeGoSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials CredentialResolver
	Evidence    *EvidenceRegistry
	Now         func() time.Time
}

// OpenCodeGoEvidence returns the reviewed, sanitized OpenCode Go contract
// record. The dates are release-owned and do not renew on construction.
func OpenCodeGoEvidence(_ time.Time) Evidence {
	return Evidence{
		Provider:    opencodeGoProviderName,
		Endpoint:    opencodeGoUsageEndpoint,
		Method:      http.MethodGet,
		AuthType:    "bearer-api-key",
		SchemaNote:  "three windows: rolling/weekly/monthly, each {status, percent, resetsAt}; percent is percent USED (never dollars); windows decode in fixed order rolling, weekly, monthly; errors: 401 AuthError envelope, 403 EntitlementError envelope with a valid key that has no OpenCode Go subscription; contract derived from the provider's first-party open-source console code and not officially documented — re-verify at the quarterly evidence review",
		FixturePath: "contract/testdata/quota/opencode-go/usage.json",
		RecordedAt:  evidenceRecordedAt(),
		ReviewBy:    evidenceRecordedAt().AddDate(0, 3, 0), // quarterly review; contract is not officially documented
	}
}

// NewOpenCodeGoSource constructs a source using the supplied evidence
// registry. Construction never registers or refreshes evidence.
func NewOpenCodeGoSource(mappingID string, client *BoundedClient, creds CredentialResolver, reg *EvidenceRegistry, now time.Time) *OpenCodeGoSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	return &OpenCodeGoSource{
		mappingID:   mappingID,
		Client:      client,
		Credentials: creds,
		Evidence:    reg,
		Now:         func() time.Time { return now },
	}
}

func (o *OpenCodeGoSource) MappingID() string { return o.mappingID }

// cleanOpenCodeGoKey trims whitespace and strips one layer of matching
// surrounding quotes (the same leniency cleanNeuralwattKey applies), without
// ever persisting or logging the value.
func cleanOpenCodeGoKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}

func (o *OpenCodeGoSource) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OpenCodeGoSource) Status() SupportStatus {
	return SupportFromEvidence(o.evidenceStatus())
}

func (o *OpenCodeGoSource) evidenceStatus() EvidenceStatus {
	if o.Evidence == nil {
		return EvidenceStatus{
			State:  EvidenceAbsent,
			Reason: "provider " + opencodeGoProviderName + " has no recorded contract evidence; record evidence before enabling",
		}
	}
	return o.Evidence.Status(opencodeGoProviderName, o.now())
}

func (o *OpenCodeGoSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := o.Status()
	if !st.Supported {
		return o.fail(st.Reason), errors.New(st.Reason)
	}
	if o.Client == nil || o.Credentials == nil {
		msg := "opencode-go: adapter is not configured"
		return o.fail(msg), errors.New(msg)
	}
	key, err := o.Credentials.Resolve(CredentialRef{Kind: CredentialEnv, Locator: opencodeGoAPIKeyEnv})
	key = cleanOpenCodeGoKey(key)
	if err != nil || key == "" {
		msg := "opencode-go: could not resolve OPENCODE_API_KEY"
		return o.fail(msg), errors.New(msg)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeGoUsageEndpoint, nil)
	if err != nil {
		msg := SanitizeError(err)
		return o.fail(msg), errors.New(msg)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")
	resp, err := o.Client.Do(req)
	if err != nil {
		msg := SanitizeError(err)
		return o.fail(msg), errors.New(msg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("opencode-go: server error (HTTP %d)", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg = "opencode-go: authentication failed; check OPENCODE_API_KEY"
		case http.StatusForbidden:
			// Distinct from an auth failure: the EntitlementError envelope
			// means the key authenticated but has no OpenCode Go subscription.
			msg = "opencode-go: request rejected (HTTP 403); the key is valid but has no OpenCode Go subscription"
		case http.StatusTooManyRequests:
			msg = fmt.Sprintf("opencode-go: quota endpoint rate limited (HTTP 429)%s; retry on the next scheduled check", retryAfterSuffix(resp.Headers))
		}
		return o.fail(msg), errors.New(msg)
	}
	if len(resp.Body) == 0 {
		msg := "opencode-go: empty response body"
		return o.fail(msg), errors.New(msg)
	}
	checkedAt := o.now()
	windows, partial, err := parseOpenCodeGoUsage(resp.Body)
	if err != nil {
		msg := SanitizeError(err)
		return o.fail(msg), errors.New(msg)
	}
	status := SourceFresh
	if partial {
		status = SourcePartial
	}
	return QuotaSnapshot{
		MappingID:    o.mappingID,
		CheckedAt:    checkedAt,
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
	}, nil
}

func (o *OpenCodeGoSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{MappingID: o.mappingID, Availability: QuotaUnknown, Status: SourceFailed, Error: reason}
}

// opencodeGoUsageWindow mirrors one usage window object. Pointers distinguish
// "key absent" from "key present but unusable"; both unusable cases fail the
// window closed.
type opencodeGoUsageWindow struct {
	Status   *string  `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resetsAt"`
}

// parseOpenCodeGoUsage decodes the OpenCode Go usage payload. The payload has
// no snapshot timestamp; the caller stamps CheckedAt from the local clock.
//
// Windows decode in the fixed opencodeGoWindowOrder. A window key absent from
// the payload is simply skipped without syntax damage, but a snapshot
// covering fewer than the three known windows is still only partial.
// A syntactically malformed document (including invalid JSON inside any
// window value, e.g. a trailing comma or a NaN literal) fails the whole
// payload at the envelope decode. A window that is JSON-valid but semantically
// unusable (wrong type, missing/unrecognized status, missing/non-numeric/
// non-finite percent) fails that window closed and marks the snapshot partial
// — a present-but-unusable signal must never fall back to a weaker window.
// A window missing (or carrying an unparseable) resetsAt still decodes,
// without ResetAt, and also marks the snapshot partial. If no window decodes
// at all (including an absent/empty usage object), the payload has no usable
// signal and the caller hard-fails.
func parseOpenCodeGoUsage(body []byte) ([]QuotaWindow, bool, error) {
	var envelope struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, errors.New("opencode-go: invalid response body (could not decode JSON)")
	}
	windows := make([]QuotaWindow, 0, len(opencodeGoWindowOrder))
	partial := false
	for _, name := range opencodeGoWindowOrder {
		raw, ok := envelope.Usage[name]
		if !ok {
			continue // window absent from the payload; not an error by itself
		}
		var decoded opencodeGoUsageWindow
		if err := json.Unmarshal(raw, &decoded); err != nil {
			partial = true // window present but semantically malformed (JSON-valid) fails that window closed
			continue
		}
		if decoded.Status == nil || (*decoded.Status != "ok" && *decoded.Status != "rate-limited") {
			partial = true // status is advisory, but missing/unrecognized still fails the window closed
			continue
		}
		if decoded.Percent == nil || !finite(*decoded.Percent) {
			partial = true // a present-but-unusable percent must not fall back to a weaker window
			continue
		}
		usagePercent := clampRange(*decoded.Percent, 0, 100)
		window := QuotaWindow{Name: name, UsagePercent: &usagePercent}
		period := opencodeGoWindowPeriods[name]
		window.Period = &period // every decoded window carries its Period
		if decoded.ResetsAt == nil {
			partial = true // decodes without ResetAt; the reset is unknown
		} else if reset, err := time.Parse(time.RFC3339, *decoded.ResetsAt); err == nil {
			window.ResetAt = &reset
		} else {
			partial = true // unparseable reset still decodes the window, without ResetAt
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, false, errors.New("opencode-go: response has no usable usage window")
	}
	if len(windows) < len(opencodeGoWindowOrder) {
		partial = true // only some of the known windows decoded
	}
	return windows, partial, nil
}

var _ QuotaSource = (*OpenCodeGoSource)(nil)
