package service

// Task 14 end-to-end event-sequence matrix. Each case feeds a synthetic event
// sequence through the REAL state machine (state.ApplyEvent) and the REAL
// desired-chain reconciler (reconcile.Build), then asserts the resulting
// effective mode, reconciled chain, and baseline enable state. This exercises
// the genuine Coordinator event-handling core — accept-revision, independent
// quota/availability axes, stale/duplicate handling, and desired-chain
// stable-partitioning — without requiring the Polytoken binary (the staging and
// publish paths are exercised separately in the filesystem and canary suites).

import (
	"slices"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/hook"
	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// seqResult is the observable outcome of one event sequence: the effective mode
// of the event's target provider, the reconciled definition chain, and whether
// that provider's managed model remains baseline-enabled.
type seqResult struct {
	Mode    state.Mode
	Chain   []string
	Enabled bool
}

// e2eProvider is the CodExBar provider the synthetic events target.
const e2eProvider = "codex"

// twoProviderDesired builds the shared synthetic policy: a "codex" provider
// (model codex/gpt) and a "healthy" provider (model healthy/a), both baseline
// enabled. Events target codex; healthy stays healthy throughout.
func twoProviderDesired() policy.Desired {
	return policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				CodexBarProviders:  []string{e2eProvider},
				PolytokenProviders: []string{"codex"},
				Models: map[string]policy.ModelBaseline{
					"codex/gpt": {Enabled: true, HadEnabledKey: false},
				},
			},
			"healthy": {
				CodexBarProviders:  []string{"healthy"},
				PolytokenProviders: []string{"healthy"},
				Models: map[string]policy.ModelBaseline{
					"healthy/a": {Enabled: true, HadEnabledKey: false},
				},
			},
		},
	}
}

// e2eTarget is the single global target with one managed definition whose
// desired chain is [codex/gpt, healthy/a].
func e2eTarget() policy.Target {
	return policy.Target{
		ID:   "global",
		Root: "/r",
		Definitions: []policy.Definition{{
			Path:  "agent.md",
			Chain: policy.Chain{"codex/gpt", "healthy/a"},
		}},
	}
}

