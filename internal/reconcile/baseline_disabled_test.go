package reconcile

// pq-m4k8: a chain entry whose baseline enabled flag is false is unusable.
// Polytoken's subagent registry rejects any reference to a disabled model
// (contract fixture disabled-fallback: config validate passes, doctor fails),
// so a chain that survives with a baseline-disabled model produces a candidate
// that can never validate: Build writes the reference into the definition file
// while writing enabled=false for the same model into the config's models
// block. Baseline-disabled models must therefore drop from chains exactly like
// mode-disabled mappings.

import (
	"errors"
	"slices"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

func baselineDisable(d *policy.Desired, base string) {
	mid := policy.MappingID(providerOf(base))
	m := d.Providers[mid]
	m.Models[base] = policy.ModelBaseline{Enabled: false, HadEnabledKey: true}
	d.Providers[mid] = m
}

func TestBaselineDisabledRemovedFromChains(t *testing.T) {
	d, s, target := fixture("a/x", "b/x", "c/x")
	baselineDisable(&d, "b/x")
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	want := []string{"a/x", "c/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestBaselineDisabledScalarFirstSurvivorSkipped(t *testing.T) {
	d, s, target := fixture("bad/x", "good/x")
	baselineDisable(&d, "bad/x")
	target.Full = policy.Chain{"bad/x", "good/x"}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	full := scalarEdit(t, p, "defaults", "full")
	if full.Scalar == nil || *full.Scalar != "good/x" {
		t.Fatalf("defaults.full=%+v, want good/x (baseline-disabled head must not be written)", full)
	}
}

func TestBaselineDisabledAllDroppedIsEmptyChain(t *testing.T) {
	d, s, target := fixture("a/x", "b/x")
	baselineDisable(&d, "a/x")
	baselineDisable(&d, "b/x")
	target.Full = policy.Chain{"a/x", "b/x"}
	p, err := Build(d, s, target, nil)
	if err == nil {
		t.Fatalf("plan=%+v err=nil", p)
	}
	var empty EmptyChainError
	if !errors.As(err, &empty) || len(p.Edits) != 0 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestEffectiveOrderExcludesBaselineDisabled(t *testing.T) {
	d, s, _ := fixture("a/x", "b/x", "c/x")
	baselineDisable(&d, "b/x")
	chain := policy.Chain{"a/x", "b/x", "c/x"}
	got, err := EffectiveOrder(d, s, chain, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/x", "c/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
