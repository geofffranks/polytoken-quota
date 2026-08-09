// codex.go implements the Codex (ChatGPT) quota adapter — the codebase's first
// real provider adapter. It polls the ChatGPT usage API behind the evidence
// gate using a bounded HTTPS client and transient OAuth/API-key credentials,
// parses the recorded response contract leniently (per-element, downgrading to
// SourcePartial rather than hard-failing on a single malformed window), and
// returns a sanitized QuotaSnapshot.
//
// Credentials are transient: the bearer token is read from auth.json, attached
// to exactly one request, and discarded. No credentials, auth headers, account
// IDs, or raw response bodies are ever persisted, logged, or included in a
// snapshot or error. All errors are sanitized via SanitizeError.
//
// This adapter performs no OAuth token refresh: a 401/403 is reported as an
// auth-failure diagnostic for the user to re-authenticate.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// codexUsageEndpoint is the primary Codex (ChatGPT) usage endpoint recorded in
// the contract evidence. It is HTTPS and takes no query parameters or body.
const (
	codexUsageEndpoint        = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

	// CodexUsageContract and CodexResetCreditsContract are independent evidence
	// contract ids under the codex provider key.
	CodexUsageContract        = "usage"
	CodexResetCreditsContract = "reset_credits"
)

// codexProviderName is the evidence-registry key and adapter identifier.
const codexProviderName = "codex"

// CodexSource polls the Codex (ChatGPT) usage API behind the evidence gate. It
// satisfies the QuotaSource interface. The HTTP transport is injectable for
// tests; credentials are resolved transiently and never retained.
//
// The mapping id is held in the unexported mappingID field because the
// QuotaSource.MappingID method name would otherwise shadow an exported field of
// the same name.
type CodexSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials CredentialResolver
	Evidence    *EvidenceRegistry
	CodexHome   string // CODEX_HOME path; empty → ~/.codex
	Now         func() time.Time
}

// CodexEvidence is the compatibility name for mandatory usage evidence.
func CodexEvidence(now time.Time) Evidence { return CodexUsageEvidence(now) }

// CodexUsageEvidence returns the mandatory /wham/usage contract evidence.
func CodexUsageEvidence(now time.Time) Evidence {
	return Evidence{
		Provider:    codexProviderName,
		ContractID:  CodexUsageContract,
		Endpoint:    codexUsageEndpoint,
		Method:      http.MethodGet,
		AuthType:    "oauth-bearer",
		SchemaNote:  "rate_limit windows + individual_limit spend control + ordinary credits",
		FixturePath: "contract/testdata/quota/codex/pro.json",
		RecordedAt:  now,
		ReviewBy:    now.AddDate(1, 0, 0),
	}
}

// CodexResetCreditsEvidence returns the optional account-scoped inventory
// contract evidence. Its volatile schema is reviewed every three months.
func CodexResetCreditsEvidence(now time.Time) Evidence {
	return Evidence{
		Provider:    codexProviderName,
		ContractID:  CodexResetCreditsContract,
		Endpoint:    codexResetCreditsEndpoint,
		Method:      http.MethodGet,
		AuthType:    "oauth-bearer-account",
		SchemaNote:  "non-negative available_count + credits status and optional ISO-8601 expires_at",
		FixturePath: "contract/testdata/quota/codex/reset_credits.json",
		RecordedAt:  now,
		ReviewBy:    now.AddDate(0, 3, 0),
	}
}

// NewCodexSource constructs a CodexSource consulting the supplied release-owned
// evidence registry. Construction never registers or refreshes evidence. If reg
// is nil, an empty registry is created and the source is unsupported.
func NewCodexSource(mappingID string, client *BoundedClient, creds CredentialResolver, codexHome string, reg *EvidenceRegistry, now time.Time) *CodexSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	return &CodexSource{
		mappingID:   mappingID,
		Client:      client,
		Credentials: creds,
		Evidence:    reg,
		CodexHome:   codexHome,
		Now:         func() time.Time { return now },
	}
}

