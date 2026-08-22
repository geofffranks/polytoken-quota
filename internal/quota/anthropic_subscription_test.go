package quota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// subStubTransport replays a canned status/body with optional headers and
// captures the outgoing request for header assertions.
type subStubTransport struct {
	body       []byte
	code       int
	respHeader http.Header
	req        *http.Request
}

func (t *subStubTransport) Do(req *http.Request) (*http.Response, error) {
	t.req = req
	return &http.Response{
		StatusCode: t.code,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     t.respHeader,
	}, nil
}

// subLoader returns an in-memory credential loader resolving the given
// credentials JSON from every candidate path (and optionally the Keychain).
func subLoader(creds string, keychain func(context.Context) ([]byte, error)) *ClaudeCredentialLoader {
	return &ClaudeCredentialLoader{
		ReadFile: func(string) ([]byte, error) { return []byte(creds), nil },
		Keychain: keychain,
	}
}

const subValidCreds = `{"claudeAiOauth":{"accessToken":"synthetic-subscription-token","refreshToken":"synthetic-refresh-token-never-used","expiresAt":"2027-01-01T00:00:00Z"}}`

func newSubscriptionSource(t *testing.T, transport *subStubTransport, creds *ClaudeCredentialLoader) *AnthropicSubscriptionSource {
	t.Helper()
	reg := NewEvidenceRegistry()
	reg.Register(AnthropicSubscriptionEvidence(time.Now()))
	client := &BoundedClient{Transport: transport, Timeout: time.Second, MaxBodyBytes: 1 << 20}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return NewAnthropicSubscriptionSource("anthropic", client, creds, reg, now)
}

func subFindWindow(ws []QuotaWindow, name string) *QuotaWindow {
	for i := range ws {
		if ws[i].Name == name {
			return &ws[i]
		}
	}
	return nil
}

