package quota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---------------------------------------------------------

// zaiTestNow is a stable observation/evidence clock for deterministic tests.
var zaiTestNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// A distinctive synthetic API key so redaction assertions can detect any leak.
const zaiTestKey = "synthetic-zai-key-AbCd1234"

// zaiTestSource builds a ZaiSource with the given transport, credential
// resolver, and evidence (evaluated at zaiTestNow). An empty Evidence.Provider
// leaves the registry empty (absent evidence).
func zaiTestSource(t *testing.T, doer HTTPDoer, resolver CredentialResolver, ev Evidence) *ZaiSource {
	t.Helper()
	reg := NewEvidenceRegistry()
	if ev.Provider != "" {
		reg.Register(ev)
	}
	return &ZaiSource{
		mappingID:   "zai-test",
		Client:      &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 20},
		Credentials: resolver,
		Evidence:    reg,
		Now:         func() time.Time { return zaiTestNow },
	}
}

// freshZai returns a ZaiSource backed by doer + a synthetic key resolver, with
// fresh contract evidence registered.
func freshZai(t *testing.T, doer HTTPDoer) *ZaiSource {
	t.Helper()
	return zaiTestSource(t, doer, &fakeResolver{val: zaiTestKey}, ZaiEvidence(zaiTestNow))
}

type recordingZaiResolver struct {
	ref CredentialRef
	val string
}

func (r *recordingZaiResolver) Resolve(ref CredentialRef) (string, error) {
	r.ref = ref
	return r.val, nil
}

func TestZaiUsesCanonicalAPIKeyEnvironmentName(t *testing.T) {
	resolver := &recordingZaiResolver{val: zaiTestKey}
	src := zaiTestSource(t, &recordingDoer{}, resolver, ZaiEvidence(zaiTestNow))

	got, err := src.resolveCredentials()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != zaiTestKey {
		t.Fatalf("key = %q, want test key", got)
	}
	if resolver.ref.Kind != CredentialEnv || resolver.ref.Locator != "ZAI_API_KEY" {
		t.Fatalf("credential ref = %#v, want env ZAI_API_KEY", resolver.ref)
	}
}

// Inline response bodies matching the recorded contract shapes. The nextResetTime
// value (1768507567547) is the millisecond sample documented in the contract
// evidence; the numerics are placeholders.

const zaiProBody = `{
  "code": 200,
  "msg": "Operation successful",
  "success": true,
  "data": {
    "limits": [
      {"type":"TIME_LIMIT","unit":5,"number":1,"percentage":34,"usage":40000000,"currentValue":13628365,"remaining":26371635,"nextResetTime":1768507567547},
      {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":34,"usage":40000000,"currentValue":13628365,"remaining":26371635,"nextResetTime":1768507567547}
    ],
    "planName": "Pro"
  }
}`

const zaiBigmodelCNBody = `{
  "code": 200,
  "success": true,
  "data": {
    "limits": [
      {"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":40,"usage":10000000,"currentValue":4000000,"remaining":6000000},
      {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":20,"usage":5000000,"currentValue":1000000,"remaining":4000000},
      {"type":"TIME_LIMIT","unit":5,"number":1,"percentage":50,"nextResetTime":1768507567547}
    ],
    "level": "pro"
  }
}`

const zaiExhaustedBody = `{
  "code": 200,
  "msg": "Operation successful",
  "success": true,
  "data": {
    "limits": [
      {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":100,"usage":10000000,"currentValue":10000000,"remaining":0,"nextResetTime":1768507567547}
    ],
    "planName": "Pro"
  }
}`

const zaiMissingCountsBody = `{
  "code": 200,
  "msg": "Operation successful",
  "success": true,
  "data": {
    "limits": [
      {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42}
    ],
    "planName": "Pro"
  }
}`

const zaiAuthFailureBody = `{"code":1001,"msg":"Authorization Token Missing","success":false}`

const zaiEmptyLimitsBody = `{"code":200,"msg":"Operation successful","success":true,"data":{"limits":[],"planName":"Pro"}}`

