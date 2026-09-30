package service

// Pure pace-gate pool rule and per-provider gate summary channel (pace gating,
// approved design decisions 2 and 6).
//
// The pool rule implements the operator's escape hatch: when every provider in
// a balance-group pool that would otherwise remain enabled is proposed for a
// pace gate this pass, the pace axis is skipped for that pool — and every
// pace-held claim in the pool is released through the normal restore branch —
// so the pace axis alone can never leave a pool with zero enabled providers.
// Reserve/disabled gating and operator-held fields are never weakened: members
// the non-pace axes gate (or that the operator holds off) are not counted as
// "otherwise enabled", and members held under those axes stay gated by them.

import (
	"fmt"
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Sanitized gate-summary actions for the outcome/trace channel.
const (
	GateActionDisabled  = "disabled"  // the pass wrote enabled=false for the provider
	GateActionHeld      = "held"      // a held claim stays intact, already off
	GateActionRestored  = "restored"  // the operator baseline was restored (claim released with an edit)
	GateActionReleased  = "released"  // the claim was released without an edit (live field already matches)
	GateActionPoolSkip  = "pool-skip" // pace evidence gated the provider but the pool rule suppressed the axis
	GateActionConflict  = "conflict"  // an operator edit diverges from the held claim
	GateActionUnchanged = "unchanged" // nothing to do for this provider
)

// ProviderGateSummary is one enrolled provider's sanitized gate decision for
// the outcome and verbose-trace channel: what the pass did (or deliberately
// did not do), which gating axis owns it, the observed projection pace, and
// the resolved threshold. Dry runs report the same rows without publishing.
type ProviderGateSummary struct {
	Provider  string   `json:"provider"`
	Action    string   `json:"action"`
	Axis      string   `json:"axis,omitempty"`
	Pace      *float64 `json:"pace,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	// Detail carries a sanitized reason for non-obvious rows (pool skips,
	// conflicts, degraded pace evidence). It never echoes document values.
	Detail string `json:"detail,omitempty"`
}

// paceGateReason renders the sanitized status REASON prefix for a pace-held
// gate: the observed pace and engaged threshold when both are known, the bare
// attribution otherwise.
func paceGateReason(g *GateReport) string {
	if g.Pace != nil && g.Threshold != nil {
		return fmt.Sprintf("pace-gated (%d%% >= %d%%)", routing.PacePercent(*g.Pace), routing.PacePercent(*g.Threshold))
	}
	return "pace-gated"
}

// gateNoticeReason renders the sanitized reason carried by a provider notice
// for a pace-gated provider; empty for every other axis (the notice schema
// gains a reason only where pace gating explains the committed state).
func gateNoticeReason(g ProviderGateSummary) string {
	if g.Axis != state.OwnershipAxisPace || g.Pace == nil {
		return ""
	}
	threshold := policy.DefaultPaceGateThreshold
	if g.Threshold != nil {
		threshold = *g.Threshold
	}
	return fmt.Sprintf("pace-gated (%d%% >= %d%%)", routing.PacePercent(*g.Pace), routing.PacePercent(threshold))
}

// paceBalanceGroup resolves a mapping's effective balance group. A mapping
// without a quota section (or an empty balance_group) shares the implicit
// "default" pool, matching the reconciler's and routing package's convention.
func paceBalanceGroup(m policy.Mapping) string {
	if m.Quota == nil || m.Quota.BalanceGroup == "" {
		return "default"
	}
	return m.Quota.BalanceGroup
}

// paceGateInputs builds the routing verdict inputs for every enrolled provider
// from the desired policy and observed state. A mapping without a quota
// configuration can never gate (no adapter, no observation): its input is
// disabled, while it still counts as a pool member elsewhere.
func paceGateInputs(desired policy.Desired, observed state.State) []routing.PaceGateInput {
	ids := sortedProviderIDs(desired)
	inputs := make([]routing.PaceGateInput, 0, len(ids))
	for _, id := range ids {
		m := desired.Providers[policy.MappingID(id)]
		ps, seen := observed.Providers[id]
		var snap *quota.QuotaSnapshot
		if seen {
			snap = ps.QuotaSnapshot
		}
		enabled := false
		input := routing.PaceGateInput{MappingID: id, Snapshot: snap}
		if m.Quota != nil {
			pg := m.Quota.PaceGate.Resolved()
			enabled = pg.Enabled
			input.FreshnessTTL = m.Quota.FreshnessTTL
			input.Threshold = pg.Threshold
		}
		input.Enabled = enabled
		inputs = append(inputs, input)
	}
	return inputs
}

// pacePoolDecision is the pool rule's verdict for one pass. Release lists the
// pace-held claims the rule frees; the planner realizes releases by treating
// those providers as not-gated, which routes them through the normal restore
// branch (baseline restored in its original shape, or a no-edit release when
// the operator already restored the exact baseline).
type pacePoolDecision struct {
	// SkipPace names providers whose pace gate is suppressed this pass.
	SkipPace map[string]bool
	// Release names pace-held claims freed by a skipped pool (no other gating
	// axis still applies to them).
	Release map[string]bool
	// PoolOf resolves each enrolled provider's effective balance group.
	PoolOf map[string]string
	// Pools lists the skipped pool names, sorted.
	Pools []string
}

// pacePoolRule evaluates the all-hot pool escape hatch over enrolled
// providers. A pool is skipped when its proposed pace-gate set is non-empty
// and covers every member that would otherwise remain enabled: members whose
// global enabled field is currently absent-or-true AND that no non-pace axis
// gates this pass. Every pace-held claim in a skipped pool is released unless
// another axis still gates it.
func pacePoolRule(desired policy.Desired, verdicts map[string]routing.PaceGateVerdict, liveEnabled map[string]bool, modes map[string]state.Mode, paceHeld map[string]bool) pacePoolDecision {
	dec := pacePoolDecision{PoolOf: map[string]string{}}
	members := make(map[string][]string)
	seen := make(map[string]bool)
	var pools []string
	for _, id := range sortedProviderIDs(desired) {
		g := paceBalanceGroup(desired.Providers[policy.MappingID(id)])
		dec.PoolOf[id] = g
		members[g] = append(members[g], id)
		if !seen[g] {
			seen[g] = true
			pools = append(pools, g)
		}
	}
	sort.Strings(pools)
	for _, g := range pools {
		gated := make(map[string]bool)
		for _, id := range members[g] {
			if v, ok := verdicts[id]; ok && v.Gated {
				gated[id] = true
			}
		}
		if len(gated) == 0 {
			continue
		}
		otherwiseEnabled := make(map[string]bool)
		for _, id := range members[g] {
			if !liveEnabled[id] {
				continue // operator holds the field off
			}
			switch modes[id] {
			case state.ModeReserve, state.ModeDisabled:
				continue // a non-pace axis gates it this pass
			}
			otherwiseEnabled[id] = true
		}
		covers := true
		for id := range otherwiseEnabled {
			if !gated[id] {
				covers = false
				break
			}
		}
		if !covers {
			continue
		}
		if dec.SkipPace == nil {
			dec.SkipPace = map[string]bool{}
		}
		for id := range gated {
			dec.SkipPace[id] = true
		}
		if dec.Release == nil {
			dec.Release = map[string]bool{}
		}
		for _, id := range members[g] {
			if !paceHeld[id] {
				continue
			}
			switch modes[id] {
			case state.ModeReserve, state.ModeDisabled:
				continue // another axis still holds the gate
			}
			dec.Release[id] = true
		}
		dec.Pools = append(dec.Pools, g)
	}
	return dec
}
