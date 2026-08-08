package quota

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- Test helpers -----------------------------------------------------------

// anthropicTestNow is a stable observation/evidence clock for deterministic
// tests: mid-month so the month window is unambiguous.
var anthropicTestNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// A distinctive synthetic ADMIN key so redaction assertions can detect any leak.
const anthropicTestAdminKey = "synthetic-anthropic-admin-AbCd1234"

// anthropicTestBudget is the monthly budget used across tests.
const anthropicTestBudget = 200.0

// anthropicTestSource builds an AnthropicSource with the given transport,
// credential resolver, and evidence (evaluated at anthropicTestNow). An empty
// Evidence.Provider leaves the registry empty (absent evidence).
func anthropicTestSource(t *testing.T, doer HTTPDoer, resolver CredentialResolver, ev Evidence) *AnthropicSource {
	t.Helper()
	reg := NewEvidenceRegistry()
	if ev.Provider != "" {
		reg.Register(ev)
	}
	return &AnthropicSource{
		mappingID:        "anthropic-test",
		Client:           &BoundedClient{Transport: doer, Timeout: time.Second, MaxBodyBytes: 1 << 20},
		Credentials:      resolver,
		Evidence:         reg,
		MonthlyBudgetUSD: anthropicTestBudget,
		Now:              func() time.Time { return anthropicTestNow },
	}
}

// freshAnthropic returns an AnthropicSource backed by doer + a synthetic admin
// key resolver, with fresh contract evidence registered.
func freshAnthropic(t *testing.T, doer HTTPDoer) *AnthropicSource {
	t.Helper()
	return anthropicTestSource(t, doer, &fakeResolver{val: anthropicTestAdminKey}, AnthropicEvidence(anthropicTestNow))
}

// costResponse builds a canned *http.Response with the given JSON body,
// reusing the package's shared bodyResponse helper.
func costResponse(status int, body string) *http.Response {
	return bodyResponse(status, []byte(body))
}

// costBody builds a single-page cost report with the given amount strings.
func costBody(amounts ...string) string {
	var items []string
	for _, a := range amounts {
		items = append(items, `{"amount":"`+a+`","currency":"USD","cost_type":"tokens","model":"claude-sonnet-4-5"}`)
	}
	return `{"data":[{"starting_at":"2026-08-01T00:00:00Z","ending_at":"2026-08-02T00:00:00Z","results":[` +
		strings.Join(items, ",") + `]}],"has_more":false,"next_page":null}`
}

// pagingDoer returns a different canned response per call.
type pagingDoer struct {
	responses []*http.Response
	calls     []*http.Request
}

func (d *pagingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls = append(d.calls, req.Clone(context.Background()))
	i := len(d.calls) - 1
	if i >= len(d.responses) {
		i = len(d.responses) - 1
	}
	return d.responses[i], nil
}

// --- Tests -------------------------------------------------------------------

func TestAnthropicUsesCanonicalAdminKeyEnvironmentName(t *testing.T) {
	resolver := &recordingZaiResolver{val: anthropicTestAdminKey}
	src := anthropicTestSource(t, &recordingDoer{}, resolver, AnthropicEvidence(anthropicTestNow))
	got, err := src.resolveCredentials()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != anthropicTestAdminKey {
		t.Fatalf("key = %q, want test key", got)
	}
	if resolver.ref.Kind != CredentialEnv || resolver.ref.Locator != "ANTHROPIC_ADMIN_API_KEY" {
		t.Fatalf("credential ref = %#v, want env ANTHROPIC_ADMIN_API_KEY", resolver.ref)
	}
}

// TestAnthropicMidMonthSpend proves a cost report summing to mid-budget spend
// yields one fresh monthly window with the right used/limit/percent, a reset
// at the first of the next month, and the documented request contract
// (x-api-key + anthropic-version + starting_at at the month boundary).
func TestAnthropicMidMonthSpend(t *testing.T) {
	doer := &recordingDoer{resp: costResponse(200, costBody("12050", "2950"))}
	src := freshAnthropic(t, doer)
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable {
		t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Name != "monthly" {
		t.Fatalf("windows=%+v", snap.Windows)
	}
	w := snap.Windows[0]
	if *w.Used != 150 || *w.Limit != anthropicTestBudget || *w.UsagePercent != 75 {
		t.Fatalf("window=%+v", w)
	}
	if w.ResetAt == nil || !w.ResetAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset=%+v want first of next month", w.ResetAt)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0.25 {
		t.Fatalf("effective remaining=%v want 0.25", rem)
	}
	req := doer.lastCall()
	if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.String(), anthropicCostReportBase) {
		t.Fatalf("request=%s %s", req.Method, req.URL)
	}
	if got := req.URL.Query().Get("starting_at"); got != "2026-08-01T00:00:00Z" {
		t.Fatalf("starting_at=%q want month start", got)
	}
	if req.Header.Get("x-api-key") != anthropicTestAdminKey {
		t.Fatal("x-api-key header not set")
	}
	if req.Header.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("anthropic-version=%q", req.Header.Get("anthropic-version"))
	}
}

