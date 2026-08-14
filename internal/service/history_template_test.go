package service

import (
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TestHistoryTriggerMatrix verifies that TriggerFromTransaction maps every
// coordinator transaction kind to the correct typed state trigger with
// kind-relevant sanitized fields.
func TestHistoryTriggerMatrix(t *testing.T) {
	tests := []struct {
		name string
		kind transactionKind
		in   transactionInput
		want state.Trigger
	}{
		{
			name: "init",
			kind: txInit,
			in:   transactionInput{},
			want: state.Trigger{Kind: state.TriggerInit},
		},
		{
			name: "explicit reconcile",
			kind: txReconcile,
			in:   transactionInput{},
			want: state.Trigger{Kind: state.TriggerReconcile},
		},
		{
			name: "routing disable",
			kind: txDisable,
			in:   transactionInput{Provider: "anthropic"},
			want: state.Trigger{Kind: state.TriggerRoutingDisable, MappingID: "anthropic"},
		},
		{
			name: "routing enable",
			kind: txEnable,
			in:   transactionInput{Provider: "anthropic"},
			want: state.Trigger{Kind: state.TriggerRoutingEnable, MappingID: "anthropic"},
		},
		{
			name: "routing reset",
			kind: txReset,
			in:   transactionInput{},
			want: state.Trigger{Kind: state.TriggerRoutingReset},
		},
		{
			name: "quota check with mapping filter",
			kind: txQuotaCheck,
			in:   transactionInput{Provider: "openai"},
			want: state.Trigger{Kind: state.TriggerQuotaCheck, MappingID: "openai"},
		},
		{
			name: "quota check all",
			kind: txQuotaCheck,
			in:   transactionInput{},
			want: state.Trigger{Kind: state.TriggerQuotaCheck},
		},
		{
			name: "set with quota patch",
			kind: txSet,
			in: transactionInput{
				Provider: "codex",
				Patch: state.ProviderPatch{
					Quota:        quotaPtr(state.QuotaLow),
					Availability: availPtr(state.Unavailable),
				},
			},
			want: state.Trigger{
				Kind: state.TriggerSet,
				Set: &state.SetEvidence{
					Provider:     "codex",
					Quota:        quotaPtr(state.QuotaLow),
					Availability: availPtr(state.Unavailable),
				},
			},
		},
		{
			name: "clear specific provider",
			kind: txClear,
			in:   transactionInput{Selector: state.Selector{Provider: "codex"}},
			want: state.Trigger{
				Kind: state.TriggerClear,
				Clear: &state.ClearEvidence{
					Provider: "codex",
				},
			},
		},
		{
			name: "clear all",
			kind: txClear,
			in:   transactionInput{Selector: state.Selector{All: true}},
			want: state.Trigger{
				Kind: state.TriggerClear,
				Clear: &state.ClearEvidence{
					All: true,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TriggerFromTransaction(tc.kind, tc.in)
			if got.Kind != tc.want.Kind {
				t.Errorf("Kind: got %s, want %s", got.Kind, tc.want.Kind)
			}
			if got.MappingID != tc.want.MappingID {
				t.Errorf("MappingID: got %q, want %q", got.MappingID, tc.want.MappingID)
			}
			if (got.Set == nil) != (tc.want.Set == nil) {
				t.Errorf("Set presence: got %v, want %v", got.Set != nil, tc.want.Set != nil)
			}
			if got.Set != nil && tc.want.Set != nil {
				if got.Set.Provider != tc.want.Set.Provider {
					t.Errorf("Set.Provider: got %q, want %q", got.Set.Provider, tc.want.Set.Provider)
				}
				if (got.Set.Quota == nil) != (tc.want.Set.Quota == nil) {
					t.Errorf("Set.Quota presence mismatch")
				}
			}
			if (got.Clear == nil) != (tc.want.Clear == nil) {
				t.Errorf("Clear presence: got %v, want %v", got.Clear != nil, tc.want.Clear != nil)
			}
			if got.Clear != nil && tc.want.Clear != nil {
				if got.Clear.Provider != tc.want.Clear.Provider {
					t.Errorf("Clear.Provider: got %q, want %q", got.Clear.Provider, tc.want.Clear.Provider)
				}
				if got.Clear.All != tc.want.Clear.All {
					t.Errorf("Clear.All: got %v, want %v", got.Clear.All, tc.want.Clear.All)
				}
			}
		})
	}
}

// TestExcludedTransactionsDoNotRecordHistory verifies that dry-run, unaccepted,
// and no-proven-change transactions are excluded from history.
func TestExcludedTransactionsDoNotRecordHistory(t *testing.T) {
	noChange := []PrepareResult{{ChangedFiles: map[string]bool{}}}
	hasChange := []PrepareResult{{ChangedFiles: map[string]bool{"config.yaml": true}}}

	tests := []struct {
		name        string
		dryRun      bool
		accepted    bool
		prepResults []PrepareResult
		want        bool
	}{
		{"dry run excluded", true, true, hasChange, false},
		{"unaccepted excluded", false, false, hasChange, false},
		{"no proven change excluded", false, true, noChange, false},
		{"qualified included", false, true, hasChange, true},
		{"nil prep results excluded", false, true, nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldRecordHistory(tc.dryRun, tc.accepted, tc.prepResults)
			if got != tc.want {
				t.Errorf("ShouldRecordHistory: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildRecordTemplateBasic verifies that BuildRecordTemplate produces a
// template with correct trigger, revision, and bounded projections.
func TestBuildRecordTemplateBasic(t *testing.T) {
	trigger := state.Trigger{Kind: state.TriggerReconcile}
	targets := []TargetTemplateInput{
		{
			TargetID:     "global",
			PlanComputed: true,
			ChangedEdits: []reconcile.FieldEdit{}, // need import
		},
	}

	tpl := BuildRecordTemplate(trigger, 42, policy.Desired{}, state.State{}, nil, routing.RankingResult{}, targets)

	if tpl.Revision != 42 {
		t.Errorf("Revision: got %d, want 42", tpl.Revision)
	}
	if tpl.Trigger.Kind != state.TriggerReconcile {
		t.Errorf("Trigger.Kind: got %s, want %s", tpl.Trigger.Kind, state.TriggerReconcile)
	}
	if len(tpl.Targets) != 1 {
		t.Fatalf("Targets: got %d, want 1", len(tpl.Targets))
	}
	if tpl.Targets[0].ID != "global" {
		t.Errorf("Target[0].ID: got %q, want global", tpl.Targets[0].ID)
	}
	if !tpl.Targets[0].PlanComputed {
		t.Error("Target[0].PlanComputed should be true")
	}
}

// TestBuildRecordTemplateEmpty verifies that an empty target list produces a
// valid template with no targets.
func TestBuildRecordTemplateEmpty(t *testing.T) {
	tpl := BuildRecordTemplate(
		state.Trigger{Kind: state.TriggerInit},
		1,
		policy.Desired{}, state.State{}, nil, routing.RankingResult{},
		nil,
	)
	if len(tpl.Targets) != 0 {
		t.Errorf("Targets: got %d, want 0", len(tpl.Targets))
	}
	if tpl.Revision != 1 {
		t.Errorf("Revision: got %d, want 1", tpl.Revision)
	}
}

// --- helpers ---------------------------------------------------------------

func quotaPtr(q state.Quota) *state.Quota               { return &q }
func availPtr(a state.Availability) *state.Availability { return &a }
