package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---------------------------------------------------------

// codexTestNow is a stable observation/evidence clock for deterministic tests.
var codexTestNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// Synthetic auth.json content used across tests. The token and account values
// are deliberately distinctive so redaction assertions can detect any leak.
const (
	codexTestBearer   = "synthetic-bearer-T0k3n-V4lu3"
	codexTestAccount  = "acct-s3cr3t-synthetic"
	codexTestAuthJSON = `{"tokens":{"access_token":"` + codexTestBearer +
		`","refresh_token":"synthetic-refresh","id_token":"synthetic-id","account_id":"` +
		codexTestAccount + `"},"last_refresh":"2026-01-01T00:00:00Z"}`
)

// codexTestSource builds a CodexSource with the given transport, credential
// resolver, and evidence (evaluated at codexTestNow). An empty Evidence.Provider
// leaves the registry empty (absent evidence).
func codexTestSource(t *testing.T, doer HTTPDoer, resolver CredentialResolver, ev Evidence) *CodexSource {
	t.Helper()
	reg := NewEvidenceRegistry()
	if ev.Provider != "" {
		reg.Register(ev)
	}
	return &CodexSource{
		mappingID:   "codex-test",
		Client:      &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 20},
		Credentials: resolver,
		Evidence:    reg,
		Now:         func() time.Time { return codexTestNow },
	}
}

// freshCodex returns a CodexSource backed by doer + a synthetic auth resolver,
// with fresh contract evidence registered.
func freshCodex(t *testing.T, doer HTTPDoer) *CodexSource {
	t.Helper()
	return codexTestSource(t, doer, &fakeResolver{val: codexTestAuthJSON}, CodexEvidence(codexTestNow))
}

func findWindow(ws []QuotaWindow, name string) *QuotaWindow {
	for i := range ws {
		if ws[i].Name == name {
			return &ws[i]
		}
	}
	return nil
}

