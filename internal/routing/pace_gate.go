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
// computation the routing rank uses, exported so the provider gate derives
// from the same source and the two can never diverge.
func PaceOf(snap *quota.QuotaSnapshot, now time.Time) (pace float64, ok bool) {
	return computePace(snap, now)
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
