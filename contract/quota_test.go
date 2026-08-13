// These tests prove the contract evidence gate's negative path: a provider
// adapter whose evidence is absent, expired, or incomplete must be unsupported
// and make NO provider request (fail closed). They are plain Go tests — no
// external binary or real network is required. A synthetic gated source mirrors
// how Task 5 adapters will compose the evidence gate.
//
// The release evidence check skips cleanly when no provider adapters are
// configured (the default local-dev case) and asserts every configured adapter
// has fresh evidence when they are.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// contractNow is a stable reference time for deterministic gate evaluation.
var contractNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// freshEvidence builds a complete, current Evidence record for the contract tests.
func freshEvidence(provider string) quota.Evidence {
	return quota.Evidence{
		Provider:   provider,
		Endpoint:   "https://api.example.com/v1/quota",
		Method:     "GET",
		AuthType:   "oauth-bearer",
		SchemaNote: "usage and limit fields",
		RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ReviewBy:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// --- Synthetic evidence-gated source -------------------------------------

// recordingTransport captures whether Do was called and returns a trivial 200.
type recordingTransport struct{ calls int }

func (r *recordingTransport) Do(req *http.Request) (*http.Response, error) {
	r.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

// gatedSource is a minimal QuotaSource that gates Fetch on evidence freshness,
// mirroring how Task 5 provider adapters compose the evidence gate with the
// bounded HTTP client.
type gatedSource struct {
	provider  string
	reg       *quota.EvidenceRegistry
	now       time.Time
	transport *recordingTransport
}

func (g *gatedSource) MappingID() string { return g.provider }

func (g *gatedSource) Status() quota.SupportStatus {
	return quota.SupportFromEvidence(g.reg.Status(g.provider, g.now))
}

func (g *gatedSource) Fetch(ctx context.Context) (quota.QuotaSnapshot, error) {
	st := g.Status()
	if !st.Supported {
		// Fail closed: return an error WITHOUT touching the transport.
		return quota.QuotaSnapshot{
			MappingID: g.provider,
			Status:    quota.SourceFailed,
			Error:     st.Reason,
		}, errors.New(st.Reason)
	}
	bc := &quota.BoundedClient{Transport: g.transport, Timeout: time.Second, MaxBodyBytes: 1 << 10}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/v1/quota", nil)
	if err != nil {
		return quota.QuotaSnapshot{
			MappingID: g.provider,
			Status:    quota.SourceFailed,
			Error:     "bad request",
		}, err
	}
	if _, err := bc.Do(req); err != nil {
		return quota.QuotaSnapshot{
			MappingID: g.provider,
			Status:    quota.SourceFailed,
			Error:     quota.SanitizeError(err),
		}, err
	}
	return quota.QuotaSnapshot{
		MappingID:    g.provider,
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		CheckedAt:    g.now,
	}, nil
}

// --- Negative gate: absent / expired / incomplete ------------------------

func TestEvidenceGateAbsentFailsClosed(t *testing.T) {
	reg := quota.NewEvidenceRegistry() // no evidence registered for "codex"
	rt := &recordingTransport{}
	src := &gatedSource{provider: "codex", reg: reg, now: contractNow, transport: rt}

	if src.Status().Supported {
		t.Fatal("expected unsupported for absent evidence")
	}
	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for absent evidence")
	}
	if snap.Status != quota.SourceFailed {
		t.Fatalf("snapshot status = %s, want failed", snap.Status)
	}
	if rt.calls != 0 {
		t.Fatalf("transport must not be called when evidence is absent; got %d calls", rt.calls)
	}
}

func TestEvidenceGateExpiredFailsClosed(t *testing.T) {
	reg := quota.NewEvidenceRegistry()
	e := freshEvidence("codex")
	e.ReviewBy = contractNow.Add(-24 * time.Hour) // expired yesterday
	reg.Register(e)

	rt := &recordingTransport{}
	src := &gatedSource{provider: "codex", reg: reg, now: contractNow, transport: rt}

	if src.Status().Supported {
		t.Fatal("expected unsupported for expired evidence")
	}
	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for expired evidence")
	}
	if snap.Status != quota.SourceFailed {
		t.Fatalf("snapshot status = %s, want failed", snap.Status)
	}
	if rt.calls != 0 {
		t.Fatalf("transport must not be called when evidence is expired; got %d calls", rt.calls)
	}
}

func TestEvidenceGateIncompleteFailsClosed(t *testing.T) {
	reg := quota.NewEvidenceRegistry()
	e := freshEvidence("codex")
	e.Endpoint = "" // missing required field
	reg.Register(e)

	rt := &recordingTransport{}
	src := &gatedSource{provider: "codex", reg: reg, now: contractNow, transport: rt}

	if src.Status().Supported {
		t.Fatal("expected unsupported for incomplete evidence")
	}
	snap, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for incomplete evidence")
	}
	if snap.Status != quota.SourceFailed {
		t.Fatalf("snapshot status = %s, want failed", snap.Status)
	}
	if rt.calls != 0 {
		t.Fatalf("transport must not be called when evidence is incomplete; got %d calls", rt.calls)
	}
}

