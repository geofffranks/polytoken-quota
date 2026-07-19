package state

import (
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/hook"
)

// seedState returns a clean state tracking a single healthy codex provider with
// zero (unusable) accepted timestamps, so any real event is newer.
func seedState() State {
	return State{
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaNormal, Availability: Available},
		},
	}
}

// event builds a hook.Event for the codex provider whose timestamp is rooted at
// the Unix epoch second n. Larger n means newer.
func event(typ hook.Type, n int) hook.Event {
	return hook.Event{Type: typ, Provider: "codex", Timestamp: time.Unix(int64(n), 0)}
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		event        hook.Type
		quota        Quota
		availability Availability
		mode         Mode
	}{
		{hook.QuotaLow, QuotaLow, Available, ModeReserve},
		{hook.QuotaReached, QuotaExhausted, Available, ModeDisabled},
		{hook.QuotaReset, QuotaNormal, Available, ModeNormal},
		{hook.ProviderUnavailable, QuotaNormal, Unavailable, ModeDisabled},
		{hook.ProviderRecovered, QuotaNormal, Available, ModeNormal},
		{hook.RefreshFailed, QuotaNormal, Available, ModeNormal},
	}
	for _, tc := range cases {
		next, accepted, _, err := ApplyEvent(seedState(), event(tc.event, 2), Arrival{Sequence: 2})
		if err != nil || !accepted ||
			next.Providers["codex"].Quota != tc.quota ||
			next.Providers["codex"].Availability != tc.availability ||
			EffectiveMode(next.Providers["codex"]) != tc.mode {
			t.Fatalf("case=%+v next=%+v err=%v", tc, next, err)
		}
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

func TestAxesStaleDuplicateAndEqualArrival(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaReached, 10), Arrival{Sequence: 10})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderUnavailable, 11), Arrival{Sequence: 11})
	s, accepted, _, _ := ApplyEvent(s, event(hook.QuotaReset, 9), Arrival{Sequence: 12})
	if accepted {
		t.Fatal("accepted stale quota event")
	}
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderRecovered, 12), Arrival{Sequence: 13})
	if s.Providers["codex"].Quota != QuotaExhausted {
		t.Fatal("recovery cleared quota")
	}
}

func TestRecoveryDoesNotClearQuota(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaLow, 5), Arrival{Sequence: 1})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderUnavailable, 6), Arrival{Sequence: 2})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderRecovered, 7), Arrival{Sequence: 3})
	ps := s.Providers["codex"]
	if ps.Quota != QuotaLow {
		t.Fatalf("recovery cleared quota: %+v", ps)
	}
	if ps.Availability != Available {
		t.Fatalf("recovery did not restore availability: %+v", ps)
	}
	if EffectiveMode(ps) != ModeReserve {
		t.Fatalf("mode=%s want reserve", EffectiveMode(ps))
	}
}

func TestResetDoesNotClearAvailability(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaReached, 5), Arrival{Sequence: 1})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderUnavailable, 6), Arrival{Sequence: 2})
	// Newer quota reset restores quota but must not clear the unavailable axis.
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaReset, 7), Arrival{Sequence: 3})
	ps := s.Providers["codex"]
	if ps.Availability != Unavailable {
		t.Fatalf("reset cleared availability: %+v", ps)
	}
	if ps.Quota != QuotaNormal {
		t.Fatalf("reset did not restore quota: %+v", ps)
	}
	if EffectiveMode(ps) != ModeDisabled {
		t.Fatalf("mode=%s want disabled", EffectiveMode(ps))
	}
}

func TestRefreshFailedDiagnosticOnly(t *testing.T) {
	s := seedState()
	status := "rate limited"
	e := hook.Event{Type: hook.RefreshFailed, Provider: "codex", Timestamp: time.Unix(8, 0), Status: &status}
	next, accepted, _, err := ApplyEvent(s, e, Arrival{Sequence: 1})
	if err != nil || !accepted {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	if got := EffectiveMode(next.Providers["codex"]); got != ModeNormal {
		t.Fatalf("refresh_failed changed mode to %s", got)
	}
	if len(next.RefreshFailed) != 1 {
		t.Fatalf("refresh failed diag count=%d", len(next.RefreshFailed))
	}
	d := next.RefreshFailed[0]
	if d.Provider != "codex" || d.Code != string(hook.RefreshFailed) || d.Summary != status || !d.At.Equal(time.Unix(8, 0)) {
		t.Fatalf("diagnostic=%+v", d)
	}
}

func TestStaleEventRejectedPerAxis(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaLow, 10), Arrival{Sequence: 10})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderUnavailable, 10), Arrival{Sequence: 10})
	// Older quota event is stale; the independent availability axis is untouched.
	next, accepted, diag, err := ApplyEvent(s, event(hook.QuotaReset, 5), Arrival{Sequence: 99})
	if err != nil || accepted {
		t.Fatalf("accepted stale quota accepted=%v err=%v", accepted, err)
	}
	if diag.Code != "stale" || diag.Provider != "codex" {
		t.Fatalf("diag=%+v", diag)
	}
	if next.Providers["codex"].Quota != QuotaLow {
		t.Fatalf("stale event mutated quota: %+v", next.Providers["codex"])
	}
	if next.Providers["codex"].Availability != Unavailable {
		t.Fatalf("stale quota event mutated availability: %+v", next.Providers["codex"])
	}
}

