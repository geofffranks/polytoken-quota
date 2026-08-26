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

func TestNextResetAtPrefersQuotaCycleOverRateLimit(t *testing.T) {
	checked := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	fiveHours := 5 * time.Hour
	week := 7 * 24 * time.Hour
	sessionReset := checked.Add(2 * time.Hour)
	weeklyReset := checked.Add(3 * 24 * time.Hour)
	s := QuotaSnapshot{
		CheckedAt: checked,
		Windows: []QuotaWindow{
			{Name: "session", Period: &fiveHours, ResetAt: &sessionReset},
			{Name: "weekly", Period: &week, ResetAt: &weeklyReset},
		},
	}
	got := s.NextResetAt()
	if got == nil || !got.Equal(weeklyReset) {
		t.Fatalf("next reset=%v want weekly quota-cycle reset %v", got, weeklyReset)
	}
}

func TestNextResetAtAnchorSkipsAdditionalLimits(t *testing.T) {
	// Codex shape: the primary quota windows (session/weekly) are decoded
	// before additional_rate_limits products (spark/spark-weekly). The weekly
	// quota cycle must win even though the spark 5h window resets soonest and
	// the spark weekly window resets earlier than the primary weekly window.
	checked := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	fiveHours := 5 * time.Hour
	week := 7 * 24 * time.Hour
	sparkReset := checked.Add(1 * time.Hour)
	sessionReset := checked.Add(2 * time.Hour)
	sparkWeeklyReset := checked.Add(2 * 24 * time.Hour)
	weeklyReset := checked.Add(3 * 24 * time.Hour)
	spendReset := checked.Add(10 * 24 * time.Hour)
	s := QuotaSnapshot{
		CheckedAt: checked,
		Windows: []QuotaWindow{
			{Name: "session", Period: &fiveHours, ResetAt: &sessionReset},
			{Name: "weekly", Period: &week, ResetAt: &weeklyReset},
			{Name: "spend-control", ResetAt: &spendReset}, // no period: not an anchor candidate
			{Name: "spark", Period: &fiveHours, ResetAt: &sparkReset},
			{Name: "spark-weekly", Period: &week, ResetAt: &sparkWeeklyReset}, // same period, decoded later
		},
	}
	got := s.NextResetAt()
	if got == nil || !got.Equal(weeklyReset) {
		t.Fatalf("next reset=%v want primary weekly quota reset %v", got, weeklyReset)
	}
}

func TestNextResetAtFallsBackToEarliestWhenOnlyRateLimits(t *testing.T) {
	checked := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	fiveHours := 5 * time.Hour
	soon := checked.Add(1 * time.Hour)
	later := checked.Add(3 * time.Hour)
	s := QuotaSnapshot{
		CheckedAt: checked,
		Windows: []QuotaWindow{
			{Name: "session", Period: &fiveHours, ResetAt: &later},
			{Name: "spark", Period: &fiveHours, ResetAt: &soon},
		},
	}
	got := s.NextResetAt()
	if got == nil || !got.Equal(soon) {
		t.Fatalf("next reset=%v want earliest future reset %v", got, soon)
	}
}

func TestNextResetAtSkipsStaleAnchor(t *testing.T) {
	checked := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	month := 30 * 24 * time.Hour
	stale := checked.Add(-1 * time.Hour)
	monthlyReset := checked.Add(20 * 24 * time.Hour)
	s := QuotaSnapshot{
		CheckedAt: checked,
		Windows: []QuotaWindow{
			{Name: "weekly", Period: &week, ResetAt: &stale}, // past at observation time
			{Name: "monthly", Period: &month, ResetAt: &monthlyReset},
		},
	}
	got := s.NextResetAt()
	if got == nil || !got.Equal(monthlyReset) {
		t.Fatalf("next reset=%v want next-longest future anchor %v", got, monthlyReset)
	}
}

func TestResetCreditObservationMergeMatrix(t *testing.T) {
	asOf := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	future := asOf.Add(time.Hour)
	past := asOf.Add(-time.Hour)
	success := ResetCreditAttempt{
		Status: CreditAttemptSuccess,
		At:     asOf.Add(-2 * time.Hour),
		Inventory: &ResetCreditInventory{ServerAvailableCount: 2, UsableCount: 2,
			AvailableExpiries: []*time.Time{&future, nil}, ObservedAt: asOf.Add(-2 * time.Hour)},
	}
	state := MergeResetCreditObservation(ResetCreditState{}, success)
	if state.LastSuccess == nil || state.LatestAttempt.Status != CreditAttemptSuccess {
		t.Fatalf("success merge=%+v", state)
	}

	// Failed and repeated failed attempts preserve prior success and age from it.
	failedAt := asOf.Add(-time.Hour)
	state = MergeResetCreditObservation(state, ResetCreditAttempt{Status: CreditAttemptFailed, At: failedAt, Error: "HTTP 500"})
	prior := state.LastSuccess
	state = MergeResetCreditObservation(state, ResetCreditAttempt{Status: CreditAttemptFailed, At: asOf, Error: "HTTP 503"})
	if state.LastSuccess != prior || !state.LatestAttempt.At.Equal(asOf) {
		t.Fatalf("failure must preserve success/update attempt: %+v", state)
	}

	// A fresh usage summary updates provenance but never overwrites inventory.
	balance := "1E+2"
	summary := &CodexUsageSummary{ObservedAt: asOf, Credits: &UsageCredits{Balance: &balance}}
	state = MergeCodexUsageSummary(state, summary)
	if state.UsageSummary != summary || state.LastSuccess != prior {
		t.Fatalf("usage summary clobbered inventory: %+v", state)
	}

	// Partial valid inventory becomes the new last success and latest partial attempt.
	partial := ResetCreditAttempt{Status: CreditAttemptPartial, At: asOf.Add(time.Minute), Inventory: &ResetCreditInventory{ServerAvailableCount: 2, UsableCount: 1, DiscrepancyCount: 1, ObservedAt: asOf.Add(time.Minute)}}
	state = MergeResetCreditObservation(state, partial)
	if state.LastSuccess != partial.Inventory || state.LatestAttempt.Status != CreditAttemptPartial {
		t.Fatalf("partial inventory not accepted: %+v", state)
	}

	// Skipped attempts preserve success; no prior success remains unknown.
	state = MergeResetCreditObservation(state, ResetCreditAttempt{Status: CreditAttemptSkipped, At: asOf.Add(2 * time.Minute), Error: "evidence stale"})
	if state.LastSuccess != partial.Inventory {
		t.Fatalf("skipped attempt erased success: %+v", state)
	}
	empty := MergeResetCreditObservation(ResetCreditState{}, ResetCreditAttempt{Status: CreditAttemptFailed, At: asOf})
	if empty.LastSuccess != nil || empty.UsableCountAt(asOf) != nil {
		t.Fatalf("no prior success must remain unknown: %+v", empty)
	}

	// Report-time expiry filtering is as-of based and does not rewrite history;
	// absent expiry remains usable/unknown rather than zero expiry.
	history := &ResetCreditInventory{ServerAvailableCount: 3, UsableCount: 3, AvailableExpiries: []*time.Time{&past, &future, nil}, ObservedAt: asOf.Add(-2 * time.Hour)}
	state.LastSuccess = history
	if got := state.UsableCountAt(asOf); got == nil || *got != 2 {
		t.Fatalf("usable at as-of=%v want 2", got)
	}
	if len(state.LastSuccess.AvailableExpiries) != 3 || state.LastSuccess.AvailableExpiries[0] != &past {
		t.Fatal("report-time filtering rewrote durable history")
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