// TestAnthropicEmptyMonthIsFreshAndFullyAvailable proves an empty cost report
// (no spend yet, or within the report's lag) is a valid zero-spend snapshot.
func TestAnthropicEmptyMonthIsFreshAndFullyAvailable(t *testing.T) {
	doer := &recordingDoer{resp: costResponse(200, `{"data":[],"has_more":false,"next_page":null}`)}
	snap, err := freshAnthropic(t, doer).Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable {
		t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
	}
	w := snap.Windows[0]
	if *w.Used != 0 || *w.UsagePercent != 0 {
		t.Fatalf("window=%+v want zero spend", w)
	}
}

// TestAnthropicOverBudgetIsUnavailable proves spend at or beyond the budget
// yields an exhausted (unavailable) observation with percent capped at 100.
func TestAnthropicOverBudgetIsUnavailable(t *testing.T) {
	doer := &recordingDoer{resp: costResponse(200, costBody("25000"))}
	snap, err := freshAnthropic(t, doer).Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Availability != QuotaUnavailable {
		t.Fatalf("availability=%s want unavailable", snap.Availability)
	}
	w := snap.Windows[0]
	if *w.UsagePercent != 100 {
		t.Fatalf("percent=%v want capped at 100", *w.UsagePercent)
	}
	// Used is deliberately not clamped: the true overspend is reported.
	if *w.Used != 250 {
		t.Fatalf("used=%v want 250", *w.Used)
	}
}

// TestAnthropicPagination proves has_more pages are followed and summed, with
// the page token forwarded.
func TestAnthropicPagination(t *testing.T) {
	page1 := `{"data":[{"results":[{"amount":"6000","currency":"USD"}]}],"has_more":true,"next_page":"page_two"}`
	page2 := `{"data":[{"results":[{"amount":"4000","currency":"USD"}]}],"has_more":false,"next_page":null}`
	doer := &pagingDoer{responses: []*http.Response{costResponse(200, page1), costResponse(200, page2)}}
	snap, err := freshAnthropic(t, doer).Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if *snap.Windows[0].Used != 100 {
		t.Fatalf("used=%v want 100 across pages", *snap.Windows[0].Used)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(doer.calls))
	}
	if got := doer.calls[1].URL.Query().Get("page"); got != "page_two" {
		t.Fatalf("second page token=%q", got)
	}
}

// TestAnthropicUnboundedPaginationFails proves a server that always reports
// has_more cannot hold a poll open past the page bound.
func TestAnthropicUnboundedPaginationFails(t *testing.T) {
	loop := `{"data":[{"results":[{"amount":"100","currency":"USD"}]}],"has_more":true,"next_page":"again"}`
	doer := &pagingDoer{responses: []*http.Response{costResponse(200, loop)}}
	if _, err := freshAnthropic(t, doer).Fetch(context.Background()); err == nil {
		t.Fatal("unbounded pagination did not fail")
	}
}

// TestAnthropicSkipsUnusableEntriesAsPartial proves malformed amounts and
// non-USD currencies are skipped with a partial downgrade rather than failing
// or inventing values.
func TestAnthropicSkipsUnusableEntriesAsPartial(t *testing.T) {
	body := `{"data":[{"results":[
		{"amount":"5000","currency":"USD"},
		{"amount":"nonsense","currency":"USD"},
		{"amount":"999","currency":"EUR"},
		{"amount":"-1000","currency":"USD"}
	]}],"has_more":false,"next_page":null}`
	doer := &recordingDoer{resp: costResponse(200, body)}
	snap, err := freshAnthropic(t, doer).Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Status != SourcePartial {
		t.Fatalf("status=%s want partial", snap.Status)
	}
	if *snap.Windows[0].Used != 50 {
		t.Fatalf("used=%v want 50 (only the clean USD entry)", *snap.Windows[0].Used)
	}
}

