// zai.go implements the z.ai (Zai / BigModel) quota adapter — a second provider
// adapter mirroring the Codex adapter's contract-gated structure. It polls the
// z.ai quota-limit API behind the evidence gate using a bounded HTTPS client and
// a transient Bearer API key, parses the recorded response contract leniently
// (per-element, downgrading to SourcePartial rather than hard-failing on a
// single malformed limit), and returns a sanitized QuotaSnapshot.
//
// The API key is transient: read from the credential resolver, attached to
// exactly one request, and discarded. No API keys, auth headers, account names,
// or raw response bodies are ever persisted, logged, or included in a snapshot
// or error. All errors are sanitized via SanitizeError.
//
// There is no token refresh and no retry on 429.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// zai region base URLs and the shared quota-limit path. Both regions expose the
// identical relative path under their respective hosts.
const (
	zaiGlobalBase      = "https://api.z.ai"
	zaiBigmodelCNBase  = "https://open.bigmodel.cn"
	zaiQuotaPath       = "/api/monitor/usage/quota/limit"
	zaiGlobalEndpoint  = zaiGlobalBase + zaiQuotaPath
	zaiBigmodelCNEndpt = zaiBigmodelCNBase + zaiQuotaPath

	// zaiProviderName is the evidence-registry key and adapter identifier.
	zaiProviderName = "zai"
	// zaiAPIKeyEnv is the canonical env var the transient Bearer key resolves from.
	zaiAPIKeyEnv = "ZAI_API_KEY"
)

// ZaiSource polls the z.ai (BigModel) quota API behind the evidence gate. It
// satisfies the QuotaSource interface. The HTTP transport is injectable for
// tests; the API key is resolved transiently and never retained.
//
// Region selects the API host: "" or "global" (default) → api.z.ai;
// "bigmodel-cn" → open.bigmodel.cn. Personal scope is used (no team headers).
//
// The mapping id is held in the unexported mappingID field because the
// QuotaSource.MappingID method name would otherwise shadow an exported field of
// the same name.
type ZaiSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials CredentialResolver
	Evidence    *EvidenceRegistry
	Region      string // "global" (default) or "bigmodel-cn"
	Now         func() time.Time
}

// ZaiEvidence returns the sanitized contract evidence for the z.ai adapter.
// Register it in an EvidenceRegistry so the gate recognizes "zai" as fresh.
// The dates are release-owned and do not renew on construction. ReviewBy is
// three months after the recording date (quarterly review) per the evidence
// policy, reflecting the z.ai schema's higher drift rate.
func ZaiEvidence(_ time.Time) Evidence {
	return Evidence{
		Provider:    zaiProviderName,
		Endpoint:    zaiGlobalEndpoint,
		Method:      http.MethodGet,
		AuthType:    "api-key",
		SchemaNote:  "envelope {code,success,data:{limits[]}} with percentage always present, optional raw counts, millis resets",
		FixturePath: "contract/testdata/quota/zai/pro.json",
		RecordedAt:  evidenceRecordedAt(),
		ReviewBy:    evidenceRecordedAt().AddDate(0, 3, 0), // quarterly review per evidence policy
	}
}

// NewZaiSource constructs a ZaiSource consulting the supplied release-owned
// evidence registry. Construction never registers or refreshes evidence. If reg
// is nil, an empty registry is created and the source is unsupported.
func NewZaiSource(mappingID string, client *BoundedClient, creds CredentialResolver, region string, reg *EvidenceRegistry, now time.Time) *ZaiSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	return &ZaiSource{
		mappingID:   mappingID,
		Client:      client,
		Credentials: creds,
		Evidence:    reg,
		Region:      region,
		Now:         func() time.Time { return now },
	}
}

// MappingID returns the provider mapping this source serves.
func (z *ZaiSource) MappingID() string { return z.mappingID }

