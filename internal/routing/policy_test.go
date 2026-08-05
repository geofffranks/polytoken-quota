package routing

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// ----- shared test helpers -------------------------------------------------

func fptr(v float64) *float64     { x := v; return &x }
func tptr(t time.Time) *time.Time { x := t; return &x }

var allDays = []DayOfWeek{Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday}

// rankNow is the stable injected "now" used across ranking tests.
var rankNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// dayOf maps a time.Weekday to the canonical DayOfWeek abbreviation.
func dayOf(t time.Time) DayOfWeek {
	switch t.Weekday() {
	case time.Sunday:
		return Sunday
	case time.Monday:
		return Monday
	case time.Tuesday:
		return Tuesday
	case time.Wednesday:
		return Wednesday
	case time.Thursday:
		return Thursday
	case time.Friday:
		return Friday
	default:
		return Saturday
	}
}

// alwaysOffPeak is a schedule that is off-peak at any moment in LA.
func alwaysOffPeak(t *testing.T) Schedule {
	t.Helper()
	s, err := ParseSchedule("America/Los_Angeles", []OffPeakWindow{{Days: allDays, Start: "00:00", End: "24:00"}})
	if err != nil {
		t.Fatalf("parse alwaysOffPeak: %v", err)
	}
	return s
}

// remSnap builds a fresh, available snapshot with one usable window reporting
// remaining fraction rem (via used/limit).
func remSnap(mid string, rem float64, checkedAt time.Time) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		MappingID:    mid,
		CheckedAt:    checkedAt,
		Status:       quota.SourceFresh,
		Availability: quota.QuotaAvailable,
		Windows: []quota.QuotaWindow{{
			Name:  "primary",
			Used:  fptr(1 - rem),
			Limit: fptr(1.0),
		}},
	}
}

// remSnapReset is remSnap with a window reset time.
func remSnapReset(mid string, rem float64, checkedAt, reset time.Time) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		MappingID:    mid,
		CheckedAt:    checkedAt,
		Status:       quota.SourceFresh,
		Availability: quota.QuotaAvailable,
		Windows: []quota.QuotaWindow{{
			Name:    "primary",
			Used:    fptr(1 - rem),
			Limit:   fptr(1.0),
			ResetAt: tptr(reset),
		}},
	}
}

// order returns the ordered mapping IDs of a ranking result.
func order(r RankingResult) []string {
	out := make([]string, len(r.Entries))
	for i, e := range r.Entries {
		out[i] = e.MappingID
	}
	return out
}

