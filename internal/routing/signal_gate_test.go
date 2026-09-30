package routing

// Signal-gate verdict tests (approved amendment: gating keys off the
// use-it-or-lose-it routing signal): the fail-closed evidence rules (stale,
// missing, degraded, sub-day windows never gate), the threshold comparison
// (gate at or below threshold; zero is the projected-exhaustion line), the
// freshness default mirroring the ranking policy, and the keyed verdict map.
// All fixtures are synthetic.

import (
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// signalSnapshot builds a snapshot with one qualifying two-day window at
// exactly half elapsed relative to rankNow (every case evaluates at rankNow).
// With t = e = 0.5 the use-it-or-lose-it signal is exactly
// 2*remaining - 2*used: used 0.68 -> -0.72, used 0.50 -> 0.00,
// used 0.20 -> +1.20, used 0.10 -> +1.60.
func signalSnapshot(usedFrac float64, checkedAt time.Time, status quota.SourceStatus) *quota.QuotaSnapshot {
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

// TestSignalGateVerdictsFailClosed proves only fresh, usable, at-or-below
// threshold evidence holds a signal gate; every degraded-evidence case yields
// not-gated with a sanitized reason and never a fabricated signal.
func TestSignalGateVerdictsFailClosed(t *testing.T) {
	fresh := rankNow
	cases := []struct {
		name      string
		enabled   bool
		threshold float64
		snap      *quota.QuotaSnapshot
		ttl       time.Duration
		now       time.Time
		wantGated bool
		wantSig   bool // whether a computed signal must be present
		wantSub   string
	}{
		{
			name:      "fresh projected exhaustion gates at the default threshold",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantSig:   true,
			wantSub:   "signal -0.72 <= threshold +0.00",
		},
		{
			name:      "exactly at the exhaustion line gates",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.50, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantSig:   true,
			wantSub:   "signal +0.00 <= threshold +0.00",
		},
		{
			name:      "fresh surplus does not gate",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.20, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantSig:   true,
			wantSub:   "signal +1.20 above threshold +0.00",
		},
		{
			name:      "negative threshold demands overdraw margin",
			enabled:   true,
			threshold: -0.5,
			snap:      signalSnapshot(0.60, fresh, quota.SourceFresh), // signal -0.40
			now:       rankNow,
			wantGated: false,
			wantSig:   true,
			wantSub:   "signal -0.40 above threshold -0.50",
		},
		{
			name:      "negative threshold gates clear overdraw",
			enabled:   true,
			threshold: -0.5,
			snap:      signalSnapshot(0.68, fresh, quota.SourceFresh), // signal -0.72
			now:       rankNow,
			wantGated: true,
			wantSig:   true,
			wantSub:   "signal -0.72 <= threshold -0.50",
		},
		{
			name:      "disabled gate never gates on hot data",
			enabled:   false,
			threshold: 0,
			snap:      signalSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "signal gate disabled",
		},
		{
			name:      "missing snapshot never gates",
			enabled:   true,
			threshold: 0,
			snap:      nil,
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "no quota snapshot",
		},
		{
			name:      "stale snapshot never gates",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.68, rankNow.Add(-31*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "stale quota snapshot",
		},
		{
			name:      "freshness default is thirty minutes when ttl unset",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.68, rankNow.Add(-29*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: true,
			wantSig:   true,
			wantSub:   "signal -0.72",
		},
		{
			name:      "configured ttl tighter than default",
			enabled:   true,
			threshold: 0,
			ttl:       5 * time.Minute,
			snap:      signalSnapshot(0.68, rankNow.Add(-6*time.Minute), quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "stale quota snapshot",
		},
		{
			name:      "failed source never gates",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.68, fresh, quota.SourceFailed),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "degraded quota source",
		},
		{
			name:      "partial source never gates",
			enabled:   true,
			threshold: 0,
			snap:      signalSnapshot(0.68, fresh, quota.SourcePartial),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "degraded quota source",
		},
		{
			name:      "sub-day windows never gate (no qualifying window)",
			enabled:   true,
			threshold: 0,
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
			wantSig:   false,
			wantSub:   "no qualifying quota window",
		},
		{
			name:      "positive threshold is invalid and never gates",
			enabled:   true,
			threshold: 0.5,
			snap:      signalSnapshot(0.68, fresh, quota.SourceFresh),
			now:       rankNow,
			wantGated: false,
			wantSig:   false,
			wantSub:   "invalid signal threshold",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdicts := SignalGateVerdicts([]SignalGateInput{{
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
			if tc.wantSig != (v.Signal != nil) {
				t.Fatalf("signal present = %v (reason %q), want %v", v.Signal != nil, v.Reason, tc.wantSig)
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

// TestSignalGateVerdictsKeyedByMappingID proves the verdict map keys by
// mapping ID and stays empty-but-present for empty input.
func TestSignalGateVerdictsKeyedByMappingID(t *testing.T) {
	fresh := rankNow
	verdicts := SignalGateVerdicts([]SignalGateInput{
		{MappingID: "gp", Snapshot: signalSnapshot(0.68, fresh, quota.SourceFresh), Threshold: 0, Enabled: true},
		{MappingID: "pp", Snapshot: signalSnapshot(0.10, fresh, quota.SourceFresh), Threshold: 0, Enabled: true},
	}, rankNow)
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %d entries, want 2", len(verdicts))
	}
	if !verdicts["gp"].Gated || verdicts["pp"].Gated {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if empty := SignalGateVerdicts(nil, rankNow); empty == nil || len(empty) != 0 {
		t.Fatalf("empty input must yield an empty non-nil map, got %+v", empty)
	}
}

// TestSignalFormat proves the sanitized signal rendering matches the ranking
// explanation's format.
func TestSignalFormat(t *testing.T) {
	for _, tc := range []struct {
		signal float64
		want   string
	}{{0, "+0.00"}, {-0.72, "-0.72"}, {1.6, "+1.60"}, {-100, "-100.00"}} {
		if got := SignalFormat(tc.signal); got != tc.want {
			t.Fatalf("SignalFormat(%v) = %q, want %q", tc.signal, got, tc.want)
		}
	}
}