// --- Positive gate: fresh evidence proceeds ------------------------------

func TestEvidenceGateFreshProceeds(t *testing.T) {
	reg := quota.NewEvidenceRegistry()
	reg.Register(freshEvidence("codex"))

	rt := &recordingTransport{}
	src := &gatedSource{provider: "codex", reg: reg, now: contractNow, transport: rt}

	if !src.Status().Supported {
		t.Fatal("expected supported for fresh evidence")
	}
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for fresh evidence: %v", err)
	}
	if snap.Status != quota.SourceFresh {
		t.Fatalf("snapshot status = %s, want fresh", snap.Status)
	}
	if rt.calls != 1 {
		t.Fatalf("transport should be called exactly once for fresh evidence; got %d calls", rt.calls)
	}
}

// --- Redaction: no secrets in evidence fields or remediation reasons -----

// secretPattern matches bearer tokens, URLs with embedded credentials, and
// key/value secret assignments. Evidence fields and remediation reasons must
// never contain these.
//
// "bearer" is anchored to whitespace or start-of-string so auth-type category
// labels like "oauth-bearer" (which are safe, not real tokens) do not trigger a
// false positive; only a standalone "Bearer <opaque>" token — preceded by
// whitespace — matches.
var secretPattern = regexp.MustCompile(
	`(?i)((?:^|\s)bearer\s+\S+|(?:https?://)[^/\s:@]+:[^/\s@]+@|(?:api[_-]?key|apikey|\btoken\b|secret|password|passwd|account|acct)\s*[=:]\s*\S+)`,
)

// evidenceFieldsFlattened returns all Evidence field values as a single string
// for secret scanning.
func evidenceFieldsFlattened(e quota.Evidence) string {
	return strings.Join([]string{
		e.Provider, e.Endpoint, e.Method, e.AuthType, e.SchemaNote, e.FixturePath,
		e.RecordedAt.Format(time.RFC3339), e.ReviewBy.Format(time.RFC3339),
	}, " ")
}

func TestEvidenceAndReasonsAreSecretFree(t *testing.T) {
	// Collect remediation reasons from every evidence state.
	var reasons []string

	reg := quota.NewEvidenceRegistry()
	reasons = append(reasons, reg.Status("codex", contractNow).Reason) // absent
	reasons = append(reasons, reg.Status("zai", contractNow).Reason)   // absent

	exp := freshEvidence("codex")
	exp.ReviewBy = contractNow.Add(-24 * time.Hour)
	regExp := quota.NewEvidenceRegistry()
	regExp.Register(exp)
	reasons = append(reasons, regExp.Status("codex", contractNow).Reason) // expired

	for _, m := range []func(*quota.Evidence){
		func(e *quota.Evidence) { e.Endpoint = "" },
		func(e *quota.Evidence) { e.AuthType = "" },
		func(e *quota.Evidence) { e.Method = "" },
	} {
		e := freshEvidence("codex")
		m(&e)
		regInc := quota.NewEvidenceRegistry()
		regInc.Register(e)
		reasons = append(reasons, regInc.Status("codex", contractNow).Reason) // incomplete
	}

	for i, r := range reasons {
		if secretPattern.MatchString(r) {
			t.Fatalf("remediation reason %d contains a secret pattern: %q", i, r)
		}
	}

	// Collect sanitized evidence field values and scan them too.
	var evidenceValues []string
	evidenceValues = append(evidenceValues, evidenceFieldsFlattened(freshEvidence("codex")))
	evidenceValues = append(evidenceValues, evidenceFieldsFlattened(freshEvidence("zai")))
	for i, v := range evidenceValues {
		if secretPattern.MatchString(v) {
			t.Fatalf("evidence record %d contains a secret pattern: %q", i, v)
		}
	}

	// Negative control: the secret scanner must catch real secrets, so the
	// guard above is not vacuous. Each of these MUST match.
	mustCatch := []string{
		"Bearer eyJhbGciOiJIUzI1NiJ9.secret.payload",
		"https://user:hunter2@api.example.com/v1",
		"api_key=sk-1234567890abcdef",
		"token=ghp_abcDEF123456",
	}
	for i, s := range mustCatch {
		if !secretPattern.MatchString(s) {
			t.Fatalf("negative control %d failed: scanner did not catch %q", i, s)
		}
	}
}

