package quota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var neuralwattTestNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

const neuralwattTestKey = "synthetic-neuralwatt-key-AbCd1234"

type neuralwattResolver struct {
	ref   CredentialRef
	value string
	err   error
}

func (r *neuralwattResolver) Resolve(ref CredentialRef) (string, error) {
	r.ref = ref
	if r.value != "" || r.err != nil {
		return r.value, r.err
	}
	return neuralwattTestKey, nil
}

func neuralwattTestSource(t *testing.T, body string, status int, evidence bool) (*NeuralwattSource, *recordingDoer) {
	t.Helper()
	reg := NewEvidenceRegistry()
	if evidence {
		reg.Register(NeuralwattEvidence(neuralwattTestNow))
	}
	doer := &recordingDoer{resp: bodyResponse(status, []byte(body))}
	return &NeuralwattSource{
		mappingID: "neuralwatt-test", Client: &BoundedClient{Transport: doer},
		Credentials: &neuralwattResolver{}, Evidence: reg,
		Now: func() time.Time { return neuralwattTestNow },
	}, doer
}

func TestNeuralwattEvidenceAndCredentialContract(t *testing.T) {
	ev := NeuralwattEvidence(neuralwattTestNow)
	if ev.Endpoint != neuralwattQuotaEndpoint || ev.Method != http.MethodGet || ev.FixturePath == "" {
		t.Fatalf("evidence=%+v", ev)
	}
	if !ev.RecordedAt.Equal(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)) || !ev.ReviewBy.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("evidence dates=%v/%v", ev.RecordedAt, ev.ReviewBy)
	}
	resolver := &neuralwattResolver{}
	src := &NeuralwattSource{mappingID: "test", Credentials: resolver, Evidence: NewEvidenceRegistry(), Now: func() time.Time { return neuralwattTestNow }}
	_ = src
	if resolver.ref != (CredentialRef{}) {
		t.Fatal("credential should not resolve during construction")
	}
}

func TestNeuralwattEvidenceDoesNotRenewOnConstruction(t *testing.T) {
	first := NeuralwattEvidence(time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC))
	later := NeuralwattEvidence(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if !first.RecordedAt.Equal(later.RecordedAt) || !first.ReviewBy.Equal(later.ReviewBy) {
		t.Fatalf("evidence dates changed on construction: first=%+v later=%+v", first, later)
	}
	status := EvaluateEvidence(&later, later.ReviewBy.Add(time.Minute))
	if status.State != EvidenceExpired {
		t.Fatalf("stale evidence state=%s want expired", status.State)
	}
}