func TestDuplicateIdempotent(t *testing.T) {
	s := seedState()
	s1, acc1, _, err := ApplyEvent(s, event(hook.QuotaLow, 10), Arrival{Sequence: 10})
	if err != nil || !acc1 {
		t.Fatalf("first accept=%v err=%v", acc1, err)
	}
	// Re-delivery of the same event with a later arrival is accepted and idempotent.
	s2, acc2, _, err := ApplyEvent(s1, event(hook.QuotaLow, 10), Arrival{Sequence: 11})
	if err != nil || !acc2 {
		t.Fatalf("second accept=%v err=%v", acc2, err)
	}
	ps := s2.Providers["codex"]
	if ps.Quota != QuotaLow {
		t.Fatalf("quota=%s want low", ps.Quota)
	}
	if ps.QuotaArrival != 11 || !ps.QuotaAt.Equal(time.Unix(10, 0)) {
		t.Fatalf("arrival metadata=%+v", ps)
	}
}

func TestEqualTimestampOrderedByArrival(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaLow, 10), Arrival{Sequence: 10})
	// Equal timestamp, later arrival wins.
	next, accepted, _, err := ApplyEvent(s, event(hook.QuotaReset, 10), Arrival{Sequence: 11})
	if err != nil || !accepted {
		t.Fatalf("equal-ts newer-arrival accepted=%v err=%v", accepted, err)
	}
	if next.Providers["codex"].Quota != QuotaNormal {
		t.Fatalf("quota=%s want normal", next.Providers["codex"].Quota)
	}
	// Equal timestamp, older arrival is stale.
	next2, accepted2, _, err := ApplyEvent(next, event(hook.QuotaLow, 10), Arrival{Sequence: 9})
	if err != nil || accepted2 {
		t.Fatalf("equal-ts older-arrival accepted=%v err=%v", accepted2, err)
	}
	if next2.Providers["codex"].Quota != QuotaNormal {
		t.Fatalf("older arrival mutated quota: %s", next2.Providers["codex"].Quota)
	}
}

func TestUnusableTimestampOrderedByArrival(t *testing.T) {
	s := State{Providers: map[string]ProviderState{
		"codex": {Quota: QuotaLow, QuotaArrival: 5}, // unusable timestamp
	}}
	// Newer arrival with an unusable timestamp is accepted by arrival order.
	next, accepted, _, err := ApplyEvent(s, hook.Event{Type: hook.QuotaReset, Provider: "codex"}, Arrival{Sequence: 6})
	if err != nil || !accepted {
		t.Fatalf("unusable-ts newer-arrival accepted=%v err=%v", accepted, err)
	}
	if next.Providers["codex"].Quota != QuotaNormal {
		t.Fatalf("quota=%s want normal", next.Providers["codex"].Quota)
	}
	// Older arrival with an unusable timestamp is stale.
	next2, accepted2, _, err := ApplyEvent(next, hook.Event{Type: hook.QuotaLow, Provider: "codex"}, Arrival{Sequence: 4})
	if err != nil || accepted2 {
		t.Fatalf("unusable-ts older-arrival accepted=%v err=%v", accepted2, err)
	}
	if next2.Providers["codex"].Quota != QuotaNormal {
		t.Fatalf("older arrival mutated quota: %s", next2.Providers["codex"].Quota)
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
	// Manual override dominates a subsequent stale event.
	next2, accepted, _, _ := ApplyEvent(next, event(hook.QuotaReset, 1), Arrival{Sequence: 99})
	if accepted || next2.Providers["codex"].Quota != QuotaLow {
		t.Fatalf("manual override did not dominate stale event: accepted=%v", accepted)
	}
	// An empty patch is rejected.
	if _, err := SetProvider(s, "codex", ProviderPatch{}, at); err == nil {
		t.Fatal("empty patch accepted")
	}
}

func TestClearProviderResetsBaseline(t *testing.T) {
	s := seedState()
	s, _, _, _ = ApplyEvent(s, event(hook.QuotaReached, 5), Arrival{Sequence: 1})
	s, _, _, _ = ApplyEvent(s, event(hook.ProviderUnavailable, 6), Arrival{Sequence: 2})
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
