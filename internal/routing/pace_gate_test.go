package routing

// Pace-gate verdict tests (pace gating AC.6): the fail-closed evidence rules
// (stale, missing, degraded, sub-day windows never gate), the threshold
// comparison, the freshness default mirroring the ranking policy, and the
// keyed verdict map. All fixtures are synthetic.

import (
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// paceSnapshot builds a snapshot with one qualifying two-day window at exactly
// half elapsed relative to rankNow (every case evaluates at rankNow), so the
// projection pace is exactly twice the used fraction, checked at checkedAt.
func paceSnapshot(usedFrac float64, checkedAt time.Time, status quota.SourceStatus) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		CheckedAt: checkedAt,
		Status:    status,
		Windows: []quota.QuotaWindow{{
			Used:    fptr(usedFrac * 100),
			Limit:   fptr(100),
			ResetAt: tptr(rankNow.Add(24 * time.Hour)),
			Period:  durptr(48 * time.Hour),
		}},
	}
}

// TestPaceGateVerdictsFailClosed proves only fresh, usable, over-threshold
// evidence holds a pace gate; every degraded-evidence case yields not-gated
// with a sanitized reason and never a fabricated pace.
func TestPaceGateVerdictsFailClosed(t *testing.T) {
	fresh := rankNow
	cases := []struct {
		name      string
		enabled   bool
		threshold float64
		snap      *quota.QuotaSnapshot
		ttl       time.Duration
		now       time.Time
		wantGated bool
		wantPace  bool // whether a computed pace must be present
		wantSub   string
	}{
		{
			name:      "fresh over-threshold gates",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantPace:  true,
			wantSub:   "pace 136% >= threshold 100%",
		},
		{
			name:      "fresh at exactly the threshold gates",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.5, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantPace:  true,
			wantSub:   "pace 100% >= threshold 100%",
		},
		{
			name:      "fresh under threshold does not gate",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.2, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantPace:  true,
			wantSub:   "pace 40% under threshold 100%",
		},
		{
			name:      "disabled gate never gates on hot data",
			enabled:   false,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "pace gate disabled",
		},
		{
			name:      "missing snapshot never gates",
			enabled:   true,
			threshold: 1.0,
			snap:      nil,
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "no quota snapshot",
		},
		{
			name:      "stale snapshot never gates",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, rankNow.Add(-31*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "stale quota snapshot",
		},
		{
			name:      "freshness default is thirty minutes when ttl unset",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, rankNow.Add(-29*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantPace:  true,
			wantSub:   "pace",
		},
		{
			name:      "configured ttl tighter than default",
			enabled:   true,
			threshold: 1.0,
			ttl:       5 * time.Minute,
			snap:      paceSnapshot(0.68, rankNow.Add(-6*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "stale quota snapshot",
		},
		{
			name:      "failed source never gates",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, fresh, quota.SourceFailed),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "degraded quota source",
		},
		{
			name:      "partial source never gates",
			enabled:   true,
			threshold: 1.0,
			snap:      paceSnapshot(0.68, fresh, quota.SourcePartial),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "degraded quota source",
		},
		{
			name:      "sub-day windows never gate (no qualifying window)",
			enabled:   true,
			threshold: 1.0,
			snap: &quota.QuotaSnapshot{
				CheckedAt: fresh,
				Status:    quota.SourceFresh,
				Windows: []quota.QuotaWindow{{
					Used: fptr(95), Limit: fptr(100),
					ResetAt: tptr(rankNow.Add(2 * time.Hour)), Period: durptr(5 * time.Hour),
				}},
			},
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "no qualifying quota window",
		},
		{
			name:      "invalid threshold never gates",
			enabled:   true,
			threshold: 0,
			snap:      paceSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantPace:  false,
			wantSub:   "invalid pace threshold",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdicts := PaceGateVerdicts([]PaceGateInput{{
				MappingID:    "gp",
				Snapshot:     tc.snap,
				FreshnessTTL: tc.ttl,
				Threshold:    tc.threshold,
				Enabled:      tc.enabled,
			}}, tc.now)
			v, ok := verdicts["gp"]
			if !ok {
				t.Fatalf("verdict missing for gp")
			}
			if v.Gated != tc.wantGated {
				t.Fatalf("gated = %v (reason %q), want %v", v.Gated, v.Reason, tc.wantGated)
			}
			if tc.wantPace != (v.Pace != nil) {
				t.Fatalf("pace present = %v (reason %q), want %v", v.Pace != nil, v.Reason, tc.wantPace)
			}
			if !strings.Contains(v.Reason, tc.wantSub) {
				t.Fatalf("reason %q does not name %q", v.Reason, tc.wantSub)
			}
			if v.MappingID != "gp" {
				t.Fatalf("mapping id = %q", v.MappingID)
			}
		})
	}
}

// TestPaceGateVerdictsKeyedByMappingID proves the verdict map keys by mapping
// ID and stays empty-but-present for empty input.
func TestPaceGateVerdictsKeyedByMappingID(t *testing.T) {
	fresh := rankNow.Add(-time.Minute)
	verdicts := PaceGateVerdicts([]PaceGateInput{
		{MappingID: "gp", Snapshot: paceSnapshot(0.68, fresh, quota.SourceFresh), Threshold: 1.0, Enabled: true},
		{MappingID: "pp", Snapshot: paceSnapshot(0.1, fresh, quota.SourceFresh), Threshold: 1.0, Enabled: true},
	}, rankNow)
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %d entries, want 2", len(verdicts))
	}
	if !verdicts["gp"].Gated || verdicts["pp"].Gated {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if empty := PaceGateVerdicts(nil, rankNow); empty == nil || len(empty) != 0 {
		t.Fatalf("empty input must yield an empty non-nil map, got %+v", empty)
	}
}

// TestPacePercent proves the sanitized percentage rendering matches the
// ranking explanation's rounding.
func TestPacePercent(t *testing.T) {
	for _, tc := range []struct {
		pace float64
		want int
	}{{1.0, 100}, {1.36, 136}, {0.999, 100}, {0.5, 50}} {
		if got := PacePercent(tc.pace); got != tc.want {
			t.Fatalf("PacePercent(%v) = %d, want %d", tc.pace, got, tc.want)
		}
	}
}
