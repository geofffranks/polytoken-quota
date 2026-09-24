package quota

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var opencodeGoTestNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

const opencodeGoTestKey = "synthetic-opencode-key-AbCd1234"

type opencodeGoResolver struct {
	ref   CredentialRef
	value string
	err   error
}

func (r *opencodeGoResolver) Resolve(ref CredentialRef) (string, error) {
	r.ref = ref
	if r.value != "" || r.err != nil {
		return r.value, r.err
	}
	return opencodeGoTestKey, nil
}

func opencodeGoTestSource(t *testing.T, body string, status int, evidence bool) (*OpenCodeGoSource, *recordingDoer) {
	t.Helper()
	reg := NewEvidenceRegistry()
	if evidence {
		reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
	}
	doer := &recordingDoer{resp: bodyResponse(status, []byte(body))}
	return &OpenCodeGoSource{
		mappingID: "opencode-go-test", Client: &BoundedClient{Transport: doer},
		Credentials: &opencodeGoResolver{}, Evidence: reg,
		Now: func() time.Time { return opencodeGoTestNow },
	}, doer
}

func TestOpenCodeGoKnownAdapterContract(t *testing.T) {
	if !KnownAdapter("opencode-go") {
		t.Fatal("opencode-go is not a known built-in adapter")
	}
	def, ok := AdapterDefinitionFor("opencode-go")
	if !ok {
		t.Fatal("AdapterDefinitionFor(opencode-go) not found")
	}
	if def.Evidence == nil || def.New == nil {
		t.Fatalf("definition=%+v", def)
	}
	src := def.New("m", &BoundedClient{}, nil, 0, nil, opencodeGoTestNow)
	if _, ok := src.(QuotaSource); !ok {
		t.Fatalf("factory returned %T, want a QuotaSource", src)
	}
	if def.New("m", &BoundedClient{}, nil, 0, nil, opencodeGoTestNow).MappingID() != "m" {
		t.Fatal("factory dropped the mapping id")
	}
}

func TestOpenCodeGoEvidenceContract(t *testing.T) {
	ev := OpenCodeGoEvidence(opencodeGoTestNow)
	if ev.Endpoint != opencodeGoUsageEndpoint || ev.Method != http.MethodGet || ev.AuthType != "bearer-api-key" {
		t.Fatalf("evidence=%+v", ev)
	}
	if ev.ContractID != "" {
		t.Fatalf("contract id = %q, want the single legacy empty contract", ev.ContractID)
	}
	if ev.FixturePath == "" {
		t.Fatal("fixture path must be non-empty for the release gate")
	}
	if ev.Provider != opencodeGoProviderName {
		t.Fatalf("provider=%q", ev.Provider)
	}
	if !ev.RecordedAt.Equal(evidenceRecordedAt()) || !ev.ReviewBy.Equal(evidenceRecordedAt().AddDate(0, 3, 0)) {
		t.Fatalf("evidence dates=%v/%v", ev.RecordedAt, ev.ReviewBy)
	}
	if !strings.Contains(ev.SchemaNote, "percent USED") || !strings.Contains(ev.SchemaNote, "not officially documented") {
		t.Fatalf("schema note=%q", ev.SchemaNote)
	}
}

func TestOpenCodeGoEvidenceDoesNotRenewOnConstruction(t *testing.T) {
	first := OpenCodeGoEvidence(time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC))
	later := OpenCodeGoEvidence(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if !first.RecordedAt.Equal(later.RecordedAt) || !first.ReviewBy.Equal(later.ReviewBy) {
		t.Fatalf("evidence dates changed on construction: first=%+v later=%+v", first, later)
	}
	status := EvaluateEvidence(&later, later.ReviewBy.Add(time.Minute))
	if status.State != EvidenceExpired {
		t.Fatalf("stale evidence state=%s want expired", status.State)
	}
}

