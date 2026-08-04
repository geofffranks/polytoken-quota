package quota

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fp(v float64) *float64 { return &v }

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestEffectiveRemainingPicksMinimumAcrossUsableWindows(t *testing.T) {
	// A: used=8, limit=10  -> remaining 0.2
	// B: used=4, limit=10  -> remaining 0.6
	// C: no data           -> unusable
	s := QuotaSnapshot{
		Windows: []QuotaWindow{
			{Name: "A", Used: fp(8), Limit: fp(10)},
			{Name: "B", Used: fp(4), Limit: fp(10)},
			{Name: "C"},
		},
	}
	rem := s.EffectiveRemaining()
	if rem == nil {
		t.Fatal("expected non-nil effective remaining")
	}
	if !approxEqual(*rem, 0.2) {
		t.Fatalf("effective remaining=%v want 0.2", *rem)
	}
}

func TestEffectiveRemainingClampsOverLimitToZero(t *testing.T) {
	// used=12, limit=10 -> raw -0.2 -> clamped to 0
	s := QuotaSnapshot{
		Windows: []QuotaWindow{{Name: "over", Used: fp(12), Limit: fp(10)}},
	}
	rem := s.EffectiveRemaining()
	if rem == nil || !approxEqual(*rem, 0) {
		t.Fatalf("effective remaining=%v want 0", rem)
	}
}

func TestSnapshotWithOnlyUsagePercentIsUsable(t *testing.T) {
	// 90% used -> remaining 0.1
	s := QuotaSnapshot{
		Windows: []QuotaWindow{{Name: "pct", UsagePercent: fp(90)}},
	}
	rem := s.EffectiveRemaining()
	if rem == nil || !approxEqual(*rem, 0.1) {
		t.Fatalf("effective remaining=%v want 0.1", rem)
	}
	if c := s.Class(); c != ClassLow {
		t.Fatalf("class=%s want low", c)
	}
}

func TestEffectiveRemainingNilWhenNoneUsable(t *testing.T) {
	s := QuotaSnapshot{
		Windows: []QuotaWindow{{Name: "empty"}},
	}
	if s.EffectiveRemaining() != nil {
		t.Fatal("expected nil effective remaining when no window is usable")
	}
}

func TestClassThresholds(t *testing.T) {
	cases := []struct {
		name  string
		used  float64
		limit float64
		want  QuotaClass
	}{
		{"exhausted at zero remaining", 10, 10, ClassExhausted},
		{"over-limit is exhausted", 12, 10, ClassExhausted},
		{"low below one-third", 8, 10, ClassLow},           // remaining 0.2
		{"normal above one-third", 2, 10, ClassNormal},     // remaining 0.8
		{"exactly one-third is normal", 2, 3, ClassNormal}, // remaining 1/3
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := QuotaSnapshot{
				Windows: []QuotaWindow{{Name: "w", Used: fp(c.used), Limit: fp(c.limit)}},
			}
			if got := s.Class(); got != c.want {
				t.Fatalf("class=%s want %s", got, c.want)
			}
		})
	}
}

func TestClassUnknownNeverDemotes(t *testing.T) {
	s := QuotaSnapshot{Windows: []QuotaWindow{{Name: "no-data"}}}
	if c := s.Class(); c != ClassUnknown {
		t.Fatalf("class=%s want unknown", c)
	}
}

func TestNoUsableDataYieldsUnknownAndNils(t *testing.T) {
	s := QuotaSnapshot{Windows: []QuotaWindow{{Name: "empty"}}}
	if s.Class() != ClassUnknown {
		t.Fatalf("class=%s want unknown", s.Class())
	}
	if s.EffectiveRemaining() != nil {
		t.Fatal("expected nil effective remaining")
	}
	if s.NextResetAt() != nil {
		t.Fatal("expected nil next reset")
	}
}

func TestNextResetAtEarliestFuture(t *testing.T) {
	checked := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	earliest := checked.Add(2 * time.Hour)
	later := checked.Add(5 * time.Hour)
	past := checked.Add(-1 * time.Hour)
	s := QuotaSnapshot{
		CheckedAt: checked,
		Windows: []QuotaWindow{
			{Name: "later", ResetAt: &later},
			{Name: "earliest", ResetAt: &earliest},
			{Name: "past", ResetAt: &past}, // past relative to CheckedAt -> ignored
			{Name: "none"},
		},
	}
	got := s.NextResetAt()
	if got == nil || !got.Equal(earliest) {
		t.Fatalf("next reset=%v want %v", got, earliest)
	}
}

func TestNextResetAtNilWhenNone(t *testing.T) {
	s := QuotaSnapshot{
		CheckedAt: time.Now(),
		Windows:   []QuotaWindow{{Name: "a"}},
	}
	if s.NextResetAt() != nil {
		t.Fatal("expected nil next reset when no windows report one")
	}
}

func TestSanitizeErrorNil(t *testing.T) {
	if got := SanitizeError(nil); got != "" {
		t.Fatalf("nil error -> %q want empty", got)
	}
}

func TestSanitizeErrorUnicodeBound(t *testing.T) {
	out := SanitizeError(errors.New(strings.Repeat("界", 300)))
	if len(out) > maxErrorSummary {
		t.Fatalf("sanitized output not bounded: %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("expected truncation marker, got suffix %q", out[len(out)-3:])
	}
	if !utf8.ValidString(out) {
		t.Fatal("sanitized output split UTF-8")
	}
}

func TestSanitizeErrorStripsSecrets(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		forbidden string
	}{
		{"url with credentials", "GET https://alice:hunter2@host.example.com/v1/quota failed", "hunter2"},
		{"url account stripped", "GET https://alice:hunter2@host.example.com/v1/quota failed", "alice"},
		{"bearer token", "auth header Bearer eyJabc.def-ghi_token rejected", "eyJabc.def-ghi_token"},
		{"account substring", "query account=billing-prod-42 denied", "billing-prod-42"},
		{"api key", "api_key=sk-live-12345 invalid", "sk-live-12345"},
		{"password", "password=p4ssw0rd! rejected", "p4ssw0rd!"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := SanitizeError(errors.New(c.input))
			if strings.Contains(out, c.forbidden) {
				t.Errorf("sanitized output still contains %q: %q", c.forbidden, out)
			}
			if len(out) > 512 {
				t.Errorf("sanitized output not bounded: %d bytes", len(out))
			}
		})
	}
}