func eqOrder(t *testing.T, r RankingResult, want ...string) {
	t.Helper()
	got := order(r)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// ----- Part A: schedule parsing --------------------------------------------

func TestParseScheduleValid(t *testing.T) {
	s, err := ParseSchedule("America/Los_Angeles", []OffPeakWindow{
		{Days: []DayOfWeek{Monday, Wednesday}, Start: "09:00", End: "17:00"},
		{Days: []DayOfWeek{Saturday}, Start: "22:00", End: "06:00"}, // midnight-crossing
		{Days: []DayOfWeek{Friday}, Start: "18:00", End: "24:00"},   // end sentinel
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Timezone != "America/Los_Angeles" {
		t.Fatalf("timezone = %q", s.Timezone)
	}
	if len(s.OffPeak) != 3 {
		t.Fatalf("windows = %d, want 3", len(s.OffPeak))
	}
}

func TestParseScheduleInvalidTimezone(t *testing.T) {
	if _, err := ParseSchedule("Not/A/Zone", nil); err == nil {
		t.Fatal("expected error for invalid timezone")
	}
}

func TestParseScheduleInvalidDay(t *testing.T) {
	if _, err := ParseSchedule("UTC", []OffPeakWindow{{Days: []DayOfWeek{"funday"}, Start: "00:00", End: "01:00"}}); err == nil {
		t.Fatal("expected error for invalid day abbreviation")
	}
}

func TestParseScheduleInvalidTime(t *testing.T) {
	for _, w := range []OffPeakWindow{
		{Days: allDays, Start: "25:00", End: "01:00"}, // hour out of range
		{Days: allDays, Start: "12:60", End: "01:00"}, // minute out of range
		{Days: allDays, Start: "9:00", End: "01:00"},  // wrong format (not HH:MM)
		{Days: allDays, Start: "abc", End: "01:00"},   // garbage
		{Days: allDays, Start: "24:00", End: "01:00"}, // 24:00 only valid as end
	} {
		if _, err := ParseSchedule("UTC", []OffPeakWindow{w}); err == nil {
			t.Fatalf("expected error for window %+v", w)
		}
	}
}

func TestParseScheduleEndSentinelBehavior(t *testing.T) {
	// 24:00 end sentinel accepted and behaves as end-of-day.
	s, err := ParseSchedule("America/Los_Angeles", []OffPeakWindow{{Days: []DayOfWeek{Monday}, Start: "18:00", End: "24:00"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mon := time.Date(2026, 6, 15, 23, 0, 0, 0, mustLoc(t, "America/Los_Angeles")) // Monday
	before := time.Date(2026, 6, 15, 17, 0, 0, 0, mustLoc(t, "America/Los_Angeles"))
	tueMidnight := time.Date(2026, 6, 16, 0, 0, 0, 0, mustLoc(t, "America/Los_Angeles"))
	if !s.IsOffPeak(mon) {
		t.Fatal("23:00 Monday should be off-peak for 18:00-24:00")
	}
	if s.IsOffPeak(before) {
		t.Fatal("17:00 Monday should be peak for 18:00-24:00")
	}
	if s.IsOffPeak(tueMidnight) {
		t.Fatal("00:00 Tuesday should be peak; 24:00 is end-of-day, not crossing")
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

// ----- Part B: off-peak evaluation -----------------------------------------

func TestIsOffPeakInsideOutside(t *testing.T) {
	s, err := ParseSchedule("UTC", []OffPeakWindow{{Days: allDays, Start: "09:00", End: "17:00"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	loc := time.UTC
	if !s.IsOffPeak(time.Date(2026, 6, 15, 12, 0, 0, 0, loc)) {
		t.Fatal("12:00 should be off-peak")
	}
	if s.IsOffPeak(time.Date(2026, 6, 15, 18, 0, 0, 0, loc)) {
		t.Fatal("18:00 should be peak")
	}
	// Half-open: end boundary excluded.
	if s.IsOffPeak(time.Date(2026, 6, 15, 17, 0, 0, 0, loc)) {
		t.Fatal("17:00 (end boundary) should be peak [Start,End)")
	}
}

func TestIsOffPeakMidnightCrossing(t *testing.T) {
	s, err := ParseSchedule("UTC", []OffPeakWindow{{Days: []DayOfWeek{Monday}, Start: "22:00", End: "06:00"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	loc := time.UTC
	monLate := time.Date(2026, 6, 15, 23, 0, 0, 0, loc) // Monday 23:00 -> before-midnight part
	tueEarly := time.Date(2026, 6, 16, 5, 0, 0, 0, loc) // Tuesday 05:00 -> after-midnight part (window started Mon)
	tueMidday := time.Date(2026, 6, 16, 12, 0, 0, 0, loc)
	if !s.IsOffPeak(monLate) {
		t.Fatal("Monday 23:00 should be off-peak (22:00->06:00)")
	}
	if !s.IsOffPeak(tueEarly) {
		t.Fatal("Tuesday 05:00 should be off-peak (window started Monday)")
	}
	if s.IsOffPeak(tueMidday) {
		t.Fatal("Tuesday 12:00 should be peak")
	}
	// Sunday late should NOT match (window only starts Monday).
	sunLate := time.Date(2026, 6, 14, 23, 0, 0, 0, loc)
	if s.IsOffPeak(sunLate) {
		t.Fatal("Sunday 23:00 should be peak (window starts Monday)")
	}
}

func TestIsOffPeakDayOfWeek(t *testing.T) {
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) // some weekday at 12:00
	next := base.AddDate(0, 0, 1)                         // next calendar day, same wall time (UTC)
	d := dayOf(base)
	s, err := ParseSchedule("UTC", []OffPeakWindow{{Days: []DayOfWeek{d}, Start: "09:00", End: "17:00"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.IsOffPeak(base) {
		t.Fatal("base day 12:00 should be off-peak")
	}
	if s.IsOffPeak(next) {
		t.Fatal("next day 12:00 should be peak (different weekday)")
	}
}

func TestIsOffPeakDST(t *testing.T) {
	loc := mustLoc(t, "America/Los_Angeles")
	// Spring forward in 2026 happens March 8 at 02:00->03:00. A 00:00-08:00 window
	// must match by wall-clock regardless of the offset jump.
	s, err := ParseSchedule("America/Los_Angeles", []OffPeakWindow{{Days: allDays, Start: "00:00", End: "08:00"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	at5 := time.Date(2026, 3, 8, 5, 0, 0, 0, loc) // 05:00 PT on spring-forward day
	at9 := time.Date(2026, 3, 8, 9, 0, 0, 0, loc)
	if !s.IsOffPeak(at5) {
		t.Fatal("05:00 on spring-forward day should be off-peak")
	}
	if s.IsOffPeak(at9) {
		t.Fatal("09:00 should be peak")
	}
	// Consistent across the boundary: day before (standard) and day after (DST).
	if !s.IsOffPeak(time.Date(2026, 3, 7, 5, 0, 0, 0, loc)) {
		t.Fatal("05:00 day before DST should be off-peak")
	}
}

func TestIsOffPeakDifferentTimezones(t *testing.T) {
	// Same UTC instant is off-peak in one timezone, peak in another.
	ny, err := ParseSchedule("America/New_York", []OffPeakWindow{{Days: []DayOfWeek{Monday}, Start: "09:00", End: "17:00"}})
	if err != nil {
		t.Fatalf("parse ny: %v", err)
	}
	la, err := ParseSchedule("America/Los_Angeles", []OffPeakWindow{{Days: []DayOfWeek{Monday}, Start: "09:00", End: "17:00"}})
	if err != nil {
		t.Fatalf("parse la: %v", err)
	}
	instant := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC) // Monday 14:00 UTC
	// 14:00 UTC = 10:00 EDT (off-peak in NY) = 07:00 PDT (peak in LA).
	if !ny.IsOffPeak(instant) {
		t.Fatal("instant should be off-peak in New York")
	}
	if la.IsOffPeak(instant) {
		t.Fatal("instant should be peak in Los Angeles")
	}
}

func TestIsOffPeakNilSchedule(t *testing.T) {
	var s Schedule // no timezone, no windows
	if s.IsOffPeak(rankNow) {
		t.Fatal("zero-value schedule should never report off-peak")
	}
}

// ----- Part D: eligibility -------------------------------------------------

func TestCheckEligibilityRankable(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	obs := ProviderObs{MappingID: "p", Mode: "normal", Snapshot: remSnap("p", 0.5, rankNow)}
	e := CheckEligibility(p, obs, rankNow)
	if !e.Rankable {
		t.Fatalf("expected rankable, got reason %q", e.Reason)
	}
	if e.Reason != "" {
		t.Fatalf("rankable reason should be empty, got %q", e.Reason)
	}
}

func TestCheckEligibilityDisabled(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	obs := ProviderObs{MappingID: "p", Mode: "disabled", Snapshot: remSnap("p", 0.5, rankNow)}
	e := CheckEligibility(p, obs, rankNow)
	if e.Rankable {
		t.Fatal("disabled should not be rankable")
	}
	if !strings.Contains(e.Reason, "disabled") {
		t.Fatalf("reason %q should mention disabled", e.Reason)
	}
}

func TestCheckEligibilityStale(t *testing.T) {
	p := ProviderPolicy{MappingID: "p", FreshnessTTL: 30 * time.Minute}
	obs := ProviderObs{MappingID: "p", Mode: "normal", Snapshot: remSnap("p", 0.5, rankNow.Add(-31*time.Minute))}
	e := CheckEligibility(p, obs, rankNow)
	if e.Rankable {
		t.Fatal("stale snapshot should not be rankable")
	}
	if !strings.Contains(e.Reason, "fresh") {
		t.Fatalf("reason %q should mention freshness", e.Reason)
	}
}

func TestCheckEligibilityNilSnapshot(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	obs := ProviderObs{MappingID: "p", Mode: "normal", Snapshot: nil}
	e := CheckEligibility(p, obs, rankNow)
	if e.Rankable {
		t.Fatal("nil snapshot should not be rankable")
	}
	if !strings.Contains(e.Reason, "fresh") {
		t.Fatalf("reason %q should mention freshness", e.Reason)
	}
}

func TestCheckEligibilityAllUnknown(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := &quota.QuotaSnapshot{
		MappingID:    "p",
		CheckedAt:    rankNow,
		Status:       quota.SourceFresh,
		Availability: quota.QuotaUnknown,
		Windows:      []quota.QuotaWindow{{Name: "primary"}}, // no usable values
	}
	obs := ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}
	e := CheckEligibility(p, obs, rankNow)
	if e.Rankable {
		t.Fatal("all-unknown snapshot should not be rankable")
	}
	if !strings.Contains(e.Reason, "usable") {
		t.Fatalf("reason %q should mention usable data", e.Reason)
	}
}

// TestCheckEligibilityUnavailable proves a fresh snapshot that explicitly
// reports quota exhausted/unavailable is never rankable, even when it still
// carries usable window numbers.
func TestCheckEligibilityUnavailable(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := remSnap("p", 0.0, rankNow)
	snap.Availability = quota.QuotaUnavailable
	e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
	if e.Rankable {
		t.Fatal("unavailable snapshot should not be rankable")
	}
	if !strings.Contains(e.Reason, "unavailable") {
		t.Fatalf("reason %q should mention unavailability", e.Reason)
	}
}

// TestCheckEligibilityZeroRemaining proves an available snapshot with no
// remaining headroom fails closed rather than ranking an exhausted provider.
func TestCheckEligibilityZeroRemaining(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := remSnap("p", 0.0, rankNow) // used == limit → remaining 0
	e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
	if e.Rankable {
		t.Fatal("zero-remaining snapshot should not be rankable")
	}
}

// TestCheckEligibilityInvalidAvailabilityEnum proves an unrecognized or empty
// availability enum value fails closed.
func TestCheckEligibilityInvalidAvailabilityEnum(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	for _, v := range []quota.QuotaAvailability{"", "garbage"} {
		snap := remSnap("p", 0.5, rankNow)
		snap.Availability = v
		e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
		if e.Rankable {
			t.Fatalf("availability %q should not be rankable", v)
		}
	}
}

func TestCheckEligibilityFreshUsableSignalButUnknownAvailability(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	used, limit := 1.0, 2.0
	snap := &quota.QuotaSnapshot{MappingID: "p", CheckedAt: rankNow, Status: quota.SourceFresh, Availability: quota.QuotaUnknown, Windows: []quota.QuotaWindow{{Used: &used, Limit: &limit}}}
	e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
	if e.Rankable || !strings.Contains(e.Reason, "usable") {
		t.Fatalf("eligibility=%+v, want ineligible unknown availability", e)
	}
}

func TestCheckEligibilityFreshNoRemainingSignal(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := &quota.QuotaSnapshot{MappingID: "p", CheckedAt: rankNow, Status: quota.SourceFresh, Availability: quota.QuotaAvailable, Windows: []quota.QuotaWindow{{Name: "primary"}}}
	e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
	if e.Rankable || !strings.Contains(e.Reason, "usable") {
		t.Fatalf("eligibility=%+v, want ineligible without remaining signal", e)
	}
}

func TestCheckEligibilityAuthFailure(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := &quota.QuotaSnapshot{
		MappingID:    "p",
		CheckedAt:    rankNow,
		Status:       quota.SourceFailed,
		Availability: quota.QuotaAvailable,
		Windows:      []quota.QuotaWindow{{Name: "primary", Used: fptr(0.5), Limit: fptr(1.0)}},
	}
	obs := ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}
	e := CheckEligibility(p, obs, rankNow)
	if e.Rankable {
		t.Fatal("failed source should not be rankable")
	}
	if !strings.Contains(e.Reason, "authentication") && !strings.Contains(e.Reason, "configuration") {
		t.Fatalf("reason %q should mention authentication/configuration", e.Reason)
	}
}

// ----- Part E: lexicographic ranking ---------------------------------------

func TestRankOffPeakDominates(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "off", Schedule: &offPeak},
			{MappingID: "peak"},
		},
		Obs: []ProviderObs{
			{MappingID: "off", Mode: "normal", Snapshot: remSnap("off", 0.21, rankNow)},
			{MappingID: "peak", Mode: "normal", Snapshot: remSnap("peak", 0.78, rankNow)},
		},
	}
	eqOrder(t, Rank(in), "off", "peak")
}

func TestRankHeadroomTieBreak(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "low"}, {MappingID: "high"}},
		Obs: []ProviderObs{
			{MappingID: "low", Mode: "normal", Snapshot: remSnap("low", 0.21, rankNow)},
			{MappingID: "high", Mode: "normal", Snapshot: remSnap("high", 0.78, rankNow)},
		},
	}
	eqOrder(t, Rank(in), "high", "low")
}

func TestRankUsageTieBreak(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "a"}, {MappingID: "b"}},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: remSnap("a", 0.5, rankNow)},
			{MappingID: "b", Mode: "normal", Snapshot: remSnap("b", 0.5, rankNow)},
		},
		Usage: []UsageShare{
			{MappingID: "a", Share: 0.1},
			{MappingID: "b", Share: 0.3},
		},
	}
	eqOrder(t, Rank(in), "a", "b")
}

func TestRankResetTieBreak(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "sooner"}, {MappingID: "later"}},
		Obs: []ProviderObs{
			{MappingID: "sooner", Mode: "normal", Snapshot: remSnapReset("sooner", 0.5, rankNow, rankNow.Add(1*time.Hour))},
			{MappingID: "later", Mode: "normal", Snapshot: remSnapReset("later", 0.5, rankNow, rankNow.Add(2*time.Hour))},
		},
	}
	eqOrder(t, Rank(in), "sooner", "later")
}

func TestRankWeightTieBreak(t *testing.T) {
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "low", Weight: 1},
			{MappingID: "high", Weight: 5},
		},
		Obs: []ProviderObs{
			{MappingID: "low", Mode: "normal", Snapshot: remSnap("low", 0.5, rankNow)},
			{MappingID: "high", Mode: "normal", Snapshot: remSnap("high", 0.5, rankNow)},
		},
	}
	eqOrder(t, Rank(in), "high", "low")
}

