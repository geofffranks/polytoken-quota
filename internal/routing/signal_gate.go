package routing

// Pure signal-gate verdicts for the provider-only gate (approved design,
// amended 2026-09-30: gating keys off the routing package's use-it-or-lose-it
// signal instead of projection pace). A verdict is a pure function of the
// provider's configured gate, its observed quota snapshot, and the evaluation
// time. It never touches the filesystem, the network, or the state store.
//
// The fail-closed direction matches the ranking policy exactly: only FRESH
// evidence that the provider is projected to run out before reset holds a
// gate. A missing snapshot, a snapshot older than the provider's freshness
// TTL, a degraded quota source, or a snapshot with no qualifying window (all
// periods under one day) yields a not-gated verdict with a sanitized reason —
// degraded evidence never gates. The freshness comparison deliberately mirrors
// CheckEligibility's, so the gate and the rank can never disagree about
// whether a snapshot is fresh.

import (
	"fmt"
	"math"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// SignalGateInput is one provider mapping's signal-gate evaluation input. The
// service layer builds it from the desired policy and observed state; routing
// consumes plain values so this package stays free of policy imports.
type SignalGateInput struct {
	MappingID string
	// Snapshot is the provider's latest quota observation; nil when the
	// provider has never been observed.
	Snapshot *quota.QuotaSnapshot
	// FreshnessTTL bounds the snapshot's age; non-positive selects the same
	// 30m default the ranking policy applies.
	FreshnessTTL time.Duration
	// Threshold is the resolved signal threshold at which gating engages: the
	// gate holds when the use-it-or-lose-it signal falls to or below it. Zero
	// (the documented default) means "projected to run out before reset";
	// negative thresholds demand overdraw margin. The service layer resolves
	// the documented default; a threshold that is not finite and non-positive
	// is fail-closed (never gates).
	Threshold float64
	// Enabled reflects the resolved signal_gate.enabled configuration. A
	// disabled gate never gates.
	Enabled bool
}

// SignalGateVerdict is the signal-gate decision for one provider mapping: the
// observed use-it-or-lose-it signal (nil when not computable), whether fresh
// at-or-below-threshold evidence holds a gate, and a sanitized reason naming
// the decisive comparison.
type SignalGateVerdict struct {
	MappingID string
	Signal    *float64
	Gated     bool
	Reason    string
}

// SignalFormat renders a signal value the way the ranking explanation does
// ("%+.2f"), so gate diagnostics and rank explanations read identically.
func SignalFormat(signal float64) string {
	return fmt.Sprintf("%+.2f", signal)
}

// signalFreshnessTTL resolves the effective freshness TTL, mirroring the
// ranking policy's own default so one bound governs both consumers.
func signalFreshnessTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultFreshTTL
	}
	return ttl
}

// SignalGateVerdicts computes signal verdicts for every input at now, keyed by
// mapping ID. It is deterministic and never mutates its inputs; the returned
// map is empty (never nil) for empty input.
func SignalGateVerdicts(inputs []SignalGateInput, now time.Time) map[string]SignalGateVerdict {
	out := make(map[string]SignalGateVerdict, len(inputs))
	for _, in := range inputs {
		out[in.MappingID] = signalGateVerdict(in, now)
	}
	return out
}

// signalGateVerdict evaluates one input. The comparison set is deliberately
// ordered fail-closed: configuration and evidence problems short-circuit to
// not-gated before any threshold comparison can engage a gate.
func signalGateVerdict(in SignalGateInput, now time.Time) SignalGateVerdict {
	v := SignalGateVerdict{MappingID: in.MappingID}
	if !in.Enabled {
		v.Reason = "signal gate disabled"
		return v
	}
	if math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0) || in.Threshold > 0 {
		v.Reason = "invalid signal threshold"
		return v
	}
	snap := in.Snapshot
	if snap == nil {
		v.Reason = "no quota snapshot"
		return v
	}
	// Mirror CheckEligibility's freshness condition exactly.
	if now.Sub(snap.CheckedAt) > signalFreshnessTTL(in.FreshnessTTL) {
		v.Reason = "stale quota snapshot"
		return v
	}
	if snap.Status == quota.SourceFailed || snap.Status == quota.SourcePartial {
		v.Reason = "degraded quota source"
		return v
	}
	signal, ok := ComputeSignal(snap, now)
	if !ok {
		v.Reason = "no qualifying quota window"
		return v
	}
	s := signal
	v.Signal = &s
	if signal <= in.Threshold {
		v.Gated = true
		v.Reason = fmt.Sprintf("signal %s <= threshold %s", SignalFormat(signal), SignalFormat(in.Threshold))
		return v
	}
	v.Reason = fmt.Sprintf("signal %s above threshold %s", SignalFormat(signal), SignalFormat(in.Threshold))
	return v
}