func TestNeuralwattBalanceFetch(t *testing.T) {
	src, doer := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":72.5,"total_credits_usd":100,"credits_used_usd":27.5},"usage":{"current_month":{"cost_usd":27.5,"requests":12,"energy_kwh":0.75}},"limits":{"overage_limit_usd":null,"rate_limit_tier":"standard"},"subscription":null}`, http.StatusOK, true)
	resolver := src.Credentials.(*neuralwattResolver)
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable || len(snap.Windows) != 1 {
		t.Fatalf("snapshot=%+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "balance_usd" || *w.Used != 27.5 || *w.Limit != 100 {
		t.Fatalf("window=%+v", w)
	}
	req := doer.lastCall()
	if req.Method != http.MethodGet || req.URL.String() != neuralwattQuotaEndpoint || (req.Body != nil && req.Body != http.NoBody) || req.URL.RawQuery != "" {
		t.Fatalf("request=%s %s query=%q", req.Method, req.URL, req.URL.RawQuery)
	}
	if req.Header.Get("Authorization") != "Bearer "+neuralwattTestKey || req.Header.Get("Accept") != "application/json" || req.Header.Get("User-Agent") != "polytoken-quota" {
		t.Fatalf("request headers=%v", req.Header)
	}
	if resolver.ref.Kind != CredentialEnv || resolver.ref.Locator != neuralwattAPIKeyEnv {
		t.Fatalf("credential ref=%+v", resolver.ref)
	}
}

func TestNeuralwattKeyAllowanceTakesPrecedence(t *testing.T) {
	src, _ := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":1,"total_credits_usd":100},"key":{"allowance":{"period":"monthly","limit_usd":20,"spent_usd":5,"remaining_usd":15,"blocked":false}},"subscription":{"kwh_included":100,"kwh_used":90,"kwh_remaining":10}}`, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || len(snap.Windows) != 1 || snap.Windows[0].Name != "key_allowance" || *snap.Windows[0].Limit != 20 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestNeuralwattBlockedAndOverageFailClosedAsUnavailable(t *testing.T) {
	for name, body := range map[string]string{
		"blocked": `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"key":{"allowance":{"blocked":true}}}`,
		"overage": `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"subscription":{"in_overage":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Availability != QuotaUnavailable {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwattSubscriptionWindowAndNullableReset(t *testing.T) {
	src, _ := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"subscription":{"kwh_included":100,"kwh_used":25,"kwh_remaining":75,"current_period_end":"2026-09-01T00:00:00Z","in_overage":false}}`, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Windows[0].Name != "subscription_kwh" || *snap.Windows[0].Used != 25 || snap.Windows[0].ResetAt == nil {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestNeuralwattExhaustedBalanceIsUnavailable(t *testing.T) {
	src, _ := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":0,"total_credits_usd":100,"credits_used_usd":100},"subscription":null}`, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Availability != QuotaUnavailable || len(snap.Windows) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestNeuralwattContradictoryNumericValuesFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"key remaining exceeds limit":             `{"snapshot_at":"2026-08-15T12:00:00Z","key":{"allowance":{"limit_usd":20,"remaining_usd":25,"blocked":false}}}`,
		"key spent negative":                      `{"snapshot_at":"2026-08-15T12:00:00Z","key":{"allowance":{"limit_usd":20,"remaining_usd":15,"spent_usd":-1,"blocked":false}}}`,
		"subscription remaining exceeds included": `{"snapshot_at":"2026-08-15T12:00:00Z","subscription":{"kwh_included":100,"kwh_remaining":125,"in_overage":false}}`,
		"subscription used exceeds included":      `{"snapshot_at":"2026-08-15T12:00:00Z","subscription":{"kwh_included":100,"kwh_remaining":75,"kwh_used":125,"in_overage":false}}`,
		"balance used negative":                   `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100,"credits_used_usd":-1}}`,
		"balance used exceeds total":              `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100,"credits_used_usd":125}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwattMalformedSelectedAllowanceFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"key":          `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"key":{"allowance":{"limit_usd":20,"blocked":false}}}`,
		"subscription": `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100},"subscription":{"in_overage":false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || !strings.Contains(err.Error(), "allowance") {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if len(doer.calls) != 1 {
				t.Fatalf("calls=%d", len(doer.calls))
			}
		})
	}
}

func TestNeuralwattMissingAllowanceStateFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"key blocked state":          `{"snapshot_at":"2026-08-15T12:00:00Z","key":{"allowance":{"limit_usd":20,"remaining_usd":15}}}`,
		"subscription overage state": `{"snapshot_at":"2026-08-15T12:00:00Z","subscription":{"kwh_included":100,"kwh_remaining":75}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwattSnapshotTimestampValidation(t *testing.T) {
	bodies := map[string]string{
		"missing":   `{"balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`,
		"malformed": `{"snapshot_at":"not-a-time","balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`,
		"stale":     `{"snapshot_at":"2026-08-14T11:59:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`,
		"future":    `{"snapshot_at":"2026-08-15T12:10:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			src, _ := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if name == "stale" {
				want := time.Date(2026, 8, 14, 11, 59, 0, 0, time.UTC)
				if err != nil || snap.Status != SourceFresh || !snap.CheckedAt.Equal(want) {
					t.Fatalf("stale snapshot=%+v err=%v", snap, err)
				}
				return
			}
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwattQuotedKeyAndResolverFailures(t *testing.T) {
	src, doer := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`, http.StatusOK, true)
	resolver := src.Credentials.(*neuralwattResolver)
	resolver.value = `"` + neuralwattTestKey + `"`
	if _, err := src.Fetch(context.Background()); err != nil || doer.lastCall().Header.Get("Authorization") != "Bearer "+neuralwattTestKey {
		t.Fatalf("quoted key fetch err=%v headers=%v", err, doer.lastCall().Header)
	}
	for name, tc := range map[string]struct {
		value string
		err   error
	}{
		"blank":          {value: "   "},
		"resolver error": {err: errors.New("missing synthetic key")},
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","balance":{"credits_remaining_usd":50,"total_credits_usd":100}}`, http.StatusOK, true)
			r := src.Credentials.(*neuralwattResolver)
			r.value, r.err = tc.value, tc.err
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || len(doer.calls) != 0 {
				t.Fatalf("snapshot=%+v err=%v calls=%d", snap, err, len(doer.calls))
			}
		})
	}
}

func TestNeuralwattInvalidAndEmptyResponsesFailClosed(t *testing.T) {
	for name, body := range map[string]string{"invalid json": "{", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			src, _ := neuralwattTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwattInvalidSubscriptionPeriodIsPartial(t *testing.T) {
	src, _ := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z","subscription":{"kwh_included":100,"kwh_used":25,"kwh_remaining":75,"current_period_end":"bad","in_overage":false}}`, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourcePartial || len(snap.Windows) != 1 || snap.Windows[0].ResetAt != nil {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestNeuralwattNilConfigurationFailsClosed(t *testing.T) {
	reg := NewEvidenceRegistry()
	reg.Register(NeuralwattEvidence(neuralwattTestNow))
	for name, src := range map[string]*NeuralwattSource{
		"nil client":      {mappingID: "test", Credentials: &neuralwattResolver{}, Evidence: reg, Now: func() time.Time { return neuralwattTestNow }},
		"nil credentials": {mappingID: "test", Client: &BoundedClient{}, Evidence: reg, Now: func() time.Time { return neuralwattTestNow }},
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestNeuralwatt429Diagnostic(t *testing.T) {
	src, _ := neuralwattTestSource(t, `{"snapshot_at":"2026-08-15T12:00:00Z"}`, http.StatusTooManyRequests, true)
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "429") {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestNeuralwattEvidenceGateAndAuthFailure(t *testing.T) {
	src, doer := neuralwattTestSource(t, `{}`, http.StatusOK, false)
	if st := src.Status(); st.Supported {
		t.Fatal("absent evidence reported supported")
	}
	if _, err := src.Fetch(context.Background()); err == nil || len(doer.calls) != 0 {
		t.Fatalf("gate err=%v calls=%d", err, len(doer.calls))
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		src, _ = neuralwattTestSource(t, `{"error":"unauthorized"}`, status, true)
		snap, err := src.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), neuralwattAPIKeyEnv) || strings.Contains(err.Error(), neuralwattTestKey) || strings.Contains(snap.Error, neuralwattTestKey) {
			t.Fatalf("HTTP %d auth snapshot=%+v err=%v", status, snap, err)
		}
	}
}
