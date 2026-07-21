package service

// Task 14 secret-canary security test. A synthetic, non-personal canary is
// injected across every channel the tool reads — environment, provider auth
// blocks, captured command stderr, unregistered project paths, and the JSON
// account field — and the success, validation-failure, and recovery scenarios
// are run through the REAL decode, staging (AuthInert), validate-redaction,
// and publish paths. The canary must never appear in any durable artifact
// (state.json, backups, journal) or sanitized diagnostic output (logs), and no
// staging root may survive cleanup.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/hook"
	"github.com/geofffranks/codexbar-hooks/internal/publish"
	"github.com/geofffranks/codexbar-hooks/internal/reconcile"
	"github.com/geofffranks/codexbar-hooks/internal/staging"
	"github.com/geofffranks/codexbar-hooks/internal/state"
	"github.com/geofffranks/codexbar-hooks/internal/target"
	"github.com/geofffranks/codexbar-hooks/internal/testutil"
	"github.com/geofffranks/codexbar-hooks/internal/validate"
)

// canarySecret is the synthetic, non-personal sentinel injected everywhere.
const canarySecret = "PQ_CANARY_7f8a"

// runCanaryScenario builds an isolated root, injects canarySecret across every
// input channel, runs the named scenarios through the real pipeline, and
// returns the root holding state.json, backups/, journal/, logs/, and the
// staging temp area. Each scenario cleans up its own staging candidate.
func runCanaryScenario(t *testing.T, canary string, scenarios []string) string {
	t.Helper()
	root := t.TempDir()
	stageTmp := filepath.Join(root, "staging-tmp")
	if err := os.MkdirAll(stageTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state.json")
	backupRoot := filepath.Join(root, "backups")
	journalPath := filepath.Join(root, "journal", "apply.json")
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The canary-bearing SOURCE layer (read by staging; AuthInert redacts it).
	// The live managed target file is clean so its backup never carries the
	// canary.
	sourceDir := filepath.Join(root, "source")
	sourceConfig := "providers:\n  codex:\n    api_key: " + canary + "\n" +
		"models:\n  codex/gpt:\n    enabled: true\ndefaults:\n  full: codex/gpt\n"
	testutil.WriteFile(t, filepath.Join(sourceDir, "config.yaml"), sourceConfig)
	testutil.WriteFile(t, filepath.Join(sourceDir, "subagents", "agent.md"),
		"---\npolytoken:\n  model: codex/gpt\n---\nbody\n")

	// An unregistered project path whose name and content carry the canary. The
	// tool never scans or adopts unregistered roots.
	unregDir := filepath.Join(root, "unregistered-"+canary)
	testutil.WriteFile(t, filepath.Join(unregDir, "secret.yaml"), "api_key: "+canary+"\n")

	store := state.Store{Path: statePath, RecoveredRetention: 24 * time.Hour}
	clock := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

	res := target.Resolved{ID: "global", CanonicalRoot: sourceDir, Global: true,
		DefinitionFiles: []string{filepath.Join(sourceDir, "subagents", "agent.md")}}

	for _, sc := range scenarios {
		switch sc {
		case "success":
			canarySuccess(t, root, canary, store, clock, sourceDir, stageTmp, res, journalPath, backupRoot)
		case "validation-failure":
			canaryValidationFailure(t, root, canary, stageTmp, logsDir, sourceDir, res)
		case "recovery":
			canaryRecovery(t, canary, store, clock, stageTmp, sourceDir, res, journalPath, backupRoot)
		default:
			t.Fatalf("unknown canary scenario %q", sc)
		}
	}
	return root
}

// canarySuccess exercises the full accept→stage→publish path with the canary in
// the JSON account field and env. The decoded event (account discarded) drives a
// state save; AuthInert staging redacts the source api_key; the real Publisher
// commits state/journal/backup carrying only hashes and clean bytes.
func canarySuccess(t *testing.T, root, canary string, store state.Store, clock func() time.Time,
	sourceDir, stageTmp string, res target.Resolved, journalPath, backupRoot string) {
	t.Helper()
	// Decode a payload whose account is the canary; account is discarded.
	payload := `{"event":"quota_low","provider":"codex","account":"` + canary + `","timestamp":"2026-07-19T12:00:01Z"}`
	env := map[string]string{"CODEXBAR_TRACE_ID": canary} // unrecognized env var, never persisted
	ev, err := hook.Decode(strings.NewReader(payload), env, 4096)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(fmtEvent(ev), canary) {
		t.Fatal("canary survived decode into the normalized event")
	}
	// Apply the event to state and persist: state.json carries only quota axes.
	observed, _, _, err := state.ApplyEvent(state.State{Providers: map[string]state.ProviderState{}},
		ev, state.Arrival{Sequence: 1, ReceivedAt: clock()})
	if err != nil {
		t.Fatal(err)
	}
	observed.Schema = 1
	observed.Revision = 2
	if err := store.Save(observed); err != nil {
		t.Fatal(err)
	}

	// Stage from the canary-bearing source under AuthInert and publish.
	cand := canaryStage(t, sourceDir, stageTmp, res)
	t.Cleanup(func() { _ = cand.Cleanup() })
	staged, err := os.ReadFile(filepath.Join(cand.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(staged, []byte(canary)) {
		t.Fatal("AuthInert left canary in staged candidate")
	}
	canaryPublish(t, cand, store, clock, res, journalPath, backupRoot, root)
	_ = cand.Cleanup() // staging bytes consumed; remove the transient root now
}

// canaryValidationFailure proves captured command stderr carrying the canary is
// sanitized before it can be persisted as a diagnostic. It runs the real
// validate.Runner with a spy returning canary-laden stderr and writes the
// sanitized summary to logs/, then confirms the log has no canary.
func canaryValidationFailure(t *testing.T, root, canary, stageTmp, logsDir, sourceDir string, res target.Resolved) {
	t.Helper()
	cand := canaryStage(t, sourceDir, stageTmp, res)
	t.Cleanup(func() { _ = cand.Cleanup() })
	runner := validate.Runner{
		Binary:   "polytoken",
		Commands: canaryStderrRunner{canary: canary},
	}
	result := runner.Validate(context.Background(), cand, time.Second)
	if result.StartupValid {
		t.Fatal("expected validation failure")
	}
	if result.Error == nil {
		t.Fatal("expected sanitized error")
	}
	if strings.Contains(result.Error.Summary, canary) {
		t.Fatalf("canary survived sanitization into summary: %q", result.Error.Summary)
	}
	// Simulate persisting the diagnostic to logs (as status/doctor would).
	testutil.WriteFile(t, filepath.Join(logsDir, "validation-failure.txt"), result.Error.Summary+"\n")
}

// canaryRecovery proves an interrupted apply recovers to a coherent commit and
// the surviving journal (only hashes/paths) plus the restored backup never carry
// the canary. It stages a redacted candidate, publishes with a fault at the
// rename step (journal durable), then recovers.
func canaryRecovery(t *testing.T, canary string, store state.Store, clock func() time.Time,
	stageTmp, sourceDir string, res target.Resolved, journalPath, backupRoot string) {
	t.Helper()
	prior := state.State{Schema: 1, Revision: 5, Providers: map[string]state.ProviderState{},
		Targets: map[string]state.TargetState{}}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	cand := canaryStage(t, sourceDir, stageTmp, res)
	t.Cleanup(func() { _ = cand.Cleanup() })
	// A clean live managed file (no canary) so its backup is clean.
	liveDir := filepath.Join(filepath.Dir(journalPath), "live-"+sanitize(canary))
	livePath := filepath.Join(liveDir, "agent.md")
	testutil.WriteFile(t, livePath, "---\npolytoken:\n  model: codex/gpt\n---\nbody\n")
	candBytes := readOrFail(t, filepath.Join(cand.ConfigDir, "subagents", "agent.md"))
	tempPath := filepath.Join(liveDir, ".candidate-agent.md")
	testutil.WriteFile(t, tempPath, string(candBytes))
	_ = cand.Cleanup() // staging bytes consumed; remove the transient root now
	next := prior
	next.Revision = 6
	pub := publish.Publisher{
		Locker: realFlock{path: filepath.Join(filepath.Dir(journalPath), "r.lock")},
		State:  store, JournalPath: journalPath,
		Backups:     publish.BackupStore{Root: backupRoot, Limit: 3},
		ManagedRoot: liveDir, Clock: clock,
		Fault: faultAt("rename"),
	}
	tx := publish.Transaction{Prior: prior, Next: next, TargetID: "global", Replacements: []publish.Replacement{{
		LivePath: livePath, TempPath: tempPath,
		OldHash: sha256sum(readOrFail(t, livePath)), NewHash: sha256sum(string(candBytes)),
		Mode: 0o600,
	}}}
	if _, err := pub.ApplyUnderLock(context.Background(), tx); err == nil {
		t.Fatal("expected apply to fail at the rename fault")
	}
	// Recover to a coherent commit with the fault cleared.
	pub.Fault = nil
	if _, _, err := pub.Recover(context.Background(), prior); err != nil {
		t.Fatalf("recover: %v", err)
	}
}

// TestNoSecretCanaryPersists is the Task 14 blueprint secret-canary test. It
// injects canarySecret across environment, auth blocks, stderr, project paths,
// and account input; runs success, validation-failure, and recovery; then scans
// state.json, backups, journal, and logs for the canary and asserts it never
// persists, and that no staging root survives.
func TestNoSecretCanaryPersists(t *testing.T) {
	root := runCanaryScenario(t, canarySecret, []string{"success", "validation-failure", "recovery"})
	for _, tree := range []string{"state.json", "backups", "journal", "logs"} {
		path := filepath.Join(root, tree)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue // tree not produced by a scenario
		}
		scanTree(t, path, func(b []byte) {
			if bytes.Contains(b, []byte(canarySecret)) {
				t.Fatalf("canary %q persisted in %s", canarySecret, tree)
			}
		})
	}
	assertNoStagingRoots(t, root)
}

// --- canary helpers ---------------------------------------------------------

// canaryStage builds an AuthInert candidate from sourceDir under stageTmp.
func canaryStage(t *testing.T, sourceDir, stageTmp string, res target.Resolved) staging.Candidate {
	t.Helper()
	c, err := staging.Builder{
		TempRoot: stageTmp, AuthMode: staging.AuthInert,
		Sources: staging.FSMaterializer{GlobalDir: sourceDir},
	}.Build(context.Background(), res, reconcile.Plan{TargetID: res.ID})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return c
}

// canaryPublish publishes the staged candidate's agent.md through the real
// Publisher so journal/backup/state artifacts land under root.
func canaryPublish(t *testing.T, cand staging.Candidate, store state.Store, clock func() time.Time,
	res target.Resolved, journalPath, backupRoot, root string) {
	t.Helper()
	liveDir := filepath.Join(root, "live")
	livePath := filepath.Join(liveDir, "agent.md")
	testutil.WriteFile(t, livePath, "---\npolytoken:\n  model: old/a\n---\nbody\n")
	candBytes := readOrFail(t, filepath.Join(cand.ConfigDir, "subagents", "agent.md"))
	tempPath := filepath.Join(liveDir, ".candidate-agent.md")
	testutil.WriteFile(t, tempPath, string(candBytes))
	prior := state.State{Schema: 1, Revision: 3, Providers: map[string]state.ProviderState{},
		Targets: map[string]state.TargetState{}}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	next := prior
	next.Revision = 4
	pub := publish.Publisher{
		Locker: realFlock{path: filepath.Join(root, "lock.lock")}, State: store,
		JournalPath: journalPath, Backups: publish.BackupStore{Root: backupRoot, Limit: 3},
		ManagedRoot: liveDir, Clock: clock,
	}
	tx := publish.Transaction{Prior: prior, Next: next, TargetID: res.ID, Replacements: []publish.Replacement{{
		LivePath: livePath, TempPath: tempPath,
		OldHash: sha256sum(readOrFail(t, livePath)), NewHash: sha256sum(string(candBytes)),
		Mode: 0o600,
	}}}
	if _, err := pub.ApplyUnderLock(context.Background(), tx); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// canaryStderrRunner is a validate.CommandRunner that returns canary-laden
// stderr to exercise the real sanitizer.
type canaryStderrRunner struct{ canary string }

func (r canaryStderrRunner) Run(context.Context, string, []string, int64, map[string]string) ([]byte, []byte, int, error) {
	// Realistic secret-bearing forms a binary might echo in diagnostics: a
	// credential assignment, a token header, and a bearer token. DefaultSanitize
	// redacts all three before any summary is persisted.
	stderr := "error: api_key=" + r.canary + " invalid\n" +
		"auth_token: " + r.canary + "\n" +
		"Authorization: Bearer " + r.canary + "\n"
	return []byte(""), []byte(stderr), 1, nil
}

// scanTree walks path and invokes fn on every regular file's bytes.
func scanTree(t *testing.T, path string, fn func([]byte)) {
	t.Helper()
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(b)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
}

// assertNoStagingRoots fails if any transient staging root (quota-stage-*)
// survives under root after the scenarios. Every scenario cleans up its
// candidate; none should leak.
func assertNoStagingRoots(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), "quota-stage-") {
			t.Fatalf("transient staging root survived: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for staging roots: %v", err)
	}
}

// faultAt returns a publish.FaultHook that fails once at step.
func faultAt(step string) publish.FaultHook {
	fired := false
	return func(s string) error {
		if !fired && s == step {
			fired = true
			return publish.ErrInjected
		}
		return nil
	}
}

func readOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fmtEvent renders an event for substring checks without importing fmt at the
// call site repeatedly.
func fmtEvent(e hook.Event) string {
	var b strings.Builder
	b.WriteString(string(e.Type))
	b.WriteString(e.Provider)
	if e.Status != nil {
		b.WriteString(*e.Status)
	}
	if e.Window != nil {
		b.WriteString(*e.Window)
	}
	return b.String()
}

// sanitize reduces a string to filesystem-safe characters (for temp dir names).
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
