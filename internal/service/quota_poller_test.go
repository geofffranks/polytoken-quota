package service

// Production QuotaPoller tests (Task 8b). These exercise QuotaPollerImpl with
// real CodexSource/ZaiSource adapters backed by a fake HTTP transport and a
// literal credential resolver, verifying: provider isolation (one failure never
// blocks another), the evidence-gate fail-closed path (no request made), the
// unknown-adapter fail-closed path, and the --provider filter — all without
// touching the network.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// fakeTransport is an HTTPDoer that returns a preset canned response per host.
type fakeTransport struct {
	canned map[string]*http.Response
	called map[string]int
}

func (t *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	if t.called != nil {
		t.called[req.URL.Host]++
	}
	if resp, ok := t.canned[req.URL.Host]; ok {
		return resp, nil
	}
	return &http.Response{StatusCode: 500, Body: http.NoBody, Header: http.Header{}}, nil
}

// bodyResponse builds a 200 response whose body is the given string.
func bodyResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

// literalCreds always resolves any credential reference to a fixed value. For
// the codex adapter (which JSON-parses auth.json), it returns a valid auth
// envelope; for zai (which reads an env key) it returns a bare token.
type literalCreds struct{}

func (literalCreds) Resolve(ref quota.CredentialRef) (string, error) {
	if ref.Kind == quota.CredentialFile {
		// codex adapter JSON-parses this into a bearer token.
		return `{"tokens":{"access_token":"test-bearer","account_id":""}}`, nil
	}
	return "test-api-key", nil
}

func TestNewQuotaPollerUsesReviewedReleaseEvidence(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	transport := &fakeTransport{
		canned: map[string]*http.Response{
			"chatgpt.com":        bodyResponse(`{"rate_limit":{"primary_window":{"used_percent":20,"reset_at":100}}}`),
			"api.z.ai":           bodyResponse(`{"code":200,"success":true,"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":34,"usage":40000000,"currentValue":13628365,"remaining":26371635,"nextResetTime":1768507567547},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":34,"usage":40000000,"currentValue":13628365,"remaining":26371635,"nextResetTime":1768507567547}],"planName":"Pro"}}`),
			"api.neuralwatt.com": bodyResponse(`{"snapshot_at":"2026-07-19T12:00:00Z","balance":{"credits_remaining_usd":72.5,"total_credits_usd":100,"credits_used_usd":27.5},"subscription":null}`),
		},
		called: map[string]int{},
	}
	poller := NewQuotaPoller()
	impl, ok := poller.(*QuotaPollerImpl)
	if !ok {
		t.Fatalf("factory returned %T, want *QuotaPollerImpl", poller)
	}
	impl.Client = &quota.BoundedClient{Transport: transport}
	impl.Credentials = literalCreds{}
	impl.Now = func() time.Time { return now }
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"codex":      {Quota: &policy.QuotaConfig{Adapter: "codex"}},
		"zai":        {Quota: &policy.QuotaConfig{Adapter: "zai"}},
		"neuralwatt": {Quota: &policy.QuotaConfig{Adapter: "neuralwatt"}},
	}}
	out, err := poller.Poll(context.Background(), desired, "", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"codex", "zai", "neuralwatt"} {
		if out[id].Status != quota.SourceFresh {
			t.Fatalf("%s status=%s, want fresh: %+v", id, out[id].Status, out[id])
		}
	}
	if transport.called["chatgpt.com"] == 0 || transport.called["api.z.ai"] == 0 || transport.called["api.neuralwatt.com"] == 0 {
		t.Fatalf("reviewed adapters did not poll: calls=%v", transport.called)
	}
}

func TestQuotaPollerIsolationOneFailureDoesNotBlockAnother(t *testing.T) {
	reg := quota.NewEvidenceRegistry()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	reg.Register(quota.CodexEvidence(now))
	reg.Register(quota.ZaiEvidence(now))
	transport := &fakeTransport{
		canned: map[string]*http.Response{
			// codex succeeds; zai gets a 500 (server error).
			"chatgpt.com": bodyResponse(`{"rate_limit":{"primary_window":{"used_percent":20,"reset_at":100}}}`),
			"api.z.ai":    {StatusCode: 500, Body: http.NoBody, Header: http.Header{}},
		},
		called: map[string]int{},
	}
	poller := &QuotaPollerImpl{
		Client:      &quota.BoundedClient{Transport: transport},
		Credentials: literalCreds{},
		Evidence:    reg,
		Now:         func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
	}
	desired := policy.Desired{
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
			"zai":   {Quota: &policy.QuotaConfig{Adapter: "zai"}},
		},
	}

	out, err := poller.Poll(context.Background(), desired, "", time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("poll error: %v", err)
	}
	// codex succeeds → fresh.
	if out["codex"].Status != quota.SourceFresh {
		t.Fatalf("codex status=%s want fresh: %+v", out["codex"].Status, out["codex"])
	}
	// zai fails → failed, isolated (codex still accepted).
	if out["zai"].Status != quota.SourceFailed {
		t.Fatalf("zai status=%s want failed: %+v", out["zai"].Status, out["zai"])
	}
	// The map includes both providers (isolation: failure included, not dropped).
	if len(out) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(out), out)
	}
}

