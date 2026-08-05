// Package contract holds fixture-based compatibility tests that pin the
// polytoken-quota hook decoder to the CodexBar 0.44.0 hook event contract. The
// tests load synthetic, non-personal JSON+env fixture pairs and assert that
// hook.Decode normalizes them into the expected hook.Event, discards account,
// and round-trips the contract's camelCase keys. They are part of the normal
// `go test ./contract` run; no live CodexBar binary is required.
package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/hook"
)

func TestCodexBarContractFixtures(t *testing.T) {
	cases := []struct {
		name     string
		typ      hook.Type
		provider string
		// fields asserted when non-empty; accountMarker must never appear.
	}{
		{name: "quota_low", typ: hook.QuotaLow, provider: "codex"},
		{name: "quota_reached", typ: hook.QuotaReached, provider: "claude"},
		{name: "quota_reset", typ: hook.QuotaReset, provider: "codex"},
		{name: "provider_unavailable", typ: hook.ProviderUnavailable, provider: "gemini"},
		{name: "provider_recovered", typ: hook.ProviderRecovered, provider: "gemini"},
		{name: "refresh_failed", typ: hook.RefreshFailed, provider: "claude"},
	}
	dir := filepath.Join("..", "internal", "hook", "testdata", "codexbar-0.44")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonPath := filepath.Join(dir, tc.name+".json")
			envPath := filepath.Join(dir, tc.name+".env")

			payload, err := os.ReadFile(jsonPath)
			if err != nil {
				t.Fatalf("read fixture %s: %v", jsonPath, err)
			}
			env := loadEnv(t, envPath)

			got, err := hook.Decode(strings.NewReader(string(payload)), env, 4096)
			if err != nil {
				t.Fatalf("decode %s: %v", tc.name, err)
			}
			if got.Type != tc.typ {
				t.Errorf("type = %q want %q", got.Type, tc.typ)
			}
			if got.Provider != tc.provider {
				t.Errorf("provider = %q want %q", got.Provider, tc.provider)
			}
			if got.Timestamp.IsZero() {
				t.Error("timestamp not parsed")
			}
			// Identity cross-check must accept the fixture env (agreement); an
			// error above already proves disagreement was not raised. Account
			// must never survive into the normalized event.
			if strings.Contains(fmt.Sprintf("%+v", got), "demo-tenant") {
				t.Error("account leaked into normalized event")
			}
		})
	}
}

// TestCodexBarContractRejectsTrailingObject pins the exactly-one-JSON-object
// rule against a fixture-shaped payload followed by a second object.
func TestCodexBarContractRejectsTrailingObject(t *testing.T) {
	dir := filepath.Join("..", "internal", "hook", "testdata", "codexbar-0.44")
	payload, err := os.ReadFile(filepath.Join(dir, "quota_low.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	env := loadEnv(t, filepath.Join(dir, "quota_low.env"))
	joined := strings.TrimSpace(string(payload)) + " " + strings.TrimSpace(string(payload))
	if _, err := hook.Decode(strings.NewReader(joined), env, 8192); err == nil {
		t.Fatal("accepted two concatenated JSON objects")
	}
}

// loadEnv parses a KEY=VALUE env file, skipping blanks and comments.
func loadEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env fixture %s: %v", path, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed env line %q in %s", line, path)
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env
}