func floatClose(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// snapshotSecretSurface flattens the sanitized snapshot fields a secret scan can
// inspect. It deliberately excludes nothing — any leak surfaces here.
func snapshotSecretSurface(snap QuotaSnapshot) string {
	var b strings.Builder
	b.WriteString(snap.MappingID)
	b.WriteByte(' ')
	b.WriteString(string(snap.Availability))
	b.WriteByte(' ')
	b.WriteString(string(snap.Status))
	b.WriteByte(' ')
	b.WriteString(snap.Error)
	for _, w := range snap.Windows {
		b.WriteByte(' ')
		b.WriteString(w.Name)
		if w.Used != nil {
			b.WriteString(" used=")
			b.WriteString(formatFloat(*w.Used))
		}
		if w.Limit != nil {
			b.WriteString(" limit=")
			b.WriteString(formatFloat(*w.Limit))
		}
		if w.UsagePercent != nil {
			b.WriteString(" pct=")
			b.WriteString(formatFloat(*w.UsagePercent))
		}
		if w.ResetAt != nil {
			b.WriteString(" reset=")
			b.WriteString(w.ResetAt.Format(time.RFC3339Nano))
		}
	}
	return b.String()
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Inline response bodies matching the recorded contract shapes.

const codexProBody = `{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window":   { "used_percent": 22, "reset_at": 1766948068, "limit_window_seconds": 18000 },
    "secondary_window": { "used_percent": 43, "reset_at": 1767407914, "limit_window_seconds": 604800 },
    "individual_limit": { "limit": 100000, "used": 7761, "remaining_percent": 92.239, "resets_at": 1782864000 }
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0E-10" }
}`

const codexMinimalBody = `{
  "rate_limit": {
    "primary_window": { "used_percent": 22, "reset_at": 1766948068, "limit_window_seconds": 18000 }
  }
}`

const codexAdditionalBody = `{
  "rate_limit": {
    "primary_window": { "used_percent": 22, "reset_at": 1766948068, "limit_window_seconds": 18000 }
  },
  "additional_rate_limits": [
    { "limit_name": "GPT-5.3-Codex-Spark", "rate_limit": { "primary_window": { "used_percent": 15, "reset_at": 1766948068, "limit_window_seconds": 18000 }, "secondary_window": { "used_percent": 30, "reset_at": 1767407914, "limit_window_seconds": 604800 } } },
    { "limit_name": "Another-Model", "rate_limit": { "primary_window": { "used_percent": 5, "reset_at": 1766948068, "limit_window_seconds": 18000 } } }
  ]
}`

const codexExhaustedBody = `{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window":   { "used_percent": 100, "reset_at": 1766948068, "limit_window_seconds": 18000 },
    "secondary_window": { "used_percent": 43, "reset_at": 1767407914, "limit_window_seconds": 604800 }
  }
}`

const codexPartialBody = `{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window":   { "used_percent": 22, "reset_at": 1766948068, "limit_window_seconds": 18000 },
    "secondary_window": { "used_percent": "not-a-number", "reset_at": "bad" }
  }
}`

// Two additional entries plus a third malformed one: the good siblings must
// survive and the snapshot downgrades to SourcePartial.
const codexBadSiblingBody = `{
  "rate_limit": {
    "primary_window": { "used_percent": 10, "reset_at": 1766948068, "limit_window_seconds": 18000 }
  },
  "additional_rate_limits": [
    { "limit_name": "Good-One", "rate_limit": { "primary_window": { "used_percent": 5, "reset_at": 1766948068, "limit_window_seconds": 18000 } } },
    { "limit_name": "Bad-Two", "rate_limit": "not-an-object" },
    { "limit_name": "Good-Three", "rate_limit": { "primary_window": { "used_percent": 8, "reset_at": 1766948068, "limit_window_seconds": 18000 } } }
  ]
}`

// Top-level individual_limit takes precedence over rate_limit.individual_limit.
const codexPrecedenceBody = `{
  "rate_limit": {
    "primary_window": { "used_percent": 22, "reset_at": 1766948068, "limit_window_seconds": 18000 },
    "individual_limit": { "limit": 111, "used": 11, "remaining_percent": 90, "resets_at": 1700000000 }
  },
  "individual_limit": { "limit": 222, "used": 22, "remaining_percent": 80, "resets_at": 1800000000 }
}`

type sequenceDoer struct {
	responses []*http.Response
	calls     []*http.Request
}

func (d *sequenceDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls = append(d.calls, req)
	if len(d.responses) == 0 {
		return nil, errors.New("unexpected request")
	}
	resp := d.responses[0]
	d.responses = d.responses[1:]
	return resp, nil
}

func codexSourceWithEvidence(t *testing.T, doer HTTPDoer, usage, reset Evidence) *CodexSource {
	t.Helper()
	reg := NewEvidenceRegistry()
	if usage.Provider != "" {
		reg.Register(usage)
	}
	if reset.Provider != "" {
		reg.Register(reset)
	}
	return &CodexSource{
		mappingID:   "codex-test",
		Client:      &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 20},
		Credentials: &fakeResolver{val: codexTestAuthJSON},
		Evidence:    reg,
		Now:         func() time.Time { return codexTestNow },
	}
}

func TestResetCreditEvidenceFailureDoesNotBlockUsage(t *testing.T) {
	cases := []struct {
		name  string
		reset Evidence
	}{
		{name: "absent"},
		{name: "stale", reset: func() Evidence {
			e := CodexResetCreditsEvidence(codexTestNow)
			e.ReviewBy = codexTestNow.Add(-time.Hour)
			return e
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &sequenceDoer{responses: []*http.Response{bodyResponse(200, []byte(codexProBody))}}
			src := codexSourceWithEvidence(t, doer, CodexUsageEvidence(codexTestNow), tc.reset)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status == SourceFailed || snap.Availability != QuotaAvailable {
				t.Fatalf("ordinary usage blocked by optional evidence: snap=%+v err=%v", snap, err)
			}
			if len(doer.calls) != 1 {
				t.Fatalf("calls=%d want usage request only", len(doer.calls))
			}
			if snap.ResetCredits == nil || snap.ResetCredits.Status != CreditAttemptSkipped || snap.ResetCredits.Error == "" {
				t.Fatalf("missing sanitized skipped enrichment attempt: %+v", snap.ResetCredits)
			}
			if strings.Contains(snapshotSecretSurface(snap)+snap.ResetCredits.Error, codexTestAccount) {
				t.Fatal("skipped attempt leaks account context")
			}
		})
	}
}

func TestCodexUsageCreditsContract(t *testing.T) {
	doer := &sequenceDoer{responses: []*http.Response{bodyResponse(200, []byte(codexProBody))}}
	src := codexSourceWithEvidence(t, doer, CodexUsageEvidence(codexTestNow), Evidence{})
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsageSummary == nil || snap.UsageSummary.Credits == nil {
		t.Fatalf("missing ordinary credit observation: %+v", snap.UsageSummary)
	}
	credits := snap.UsageSummary.Credits
	if credits.HasCredits == nil || *credits.HasCredits || credits.Unlimited == nil || *credits.Unlimited {
		t.Fatalf("credit booleans not preserved: %+v", credits)
	}
	if credits.Balance == nil || *credits.Balance != "0E-10" {
		t.Fatalf("balance=%v want original exponent decimal string", credits.Balance)
	}
}

func TestCodexSpendControlContract(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":1},"individualLimit":{"limit":"100000.50","used":7761,"remaining_percent":"92.239","resets_at":1782864000}}}`
	doer := &sequenceDoer{responses: []*http.Response{bodyResponse(200, []byte(body))}}
	src := codexSourceWithEvidence(t, doer, CodexUsageEvidence(codexTestNow), Evidence{})
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	spend := snap.UsageSummary.SpendControl
	if spend == nil || spend.Limit == nil || *spend.Limit != 100000.5 || spend.Used == nil || *spend.Used != 7761 || spend.Remaining == nil || *spend.Remaining != 92239.5 {
		t.Fatalf("spend-control values not preserved: %+v", spend)
	}
	wantReset := time.Unix(1782864000, 0).UTC()
	if spend.ResetAt == nil || !spend.ResetAt.Equal(wantReset) {
		t.Fatalf("spend reset=%v want %v", spend.ResetAt, wantReset)
	}
}

func TestCodexResetCreditInventoryContract(t *testing.T) {
	future := codexTestNow.Add(24 * time.Hour).Format(time.RFC3339)
	past := codexTestNow.Add(-time.Hour).Format(time.RFC3339)
	body := `{"available_count":4,"credits":[` +
		`{"id":"secret-id-1","type":"limit_reset","title":"private title","description":"private description","status":"available","expires_at":"` + future + `"},` +
		`{"id":"secret-id-2","status":"available"},` +
		`{"id":"secret-id-3","status":"available","expires_at":"` + past + `"},` +
		`{"id":"secret-id-4","status":"mystery"},` +
		`{"id":"secret-id-5","status":"redeemed"}]}`
	doer := &sequenceDoer{responses: []*http.Response{
		bodyResponse(200, []byte(codexMinimalBody)),
		bodyResponse(200, []byte(body)),
	}}
	src := codexSourceWithEvidence(t, doer, CodexUsageEvidence(codexTestNow), CodexResetCreditsEvidence(codexTestNow))
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("calls=%d want usage + reset inventory", len(doer.calls))
	}
	req := doer.calls[1]
	accountHeader := req.Header["ChatGPT-Account-ID"]
	if req.URL.String() != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" || req.Header.Get("OpenAI-Beta") != "codex-1" || req.Header.Get("originator") != "Codex Desktop" || len(accountHeader) != 1 || accountHeader[0] != codexTestAccount {
		t.Fatalf("reset request contract mismatch: url=%s headers=%v", req.URL, req.Header)
	}
	attempt := snap.ResetCredits
	if attempt == nil || attempt.Status != CreditAttemptPartial || attempt.Inventory == nil {
		t.Fatalf("reset attempt=%+v want partial inventory", attempt)
	}
	inv := attempt.Inventory
	if inv.ServerAvailableCount != 4 || inv.UsableCount != 2 || inv.DiscrepancyCount != 2 || inv.SkippedCount != 3 || len(inv.AvailableExpiries) != 3 || inv.AvailableExpiries[1] != nil {
		t.Fatalf("inventory semantics mismatch: %+v", inv)
	}
	persisted := fmt.Sprintf("%+v", snap)
	for _, prohibited := range []string{"secret-id", "private title", "private description", "limit_reset", "redeemed", "mystery"} {
		if strings.Contains(persisted, prohibited) {
			t.Fatalf("normalized observation persists prohibited %q: %s", prohibited, persisted)
		}
	}
}