// TestQuotaPollerUnsupportedSourceFailsClosedNoRequest verifies that when an
// adapter source reports unsupported (absent evidence), pollOne returns a failed
// snapshot without making any HTTP request. We construct an explicitly
// unsupported source directly (a registry with no registered evidence).
func TestQuotaPollerExpiredOrAbsentEvidenceNeverCallsTransport(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		evidence *quota.Evidence
	}{
		{name: "absent"},
		{name: "expired", evidence: func() *quota.Evidence { e := quota.CodexEvidence(now); e.ReviewBy = now.Add(-time.Minute); return &e }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := quota.NewEvidenceRegistry()
			if tc.evidence != nil {
				reg.Register(*tc.evidence)
			}
			transport := &fakeTransport{called: map[string]int{}}
			poller := &QuotaPollerImpl{Client: &quota.BoundedClient{Transport: transport}, Credentials: literalCreds{}, Evidence: reg, Now: func() time.Time { return now }}
			desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{"codex": {Quota: &policy.QuotaConfig{Adapter: "codex"}}}}
			out, err := poller.Poll(context.Background(), desired, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if out["codex"].Status != quota.SourceFailed {
				t.Fatalf("status=%s", out["codex"].Status)
			}
			if transport.called["chatgpt.com"] != 0 {
				t.Fatalf("transport calls=%d", transport.called["chatgpt.com"])
			}
			if got := reg.Status("codex", now); got.State == quota.EvidenceFresh {
				t.Fatal("poll refreshed release evidence")
			}
		})
	}
}

func TestQuotaPollerUnsupportedSourceFailsClosedNoRequest(t *testing.T) {
	src := unsupportedSource{mappingID: "codex", reason: "no evidence"}
	snap := pollOne(context.Background(), src)
	if snap.Status != quota.SourceFailed {
		t.Fatalf("status=%s want failed: %+v", snap.Status, snap)
	}
	if snap.MappingID != "codex" {
		t.Fatalf("mapping id=%q want codex", snap.MappingID)
	}
}

func TestQuotaPollerUnknownAdapterFailsClosed(t *testing.T) {
	poller := &QuotaPollerImpl{
		Client:      &quota.BoundedClient{},
		Credentials: quota.DefaultCredentialResolver(),
		Evidence:    quota.NewEvidenceRegistry(),
		Now:         func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
	}
	desired := policy.Desired{
		Providers: map[policy.MappingID]policy.Mapping{
			"weird": {Quota: &policy.QuotaConfig{Adapter: "unknown-vendor"}},
		},
	}
	out, err := poller.Poll(context.Background(), desired, "", time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("poll error: %v", err)
	}
	if out["weird"].Status != quota.SourceFailed {
		t.Fatalf("unknown adapter should fail closed: %+v", out["weird"])
	}
	if out["weird"].MappingID != "weird" {
		t.Fatalf("mapping id=%q want weird", out["weird"].MappingID)
	}
}

type fixedDoctorPolicy struct{ desired policy.Desired }

func (f fixedDoctorPolicy) LoadPolicy() (policy.Desired, error) { return f.desired, nil }
func (fixedDoctorPolicy) DesiredExists() bool                   { return true }

func doctorStateStore(t *testing.T, st state.State) state.Store {
	t.Helper()
	path := t.TempDir() + "/state.json"
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return state.Store{Path: path}
}

// doctorQuotaFindings builds quota probes from the preloaded observed state +
// desired policy + evidence, then evaluates them through doctor.QuotaFindings.
// It replaces the removed quotaDoctorInspector.Findings path.
func doctorQuotaFindings(t *testing.T, observed state.State, desired policy.Desired, now time.Time, evidence *quota.EvidenceRegistry) []doctor.Finding {
	t.Helper()
	probes, _ := buildDoctorQuotaProbes(doctorQuotaInputs{
		observed: observed,
		desired:  desired,
		now:      now,
		evidence: evidence,
	})
	return doctor.QuotaFindings(probes, false, now)
}