func TestRankLexicalTieBreak(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "zebra"}, {MappingID: "alpha"}},
		Obs: []ProviderObs{
			{MappingID: "zebra", Mode: "normal", Snapshot: remSnap("zebra", 0.5, rankNow)},
			{MappingID: "alpha", Mode: "normal", Snapshot: remSnap("alpha", 0.5, rankNow)},
		},
	}
	eqOrder(t, Rank(in), "alpha", "zebra")
}

// ----- Part 5: incomparable units ------------------------------------------

func TestRankIncomparableUnits(t *testing.T) {
	t.Run("two unknown share ranked by headroom", func(t *testing.T) {
		in := RankingInput{
			Now:      rankNow,
			Policies: []ProviderPolicy{{MappingID: "a"}, {MappingID: "b"}},
			Obs: []ProviderObs{
				{MappingID: "a", Mode: "normal", Snapshot: remSnap("a", 0.7, rankNow)},
				{MappingID: "b", Mode: "normal", Snapshot: remSnap("b", 0.4, rankNow)},
			},
			Usage: []UsageShare{
				{MappingID: "a", Share: -1},
				{MappingID: "b", Share: -1},
			},
		}
		// Both unknown -> usage key skipped -> headroom decides: a before b.
		eqOrder(t, Rank(in), "a", "b")
	})
	t.Run("one unknown skips usage for whole group", func(t *testing.T) {
		// Without usage, headroom orders: a(0.7), b(0.4), c(0.1).
		// If usage were (wrongly) applied, b(0.2) would beat a(0.9). It must NOT.
		in := RankingInput{
			Now:      rankNow,
			Policies: []ProviderPolicy{{MappingID: "a"}, {MappingID: "b"}, {MappingID: "c"}},
			Obs: []ProviderObs{
				{MappingID: "a", Mode: "normal", Snapshot: remSnap("a", 0.7, rankNow)},
				{MappingID: "b", Mode: "normal", Snapshot: remSnap("b", 0.4, rankNow)},
				{MappingID: "c", Mode: "normal", Snapshot: remSnap("c", 0.1, rankNow)},
			},
			Usage: []UsageShare{
				{MappingID: "a", Share: 0.9},
				{MappingID: "b", Share: 0.2},
				{MappingID: "c", Share: -1}, // unknown -> group incomparable
			},
		}
		eqOrder(t, Rank(in), "a", "b", "c")
	})
}

