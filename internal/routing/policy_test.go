package routing

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// ----- shared test helpers -------------------------------------------------

func fptr(v float64) *float64               { x := v; return &x }
func tptr(t time.Time) *time.Time           { x := t; return &x }
func durptr(d time.Duration) *time.Duration { x := d; return &x }

func floatClose(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

var allDays = []DayOfWeek{Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday}

// rankNow is the stable injected "now" used across ranking tests.
var rankNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// weekWindow builds a 7-day window whose reset lands after daysElapsed of the
// cycle has passed (fractional days allowed), with usedFrac of the quota used.
func weekWindow(usedFrac, daysElapsed float64) quota.QuotaWindow {
	period := 7 * 24 * time.Hour
	elapsed := time.Duration(daysElapsed * float64(24*time.Hour))
	return quota.QuotaWindow{
		Used: fptr(usedFrac), Limit: fptr(1),
		ResetAt: tptr(rankNow.Add(period - elapsed)), Period: durptr(period),
	}
}

func TestComputeSignal(t *testing.T) {
	week := 7 * 24 * time.Hour
	month := 30 * 24 * time.Hour
	tests := []struct {
		name    string
		windows []quota.QuotaWindow
		want    float64
		ok      bool
	}{
		{
			name:    "exactly on pace at a whole-day boundary is zero",
			windows: []quota.QuotaWindow{weekWindow(3.0/7.0, 3)},
			want:    0,
			ok:      true,
		},
		{
			name: "on pace mid-day leans positive from whole-day rounding",
			// 3.5 days rounds to 4: r/t - u/e = 0.5/0.5 - 0.5/(4/7).
			windows: []quota.QuotaWindow{weekWindow(0.5, 3.5)},
			want:    0.125,
			ok:      true,
		},
		{
			name: "overdrawn is negative",
			// (2/7)/(4/7) - (5/7)/(3/7)
			windows: []quota.QuotaWindow{weekWindow(5.0/7.0, 3)},
			want:    0.5 - 5.0/3.0,
			ok:      true,
		},
		{
			name: "late-cycle surplus is strongly positive",
			// 0.8/(1/7) - 0.2/(6/7)
			windows: []quota.QuotaWindow{weekWindow(0.2, 6)},
			want:    5.6 - 0.2*7.0/6.0,
			ok:      true,
		},
		{
			name: "first minute measures burn against one whole day",
			windows: []quota.QuotaWindow{{
				Used: fptr(0.01), Limit: fptr(1),
				ResetAt: tptr(rankNow.Add(week - time.Minute)), Period: durptr(week),
			}},
			want: 0.99*float64(week)/float64(week-time.Minute) - 0.01*7,
			ok:   true,
		},
		{
			name: "sub-day window alone does not qualify",
			windows: []quota.QuotaWindow{{
				Used: fptr(0.5), Limit: fptr(1),
				ResetAt: tptr(rankNow.Add(2 * time.Hour)), Period: durptr(5 * time.Hour),
			}},
			ok: false,
		},
		{
			name:    "nil period does not qualify",
			windows: []quota.QuotaWindow{{Used: fptr(0.5), Limit: fptr(1), ResetAt: tptr(rankNow.Add(time.Hour))}},
			ok:      false,
		},
		{
			name:    "missing reset does not qualify",
			windows: []quota.QuotaWindow{{Used: fptr(0.5), Limit: fptr(1), Period: durptr(week)}},
			ok:      false,
		},
		{
			name: "sub-day window is ignored alongside a weekly window",
			windows: []quota.QuotaWindow{
				{Used: fptr(0.99), Limit: fptr(1), ResetAt: tptr(rankNow.Add(time.Hour)), Period: durptr(5 * time.Hour)},
				weekWindow(5.0/7.0, 3),
			},
			want: 0.5 - 5.0/3.0,
			ok:   true,
		},
		{
			name: "weekly and monthly windows combine as a period-weighted mean",
			windows: []quota.QuotaWindow{
				weekWindow(3.0/7.0, 3), // 0
				// 20 of 30 days elapsed, half used: 0.5/(1/3) - 0.5/(2/3) = 0.75.
				{Used: fptr(0.5), Limit: fptr(1), ResetAt: tptr(rankNow.Add(10 * 24 * time.Hour)), Period: durptr(month)},
			},
			want: 0.75 * 30 / 37,
			ok:   true,
		},
		{
			name:    "unused quota at reset clamps to +100",
			windows: []quota.QuotaWindow{{Used: fptr(0), Limit: fptr(1), ResetAt: tptr(rankNow), Period: durptr(week)}},
			want:    100,
			ok:      true,
		},
		{
			name:    "fully used at cycle start clamps to -100",
			windows: []quota.QuotaWindow{{Used: fptr(1), Limit: fptr(1), ResetAt: tptr(rankNow.Add(week)), Period: durptr(week)}},
			want:    -100,
			ok:      true,
		},
		{
			name: "reset in the past stays finite through the one-hour floor",
			// 0.5/(1h/7d) - 0.5/1
			windows: []quota.QuotaWindow{{Used: fptr(0.5), Limit: fptr(1), ResetAt: tptr(rankNow.Add(-time.Hour)), Period: durptr(week)}},
			want:    0.5*168 - 0.5,
			ok:      true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := computeSignal(&quota.QuotaSnapshot{Windows: tc.windows}, rankNow)
			if ok != tc.ok {
				t.Fatalf("computeSignal ok = %v, want %v", ok, tc.ok)
			}
			if ok && !floatClose(got, tc.want) {
				t.Fatalf("computeSignal = %.6f, want %.6f", got, tc.want)
			}
		})
	}
}