// --- Release evidence gate -----------------------------------------------

// configuredProviders returns the provider names that are configured for this
// environment, read from the PQ_TEST_PROVIDERS env var (comma-separated). When
// unset or empty (the default local-dev case), no providers are configured.
func configuredProviders() []string {
	v := os.Getenv("PQ_TEST_PROVIDERS")
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildReleaseRegistry returns an EvidenceRegistry populated with production
// adapter evidence. Every provider adapter registers its sanitized contract
// evidence here so the release gate recognizes it as fresh.
func buildReleaseRegistry() *quota.EvidenceRegistry {
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.CodexUsageEvidence(time.Now()))
	reg.Register(quota.CodexResetCreditsEvidence(time.Now()))
	reg.Register(quota.ZaiEvidence(time.Now()))
	reg.Register(quota.AnthropicEvidence(time.Now()))
	reg.Register(quota.NeuralwattEvidence(time.Now()))
	return reg
}

func TestReleaseEvidenceValidatesBothCodexContracts(t *testing.T) {
	reg := buildReleaseRegistry()
	now := time.Now()
	statuses := quota.ValidateRelease(reg, []string{"codex"}, now)
	if len(statuses) != 2 || statuses[0].State != quota.EvidenceFresh || statuses[1].State != quota.EvidenceFresh {
		t.Fatalf("release validation must require fresh codex usage/reset contracts: %+v", statuses)
	}
	for _, contractID := range []string{quota.CodexUsageContract, quota.CodexResetCreditsContract} {
		e, ok := reg.GetContract("codex", contractID)
		if !ok {
			t.Fatalf("missing codex/%s release evidence", contractID)
		}
		if e.FixturePath == "" {
			t.Fatalf("codex/%s evidence lacks fixture path", contractID)
		}
		fixturePath := strings.TrimPrefix(filepath.ToSlash(e.FixturePath), "contract/")
		if _, err := os.Stat(filepath.Clean(fixturePath)); err != nil {
			t.Fatalf("codex/%s fixture %q unavailable: %v", contractID, e.FixturePath, err)
		}
		if got := reg.StatusContract("codex", contractID, now); got.State != quota.EvidenceFresh {
			t.Fatalf("codex/%s evidence=%s: %s", contractID, got.State, got.Reason)
		}
	}
}

func TestReleaseEvidenceRejectsNonFreshCodexResetContract(t *testing.T) {
	now := contractNow
	usage := quota.CodexUsageEvidence(now)
	reset := quota.CodexResetCreditsEvidence(now)
	legacy := usage
	legacy.ContractID = ""
	legacyRegistry := quota.NewEvidenceRegistry()
	legacyRegistry.Register(legacy)
	if got := legacyRegistry.Status("codex", now); got.State != quota.EvidenceFresh {
		t.Fatalf("legacy generic evidence should authorize runtime usage, got %+v", got)
	}

	cases := []struct {
		name       string
		evidence   []quota.Evidence
		wantStates []quota.EvidenceState
		wantReason string
	}{
		{
			name:       "reset absent",
			evidence:   []quota.Evidence{usage},
			wantStates: []quota.EvidenceState{quota.EvidenceFresh, quota.EvidenceAbsent},
			wantReason: "provider codex/reset_credits has no recorded contract evidence; record evidence before enabling",
		},
		{
			name: "reset stale",
			evidence: []quota.Evidence{usage, func() quota.Evidence {
				stale := reset
				stale.ReviewBy = now.Add(-time.Hour)
				return stale
			}()},
			wantStates: []quota.EvidenceState{quota.EvidenceFresh, quota.EvidenceExpired},
			wantReason: "provider codex contract evidence expired on 2026-06-15; re-verify and update",
		},
		{
			name:       "legacy generic only",
			evidence:   []quota.Evidence{legacy},
			wantStates: []quota.EvidenceState{quota.EvidenceIncomplete, quota.EvidenceAbsent},
			wantReason: "provider codex/reset_credits has no recorded contract evidence; record evidence before enabling",
		},
		{
			name: "reset fixture blank",
			evidence: []quota.Evidence{usage, func() quota.Evidence {
				blank := reset
				blank.FixturePath = ""
				return blank
			}()},
			wantStates: []quota.EvidenceState{quota.EvidenceFresh, quota.EvidenceIncomplete},
			wantReason: "provider codex/reset_credits release evidence is incomplete (missing fixture_path); record complete evidence",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := quota.NewEvidenceRegistry()
			for _, evidence := range tc.evidence {
				reg.Register(evidence)
			}
			statuses := quota.ValidateRelease(reg, []string{"codex"}, now)
			if len(statuses) != 2 {
				t.Fatalf("release statuses=%+v want ordered usage/reset pair", statuses)
			}
			for i, want := range tc.wantStates {
				if statuses[i].State != want {
					t.Fatalf("release status[%d]=%s want %s; all=%+v", i, statuses[i].State, want, statuses)
				}
			}
			if statuses[1].Reason != tc.wantReason {
				t.Fatalf("release reset reason=%q want %q", statuses[1].Reason, tc.wantReason)
			}
		})
	}
}

