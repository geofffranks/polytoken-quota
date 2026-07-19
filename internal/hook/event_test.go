package hook

import (
	"fmt"
	"strings"
	"testing"
)

// TestDecodeContract is the Task 3 blueprint contract test. It verifies
// camelCase JSON keys, unknown-key tolerance, account discard, the ISO-8601
// timestamp parse, and JSON/env identity cross-check agreement.
func TestDecodeContract(t *testing.T) {
	valid := `{"event":"quota_low","provider":"codex","account":"discard-me","usagePercent":0.92,"timestamp":"2026-07-19T12:00:00Z","future":true}`
	env := map[string]string{"CODEXBAR_EVENT": "quota_low", "CODEXBAR_PROVIDER": "codex", "CODEXBAR_TIMESTAMP": "2026-07-19T12:00:00Z"}
	got, err := Decode(strings.NewReader(valid), env, 4096)
	if err != nil || got.Type != QuotaLow || got.Provider != "codex" || got.UsagePercent == nil || *got.UsagePercent != .92 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "discard-me") {
		t.Fatal("normalized event retained account")
	}
}

// TestDecodeRejectsInvalidWithoutMutation proves that no valid Event escapes a
// bad decode: every malformed input yields a zero-value Event and a non-nil
// error.
func TestDecodeRejectsInvalidWithoutMutation(t *testing.T) {
	cases := []string{
		`{}`,
		`{"event":"future","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`,
		`{"event":"quota_low","provider":"codex","usagePercent":1.1,"timestamp":"2026-07-19T12:00:00Z"}`,
		`{"event":"quota_low","provider":"codex","used":1e999,"timestamp":"2026-07-19T12:00:00Z"}`,
		`{"event":"quota_low","provider":"codex","timestamp":"bad"}`,
		`{} {}`,
	}
	for _, input := range cases {
		got, err := Decode(strings.NewReader(input), nil, 4096)
		if err == nil {
			t.Fatalf("accepted %q", input)
		}
		if got != (Event{}) {
			t.Fatalf("non-zero Event returned for %q: %+v", input, got)
		}
	}
	if got, err := Decode(strings.NewReader(strings.Repeat("x", 4097)), nil, 4096); err == nil {
		t.Fatal("accepted oversized input")
	} else if got != (Event{}) {
		t.Fatalf("oversized input returned non-zero Event: %+v", got)
	}
}

// TestDecodeRejectsIdentityDisagreement proves that an env identity key that
// contradicts the JSON payload is rejected.
func TestDecodeRejectsIdentityDisagreement(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{"CODEXBAR_EVENT": "quota_reached", "CODEXBAR_PROVIDER": "codex", "CODEXBAR_TIMESTAMP": "2026-07-19T12:00:00Z"}
	if _, err := Decode(strings.NewReader(input), env, 4096); err == nil {
		t.Fatal("accepted contradictory identity")
	}
}

// TestDecodeRejectsProviderIdentityDisagreement covers the provider identity
// cross-check independently.
func TestDecodeRejectsProviderIdentityDisagreement(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{"CODEXBAR_EVENT": "quota_low", "CODEXBAR_PROVIDER": "claude", "CODEXBAR_TIMESTAMP": "2026-07-19T12:00:00Z"}
	if _, err := Decode(strings.NewReader(input), env, 4096); err == nil {
		t.Fatal("accepted provider identity mismatch")
	}
}

// TestDecodeRejectsTimestampIdentityDisagreement covers the timestamp identity
// cross-check independently.
func TestDecodeRejectsTimestampIdentityDisagreement(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{"CODEXBAR_EVENT": "quota_low", "CODEXBAR_PROVIDER": "codex", "CODEXBAR_TIMESTAMP": "2026-07-20T12:00:00Z"}
	if _, err := Decode(strings.NewReader(input), env, 4096); err == nil {
		t.Fatal("accepted timestamp identity mismatch")
	}
}