// A valid token limit, a malformed (string) entry, then a valid MCP time limit:
// the two good siblings must survive and the snapshot downgrades to partial.
const zaiMalformedLimitBody = `{
  "code": 200,
  "success": true,
  "data": {
    "limits": [
      {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":10,"usage":1000,"currentValue":100,"remaining":900},
      "broken-entry",
      {"type":"TIME_LIMIT","unit":5,"number":1,"percentage":5}
    ]
  }
}`

// --- Evidence gate --------------------------------------------------------

func TestZaiStatusGatedByEvidence(t *testing.T) {
	// Fresh evidence → supported.
	fresh := freshZai(t, &recordingDoer{})
	if !fresh.Status().Supported {
		t.Fatal("fresh evidence should be supported")
	}

	// Absent evidence → unsupported.
	absent := zaiTestSource(t, &recordingDoer{}, &fakeResolver{val: zaiTestKey}, Evidence{})
	if absent.Status().Supported {
		t.Fatal("absent evidence should be unsupported")
	}

	// Expired evidence → unsupported.
	expired := ZaiEvidence(zaiTestNow)
	expired.ReviewBy = zaiTestNow.Add(-24 * time.Hour)
	exp := zaiTestSource(t, &recordingDoer{}, &fakeResolver{val: zaiTestKey}, expired)
	if exp.Status().Supported {
		t.Fatal("expired evidence should be unsupported")
	}
}

func TestZaiEvidenceGateAbsentFailsClosed(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := zaiTestSource(t, doer, &fakeResolver{val: zaiTestKey}, Evidence{}) // absent

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

func TestZaiEvidenceGateExpiredFailsClosed(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	expired := ZaiEvidence(zaiTestNow)
	expired.ReviewBy = zaiTestNow.Add(-24 * time.Hour)
	src := zaiTestSource(t, doer, &fakeResolver{val: zaiTestKey}, expired)

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

func TestZaiEvidenceFreshProceeds(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := freshZai(t, doer)

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

func TestZaiCredentialResolutionFailure(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := zaiTestSource(t, doer, &fakeResolver{err: errors.New("boom")}, ZaiEvidence(zaiTestNow))

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error when credentials cannot be resolved")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// Transport must never be reached when the key cannot be resolved.
	if len(doer.calls) != 0 {
		t.Fatalf("transport must not be called; got %d calls", len(doer.calls))
	}
	// The error must be a generic config diagnostic, never the key value.
	if strings.Contains(err.Error(), zaiTestKey) {
		t.Fatalf("credential error leaks key: %s", err)
	}
	if strings.Contains(snap.Error, zaiTestKey) {
		t.Fatalf("snapshot error leaks key: %s", snap.Error)
	}
}

func TestZaiRequestHeaders(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiMissingCountsBody))}
	src := freshZai(t, doer)

	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := doer.lastCall()
	if got.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", got.Method)
	}
	if got.URL.String() != "https://api.z.ai/api/monitor/usage/quota/limit" {
		t.Fatalf("url = %q", got.URL.String())
	}
	if got.Header.Get("Authorization") != "Bearer "+zaiTestKey {
		t.Fatalf("authorization header = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Accept") != "application/json" {
		t.Fatalf("accept header = %q", got.Header.Get("Accept"))
	}
}

func TestZaiRegionEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		region string
		want   string
	}{
		{"global default", "", "https://api.z.ai/api/monitor/usage/quota/limit"},
		{"global explicit", "global", "https://api.z.ai/api/monitor/usage/quota/limit"},
		{"bigmodel-cn", "bigmodel-cn", "https://open.bigmodel.cn/api/monitor/usage/quota/limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiMissingCountsBody))}
			reg := NewEvidenceRegistry()
			reg.Register(ZaiEvidence(zaiTestNow))
			src := &ZaiSource{
				mappingID:   "zai-test",
				Client:      &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 20},
				Credentials: &fakeResolver{val: zaiTestKey},
				Evidence:    reg,
				Region:      tc.region,
				Now:         func() time.Time { return zaiTestNow },
			}
			if _, err := src.Fetch(context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := doer.lastCall().URL.String(); got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Parsing: slotting, percentage, millis resets -------------------------

func TestZaiProParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := freshZai(t, doer)

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
	if !snap.CheckedAt.Equal(zaiTestNow) {
		t.Fatalf("checked-at = %v, want %v", snap.CheckedAt, zaiTestNow)
	}

	// One TOKENS_LIMIT (5-hour) → "primary". One TIME_LIMIT (MCP) → "monthly".
	primary := findWindow(snap.Windows, "primary")
	if primary == nil {
		t.Fatal("missing primary token window")
	}
	monthly := findWindow(snap.Windows, "monthly")
	if monthly == nil {
		t.Fatal("missing monthly (MCP) time window")
	}

	// Raw counts present → used/limit derived from currentValue/usage.
	if primary.Used == nil || *primary.Used != 13628365 {
		t.Fatalf("primary used = %v, want 13628365", primary.Used)
	}
	if primary.Limit == nil || *primary.Limit != 40000000 {
		t.Fatalf("primary limit = %v, want 40000000", primary.Limit)
	}
	// UsagePercent derived from counts (clamped 0..100), not the server value.
	wantPct := 100.0 * 13628365 / 40000000
	if primary.UsagePercent == nil || !floatClose(*primary.UsagePercent, wantPct) {
		t.Fatalf("primary usage_percent = %v, want %v", primary.UsagePercent, wantPct)
	}
	// ResetAt converted from epoch MILLISECONDS.
	wantReset := millisToTime(1768507567547)
	if primary.ResetAt == nil || !primary.ResetAt.Equal(wantReset) {
		t.Fatalf("primary reset_at = %v, want %v", primary.ResetAt, wantReset)
	}
	// Monthly window shares the same counts.
	if monthly.Used == nil || *monthly.Used != 13628365 {
		t.Fatalf("monthly used = %v, want 13628365", monthly.Used)
	}
	if monthly.ResetAt == nil || !monthly.ResetAt.Equal(wantReset) {
		t.Fatalf("monthly reset_at = %v, want %v", monthly.ResetAt, wantReset)
	}
}

func TestZaiPeriodCapture(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Primary TOKENS_LIMIT: unit=3 (hours), number=5 → 5h.
	primary := findWindow(snap.Windows, "primary")
	if primary == nil {
		t.Fatal("missing primary window")
	}
	want := 5 * time.Hour
	if primary.Period == nil || *primary.Period != want {
		t.Fatalf("primary period = %v, want %v", primary.Period, want)
	}

	// Monthly TIME_LIMIT: unit=5 (minutes), number=1 → 1m.
	monthly := findWindow(snap.Windows, "monthly")
	if monthly == nil {
		t.Fatal("missing monthly window")
	}
	want = 1 * time.Minute
	if monthly.Period == nil || *monthly.Period != want {
		t.Fatalf("monthly period = %v, want %v", monthly.Period, want)
	}
}

func TestZaiBigmodelCNParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiBigmodelCNBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Status != SourceFresh {
		t.Fatalf("status = %s, want fresh", snap.Status)
	}

	// Two TOKENS_LIMIT: weekly (longest) → primary, 5-hour (shortest) → session.
	primary := findWindow(snap.Windows, "primary")
	if primary == nil {
		t.Fatal("missing primary (weekly) token window")
	}
	if primary.Limit == nil || *primary.Limit != 10000000 {
		t.Fatalf("primary limit = %v, want 10000000", primary.Limit)
	}
	if primary.UsagePercent == nil || !floatClose(*primary.UsagePercent, 40) {
		t.Fatalf("primary usage_percent = %v, want 40", primary.UsagePercent)
	}
	session := findWindow(snap.Windows, "session")
	if session == nil {
		t.Fatal("missing session (5h) token window")
	}
	if session.Limit == nil || *session.Limit != 5000000 {
		t.Fatalf("session limit = %v, want 5000000", session.Limit)
	}
	if session.UsagePercent == nil || !floatClose(*session.UsagePercent, 20) {
		t.Fatalf("session usage_percent = %v, want 20", session.UsagePercent)
	}
	// TIME_LIMIT MCP marker → monthly.
	if findWindow(snap.Windows, "monthly") == nil {
		t.Fatal("missing monthly (MCP) window")
	}
}

func TestZaiExhaustedParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiExhaustedBody))}
	src := freshZai(t, doer)

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

func TestZaiMissingCountsParse(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiMissingCountsBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	primary := findWindow(snap.Windows, "primary")
	if primary == nil {
		t.Fatal("missing primary token window")
	}
	// No raw counts → UsagePercent falls back to server percentage; Used/Limit nil.
	if primary.UsagePercent == nil || *primary.UsagePercent != 42 {
		t.Fatalf("primary usage_percent = %v, want 42 (server percentage)", primary.UsagePercent)
	}
	if primary.Used != nil {
		t.Fatalf("primary used should be nil when counts absent, got %v", *primary.Used)
	}
	if primary.Limit != nil {
		t.Fatalf("primary limit should be nil when counts absent, got %v", *primary.Limit)
	}
}

func TestZaiEmptyLimitsYieldsPartialUnknown(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiEmptyLimitsBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty limits is a valid, not-an-error response.
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

func TestZaiMalformedLimitSkipsSibling(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiMalformedLimitBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The malformed entry is skipped; both good siblings survive.
	if findWindow(snap.Windows, "primary") == nil {
		t.Fatal("valid token limit must survive a malformed sibling")
	}
	if findWindow(snap.Windows, "monthly") == nil {
		t.Fatal("valid time limit must survive a malformed sibling")
	}
	if snap.Status != SourcePartial {
		t.Fatalf("status = %s, want partial (one limit malformed)", snap.Status)
	}
}

// --- Envelope / error behavior (sanitized, no retry) ----------------------

func TestZaiAuthFailureFromEnvelope(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiAuthFailureBody))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected auth-failure error for envelope code 1001")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// No retry: exactly one request.
	if len(doer.calls) != 1 {
		t.Fatalf("transport calls = %d, want 1 (no retry)", len(doer.calls))
	}
	// Sanitized: the diagnostic mentions auth/code, never the key or raw body.
	if !strings.Contains(err.Error(), "auth") {
		t.Fatalf("error should mention auth: %s", err)
	}
	if strings.Contains(err.Error(), zaiTestKey) {
		t.Fatalf("auth error leaks key: %s", err)
	}
}

func TestZaiEmptyBodyParseError(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte{})}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected parse error for empty 200 body")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if !strings.Contains(err.Error(), "empty response body") {
		t.Fatalf("error should mention empty body: %s", err)
	}
	if !strings.Contains(err.Error(), "region") {
		t.Fatalf("error should hint at region/token: %s", err)
	}
}

func TestZaiMissingDataParseError(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte(`{"code":200,"success":true}`))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected parse error for missing data field")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
}

func TestZaiInvalidResponseBody(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(200, []byte("not json at all"))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-JSON 200 body")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
}

func TestZaiServerErrorSanitized(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(http.StatusInternalServerError, []byte("upstream boom detail"))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected server error for 500")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	// Status code is reported; the raw body never is.
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status 500: %s", err)
	}
	if strings.Contains(err.Error(), "upstream boom detail") {
		t.Fatalf("server error must not include raw body: %s", err)
	}
	// No retry on a non-200.
	if len(doer.calls) != 1 {
		t.Fatalf("transport calls = %d, want 1 (no retry)", len(doer.calls))
	}
}

func TestZaiServerError429NoRetry(t *testing.T) {
	doer := &recordingDoer{resp: bodyResponse(http.StatusTooManyRequests, []byte(`{"detail":"rate limit"}`))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected server error for 429")
	}
	if snap.Status != SourceFailed {
		t.Fatalf("status = %s, want failed", snap.Status)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("error should mention status 429: %s", err)
	}
	// No retry on 429.
	if len(doer.calls) != 1 {
		t.Fatalf("transport calls = %d, want 1 (no retry on 429)", len(doer.calls))
	}
}

// --- Redaction: no secrets anywhere ---------------------------------------