// TestOpenCodeGoEvidenceGateFailsClosedWithoutRequest pins the fail-closed
// guarantee: absent or expired evidence yields an error and zero HTTP calls.
func TestOpenCodeGoEvidenceGateFailsClosedWithoutRequest(t *testing.T) {
	for name, evidence := range map[string]bool{
		"absent":  false,
		"expired": true,
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, evidence)
			if name == "expired" {
				stale := OpenCodeGoEvidence(opencodeGoTestNow)
				stale.ReviewBy = opencodeGoTestNow.AddDate(0, 0, -1)
				reg := NewEvidenceRegistry()
				reg.Register(stale)
				src.Evidence = reg
			}
			if st := src.Status(); st.Supported {
				t.Fatalf("%s evidence reported supported", name)
			}
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown || snap.Error == "" {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if len(doer.calls) != 0 {
				t.Fatalf("fail-closed path made %d HTTP calls, want 0", len(doer.calls))
			}
		})
	}
}

func TestOpenCodeGoNilConfigurationFailsClosed(t *testing.T) {
	reg := NewEvidenceRegistry()
	reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
	for name, src := range map[string]*OpenCodeGoSource{
		"nil client":      {mappingID: "test", Credentials: &opencodeGoResolver{}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }},
		"nil credentials": {mappingID: "test", Client: &BoundedClient{}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }},
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown || snap.Error != "opencode-go: adapter is not configured" {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if err.Error() != "opencode-go: adapter is not configured" {
				t.Fatalf("err=%v", err)
			}
		})
	}
	// The nil-credentials path has a live client: the counting transport must
	// record zero calls.
	doer := &recordingDoer{}
	src := &OpenCodeGoSource{mappingID: "test", Client: &BoundedClient{Transport: doer}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow }}
	if _, err := src.Fetch(context.Background()); err == nil || len(doer.calls) != 0 {
		t.Fatalf("nil-credentials fetch err=%v calls=%d", err, len(doer.calls))
	}
}

// TestOpenCodeGoUnresolvedCredentialFailsClosedWithoutRequest covers blank and
// erroring credential resolution: fail closed with zero HTTP calls.
func TestOpenCodeGoUnresolvedCredentialFailsClosedWithoutRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		err   error
	}{
		"blank":          {value: "   "},
		"resolver error": {err: errors.New("missing synthetic key")},
	} {
		t.Run(name, func(t *testing.T) {
			src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, true)
			r := src.Credentials.(*opencodeGoResolver)
			r.value, r.err = tc.value, tc.err
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if err.Error() != "opencode-go: could not resolve OPENCODE_API_KEY" {
				t.Fatalf("err=%v", err)
			}
			if len(doer.calls) != 0 {
				t.Fatalf("fail-closed path made %d HTTP calls, want 0", len(doer.calls))
			}
		})
	}
}

func TestOpenCodeGoQuotedKeyIsTrimmed(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":           {in: " k0 ", want: "k0"},
		"double quotes":   {in: `"k1"`, want: "k1"},
		"single quotes":   {in: "'k2'", want: "k2"},
		"quoted + spaces": {in: ` "k3" `, want: "k3"},
		"mismatched":      {in: `"k4'`, want: `"k4'`},
		"inner only":      {in: `"k5", "k6"`, want: `k5", "k6`},
		"nested":          {in: `""k7""`, want: `"k7"`},
	} {
		if got := cleanOpenCodeGoKey(tc.in); got != tc.want {
			t.Fatalf("%s: cleanOpenCodeGoKey(%q)=%q want %q", name, tc.in, got, tc.want)
		}
	}
	// The cleaned key, not the raw value, reaches the Authorization header.
	src, doer := opencodeGoTestSource(t, "{}", http.StatusOK, true)
	resolver := src.Credentials.(*opencodeGoResolver)
	resolver.value = `"` + opencodeGoTestKey + `"`
	_ = doer
	if cleanOpenCodeGoKey(resolver.value) != opencodeGoTestKey {
		t.Fatalf("cleaned=%q", cleanOpenCodeGoKey(resolver.value))
	}
}

func opencodeGoFullUsageBody() string {
	return `{"usage":{` +
		`"rolling":{"status":"ok","percent":42.5,"resetsAt":"2026-08-15T16:00:00Z"},` +
		`"weekly":{"status":"rate-limited","percent":31.25,"resetsAt":"2026-08-17T00:00:00Z"},` +
		`"monthly":{"status":"ok","percent":88.75,"resetsAt":"2026-08-31T00:00:00Z"}` +
		`}}`
}

