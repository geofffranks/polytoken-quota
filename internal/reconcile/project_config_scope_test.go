package reconcile

// pq-m4k10: the models baseline enable block is a global-layer concern only.
// Polytoken's project config layer treats a `models` entry as a custom model
// override that requires a provider definition (config validate --project
// fails with "custom model overrides require a provider reference"), so a
// project target's plan must never address the config.yaml models block. The
// global target keeps the baseline block unchanged.

import (
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// TestProjectPlanHasNoModelsBaselineEdits proves a project target's plan never
// touches config.yaml's models block, while its own managed chain scalars
// (valid project-layer keys) are still projected.
func TestProjectPlanHasNoModelsBaselineEdits(t *testing.T) {
	d, s, target := fixture("a/x", "b/x")
	target.Global = false
	target.Full = policy.Chain{"a/x", "b/x"}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Edits {
		if e.File == configFile && len(e.Path) > 0 && e.Path[0] == "models" {
			t.Fatalf("project plan contains a models baseline edit: %+v", e)
		}
	}
	if scalarEdit(t, p, "defaults", "full").Scalar == nil {
		t.Fatalf("project defaults.full edit missing from plan %+v", p)
	}
}
