// anthropic_subscription.go implements the experimental Claude subscription
// quota adapter. It is selected by `providers.anthropic.quota.mode:
// subscription` in desired.yaml (policy load maps that to the
// "anthropic-subscription" adapter) and is entirely separate from the default
// admin-budget Anthropic adapter in anthropic.go.
//
// Claude Pro/Max subscriptions expose usage via the OAuth usage endpoint
// (GET https://api.anthropic.com/api/oauth/usage) behind the Claude Code
// OAuth access token. The response reports two rolling windows — `five_hour`
// (the session cap) and `seven_day` (the weekly cap) — each with a utilization
// percentage and reset timestamps. This adapter maps them to "session"
// (period 300m) and "weekly" (period 10080m) quota windows keyed on
// usage percentage; there is no absolute used/limit pair to report.
//
// Credentials are transient and READ-ONLY. The access token is resolved at
// request time from, in order: $CLAUDE_CONFIG_DIR/.credentials.json,
// ~/.claude/.credentials.json, or (macOS only, when both files are absent)
// the macOS Keychain item "Claude Code-credentials" via the read-only
// `security find-generic-password -w` subprocess. The refresh token in the
// same credential blob is never used, written, or logged; nothing is ever
// persisted back. All errors are sanitized via SanitizeError and never carry
// tokens, auth headers, or raw response bodies.
package quota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicOAuthUsageURL    = "https://api.anthropic.com/api/oauth/usage"
	anthropicSubscriptionName = "anthropic-subscription"
	// anthropicOAuthBeta is the required anthropic-beta header value for the
	// OAuth-scoped usage endpoint.
	anthropicOAuthBeta = "oauth-2025-04-20"
	// anthropicOAuthUserAgent mirrors the Claude Code client the endpoint
	// expects; a generic tool UA risks rejection.
	anthropicOAuthUserAgent = "claude-code/2.1.0"
	// anthropicCredentialsFile is the credentials file name inside the Claude
	// config directory.
	anthropicCredentialsFile = ".credentials.json"
	// anthropicKeychainService is the macOS Keychain generic-password service
	// Claude Code stores its credentials JSON under.
	anthropicKeychainService = "Claude Code-credentials"
	// anthropicKeychainTimeout bounds the read-only `security` subprocess.
	anthropicKeychainTimeout = 10 * time.Second
	// Window periods for the two reported quotas.
	anthropicSessionPeriod = 300 * time.Minute
	anthropicWeeklyPeriod  = 10080 * time.Minute
)

// AnthropicSubscriptionSource polls the Claude OAuth usage endpoint behind the
// evidence gate and reports session/weekly percentage windows. It satisfies the
// QuotaSource interface. The HTTP transport and credential loader are
// injectable for tests.
type AnthropicSubscriptionSource struct {
	mappingID   string
	Client      *BoundedClient
	Credentials *ClaudeCredentialLoader
	Evidence    *EvidenceRegistry
	Now         func() time.Time
}

// AnthropicSubscriptionEvidence returns the sanitized contract evidence for
// the subscription adapter. Register it in an EvidenceRegistry so the gate
// recognizes "anthropic-subscription" as fresh. Dates are release-owned and do
// not renew on construction. ReviewBy is three months after the recording
// date: the endpoint is undocumented, so it reviews on the quarterly cadence.
func AnthropicSubscriptionEvidence(_ time.Time) Evidence {
	return Evidence{
		Provider:    anthropicSubscriptionName,
		Endpoint:    anthropicOAuthUsageURL,
		Method:      http.MethodGet,
		AuthType:    "oauth-bearer",
		SchemaNote:  "undocumented; observed as {five_hour:{utilization|used_percentage,resets_at},seven_day:{...}}; percentages are clamped to 0-100; resets_at accepts RFC3339 or epoch seconds/milliseconds",
		FixturePath: "contract/testdata/quota/anthropic-subscription/usage.json",
		RecordedAt:  evidenceRecordedAt(),
		ReviewBy:    evidenceRecordedAt().AddDate(0, 3, 0),
	}
}