// ----- Part 6: balance group isolation -------------------------------------

func TestRankBalanceGroupIsolation(t *testing.T) {
	// g1: p1(0.2), p2(0.9) -> within g1 by headroom: p2, p1
	// g2: p3(0.3), p4(0.8) -> within g2 by headroom: p4, p3
	// Input order interleaves groups; output must keep group blocks in
	// first-appearance order (g1 then g2) and not interleave.
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "p1", BalanceGroup: "g1"},
			{MappingID: "p3", BalanceGroup: "g2"},
			{MappingID: "p2", BalanceGroup: "g1"},
			{MappingID: "p4", BalanceGroup: "g2"},
		},
		Obs: []ProviderObs{
			{MappingID: "p1", Mode: "normal", Snapshot: remSnap("p1", 0.2, rankNow)},
			{MappingID: "p2", Mode: "normal", Snapshot: remSnap("p2", 0.9, rankNow)},
			{MappingID: "p3", Mode: "normal", Snapshot: remSnap("p3", 0.3, rankNow)},
			{MappingID: "p4", Mode: "normal", Snapshot: remSnap("p4", 0.8, rankNow)},
		},
	}
	eqOrder(t, Rank(in), "p2", "p1", "p4", "p3")
}

// ----- Part 7: ineligible placement ----------------------------------------