// TestAnthropicAuthFailure proves 401/403 fail with an actionable admin-key
// message that never contains the key.
func TestAnthropicAuthFailure(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		doer := &recordingDoer{resp: costResponse(code, `{"type":"error"}`)}
		snap, err := freshAnthropic(t, doer).Fetch(context.Background())
		if err == nil {
			t.Fatalf("HTTP %d did not fail", code)
		}
		if snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
			t.Fatalf("snapshot=%+v", snap)
		}
		if !strings.Contains(err.Error(), "ANTHROPIC_ADMIN_API_KEY") || !strings.Contains(err.Error(), "ADMIN") {
			t.Fatalf("error not actionable: %v", err)
		}
		if strings.Contains(err.Error(), anthropicTestAdminKey) || strings.Contains(snap.Error, anthropicTestAdminKey) {
			t.Fatal("admin key leaked into error")
		}
	}
}

// TestAnthropicMissingBudgetFailsClosed proves an unset budget is unsupported:
// no request is made and the reason names the config field.
func TestAnthropicMissingBudgetFailsClosed(t *testing.T) {
	doer := &recordingDoer{}
	src := freshAnthropic(t, doer)
	src.MonthlyBudgetUSD = 0
	if st := src.Status(); st.Supported || !strings.Contains(st.Reason, "monthly_budget_usd") {
		t.Fatalf("status=%+v", st)
	}
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("fetch succeeded without a budget")
	}
	if len(doer.calls) != 0 {
		t.Fatalf("HTTP request made despite missing budget: %d calls", len(doer.calls))
	}
}

// TestAnthropicEvidenceGateFailsClosed proves absent evidence blocks the fetch
// before any HTTP request is made.
func TestAnthropicEvidenceGateFailsClosed(t *testing.T) {
	doer := &recordingDoer{}
	src := anthropicTestSource(t, doer, &fakeResolver{val: anthropicTestAdminKey}, Evidence{})
	if st := src.Status(); st.Supported {
		t.Fatal("absent evidence reported supported")
	}
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("fetch succeeded without evidence")
	}
	if len(doer.calls) != 0 {
		t.Fatalf("HTTP request made despite failed gate: %d calls", len(doer.calls))
	}
}

func TestAnthropicMalformedEnvelopeFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"error":"temporary"}`, `{"data":[{}],"has_more":false}`, `{"data":[{"results":null}],"has_more":false}`, `{"data":null,"has_more":false}`, `{"data":[],"has_more":null}`} {
		doer := &recordingDoer{resp: costResponse(http.StatusOK, body)}
		snap, err := freshAnthropic(t, doer).Fetch(context.Background())
		if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
			t.Fatalf("body %s: snapshot=%+v err=%v; want failed unknown", body, snap, err)
		}
	}
}

func TestAnthropicHasMoreRequiresNextPage(t *testing.T) {
	doer := &recordingDoer{resp: costResponse(http.StatusOK, `{"data":[],"has_more":true,"next_page":null}`)}
	snap, err := freshAnthropic(t, doer).Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed {
		t.Fatalf("snapshot=%+v err=%v; want failed missing pagination token", snap, err)
	}
}

func TestAnthropicRequiresExplicitUSDCurrency(t *testing.T) {
	body := `{"data":[{"results":[{"amount":"1000"}]}],"has_more":false,"next_page":null}`
	snap, err := freshAnthropic(t, &recordingDoer{resp: costResponse(http.StatusOK, body)}).Fetch(context.Background())
	if err != nil || snap.Status != SourcePartial || *snap.Windows[0].Used != 0 {
		t.Fatalf("snapshot=%+v err=%v; want partial zero spend", snap, err)
	}
}

func TestAnthropicNonFiniteBudgetFailsClosed(t *testing.T) {
	for _, budget := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		src := freshAnthropic(t, &recordingDoer{resp: costResponse(http.StatusOK, costBody("100"))})
		src.MonthlyBudgetUSD = budget
		snap, err := src.Fetch(context.Background())
		if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
			t.Fatalf("budget=%v: snapshot=%+v err=%v; want failed unknown", budget, snap, err)
		}
	}
}