func TestComputeSignalNilSnapshot(t *testing.T) {
	if _, ok := computeSignal(nil, rankNow); ok {
		t.Fatal("nil snapshot must not produce a signal")
	}
}

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

// signalSnap builds a fresh, available snapshot around weekWindow.
func signalSnap(mid string, usedFrac, daysElapsed float64) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		MappingID:    mid,
		CheckedAt:    rankNow,
		Status:       quota.SourceFresh,
		Availability: quota.QuotaAvailable,
		Windows:      []quota.QuotaWindow{weekWindow(usedFrac, daysElapsed)},
	}
}

// paceSnap creates a snapshot with one weekly window that has a computable
// signal. usedFrac and elapsedFrac are both in [0,1]. At elapsedFrac 0.5 the
// 3.5 elapsed days round up to 4, so the signal is 2 - 3.75*usedFrac.
func paceSnap(mid string, usedFrac, elapsedFrac float64) *quota.QuotaSnapshot {
	period := 7 * 24 * time.Hour
	timeToReset := time.Duration((1 - elapsedFrac) * float64(period))
	return &quota.QuotaSnapshot{
		MappingID:    mid,
		CheckedAt:    rankNow,
		Status:       quota.SourceFresh,
		Availability: quota.QuotaAvailable,
		Windows: []quota.QuotaWindow{{
			Name:    "primary",
			Used:    fptr(usedFrac),
			Limit:   fptr(1.0),
			ResetAt: tptr(rankNow.Add(timeToReset)),
			Period:  durptr(period),
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

// TestCheckEligibilityPartialSnapshot proves incomplete quota observations do
// not make a provider eligible from a potentially understated lower bound.
func TestCheckEligibilityPartialSnapshot(t *testing.T) {
	p := ProviderPolicy{MappingID: "p"}
	snap := remSnap("p", 0.5, rankNow)
	snap.Status = quota.SourcePartial
	e := CheckEligibility(p, ProviderObs{MappingID: "p", Mode: "normal", Snapshot: snap}, rankNow)
	if e.Rankable {
		t.Fatal("partial snapshot should not be rankable")
	}
	if !strings.Contains(e.Reason, "partial") {
		t.Fatalf("reason %q should mention partial data", e.Reason)
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

func TestRankExactTieSharesRank(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "zebra"}, {MappingID: "alpha"}},
		Obs: []ProviderObs{
			{MappingID: "zebra", Mode: "normal", Snapshot: remSnap("zebra", 0.5, rankNow)},
			{MappingID: "alpha", Mode: "normal", Snapshot: remSnap("alpha", 0.5, rankNow)},
		},
	}
	got := Rank(in)
	// Mapping ID may stabilize diagnostic presentation, but it must not create a
	// semantic preference between otherwise equal providers.
	eqOrder(t, got, "alpha", "zebra")
	if got.Entries[0].Rank != got.Entries[1].Rank {
		t.Fatalf("exact tie ranks = %d, %d; want shared rank", got.Entries[0].Rank, got.Entries[1].Rank)
	}
}

// ----- Part 5: signal ranking ----------------------------------------------

func TestRankOrdersBySignalDescending(t *testing.T) {
	// Signals +1.17 / 0 / -1.17. Weights oppose the signal order so the test
	// fails if weight were consulted before the signal.
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "over", Weight: 5},
			{MappingID: "on", Weight: 3},
			{MappingID: "under", Weight: 1},
		},
		Obs: []ProviderObs{
			{MappingID: "over", Mode: "normal", Snapshot: signalSnap("over", 5.0/7.0, 3)},
			{MappingID: "on", Mode: "normal", Snapshot: signalSnap("on", 3.0/7.0, 3)},
			{MappingID: "under", Mode: "normal", Snapshot: signalSnap("under", 1.0/7.0, 3)},
		},
	}
	got := Rank(in)
	eqOrder(t, got, "under", "on", "over")
	if got.Entries[0].Rank == got.Entries[1].Rank || got.Entries[1].Rank == got.Entries[2].Rank {
		t.Fatalf("ranks = %d, %d, %d; want distinct", got.Entries[0].Rank, got.Entries[1].Rank, got.Entries[2].Rank)
	}
}

