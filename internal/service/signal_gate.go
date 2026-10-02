package service

// Pure signal-gate pool rule and per-provider gate summary channel (approved
// design, amended 2026-09-30: gating keys off the routing package's
// use-it-or-lose-it signal instead of projection pace).
//
// The pool rule implements the operator's escape hatch: when every provider in
// a balance-group pool that would otherwise remain enabled is proposed for a
// signal gate this pass, the signal axis is skipped for that pool — and every
// signal-held claim in the pool is released through the normal restore
// branch — so the signal axis alone can never leave a pool with zero enabled
// providers. Reserve/disabled gating and operator-held fields are never
// weakened: members the non-pace axes gate (or that the operator holds off)
// are not counted as "otherwise enabled", and members held under those axes
// stay gated by them.

import (
	"fmt"
	"sort"
	"time"

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
	GateActionPoolSkip  = "pool-skip" // signal evidence gated the provider but the pool rule suppressed the axis
	GateActionConflict  = "conflict"  // an operator edit diverges from the held claim
	GateActionUnchanged = "unchanged" // nothing to do for this provider
)

// ProviderGateSummary is one enrolled provider's sanitized gate decision for
// the outcome and verbose-trace channel: what the pass did (or deliberately
// did not do), which gating axis owns it, the observed routing signal, and the
// resolved threshold. Dry runs report the same rows without publishing.
type ProviderGateSummary struct {
	Provider  string   `json:"provider"`
	Action    string   `json:"action"`
	Axis      string   `json:"axis,omitempty"`
	Signal    *float64 `json:"signal,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	// Detail carries a sanitized reason for non-obvious rows (pool skips,
	// conflicts, degraded evidence). It never echoes document values.
	Detail string `json:"detail,omitempty"`
}

// signalGateReason renders the sanitized status REASON prefix for a
// signal-held gate: the observed signal and engaged threshold when both are
// known, the bare attribution otherwise.
func signalGateReason(g *GateReport) string {
	if g.Signal != nil && g.Threshold != nil {
		return fmt.Sprintf("signal-gated (%s <= %s)", routing.SignalFormat(*g.Signal), routing.SignalFormat(*g.Threshold))
	}
	return "signal-gated"
}

// gateNoticeReason renders the sanitized reason carried by a provider notice
// for a signal-gated provider; empty for every other axis (the notice schema
// gains a reason only where signal gating explains the committed state).
func gateNoticeReason(g ProviderGateSummary) string {
	if g.Axis != state.OwnershipAxisSignal || g.Signal == nil {
		return ""
	}
	threshold := policy.DefaultSignalGateThreshold
	if g.Threshold != nil {
		threshold = *g.Threshold
	}
	return fmt.Sprintf("signal-gated (%s <= %s)", routing.SignalFormat(*g.Signal), routing.SignalFormat(threshold))
}

// appendSignalGateEvents records the durable signal_gated / signal_recovered
// transitions a committed gate pass produced, derived by comparing the
// published ownership claims against the observed ones: a signal claim
// appearing where none was held is a gating transition; an observed signal
// claim that is gone (or no longer signal-held) in the published map is a
// recovery. Refused and dry-run passes never call this: no bytes, no events.
// Events carry the sanitized reason from the pass's gate summary when one
// exists.
func appendSignalGateEvents(next state.State, observed state.State, plan providerGatePlan, revision uint64, now time.Time) state.State {
	wasHeld := make(map[string]bool, len(observed.ProviderOwnership))
	for id, record := range observed.ProviderOwnership {
		if record.SignalHeld() {
			wasHeld[id] = true
		}
	}
	reasons := make(map[string]string, len(plan.GateSummary))
	for _, g := range plan.GateSummary {
		if g.Detail != "" {
			reasons[g.Provider] = g.Detail
		}
	}
	ids := make([]string, 0, len(plan.PublishedOwnership)+len(wasHeld))
	for id := range plan.PublishedOwnership {
		ids = append(ids, id)
	}
	for id := range wasHeld {
		if _, ok := plan.PublishedOwnership[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		nowHeld := false
		if claim, ok := plan.PublishedOwnership[id]; ok && claim.SignalHeld() {
			nowHeld = true
		}
		var action string
		switch {
		case nowHeld && !wasHeld[id]:
			action = "signal_gated"
		case !nowHeld && wasHeld[id]:
			action = "signal_recovered"
		default:
			continue
		}
		e := state.EventRecord{
			Sequence:   nextEventSequence(&next),
			Revision:   revision,
			Ordinal:    len(next.EventHistory.Events),
			At:         now.UTC(),
			RecordedAt: now.UTC(),
			Category:   state.EventRoutingChange,
			Action:     action,
			MappingID:  id,
			Result:     state.EventChanged,
			Reason:     reasons[id],
		}
		next.EventHistory, _ = state.AppendEvent(next.EventHistory, e)
	}
	return next
}

// signalBalanceGroup resolves a mapping's effective balance group. A mapping
// without a quota section (or an empty balance_group) shares the implicit
// "default" pool, matching the reconciler's and routing package's convention.
func signalBalanceGroup(m policy.Mapping) string {
	if m.Quota == nil || m.Quota.BalanceGroup == "" {
		return "default"
	}
	return m.Quota.BalanceGroup
}

// signalGateInputs builds the routing verdict inputs for every enrolled
// provider from the desired policy and observed state. A mapping without a
// quota configuration can never gate (no adapter, no observation): its input
// is disabled, while it still counts as a pool member elsewhere.
func signalGateInputs(desired policy.Desired, observed state.State) []routing.SignalGateInput {
	ids := sortedProviderIDs(desired)
	inputs := make([]routing.SignalGateInput, 0, len(ids))
	for _, id := range ids {
		m := desired.Providers[policy.MappingID(id)]
		ps, seen := observed.Providers[id]
		var snap *quota.QuotaSnapshot
		if seen {
			snap = ps.QuotaSnapshot
		}
		enabled := false
		input := routing.SignalGateInput{MappingID: id, Snapshot: snap}
		if m.Quota != nil {
			sg := m.Quota.SignalGate.Resolved()
			enabled = !sg.Disabled
			input.FreshnessTTL = m.Quota.FreshnessTTL
			input.Threshold = sg.Threshold
		}
		input.Enabled = enabled
		inputs = append(inputs, input)
	}
	return inputs
}

// signalPoolDecision is the pool rule's verdict for one pass. Release lists
// the signal-held claims the rule frees; the planner realizes releases by
// treating those providers as not-gated, which routes them through the normal
// restore branch (baseline restored in its original shape, or a no-edit
// release when the operator already restored the exact baseline).
type signalPoolDecision struct {
	// SkipSignal names providers whose signal gate is suppressed this pass.
	SkipSignal map[string]bool
	// Release names signal-held claims freed by a skipped pool (no other
	// gating axis still applies to them).
	Release map[string]bool
	// PoolOf resolves each enrolled provider's effective balance group.
	PoolOf map[string]string
	// Pools lists the skipped pool names, sorted.
	Pools []string
}

// signalPoolRule evaluates the all-hot pool escape hatch over enrolled
// providers. A pool is skipped when its proposed signal-gate set is non-empty
// and covers every member that would otherwise remain enabled: members whose
// global enabled field is currently absent-or-true AND that no non-pace axis
// gates this pass. Every signal-held claim in a skipped pool is released
// unless another axis still gates it.
//
// The quota-gate exemption set (members declaring
// `quota_gate: {enabled: false}`, per reconcile.QuotaGateExempt) must reach
// both coupled uses without distorting the rule's premises:
//
//   - Coverage: an exempt member counts as otherwise-enabled whenever the
//     operator does not hold its field off and its mode is not a disable the
//     gate will actually apply this pass — the exemption lifts only the
//     quota-derived clamped reserve (mode reserve), never the operator-held
//     check and never a mode disabled from a manual disable or corrupted
//     observation. Otherwise an exempt exhausted member would make all-hot
//     coverage look complete and wrongly suppress a signal-hot sibling's
//     gate, while an exempt corrupted member would be durably disabled in
//     the same pass that coverage counted it as enabled, emptying the pool
//     for an interval.
//   - Release: when a pool IS skipped, an exempt member's own held signal
//     claim is released even at its clamped reserve — the clamp is not
//     another axis holding it — while a manual or corrupted disable keeps
//     holding it, so a pool skip can never restore over an operator disable.
//
// Singleton pools stay byte-identical to the healthy case: a lone exempt
// signal-hot member pool-skips and releases its held claim exactly as the
// tested healthy singleton does.
func signalPoolRule(desired policy.Desired, verdicts map[string]routing.SignalGateVerdict, liveEnabled map[string]bool, modes map[string]state.Mode, signalHeld map[string]bool, exempt map[string]bool) signalPoolDecision {
	dec := signalPoolDecision{PoolOf: map[string]string{}}
	members := make(map[string][]string)
	seen := make(map[string]bool)
	var pools []string
	for _, id := range sortedProviderIDs(desired) {
		g := signalBalanceGroup(desired.Providers[policy.MappingID(id)])
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
				continue // operator holds the field off — exemption or not
			}
			// The exemption lifts only the quota-derived clamped reserve —
			// exactly the causes the gate's durable branch does not gate (see
			// planProviderGate). An exempt member at mode disabled (a manual
			// disable or a corrupted observation) is durably gated this very
			// pass and must not count as otherwise-enabled.
			if modes[id] == state.ModeDisabled || (modes[id] == state.ModeReserve && !exempt[id]) {
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
		if dec.SkipSignal == nil {
			dec.SkipSignal = map[string]bool{}
		}
		for id := range gated {
			dec.SkipSignal[id] = true
		}
		if dec.Release == nil {
			dec.Release = map[string]bool{}
		}
		for _, id := range members[g] {
			if !signalHeld[id] {
				continue
			}
			heldByAnotherAxis := false
			switch modes[id] {
			case state.ModeDisabled:
				// Manual or corrupted disable: the disable stands, even for
				// an exempt member.
				heldByAnotherAxis = true
			case state.ModeReserve:
				// Reserve gates only a non-exempt member: an exempt member's
				// reserve is the quota-gate clamp, not a held gate, so its
				// own signal claim is still released on the pool skip.
				heldByAnotherAxis = !exempt[id]
			}
			if heldByAnotherAxis {
				continue // another axis still holds the gate
			}
			dec.Release[id] = true
		}
		dec.Pools = append(dec.Pools, g)
	}
	return dec
}
