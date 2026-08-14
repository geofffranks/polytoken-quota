package state

import (
	"testing"
	"time"
)

// seedState returns a clean state tracking a single healthy codex provider.
func seedState() State {
	return State{
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaNormal, Availability: Available},
		},
	}
}

func TestEffectiveModeFormula(t *testing.T) {
	cases := []struct {
		quota        Quota
		availability Availability
		mode         Mode
	}{
		{QuotaNormal, Available, ModeNormal},
		{QuotaLow, Available, ModeReserve},
		{QuotaExhausted, Available, ModeDisabled},
		{QuotaNormal, Unavailable, ModeDisabled},
		{QuotaLow, Unavailable, ModeDisabled},
		{QuotaExhausted, Unavailable, ModeDisabled},
	}
	for _, tc := range cases {
		ps := ProviderState{Quota: tc.quota, Availability: tc.availability}
		if got := EffectiveMode(ps); got != tc.mode {
			t.Fatalf("quota=%s availability=%s mode=%s want=%s", tc.quota, tc.availability, got, tc.mode)
		}
	}
}

// TestEffectiveModeZeroAndUnknownValues proves zero axis values normalize to
// the healthy baseline (so a sparse {low, ""} state is reserve, not normal)
// while unrecognized enum values fail closed to disabled.
func TestEffectiveModeZeroAndUnknownValues(t *testing.T) {
	cases := []struct {
		quota        Quota
		availability Availability
		mode         Mode
	}{
		{"", "", ModeNormal},                 // legacy sparse baseline
		{QuotaLow, "", ModeReserve},          // "" availability = available
		{"", Unavailable, ModeDisabled},      // "" quota = normal
		{"garbage", Available, ModeDisabled}, // corrupted quota fails closed
		{QuotaNormal, "garbage", ModeDisabled},
		{"garbage", "garbage", ModeDisabled},
	}
	for _, tc := range cases {
		ps := ProviderState{Quota: tc.quota, Availability: tc.availability}
		if got := EffectiveMode(ps); got != tc.mode {
			t.Fatalf("quota=%q availability=%q mode=%s want=%s", tc.quota, tc.availability, got, tc.mode)
		}
	}
}

func TestSetProviderOverride(t *testing.T) {
	s := seedState()
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	low := QuotaLow
	next, err := SetProvider(s, "codex", ProviderPatch{Quota: &low}, at)
	if err != nil {
		t.Fatal(err)
	}
	ps := next.Providers["codex"]
	if ps.Quota != QuotaLow || !ps.QuotaAt.Equal(at) {
		t.Fatalf("ps=%+v", ps)
	}
	if ps.Availability != Available {
		t.Fatalf("availability=%s", ps.Availability)
	}
	// An empty patch is rejected.
	if _, err := SetProvider(s, "codex", ProviderPatch{}, at); err == nil {
		t.Fatal("empty patch accepted")
	}
}

func TestClearProviderResetsBaseline(t *testing.T) {
	s := seedState()
	s.Providers["codex"] = ProviderState{Quota: QuotaExhausted, Availability: Unavailable}
	at := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	next, err := ClearProvider(s, Selector{Provider: "codex"}, at)
	if err != nil {
		t.Fatal(err)
	}
	ps := next.Providers["codex"]
	if ps.Quota != QuotaNormal || ps.Availability != Available {
		t.Fatalf("ps=%+v", ps)
	}
	if !ps.QuotaAt.Equal(at) || !ps.AvailabilityAt.Equal(at) {
		t.Fatalf("timestamps=%+v", ps)
	}
	if EffectiveMode(ps) != ModeNormal {
		t.Fatalf("mode=%s", EffectiveMode(ps))
	}
	// --all resets every provider.
	s2 := seedState()
	s2.Providers["zai"] = ProviderState{Quota: QuotaExhausted, Availability: Unavailable}
	next2, err := ClearProvider(s2, Selector{All: true}, at)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range next2.Providers {
		if p.Quota != QuotaNormal || p.Availability != Available {
			t.Fatalf("%s not reset: %+v", name, p)
		}
	}
}

func TestManualDisableOverridesAutomaticState(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	next, err := DisableProvider(seedState(), "codex", at)
	if err != nil {
		t.Fatal(err)
	}
	ps := next.Providers["codex"]
	if !ps.ManualDisabled || !ps.ManualDisabledAt.Equal(at) {
		t.Fatalf("manual state=%+v", ps)
	}
	if got := EffectiveMode(ps); got != ModeDisabled {
		t.Fatalf("mode=%s want %s", got, ModeDisabled)
	}
}

func TestEnableProviderResumesAutomaticState(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	low := QuotaLow
	s, err := SetProvider(seedState(), "codex", ProviderPatch{Quota: &low}, at)
	if err != nil {
		t.Fatal(err)
	}
	s, err = DisableProvider(s, "codex", at)
	if err != nil {
		t.Fatal(err)
	}
	next, err := EnableProvider(s, "codex", at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ps := next.Providers["codex"]
	if ps.ManualDisabled || !ps.ManualDisabledAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("manual state=%+v", ps)
	}
	if got := EffectiveMode(ps); got != ModeReserve {
		t.Fatalf("mode=%s want %s", got, ModeReserve)
	}
}

func TestResetManualDisablesPreservesAutomaticObservations(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	exhausted := QuotaExhausted
	unavailable := Unavailable
	s, err := SetProvider(seedState(), "codex", ProviderPatch{Quota: &exhausted, Availability: &unavailable}, at)
	if err != nil {
		t.Fatal(err)
	}
	original := s.Providers["codex"]
	s.Providers["zai"] = ProviderState{Quota: QuotaNormal, Availability: Available}
	s, err = DisableProvider(s, "codex", at)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ResetManualDisables(s, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ps := next.Providers["codex"]
	if ps.ManualDisabled {
		t.Fatal("reset left manual disable active")
	}
	if !next.Providers["zai"].ManualDisabledAt.IsZero() {
		t.Fatalf("reset changed inactive manual timestamp: %+v", next.Providers["zai"])
	}
	if ps.Quota != original.Quota || ps.Availability != original.Availability ||
		!ps.QuotaAt.Equal(original.QuotaAt) || !ps.AvailabilityAt.Equal(original.AvailabilityAt) ||
		ps.QuotaArrival != original.QuotaArrival || ps.AvailabilityArrival != original.AvailabilityArrival {
		t.Fatalf("reset changed automatic observations: before=%+v after=%+v", original, ps)
	}
	if got := EffectiveMode(ps); got != ModeDisabled {
		t.Fatalf("mode=%s want %s", got, ModeDisabled)
	}
}
