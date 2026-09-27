package reconcile

// The modelgroups guard: Polytoken rejects a version-4 config that combines a
// legacy tier default with an explicit model-group definition, and the staged
// merge composes the global and project layers, so a modelgroups key on any
// registered layer makes every tier-default write an invalid candidate. When the
// service coordinator stamps a target as living on a modelgroups config surface,
// Build must leave every managed tier-default field operator-owned and report
// the skipped fields, while models.*.enabled and definition-file edits keep
// working. A target without the stamp keeps today's byte-for-byte behavior.

import (
	"slices"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// tierDefaultEditFields lists the four managed config.yaml tier-default fields
// in Build's fixed order.
var tierDefaultEditFields = []string{
	"defaults.full",
	"defaults.mini",
	"defaults.nano",
	"autonomous_permission_matcher.classifier_model",
}

// tierDefaultEditPaths maps each tier-default field to its config.yaml key path.
var tierDefaultEditPaths = map[string][]string{
	"defaults.full": {"defaults", "full"},
	"defaults.mini":  {"defaults", "mini"},
	"defaults.nano":  {"defaults", "nano"},
	"autonomous_permission_matcher.classifier_model": {"autonomous_permission_matcher", "classifier_model"},
}

// modelGroupsFixture returns a healthy desired/state/target triple managing all
// four tier-default fields plus one definition, with the modelgroups stamp set
// exactly when enabled is true.
func modelGroupsFixture(enabled bool) (policy.Desired, state.State, policy.Target) {
	d, s, target := fixture("codex/gpt", "zai/glm")
	target.Global = true
	target.Full = policy.Chain{"codex/gpt", "zai/glm"}
	target.Mini = policy.Chain{"zai/glm"}
	target.Nano = policy.Chain{"codex/gpt"}
	target.Classifier = policy.Chain{"zai/glm"}
	target.UsesModelGroups = enabled
	return d, s, target
}

// assertNoTierDefaultEdits fails the test when the plan proposes any
// tier-default edit.
func assertNoTierDefaultEdits(t *testing.T, p Plan) {
	t.Helper()
	for _, e := range p.Edits {
		for _, field := range tierDefaultEditFields {
			if slices.Equal(e.Path, tierDefaultEditPaths[field]) {
				t.Fatalf("plan proposes tier-default edit %s under modelgroups: %+v", field, e)
			}
		}
	}
}

// skippedFields returns the sorted field names in the plan's skip diagnostics.
func skippedFields(p Plan) []string {
	out := make([]string, 0, len(p.Skipped))
	for _, sk := range p.Skipped {
		out = append(out, sk.Field)
	}
	slices.Sort(out)
	return out
}

func TestModelGroupsSkipsTierDefaults(t *testing.T) {
	d, s, target := modelGroupsFixture(true)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoTierDefaultEdits(t, p)
	want := append([]string(nil), tierDefaultEditFields...)
	slices.Sort(want)
	if got := skippedFields(p); !slices.Equal(got, want) {
		t.Fatalf("skipped fields = %v, want %v", got, want)
	}
	for _, sk := range p.Skipped {
		if sk.Reason != modelGroupsSkipReason {
			t.Fatalf("skip reason for %s = %q, want %q", sk.Field, sk.Reason, modelGroupsSkipReason)
		}
	}
	// models.*.enabled edits keep working.
	for _, base := range []string{"codex/gpt", "zai/glm"} {
		if got := enabledEdit(t, p, base); got.Enabled == nil || !*got.Enabled {
			t.Fatalf("%s enabled edit = %+v", base, got)
		}
	}
	// Definition-file edits keep working.
	if got := chainEdit(t, p, "agent.md"); !slices.Equal(got, []string{"codex/gpt", "zai/glm"}) {
		t.Fatalf("definition chain = %v", got)
	}
}

func TestModelGroupsSkipsRoutingReorderDefaults(t *testing.T) {
	// Routing with inverted ranks: without the guard the reorder would rewrite
	// every tier-default first survivor to zai/glm. Under modelgroups the plan
	// must not propose tier-default edits at all, while the definition file
	// still carries the reordered chain.
	d, s, target := modelGroupsFixture(true)
	d.Routing.Enabled = true
	ranks := RankLookup{"codex": 1, "zai": 0}
	p, err := Build(d, s, target, ranks)
	if err != nil {
		t.Fatal(err)
	}
	assertNoTierDefaultEdits(t, p)
	if len(p.Skipped) == 0 {
		t.Fatalf("no skip diagnostics on reordered plan %+v", p)
	}
	if got := chainEdit(t, p, "agent.md"); !slices.Equal(got, []string{"zai/glm", "codex/gpt"}) {
		t.Fatalf("definition chain = %v, want the reordered chain", got)
	}
}

func TestWithoutModelGroupsDefaultsUnchanged(t *testing.T) {
	// Control: a target without the stamp keeps today's behavior — tier-default
	// edits are proposed and no skip diagnostics exist.
	d, s, target := modelGroupsFixture(false)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := skippedFields(p); len(got) != 0 {
		t.Fatalf("unexpected skip diagnostics: %v", got)
	}
	if e := scalarEdit(t, p, "defaults", "full"); e.Scalar == nil || *e.Scalar != "codex/gpt" {
		t.Fatalf("defaults.full edit = %+v", e)
	}
	if e := scalarEdit(t, p, "autonomous_permission_matcher", "classifier_model"); e.Scalar == nil || *e.Scalar != "zai/glm" {
		t.Fatalf("classifier_model edit = %+v", e)
	}
}

func TestModelGroupsSkipsOnlyManagedFields(t *testing.T) {
	// A field this target does not manage produces no skip diagnostic: nothing
	// would have been written for it anyway.
	d, s, target := modelGroupsFixture(true)
	target.Nano = nil
	target.Classifier = nil
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"defaults.full", "defaults.mini"}
	if got := skippedFields(p); !slices.Equal(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestModelGroupsDeadDefaultsChainDoesNotBlockTarget(t *testing.T) {
	// Under modelgroups a tier-default chain whose every provider is disabled
	// must neither fail the render nor block the remaining managed edits: the
	// field is left operator-owned.
	d, s, target := modelGroupsFixture(true)
	target.Full = policy.Chain{"codex/gpt"}
	target.Definitions = []policy.Definition{{Path: "agent.md", Chain: policy.Chain{"zai/glm"}}}
	setMode(&s, "codex", state.ModeDisabled)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatalf("modelgroups target must not fail on a dead defaults chain: %v", err)
	}
	assertNoTierDefaultEdits(t, p)
	if got := chainEdit(t, p, "agent.md"); !slices.Equal(got, []string{"zai/glm"}) {
		t.Fatalf("definition chain = %v", got)
	}
}