// --- Evidence gate --------------------------------------------------------

func TestCodexStatusGatedByEvidence(t *testing.T) {
	// Fresh evidence → supported.
	fresh := freshCodex(t, &recordingDoer{})
	if !fresh.Status().Supported {
		t.Fatal("fresh evidence should be supported")
	}

	// Absent evidence → unsupported.
	absent := codexTestSource(t, &recordingDoer{}, &fakeResolver{val: codexTestAuthJSON}, Evidence{})
	if absent.Status().Supported {
		t.Fatal("absent evidence should be unsupported")
	}

	// Expired evidence → unsupported.
	expired := CodexEvidence(codexTestNow)
	expired.ReviewBy = codexTestNow.Add(-24 * time.Hour)
	exp := codexTestSource(t, &recordingDoer{}, &fakeResolver{val: codexTestAuthJSON}, expired)
	if exp.Status().Supported {
		t.Fatal("expired evidence should be unsupported")
	}
}

func TestCodexEvidenceGateAbsentFailsClosed(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	src := codexTestSource(t, doer, &fakeResolver{val: codexTestAuthJSON}, Evidence{}) // absent

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for absent evidence")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called when evidence is absent; got %d calls", len(doer.calls))
	}
}

func TestCodexEvidenceGateExpiredFailsClosed(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	expired := CodexEvidence(codexTestNow)
	expired.ReviewBy = codexTestNow.Add(-24 * time.Hour)
	src := codexTestSource(t, doer, &fakeResolver{val: codexTestAuthJSON}, expired)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for expired evidence")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called when evidence is expired; got %d calls", len(doer.calls))
	}
}