func TestReleaseEvidenceGate(t *testing.T) {
	configured := configuredProviders()
	if len(configured) == 0 {
		t.Skip("no providers configured (set PQ_TEST_PROVIDERS=codex,zai to enable the release evidence check)")
	}
	reg := buildReleaseRegistry()
	statuses := quota.ValidateRelease(reg, configured, time.Now())
	wantStatuses := len(configured)
	for _, provider := range configured {
		if provider == "codex" {
			wantStatuses++ // codex expands to usage + reset-credit contracts
		}
	}
	if len(statuses) != wantStatuses {
		t.Fatalf("got %d statuses, want %d configured contracts", len(statuses), wantStatuses)
	}
	for _, s := range statuses {
		if s.State != quota.EvidenceFresh {
			t.Errorf("release evidence is %s, want fresh: %s", s.State, s.Reason)
		}
	}
}

// TestBuiltInReleaseEvidenceIsFreshNow is an unconditional CI guard. Built-in
// adapter evidence uses release-owned fixed dates; this test fails if any
// adapter's ReviewBy date has passed, so stale evidence cannot ship unnoticed.
func TestBuiltInReleaseEvidenceIsFreshNow(t *testing.T) {
	reg := buildReleaseRegistry()
	now := time.Now()
	for _, def := range quota.AdapterDefinitions() {
		if def.Name == "codex" {
			for _, contractID := range []string{quota.CodexUsageContract, quota.CodexResetCreditsContract} {
				if got := reg.StatusContract(def.Name, contractID, now); got.State != quota.EvidenceFresh {
					t.Errorf("adapter %s/%s evidence is %s at %s: %s", def.Name, contractID, got.State, now.Format("2006-01-02"), got.Reason)
				}
			}
			continue
		}
		if got := reg.StatusContract(def.Name, "", now); got.State != quota.EvidenceFresh {
			t.Errorf("adapter %s evidence is %s at %s: %s", def.Name, got.State, now.Format("2006-01-02"), got.Reason)
		}
	}
}

// --- Codex adapter fixture acceptance -------------------------------------
//
// These acceptance tests load the sanitized Codex fixture files from the
// contract testdata tree, run them through the real CodexSource adapter (behind
// a fake transport + synthetic credential resolver), and verify the resulting
// QuotaSnapshot. They are the acceptance tests for the Codex contract evidence.

// codexStubTransport returns a canned response body/status and records calls.
type codexStubTransport struct {
	body  []byte
	code  int
	calls int
}

func (t *codexStubTransport) Do(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: t.code,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     http.Header{},
	}, nil
}

// codexAuthResolver returns synthetic auth.json content (no real secrets).
type codexAuthResolver struct{ fail bool }

func (r *codexAuthResolver) Resolve(quota.CredentialRef) (string, error) {
	if r.fail {
		return "", errors.New("missing auth file")
	}
	return `{"tokens":{"access_token":"synthetic-token","account_id":"acct-synthetic"},"last_refresh":"2026-01-01T00:00:00Z"}`, nil
}

func loadCodexFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "quota", "codex", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

// newCodexFixtureSource builds a CodexSource with fresh evidence that returns the
// given fixture bytes on a 200.
func newCodexFixtureSource(t *testing.T, body []byte) *quota.CodexSource {
	t.Helper()
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.CodexEvidence(contractNow))
	client := &quota.BoundedClient{
		Transport:    &codexStubTransport{body: body, code: 200},
		Timeout:      time.Second,
		MaxBodyBytes: 1 << 20,
	}
	return quota.NewCodexSource("codex-acct1", client, &codexAuthResolver{}, "", reg, contractNow)
}

func contractFindWindow(ws []quota.QuotaWindow, name string) *quota.QuotaWindow {
	for i := range ws {
		if ws[i].Name == name {
			return &ws[i]
		}
	}
	return nil
}

