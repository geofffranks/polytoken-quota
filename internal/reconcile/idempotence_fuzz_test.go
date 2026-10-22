package reconcile

// Task 14 reconcile property/fuzz corpus additions. FuzzReconcile (in
// reconcile_fuzz_test.go) already pins determinism, no-disabled-references, and
// no-injected-model. This file adds duplicate idempotence and stable-partition
// determinism properties: applying the same seed twice yields identical plans.
// Repeating a desired-chain entry repeats its survivor occurrence in place —
// the survivor projection preserves duplicate occurrences, so the fallback
// list grows by exactly that repeated occurrence while the managed scalar
// still surfaces only the first survivor.

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// FuzzReconcileDuplicateIdempotent proves Build is idempotent under repeated
// invocation and stable when a desired chain repeats an entry. For each seed it
// builds a three-mapping target, duplicates the first survivor into the chain,
// and asserts two Builds are identical. The duplicate occurrence survives in
// place (the fallback list carries it); the scalar still carries only the
// first survivor.
func FuzzReconcileDuplicateIdempotent(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(0b000101)) // one reserve, one disabled
	f.Add(uint8(0b111111)) // all disabled
	f.Fuzz(func(t *testing.T, modes uint8) {
		d, s, target := fuzzFixture(modes)
		// Duplicate the first desired chain entry to exercise duplicate handling.
		chain := append(policy.Chain(nil), target.Full...)
		if len(chain) > 0 {
			chain = append(chain, chain[0])
			target.Full = chain
		}
		a, ea := Build(d, s, target, nil)
		b, eb := Build(d, s, target, nil)
		if fmt.Sprint(ea) != fmt.Sprint(eb) || !reflect.DeepEqual(a, b) {
			t.Fatal("Build is not idempotent under duplicate entries")
		}
		// On a successful build, emitted full-default scalar spellings must each
		// resolve to a managed, non-disabled mapping (nothing injected; the
		// duplicate occurrence lives in the fallback list, not the scalar). An
		// empty-chain error is a valid, deterministic outcome.
		if ea == nil {
			assertNoInjectedOrDisabled(t, d, s, a)
		}
		// Repeated Build with the SAME inputs (no duplication) must also match.
		d2, s2, target2 := fuzzFixture(modes)
		c, ec := Build(d2, s2, target2, nil)
		if fmt.Sprint(ec) != fmt.Sprint(ea) || !reflect.DeepEqual(c, a) {
			t.Fatal("Build is not deterministic across equal fixtures")
		}
	})
}

// TestReconcileStablePartitionIsDeterministic is a non-fuzz pin: the survivor
// partition (normal then reserve, desired order preserved) is deterministic
// across equal states, including when providers cross the reserve boundary.
func TestReconcileStablePartitionIsDeterministic(t *testing.T) {
	names := []string{"a/x", "b/x", "c/x", "d/x"}
	d, s, target := fixture(names...)
	target.Full = append(policy.Chain(nil), names...)
	setMode(&s, "b", state.ModeReserve)
	setMode(&s, "d", state.ModeReserve)
	p1, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("stable partition nondeterministic")
	}
	// Normal survivors precede reserve survivors, preserving desired order.
	e := scalarEdit(t, p1, "defaults", "full")
	if e.Scalar == nil || *e.Scalar != "a/x" {
		t.Fatalf("first survivor = %v want a/x (normal-first)", e.Scalar)
	}
}