func TestCodexEvidenceFreshProceeds(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("transport should be called once; got %d calls", len(doer.calls))
	}
}

// --- Credential resolution ------------------------------------------------

func TestCodexCredentialResolutionFailure(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	src := codexTestSource(t, doer, &fakeResolver{err: errors.New("boom")}, CodexEvidence(codexTestNow))

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error when credentials cannot be resolved")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// Transport must never be reached when the token cannot be resolved.
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called; got %d calls", len(doer.calls))
	}
	// The error must be a generic auth/config diagnostic, never the token value.
	if strings.Contains(err.Error(), codexTestBearer) {
		t.Fatalf("credential error leaks token: %s", err)
	}
	if strings.Contains(snap.Error, codexTestBearer) {
		t.Fatalf("snapshot error leaks token: %s", snap.Error)
	}
}

func TestCodexMalformedAuthJSON(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	src := codexTestSource(t, doer, &fakeResolver{val: "{not valid json"}, CodexEvidence(codexTestNow))

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed auth.json")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called; got %d calls", len(doer.calls))
	}
	if strings.Contains(err.Error(), "not valid json") {
		t.Fatalf("malformed-auth error leaks raw contents: %s", err)
	}
}

func TestCodexAPIKeyFallback(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexMinimalBody))}
	// No access_token; the plain OPENAI_API_KEY fallback is used (no account id).
	src := codexTestSource(t, doer, &fakeResolver{val: `{"OPENAI_API_KEY":"sk-synthetic-key"}`}, CodexEvidence(codexTestNow))

	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("transport should be called once; got %d calls", len(doer.calls))
	}
	got := doer.lastCall()
	if got.Header.Get("Authorization") != "Bearer sk-synthetic-key" {
		t.Fatalf("api-key fallback not used as bearer: %q", got.Header.Get("Authorization"))
	}
	// No account id in the API-key path → header absent.
	if h := got.Header.Get("ChatGPT-Account-Id"); h != "" {
		t.Fatalf("ChatGPT-Account-Id should be absent for api-key fallback; got %q", h)
	}
}

func TestCodexRequestHeaders(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexMinimalBody))}
	src := freshCodex(t, doer)

	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := doer.lastCall()
	if got.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", got.Method)
	}
	if got.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("url = %q", got.URL.String())
	}
	if got.Header.Get("Authorization") != "Bearer "+codexTestBearer {
		t.Fatalf("authorization header = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Accept") != "application/json" {
		t.Fatalf("accept header = %q", got.Header.Get("Accept"))
	}
	if got.Header.Get("User-Agent") != "polytoken-quota" {
		t.Fatalf("user-agent header = %q", got.Header.Get("User-Agent"))
	}
	if got.Header.Get("ChatGPT-Account-Id") != codexTestAccount {
		t.Fatalf("account-id header = %q", got.Header.Get("ChatGPT-Account-Id"))
	}
}

// --- Parsing: lenient per-element decode ----------------------------------

func TestCodexProParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}
	if snap.Availability != QuotaAvailable {
		t.Fatalf("availability = %s, want available", snap.Availability)
	}
	if !snap.CheckedAt.Equal(codexTestNow) {
		t.Fatalf("checked-at = %v, want %v", snap.CheckedAt, codexTestNow)
	}

	// Session (primary) window: used_percent → UsagePercent, reset_at seconds.
	session := findWindow(snap.Windows, "session")
	if session == nil {
		t.Fatal("missing session window")
	}
	if session.UsagePercent == nil || *session.UsagePercent != 22 {
		t.Fatalf("session usage_percent = %v, want 22", session.UsagePercent)
	}
	if session.ResetAt == nil || !session.ResetAt.Equal(time.Unix(1766948068, 0).UTC()) {
		t.Fatalf("session reset_at = %v", session.ResetAt)
	}

	// Weekly (secondary) window.
	weekly := findWindow(snap.Windows, "weekly")
	if weekly == nil {
		t.Fatal("missing weekly window")
	}
	if weekly.UsagePercent == nil || *weekly.UsagePercent != 43 {
		t.Fatalf("weekly usage_percent = %v, want 43", weekly.UsagePercent)
	}
	if weekly.ResetAt == nil || !weekly.ResetAt.Equal(time.Unix(1767407914, 0).UTC()) {
		t.Fatalf("weekly reset_at = %v", weekly.ResetAt)
	}

	// Spend-control (individual_limit): used/limit/remaining_percent/resets_at.
	sc := findWindow(snap.Windows, "spend-control")
	if sc == nil {
		t.Fatal("missing spend-control window")
	}
	if sc.Used == nil || *sc.Used != 7761 {
		t.Fatalf("spend-control used = %v, want 7761", sc.Used)
	}
	if sc.Limit == nil || *sc.Limit != 100000 {
		t.Fatalf("spend-control limit = %v, want 100000", sc.Limit)
	}
	wantPct := 100 - 92.239 // remaining_percent 92.239 → 7.761% used
	if sc.UsagePercent == nil || !floatClose(*sc.UsagePercent, wantPct) {
		t.Fatalf("spend-control usage_percent = %v, want %v", sc.UsagePercent, wantPct)
	}
	if sc.ResetAt == nil || !sc.ResetAt.Equal(time.Unix(1782864000, 0).UTC()) {
		t.Fatalf("spend-control reset_at = %v", sc.ResetAt)
	}

	// Derived class: the most-used window is weekly at 43% → remaining 0.57 → normal.
	if got := snap.Class(); got != ClassNormal {
		t.Fatalf("class = %s, want normal", got)
	}
}

func TestCodexMinimalParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexMinimalBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}
	if snap.Availability != QuotaAvailable {
		t.Fatalf("availability = %s, want available", snap.Availability)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(snap.Windows))
	}
	if findWindow(snap.Windows, "session") == nil {
		t.Fatal("missing session window")
	}
}

func TestCodexExhaustedParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexExhaustedBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Availability != QuotaUnavailable {
		t.Fatalf("availability = %s, want unavailable", snap.Availability)
	}
	if got := snap.Class(); got != ClassExhausted {
		t.Fatalf("class = %s, want exhausted", got)
	}
}

func TestCodexPartialParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexPartialBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Primary window survives; the malformed secondary is skipped.
	if snap.Status != SourcePartial {
		t.Fatalf("status = %s, want partial", snap.Status)
	}
	if findWindow(snap.Windows, "session") == nil {
		t.Fatal("primary session window must survive")
	}
	if findWindow(snap.Windows, "weekly") != nil {
		t.Fatal("malformed secondary window must be skipped")
	}
	// Primary at 22% → still available.
	if snap.Availability != QuotaAvailable {
		t.Fatalf("availability = %s, want available", snap.Availability)
	}
}

func TestCodexAdditionalLimitsParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexAdditionalBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}
	// Named extra windows present.
	spark := findWindow(snap.Windows, "GPT-5.3-Codex-Spark")
	if spark == nil {
		t.Fatal("missing named additional window GPT-5.3-Codex-Spark")
	}
	if spark.UsagePercent == nil || *spark.UsagePercent != 15 {
		t.Fatalf("spark usage_percent = %v, want 15", spark.UsagePercent)
	}
	if findWindow(snap.Windows, "GPT-5.3-Codex-Spark-weekly") == nil {
		t.Fatal("missing named additional secondary window")
	}
	if findWindow(snap.Windows, "Another-Model") == nil {
		t.Fatal("missing named additional window Another-Model")
	}
}