func TestCodexContractFixtures(t *testing.T) {
	// pro.json: full response → session/weekly/spend-control, fresh, available.
	t.Run("pro", func(t *testing.T) {
		src := newCodexFixtureSource(t, loadCodexFixture(t, "pro.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh {
			t.Fatalf("status = %s, want fresh", snap.Status)
		}
		if snap.Availability != quota.QuotaAvailable {
			t.Fatalf("availability = %s, want available", snap.Availability)
		}
		s := contractFindWindow(snap.Windows, "session")
		if s == nil || s.UsagePercent == nil || *s.UsagePercent != 22 {
			t.Fatalf("session window usage_percent = %v", s)
		}
		if w := contractFindWindow(snap.Windows, "weekly"); w == nil || w.UsagePercent == nil || *w.UsagePercent != 43 {
			t.Fatalf("weekly window = %v", w)
		}
		sc := contractFindWindow(snap.Windows, "spend-control")
		if sc == nil || sc.Used == nil || *sc.Used != 7761 || sc.Limit == nil || *sc.Limit != 100000 {
			t.Fatalf("spend-control window = %v", sc)
		}
	})

	// exhausted.json: a window at used_percent 100 → unavailable, exhausted.
	t.Run("exhausted", func(t *testing.T) {
		src := newCodexFixtureSource(t, loadCodexFixture(t, "exhausted.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Availability != quota.QuotaUnavailable {
			t.Fatalf("availability = %s, want unavailable", snap.Availability)
		}
		if got := snap.Class(); got != quota.ClassExhausted {
			t.Fatalf("class = %s, want exhausted", got)
		}
	})

	// partial.json: malformed secondary → skipped, partial status, session survives.
	t.Run("partial", func(t *testing.T) {
		src := newCodexFixtureSource(t, loadCodexFixture(t, "partial.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourcePartial {
			t.Fatalf("status = %s, want partial", snap.Status)
		}
		if contractFindWindow(snap.Windows, "session") == nil {
			t.Fatal("session window must survive a malformed sibling")
		}
		if contractFindWindow(snap.Windows, "weekly") != nil {
			t.Fatal("malformed weekly window must be skipped")
		}
	})

	// minimal.json: only primary → one window, fresh, available.
	t.Run("minimal", func(t *testing.T) {
		src := newCodexFixtureSource(t, loadCodexFixture(t, "minimal.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh {
			t.Fatalf("status = %s, want fresh", snap.Status)
		}
		if contractFindWindow(snap.Windows, "session") == nil {
			t.Fatal("missing session window")
		}
	})

	// additional_limits.json: named model-specific windows present.
	t.Run("additional_limits", func(t *testing.T) {
		src := newCodexFixtureSource(t, loadCodexFixture(t, "additional_limits.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if contractFindWindow(snap.Windows, "GPT-5.3-Codex-Spark") == nil {
			t.Fatal("missing named additional window GPT-5.3-Codex-Spark")
		}
		if contractFindWindow(snap.Windows, "Another-Model") == nil {
			t.Fatal("missing named additional window Another-Model")
		}
	})
}

// TestCodexContractFixturesAreSecretFree asserts the committed fixture files
// contain no bearer tokens, account IDs, or key/value secrets.
func TestCodexContractFixturesAreSecretFree(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "quota", "codex"))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body := loadCodexFixture(t, e.Name())
		if secretPattern.MatchString(string(body)) {
			t.Fatalf("fixture %s contains a secret pattern", e.Name())
		}
	}
}

// --- z.ai adapter fixture acceptance -------------------------------------
//
// These acceptance tests load the sanitized z.ai fixture files from the
// contract testdata tree, run them through the real ZaiSource adapter (behind a
// fake transport + synthetic credential resolver), and verify the resulting
// QuotaSnapshot. They are the acceptance tests for the z.ai contract evidence.

// zaiStubTransport returns a canned response body/status and records calls.
type zaiStubTransport struct {
	body  []byte
	code  int
	calls int
}

func (t *zaiStubTransport) Do(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: t.code,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     http.Header{},
	}, nil
}

// zaiKeyResolver returns a synthetic Bearer API key (no real secrets).
type zaiKeyResolver struct{ fail bool }

func (r *zaiKeyResolver) Resolve(quota.CredentialRef) (string, error) {
	if r.fail {
		return "", errors.New("missing key")
	}
	return "synthetic-zai-key", nil
}

func loadZaiFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "quota", "zai", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

// newZaiFixtureSource builds a ZaiSource with fresh evidence that returns the
// given fixture bytes on a 200.
func newZaiFixtureSource(t *testing.T, body []byte) *quota.ZaiSource {
	t.Helper()
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.ZaiEvidence(contractNow))
	client := &quota.BoundedClient{
		Transport:    &zaiStubTransport{body: body, code: 200},
		Timeout:      time.Second,
		MaxBodyBytes: 1 << 20,
	}
	return quota.NewZaiSource("zai-acct1", client, &zaiKeyResolver{}, "", reg, contractNow)
}