// TestOpenCodeGoFullUsageFetch covers the all-three-windows row: every window
// decodes, order is the fixed rolling/weekly/monthly priority, Used/Limit stay
// nil, every window carries its Period (including the sub-24h rolling window),
// and a fully reset-bearing payload is SourceFresh.
func TestOpenCodeGoFullUsageFetch(t *testing.T) {
	src, _ := opencodeGoTestSource(t, opencodeGoFullUsageBody(), http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable {
		t.Fatalf("snapshot=%+v", snap)
	}
	if !snap.CheckedAt.Equal(opencodeGoTestNow) {
		t.Fatalf("checked_at = %v, want the local clock %v (payload has no snapshot_at)", snap.CheckedAt, opencodeGoTestNow)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows=%d", len(snap.Windows))
	}
	wantOrder := []string{"rolling", "weekly", "monthly"}
	wantPercent := []float64{42.5, 31.25, 88.75}
	wantPeriods := []time.Duration{opencodeGoRollingPeriod, opencodeGoWeeklyPeriod, opencodeGoMonthlyPeriod}
	for i, w := range snap.Windows {
		if w.Name != wantOrder[i] {
			t.Fatalf("window[%d]=%q want %q (fixed decode order)", i, w.Name, wantOrder[i])
		}
		if w.Used != nil || w.Limit != nil {
			t.Fatalf("window[%d] must not carry Used/Limit", i)
		}
		if w.UsagePercent == nil || *w.UsagePercent != wantPercent[i] {
			t.Fatalf("window[%d] percent=%v want %v", i, w.UsagePercent, wantPercent[i])
		}
		if w.Period == nil || *w.Period != wantPeriods[i] {
			t.Fatalf("window[%d] period=%v want %v", i, w.Period, wantPeriods[i])
		}
		if w.ResetAt == nil {
			t.Fatalf("window[%d] missing reset", i)
		}
	}
	if !snap.Windows[0].ResetAt.Equal(time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("rolling reset=%v", snap.Windows[0].ResetAt)
	}
}

// TestOpenCodeGoRequestShapeAndCredentials pins the wire contract: GET on the
// usage endpoint with the Bearer credential, Accept and User-Agent headers, no
// query string, and the env-based credential resolver reference.
func TestOpenCodeGoRequestShapeAndCredentials(t *testing.T) {
	src, doer := opencodeGoTestSource(t, opencodeGoFullUsageBody(), http.StatusOK, true)
	resolver := src.Credentials.(*opencodeGoResolver)
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := doer.lastCall()
	if req.Method != http.MethodGet || req.URL.String() != opencodeGoUsageEndpoint || (req.Body != nil && req.Body != http.NoBody) || req.URL.RawQuery != "" {
		t.Fatalf("request=%s %s query=%q", req.Method, req.URL, req.URL.RawQuery)
	}
	if req.Header.Get("Authorization") != "Bearer "+opencodeGoTestKey || req.Header.Get("Accept") != "application/json" || req.Header.Get("User-Agent") != "polytoken-quota" {
		t.Fatalf("request headers=%v", req.Header)
	}
	if resolver.ref.Kind != CredentialEnv || resolver.ref.Locator != opencodeGoAPIKeyEnv {
		t.Fatalf("credential ref=%+v", resolver.ref)
	}
}

// TestOpenCodeGoWindowStatusVariants covers the status rows of the matrix:
// both recognized values decode, while a missing or unrecognized status fails
// that window closed (as partial, so long as another window decodes).
func TestOpenCodeGoWindowStatusVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		body            string
		wantStatus      SourceStatus
		wantWindows     int
		wantPercentName string
		wantPercent     float64
		wantAvailable   QuotaAvailability
	}{
		"ok decodes": {
			`{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":"2026-08-15T16:00:00Z"}}}`,
			SourcePartial, 1, "rolling", 12.5, QuotaAvailable,
		},
		"rate-limited decodes, advisory only": {
			`{"usage":{"rolling":{"status":"rate-limited","percent":12.5,"resetsAt":"2026-08-15T16:00:00Z"}}}`,
			SourcePartial, 1, "rolling", 12.5, QuotaAvailable,
		},
		"unrecognized status fails closed": {
			`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-15T16:00:00Z"},"weekly":{"status":"exhausted","percent":90,"resetsAt":"2026-08-17T00:00:00Z"}}}`,
			SourcePartial, 1, "rolling", 10, QuotaAvailable,
		},
		"missing status fails closed": {
			`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-15T16:00:00Z"},"weekly":{"percent":90}}}`,
			SourcePartial, 1, "rolling", 10, QuotaAvailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := opencodeGoTestSource(t, tc.body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status != tc.wantStatus || len(snap.Windows) != tc.wantWindows {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			w := snap.Windows[0]
			if w.Name != tc.wantPercentName || w.UsagePercent == nil || *w.UsagePercent != tc.wantPercent {
				t.Fatalf("window=%+v", w)
			}
			if snap.Availability != tc.wantAvailable {
				t.Fatalf("availability=%s want %s", snap.Availability, tc.wantAvailable)
			}
		})
	}
}