// NewAnthropicSubscriptionSource constructs the source consulting the supplied
// release-owned evidence registry. Construction never registers or refreshes
// evidence. If reg is nil, an empty registry is created and the source is
// unsupported. If creds is nil, the default credential loader (config-dir
// file, home file, then read-only macOS Keychain) is used.
func NewAnthropicSubscriptionSource(mappingID string, client *BoundedClient, creds *ClaudeCredentialLoader, reg *EvidenceRegistry, now time.Time) *AnthropicSubscriptionSource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	if creds == nil {
		creds = DefaultClaudeCredentialLoader()
	}
	return &AnthropicSubscriptionSource{
		mappingID:   mappingID,
		Client:      client,
		Credentials: creds,
		Evidence:    reg,
		Now:         func() time.Time { return now },
	}
}

// MappingID returns the provider mapping this source serves.
func (s *AnthropicSubscriptionSource) MappingID() string { return s.mappingID }

func (s *AnthropicSubscriptionSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Status reports whether this source is supported, fail-closed on absent,
// expired, or incomplete evidence.
func (s *AnthropicSubscriptionSource) Status() SupportStatus {
	if s.Evidence == nil {
		return SupportStatus{
			Supported: false,
			Reason:    "provider " + anthropicSubscriptionName + " has no recorded contract evidence; record evidence before enabling",
		}
	}
	return SupportFromEvidence(s.Evidence.Status(anthropicSubscriptionName, s.now()))
}

// fail returns a sanitized failed snapshot for this source's mapping.
func (s *AnthropicSubscriptionSource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{
		MappingID:    s.mappingID,
		CheckedAt:    s.now(),
		Availability: QuotaUnknown,
		Status:       SourceFailed,
		Error:        reason,
	}
}

// Fetch retrieves the subscription usage snapshot. It fails closed when the
// evidence gate is unsupported without making any request, resolves the OAuth
// access token transiently, performs one bounded GET, and maps five_hour /
// seven_day into session / weekly windows. Errors never carry tokens, auth
// headers, or raw bodies.
func (s *AnthropicSubscriptionSource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := s.Status()
	if !st.Supported {
		return s.fail(st.Reason), errors.New(st.Reason)
	}
	if s.Credentials == nil {
		reason := "anthropic subscription: no credential loader configured"
		return s.fail(reason), errors.New(reason)
	}

	token, err := s.Credentials.AccessToken(ctx, s.now())
	if err != nil {
		msg := SanitizeError(err)
		return s.fail(msg), errors.New(msg)
	}

	resp, herr := s.fetchUsage(ctx, token)
	if herr != nil {
		msg := SanitizeError(herr)
		return s.fail(msg), errors.New(msg)
	}

	windows, partial, perr := parseAnthropicOAuthUsage(resp)
	if perr != nil {
		msg := SanitizeError(perr)
		return s.fail(msg), errors.New(msg)
	}
	status := SourceFresh
	if partial {
		status = SourcePartial
	}
	return QuotaSnapshot{
		MappingID:    s.mappingID,
		CheckedAt:    s.now(),
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
	}, nil
}

// fetchUsage performs one bounded GET of the OAuth usage endpoint.
func (s *AnthropicSubscriptionSource) fetchUsage(ctx context.Context, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicOAuthUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	req.Header.Set("User-Agent", anthropicOAuthUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("anthropic subscription: credentials rejected (HTTP 401); re-authenticate with Claude Code (`claude login`) so a fresh access token is written, then retry")
	case resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("anthropic subscription: access denied (HTTP 403); verify the account has a Claude subscription and the OAuth session is still valid")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("anthropic subscription: rate limited (HTTP 429)%s; retry on the next scheduled check", retryAfterSuffix(resp.Headers))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("anthropic subscription: server error (HTTP %d)", resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		return nil, errors.New("anthropic subscription: empty usage response body")
	}
	return resp.Body, nil
}

// retryAfterSuffix formats a Retry-After header (delta-seconds or HTTP-date)
// for a 429 error string. The response body is never consulted or leaked.
func retryAfterSuffix(h http.Header) string {
	if h == nil {
		return ""
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return " (no Retry-After supplied)"
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return fmt.Sprintf(" (Retry-After: %ds)", secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		return " (Retry-After: " + t.UTC().Format(time.RFC3339) + ")"
	}
	return " (malformed Retry-After header)"
}

// --- Response parsing (lenient) ---------------------------------------------

// anthropicOAuthUsage is the observed usage response shape. The two windows
// are top-level objects with a percentage and optional reset timestamp.
type anthropicOAuthUsage struct {
	FiveHour json.RawMessage `json:"five_hour"`
	SevenDay json.RawMessage `json:"seven_day"`
}

// parseAnthropicOAuthUsage decodes the usage body into session and weekly
// windows. Parsing is lenient per window: an absent or unusable window is
// skipped and the snapshot marked partial rather than failed, so one broken
// window never hides the other. Both windows unusable is an error.
func parseAnthropicOAuthUsage(body []byte) ([]QuotaWindow, bool, error) {
	var top anthropicOAuthUsage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, errors.New("anthropic subscription: invalid usage body (not JSON)")
	}
	five, seven := top.FiveHour, top.SevenDay
	var windows []QuotaWindow
	partial := false
	if w, ok := parseOAuthWindow("session", anthropicSessionPeriod, five); ok {
		windows = append(windows, w)
	} else {
		partial = true
	}
	if w, ok := parseOAuthWindow("weekly", anthropicWeeklyPeriod, seven); ok {
		windows = append(windows, w)
	} else {
		partial = true
	}
	if len(windows) == 0 {
		return nil, false, errors.New("anthropic subscription: usage body has no usable five_hour or seven_day window")
	}
	return windows, partial, nil
}

// parseOAuthWindow decodes one window's percentage and optional reset timestamp.
// It returns ok=false when the window is absent or lacks a usable percentage.
func parseOAuthWindow(name string, period time.Duration, raw json.RawMessage) (QuotaWindow, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return QuotaWindow{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return QuotaWindow{}, false
	}
	var pct *float64
	if v, ok := percentFromRaw(fields["utilization"]); ok {
		v = clampRange(v, 0, 100)
		pct = &v
	} else if v, ok := percentFromRaw(fields["used_percentage"]); ok {
		v = clampRange(v, 0, 100)
		pct = &v
	}
	if pct == nil {
		return QuotaWindow{}, false
	}
	var reset *time.Time
	if t, ok := resetFromRaw(fields["resets_at"]); ok {
		reset = &t
	}
	p := period
	return QuotaWindow{
		Name:         name,
		UsagePercent: pct,
		ResetAt:      reset,
		Period:       &p,
	}, true
}

// percentFromRaw extracts a finite numeric percentage. The caller clamps the
// provider value to 0-100, matching Claude Code's mapper.
func percentFromRaw(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// resetFromRaw accepts the formats emitted by Claude Code: RFC3339 strings,
// numeric strings, or numeric epoch seconds/milliseconds.
func resetFromRaw(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return epochTime(number)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, false
	}
	text = strings.TrimSpace(text)
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, true
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		return epochTime(number)
	}
	return time.Time{}, false
}

func epochTime(value float64) (time.Time, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}, false
	}
	if value > 10_000_000_000 {
		return time.UnixMilli(int64(value)), true
	}
	return time.Unix(int64(value), 0), true
}

