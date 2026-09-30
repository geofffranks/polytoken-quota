package routing

// Pure pace-gate verdicts for the provider-only gate (pace gating, approved
// design). A verdict is a pure function of the provider's configured gate, its
// observed quota snapshot, and the evaluation time. It never touches the
// filesystem, the network, or the state store.
//
// The fail-closed direction matches the ranking policy exactly: only FRESH
// over-threshold evidence holds a gate. A missing snapshot, a snapshot older
// than the provider's freshness TTL, a degraded quota source, or a snapshot
// with no qualifying projection window (all periods under one day) yields a
// not-gated verdict with a sanitized reason — degraded evidence never gates.
// The freshness comparison deliberately mirrors CheckEligibility's, so the
// gate and the rank can never disagree about whether a snapshot is fresh.

import (
	"fmt"
	"math"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// PaceGateInput is one provider mapping's pace-gate evaluation input. The
// service layer builds it from the desired policy and observed state; routing
// consumes plain values so this package stays free of policy imports.
type PaceGateInput struct {
	MappingID string
	// Snapshot is the provider's latest quota observation; nil when the
	// provider has never been observed.
	Snapshot *quota.QuotaSnapshot
	// FreshnessTTL bounds the snapshot's age; non-positive selects the same
	// 30m default the ranking policy applies.
	FreshnessTTL time.Duration
	// Threshold is the resolved pace fraction at which gating engages. The
	// service layer resolves the documented default; a threshold that is not
	// finite and positive is fail-closed (never gates).
	Threshold float64
	// Enabled reflects the resolved pace_gate.enabled configuration. A
	// disabled gate never gates.
	Enabled bool
}

// PaceGateVerdict is the pace-gate decision for one provider mapping: the
// observed projection pace (nil when not computable), whether fresh
// over-threshold evidence holds a gate, and a sanitized reason naming the
// decisive comparison.
type PaceGateVerdict struct {
	MappingID string
	Pace      *float64
	Gated     bool
	Reason    string
}

// PaceOf reports the projection pace for a snapshot: the pure anchor-window
// computation the routing signal package formerly shared, exported so the
// provider gate and any diagnostic derive from one source.
func PaceOf(snap *quota.QuotaSnapshot, now time.Time) (pace float64, ok bool) {
	return computePace(snap, now)
}

// computePace calculates the projection pace for a provider from its anchor
// window — the longest window with Period + ResetAt + a usable remaining that
// clears the minimum-period floor. Returns the pace and true when computable;
// 0 and false when no qualifying window exists.
//
//	usedFrac    = 1 - remainingFraction
//	elapsedFrac = ceilToDay(clamp01(1 - (ResetAt - now) / Period))
//	pace        = usedFrac / max(elapsedFrac, eps)
//
// Elapsed time is rounded up to whole days before normalization. This avoids
// transient pace spikes immediately after a reset while retaining a small
// epsilon for a window that has not reached its first day. Pace < 1.0 →
// under-utilized; pace > 1.0 → over-utilized.
//
// This is deliberately NOT the ranking package's use-it-or-lose-it gap signal:
// the signal answers "will quota reach reset unused" (positive = prefer) and
// mixes forfeiture into its terms, while the gate answers the operator's
// approved question "is this provider burning faster than the projection
// window allows" (pace >= 1.0). The two share window eligibility (Period >=
// minProjectionPeriod, a ResetAt, a usable remaining) so neither can see a
// window the other cannot.
func computePace(snap *quota.QuotaSnapshot, now time.Time) (pace float64, ok bool) {
	if snap == nil {
		return 0, false
	}
	var anchor *quota.QuotaWindow
	for i := range snap.Windows {
		w := &snap.Windows[i]
		if w.Period == nil || *w.Period < minProjectionPeriod {
			continue
		}
		if w.ResetAt == nil {
			continue
		}
		if w.Remaining() == nil {
			continue
		}
		if anchor == nil || *w.Period > *anchor.Period {
			anchor = w
		}
	}
	if anchor == nil {
		return 0, false
	}
	rem := anchor.Remaining()
	usedFrac := 1.0 - *rem
	timeToReset := anchor.ResetAt.Sub(now)
	elapsed := *anchor.Period - timeToReset
	if elapsed < 0 {
		elapsed = 0
	} else if elapsed > *anchor.Period {
		elapsed = *anchor.Period
	}
	// Quantize elapsed time upward so a newly reset quota is measured against
	// one day, not a few minutes or seconds. This makes pace stable enough for
	// periodic reconciliation while preserving the full-period endpoint.
	elapsed = ((elapsed + 24*time.Hour - 1) / (24 * time.Hour)) * (24 * time.Hour)
	elapsedFrac := float64(elapsed) / float64(*anchor.Period)
	eps := float64(5*time.Minute) / float64(*anchor.Period)
	if elapsedFrac < eps {
		elapsedFrac = eps
	}
	return usedFrac / elapsedFrac, true
}

// PacePercent renders a pace fraction as a rounded integer percentage for
// sanitized diagnostics ("1.36 -> 136").
func PacePercent(pace float64) int {
	return int(math.Round(pace * 100))
}

// paceFreshnessTTL resolves the effective freshness TTL, mirroring the
// ranking policy's own default so one bound governs both consumers.
func paceFreshnessTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultFreshTTL
	}
	return ttl
}

// PaceGateVerdicts computes pace verdicts for every input at now, keyed by
// mapping ID. It is deterministic and never mutates its inputs; the returned
// map is empty (never nil) for empty input.
func PaceGateVerdicts(inputs []PaceGateInput, now time.Time) map[string]PaceGateVerdict {
	out := make(map[string]PaceGateVerdict, len(inputs))
	for _, in := range inputs {
		out[in.MappingID] = paceGateVerdict(in, now)
	}
	return out
}

// paceGateVerdict evaluates one input. The comparison set is deliberately
// ordered fail-closed: configuration and evidence problems short-circuit to
// not-gated before any threshold comparison can engage a gate.
func paceGateVerdict(in PaceGateInput, now time.Time) PaceGateVerdict {
	v := PaceGateVerdict{MappingID: in.MappingID}
	if !in.Enabled {
		v.Reason = "pace gate disabled"
		return v
	}
	if math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0) || in.Threshold <= 0 {
		v.Reason = "invalid pace threshold"
		return v
	}
	snap := in.Snapshot
	if snap == nil {
		v.Reason = "no quota snapshot"
		return v
	}
	// Mirror CheckEligibility's freshness condition exactly.
	if now.Sub(snap.CheckedAt) > paceFreshnessTTL(in.FreshnessTTL) {
		v.Reason = "stale quota snapshot"
		return v
	}
	if snap.Status == quota.SourceFailed || snap.Status == quota.SourcePartial {
		v.Reason = "degraded quota source"
		return v
	}
	pace, ok := PaceOf(snap, now)
	if !ok {
		v.Reason = "no qualifying quota window"
		return v
	}
	p := pace
	v.Pace = &p
	if pace >= in.Threshold {
		v.Gated = true
		v.Reason = fmt.Sprintf("pace %d%% >= threshold %d%%", PacePercent(pace), PacePercent(in.Threshold))
		return v
	}
	v.Reason = fmt.Sprintf("pace %d%% under threshold %d%%", PacePercent(pace), PacePercent(in.Threshold))
	return v
}
