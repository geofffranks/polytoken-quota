package quota

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"
)

// These bounded properties use synthetic inputs and reference calculations that
// do not call the implementation's normalization helpers as their oracle.
func TestPropertyRemainingBoundsAndMonotonicity(t *testing.T) {
	for limit := 1.0; limit <= 64; limit++ {
		previous := 1.0
		for used := -limit; used <= 2*limit; used++ {
			w := QuotaWindow{Used: &used, Limit: &limit}
			got := w.Remaining()
			want := (limit - used) / limit
			if want < 0 {
				want = 0
			}
			if want > 1 {
				want = 1
			}
			if got == nil || *got != want || *got < 0 || *got > 1 {
				t.Fatalf("limit=%v used=%v remaining=%v want %v in [0,1]", limit, used, got, want)
			}
			if *got > previous {
				t.Fatalf("remaining increased as used increased: limit=%v used=%v got=%v prior=%v", limit, used, got, previous)
			}
			previous = *got
		}
	}
	for pct := -100.0; pct <= 200; pct++ {
		w := QuotaWindow{UsagePercent: &pct}
		got := w.Remaining()
		want := (100 - pct) / 100
		if want < 0 {
			want = 0
		}
		if want > 1 {
			want = 1
		}
		if got == nil || *got != want || *got < 0 || *got > 1 {
			t.Fatalf("percent=%v remaining=%v want %v", pct, got, want)
		}
	}
}

func FuzzRemainingBoundsAndMonotonicity(f *testing.F) {
	for _, seed := range [][2]int64{{1, 0}, {1, 1}, {10, -5}, {10, 20}, {100, 25}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, rawLimit, rawUsed int64) {
		limit := float64(uint64(rawLimit)%1_000_000 + 1)
		used := float64(rawUsed % 2_000_001)
		got := (QuotaWindow{Used: &used, Limit: &limit}).Remaining()
		want := (limit - used) / limit
		if want < 0 {
			want = 0
		}
		if want > 1 {
			want = 1
		}
		if got == nil || math.IsNaN(*got) || *got < 0 || *got > 1 || *got != want {
			t.Fatalf("limit=%v used=%v remaining=%v want=%v", limit, used, got, want)
		}
		moreUsed := used + 1
		if later := (QuotaWindow{Used: &moreUsed, Limit: &limit}).Remaining(); later == nil || *later > *got {
			t.Fatalf("remaining not monotone: limit=%v used=%v got=%v later=%v", limit, used, got, later)
		}
	})
}

func TestPropertyCodexIndividualLimitSourcePrecedence(t *testing.T) {
	const body = `{"individual_limit":{"limit":100,"used":25},"rate_limit":{"individual_limit":{"limit":80,"used":40}}}`
	windows, _, partial, err := parseCodexUsage([]byte(body), time.Unix(1, 0))
	if err != nil || partial {
		t.Fatalf("parse: windows=%+v partial=%v err=%v", windows, partial, err)
	}
	var got *QuotaWindow
	for i := range windows {
		if windows[i].Name == "spend-control" {
			got = &windows[i]
			break
		}
	}
	if got == nil || got.Limit == nil || got.Used == nil || *got.Limit != 100 || *got.Used != 25 {
		t.Fatalf("top-level source did not take precedence: %+v", got)
	}
}

