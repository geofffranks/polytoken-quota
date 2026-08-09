package service

// Reconcile trace types for --verbose output. These capture the decision data
// the coordinator already computes (provider modes, ranking, chain survivors,
// produced edits) so the CLI can render a human-readable explanation of why
// each target's config did or did not change. All fields are sanitized — no
// credentials, auth values, or raw account data.

import (
	"fmt"
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ProviderModeReport is one provider mapping's effective mode and reason at
// reconcile time.
type ProviderModeReport struct {
	MappingID string `json:"mapping_id"`
	Mode      string `json:"mode"`   // normal, reserve, disabled
	Reason    string `json:"reason"` // sanitized explanation
}

// ChainSurvivorReport shows the desired vs effective model ordering for one
// managed chain, including which entries were dropped (disabled) and why.
type ChainSurvivorReport struct {
	Name     string   `json:"name"`    // full, mini, nano, classifier, or definition path
	Desired  []string `json:"desired"`  // the desired chain as written
	Survived []string `json:"survived"` // entries that survived filtering
	Dropped  []string `json:"dropped,omitempty"` // entries dropped (disabled)
}

// EditReport is one managed-field change the reconciler produced.
type EditReport struct {
	File   string   `json:"file"`
	Path   []string `json:"path"`
	Action string   `json:"action"` // set-scalar, set-sequence, set-bool, remove
	Detail string   `json:"detail"` // the value (sanitized scalar/bool) or "removed"
}

// ReconcileTrace captures the decision data for one target's reconcile pass.
// It is populated only when the caller requests verbose output.
type ReconcileTrace struct {
	ProviderModes []ProviderModeReport  `json:"provider_modes"`
	Ranking       []RankEntryReport     `json:"ranking,omitempty"`
	Chains        []ChainSurvivorReport `json:"chains,omitempty"`
	Edits         []EditReport          `json:"edits,omitempty"`
}

// buildProviderModes computes the effective mode for every provider mapping
// in the desired policy, with a sanitized reason string.
func buildProviderModes(desired policy.Desired, observed state.State) []ProviderModeReport {
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	out := make([]ProviderModeReport, 0, len(ids))
	for _, idStr := range ids {
		id := policy.MappingID(idStr)
		m := desired.Providers[id]
		mode := reconcile.MappingMode(desired, observed, id)
		reason := providerModeReason(m, mode, observed)
		out = append(out, ProviderModeReport{
			MappingID: idStr,
			Mode:      string(mode),
			Reason:    reason,
		})
	}
	return out
}

// providerModeReason produces a short, sanitized explanation for a mapping's
// effective mode.
func providerModeReason(m policy.Mapping, mode state.Mode, observed state.State) string {
	switch mode {
	case state.ModeDisabled:
		for _, cb := range m.CodexBarProviders {
			ps, ok := observed.Providers[cb]
			if !ok {
				continue
			}
			if ps.ManualDisabled {
				return "manually disabled"
			}
			if ps.Availability == state.Unavailable {
				return "provider unavailable"
			}
			if ps.Quota == state.QuotaExhausted {
				return "quota exhausted"
			}
		}
		return "disabled"
	case state.ModeReserve:
		return "quota low (reserve)"
	default:
		return "healthy"
	}
}

// buildChainSurvivors reports the desired vs effective ordering for every
// managed chain on a target.
func buildChainSurvivors(desired policy.Desired, observed state.State, target policy.Target, ranks reconcile.RankLookup) []ChainSurvivorReport {
	var out []ChainSurvivorReport
	chains := []struct {
		name  string
		chain policy.Chain
	}{
		{"full", target.Full},
		{"mini", target.Mini},
		{"nano", target.Nano},
		{"classifier", target.Classifier},
	}
	for _, ch := range chains {
		if len(ch.chain) == 0 {
			continue
		}
		out = append(out, chainSurvivorReport(desired, observed, ch.name, ch.chain, ranks))
	}
	for _, def := range target.Definitions {
		if len(def.Chain) == 0 {
			continue
		}
		out = append(out, chainSurvivorReport(desired, observed, def.Path, def.Chain, ranks))
	}
	return out
}

// chainSurvivorReport computes the desired/survived/dropped partition for one chain.
func chainSurvivorReport(desired policy.Desired, observed state.State, name string, chain policy.Chain, ranks reconcile.RankLookup) ChainSurvivorReport {
	effective, err := reconcile.EffectiveOrder(desired, observed, chain, ranks)
	if err != nil {
		effective = []string{}
	}
	// Dropped = desired entries not in effective.
	effSet := make(map[string]bool, len(effective))
	for _, e := range effective {
		effSet[e] = true
	}
	var dropped []string
	for _, entry := range chain {
		if !effSet[entry] {
			dropped = append(dropped, entry)
		}
	}
	return ChainSurvivorReport{
		Name:     name,
		Desired:  append([]string(nil), chain...),
		Survived: effective,
		Dropped:  dropped,
	}
}

// editReport converts one FieldEdit into a human-readable EditReport.
func editReport(fe reconcile.FieldEdit) EditReport {
	r := EditReport{File: fe.File, Path: fe.Path}
	switch {
	case fe.Remove:
		r.Action = "remove"
		r.Detail = "removed"
	case fe.Scalar != nil:
		r.Action = "set-scalar"
		r.Detail = *fe.Scalar
	case len(fe.Sequence) > 0:
		r.Action = "set-sequence"
		r.Detail = fmt.Sprintf("%v", fe.Sequence)
	case fe.Enabled != nil:
		r.Action = "set-bool"
		if *fe.Enabled {
			r.Detail = "true"
		} else {
			r.Detail = "false"
		}
	}
	return r
}

// buildTrace assembles a ReconcileTrace from the computed ranking, plan, and
// observed state. It is a pure projection — it never mutates its inputs.
func buildTrace(
	desired policy.Desired,
	observed state.State,
	target policy.Target,
	ranks reconcile.RankLookup,
	ranking []RankEntryReport,
	plan reconcile.Plan,
) ReconcileTrace {
	return ReconcileTrace{
		ProviderModes: buildProviderModes(desired, observed),
		Ranking:       ranking,
		Chains:        buildChainSurvivors(desired, observed, target, ranks),
		Edits:         editReports(plan),
	}
}

func editReports(plan reconcile.Plan) []EditReport {
	out := make([]EditReport, 0, len(plan.Edits))
	for _, fe := range plan.Edits {
		out = append(out, editReport(fe))
	}
	return out
}