func TestRankSignalTieBandSharesRank(t *testing.T) {
	// Signals +0.125 and -0.025 are 0.15 apart, inside the 0.20 tie band.
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "zeta"}, {MappingID: "alpha"}},
		Obs: []ProviderObs{
			{MappingID: "zeta", Mode: "normal", Snapshot: paceSnap("zeta", 0.50, 0.5)},
			{MappingID: "alpha", Mode: "normal", Snapshot: paceSnap("alpha", 0.54, 0.5)},
		},
	}
	got := Rank(in)
	if got.Entries[0].Rank != got.Entries[1].Rank {
		t.Fatalf("ranks = %d, %d; want shared rank inside the tie band", got.Entries[0].Rank, got.Entries[1].Rank)
	}
}

func TestRankSignalTieBandOffPeakDecides(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	// Signals +0.125 (peak) and -0.025 (off-peak) share a cluster, so off-peak
	// decides.
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "a"}, {MappingID: "b", Schedule: &offPeak}},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 0.50, 0.5)},
			{MappingID: "b", Mode: "normal", Snapshot: paceSnap("b", 0.54, 0.5)},
		},
	}
	eqOrder(t, Rank(in), "b", "a")
}

func TestRankNoUnderPaceTier(t *testing.T) {
	// Signals +0.9 and +0.3 were both "under pace" (paces 0.51 and 0.79) and
	// used to share a tier where weight decided. They now rank separately.
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "low", Weight: 5}, {MappingID: "high", Weight: 1}},
		Obs: []ProviderObs{
			{MappingID: "low", Mode: "normal", Snapshot: paceSnap("low", 1.7/3.75, 0.5)},
			{MappingID: "high", Mode: "normal", Snapshot: paceSnap("high", 1.1/3.75, 0.5)},
		},
	}
	got := Rank(in)
	eqOrder(t, got, "high", "low")
	if got.Entries[0].Rank == got.Entries[1].Rank {
		t.Fatal("signals 0.9 and 0.3 must not share a rank")
	}
}

func TestRankSignalClusterChain(t *testing.T) {
	// Signals: a +0.125, b -0.025, c -0.25. a-b is inside the band, b-c is not.
	// Within cluster 0, weight decides (b=3 before a=1).
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "a", Weight: 1},
			{MappingID: "b", Weight: 3},
			{MappingID: "c", Weight: 2},
		},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 0.50, 0.5)},
			{MappingID: "b", Mode: "normal", Snapshot: paceSnap("b", 0.54, 0.5)},
			{MappingID: "c", Mode: "normal", Snapshot: paceSnap("c", 0.60, 0.5)},
		},
	}
	eqOrder(t, Rank(in), "b", "a", "c")
}

func TestRankSignalClustersChainTransitively(t *testing.T) {
	// Signals +0.3 / +0.15 / 0.0: each adjacent pair is inside the band, so
	// all three share one cluster even though the span exceeds it.
	in := RankingInput{
		Now: rankNow,
		Policies: []ProviderPolicy{
			{MappingID: "a", Weight: 1},
			{MappingID: "b", Weight: 3},
			{MappingID: "c", Weight: 2},
		},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 1.7/3.75, 0.5)},
			{MappingID: "b", Mode: "normal", Snapshot: paceSnap("b", 1.85/3.75, 0.5)},
			{MappingID: "c", Mode: "normal", Snapshot: paceSnap("c", 2.0/3.75, 0.5)},
		},
	}
	eqOrder(t, Rank(in), "b", "c", "a")
}

func TestRankSignalBeatsOffPeak(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	// a: peak, signal +0.875. b: off-peak, signal -0.625. The signal gap
	// exceeds the band, so it outranks off-peak.
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "a"}, {MappingID: "b", Schedule: &offPeak}},
		Obs: []ProviderObs{
			{MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 0.3, 0.5)},
			{MappingID: "b", Mode: "normal", Snapshot: paceSnap("b", 0.7, 0.5)},
		},
	}
	eqOrder(t, Rank(in), "a", "b")
}

func TestRankLateCycleSurplusBeatsEarlyCycle(t *testing.T) {
	// Both have used quota at half the elapsed rate. The one about to reset
	// with 4/7 left (signal +3.5) must drain before the early-cycle one (+0.7).
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "early", Weight: 5}, {MappingID: "late"}},
		Obs: []ProviderObs{
			{MappingID: "early", Mode: "normal", Snapshot: signalSnap("early", 1.0/7.0, 2)},
			{MappingID: "late", Mode: "normal", Snapshot: signalSnap("late", 3.0/7.0, 6)},
		},
	}
	eqOrder(t, Rank(in), "late", "early")
}