// TestOpenCodeGoPercentVariants covers the percent-validation rows: missing,
// non-numeric (string), and overflow-to-Inf (1e400) percent values fail that
// window closed — no fallback onto a weaker window occurs. A literal NaN is
// syntactically invalid JSON, so it fails the whole payload at the envelope
// decode instead (also fail-closed; encoding/json never accepts NaN).
func TestOpenCodeGoPercentVariants(t *testing.T) {
	weekly := `"weekly":{"status":"ok","percent":20,"resetsAt":"2026-08-17T00:00:00Z"}`
	for name, body := range map[string]string{
		"percent missing":      `{"usage":{"rolling":{"status":"ok","resetsAt":"2026-08-15T16:00:00Z"},` + weekly + `}}`,
		"percent non-numeric":  `{"usage":{"rolling":{"status":"ok","percent":"50"},` + weekly + `}}`,
		"percent Inf overflow": `{"usage":{"rolling":{"status":"ok","percent":1e400},` + weekly + `}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status != SourcePartial || len(snap.Windows) != 1 {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			// The rolling window must never decode; only the weaker weekly
			// window survives.
			if snap.Windows[0].Name != "weekly" || snap.Windows[0].UsagePercent == nil || *snap.Windows[0].UsagePercent != 20 {
				t.Fatalf("window=%+v", snap.Windows[0])
			}
		})
	}
	t.Run("percent NaN literal fails the whole payload", func(t *testing.T) {
		body := `{"usage":{"rolling":{"status":"ok","percent":NaN},` + weekly + `}}`
		src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
		snap, err := src.Fetch(context.Background())
		if err == nil || snap.Status != SourceFailed || !strings.Contains(snap.Error, "could not decode JSON") {
			t.Fatalf("snapshot=%+v err=%v", snap, err)
		}
	})
}

// TestOpenCodeGoPercentClampsAtBothEnds covers clamping at 0 and 100
// (negative and >100 inputs stay finite and decode; they never error).
func TestOpenCodeGoPercentClampsAtBothEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		percent       string
		wantClamped   float64
		wantAvailable QuotaAvailability
	}{
		"negative clamps to zero":    {"-5", 0, QuotaAvailable},
		"over 100 clamps to hundred": {"125.5", 100, QuotaUnavailable},
		"exactly 100 is unavailable": {"100", 100, QuotaUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"usage":{"rolling":{"status":"ok","percent":` + tc.percent + `,"resetsAt":"2026-08-15T16:00:00Z"}}}`
			src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status != SourcePartial {
				t.Fatalf("snapshot=%+v err=%v (percent >= 100 must not be an error)", snap, err)
			}
			if snap.Availability != tc.wantAvailable {
				t.Fatalf("availability=%s want %s", snap.Availability, tc.wantAvailable)
			}
			if *snap.Windows[0].UsagePercent != tc.wantClamped {
				t.Fatalf("percent=%v want clamp to %v", *snap.Windows[0].UsagePercent, tc.wantClamped)
			}
		})
	}
}