func TestRankIneligiblePlacement(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "e1"}, {MappingID: "e2"}, {MappingID: "d3"}, {MappingID: "d1"}},
		Obs: []ProviderObs{
			{MappingID: "e1", Mode: "normal", Snapshot: remSnap("e1", 0.5, rankNow)},
			{MappingID: "e2", Mode: "normal", Snapshot: remSnap("e2", 0.9, rankNow)},
			{MappingID: "d3", Mode: "disabled", Snapshot: remSnap("d3", 0.5, rankNow)},
			{MappingID: "d1", Mode: "disabled", Snapshot: remSnap("d1", 0.5, rankNow)},
		},
	}
	r := Rank(in)
	eqOrder(t, r, "e2", "e1", "d1", "d3") // eligible by headroom, then ineligible by ID
	// Verify eligible/ineligible flags and contiguous 0-based ranks.
	for i, e := range r.Entries {
		if e.Rank != i {
			t.Fatalf("entry %d rank = %d, want %d", i, e.Rank, i)
		}
		wantEligible := i < 2
		if e.Eligible != wantEligible {
			t.Fatalf("entry %d (%s) eligible = %v, want %v", i, e.MappingID, e.Eligible, wantEligible)
		}
	}
}

// ----- Part 7b: policy without an observation -------------------------------