func TestRankSignalGroupSkipWhenAnyProviderLacksSignal(t *testing.T) {
	policies := map[string]ProviderPolicy{
		"a": {MappingID: "a"},
		"m": {MappingID: "m"},
		"z": {MappingID: "z"},
	}
	observations := map[string]ProviderObs{
		"a": {MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 0.75, 0.5)},
		"m": {MappingID: "m", Mode: "normal", Snapshot: remSnap("m", 0.5, rankNow)},
		"z": {MappingID: "z", Mode: "normal", Snapshot: paceSnap("z", 0.10, 0.5)},
	}
	for _, ids := range [][]string{
		{"a", "m", "z"}, {"a", "z", "m"}, {"m", "a", "z"},
		{"m", "z", "a"}, {"z", "a", "m"}, {"z", "m", "a"},
	} {
		in := RankingInput{Now: rankNow}
		for _, id := range ids {
			in.Policies = append(in.Policies, policies[id])
			in.Obs = append(in.Obs, observations[id])
		}
		got := Rank(in)
		eqOrder(t, got, "a", "m", "z")
		if got.Entries[0].Rank != got.Entries[1].Rank || got.Entries[1].Rank != got.Entries[2].Rank {
			t.Fatalf("input %v ranks = %d, %d, %d; want shared rank after group signal skip", ids, got.Entries[0].Rank, got.Entries[1].Rank, got.Entries[2].Rank)
		}
	}
}

func TestRankSignalReorderedDeterminism(t *testing.T) {
	// Signals +0.875 / +0.125 / -0.625 each sit in their own cluster.
	policies := []ProviderPolicy{{MappingID: "a"}, {MappingID: "b"}, {MappingID: "c"}}
	obs := []ProviderObs{
		{MappingID: "a", Mode: "normal", Snapshot: paceSnap("a", 0.3, 0.5)},
		{MappingID: "b", Mode: "normal", Snapshot: paceSnap("b", 0.5, 0.5)},
		{MappingID: "c", Mode: "normal", Snapshot: paceSnap("c", 0.7, 0.5)},
	}
	first := Rank(RankingInput{Now: rankNow, Policies: policies, Obs: obs})
	second := Rank(RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{policies[2], policies[0], policies[1]},
		Obs:      []ProviderObs{obs[2], obs[0], obs[1]},
	})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reordered input changed ranking:\n  first:  %+v\n  second: %+v", first, second)
	}
	eqOrder(t, first, "a", "b", "c")
}

func TestRankExplainSignalFormat(t *testing.T) {
	offPeak := alwaysOffPeak(t)
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "pk"}, {MappingID: "off", Schedule: &offPeak}},
		Obs: []ProviderObs{
			{MappingID: "pk", Mode: "normal", Snapshot: signalSnap("pk", 1.0/7.0, 3)},
			{MappingID: "off", Mode: "normal", Snapshot: signalSnap("off", 5.0/7.0, 3)},
		},
	}
	want := map[string]string{"pk": "peak, signal +1.17", "off": "off-peak, signal -1.17"}
	for _, e := range Rank(in).Entries {
		if e.Explanation != want[e.MappingID] {
			t.Fatalf("%s explanation = %q, want %q", e.MappingID, e.Explanation, want[e.MappingID])
		}
	}
}

func TestRankExplainOmitsGroupSkippedSignal(t *testing.T) {
	in := RankingInput{
		Now:      rankNow,
		Policies: []ProviderPolicy{{MappingID: "proj"}, {MappingID: "nosignal"}},
		Obs: []ProviderObs{
			{MappingID: "proj", Mode: "normal", Snapshot: paceSnap("proj", 0.5, 0.5)},
			{MappingID: "nosignal", Mode: "normal", Snapshot: remSnap("nosignal", 0.5, rankNow)},
		},
	}
	for _, e := range Rank(in).Entries {
		if e.Eligible && strings.Contains(e.Explanation, "signal") {
			t.Fatalf("group-skipped signal leaked into %s explanation %q", e.MappingID, e.Explanation)
		}
	}
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
	eqOrder(t, Rank(in), "p1", "p2", "p3", "p4")
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
	eqOrder(t, r, "e1", "e2", "d1", "d3") // eligible by ID, then ineligible by ID
	// Exact eligible ties share rank 0; ineligible entries remain strictly after
	// every eligible priority class and retain deterministic lexical ordering.
	wantRanks := []int{0, 0, 1, 2}
	for i, e := range r.Entries {
		if e.Rank != wantRanks[i] {
			t.Fatalf("entry %d rank = %d, want %d", i, e.Rank, wantRanks[i])
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