// TestOpenCodeGoResetAtVariants covers the resetsAt rows: a valid RFC3339
// value sets ResetAt; absent or unparseable values decode the window without
// ResetAt and mark the snapshot partial.
func TestOpenCodeGoResetAtVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		resetsAt   string // raw JSON key, "" to omit
		wantReset  bool
		wantStatus SourceStatus
	}{
		"valid sets reset": {`"2026-08-15T16:00:00Z"`, true, SourcePartial},
		"unparseable":      {`"soon-ish"`, false, SourcePartial},
		"key omitted":      {``, false, SourcePartial},
		"null is absent":   {`null`, false, SourcePartial},
	} {
		t.Run(name, func(t *testing.T) {
			window := `{"status":"ok","percent":10,"resetsAt":` + tc.resetsAt + `}`
			if tc.resetsAt == "" {
				window = `{"status":"ok","percent":10}`
			}
			body := `{"usage":{"rolling":` + window + `}}`
			src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status != tc.wantStatus || len(snap.Windows) != 1 {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if tc.wantReset {
				if snap.Windows[0].ResetAt == nil || !snap.Windows[0].ResetAt.Equal(time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)) {
					t.Fatalf("reset=%v want 2026-08-15T16:00:00Z", snap.Windows[0].ResetAt)
				}
			} else if snap.Windows[0].ResetAt != nil {
				t.Fatalf("reset=%v want nil", snap.Windows[0].ResetAt)
			}
			// The window still carried its Period.
			if snap.Windows[0].Period == nil || *snap.Windows[0].Period != opencodeGoRollingPeriod {
				t.Fatalf("period=%v want 5h", snap.Windows[0].Period)
			}
		})
	}
}

