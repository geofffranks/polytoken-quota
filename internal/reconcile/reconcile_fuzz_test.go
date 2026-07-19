package reconcile

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// FuzzReconcile asserts that Build is deterministic and never emits a managed
// reference to a disabled or desired-absent model.
func FuzzReconcile(f *testing.F) {
	f.Add(uint8(0))
	f.Fuzz(func(t *testing.T, modes uint8) {
		d, s, target := fuzzFixture(modes)
		a, e1 := Build(d, s, target)
		b, e2 := Build(d, s, target)
		if fmt.Sprint(e1) != fmt.Sprint(e2) || !reflect.DeepEqual(a, b) {
			t.Fatal("nondeterministic")
		}
		assertNoInjectedOrDisabled(t, d, s, a)
	})
}

// fuzzFixture builds a three-mapping target where each provider's mode is selected
// from two bits of the seed: 0 normal, 1 reserve, otherwise disabled. Every managed
// chain shares the same three models so all outputs are desired survivors.
func fuzzFixture(modes uint8) (policy.Desired, state.State, policy.Target) {
	names := []string{"p0/m", "p1/m", "p2/m"}
	d, s, target := fixture(names...)
	chain := append(policy.Chain(nil), names...)
	target.Full = append(policy.Chain(nil), chain...)
	target.Mini = append(policy.Chain(nil), chain...)
	for i := range names {
		mid := fmt.Sprintf("p%d", i)
		switch (modes >> uint(2*i)) & 3 {
		case 0:
			setMode(&s, mid, state.ModeNormal)
		case 1:
			setMode(&s, mid, state.ModeReserve)
		default:
			setMode(&s, mid, state.ModeDisabled)
		}
	}
	return d, s, target
}

// assertNoInjectedOrDisabled checks every emitted model spelling resolves to a
// managed, non-disabled provider mapping (so the plan injects nothing foreign and
// emits no disabled model). It uses the package's own mappingMode for an honest
// check.
func assertNoInjectedOrDisabled(t *testing.T, d policy.Desired, s state.State, p Plan) {
	t.Helper()
	for _, e := range p.Edits {
		for _, spelling := range emittedSpellings(e) {
			ref, err := ParseModelRef(spelling)
			if err != nil {
				t.Fatalf("emitted unparseable model %q: %v", spelling, err)
			}
			mid, err := d.ResolveModel(ref.Base)
			if err != nil {
				t.Fatalf("emitted model %q absent from managed mappings: %v", spelling, err)
			}
			if mappingMode(d, s, mid) == state.ModeDisabled {
				t.Fatalf("emitted disabled model %q", spelling)
			}
		}
	}
}

func emittedSpellings(e FieldEdit) []string {
	var out []string
	if e.Scalar != nil {
		out = append(out, *e.Scalar)
	}
	out = append(out, e.Sequence...)
	return out
}