func TestSubscriptionFetchMapsWindows(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-08-22T17:00:00Z"},"seven_day":{"used_percentage":61.2,"resets_at":1787648400}}`)
	tr := &subStubTransport{body: body, code: http.StatusOK}
	snap, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable {
		t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
	}
	sess := subFindWindow(snap.Windows, "session")
	if sess == nil || sess.UsagePercent == nil || *sess.UsagePercent != 42.5 {
		t.Fatalf("session window=%+v", sess)
	}
	if sess.Period == nil || *sess.Period != 300*time.Minute {
		t.Fatalf("session period=%v want 300m", sess.Period)
	}
	if sess.ResetAt == nil || !sess.ResetAt.Equal(time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("session reset=%v want 2026-08-22T17:00:00Z", sess.ResetAt)
	}
	week := subFindWindow(snap.Windows, "weekly")
	if week == nil || week.UsagePercent == nil || *week.UsagePercent != 61.2 {
		t.Fatalf("weekly window=%+v", week)
	}
	if week.Period == nil || *week.Period != 10080*time.Minute {
		t.Fatalf("weekly period=%v want 10080m", week.Period)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem > 0.389 || *rem < 0.387 {
		t.Fatalf("effective remaining=%v want ~0.388", rem)
	}
	// Exhausted weekly cap drives unavailability.
	tr2 := &subStubTransport{body: []byte(`{"five_hour":{"utilization":10},"seven_day":{"used_percentage":100}}`), code: http.StatusOK}
	snap2, err := newSubscriptionSource(t, tr2, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap2.Availability != QuotaUnavailable {
		t.Fatalf("availability=%s want unavailable at 100%%", snap2.Availability)
	}
}

func TestSubscriptionFetchClampsPercentAndParsesEpochMillis(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":117.5,"resets_at":1787420000000},"seven_day":{"used_percentage":-3,"resets_at":"1787648400"}}`)
	tr := &subStubTransport{body: body, code: http.StatusOK}
	snap, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status=%s (error: %s)", snap.Status, snap.Error)
	}
	if w := subFindWindow(snap.Windows, "session"); w == nil || w.UsagePercent == nil || *w.UsagePercent != 100 || w.ResetAt == nil {
		t.Fatalf("session window=%+v", w)
	}
	if w := subFindWindow(snap.Windows, "weekly"); w == nil || w.UsagePercent == nil || *w.UsagePercent != 0 || w.ResetAt == nil {
		t.Fatalf("weekly window=%+v", w)
	}
}

func TestSubscriptionFetchPartialWhenOneWindowMissing(t *testing.T) {
	tr := &subStubTransport{body: []byte(`{"five_hour":{"used_percentage":20}}`), code: http.StatusOK}
	snap, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != SourcePartial {
		t.Fatalf("status=%s want partial", snap.Status)
	}
	if subFindWindow(snap.Windows, "session") == nil || subFindWindow(snap.Windows, "weekly") != nil {
		t.Fatalf("windows=%+v", snap.Windows)
	}
	// No usable window at all is a failure, never an invented one.
	tr2 := &subStubTransport{body: []byte(`{"five_hour":{},"seven_day":null}`), code: http.StatusOK}
	snap2, err2 := newSubscriptionSource(t, tr2, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err2 == nil {
		t.Fatal("expected error with no usable windows")
	}
	if snap2.Status != SourceFailed {
		t.Fatalf("status=%s want failed", snap2.Status)
	}
}

func TestSubscriptionRequestHeadersAndAuth(t *testing.T) {
	tr := &subStubTransport{body: []byte(`{"five_hour":{"used_percentage":1}}`), code: http.StatusOK}
	if _, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	req := tr.req
	if req.Method != http.MethodGet {
		t.Fatalf("method=%s", req.Method)
	}
	if req.URL.String() != anthropicOAuthUsageURL {
		t.Fatalf("url=%s", req.URL.String())
	}
	if got := req.Header.Get("Authorization"); got != "Bearer synthetic-subscription-token" {
		t.Fatalf("authorization header=%q", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta=%q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "claude-code/2.1.0" {
		t.Fatalf("user-agent=%q", got)
	}
}

func TestSubscription401RemediationWithoutTokenLeak(t *testing.T) {
	tr := &subStubTransport{body: []byte(`{"error":"invalid_grant: token synthetic-subscription-token revoked"}`), code: http.StatusUnauthorized}
	snap, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err == nil {
		t.Fatal("401 did not fail")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status=%s", snap.Status)
	}
	if snap.CheckedAt.IsZero() {
		t.Fatal("failed snapshot must carry CheckedAt for durable event history")
	}
	if !strings.Contains(snap.Error, "HTTP 401") || !strings.Contains(snap.Error, "claude login") {
		t.Fatalf("error not actionable: %s", snap.Error)
	}
	if strings.Contains(snap.Error, "synthetic-subscription-token") {
		t.Fatalf("token leaked into error: %s", snap.Error)
	}
}

func TestSubscription429HonorsRetryAfterWithoutBodyLeak(t *testing.T) {
	tr := &subStubTransport{
		body:       []byte(`{"error":"slow down you leek"},"secret-body":"leak-me"`),
		code:       http.StatusTooManyRequests,
		respHeader: http.Header{"Retry-After": []string{"37"}},
	}
	snap, err := newSubscriptionSource(t, tr, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if err == nil {
		t.Fatal("429 did not fail")
	}
	if !strings.Contains(snap.Error, "Retry-After: 37s") {
		t.Fatalf("error missing Retry-After: %s", snap.Error)
	}
	if strings.Contains(snap.Error, "slow down") || strings.Contains(snap.Error, "leak-me") {
		t.Fatalf("429 body leaked into error: %s", snap.Error)
	}
	// HTTP-date form.
	tr2 := &subStubTransport{body: nil, code: http.StatusTooManyRequests, respHeader: http.Header{"Retry-After": []string{"Mon, 02 Jan 2006 15:04:05 GMT"}}}
	snap2, _ := newSubscriptionSource(t, tr2, subLoader(subValidCreds, nil)).Fetch(context.Background())
	if !strings.Contains(snap2.Error, "2006-01-02T15:04:05Z") {
		t.Fatalf("HTTP-date Retry-After not parsed: %s", snap2.Error)
	}
}

func TestSubscriptionEvidenceGateFailsClosed(t *testing.T) {
	tr := &subStubTransport{body: []byte(`{"five_hour":{"used_percentage":1}}`), code: http.StatusOK}
	// No evidence registered.
	src := NewAnthropicSubscriptionSource("anthropic", &BoundedClient{Transport: tr}, subLoader(subValidCreds, nil), nil, time.Now())
	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected fail-closed without evidence")
	}
	if snap.Status != SourceFailed || tr.req != nil {
		t.Fatalf("gate did not fail closed: status=%s request=%v", snap.Status, tr.req)
	}
}

// --- Credential loader -------------------------------------------------------

func TestCredentialLoaderFileOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	var read []string
	loader := &ClaudeCredentialLoader{
		ReadFile: func(path string) ([]byte, error) {
			read = append(read, path)
			if strings.Contains(path, "/cfg/claude") {
				return []byte(subValidCreds), nil
			}
			return nil, os.ErrNotExist
		},
		Keychain: func(context.Context) ([]byte, error) {
			t.Fatal("keychain must not be consulted when a file resolves")
			return nil, nil
		},
	}
	token, err := loader.AccessToken(context.Background(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil || token != "synthetic-subscription-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if len(read) == 0 || !strings.HasPrefix(read[0], "/cfg/claude") {
		t.Fatalf("config-dir path not tried first: %v", read)
	}
}

func TestCredentialLoaderKeychainFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	called := false
	loader := &ClaudeCredentialLoader{
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Keychain: func(context.Context) ([]byte, error) {
			called = true
			return []byte(subValidCreds), nil
		},
	}
	token, err := loader.AccessToken(context.Background(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil || token != "synthetic-subscription-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if !called {
		t.Fatal("keychain fallback not attempted")
	}
}

func TestCredentialLoaderFileOnlyWhenKeychainNil(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	loader := &ClaudeCredentialLoader{
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Keychain: nil, // file-only (non-darwin behavior)
	}
	_, err := loader.AccessToken(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "no Claude Code credentials") {
		t.Fatalf("err=%v want not-found", err)
	}
}

func TestCredentialLoaderDoesNotPreRejectStaleExpiryMetadata(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	creds := `{"claudeAiOauth":{"accessToken":"synthetic-subscription-token","refreshToken":"r","expiresAt":"2026-01-01T00:00:00Z"}}`
	loader := &ClaudeCredentialLoader{
		ReadFile: func(string) ([]byte, error) { return []byte(creds), nil },
		Keychain: func(context.Context) ([]byte, error) { return nil, errors.New("no keychain") },
	}
	token, err := loader.AccessToken(context.Background(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil || token != "synthetic-subscription-token" {
		t.Fatalf("token=%q err=%v; endpoint response must be authoritative", token, err)
	}
}

func TestCredentialLoaderMalformedAndMissing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	bad := &ClaudeCredentialLoader{ReadFile: func(string) ([]byte, error) { return []byte(`{"nope":true}`), nil }}
	if _, err := bad.AccessToken(context.Background(), now); err == nil {
		t.Fatal("malformed credentials accepted")
	}
	noToken := &ClaudeCredentialLoader{ReadFile: func(string) ([]byte, error) { return []byte(`{"claudeAiOauth":{"refreshToken":"only"}}`), nil }}
	if _, err := noToken.AccessToken(context.Background(), now); err == nil {
		t.Fatal("missing access token accepted")
	}
}

func TestCredentialLoaderEpochMillisExpiry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude")
	creds := `{"claudeAiOauth":{"accessToken":"synthetic-subscription-token","expiresAt":1798760000000}}`
	loader := &ClaudeCredentialLoader{ReadFile: func(string) ([]byte, error) { return []byte(creds), nil }}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) // before 2026-12-31
	if _, err := loader.AccessToken(context.Background(), now); err != nil {
		t.Fatalf("future millis expiry rejected: %v", err)
	}
	if _, err := loader.AccessToken(context.Background(), time.UnixMilli(1798760000001)); err != nil {
		t.Fatalf("stale expiry metadata pre-rejected before endpoint validation: %v", err)
	}
}

// TestCredentialLoaderRealFileLayout exercises the real filesystem layout in a
// temp CLAUDE_CONFIG_DIR with the default ReadFile.
func TestCredentialLoaderRealFileLayout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(subValidCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := DefaultClaudeCredentialLoader()
	loader.Keychain = nil // keep the test off the real Keychain
	token, err := loader.AccessToken(context.Background(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil || token != "synthetic-subscription-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}
