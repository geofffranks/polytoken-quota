package service

// Reconcile trace types for --verbose output. These are thin presentation
// views constructed from the pure state-owned projections in preparation.go.
// All fields are sanitized — no credentials, auth values, or raw account data.

import (
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
	Name     string   `json:"name"`                     // full, mini, nano, classifier, or definition path
	Desired  []string `json:"desired"`                  // the desired chain as written
	Survived []string `json:"survived"`                 // entries that survived filtering
	Dropped  []string `json:"dropped,omitempty"`        // entries dropped (disabled)
}

// EditReport is one managed-field change the reconciler produced.
type EditReport struct {
	File   string   `json:"file"`
	Path   []string `json:"path"`
	Action string   `json:"action"` // set-scalar, set-sequence, set-bool, remove
	Detail string   `json:"detail"` // the value (sanitized scalar/bool) or "removed"
}

// ReconcileTrace captures the decision data for one target's reconcile pass.
// It is populated only when the caller requests verbose output. It is a thin
// presentation view over the pure projections in preparation.go.
type ReconcileTrace struct {
	ProviderModes []ProviderModeReport  `json:"provider_modes"`
	Ranking       []RankEntryReport     `json:"ranking,omitempty"`
	Chains        []ChainSurvivorReport `json:"chains,omitempty"`
	Edits         []EditReport          `json:"edits,omitempty"`
}

// buildTrace assembles a ReconcileTrace from the computed ranking, plan, and
// observed state. It delegates to the pure projection helpers and converts
// state-owned DTOs into presentation reports. It never mutates its inputs.
func buildTrace(
	desired policy.Desired,
	observed state.State,
	target policy.Target,
	ranks reconcile.RankLookup,
	ranking []RankEntryReport,
	plan reconcile.Plan,
) ReconcileTrace {
	return ReconcileTrace{
		ProviderModes: providerDetailsToReports(ProjectProviders(desired, observed)),
		Ranking:       ranking,
		Chains:        chainDetailsToReports(ProjectChains(desired, observed, target, ranks)),
		Edits:         editDetailsToReports(ProjectEdits(plan.Edits, nil)),
	}
}

func providerDetailsToReports(details []state.ProviderDetail) []ProviderModeReport {
	out := make([]ProviderModeReport, 0, len(details))
	for _, d := range details {
		out = append(out, ProviderModeReport{
			MappingID: d.MappingID,
			Mode:      string(d.Mode),
			Reason:    d.Reason,
		})
	}
	return out
}

func chainDetailsToReports(details []state.ChainDetail) []ChainSurvivorReport {
	out := make([]ChainSurvivorReport, 0, len(details))
	for _, d := range details {
		out = append(out, ChainSurvivorReport{
			Name:     d.Name,
			Desired:  d.Desired,
			Survived: d.Effective,
			Dropped:  d.Dropped,
		})
	}
	return out
}

func editDetailsToReports(details []state.EditDetail) []EditReport {
	out := make([]EditReport, 0, len(details))
	for _, d := range details {
		out = append(out, EditReport{
			File:   d.File,
			Path:   d.Path,
			Action: string(d.Action),
			Detail: d.Detail,
		})
	}
	return out
}
