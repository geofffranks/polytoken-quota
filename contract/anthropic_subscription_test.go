package contract

// Anthropic-subscription adapter fixture acceptance: replay the committed
// sanitized fixture through the real adapter so the evidence artifact and
// parser cannot drift independently. Uses only synthetic tokens.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// subFixture is the {status, body} fixture envelope shared by quota fixtures.
type subFixture struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type subStubTransport struct {
	fx  subFixture
	req *http.Request
}

func (t *subStubTransport) Do(req *http.Request) (*http.Response, error) {
	t.req = req
	return &http.Response{
		StatusCode: t.fx.Status,
		Body:       io.NopCloser(bytes.NewReader(t.fx.Body)),
		Header:     http.Header{},
	}, nil
}

func loadSubscriptionFixture(t *testing.T) subFixture {
	t.Helper()
	path := filepath.Join("testdata", "quota", "anthropic-subscription", "usage.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fx subFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return fx
}

func newSubscriptionFixtureSource(t *testing.T) (*quota.AnthropicSubscriptionSource, *subStubTransport) {
	t.Helper()
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.AnthropicSubscriptionEvidence(contractNow))
	tr := &subStubTransport{fx: loadSubscriptionFixture(t)}
	client := &quota.BoundedClient{Transport: tr, Timeout: time.Second, MaxBodyBytes: 1 << 20}
	creds := &quota.ClaudeCredentialLoader{
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{"claudeAiOauth":{"accessToken":"synthetic-subscription-fixture-token","refreshToken":"never-used","expiresAt":"2027-01-01T00:00:00Z"}}`), nil
		},
	}
	return quota.NewAnthropicSubscriptionSource("anthropic", client, creds, reg, contractNow), tr
}

func TestAnthropicSubscriptionContractFixture(t *testing.T) {
	src, tr := newSubscriptionFixtureSource(t)
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != quota.SourceFresh || snap.Availability != quota.QuotaAvailable {
		t.Fatalf("status=%s availability=%s (error: %s)", snap.Status, snap.Availability, snap.Error)
	}
	sess := contractFindWindow(snap.Windows, "session")
	if sess == nil || sess.UsagePercent == nil || math.Abs(*sess.UsagePercent-42.5) > 1e-9 {
		t.Fatalf("session window=%+v", sess)
	}
	if sess.Period == nil || *sess.Period != 300*time.Minute {
		t.Fatalf("session period=%v want 300m", sess.Period)
	}
	if sess.ResetAt == nil || !sess.ResetAt.Equal(time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("session reset=%v want 2026-08-22T17:00:00Z", sess.ResetAt)
	}
	week := contractFindWindow(snap.Windows, "weekly")
	if week == nil || week.UsagePercent == nil || math.Abs(*week.UsagePercent-61.2) > 1e-9 {
		t.Fatalf("weekly window=%+v", week)
	}
	if week.Period == nil || *week.Period != 10080*time.Minute {
		t.Fatalf("weekly period=%v want 10080m", week.Period)
	}
	// Contract headers: OAuth bearer, beta flag, Claude Code UA.
	if got := tr.req.Header.Get("Authorization"); got != "Bearer synthetic-subscription-fixture-token" {
		t.Fatalf("authorization=%q", got)
	}
	if got := tr.req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta=%q", got)
	}
	if got := tr.req.Header.Get("User-Agent"); got != "claude-code/2.1.0" {
		t.Fatalf("user-agent=%q", got)
	}
}

func TestAnthropicSubscriptionFixtureIsSecretFree(t *testing.T) {
	body := loadSubscriptionFixture(t).Body
	if secretPattern.MatchString(string(body)) {
		t.Fatal("subscription fixture contains a secret pattern")
	}
}