// now returns the current observation time, defaulting to time.Now when unset.
func (z *ZaiSource) now() time.Time {
	if z.Now != nil {
		return z.Now()
	}
	return time.Now()
}

// endpoint returns the region-specific quota-limit URL.
func (z *ZaiSource) endpoint() string {
	if z.Region == "bigmodel-cn" {
		return zaiBigmodelCNEndpt
	}
	return zaiGlobalEndpoint
}

// Status reports whether this source is supported. It consults the evidence
// registry and is fail-closed: absent, expired, or incomplete evidence yields an
// unsupported status with a sanitized remediation reason.
func (z *ZaiSource) Status() SupportStatus {
	return SupportFromEvidence(z.evidenceStatus())
}

func (z *ZaiSource) evidenceStatus() EvidenceStatus {
	if z.Evidence == nil {
		return EvidenceStatus{
			State:  EvidenceAbsent,
			Reason: "provider " + zaiProviderName + " has no recorded contract evidence; record evidence before enabling",
		}
	}
	return z.Evidence.Status(zaiProviderName, z.now())
}

// Fetch retrieves the current z.ai quota snapshot. It fails closed when the
// evidence gate is unsupported (no request is made), resolves the Bearer API key
// transiently, performs one bounded HTTPS request, and parses the response
// leniently. Errors never carry API keys, auth headers, or raw bodies.
func (z *ZaiSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := z.Status()
	if !st.Supported {
		return z.fail(st.Reason), errors.New(st.Reason)
	}

	apiKey, err := z.resolveCredentials()
	if err != nil {
		msg := SanitizeError(err)
		return z.fail(msg), errors.New(msg)
	}

	req, err := z.buildRequest(ctx, apiKey)
	if err != nil {
		msg := SanitizeError(err)
		return z.fail(msg), errors.New(msg)
	}

	resp, err := z.Client.Do(req)
	if err != nil {
		msg := SanitizeError(err)
		return z.fail(msg), errors.New(msg)
	}

	// Any non-200 HTTP status is a server error (status code only, never body).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("zai: server error (HTTP %d)", resp.StatusCode)
		return z.fail(msg), errors.New(msg)
	}

	// An empty 200 body usually means a wrong region or token.
	if len(resp.Body) == 0 {
		msg := "zai: empty response body; check z.ai API region and API token"
		return z.fail(msg), errors.New(msg)
	}

	windows, partial, perr := parseZaiQuota(resp.Body)
	if perr != nil {
		msg := SanitizeError(perr)
		return z.fail(msg), errors.New(msg)
	}

	status := SourceFresh
	if partial || len(windows) == 0 {
		status = SourcePartial
	}
	return QuotaSnapshot{
		MappingID:    z.mappingID,
		CheckedAt:    z.now(),
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
	}, nil
}

// fail returns a sanitized failed snapshot for this source's mapping.
func (z *ZaiSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{
		MappingID:    z.mappingID,
		Availability: QuotaUnknown,
		Status:       SourceFailed,
		Error:        reason,
	}
}

// --- Credential resolution (transient) ------------------------------------

// resolveCredentials reads the Bearer API key transiently via the credential
// resolver from ZAI_API_KEY. The key is used for the immediate request only.
// Errors are generic and never include the key value.
func (z *ZaiSource) resolveCredentials() (string, error) {
	raw, err := z.Credentials.Resolve(CredentialRef{Kind: CredentialEnv, Locator: zaiAPIKeyEnv})
	if err != nil {
		return "", errors.New("zai: could not resolve ZAI_API_KEY")
	}
	key := cleanZaiKey(raw)
	if key == "" {
		return "", errors.New("zai: ZAI_API_KEY is empty")
	}
	return key, nil
}

// cleanZaiKey trims whitespace and strips a single pair of surrounding quotes
// from the resolved key, matching the contract's value-cleaning rule.
func cleanZaiKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}

// --- HTTP request ---------------------------------------------------------

