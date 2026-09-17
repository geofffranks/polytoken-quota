package service

// Diagnostic carriage tests: the ephemeral CommandDiagnostic must reach the
// TargetOutcome for verbose rendering while remaining structurally incapable
// of reaching any durable artifact — state.json (including its embedded
// ReconcileHistory) keeps exactly its pre-existing keys.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// TestDiagnosticNeverPersisted proves the carriage boundary: a pending
// reconcile through real collaborators attaches the full diagnostic to the
// in-memory outcome, while the persisted state.json — targets AND any embedded
// history — carries no new keys.
func TestDiagnosticNeverPersisted(t *testing.T) {
	coord := newRetentionHarness(t)
	out := coord.Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 1 {
		t.Fatalf("expected one pending target, got accepted=%v pending=%d", out.Accepted, out.PendingCount())
	}
	pending := out.Targets[0]
	if pending.Diagnostic == nil || pending.Diagnostic.FullOutput == "" {
		t.Fatalf("pending outcome carried no full diagnostic: %+v", pending)
	}
	if pending.Diagnostic.Stage != validate.Doctor {
		t.Fatalf("diagnostic stage=%q want doctor", pending.Diagnostic.Stage)
	}
	if strings.Contains(pending.Diagnostic.FullOutput, "output truncated at") {
		t.Fatalf("in-budget output carried a truncation marker: %q", pending.Diagnostic.FullOutput)
	}

	ss, ok := coord.State.(StoreState)
	if !ok {
		t.Fatalf("harness state store is %T, want StoreState", coord.State)
	}
	raw, err := os.ReadFile(ss.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	for _, forbidden := range []string{"FullOutput", "Diagnostic", "Truncated"} {
		if strings.Contains(blob, forbidden) {
			t.Fatalf("persisted state.json carries diagnostic field %q", forbidden)
		}
	}

	// The recorded pending keeps exactly the pre-existing ApplyFailure keys.
	var doc struct {
		Targets map[string]json.RawMessage `json:"Targets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var ts struct {
		Pending map[string]json.RawMessage `json:"Pending"`
	}
	if err := json.Unmarshal(doc.Targets["global"], &ts); err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{
		"TargetID": true, "Stage": true, "File": true, "Chain": true,
		"Summary": true, "Remediation": true, "LastSuccessfulRevision": true,
		"AttemptedRevision": true, "LastSuccessfulAt": true, "AttemptedAt": true,
		"ResolvedAt": true, "Reproduces": true, "LiveStatus": true,
	}
	for key := range ts.Pending {
		if !known[key] {
			t.Fatalf("persisted pending gained new key %q", key)
		}
	}
}

// TestSecretCanaryFullOutputClean extends the canary guarantee to the new
// ephemeral channel: the full sanitized diagnostic of a failed validation and
// the error-chain diagnostic of a quota-own failure never carry the canary.
func TestSecretCanaryFullOutputClean(t *testing.T) {
	root := t.TempDir()
	stageTmp := filepath.Join(root, "staging-tmp")
	if err := os.MkdirAll(stageTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "source")
	sourceConfig := "providers:\n  codex:\n    api_key: " + canarySecret + "\n" +
		"models:\n  codex/gpt:\n    enabled: true\ndefaults:\n  full: codex/gpt\n"
	writeFile(t, filepath.Join(sourceDir, "config.yaml"), sourceConfig)
	writeFile(t, filepath.Join(sourceDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	res := target.Resolved{ID: "global", CanonicalRoot: sourceDir, Global: true}

	cand := canaryStage(t, sourceDir, stageTmp, res)
	t.Cleanup(func() { _ = cand.Cleanup() })

	runner := validate.Runner{Binary: "polytoken", Commands: canaryStderrRunner{canary: canarySecret}}
	result := runner.Validate(context.Background(), cand, time.Second)
	if result.Error == nil || result.Diagnostic == nil {
		t.Fatalf("expected error and diagnostic, got %+v", result)
	}
	full := result.Diagnostic.FullOutput
	if full == "" {
		t.Fatal("expected non-empty full output")
	}
	if strings.Contains(full, canarySecret) {
		t.Fatalf("canary survived sanitization into full output: %q", full)
	}

	// The quota-own constructor redacts the same way.
	d := validate.InternalDiagnostic("stage", errors.New("stage: failed: api_key="+canarySecret))
	if d.FullOutput == "" || strings.Contains(d.FullOutput, canarySecret) {
		t.Fatalf("internal diagnostic leaked the canary: %q", d.FullOutput)
	}
	if !strings.Contains(d.FullOutput, "<redacted>") {
		t.Fatalf("internal diagnostic did not redact the credential assignment: %q", d.FullOutput)
	}
}
