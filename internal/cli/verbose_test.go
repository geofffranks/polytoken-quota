package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/service"
)

// verboseOutcomeSpy is a minimal Mutator that returns a preset Outcome with
// trace data, for testing the --verbose rendering.
type verboseOutcomeSpy struct {
	outcomeSpy
}

func TestReconcileVerboseFlagParses(t *testing.T) {
	dryRun, keepStaging, verbose, ok := parseReconcileFlags([]string{"--verbose"})
	if !ok || !verbose || dryRun || keepStaging {
		t.Fatalf("got dryRun=%v keepStaging=%v verbose=%v ok=%v", dryRun, keepStaging, verbose, ok)
	}
}

func TestReconcileVerboseWithDryRun(t *testing.T) {
	dryRun, keepStaging, verbose, ok := parseReconcileFlags([]string{"--verbose", "--dry-run"})
	if !ok || !verbose || !dryRun || keepStaging {
		t.Fatalf("got dryRun=%v keepStaging=%v verbose=%v ok=%v", dryRun, keepStaging, verbose, ok)
	}
}

func TestReconcileVerboseRendersTrace(t *testing.T) {
	mode := "normal"
	spy := &verboseOutcomeSpy{}
	spy.outcome = service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{{
			TargetID: "global",
			Trace: &service.ReconcileTrace{
				ProviderModes: []service.ProviderModeReport{{
					MappingID: "codex",
					Mode:      mode,
					Reason:    "healthy",
				}},
				Chains: []service.ChainSurvivorReport{{
					Name:     "full",
					Desired:  []string{"codex/gpt", "zai/glm"},
					Survived: []string{"codex/gpt", "zai/glm"},
				}},
				Edits: []service.EditReport{{
					File:   "config.yaml",
					Path:   []string{"defaults", "full"},
					Action: "set-scalar",
					Detail: "codex/gpt",
				}},
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
	if !strings.Contains(out, "provider modes:") {
		t.Fatalf("missing provider modes in verbose output:\n%s", out)
	}
	if !strings.Contains(out, "codex") {
		t.Fatalf("missing codex in verbose output:\n%s", out)
	}
	if !strings.Contains(out, "chains:") {
		t.Fatalf("missing chains in verbose output:\n%s", out)
	}
	if !strings.Contains(out, "edits:") {
		t.Fatalf("missing edits in verbose output:\n%s", out)
	}
}

func TestReconcileTraceJSONDoesNotGainExplainFields(t *testing.T) {
	raw, err := json.Marshal(service.ReconcileTrace{
		Ranking: []service.RankEntryReport{{MappingID: "codex", Rank: 0, Eligible: true, Explanation: "peak"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"status"`) {
		t.Fatalf("reconcile trace JSON gained explain-only status field: %s", raw)
	}
}

func TestReconcileVerboseNoTraceIsGraceful(t *testing.T) {
	spy := &verboseOutcomeSpy{}
	spy.outcome = service.Outcome{
		Accepted: true,
		Targets:  []service.TargetOutcome{{TargetID: "global"}},
	}
	stdout := &strings.Builder{}
	_ = Run(context.Background(), []string{"reconcile", "--verbose"}, strings.NewReader(""), stdout, io.Discard, Dependencies{
		Mutator: spy, Diagnoser: spy, Environment: func() map[string]string { return nil },
	})
	if !strings.Contains(stdout.String(), "(no trace data)") {
		t.Fatalf("expected graceful no-trace message:\n%s", stdout.String())
	}
}