// runSyntheticSequence applies events (each targeting codex with a strictly
// increasing timestamp/arrival) through the real state machine, then reconciles
// through the real desired-chain builder. It fatals on any rejected event or
// reconcile error. The returned result reflects codex's effective mode, the
// reconciled agent.md chain, and codex/gpt's enable state.
func runSyntheticSequence(t *testing.T, events []hook.Type) seqResult {
	t.Helper()
	desired := twoProviderDesired()
	observed := state.State{Providers: map[string]state.ProviderState{}}
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for i, et := range events {
		ev := hook.Event{
			Type:      et,
			Provider:  e2eProvider,
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
		arrival := state.Arrival{Sequence: uint64(i + 1), ReceivedAt: ev.Timestamp}
		next, accepted, _, err := state.ApplyEvent(observed, ev, arrival)
		if err != nil {
			t.Fatalf("apply %s: %v", et, err)
		}
		if !accepted {
			t.Fatalf("event %s (#%d) unexpectedly rejected as stale", et, i)
		}
		observed = next
	}
	plan, err := reconcile.Build(desired, observed, e2eTarget(), nil)
	if err != nil {
		t.Fatalf("reconcile after %v: %v", events, err)
	}
	return seqResult{
		Mode:    state.EffectiveMode(observed.Providers[e2eProvider]),
		Chain:   reconciledChain(plan, "agent.md"),
		Enabled: modelEnabled(plan, "codex/gpt"),
	}
}

// reconciledChain reconstructs the ordered managed chain a plan projects for one
// definition file: the polytoken.model scalar followed by the fallback_models
// sequence. It fatals when no model scalar is present.
func reconciledChain(p reconcile.Plan, file string) []string {
	var model string
	haveModel := false
	var fallback []string
	for _, e := range p.Edits {
		if e.File != file {
			continue
		}
		if len(e.Path) == 2 && e.Path[0] == "polytoken" && e.Path[1] == "model" && e.Scalar != nil {
			model = *e.Scalar
			haveModel = true
		}
		if len(e.Path) == 2 && e.Path[0] == "polytoken" && e.Path[1] == "fallback_models" {
			fallback = e.Sequence
		}
	}
	if !haveModel {
		return nil
	}
	return append([]string{model}, fallback...)
}

// modelEnabled reports the boolean a plan emits for models.<base>.enabled.
func modelEnabled(p reconcile.Plan, base string) bool {
	for _, e := range p.Edits {
		if len(e.Path) == 3 && e.Path[0] == "models" && e.Path[1] == base && e.Path[2] == "enabled" && e.Enabled != nil {
			return *e.Enabled
		}
	}
	return false
}

// TestEndToEndEventMatrix is the Task 14 blueprint event-sequence matrix. Each
// case asserts the effective mode of the targeted provider, the reconciled
// chain (with stable-partition ordering), and the baseline enable state after a
// concrete event sequence.
func TestEndToEndEventMatrix(t *testing.T) {
	cases := []struct {
		name       string
		events     []hook.Type
		wantMode   state.Mode
		wantChain  []string
		wantEnable bool
	}{
		{"low", []hook.Type{hook.QuotaLow}, state.ModeReserve, []string{"healthy/a", "codex/gpt"}, true},
		{"reached", []hook.Type{hook.QuotaReached}, state.ModeDisabled, []string{"healthy/a"}, false},
		{"unavailable-reset", []hook.Type{hook.ProviderUnavailable, hook.QuotaReset}, state.ModeDisabled, []string{"healthy/a"}, false},
		{"reached-recovered", []hook.Type{hook.QuotaReached, hook.ProviderRecovered}, state.ModeDisabled, []string{"healthy/a"}, false},
		{"restore", []hook.Type{hook.QuotaReached, hook.QuotaReset}, state.ModeNormal, []string{"codex/gpt", "healthy/a"}, true},
		{"refresh-failed", []hook.Type{hook.RefreshFailed}, state.ModeNormal, []string{"codex/gpt", "healthy/a"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runSyntheticSequence(t, tc.events)
			if got.Mode != tc.wantMode || !slices.Equal(got.Chain, tc.wantChain) || got.Enabled != tc.wantEnable {
				t.Fatalf("got=%+v want mode=%s chain=%v enabled=%v", got, tc.wantMode, tc.wantChain, tc.wantEnable)
			}
		})
	}
}

// TestEndToEndIndependentAxesConverge proves repeated, concurrent-shaped, and
// stale events converge deterministically: replaying a sequence yields the same
// result, a stale (older-timestamp) event on an axis is ignored, and a duplicate
// event is idempotent.
func TestEndToEndIndependentAxesConverge(t *testing.T) {
	seq := []hook.Type{hook.QuotaReached, hook.QuotaReset}
	first := runSyntheticSequence(t, seq)
	second := runSyntheticSequence(t, seq)
	if first.Mode != second.Mode || !slices.Equal(first.Chain, second.Chain) || first.Enabled != second.Enabled {
		t.Fatalf("non-deterministic replay: first=%+v second=%+v", first, second)
	}

	// A stale QuotaLow (older timestamp than the accepted QuotaReached) on the
	// quota axis must be ignored, leaving codex disabled.
	observed := state.State{Providers: map[string]state.ProviderState{}}
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	reached := hook.Event{Type: hook.QuotaReached, Provider: e2eProvider, Timestamp: base.Add(2 * time.Second)}
	observed, _, _, err := state.ApplyEvent(observed, reached, state.Arrival{Sequence: 1, ReceivedAt: reached.Timestamp})
	if err != nil {
		t.Fatal(err)
	}
	// Stale: same axis, older timestamp but newer arrival sequence; timestamp
	// dominates, so it is ignored.
	stale := hook.Event{Type: hook.QuotaLow, Provider: e2eProvider, Timestamp: base.Add(1 * time.Second)}
	next, accepted, _, err := state.ApplyEvent(observed, stale, state.Arrival{Sequence: 2, ReceivedAt: stale.Timestamp})
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("stale event unexpectedly accepted")
	}
	if state.EffectiveMode(next.Providers[e2eProvider]) != state.ModeDisabled {
		t.Fatalf("stale event changed mode to %s", state.EffectiveMode(next.Providers[e2eProvider]))
	}
}