func TestCodexAdditionalBadSibling(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexBadSiblingBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One bad entry never discards siblings: both good named windows survive.
	if findWindow(snap.Windows, "Good-One") == nil {
		t.Fatal("Good-One sibling must survive a bad entry")
	}
	if findWindow(snap.Windows, "Good-Three") == nil {
		t.Fatal("Good-Three sibling must survive a bad entry")
	}
	// The malformed entry downgrades the snapshot to partial.
	if snap.Status != SourcePartial {
		t.Fatalf("status = %s, want partial", snap.Status)
	}
}

func TestCodexIndividualLimitPrecedence(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(codexPrecedenceBody))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sc := findWindow(snap.Windows, "spend-control")
	if sc == nil {
		t.Fatal("missing spend-control window")
	}
	// Top-level individual_limit (limit 222) wins over rate_limit.individual_limit (limit 111).
	if sc.Limit == nil || *sc.Limit != 222 {
		t.Fatalf("spend-control limit = %v, want 222 (top-level precedence)", sc.Limit)
	}
	if sc.ResetAt == nil || !sc.ResetAt.Equal(time.Unix(1800000000, 0).UTC()) {
		t.Fatalf("spend-control reset_at = %v, want top-level resets_at", sc.ResetAt)
	}
}

func TestCodexNoWindowsYieldsPartialUnknown(t *testing.T) {
	// Valid JSON but no recognizable windows.
	doer := &recordingDoer{resp: bodyResponse(200, []byte(`{"plan_type":"free","credits":{"has_credits":false}}`))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("windows = %d, want 0", len(snap.Windows))
	}
	if snap.Availability != QuotaUnknown {
		t.Fatalf("availability = %s, want unknown", snap.Availability)
	}
	if snap.Status != SourcePartial {
		t.Fatalf("status = %s, want partial", snap.Status)
	}
}

func TestCodexInvalidResponseBody(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte("not json at all"))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-JSON 200 body")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
}

// --- HTTP error behavior (sanitized, no retry) ----------------------------

func TestCodexAuthFailureNoRetry(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(http.StatusUnauthorized, []byte(`{"error":"bearer `+codexTestBearer+`"}`))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected auth-failure error for 401")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// No retry: exactly one request.
	if len(doer.calls) != 1 {
		t.Fatalf("transport calls = %d, want 1 (no retry)", len(doer.calls))
	}
	// Status code is reported; the raw body never is.
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention status 401: %s", err)
	}
	if strings.Contains(err.Error(), codexTestBearer) {
		t.Fatalf("auth error leaks body/token: %s", err)
	}
}

func TestCodexAuthFailure403(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(http.StatusForbidden, []byte("forbidden body"))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected auth-failure error for 403")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention status 403: %s", err)
	}
	if strings.Contains(err.Error(), "forbidden body") {
		t.Fatalf("server error must not include raw body: %s", err)
	}
}

func TestCodexServerErrorSanitized(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(http.StatusInternalServerError, []byte("upstream boom detail"))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected server error for 500")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status 500: %s", err)
	}
	if strings.Contains(err.Error(), "upstream boom detail") {
		t.Fatalf("server error must not include raw body: %s", err)
	}
}

func TestCodexNetworkErrorSanitized(t *testing.T) {
	doer := &recordingDoer{err: errors.New(`Get "https://chatgpt.com/backend-api/wham/usage": dial tcp: connection refused`)}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected network error")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// The URL host is not a secret, but the error should be sanitized and bounded.
	if !strings.Contains(err.Error(), "bounded http") && !strings.Contains(err.Error(), "connection") {
		t.Fatalf("network error unexpected: %s", err)
	}
}

// --- Redaction: no secrets anywhere ---------------------------------------