// TestDecodeEnvFallbackForOmittedOptionals proves that env fills in optional
// fields only when JSON omits them.
func TestDecodeEnvFallbackForOmittedOptionals(t *testing.T) {
	input := `{"event":"quota_reset","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{
		"CODEXBAR_EVENT":         "quota_reset",
		"CODEXBAR_PROVIDER":      "codex",
		"CODEXBAR_TIMESTAMP":     "2026-07-19T12:00:00Z",
		"CODEXBAR_WINDOW":        "session",
		"CODEXBAR_USED":          "100",
		"CODEXBAR_LIMIT":         "500",
		"CODEXBAR_STATUS":        "ok",
		"CODEXBAR_RESET_AT":      "2026-07-20T00:00:00Z",
		"CODEXBAR_USAGE_PERCENT": "0.1",
	}
	got, err := Decode(strings.NewReader(input), env, 4096)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.Window == nil || *got.Window != "session" {
		t.Errorf("window=%v", got.Window)
	}
	if got.Used == nil || *got.Used != 100 {
		t.Errorf("used=%v", got.Used)
	}
	if got.Limit == nil || *got.Limit != 500 {
		t.Errorf("limit=%v", got.Limit)
	}
	if got.Status == nil || *got.Status != "ok" {
		t.Errorf("status=%v", got.Status)
	}
	if got.UsagePercent == nil || *got.UsagePercent != 0.1 {
		t.Errorf("usagePercent=%v", got.UsagePercent)
	}
	if got.ResetAt == nil || got.ResetAt.Format("2006-01-02") != "2026-07-20" {
		t.Errorf("resetAt=%v", got.ResetAt)
	}
}

// TestDecodeJSONAuthorityOverEnvForOptionals proves JSON wins over env for
// optional fields when both provide one (no disagreement error; JSON value kept).
func TestDecodeJSONAuthorityOverEnvForOptionals(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","usagePercent":0.5,"timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{
		"CODEXBAR_EVENT":         "quota_low",
		"CODEXBAR_PROVIDER":      "codex",
		"CODEXBAR_TIMESTAMP":     "2026-07-19T12:00:00Z",
		"CODEXBAR_USAGE_PERCENT": "0.8",
	}
	got, err := Decode(strings.NewReader(input), env, 4096)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.UsagePercent == nil || *got.UsagePercent != 0.5 {
		t.Errorf("usagePercent=%v (env should not override JSON)", got.UsagePercent)
	}
}

// TestDecodeRejectsNonFiniteEnvNumber proves the finite-number guard catches an
// env-sourced non-finite value (strconv.ParseFloat accepts "NaN" without error,
// so Decode must reject it itself).
func TestDecodeRejectsNonFiniteEnvNumber(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{
		"CODEXBAR_EVENT":     "quota_low",
		"CODEXBAR_PROVIDER":  "codex",
		"CODEXBAR_TIMESTAMP": "2026-07-19T12:00:00Z",
		"CODEXBAR_USED":      "NaN",
	}
	if _, err := Decode(strings.NewReader(input), env, 4096); err == nil {
		t.Fatal("accepted non-finite env number")
	}
}

// TestDecodeRejectsUnknownEnvNumber proves a malformed env number is rejected.
func TestDecodeRejectsUnknownEnvNumber(t *testing.T) {
	input := `{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`
	env := map[string]string{
		"CODEXBAR_EVENT":     "quota_low",
		"CODEXBAR_PROVIDER":  "codex",
		"CODEXBAR_TIMESTAMP": "2026-07-19T12:00:00Z",
		"CODEXBAR_LIMIT":     "not-a-number",
	}
	if _, err := Decode(strings.NewReader(input), env, 4096); err == nil {
		t.Fatal("accepted malformed env number")
	}
}

// TestDecodeAccountNeverInEvent proves account is discarded regardless of source.
func TestDecodeAccountNeverInEvent(t *testing.T) {
	cases := []string{
		`{"event":"quota_low","provider":"codex","account":"leak-attempt-1","timestamp":"2026-07-19T12:00:00Z"}`,
		`{"event":"quota_reset","provider":"codex","account":"","timestamp":"2026-07-19T12:00:00Z"}`,
	}
	for _, input := range cases {
		got, err := Decode(strings.NewReader(input), nil, 4096)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), "leak-attempt") {
			t.Fatalf("account leaked: %+v", got)
		}
	}
}