// Compile-time assertion that AnthropicSubscriptionSource satisfies QuotaSource.
var _ QuotaSource = (*AnthropicSubscriptionSource)(nil)

// --- Claude credential loader (read-only) -----------------------------------

// ClaudeCredentialLoader resolves the Claude Code OAuth credentials
// transiently and read-only. Order: $CLAUDE_CONFIG_DIR/.credentials.json,
// then ~/.claude/.credentials.json, then (macOS only) the Keychain item
// "Claude Code-credentials" via a bounded, shell-free `security` subprocess.
// The refresh token is parsed only to be ignored; it is never used, written,
// or logged. Every seam is injectable so tests never touch the filesystem,
// the Keychain, or subprocesses.
type ClaudeCredentialLoader struct {
	// ReadFile reads a credentials file. Defaults to os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// Keychain returns the Keychain-stored credentials JSON. Defaults to a
	// bounded `security find-generic-password -s "Claude Code-credentials" -w`
	// invocation on darwin and to a "not available" error elsewhere. A nil
	// value disables the Keychain fallback entirely (file-only resolution).
	Keychain func(context.Context) ([]byte, error)
}

// DefaultClaudeCredentialLoader returns the production loader.
func DefaultClaudeCredentialLoader() *ClaudeCredentialLoader {
	return &ClaudeCredentialLoader{
		ReadFile: os.ReadFile,
		Keychain: darwinKeychainCredentials,
	}
}