func TestCodexRedactionSuccess(t *testing.T) {
	// The response body carries an ignored field containing a fake secret; it
	// must never surface in the snapshot.
	const bodySecret = "BODY-SECRET-9f8e7d"
	body := `{"rate_limit":{"primary_window":{"used_percent":22,"reset_at":1766948068}},"user_id":"` + bodySecret + `"}`
	doer := &recordingDoer{resp: bodyResponse(200, []byte(body))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	surface := snapshotSecretSurface(snap)
	for _, secret := range []string{codexTestBearer, codexTestAccount, bodySecret} {
		if strings.Contains(surface, secret) {
			t.Fatalf("snapshot surface leaks secret %q: %s", secret, surface)
		}
	}
}

func TestCodexRedactionAuthFailure(t *testing.T) {
	// Body deliberately echoes the token; the auth-failure error must not.
	body := `{"detail":"token ` + codexTestBearer + ` invalid"}`
	doer := &recordingDoer{resp: bodyResponse(http.StatusUnauthorized, []byte(body))}
	src := freshCodex(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, surface := range []string{err.Error(), snap.Error, snapshotSecretSurface(snap)} {
		for _, secret := range []string{codexTestBearer, codexTestAccount, "token invalid"} {
			if strings.Contains(surface, secret) {
				t.Fatalf("surface leaks secret %q: %s", secret, surface)
			}
		}
	}
}

func TestCodexRedactionCredentialError(t *testing.T) {
	// A resolver error and a malformed auth file containing a secret must both
	// produce generic diagnostics with no secret value.
	malformedAuth := `{"tokens":{"access_token":"` + codexTestBearer + `"}` // unclosed outer brace
	cases := []struct {
		name string
		r    CredentialResolver
	}{
		{"resolver error", &fakeResolver{err: errors.New("open " + codexTestBearer + ": no such file")}},
		{"malformed json with secret", &fakeResolver{val: malformedAuth}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &recordingDoer{resp: bodyResponse(200, []byte(codexProBody))}
			src := codexTestSource(t, doer, tc.r, CodexEvidence(codexTestNow))

			snap, err := src.Fetch(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			for _, surface := range []string{err.Error(), snap.Error} {
				if strings.Contains(surface, codexTestBearer) {
					t.Fatalf("credential error leaks token: %s", surface)
				}
			}
		})
	}
}

// --- Evidence record sanity -----------------------------------------------

func TestCodexEvidenceShape(t *testing.T) {
	e := CodexEvidence(codexTestNow)
	if e.Provider != "codex" {
		t.Fatalf("provider = %q", e.Provider)
	}
	if e.Endpoint != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("endpoint = %q", e.Endpoint)
	}
	if e.Method != "GET" {
		t.Fatalf("method = %q", e.Method)
	}
	if e.AuthType != "oauth-bearer" {
		t.Fatalf("auth type = %q", e.AuthType)
	}
	if e.FixturePath != "contract/testdata/quota/codex/pro.json" {
		t.Fatalf("fixture path = %q", e.FixturePath)
	}
	if !e.RecordedAt.Equal(codexTestNow) {
		t.Fatalf("recorded at = %v", e.RecordedAt)
	}
	if want := codexTestNow.AddDate(1, 0, 0); !e.ReviewBy.Equal(want) {
		t.Fatalf("review by = %v, want %v", e.ReviewBy, want)
	}
	// Evidence is fresh at its own recorded time and unsupported when empty.
	if EvaluateEvidence(&e, codexTestNow).State != EvidenceFresh {
		t.Fatal("CodexEvidence should be fresh at recorded time")
	}
	// Zero evidence is incomplete, never fresh.
	if got := EvaluateEvidence(&Evidence{}, codexTestNow).State; got == EvidenceFresh {
		t.Fatalf("zero evidence must not be fresh, got %s", got)
	}
}

// TestFlexFloatRejectsNonFinite proves quoted NaN/Inf strings (which
// strconv.ParseFloat accepts) and JSON-decoded non-finite values are rejected
// rather than flowing into windows, ranking arithmetic, and state JSON.
func TestFlexFloatRejectsNonFinite(t *testing.T) {
	for _, raw := range []string{`"NaN"`, `"Inf"`, `"-Inf"`, `"+Inf"`, `"nan"`, `"infinity"`} {
		if v, err := flexFloat([]byte(raw)); err == nil {
			t.Fatalf("flexFloat(%s) accepted non-finite value %v", raw, v)
		}
	}
	// Finite values still decode from both shapes.
	if v, err := flexFloat([]byte(`12.5`)); err != nil || v != 12.5 {
		t.Fatalf("number: v=%v err=%v", v, err)
	}
	if v, err := flexFloat([]byte(`" 12.5 "`)); err != nil || v != 12.5 {
		t.Fatalf("quoted: v=%v err=%v", v, err)
	}
}
