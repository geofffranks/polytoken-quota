package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/service"
)

// The modelgroups skip diagnostic renders in the verbose document for applied
// and pending targets alike, sanitized, with the field and reason; a quiet
// reconcile stays silent (the pinned quiet contract covers this matrix, so
// only the verbose side is exercised here).
func TestReconcileVerboseRendersSkippedDefaults(t *testing.T) {
	spy := &verboseOutcomeSpy{}
	spy.outcome = service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{{
			TargetID: "global",
			Skipped: []reconcile.SkippedEdit{
				{Field: "defaults.full", Reason: "config uses modelgroups"},
				{Field: "defaults.mini", Reason: "config uses modelgroups"},
			},
		}},
	}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code := Run(context.Background(), []string{"reconcile", "--verbose"}, strings.NewReader(""), stdout, stderr, Dependencies{
		Mutator: spy, Diagnoser: spy, Environment: func() map[string]string { return nil },
	})
	if code != ExitOK {
		t.Fatalf("exit=%d want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"skipped: defaults.full (config uses modelgroups)",
		"skipped: defaults.mini (config uses modelgroups)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("verbose document missing %q:\n%s", want, out)
		}
	}
}