// buildRequest builds the GET quota-limit request with the required Bearer
// header. The API key is attached here and discarded after the request
// completes. Personal scope: no query params, no team headers.
func (z *ZaiSource) buildRequest(ctx context.Context, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.endpoint(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")
	return req, nil
}

// --- Response parsing (lenient, per-element) ------------------------------

// zaiQuotaEnvelope is the top-level response envelope. Data is kept as raw JSON
// so it can be decoded independently of the envelope.
type zaiQuotaEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// zaiQuotaData is the data object. Only the limits array is modeled; plan-name
// keys (planName/plan/planType/plan_type/packageName/level) are recognized by the
// contract but carry no destination in QuotaSnapshot, so they are left for JSON
// decoding to ignore.
type zaiQuotaData struct {
	Limits []json.RawMessage `json:"limits"`
}

// zaiLimitRaw is one entry of data.limits. Raw counts (usage/currentValue/
// remaining) are optional pointers; percentage is the always-present server
// value (percent USED, 0..100); nextResetTime is epoch milliseconds.
// z.ai currently returns CREDIT_LIMIT for live accounts, while older contract
// responses used TOKENS_LIMIT and TIME_LIMIT.
type zaiLimitRaw struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        float64  `json:"number"`
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime *int64   `json:"nextResetTime"`
}

// zaiDecodedLimit holds a decoded limit awaiting slotting/naming.
type zaiDecodedLimit struct {
	limitType string // "TOKENS_LIMIT", "CREDIT_LIMIT", or "TIME_LIMIT"
	unit      int
	number    float64
	window    QuotaWindow // Name unset; filled by the slotter
}

// parseZaiQuota parses the z.ai quota body into windows. The body must be valid
// JSON and pass the success gate (success && code==200). Individual limits are
// decoded independently: a malformed limit is skipped and partial is set true
// rather than failing the whole response. Unknown limit types and unknown fields
// are ignored. An empty limits array is valid (returns no windows, not an error).
func parseZaiQuota(body []byte) (windows []QuotaWindow, partial bool, err error) {
	var env zaiQuotaEnvelope
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		return nil, false, errors.New("zai: invalid response body (could not decode JSON)")
	}

	// Success gate: success must be true and code must be 200. The envelope msg
	// is server-controlled text and is never echoed into an error (it can reflect
	// or echo secrets); only the sanitized code + a fixed diagnostic are used.
	if !env.Success || env.Code != 200 {
		if env.Code == 1001 {
			return nil, false, errors.New("zai: auth failure (code 1001); check API token")
		}
		return nil, false, fmt.Errorf("zai: api error (code %d)", env.Code)
	}

	if len(env.Data) == 0 {
		return nil, false, errors.New("zai: missing data field in response")
	}
	var data zaiQuotaData
	if derr := json.Unmarshal(env.Data, &data); derr != nil {
		return nil, false, errors.New("zai: invalid data field (could not decode JSON)")
	}

	// Decode each limit independently and bucket by type.
	var tokenLimits, timeLimits []zaiDecodedLimit
	for _, raw := range data.Limits {
		if len(raw) == 0 {
			continue
		}
		dl, malformed := decodeZaiLimit(raw)
		if malformed {
			partial = true
			continue
		}
		switch dl.limitType {
		case "TOKENS_LIMIT", "CREDIT_LIMIT":
			// CREDIT_LIMIT is the live z.ai response type. Its unit/number
			// fields have the same window semantics as TOKENS_LIMIT.
			tokenLimits = append(tokenLimits, dl)
		case "TIME_LIMIT":
			timeLimits = append(timeLimits, dl)
		default:
			// Unknown limit type: ignored, not malformed.
		}
	}

	// Slot token limits (TOKENS_LIMIT/CREDIT_LIMIT) by ascending window duration: shortest = session,
	// longest = primary; a single token limit is primary; intermediates are
	// tertiary.
	sort.SliceStable(tokenLimits, func(i, j int) bool {
		return zaiWindowMinutes(tokenLimits[i].unit, tokenLimits[i].number) <
			zaiWindowMinutes(tokenLimits[j].unit, tokenLimits[j].number)
	})
	for i := range tokenLimits {
		w := tokenLimits[i].window
		switch {
		case len(tokenLimits) == 1:
			w.Name = "primary"
		case i == 0:
			w.Name = "session"
		case i == len(tokenLimits)-1:
			w.Name = "primary"
		default:
			w.Name = "tertiary"
		}
		windows = append(windows, w)
	}

	// Slot TIME_LIMIT: the MCP monthly marker (unit=5 minutes, number=1) is named
	// "monthly"; any other is named "time".
	for _, dl := range timeLimits {
		w := dl.window
		if dl.unit == 5 && dl.number == 1 {
			w.Name = "monthly"
		} else {
			w.Name = "time"
		}
		windows = append(windows, w)
	}

	return windows, partial, nil
}