// MappingID returns the provider mapping this source serves.
func (c *CodexSource) MappingID() string { return c.mappingID }

// now returns the current observation time, defaulting to time.Now when unset.
func (c *CodexSource) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Status reports whether this source is supported. It consults the evidence
// registry and is fail-closed: absent, expired, or incomplete evidence yields an
// unsupported status with a sanitized remediation reason.
func (c *CodexSource) Status() SupportStatus {
	return SupportFromEvidence(c.evidenceStatus())
}

func (c *CodexSource) evidenceStatus() EvidenceStatus {
	if c.Evidence == nil {
		return EvidenceStatus{
			State:  EvidenceAbsent,
			Reason: "provider " + codexProviderName + " has no recorded contract evidence; record evidence before enabling",
		}
	}
	return c.Evidence.StatusContract(codexProviderName, CodexUsageContract, c.now())
}

// Fetch retrieves the current Codex quota snapshot. It fails closed when the
// evidence gate is unsupported (no request is made), resolves the bearer token
// transiently, performs one bounded HTTPS request, and parses the response
// leniently. Errors never carry credentials, account IDs, or raw bodies.
func (c *CodexSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := c.Status()
	if !st.Supported {
		return c.fail(st.Reason), errors.New(st.Reason)
	}

	token, accountID, err := c.resolveCredentials()
	if err != nil {
		msg := SanitizeError(err)
		return c.fail(msg), errors.New(msg)
	}

	req, err := c.buildRequest(ctx, token, accountID)
	if err != nil {
		msg := SanitizeError(err)
		return c.fail(msg), errors.New(msg)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		msg := SanitizeError(err)
		return c.fail(msg), errors.New(msg)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg := fmt.Sprintf("codex: auth failure (HTTP %d); token expired or invalid, re-authenticate needed", resp.StatusCode)
		return c.fail(msg), errors.New(msg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("codex: server error (HTTP %d)", resp.StatusCode)
		return c.fail(msg), errors.New(msg)
	}

	observedAt := c.now()
	windows, summary, partial, perr := parseCodexUsage(resp.Body, observedAt)
	if perr != nil {
		msg := "codex: invalid response body (could not decode JSON)"
		return c.fail(msg), errors.New(msg)
	}

	status := SourceFresh
	if partial || len(windows) == 0 {
		status = SourcePartial
	}
	snap := QuotaSnapshot{
		MappingID:    c.mappingID,
		CheckedAt:    observedAt,
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
		UsageSummary: summary,
	}
	snap.ResetCredits = c.fetchResetCredits(ctx, token, accountID, observedAt)
	return snap, nil
}

// fail returns a sanitized failed snapshot for this source's mapping.
func (c *CodexSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{
		MappingID:    c.mappingID,
		Availability: QuotaUnknown,
		Status:       SourceFailed,
		Error:        reason,
	}
}

// --- Credential resolution (transient) ------------------------------------

// resolveCredentials reads auth.json transiently via the credential resolver
// and extracts the bearer token (and optional account id). The token is used for
// the immediate request only. Errors are generic and never include the token,
// account id, file path, or file contents.
func (c *CodexSource) resolveCredentials() (token, accountID string, err error) {
	contents, rerr := c.Credentials.Resolve(CredentialRef{Kind: CredentialFile, Locator: c.authFilePath()})
	if rerr != nil {
		return "", "", errors.New("codex: could not read auth credentials")
	}
	var auth codexAuth
	if jerr := json.Unmarshal([]byte(contents), &auth); jerr != nil {
		return "", "", errors.New("codex: auth credentials are malformed")
	}
	token, accountID = auth.bearer()
	if token == "" {
		return "", "", errors.New("codex: no usable bearer token in auth credentials")
	}
	return token, accountID, nil
}

// authFilePath returns the path to auth.json under CODEX_HOME, or ~/.codex when
// CODEX_HOME is unset.
func (c *CodexSource) authFilePath() string {
	if c.CodexHome != "" {
		return filepath.Join(c.CodexHome, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".codex", "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// codexAuth is the sanitized shape of ~/.codex/auth.json. Only the fields needed
// to derive a transient bearer token are modeled.
type codexAuth struct {
	Tokens       *codexTokens `json:"tokens"`
	OpenAIAPIKey string       `json:"OPENAI_API_KEY"`
}

type codexTokens struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

// bearer returns the transient bearer token and optional account id. The OAuth
// access token is preferred; the plain API-key fallback is used when no access
// token is present (no account id in that case).
func (a codexAuth) bearer() (token, accountID string) {
	if a.Tokens != nil && a.Tokens.AccessToken != "" {
		return a.Tokens.AccessToken, a.Tokens.AccountID
	}
	if a.OpenAIAPIKey != "" {
		return a.OpenAIAPIKey, ""
	}
	return "", ""
}

// --- HTTP request ---------------------------------------------------------

// buildRequest builds the GET usage request with the required auth headers. The
// bearer token is attached here and discarded after the request completes.
// ChatGPT-Account-Id is added only when accountID is non-empty.
func (c *CodexSource) buildRequest(ctx context.Context, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	return req, nil
}

func (c *CodexSource) fetchResetCredits(ctx context.Context, token, accountID string, observedAt time.Time) *ResetCreditAttempt {
	if accountID == "" {
		return &ResetCreditAttempt{Status: CreditAttemptSkipped, At: observedAt, Error: "codex reset-credit enrichment skipped: account context unavailable"}
	}
	if c.Evidence == nil {
		return &ResetCreditAttempt{Status: CreditAttemptSkipped, At: observedAt, Error: "codex reset-credit enrichment skipped: contract evidence absent"}
	}
	st := c.Evidence.StatusContract(codexProviderName, CodexResetCreditsContract, observedAt)
	if st.State != EvidenceFresh {
		return &ResetCreditAttempt{Status: CreditAttemptSkipped, At: observedAt, Error: SanitizeText("codex reset-credit enrichment skipped: " + st.Reason)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexResetCreditsEndpoint, nil)
	if err != nil {
		return creditFailure(observedAt, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polytoken-quota")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("originator", "Codex Desktop")
	// Header map assignment intentionally preserves the evidenced endpoint-specific
	// capitalization; net/http Header.Set would canonicalize ID to Id.
	req.Header["ChatGPT-Account-ID"] = []string{accountID}
	resp, err := c.Client.Do(req)
	if err != nil {
		return creditFailure(observedAt, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return creditFailure(observedAt, fmt.Errorf("codex reset-credit request failed (HTTP %d)", resp.StatusCode))
	}
	inventory, partial, err := parseResetCreditInventory(resp.Body, observedAt)
	if err != nil {
		return creditFailure(observedAt, errors.New("codex reset-credit response was invalid"))
	}
	status := CreditAttemptSuccess
	if partial {
		status = CreditAttemptPartial
	}
	return &ResetCreditAttempt{Status: status, At: observedAt, Inventory: inventory}
}

func creditFailure(at time.Time, err error) *ResetCreditAttempt {
	return &ResetCreditAttempt{Status: CreditAttemptFailed, At: at, Error: SanitizeError(err)}
}

func parseResetCreditInventory(body []byte, observedAt time.Time) (*ResetCreditInventory, bool, error) {
	var payload struct {
		AvailableCount *int `json:"available_count"`
		Credits        []struct {
			Status    string           `json:"status"`
			ExpiresAt *json.RawMessage `json:"expires_at"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.AvailableCount == nil || *payload.AvailableCount < 0 {
		return nil, false, errors.New("invalid reset-credit inventory")
	}
	inventory := &ResetCreditInventory{ServerAvailableCount: *payload.AvailableCount, ObservedAt: observedAt}
	partial := false
	for _, item := range payload.Credits {
		if item.Status != "available" {
			inventory.SkippedCount++
			if item.Status != "redeeming" && item.Status != "redeemed" && item.Status != "expired" {
				inventory.DiscrepancyCount++
				partial = true
			}
			continue
		}
		var expiry *time.Time
		if item.ExpiresAt != nil && string(*item.ExpiresAt) != "null" {
			var raw string
			if err := json.Unmarshal(*item.ExpiresAt, &raw); err != nil {
				inventory.SkippedCount++
				inventory.DiscrepancyCount++
				partial = true
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				inventory.SkippedCount++
				inventory.DiscrepancyCount++
				partial = true
				continue
			}
			parsed = parsed.UTC()
			expiry = &parsed
		}
		inventory.AvailableExpiries = append(inventory.AvailableExpiries, expiry)
		if expiry == nil || expiry.After(observedAt) {
			inventory.UsableCount++
		} else {
			inventory.SkippedCount++
			inventory.DiscrepancyCount++
			partial = true
		}
	}
	if inventory.UsableCount != inventory.ServerAvailableCount {
		partial = true
		if inventory.DiscrepancyCount == 0 {
			inventory.DiscrepancyCount = absInt(inventory.ServerAvailableCount - inventory.UsableCount)
		}
	}
	return inventory, partial, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// --- Response parsing (lenient, per-element) ------------------------------

// parseCodexUsage parses the Codex usage body into windows. The top-level body
// must be valid JSON (otherwise an error is returned). Individual windows are
// decoded independently: a malformed window is skipped and partial is set true
// rather than failing the whole response. Unknown fields are ignored.
func parseCodexUsage(body []byte, observedAt time.Time) (windows []QuotaWindow, summary *CodexUsageSummary, partial bool, err error) {
	var top codexUsageTop
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, nil, false, err
	}
	summary = &CodexUsageSummary{ObservedAt: observedAt}
	if credits, bad := decodeUsageCredits(top.Credits); bad {
		partial = true
	} else {
		summary.Credits = credits
	}

	// rate_limit holds the primary (session) and secondary (weekly) windows and
	// possibly an individual_limit.
	var rl codexRateLimitRaw
	if len(top.RateLimit) > 0 {
		if rerr := json.Unmarshal(top.RateLimit, &rl); rerr != nil {
			partial = true // rate_limit itself is malformed; its windows are skipped
		}
	}
	if w, ok, bad := decodePercentWindow(rl.PrimaryWindow, "session"); ok {
		windows = append(windows, w)
	} else if bad {
		partial = true
	}
	if w, ok, bad := decodePercentWindow(rl.SecondaryWindow, "weekly"); ok {
		windows = append(windows, w)
	} else if bad {
		partial = true
	}

	// individual_limit: top-level takes precedence over rate_limit.individual_limit.
	indivRaw := firstRaw(top.IndividualLimit, top.IndividualLimitAlias)
	if len(indivRaw) == 0 {
		indivRaw = firstRaw(rl.IndividualLimit, rl.IndividualLimitAlias)
	}
	if w, ok, bad := decodeIndividualLimit(indivRaw); ok {
		windows = append(windows, w)
		summary.SpendControl = spendControlFromWindow(w)
	} else if bad {
		partial = true
	}

	// additional_rate_limits: decoded lossily per element; one bad entry never
	// discards its siblings.
	if len(top.AdditionalRateLimits) > 0 {
		var arr []codexAdditionalLimitRaw
		if aerr := json.Unmarshal(top.AdditionalRateLimits, &arr); aerr != nil {
			partial = true
		} else {
			for _, e := range arr {
				ewindows, epartial := decodeAdditionalLimit(e)
				windows = append(windows, ewindows...)
				if epartial {
					partial = true
				}
			}
		}
	}

	return windows, summary, partial, nil
}

func decodeUsageCredits(raw json.RawMessage) (*UsageCredits, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var value struct {
		HasCredits *bool           `json:"has_credits"`
		Unlimited  *bool           `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true
	}
	credits := &UsageCredits{HasCredits: value.HasCredits, Unlimited: value.Unlimited}
	if len(value.Balance) > 0 && string(value.Balance) != "null" {
		var balance string
		if err := json.Unmarshal(value.Balance, &balance); err == nil {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(balance), 64)
			if err != nil {
				return nil, true
			}
			if _, err := finiteOrErr(parsed); err != nil {
				return nil, true
			}
		} else {
			if _, err := flexFloat(value.Balance); err != nil {
				return nil, true
			}
			balance = string(value.Balance)
		}
		credits.Balance = &balance
	}
	return credits, false
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func spendControlFromWindow(w QuotaWindow) *SpendControl {
	spend := &SpendControl{Limit: w.Limit, Used: w.Used, ResetAt: w.ResetAt}
	if w.Limit != nil && w.Used != nil {
		remaining := *w.Limit - *w.Used
		spend.Remaining = &remaining
	}
	return spend
}

// codexUsageTop is the top-level usage envelope. Sub-objects are kept as raw
// JSON so each can be decoded independently (lenient per-element decode).
type codexUsageTop struct {
	RateLimit            json.RawMessage `json:"rate_limit"`
	IndividualLimit      json.RawMessage `json:"individual_limit"`
	IndividualLimitAlias json.RawMessage `json:"individualLimit"`
	AdditionalRateLimits json.RawMessage `json:"additional_rate_limits"`
	Credits              json.RawMessage `json:"credits"`
}

type codexRateLimitRaw struct {
	PrimaryWindow        json.RawMessage `json:"primary_window"`
	SecondaryWindow      json.RawMessage `json:"secondary_window"`
	IndividualLimit      json.RawMessage `json:"individual_limit"`
	IndividualLimitAlias json.RawMessage `json:"individualLimit"`
}

type codexAdditionalLimitRaw struct {
	LimitName string          `json:"limit_name"`
	RateLimit json.RawMessage `json:"rate_limit"`
}

// decodePercentWindow decodes a percent-based window (primary/secondary). It
// returns the decoded window, whether a window was produced, and whether the raw
// value was present but malformed (absent is neither decoded nor malformed).
// used_percent is percent USED (set directly on UsagePercent); reset_at is unix
// seconds.
func decodePercentWindow(raw json.RawMessage, name string) (QuotaWindow, bool, bool) {
	if len(raw) == 0 {
		return QuotaWindow{}, false, false
	}
	var pw struct {
		UsedPercent *float64 `json:"used_percent"`
		ResetAt     *int64   `json:"reset_at"`
	}
	if err := json.Unmarshal(raw, &pw); err != nil {
		return QuotaWindow{}, false, true
	}
	w := QuotaWindow{Name: name}
	w.UsagePercent = pw.UsedPercent
	if pw.ResetAt != nil {
		t := time.Unix(*pw.ResetAt, 0).UTC()
		w.ResetAt = &t
	}
	if w.UsagePercent == nil && w.ResetAt == nil {
		// present but empty object; skip without marking partial
		return QuotaWindow{}, false, false
	}
	return w, true, false
}

// decodeIndividualLimit decodes the spend-control limit. limit/used/
// remaining_percent may be a number or numeric string; remaining_percent derives
// UsagePercent = 100 − remaining_percent; resets_at is int seconds. Returns
// (window, ok, malformed).
func decodeIndividualLimit(raw json.RawMessage) (QuotaWindow, bool, bool) {
	if len(raw) == 0 {
		return QuotaWindow{}, false, false
	}
	var il struct {
		Limit            json.RawMessage `json:"limit"`
		Used             json.RawMessage `json:"used"`
		RemainingPercent json.RawMessage `json:"remaining_percent"`
		ResetsAt         json.RawMessage `json:"resets_at"`
	}
	if err := json.Unmarshal(raw, &il); err != nil {
		return QuotaWindow{}, false, true
	}
	w := QuotaWindow{Name: "spend-control"}
	if len(il.Limit) > 0 {
		v, ferr := flexFloat(il.Limit)
		if ferr != nil {
			return QuotaWindow{}, false, true
		}
		w.Limit = &v
	}
	if len(il.Used) > 0 {
		v, ferr := flexFloat(il.Used)
		if ferr != nil {
			return QuotaWindow{}, false, true
		}
		w.Used = &v
	}
	if len(il.RemainingPercent) > 0 {
		rp, ferr := flexFloat(il.RemainingPercent)
		if ferr != nil {
			return QuotaWindow{}, false, true
		}
		used := 100 - rp
		w.UsagePercent = &used
	}
	if len(il.ResetsAt) > 0 {
		ra, ferr := flexInt(il.ResetsAt)
		if ferr != nil {
			return QuotaWindow{}, false, true
		}
		t := time.Unix(ra, 0).UTC()
		w.ResetAt = &t
	}
	if w.Limit == nil && w.Used == nil && w.UsagePercent == nil && w.ResetAt == nil {
		return QuotaWindow{}, false, false // empty object, skip without partial
	}
	return w, true, false
}

// decodeAdditionalLimit decodes one additional_rate_limits entry lossily. Its
// rate_limit primary/secondary windows become named windows (limit_name and
// limit_name-weekly). Returns the decoded windows and whether this entry was
// malformed.
func decodeAdditionalLimit(e codexAdditionalLimitRaw) ([]QuotaWindow, bool) {
	name := e.LimitName
	if name == "" {
		name = "additional"
	}
	if len(e.RateLimit) == 0 {
		return nil, false
	}
	var rl codexRateLimitRaw
	if err := json.Unmarshal(e.RateLimit, &rl); err != nil {
		return nil, true // entry malformed; siblings are unaffected
	}
	var out []QuotaWindow
	bad := false
	if w, ok, b := decodePercentWindow(rl.PrimaryWindow, name); ok {
		out = append(out, w)
	} else if b {
		bad = true
	}
	if w, ok, b := decodePercentWindow(rl.SecondaryWindow, name+"-weekly"); ok {
		out = append(out, w)
	} else if b {
		bad = true
	}
	return out, bad
}

// flexFloat decodes a JSON number or a quoted numeric string into a float64. It
// is used for individual_limit fields that may arrive as either shape.
// Non-finite values (NaN, ±Inf — accepted by ParseFloat from quoted strings)
// are rejected: they would poison remaining/ranking arithmetic and make the
// persisted state JSON unmarshalable.
func flexFloat(raw json.RawMessage) (float64, error) {
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return finiteOrErr(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if perr != nil {
			return 0, errors.New("invalid numeric string")
		}
		return finiteOrErr(v)
	}
	return 0, errors.New("value is neither number nor string")
}

// finiteOrErr returns v unchanged when it is a finite float, or an error for
// NaN and ±Inf.
func finiteOrErr(v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("non-finite numeric value")
	}
	return v, nil
}

// flexInt decodes a JSON integer or a quoted integer string into an int64.
func flexInt(raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if perr != nil {
			return 0, errors.New("invalid integer string")
		}
		return v, nil
	}
	return 0, errors.New("value is neither integer nor string")
}

// determineAvailability derives the snapshot availability from the decoded
// windows. Any window reporting used_percent >= 100 → unavailable; otherwise
// available when at least one window decoded; unknown when none decoded.
func determineAvailability(windows []QuotaWindow) QuotaAvailability {
	if len(windows) == 0 {
		return QuotaUnknown
	}
	for _, w := range windows {
		if w.UsagePercent != nil && *w.UsagePercent >= 100 {
			return QuotaUnavailable
		}
	}
	return QuotaAvailable
}

// Compile-time assertion that CodexSource satisfies the QuotaSource interface.
var _ QuotaSource = (*CodexSource)(nil)