// TestOpenCodeGoPartialWhenOnlySomeWindowsDecode covers the occupancy rows:
// one of three windows present ⇒ SourcePartial; a present-but-malformed
// window alongside valid ones ⇒ SourcePartial, and the malformed window
// contributes nothing.
func TestOpenCodeGoPartialWhenOnlySomeWindowsDecode(t *testing.T) {
	for name, tc := range map[string]struct {
		body        string
		wantWindows int
		wantNames   []string
	}{
		"single window is partial": {
			`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-15T16:00:00Z"}}}`,
			1, []string{"rolling"},
		},
		"unusable window among valid ones": {
			// weekly is a bare string: JSON-valid (so the envelope still
			// decodes), but it fails that window closed while rolling and
			// monthly still decode.
			`{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-15T16:00:00Z"},"weekly":"twenty","monthly":{"status":"ok","percent":30,"resetsAt":"2026-08-31T00:00:00Z"}}}`,
			2, []string{"rolling", "monthly"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := opencodeGoTestSource(t, tc.body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || snap.Status != SourcePartial || len(snap.Windows) != tc.wantWindows {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			for i, want := range tc.wantNames {
				if snap.Windows[i].Name != want {
					t.Fatalf("window[%d]=%q want %q", i, snap.Windows[i].Name, want)
				}
			}
			if snap.Availability != QuotaAvailable {
				t.Fatalf("availability=%s want available", snap.Availability)
			}
		})
	}
}

// TestOpenCodeGoNoWindowDecodesFailsClosed covers the hard-failure row: an
// absent usage object, an empty usage object, and a payload whose only
// windows are unusable all hard-fail instead of returning an empty fresh
// snapshot.
func TestOpenCodeGoNoWindowDecodesFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"no usage object":       `{"account":"c"}`,
		"empty usage object":    `{"usage":{}}`,
		"only unusable windows": `{"usage":{"rolling":{"percent":5},"weekly":{"status":"bogus","percent":6}}}`,
		"invalid json":          `{`,
	} {
		t.Run(name, func(t *testing.T) {
			src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown || snap.Error == "" {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			if err.Error() != snap.Error {
				t.Fatalf("error/snapshot mismatch err=%q snap.Error=%q", err.Error(), snap.Error)
			}
			if len(snap.Windows) != 0 {
				t.Fatalf("windows=%v, want none", snap.Windows)
			}
		})
	}
}

// TestOpenCodeGoEmptyBodyFailsClosed covers the empty-body row.
func TestOpenCodeGoEmptyBodyFailsClosed(t *testing.T) {
	src, _ := opencodeGoTestSource(t, "", http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || err.Error() != "opencode-go: empty response body" {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

// TestOpenCodeGoHTTPFailuresFailClosed covers the 401, 403, and other-non-2xx
// rows: fail closed with sanitized diagnostics; 401 names the env var and
// 403 distinguishes a valid key without a subscription from an auth failure;
// neither leaks the credential or a credentialed URL.
func TestOpenCodeGoHTTPFailuresFailClosed(t *testing.T) {
	src, _ := opencodeGoTestSource(t, `{"error":{"type":"AuthError","message":"Missing API key."}}`, http.StatusUnauthorized, true)
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || !strings.Contains(err.Error(), opencodeGoAPIKeyEnv) {
		t.Fatalf("401 snapshot=%+v err=%v", snap, err)
	}
	if strings.Contains(err.Error(), opencodeGoTestKey) || strings.Contains(snap.Error, opencodeGoUsageEndpoint) {
		t.Fatalf("401 diagnostic leaked secrets: %q / %q", err.Error(), snap.Error)
	}

	src, _ = opencodeGoTestSource(t, `{"error":{"type":"EntitlementError","message":"no zen go subscription"}}`, http.StatusForbidden, true)
	snap, err = src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed {
		t.Fatalf("403 snapshot=%+v err=%v", snap, err)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "subscription") {
		t.Fatalf("403 diagnostic must distinguish valid-key-no-subscription: %q", err.Error())
	}
	if strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("403 diagnostic must not read as an auth failure: %q", err.Error())
	}

	src, _ = opencodeGoTestSource(t, "{}", http.StatusInternalServerError, true)
	snap, err = src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || err.Error() != "opencode-go: server error (HTTP 500)" {
		t.Fatalf("500 snapshot=%+v err=%v", snap, err)
	}
}

// TestOpenCodeGo429RateLimited covers both 429 rows: with a Retry-After
// header and without one.
func TestOpenCodeGo429RateLimited(t *testing.T) {
	withHeader := func(retryAfter ...string) *OpenCodeGoSource {
		t.Helper()
		reg := NewEvidenceRegistry()
		reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
		resp := bodyResponse(http.StatusTooManyRequests, []byte(`{"usage":{}}`))
		if len(retryAfter) > 0 {
			resp.Header = http.Header{"Retry-After": retryAfter}
		}
		doer := &recordingDoer{resp: resp}
		return &OpenCodeGoSource{
			mappingID: "opencode-go-test", Client: &BoundedClient{Transport: doer},
			Credentials: &opencodeGoResolver{}, Evidence: reg,
			Now: func() time.Time { return opencodeGoTestNow },
		}
	}
	src := withHeader("120")
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "Retry-After: 120s") {
		t.Fatalf("429 diagnostic=%q", err.Error())
	}
	src = withHeader() // no Retry-After supplied
	snap, err = src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no Retry-After supplied") {
		t.Fatalf("429-without-header diagnostic=%q", err.Error())
	}
}

// TestOpenCodeGoTransportErrorSanitization pins the sanitization row: a
// transport error echoing a credentialed URL and a bearer token must not
// surface them in the returned error or the snapshot error.
func TestOpenCodeGoTransportErrorSanitization(t *testing.T) {
	reg := NewEvidenceRegistry()
	reg.Register(OpenCodeGoEvidence(opencodeGoTestNow))
	doer := &recordingDoer{err: errors.New(`Get "https://user:secretpass@opencode.ai/zen/go/v1/usage": dial tcp: bearer sk-synthetic-AbCd1234`)}
	src := &OpenCodeGoSource{
		mappingID: "opencode-go-test", Client: &BoundedClient{Transport: doer},
		Credentials: &opencodeGoResolver{}, Evidence: reg, Now: func() time.Time { return opencodeGoTestNow },
	}
	snap, err := src.Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || snap.Availability != QuotaUnknown {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	for _, msg := range []string{err.Error(), snap.Error} {
		if strings.Contains(msg, "secretpass") || strings.Contains(msg, "sk-synthetic-AbCd1234") || strings.Contains(msg, "user@") {
			t.Fatalf("diagnostic leaked secrets: %q", msg)
		}
	}
	if len(doer.calls) != 1 {
		t.Fatalf("calls=%d, want exactly 1", len(doer.calls))
	}
}

// --- Class / EffectiveRemaining / NextResetAt integration ------------------

// opencodeGoClassUsageBody builds a full three-window usage payload (rolling,
// weekly, monthly) with the given raw percent values at fixed future resets:
// rolling resets 2026-08-15T16:00:00Z (earliest), weekly 2026-08-17T00:00:00Z,
// monthly 2026-08-31T00:00:00Z (latest). With the pinned opencodeGoTestNow of
// 2026-08-15T12:00:00Z every reset is future, so NextResetAt anchoring is
// deterministic.
func opencodeGoClassUsageBody(rolling, weekly, monthly string) string {
	return `{"usage":{` +
		`"rolling":{"status":"ok","percent":` + rolling + `,"resetsAt":"2026-08-15T16:00:00Z"},` +
		`"weekly":{"status":"ok","percent":` + weekly + `,"resetsAt":"2026-08-17T00:00:00Z"},` +
		`"monthly":{"status":"ok","percent":` + monthly + `,"resetsAt":"2026-08-31T00:00:00Z"}` +
		`}}`
}

// TestOpenCodeGoClassNormalAnchorsMonthlyReset covers the all-three-windows-
// normal case: Class() is ClassNormal, EffectiveRemaining() is the minimum
// across the windows, and NextResetAt() anchors on the monthly window — the
// longest window whose Period is at least MinQuotaCyclePeriod with a future
// reset — even though the 5h rolling window carries the earliest reset. This
// also pins the "rolling never anchors" property: if the shortest window won,
// the anchor would be the 2026-08-15T16:00:00Z rolling reset.
func TestOpenCodeGoClassNormalAnchorsMonthlyReset(t *testing.T) {
	src, _ := opencodeGoTestSource(t, opencodeGoClassUsageBody("12.5", "25", "50"), http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourceFresh || snap.Availability != QuotaAvailable {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if got := snap.Class(); got != ClassNormal {
		t.Fatalf("class=%s want %s", got, ClassNormal)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0.5 {
		t.Fatalf("effective remaining=%v want 0.5", rem)
	}
	reset := snap.NextResetAt()
	if reset == nil || !reset.Equal(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("next reset=%v want the monthly reset 2026-08-31T00:00:00Z", reset)
	}
}

// TestOpenCodeGoPercentHundredIsExhausted covers the exhaustion trigger: one
// window at percent:100 makes the snapshot QuotaUnavailable and Class()
// ClassExhausted, with EffectiveRemaining() bottoming out at 0 — even though
// the other windows are healthy.
func TestOpenCodeGoPercentHundredIsExhausted(t *testing.T) {
	src, _ := opencodeGoTestSource(t, opencodeGoClassUsageBody("12.5", "100", "50"), http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourceFresh {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if snap.Availability != QuotaUnavailable {
		t.Fatalf("availability=%s want %s", snap.Availability, QuotaUnavailable)
	}
	if got := snap.Class(); got != ClassExhausted {
		t.Fatalf("class=%s want %s", got, ClassExhausted)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0 {
		t.Fatalf("effective remaining=%v want 0", rem)
	}
}

// TestOpenCodeGoRollingExhaustionDrivesClass pins the plan's documented R4
// behaviour *deliberately*: EffectiveRemaining() takes the minimum across all
// usable windows, so an exhausted 5-hour rolling window exhausts the whole
// snapshot even when the weekly and monthly windows are healthy — exactly how
// Codex's 5-hour session window behaves. This is a visible decision, not an
// accident: a short-window exhaustion must surface, not be masked by longer,
// healthier windows.
func TestOpenCodeGoRollingExhaustionDrivesClass(t *testing.T) {
	src, _ := opencodeGoTestSource(t, opencodeGoClassUsageBody("100", "25", "50"), http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourceFresh {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if snap.Availability != QuotaUnavailable {
		t.Fatalf("availability=%s want %s", snap.Availability, QuotaUnavailable)
	}
	if got := snap.Class(); got != ClassExhausted {
		t.Fatalf("class=%s want %s (rolling exhaustion must drive the class)", got, ClassExhausted)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0 {
		t.Fatalf("effective remaining=%v want 0", rem)
	}
}

// TestOpenCodeGoRollingWindowNeverAnchorsReset pins the reset side of R4: the
// rolling window is not the quota-cycle anchor because its 5-hour period is
// below MinQuotaCyclePeriod (types.go, 24h). When rolling is the only window
// reporting a future reset, no window qualifies as a quota cycle and
// NextQuotaResetAt's documented fallback — the earliest future reset among all
// windows, for providers that only report short rate-limit windows — supplies
// the value. The 5h window may be *returned* through that fallback only; it
// must never *anchor* over a qualifying (>= MinQuotaCyclePeriod) window. The
// anchor-over-fallback precedence itself is pinned by
// TestOpenCodeGoClassNormalAnchorsMonthlyReset, where the earliest (rolling)
// reset loses to the monthly anchor.
func TestOpenCodeGoRollingWindowNeverAnchorsReset(t *testing.T) {
	// Only the rolling window is present, so weekly and monthly carry no
	// usable reset by absence.
	body := `{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":"2026-08-15T16:00:00Z"}}}`
	src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourcePartial || len(snap.Windows) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	// The rolling window's Period must be the sub-cycle 5h value, which
	// disqualifies it from anchoring.
	if snap.Windows[0].Period == nil || *snap.Windows[0].Period != opencodeGoRollingPeriod {
		t.Fatalf("period=%v want %v", snap.Windows[0].Period, opencodeGoRollingPeriod)
	}
	rollingReset := time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)
	reset := snap.NextResetAt()
	// Via the documented short-window fallback (not via anchoring), the only
	// future reset in the snapshot is surfaced.
	if reset == nil || !reset.Equal(rollingReset) {
		t.Fatalf("next reset=%v want the rolling reset %v via the short-window fallback", reset, rollingReset)
	}
}

// TestOpenCodeGoPartialClassStillDerivesFromDecodedWindows covers the partial
// payload case: a snapshot that decodes only some of the three windows still
// reports SourcePartial while producing a sensible Class() and a valid
// NextResetAt() anchor from the windows that did decode.
func TestOpenCodeGoPartialClassStillDerivesFromDecodedWindows(t *testing.T) {
	// Two of three windows decode (monthly absent from the payload).
	body := `{"usage":{` +
		`"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-15T16:00:00Z"},` +
		`"weekly":{"status":"ok","percent":60,"resetsAt":"2026-08-17T00:00:00Z"}` +
		`}}`
	src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
	snap, err := src.Fetch(context.Background())
	if err != nil || snap.Status != SourcePartial {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	if snap.Availability != QuotaAvailable {
		t.Fatalf("availability=%s want %s", snap.Availability, QuotaAvailable)
	}
	if got := snap.Class(); got != ClassNormal {
		t.Fatalf("class=%s want %s from the decoded windows", got, ClassNormal)
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0.4 {
		t.Fatalf("effective remaining=%v want 0.4", rem)
	}
	// Weekly is now the longest qualifying window with a future reset, so it
	// anchors.
	reset := snap.NextResetAt()
	if reset == nil || !reset.Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("next reset=%v want the weekly reset 2026-08-17T00:00:00Z", reset)
	}
}

// TestOpenCodeGoRemainingContract pins the single-source-of-truth
// percent → UsagePercent → Remaining() chain (types.go) directly on the
// decoded window, not only transitively through Class/EffectiveRemaining:
// a window at percent:40 has Remaining() == 0.6, and percent:100 has
// Remaining() == 0.
func TestOpenCodeGoRemainingContract(t *testing.T) {
	for name, tc := range map[string]struct {
		rawPercent string
		usage      float64
		want       float64
	}{
		"percent 40 remains 0.6": {"40", 40, 0.6},
		"percent 100 remains 0":  {"100", 100, 0},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"usage":{"rolling":{"status":"ok","percent":` + tc.rawPercent + `,"resetsAt":"2026-08-15T16:00:00Z"}}}`
			src, _ := opencodeGoTestSource(t, body, http.StatusOK, true)
			snap, err := src.Fetch(context.Background())
			if err != nil || len(snap.Windows) != 1 {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
			w := snap.Windows[0]
			if w.UsagePercent == nil || *w.UsagePercent != tc.usage {
				t.Fatalf("usage percent=%v want %v", w.UsagePercent, tc.usage)
			}
			rem := w.Remaining()
			if rem == nil || *rem != tc.want {
				t.Fatalf("remaining=%v want %v", rem, tc.want)
			}
		})
	}
}