func TestRankMissingObservation(t *testing.T) {
	// A provider with a policy but no matching observation entry (e.g. a
	// newly-configured provider that has not been polled yet) must still
	// appear in the ranking with its correct MappingID, be ineligible, and
	// explain why ("no fresh snapshot"). Its identity must never be silently
	// lost.
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "fresh"},
			{MappingID: "unpolled"},
		},
		Obs: []ProviderObs{
			// Only "fresh" has an observation; "unpolled" does not.
			{MappingID: "fresh", Mode: "normal", Snapshot: remSnap("fresh", 0.5, rankNow)},
		},
	}
	r := Rank(in)
	// Eligible providers come first, ineligible after; both must be present.
	eqOrder(t, r, "fresh", "unpolled")

	var unpolled *RankEntry
	for i := range r.Entries {
		if r.Entries[i].MappingID == "unpolled" {
			unpolled = &r.Entries[i]
			break
		}
	}
	if unpolled == nil {
		t.Fatal("unpolled provider missing from ranking result")
	}
	if unpolled.MappingID != "unpolled" {
		t.Fatalf("MappingID = %q, want %q (identity must not be lost)", unpolled.MappingID, "unpolled")
	}
	if unpolled.Eligible {
		t.Fatal("unpolled provider should be ineligible")
	}
	if !strings.Contains(unpolled.Explanation, "no fresh snapshot") {
		t.Fatalf("explanation %q should mention \"no fresh snapshot\"", unpolled.Explanation)
	}
}