func FuzzCodexIndividualLimitPrecedence(f *testing.F) {
	f.Add(int64(100), int64(25), int64(80), int64(40))
	f.Add(int64(1), int64(0), int64(2), int64(1))
	f.Fuzz(func(t *testing.T, topLimit, topUsed, nestedLimit, nestedUsed int64) {
		topLimit = int64(uint64(topLimit)%10000 + 1)
		nestedLimit = int64(uint64(nestedLimit)%10000 + 1)
		topUsed = int64(uint64(topUsed) % 10000)
		nestedUsed = int64(uint64(nestedUsed) % 10000)
		body, err := json.Marshal(map[string]any{"individual_limit": map[string]int64{"limit": topLimit, "used": topUsed}, "rate_limit": map[string]any{"individual_limit": map[string]int64{"limit": nestedLimit, "used": nestedUsed}}})
		if err != nil {
			t.Fatal(err)
		}
		windows, _, partial, err := parseCodexUsage(body, time.Unix(1, 0))
		if err != nil || partial {
			t.Fatalf("parse partial=%v err=%v", partial, err)
		}
		for _, w := range windows {
			if w.Name == "spend-control" {
				if w.Limit == nil || w.Used == nil || *w.Limit != float64(topLimit) || *w.Used != float64(topUsed) {
					t.Fatalf("got limit/used %v/%v; top-level oracle %d/%d", w.Limit, w.Used, topLimit, topUsed)
				}
				return
			}
		}
		t.Fatal("missing spend-control window")
	})
}

func TestPropertyPartialEvidenceAndResetBoundaries(t *testing.T) {
	windows, partial, err := parseAnthropicOAuthUsage([]byte(`{"five_hour":{"utilization":25},"seven_day":{"utilization":"bad"}}`))
	if err != nil || !partial || len(windows) != 1 || windows[0].Name != "session" {
		t.Fatalf("partial parse: windows=%+v partial=%v err=%v", windows, partial, err)
	}
	if _, _, err := parseAnthropicOAuthUsage([]byte(`{"five_hour":{},"seven_day":null}`)); err == nil {
		t.Fatal("response with no usable windows unexpectedly parsed")
	}
	asOf := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		expiry time.Time
		want   int
	}{{"before observation", asOf.Add(-time.Nanosecond), 0}, {"at observation", asOf, 0}, {"after observation", asOf.Add(time.Nanosecond), 1}} {
		t.Run(tc.name, func(t *testing.T) {
			state := ResetCreditState{LastSuccess: &ResetCreditInventory{AvailableExpiries: []*time.Time{&tc.expiry}}}
			if got := state.UsableCountAt(asOf); got == nil || *got != tc.want {
				t.Fatalf("usable count=%v want %d", got, tc.want)
			}
		})
	}
}

func TestPropertyFailedResetAttemptPreservesLastSuccess(t *testing.T) {
	inventory := &ResetCreditInventory{ServerAvailableCount: 2, UsableCount: 2}
	prior := ResetCreditState{LastSuccess: inventory}
	got := MergeResetCreditObservation(prior, ResetCreditAttempt{Status: CreditAttemptFailed, Error: "synthetic failure"})
	if got.LastSuccess != inventory || got.LatestAttempt == nil || got.LatestAttempt.Status != CreditAttemptFailed {
		t.Fatalf("failed optional attempt altered success provenance: %+v", got)
	}
}

func TestPropertyResetEpochUnitBoundary(t *testing.T) {
	const cutoff = float64(10_000_000_000)
	values := []float64{cutoff - 1, cutoff - 0.1, cutoff, cutoff + 0.1, cutoff + 1, cutoff*10 - 1, cutoff * 10, cutoff*10 + 1}
	for _, value := range values {
		want := time.Unix(int64(value), 0)
		if value > cutoff {
			want = time.UnixMilli(int64(value))
		}
		encoded := strconv.FormatFloat(value, 'f', -1, 64)
		for _, raw := range [][]byte{[]byte(encoded), []byte(`"` + encoded + `"`)} {
			got, ok := resetFromRaw(raw)
			if !ok || !got.Equal(want) {
				t.Fatalf("resetFromRaw(%s)=%v,%v want %v", raw, got, ok, want)
			}
		}
	}
}

func TestPropertyMalformedResetValuesAreRejected(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "[]", `"not-a-time"`, `"Infinity"`, `0`, `-1`, `"0"`} {
		if got, ok := resetFromRaw(json.RawMessage(raw)); ok {
			t.Errorf("resetFromRaw(%q)=%v,true; malformed/non-positive input must be rejected", raw, got)
		}
	}
}
