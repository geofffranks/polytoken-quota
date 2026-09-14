package staging_test

// External-package test for the D4 fail-closed pin. It lives outside package
// staging because it asserts the SANITIZED stage error through
// validate.DefaultSanitize, and validate imports staging (an in-package test
// file cannot import it without an import cycle).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/testutil"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// TestRegisteredDefinitionOutsideAllowlistFailsClosed pins D4 at the staged-edit
// failure site: a plan-referenced (registered) definition outside the allowlist
// fails staging with the allowlist message naming the file — never a silent
// skip — and the message survives validate.DefaultSanitize with the file name
// and its guidance intact (exactly what pendingOutcome persists).
func TestRegisteredDefinitionOutsideAllowlistFailsClosed(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	testutil.WriteFile(t, filepath.Join(globalDir, "config.yaml"), "models: {}\n")
	testutil.WriteFile(t, filepath.Join(globalDir, "subagents", "a.md"),
		"---\npolytoken:\n  model: m\n---\nbody\n")
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	plan := reconcile.Plan{TargetID: res.ID, Edits: []reconcile.FieldEdit{
		{File: "agents/research.md", Path: []string{"polytoken", "model"}},
	}}
	plan.Edits[0].Scalar = new(string)
	*plan.Edits[0].Scalar = "m"
	_, err := staging.Builder{
		TempRoot: t.TempDir(),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}.Build(context.Background(), res, plan, nil)
	if err == nil {
		t.Fatal("registered definition outside the allowlist did not fail staging")
	}
	sanitized := validate.DefaultSanitize([]byte(err.Error()))
	for _, want := range []string{
		`definition "agents/research.md" is outside the staging allowlist`,
		"move it under facets/ or subagents/ or update the registered policy",
	} {
		if !strings.Contains(sanitized, want) {
			t.Fatalf("sanitized stage error %q does not contain %q", sanitized, want)
		}
	}
}