// claudeCredentialsJSON is the .credentials.json shape. Only the access token
// is used for requests; expiry metadata is parsed for compatibility tests but
// is not authoritative, and the refresh token is never exposed.
type claudeCredentialsJSON struct {
	ClaudeAiOauth *struct {
		AccessToken  string `json:"accessToken"`
		ExpiresAt    any    `json:"expiresAt"` // RFC3339 string or epoch seconds/millis
		RefreshToken string `json:"refreshToken"`
	} `json:"claudeAiOauth"`
}

// credentialPaths returns the candidate credentials file paths in order.
func credentialPaths() []string {
	var paths []string
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, anthropicCredentialsFile))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".claude", anthropicCredentialsFile))
	}
	return paths
}

// AccessToken resolves the current OAuth access token transiently for the
// immediate request. The endpoint's 401 response is authoritative because
// Claude Code may leave stale expiry metadata alongside a usable scoped token.
// Errors are generic and never include file contents or token values.
func (l *ClaudeCredentialLoader) AccessToken(ctx context.Context, _ time.Time) (string, error) {
	readFile := l.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	var lastErr error
	for _, path := range credentialPaths() {
		b, err := readFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				lastErr = err
			}
			continue
		}
		token, _, perr := parseClaudeCredentials(b)
		if perr != nil {
			lastErr = perr
			continue
		}
		return token, nil
	}
	if l.Keychain != nil {
		b, err := l.Keychain(ctx)
		if err == nil {
			token, _, perr := parseClaudeCredentials(b)
			if perr == nil {
				return token, nil
			}
			lastErr = perr
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", errors.New("anthropic subscription: could not read Claude Code credentials")
	}
	return "", errors.New("anthropic subscription: no Claude Code credentials found (looked for .credentials.json in CLAUDE_CONFIG_DIR and ~/.claude; on macOS also the Keychain) — sign in with Claude Code first")
}

// parseClaudeCredentials extracts the access token and expiry from the
// credentials JSON. The refresh token field is deliberately not returned.
func parseClaudeCredentials(b []byte) (token string, expiresAt time.Time, err error) {
	var c claudeCredentialsJSON
	if uerr := json.Unmarshal(b, &c); uerr != nil || c.ClaudeAiOauth == nil {
		return "", time.Time{}, errors.New("anthropic subscription: credentials are not valid Claude Code credentials JSON")
	}
	token = strings.TrimSpace(c.ClaudeAiOauth.AccessToken)
	if token == "" {
		return "", time.Time{}, errors.New("anthropic subscription: credentials have no access token; sign in with Claude Code")
	}
	switch v := c.ClaudeAiOauth.ExpiresAt.(type) {
	case string:
		if t, terr := time.Parse(time.RFC3339, v); terr == nil {
			expiresAt = t
		}
	case float64:
		sec := int64(v)
		// Heuristic: values above ~1e12 are epoch milliseconds.
		if sec > 1e12 {
			expiresAt = time.UnixMilli(sec)
		} else {
			expiresAt = time.Unix(sec, 0)
		}
	}
	return token, expiresAt, nil
}

// darwinKeychainCredentials reads the Claude Code credentials JSON from the
// macOS Keychain via `security find-generic-password -w`. It is read-only,
// shell-free (direct exec, no shell interpretation), bounded by
// anthropicKeychainTimeout, and only attempts anything on darwin — other
// platforms get a "not available" error so resolution stays file-only there.
func darwinKeychainCredentials(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("anthropic subscription: keychain credentials only available on macOS")
	}
	services := []string{anthropicKeychainService}
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configDir = filepath.Join(home, ".claude")
		}
	}
	if configDir != "" {
		sum := sha256.Sum256([]byte(configDir))
		suffix := hex.EncodeToString(sum[:])[:8]
		services = []string{anthropicKeychainService + "-" + suffix, anthropicKeychainService}
	}
	account := os.Getenv("USER")
	if account == "" {
		account = os.Getenv("USERNAME")
	}
	if account == "" {
		account = "user"
	}
	for _, service := range services {
		cctx, cancel := context.WithTimeout(ctx, anthropicKeychainTimeout)
		cmd := exec.CommandContext(cctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w")
		out, err := cmd.Output()
		cancel()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("anthropic subscription: keychain read failed or item not found")
}