func TestZaiRedactionSuccess(t *testing.T) {
	// The response body carries an ignored field containing a fake secret; it
	// must never surface in the snapshot.
	const bodySecret = "BODY-SECRET-9f8e7d"
	body := `{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":10,"usage":1000,"currentValue":100,"account":"` + bodySecret + `"}]}}`
	doer := &recordingDoer{resp: bodyResponse(200, []byte(body))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	surface := snapshotSecretSurface(snap)
	for _, secret := range []string{zaiTestKey, bodySecret} {
		if strings.Contains(surface, secret) {
			t.Fatalf("snapshot surface leaks secret %q: %s", secret, surface)
		}
	}
}

func TestZaiRedactionAuthFailure(t *testing.T) {
	// Body deliberately echoes the key; the auth-failure error must not.
	body := `{"code":1001,"msg":"token ` + zaiTestKey + ` invalid","success":false}`
	doer := &recordingDoer{resp: bodyResponse(200, []byte(body))}
	src := freshZai(t, doer)

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, surface := range []string{err.Error(), snap.Error, snapshotSecretSurface(snap)} {
		if strings.Contains(surface, zaiTestKey) {
			t.Fatalf("surface leaks key: %s", surface)
		}
	}
}

func TestZaiRedactionCredentialError(t *testing.T) {
	// A resolver error echoing the key must produce a generic diagnostic.
	doer := &recordingDoer{resp: bodyResponse(200, []byte(zaiProBody))}
	src := zaiTestSource(t, doer, &fakeResolver{err: errors.New("env ZAI_API_KEY=" + zaiTestKey)}, ZaiEvidence(zaiTestNow))

	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, surface := range []string{err.Error(), snap.Error} {
		if strings.Contains(surface, zaiTestKey) {
			t.Fatalf("credential error leaks key: %s", surface)
		}
	}
}

// --- Evidence record sanity -----------------------------------------------

func TestZaiEvidenceShape(t *testing.T) {
	e := ZaiEvidence(zaiTestNow)
	if e.Provider != "zai" {
		t.Fatalf("provider = %q", e.Provider)
	}
	if e.Endpoint != "https://api.z.ai/api/monitor/usage/quota/limit" {
		t.Fatalf("endpoint = %q", e.Endpoint)
	}
	if e.Method != "GET" {
		t.Fatalf("method = %q", e.Method)
	}
	if e.AuthType != "api-key" {
		t.Fatalf("auth type = %q", e.AuthType)
	}
	if e.FixturePath != "contract/testdata/quota/zai/pro.json" {
		t.Fatalf("fixture path = %q", e.FixturePath)
	}
	if !e.RecordedAt.Equal(evidenceRecordedAt()) {
		t.Fatalf("recorded at = %v", e.RecordedAt)
	}
	// Quarterly review per evidence policy.
	if want := evidenceRecordedAt().AddDate(0, 3, 0); !e.ReviewBy.Equal(want) {
		t.Fatalf("review by = %v, want %v", e.ReviewBy, want)
	}
	// Evidence is fresh at its own recorded time and unsupported when empty.
	if EvaluateEvidence(&e, zaiTestNow).State != EvidenceFresh {
		t.Fatal("ZaiEvidence should be fresh at recorded time")
	}
	if got := EvaluateEvidence(&Evidence{}, zaiTestNow).State; got == EvidenceFresh {
		t.Fatalf("zero evidence must not be fresh, got %s", got)
	}
}

func TestZaiNewZaiSourceDoesNotRegisterEvidence(t *testing.T) {
	reg := NewEvidenceRegistry()
	src := NewZaiSource("zai-acct1", &BoundedClient{Transport: &recordingDoer{}}, &fakeResolver{val: zaiTestKey}, "", reg, zaiTestNow)
	if src.MappingID() != "zai-acct1" {
		t.Fatalf("mapping id = %q", src.MappingID())
	}
	if _, ok := reg.Get("zai"); ok {
		t.Fatal("constructor must not register release evidence")
	}
	if src.Status().Supported {
		t.Fatal("source should be unsupported without reviewed evidence")
	}
}