func TestZaiContractFixtures(t *testing.T) {
	// pro.json: one TOKENS_LIMIT (5h → primary) + one TIME_LIMIT (MCP → monthly),
	// raw counts present → derived percentage, fresh, available.
	t.Run("pro", func(t *testing.T) {
		src := newZaiFixtureSource(t, loadZaiFixture(t, "pro.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh {
			t.Fatalf("status = %s, want fresh", snap.Status)
		}
		if snap.Availability != quota.QuotaAvailable {
			t.Fatalf("availability = %s, want available", snap.Availability)
		}
		if contractFindWindow(snap.Windows, "primary") == nil {
			t.Fatal("missing primary token window")
		}
		if contractFindWindow(snap.Windows, "monthly") == nil {
			t.Fatal("missing monthly (MCP) time window")
		}
		// Raw counts derive used/limit; reset converted from milliseconds.
		p := contractFindWindow(snap.Windows, "primary")
		if p.Used == nil || *p.Used != 13628365 || p.Limit == nil || *p.Limit != 40000000 {
			t.Fatalf("primary used/limit = %v/%v", p.Used, p.Limit)
		}
		if p.ResetAt == nil {
			t.Fatal("primary reset_at must be set (millis)")
		}
	})

	// bigmodel_cn.json: weekly (primary) + 5h (session) token limits + monthly.
	t.Run("bigmodel_cn", func(t *testing.T) {
		src := newZaiFixtureSource(t, loadZaiFixture(t, "bigmodel_cn.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh {
			t.Fatalf("status = %s, want fresh", snap.Status)
		}
		primary := contractFindWindow(snap.Windows, "primary")
		if primary == nil || primary.Limit == nil || *primary.Limit != 10000000 {
			t.Fatalf("primary (weekly) window = %v", primary)
		}
		if contractFindWindow(snap.Windows, "session") == nil {
			t.Fatal("missing session (5h) token window")
		}
		if contractFindWindow(snap.Windows, "monthly") == nil {
			t.Fatal("missing monthly (MCP) window")
		}
	})

	// exhausted.json: a TOKENS_LIMIT at percentage 100 → unavailable, exhausted.
	t.Run("exhausted", func(t *testing.T) {
		src := newZaiFixtureSource(t, loadZaiFixture(t, "exhausted.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Availability != quota.QuotaUnavailable {
			t.Fatalf("availability = %s, want unavailable", snap.Availability)
		}
		if got := snap.Class(); got != quota.ClassExhausted {
			t.Fatalf("class = %s, want exhausted", got)
		}
	})

	// missing_counts.json: only percentage → UsagePercent from server, Used/Limit nil.
	t.Run("missing_counts", func(t *testing.T) {
		src := newZaiFixtureSource(t, loadZaiFixture(t, "missing_counts.json"))
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		p := contractFindWindow(snap.Windows, "primary")
		if p == nil || p.UsagePercent == nil || *p.UsagePercent != 42 {
			t.Fatalf("primary usage_percent = %v, want 42 (server percentage)", p)
		}
		if p.Used != nil || p.Limit != nil {
			t.Fatalf("primary used/limit must be nil when counts absent, got %v/%v", p.Used, p.Limit)
		}
	})

	// auth_failure.json: envelope code 1001 → sanitized auth-failure error.
	t.Run("auth_failure", func(t *testing.T) {
		src := newZaiFixtureSource(t, loadZaiFixture(t, "auth_failure.json"))
		snap, err := src.Fetch(context.Background())
		if err == nil {
			t.Fatal("expected auth-failure error for envelope code 1001")
		}
		if snap.Status != quota.SourceFailed {
			t.Fatalf("status = %s, want failed", snap.Status)
		}
		if !strings.Contains(err.Error(), "auth") {
			t.Fatalf("error should mention auth: %s", err)
		}
		if strings.Contains(err.Error(), "synthetic-zai-key") {
			t.Fatalf("auth error must not leak key: %s", err)
		}
	})
}

// TestZaiContractFixturesAreSecretFree asserts the committed z.ai fixture files
// contain no bearer tokens, account IDs, or key/value secrets.
func TestZaiContractFixturesAreSecretFree(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "quota", "zai"))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body := loadZaiFixture(t, e.Name())
		if secretPattern.MatchString(string(body)) {
			t.Fatalf("fixture %s contains a secret pattern", e.Name())
		}
	}
}

