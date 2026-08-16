package service

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

var noticeAt = time.Date(2026, 8, 16, 2, 0, 5, 0, time.UTC)

func fixtureDesired() policy.Desired {
	codex := policy.Mapping{Models: map[string]policy.ModelBaseline{
		"codex/gpt-5.6-luna": {},
		"codex/gpt-5.6-sol":  {},
	}}
	zai := policy.Mapping{Models: map[string]policy.ModelBaseline{
		"zai/glm-4.6": {},
	}}
	minime := policy.Mapping{Models: map[string]policy.ModelBaseline{
		"minime/off-model": {},
	}}
	return policy.Desired{
		Version:   1,
		Providers: map[policy.MappingID]policy.Mapping{"codex": codex, "zai": zai, "minime": minime},
		Global: policy.Target{
			ID:     "global",
			Global: true,
			Full:   policy.Chain{"codex/gpt-5.6-luna"},
			Definitions: []policy.Definition{
				{Path: "subagents/work-api.md", Chain: policy.Chain{"zai/glm-4.6"}},
			},
		},
		Routing: policy.RoutingConfig{Enabled: true},
	}
}

// TestBuildNoticeInputProjectsAppliedRevision: the pure projection carries the
// committed revision, routing flag, known models, effective chains (global
// chains plus a definition target), changed fields per file, and the standing
// disabled-model set (derived from observed provider modes, not this
// revision's edits).
func TestBuildNoticeInputProjectsAppliedRevision(t *testing.T) {
	desired := fixtureDesired()
	st := state.State{
		Revision: 5,
		Providers: map[string]state.ProviderState{
			// minime is standing-disabled (quota exhausted): its models are in
			// the notice's disabled set regardless of this revision's edits.
			"minime": {Quota: state.QuotaExhausted},
		},
	}

	rt := RegisteredTarget{Policy: desired.Global}
	outcomes := []TargetOutcome{{
		TargetID: targetID(rt),
		Prepare: &PrepareResult{
			PlanComputed: true,
			ChangedEdits: []reconcile.FieldEdit{
				{File: "config.yaml", Path: []string{"models", "zai/glm-4.6", "enabled"}, Enabled: boolPtr(false)},
				{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("codex/gpt-5.6-luna")},
				{File: "subagents/work-api.md", Path: []string{"polytoken", "fallback_models"}, Sequence: []string{"codex/gpt-5.6-sol"}},
			},
		},
	}}

	in := buildNoticeInput(desired, st, []RegisteredTarget{rt}, outcomes, nil, noticeAt)

	if in.Revision != 5 {
		t.Fatalf("revision = %d, want 5", in.Revision)
	}
	if !in.RoutingEnabled {
		t.Fatalf("routing_enabled should be true")
	}
	if !in.KnownModels["codex/gpt-5.6-luna"] || !in.KnownModels["codex/gpt-5.6-sol"] || !in.KnownModels["zai/glm-4.6"] {
		t.Fatalf("known models missing: %v", in.KnownModels)
	}
	if len(in.DisabledModels) != 1 || in.DisabledModels[0] != "minime/off-model" {
		t.Fatalf("disabled_models = %v, want the standing set [minime/off-model]", in.DisabledModels)
	}

	// Expect exactly one global target + one definition target.
	var globalTarget, defTarget *notice.Target
	for i := range in.Targets {
		switch in.Targets[i].Kind {
		case "global":
			globalTarget = &in.Targets[i]
		case "definition":
			defTarget = &in.Targets[i]
		}
	}
	if globalTarget == nil {
		t.Fatalf("no global target in %+v", in.Targets)
	}
	if len(globalTarget.Chains) != 1 || globalTarget.Chains[0].Name != "full" {
		t.Fatalf("global chains = %+v, want a single full chain", globalTarget.Chains)
	}
	if len(globalTarget.Chains[0].Models) != 1 || globalTarget.Chains[0].Models[0] != "codex/gpt-5.6-luna" {
		t.Fatalf("full chain models = %v, want [codex/gpt-5.6-luna]", globalTarget.Chains[0].Models)
	}
	if len(globalTarget.ChangedFields) != 2 {
		t.Fatalf("global changed_fields = %v, want 2", globalTarget.ChangedFields)
	}
	if defTarget == nil {
		t.Fatalf("no definition target in %+v", in.Targets)
	}
	if defTarget.File != "subagents/work-api.md" || len(defTarget.Chain) != 1 || defTarget.Chain[0] != "zai/glm-4.6" {
		t.Fatalf("definition target = %+v, want file subagents/work-api.md chain [zai/glm-4.6]", defTarget)
	}
}

// TestBuildNoticeInputNoPrepareYieldsEmptyEdits: outcomes without a
// Prepare never contribute edits, and the standing disabled set is carried
// even when this revision changed nothing — a session consuming this notice
// long after the disabling revision still sees the actionable tier (AC5).
func TestBuildNoticeInputNoPrepareYieldsEmptyEdits(t *testing.T) {
	desired := fixtureDesired()
	st := state.State{
		Revision:  5,
		Providers: map[string]state.ProviderState{"minime": {Quota: state.QuotaExhausted}},
	}
	rt := RegisteredTarget{Policy: desired.Global}
	outcomes := []TargetOutcome{{TargetID: targetID(rt)}}
	in := buildNoticeInput(desired, st, []RegisteredTarget{rt}, outcomes, nil, noticeAt)
	if len(in.DisabledModels) != 1 || in.DisabledModels[0] != "minime/off-model" {
		t.Fatalf("disabled_models = %v, want the standing set [minime/off-model]", in.DisabledModels)
	}
	if len(in.Targets) < 1 {
		t.Fatalf("expected at least a global target")
	}
}