func TestQuotaDoctorUsesPollerEvidenceWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	reg := quota.NewEvidenceRegistry()
	poller := &QuotaPollerImpl{Evidence: reg}
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"codex": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
	}}
	q := doctorQuotaFindings(t, state.State{Providers: map[string]state.ProviderState{}}, desired, now, poller.Evidence)
	findings := q
	if len(findings) != 2 || findings[0].Code != "quota-adapter-unsupported" || findings[1].Code != "quota-partial-unusable" {
		t.Fatalf("absent evidence findings = %+v", findings)
	}
	// Expired and incomplete records must remain unsupported rather than being replaced.
	expired := quota.CodexEvidence(now)
	expired.ReviewBy = now.Add(-time.Hour)
	reg.Register(expired)
	findings = doctorQuotaFindings(t, state.State{Providers: map[string]state.ProviderState{}}, desired, now, poller.Evidence)
	if len(findings) != 2 || !strings.Contains(findings[0].Message, "expired") || findings[1].Code != "quota-partial-unusable" {
		t.Fatalf("expired evidence findings = %+v", findings)
	}
	incomplete := quota.CodexEvidence(now)
	incomplete.Endpoint = ""
	reg.Register(incomplete)
	findings = doctorQuotaFindings(t, state.State{Providers: map[string]state.ProviderState{}}, desired, now, poller.Evidence)
	if len(findings) != 2 || !strings.Contains(findings[0].Message, "incomplete") || findings[1].Code != "quota-partial-unusable" {
		t.Fatalf("incomplete evidence findings = %+v", findings)
	}
}

func TestQuotaDoctorUsesMappingID(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"openai": {Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour}},
	}}
	used, limit := 10.0, 100.0
	observed := state.State{Providers: map[string]state.ProviderState{
		"openai":    {QuotaAttempt: &quota.QuotaSnapshot{MappingID: "openai", Status: quota.SourceFailed, Error: "poll failed"}, QuotaSnapshot: &quota.QuotaSnapshot{MappingID: "openai", CheckedAt: now, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Used: &used, Limit: &limit}}}},
		"unmanaged": {QuotaAttempt: &quota.QuotaSnapshot{MappingID: "unmanaged", Status: quota.SourceFailed, Error: "other failed"}},
	}}
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.CodexEvidence(now))
	findings := doctorQuotaFindings(t, observed, desired, now, reg)
	if len(findings) != 2 {
		t.Fatalf("findings=%+v, want mapping and residual failures only", findings)
	}
	seen := map[string]bool{}
	for _, finding := range findings {
		seen[finding.TargetID] = true

	}
	if !seen["openai"] || !seen["unmanaged"] {
		t.Fatalf("finding targets=%v, want openai and unmanaged", seen)
	}
}

func TestQuotaDoctorIncludesQuotaMappingWithoutProvider(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		"empty": {Quota: &policy.QuotaConfig{Adapter: "unknown"}},
	}}
	findings := doctorQuotaFindings(t, state.State{Providers: map[string]state.ProviderState{}}, desired, now, quota.NewEvidenceRegistry())
	if len(findings) != 2 {
		t.Fatalf("empty-provider mapping findings = %+v", findings)
	}
	for _, finding := range findings {
		if finding.TargetID != "empty" {
			t.Fatalf("empty-provider finding target=%q: %+v", finding.TargetID, findings)
		}
	}
}

func TestQuotaDoctorUsesMappingIDWhenCodexBarProviderDiffers(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	mappingID := policy.MappingID("mapping-id")
	desired := policy.Desired{Providers: map[policy.MappingID]policy.Mapping{
		mappingID: {
			Quota: &policy.QuotaConfig{Adapter: "codex"},
		},
	}}
	observed := state.State{Providers: map[string]state.ProviderState{
		string(mappingID): {
			QuotaAttempt: &quota.QuotaSnapshot{MappingID: string(mappingID), Status: quota.SourceFailed, Error: "poll failed"},
		},
	}}
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.CodexEvidence(now))
	findings := doctorQuotaFindings(t, observed, desired, now, reg)
	if len(findings) != 2 {
		t.Fatalf("mapping/provider findings = %+v, want partial snapshot and failed attempt", findings)
	}
	for _, finding := range findings {
		if finding.TargetID != string(mappingID) {
			t.Fatalf("finding target=%q, want mapping ID %q", finding.TargetID, mappingID)
		}
	}
}

func TestQuotaPollerProviderFilter(t *testing.T) {
	reg := quota.NewEvidenceRegistry()
	transport := &fakeTransport{
		canned: map[string]*http.Response{
			"chatgpt.com": bodyResponse(`{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":100}}}`),
			"api.z.ai":    bodyResponse(`{"code":200,"success":true,"data":{"limits":[]}}`),
		},
		called: map[string]int{},
	}
	poller := &QuotaPollerImpl{
		Client:      &quota.BoundedClient{Transport: transport},
		Credentials: literalCreds{},
		Evidence:    reg,
		Now:         func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
	}
	desired := policy.Desired{
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Quota: &policy.QuotaConfig{Adapter: "codex"}},
			"zai":   {Quota: &policy.QuotaConfig{Adapter: "zai"}},
		},
	}
	out, err := poller.Poll(context.Background(), desired, "zai", time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("poll error: %v", err)
	}
	if _, ok := out["codex"]; ok {
		t.Fatal("codex should not be polled under --provider zai")
	}
	if _, ok := out["zai"]; !ok {
		t.Fatal("zai should be polled")
	}
}