// --- Neuralwatt adapter fixture acceptance ----------------------------------
//
// These acceptance tests replay the committed Neuralwatt fixture through the
// real adapter so the evidence artifact and parser cannot drift independently.

type neuralwattStubTransport struct {
	body  []byte
	code  int
	calls int
}

func (t *neuralwattStubTransport) Do(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: t.code,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     http.Header{},
	}, nil
}

type neuralwattFixtureResolver struct{}

func (neuralwattFixtureResolver) Resolve(quota.CredentialRef) (string, error) {
	return "synthetic-neuralwatt-fixture-key", nil
}

func loadNeuralwattFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("testdata", "quota", "neuralwatt", "quota.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

func newNeuralwattFixtureSource(t *testing.T) *quota.NeuralwattSource {
	t.Helper()
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.NeuralwattEvidence(contractNow))
	client := &quota.BoundedClient{
		Transport:    &neuralwattStubTransport{body: loadNeuralwattFixture(t), code: http.StatusOK},
		Timeout:      time.Second,
		MaxBodyBytes: 1 << 20,
	}
	fixtureNow := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	return quota.NewNeuralwattSource("neuralwatt-fixture", client, neuralwattFixtureResolver{}, reg, fixtureNow)
}

func TestNeuralwattContractFixture(t *testing.T) {
	snap, err := newNeuralwattFixtureSource(t).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != quota.SourceFresh || snap.Availability != quota.QuotaAvailable {
		t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
	}
	w := contractFindWindow(snap.Windows, "balance_usd")
	if w == nil || w.Used == nil || *w.Used != 27.5 || w.Limit == nil || *w.Limit != 100 {
		t.Fatalf("balance window=%+v", w)
	}
	if w.UsagePercent == nil || math.Abs(*w.UsagePercent-27.5) > 1e-9 {
		t.Fatalf("balance usage percent=%v", *w.UsagePercent)
	}
}

func TestNeuralwattContractFixtureIsSecretFree(t *testing.T) {
	body := loadNeuralwattFixture(t)
	if secretPattern.MatchString(string(body)) {
		t.Fatal("Neuralwatt fixture contains a secret pattern")
	}
}

// --- Anthropic adapter fixture acceptance -----------------------------------
//
// The Anthropic adapter polls the Admin API cost report for month-to-date
// spend against the mapping's monthly budget, so its fixtures describe
// {status, body} cost-report pages. These acceptance tests replay each
// fixture through the real AnthropicSource behind a fake transport +
// synthetic admin-key resolver, with a 200 USD test budget.

// anthropicFixture is the on-disk fixture shape.
type anthropicFixture struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// anthropicStubTransport replays a fixture's status and body.
type anthropicStubTransport struct {
	fx    anthropicFixture
	calls int
}

func (t *anthropicStubTransport) Do(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: t.fx.Status,
		Body:       io.NopCloser(bytes.NewReader(t.fx.Body)),
		Header:     http.Header{},
	}, nil
}

// anthropicKeyResolver returns a synthetic admin API key.
type anthropicKeyResolver struct{}

func (anthropicKeyResolver) Resolve(quota.CredentialRef) (string, error) {
	return "synthetic-anthropic-admin-fixture-key", nil
}

func loadAnthropicFixture(t *testing.T, name string) anthropicFixture {
	t.Helper()
	path := filepath.Join("testdata", "quota", "anthropic", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fx anthropicFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return fx
}

// anthropicFixtureBudget is the monthly budget the fixtures are written against.
const anthropicFixtureBudget = 200.0

// newAnthropicFixtureSource builds an AnthropicSource with fresh evidence that
// replays the given fixture against the fixture budget.
func newAnthropicFixtureSource(t *testing.T, fx anthropicFixture) *quota.AnthropicSource {
	t.Helper()
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.AnthropicEvidence(contractNow))
	client := &quota.BoundedClient{
		Transport:    &anthropicStubTransport{fx: fx},
		Timeout:      time.Second,
		MaxBodyBytes: 1 << 20,
	}
	return quota.NewAnthropicSource("anthropic", client, anthropicKeyResolver{}, anthropicFixtureBudget, reg, contractNow)
}