// ----- Part 8: determinism -------------------------------------------------

func TestRankDeterminism(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "a", Schedule: &offPeak, Weight: 2},
			{MappingID: "b"},
			{MappingID: "c", BalanceGroup: "g2"},
		},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: remSnap("a", 0.3, rankNow)},
			{MappingID: "b", Mode: "normal", Snapshot: remSnap("b", 0.6, rankNow)},
			{MappingID: "c", Mode: "reserve", Snapshot: remSnap("c", 0.9, rankNow)},
		},
		Usage: []UsageShare{{MappingID: "a", Share: 0.4}, {MappingID: "b", Share: 0.1}},
	}
	first := Rank(in)
	second := Rank(in)
	// Run several more times to surface any ordering nondeterminism.
	for i := 0; i < 5; i++ {
		if got := Rank(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("nondeterministic ranking: iteration %d differs", i)
		}
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two consecutive runs differ")
	}
}

// ----- Part 9: explanations ------------------------------------------------

func TestRankExplanations(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "off", Schedule: &offPeak},
			{MappingID: "pk"},
			{MappingID: "bad"},
		},
		Obs: []ProviderObs{
			{MappingID: "off", Mode: "normal", Snapshot: remSnap("off", 0.78, rankNow)},
			{MappingID: "pk", Mode: "normal", Snapshot: remSnap("pk", 0.21, rankNow)},
			{MappingID: "bad", Mode: "disabled", Snapshot: remSnap("bad", 0.5, rankNow)},
		},
	}
	r := Rank(in)
	for _, e := range r.Entries {
		if e.Explanation == "" {
			t.Fatalf("entry %s has empty explanation", e.MappingID)
		}
		if e.Eligible {
			if !strings.Contains(e.Explanation, "off-peak") && !strings.Contains(e.Explanation, "peak") {
				t.Fatalf("eligible entry %s explanation %q must reference off-peak/peak", e.MappingID, e.Explanation)
			}
		} else {
			if !strings.Contains(e.Explanation, "ineligible") {
				t.Fatalf("ineligible entry %s explanation %q must mention ineligibility", e.MappingID, e.Explanation)
			}
		}
	}
}