// decodeZaiLimit decodes one limit entry lossily. It returns the decoded limit
// and whether the entry was present but malformed (undecodable as an object or
// carrying a bad numeric shape). Raw counts derive used/limit when present;
// otherwise the server percentage is used as usage_percent. A reset time is
// converted from epoch milliseconds.
func decodeZaiLimit(raw json.RawMessage) (zaiDecodedLimit, bool) {
	var lr zaiLimitRaw
	if uerr := json.Unmarshal(raw, &lr); uerr != nil {
		return zaiDecodedLimit{}, true // malformed JSON object
	}
	dl := zaiDecodedLimit{
		limitType: lr.Type,
		unit:      lr.Unit,
		number:    lr.Number,
	}

	// Limit = usage (total capacity) when present.
	if lr.Usage != nil {
		limit := *lr.Usage
		dl.window.Limit = &limit
	}
	// Used = currentValue when present, else usage − remaining.
	if lr.CurrentValue != nil {
		used := *lr.CurrentValue
		dl.window.Used = &used
	} else if lr.Usage != nil && lr.Remaining != nil {
		used := *lr.Usage - *lr.Remaining
		dl.window.Used = &used
	}

	// UsagePercent: derive from counts when both used and limit are known and the
	// limit is positive; otherwise fall back to the server percentage. Never
	// invent 0%.
	if dl.window.Used != nil && dl.window.Limit != nil && *dl.window.Limit > 0 {
		pct := clampRange(*dl.window.Used / *dl.window.Limit * 100, 0, 100)
		dl.window.UsagePercent = &pct
	} else if lr.Percentage != nil {
		pct := *lr.Percentage
		dl.window.UsagePercent = &pct
	}

	// Reset time from epoch MILLISECONDS (the #1 porting bug).
	if lr.NextResetTime != nil {
		t := millisToTime(*lr.NextResetTime)
		dl.window.ResetAt = &t
	}
	if mins := zaiWindowMinutes(dl.unit, dl.number); mins > 0 {
		d := time.Duration(mins * float64(time.Minute))
		dl.window.Period = &d
	}

	return dl, false
}

// zaiWindowMinutes converts a z.ai (unit, number) window to a duration in
// minutes for sorting. unit: 1=days, 3=hours, 5=minutes, 6=weeks; 0/2/4/other →
// unknown (0). Two unknown durations compare equal under a stable sort.
func zaiWindowMinutes(unit int, number float64) float64 {
	switch unit {
	case 1:
		return number * 1440 // days
	case 3:
		return number * 60 // hours
	case 5:
		return number // minutes
	case 6:
		return number * 10080 // weeks
	default:
		return 0 // unknown
	}
}

// millisToTime converts an epoch-millisecond value to a UTC time.
func millisToTime(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

// clampRange bounds v to the inclusive [lo, hi] range.
func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Compile-time assertion that ZaiSource satisfies the QuotaSource interface.
var _ QuotaSource = (*ZaiSource)(nil)