func TestAnthropicContractFixtures(t *testing.T) {
	// midmonth.json: 150.00 spend of 200 budget → monthly window, 75% used.
	t.Run("midmonth", func(t *testing.T) {
		snap, err := newAnthropicFixtureSource(t, loadAnthropicFixture(t, "midmonth.json")).Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh || snap.Availability != quota.QuotaAvailable {
			t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
		}
		w := contractFindWindow(snap.Windows, "monthly")
		if w == nil || w.Used == nil || *w.Used != 150 || w.Limit == nil || *w.Limit != anthropicFixtureBudget {
			t.Fatalf("monthly window=%+v", w)
		}
		if rem := snap.EffectiveRemaining(); rem == nil || *rem != 0.25 {
			t.Fatalf("effective remaining=%v", rem)
		}
	})
	// empty_month.json: no spend recorded → fresh, fully available.
	t.Run("empty_month", func(t *testing.T) {
		snap, err := newAnthropicFixtureSource(t, loadAnthropicFixture(t, "empty_month.json")).Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh || snap.Availability != quota.QuotaAvailable {
			t.Fatalf("status=%s availability=%s", snap.Status, snap.Availability)
		}
		w := contractFindWindow(snap.Windows, "monthly")
		if w == nil || *w.Used != 0 || *w.UsagePercent != 0 {
			t.Fatalf("monthly window=%+v", w)
		}
	})
	// over_budget.json: 250.00 spend of 200 budget → exhausted observation.
	t.Run("over_budget", func(t *testing.T) {
		snap, err := newAnthropicFixtureSource(t, loadAnthropicFixture(t, "over_budget.json")).Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if snap.Status != quota.SourceFresh || snap.Availability != quota.QuotaUnavailable {
			t.Fatalf("status=%s availability=%s want fresh/unavailable", snap.Status, snap.Availability)
		}
	})
	// auth_failure.json: 401 → failed attempt with admin-key remediation.
	t.Run("auth_failure", func(t *testing.T) {
		snap, err := newAnthropicFixtureSource(t, loadAnthropicFixture(t, "auth_failure.json")).Fetch(context.Background())
		if err == nil {
			t.Fatal("401 fixture did not fail")
		}
		if snap.Status != quota.SourceFailed {
			t.Fatalf("status=%s want failed", snap.Status)
		}
		if !strings.Contains(snap.Error, "ANTHROPIC_ADMIN_API_KEY") {
			t.Fatalf("error not actionable: %s", snap.Error)
		}
		if strings.Contains(snap.Error, "fixture-key") {
			t.Fatal("API key material leaked into error")
		}
	})
}

// TestAnthropicLiveContract is the opt-in real-API acceptance check: it runs
// only when ANTHROPIC_ADMIN_API_KEY is set, performs one live cost-report read
// through the production adapter (read-only, no spend), and asserts the
// cost-report contract still holds. It never prints, persists, or asserts on
// the key itself.
func TestAnthropicLiveContract(t *testing.T) {
	if os.Getenv("ANTHROPIC_ADMIN_API_KEY") == "" {
		t.Skip("set ANTHROPIC_ADMIN_API_KEY for the live Anthropic cost-report contract check (read-only)")
	}
	// liveTestBudget is a synthetic denominator for the spend/budget division.
	// It is NOT read from Anthropic — no Anthropic API exposes the account's
	// spend limit, tier cap, or credit balance; production uses the operator's
	// monthly_budget_usd from desired.yaml. Only the spend side is live here.
	const liveTestBudget = 100.0
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.AnthropicEvidence(time.Now()))
	src := quota.NewAnthropicSource("anthropic-live", &quota.BoundedClient{}, quota.DefaultCredentialResolver(), liveTestBudget, reg, time.Now())
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("live fetch failed: %v (snapshot error: %s)", err, snap.Error)
	}
	if snap.Status != quota.SourceFresh && snap.Status != quota.SourcePartial {
		t.Fatalf("live status=%s (error: %s)", snap.Status, snap.Error)
	}
	w := contractFindWindow(snap.Windows, "monthly")
	if w == nil || w.Used == nil || w.Limit == nil {
		t.Fatalf("live response produced no usable monthly window: %+v", snap.Windows)
	}
	if *w.Used < 0 {
		t.Fatalf("negative month-to-date spend: %v", *w.Used)
	}
	t.Logf("live month-to-date spend: %.4f USD against a synthetic %.2f test budget (%.1f%%; the budget is this test's constant, not an Anthropic value)", *w.Used, *w.Limit, *w.UsagePercent)
}

// TestAnthropicContractFixturesAreSecretFree asserts the committed Anthropic
// fixture files contain no bearer tokens, account IDs, or key/value secrets.
func TestAnthropicContractFixturesAreSecretFree(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "quota", "anthropic"))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join("testdata", "quota", "anthropic", e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if secretPattern.MatchString(string(b)) {
			t.Fatalf("fixture %s contains a secret pattern", e.Name())
		}
	}
}
