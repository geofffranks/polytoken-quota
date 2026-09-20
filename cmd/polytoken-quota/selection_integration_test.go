package main

// selection_integration_test.go — approved acceptance integration tests for the
// select command's durable quota-evidence contract.
//
// Unlike the internal package tests (which drive in-memory spies), these tests
// run the real production wiring end to end:
//
//	newCoordinator(config{temp utility root})
//	  → real Coordinator with a real state.Store (temp StoreState)
//	  → the production Coordinator snapshot source / selectionRefresh adapter
//	  → selection.SelectRunner with a fake NewAssessor factory
//	  → cli.Run (the actual select command surface, exit codes and output)
//
// so the acceptance criteria are proven against actual persistence:
//
//   - TestSelectRefreshIntegration: an explicit --refresh performs exactly one
//     quota check through the locked transaction path and durably persists the
//     observation to state.json; it never reconciles (no target validation, no
//     publish, no journal, no backups, no staging). The subsequent selection
//     sees the refreshed snapshot. An accepted check that carries provider
//     problems keeps the last-good snapshot usable. A fatal check stops the
//     selection before any assessment (Jev) can run.
//   - TestSelectReadOnlySnapshot: the default select reads the saved evidence
//     without polling and without writing state, history, or targets — proven
//     against the actual store, both for a missing state file (empty state,
//     not an error) and a corrupt state file (fatal, file left untouched).
//   - TestSelectIntegrationSecretCanaries: synthetic canary prompt, key, and
//     upstream strings never reach the persisted state or the CLI output, and
//     the injected fake is the only assessor ever constructed — the automatic
//     factory (which resolves the real TYPESAFE_API_KEY) is deliberately not
//     wired, so no live request path exists in this test.
//
// Everything is offline: the quota poller and the assessor are in-memory
// fakes; the only real I/O is the utility root under t.TempDir() (canonical
// TMPDIR).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/cli"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/selection"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Synthetic canary strings that must never leak into persisted state or CLI
// output. The key never reaches any code path: the fake assessor factory is
// injected instead of the automatic one, so resolveRuntimeKey is unreachable.
const (
	selectionCanaryPrompt   = "SYNTHETIC selection task canary-4f7a: write a haiku about quotas"
	selectionCanaryKey      = "sk-canary-6b2e-never-a-real-key"
	selectionCanaryUpstream = "UPSTREAM-RAW-BODY-CANARY-9c1d"
)

// selectionTestClock is the fake clock injected into the Coordinator and the
// state store so every timestamp is deterministic.
type selectionTestClock struct{ now time.Time }

func (c selectionTestClock) Now() time.Time { return c.now }

// stubSelectionPoller is the fake service.QuotaPoller: preset snapshots keyed
// by mapping ID, an optional hard poll error, and a call counter. It performs
// no I/O and never contacts a provider.
type stubSelectionPoller struct {
	snapshots map[string]quota.QuotaSnapshot
	pollErr   error
	calls     int
}

func (p *stubSelectionPoller) Poll(_ context.Context, desired policy.Desired, provider string, _ time.Time) (map[string]quota.QuotaSnapshot, error) {
	p.calls++
	if p.pollErr != nil {
		return nil, p.pollErr
	}
	out := make(map[string]quota.QuotaSnapshot, len(p.snapshots))
	for id, snap := range p.snapshots {
		if provider != "" && id != provider {
			continue
		}
		out[id] = snap
	}
	return out, nil
}

// stubSelectionAssessor is the fake selection.Assessor: it records the
// disclosure state and prompt it received and returns a preset assessment. It
// resolves no credential, opens no connection, and never touches the network.
type stubSelectionAssessor struct {
	assessment  selection.Assessment
	err         error
	calls       int
	lastEnabled bool
	lastPrompt  string
}

func (a *stubSelectionAssessor) Assess(_ context.Context, enabled bool, prompt string) (selection.Assessment, error) {
	a.calls++
	a.lastEnabled = enabled
	a.lastPrompt = prompt
	if a.err != nil {
		return selection.Assessment{}, a.err
	}
	return a.assessment, nil
}

// selectionTestEnv is one fully wired production utility root under t.TempDir:
// the real newCoordinator config (lock, journal, backups, staging, publish,
// validate) with only the poller, clock, and assessor factory replaced by
// fakes. Everything else — the Coordinator, its transaction path, the state
// store, the selection adapters — is exactly what main() runs.
type selectionTestEnv struct {
	home          string
	desiredPath   string
	candidatePath string
	statePath     string
	journalPath   string
	lockPath      string
	backupsRoot   string
	stagingRoot   string

	now      time.Time
	coord    *service.Coordinator
	poller   *stubSelectionPoller
	assessor *stubSelectionAssessor

	// assessor factory observation: which model pin and timeout the runner
	// asked for, and how many assessors were constructed.
	factoryCalls   int
	factoryModel   string
	factoryTimeout time.Duration
}

// newSelectionTestEnv writes a valid desired.yaml (codex mapping with default
// quota routing, JEV disclosure enabled) plus a candidate policy document, and
// wires the real coordinator over a temp utility root.
func newSelectionTestEnv(t *testing.T) *selectionTestEnv {
	t.Helper()
	home := t.TempDir()
	env := &selectionTestEnv{
		home:          home,
		desiredPath:   filepath.Join(home, "desired.yaml"),
		candidatePath: filepath.Join(home, "candidate-policy.yaml"),
		statePath:     filepath.Join(home, "state.json"),
		journalPath:   filepath.Join(home, "journal", "apply.json"),
		lockPath:      filepath.Join(home, "lock", "apply.lock"),
		backupsRoot:   filepath.Join(home, "backups"),
		stagingRoot:   filepath.Join(home, "stage"),
		now:           time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		poller:        &stubSelectionPoller{snapshots: map[string]quota.QuotaSnapshot{}},
		assessor: &stubSelectionAssessor{assessment: selection.Assessment{
			Model:      policy.DocumentedJevModel,
			Tier:       selection.TierNormal,
			Confidence: 0.93,
		}},
	}

	desired := "version: 1\n" +
		"providers:\n" +
		"  codex:\n" +
		"    models: [codex/gpt]\n" +
		"selection:\n" +
		"  jev:\n" +
		"    enabled: true\n"
	if err := os.WriteFile(env.desiredPath, []byte(desired), 0o600); err != nil {
		t.Fatalf("write desired.yaml: %v", err)
	}
	candidate := "version: 1\n" +
		"phases:\n" +
		"  execution:\n" +
		"    normal:\n" +
		"      - - codex/gpt\n"
	if err := os.WriteFile(env.candidatePath, []byte(candidate), 0o600); err != nil {
		t.Fatalf("write candidate policy: %v", err)
	}

	// The real production wiring; only the poller and clock are swapped for
	// fakes. The validate runner's binary is never invoked by select paths.
	cfg := config{
		Home:         home,
		DesiredPath:  env.desiredPath,
		StatePath:    env.statePath,
		LockPath:     env.lockPath,
		JournalPath:  env.journalPath,
		BackupsRoot:  env.backupsRoot,
		StagingRoot:  env.stagingRoot,
		GlobalDir:    filepath.Join(home, "polytoken-config"),
		PolytokenBin: filepath.Join(home, "unused-polytoken-binary"),
		BackupCount:  5,
		Retention:    7 * 24 * time.Hour,
		LockWait:     10 * time.Second,
		ValidateWait: 30 * time.Second,
		PolytokenEnv: map[string]string{},
	}
	env.coord = newCoordinator(cfg)
	env.coord.QuotaPoller = env.poller
	env.coord.Clock = selectionTestClock{now: env.now}
	return env
}

// selectRunner mirrors newSelectionRunners but injects the fake assessor
// factory: the automatic factory would resolve the real TYPESAFE_API_KEY and
// construct a live client, which tests must never do.
func (e *selectionTestEnv) selectRunner() *selection.SelectRunner {
	return &selection.SelectRunner{
		Snapshot: e.coord,
		Refresh:  selectionRefresh{coord: e.coord},
		NewAssessor: func(model string, timeout time.Duration) (selection.Assessor, error) {
			e.factoryCalls++
			e.factoryModel = model
			e.factoryTimeout = timeout
			return e.assessor, nil
		},
	}
}

// deps mirrors main()'s cli.Dependencies wiring.
func (e *selectionTestEnv) deps() cli.Dependencies {
	return cli.Dependencies{
		Mutator:         e.coord,
		Diagnoser:       e.coord,
		SnapshotBuilder: e.coord,
		HistoryQuerier:  service.NewHistoryReader(service.StoreState{Store: state.Store{Path: e.statePath}}, nil),
		Policy:          e.coord.Policy,
		Select:          e.selectRunner(),
	}
}

// runSelect invokes the real select command via cli.Run. stdin always carries
// the canary task; explicit-difficulty invocations never read it.
func (e *selectionTestEnv) runSelect(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"select"}, args...),
		strings.NewReader(selectionCanaryPrompt), &stdout, &stderr, e.deps())
	return code, stdout.String(), stderr.String()
}

// diskStore is an independent read-only store over the on-disk state file.
func (e *selectionTestEnv) diskStore() state.Store {
	return state.Store{Path: e.statePath, Now: func() time.Time { return e.now }, RecoveredRetention: 7 * 24 * time.Hour}
}

// loadDiskState loads the persisted state from disk.
func (e *selectionTestEnv) loadDiskState(t *testing.T) state.State {
	t.Helper()
	st, err := e.diskStore().Load()
	if err != nil {
		t.Fatalf("load on-disk state: %v", err)
	}
	return st
}

// stateBytes reads the raw persisted state file.
func (e *selectionTestEnv) stateBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(e.statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	return data
}

// seedState durably writes prior observed state: explicit provider axes (the
// event-derived quota/availability state machine output) and an optional
// last-good snapshot, at revision 1.
func (e *selectionTestEnv) seedState(t *testing.T, snap *quota.QuotaSnapshot) {
	t.Helper()
	st := state.State{
		Schema:   state.CurrentSchema,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {
				Quota:          state.QuotaNormal,
				Availability:   state.Available,
				QuotaAt:        e.now.Add(-time.Hour),
				AvailabilityAt: e.now.Add(-time.Hour),
			},
		},
		Targets: map[string]state.TargetState{},
	}
	if snap != nil {
		good := *snap
		ps := st.Providers["codex"]
		ps.QuotaSnapshot = &good
		ps.QuotaAttempt = &good
		st.Providers["codex"] = ps
	}
	if err := e.diskStore().Save(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// polledSnap builds the successful observation the fake poller reports.
func polledSnap(mappingID string, checkedAt time.Time) quota.QuotaSnapshot {
	used, limit := 20.0, 100.0
	return quota.QuotaSnapshot{
		MappingID:    mappingID,
		CheckedAt:    checkedAt,
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows:      []quota.QuotaWindow{{Name: "daily", Used: &used, Limit: &limit}},
	}
}

// failedPolledSnap builds the failed observation (a provider problem).
func failedPolledSnap(mappingID string, checkedAt time.Time, reason string) quota.QuotaSnapshot {
	return quota.QuotaSnapshot{
		MappingID:    mappingID,
		CheckedAt:    checkedAt,
		Availability: quota.QuotaUnknown,
		Status:       quota.SourceFailed,
		Error:        reason,
	}
}

// assertAbsent fails when path exists.
func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (stat error: %v); expected it to be absent", path, err)
	}
}

// assertNoReconcileSideEffects proves the quota-check-without-reconcile
// contract: no journal, no backups, no staging candidates, no recorded target
// outcomes, and no reconcile history in the persisted state.
func assertNoReconcileSideEffects(t *testing.T, env *selectionTestEnv, st state.State) {
	t.Helper()
	assertAbsent(t, env.journalPath)
	assertAbsent(t, env.backupsRoot)
	assertAbsent(t, env.stagingRoot)
	if len(st.Targets) != 0 {
		t.Fatalf("state recorded %d target outcomes; a select refresh must never reconcile targets", len(st.Targets))
	}
	if len(st.ReconcileHistory.Records) != 0 {
		t.Fatalf("state recorded %d reconcile history records; a select refresh must never reconcile", len(st.ReconcileHistory.Records))
	}
}

// TestSelectRefreshIntegration proves the --refresh acceptance criteria
// against the real Coordinator and real on-disk state: one poll, actual
// persistence, no reconcile/publish, refreshed evidence reaching the
// selection, last-good preservation on accepted-with-problems, and a fatal
// check stopping before any assessment.
func TestSelectRefreshIntegration(t *testing.T) {
	t.Run("first refresh confirms without legacy provider axes", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		env.poller.snapshots["codex"] = polledSnap("codex", env.now)
		args := []string{"--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal"}
		code, stdout, stderr := env.runSelect(t, append(args, "--refresh")...)
		if code != cli.ExitOK || !strings.Contains(stdout, "status: confirmed") {
			t.Fatalf("first refresh: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		ps := env.loadDiskState(t).Providers["codex"]
		if ps.Quota != "" || ps.Availability != "" {
			t.Fatal("polling must not manufacture legacy provider axes")
		}
		code, stdout, stderr = env.runSelect(t, args...)
		if code != cli.ExitOK || !strings.Contains(stdout, "status: confirmed") || env.poller.calls != 1 {
			t.Fatalf("saved evidence: exit=%d polls=%d stdout=%q stderr=%q", code, env.poller.calls, stdout, stderr)
		}
		assertNoReconcileSideEffects(t, env, env.loadDiskState(t))
	})
	t.Run("explicit refresh persists the snapshot and the selection sees it", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		env.seedState(t, nil) // explicit axes, no quota evidence yet
		before := env.stateBytes(t)

		// Default (no --refresh): the saved (empty) evidence yields the safe
		// missing-evidence result; nothing is polled or written.
		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal")
		if code != cli.ExitPending {
			t.Fatalf("select without refresh: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "status: uncertain") || !strings.Contains(stdout, "evidence: missing") {
			t.Fatalf("select without refresh: stdout=%q, want uncertain/missing", stdout)
		}
		if env.poller.calls != 0 {
			t.Fatalf("select without refresh polled %d times; default select must not poll", env.poller.calls)
		}
		if !bytes.Equal(env.stateBytes(t), before) {
			t.Fatal("select without refresh modified state.json")
		}

		// Explicit --refresh: exactly one poll, durably persisted, and the
		// selection now confirms against the refreshed snapshot.
		env.poller.snapshots["codex"] = polledSnap("codex", env.now)
		code, stdout, stderr = env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal", "--refresh")
		if code != cli.ExitOK {
			t.Fatalf("select --refresh: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if env.poller.calls != 1 {
			t.Fatalf("poller called %d times; explicit --refresh must poll exactly once", env.poller.calls)
		}
		if !strings.Contains(stdout, "status: confirmed") {
			t.Fatalf("select --refresh stdout=%q, want a confirmed selection over the refreshed snapshot", stdout)
		}
		for _, want := range []string{"model: codex/gpt", "mapping: codex", "headroom: 80%", "tier: normal", "as_of: 2026-09-18T12:00:00Z"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("select --refresh stdout=%q missing %q", stdout, want)
			}
		}

		disk := env.loadDiskState(t)
		if disk.Revision != 2 {
			t.Fatalf("persisted revision=%d, want 2 (refresh accepted and committed)", disk.Revision)
		}
		ps, ok := disk.Providers["codex"]
		if !ok || ps.QuotaSnapshot == nil || ps.QuotaAttempt == nil {
			t.Fatalf("refreshed snapshot not persisted to state.json: %+v", disk.Providers["codex"])
		}
		if ps.QuotaSnapshot.CheckedAt != env.now || ps.QuotaSnapshot.Status != quota.SourceFresh || ps.QuotaSnapshot.Availability != quota.QuotaAvailable {
			t.Fatalf("persisted snapshot = %+v, want the polled fresh/available observation at %s", ps.QuotaSnapshot, env.now)
		}
		if len(ps.QuotaSnapshot.Windows) != 1 {
			t.Fatalf("persisted snapshot windows = %d, want 1", len(ps.QuotaSnapshot.Windows))
		}
		assertNoReconcileSideEffects(t, env, disk)
		if len(disk.EventHistory.Events) != 0 {
			t.Fatalf("successful refresh recorded %d events; want none", len(disk.EventHistory.Events))
		}
	})

	t.Run("accepted check with provider problems keeps last-good evidence usable", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		lastGoodAt := env.now.Add(-time.Minute)
		env.seedState(t, &[]quota.QuotaSnapshot{polledSnap("codex", lastGoodAt)}[0])
		env.poller.snapshots["codex"] = failedPolledSnap("codex", env.now, "synthetic provider outage")

		// The check is accepted (the problem is a provider-level observation,
		// not a refresh failure), so the selection proceeds on last-good
		// evidence instead of failing or going missing.
		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal", "--refresh")
		if code != cli.ExitOK {
			t.Fatalf("select --refresh with provider problem: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "status: confirmed") {
			t.Fatalf("stdout=%q, want a confirmed selection on last-good evidence", stdout)
		}

		disk := env.loadDiskState(t)
		ps := disk.Providers["codex"]
		if ps.QuotaSnapshot == nil || !ps.QuotaSnapshot.CheckedAt.Equal(lastGoodAt) {
			t.Fatalf("last-good snapshot not preserved: %+v (want CheckedAt %s)", ps.QuotaSnapshot, lastGoodAt)
		}
		if ps.QuotaAttempt == nil || ps.QuotaAttempt.Status != quota.SourceFailed || !ps.QuotaAttempt.CheckedAt.Equal(env.now) {
			t.Fatalf("failed attempt not recorded: %+v", ps.QuotaAttempt)
		}
		if disk.Revision != 2 {
			t.Fatalf("revision=%d, want 2", disk.Revision)
		}
		if len(disk.EventHistory.Events) != 1 || disk.EventHistory.Events[0].Action != "refresh_failed" {
			t.Fatalf("provider problem not recorded as a quota failure event: %+v", disk.EventHistory.Events)
		}
		assertNoReconcileSideEffects(t, env, disk)
	})

	t.Run("fatal check stops the selection before any assessment", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		env.seedState(t, nil)
		before := env.stateBytes(t)
		env.poller.pollErr = errors.New("synthetic poller outage " + selectionCanaryUpstream)

		// No explicit difficulty: the request would proceed to assessment, so
		// this proves the fatal refresh stops before Jev is ever constructed.
		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--refresh")
		if code != cli.ExitRejected {
			t.Fatalf("select with failing refresh: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout=%q, want empty on a fatal refresh", stdout)
		}
		if want := "selection: quota refresh failed; selection stopped\n"; stderr != want {
			t.Fatalf("stderr=%q, want exactly %q (fixed safe sentence, no raw cause)", stderr, want)
		}
		if strings.Contains(stderr, selectionCanaryUpstream) {
			t.Fatal("fatal message leaked the raw poller cause")
		}
		if env.poller.calls != 1 {
			t.Fatalf("poller called %d times, want exactly 1", env.poller.calls)
		}
		if env.assessor.calls != 0 || env.factoryCalls != 0 {
			t.Fatalf("assessment ran despite the fatal refresh (assess=%d, factory=%d)", env.assessor.calls, env.factoryCalls)
		}
		if !bytes.Equal(env.stateBytes(t), before) {
			t.Fatal("a failed refresh must not mutate the persisted state")
		}
		disk, err := env.diskStore().Load()
		if err != nil {
			t.Fatalf("reload state: %v", err)
		}
		assertNoReconcileSideEffects(t, env, disk)
	})
}

// TestSelectReadOnlySnapshot proves the default select is a read-only
// consumer: it never polls, never writes, and its behavior against the actual
// store is consistent for a missing state file (empty state, not an error) and
// a corrupt one (fatal, file untouched).
func TestSelectReadOnlySnapshot(t *testing.T) {
	t.Run("default select never polls and never writes", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		env.seedState(t, &[]quota.QuotaSnapshot{polledSnap("codex", env.now)}[0])
		before := env.stateBytes(t)

		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal")
		if code != cli.ExitOK {
			t.Fatalf("select: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "status: confirmed") || !strings.Contains(stdout, "headroom: 80%") {
			t.Fatalf("stdout=%q, want a confirmed selection over the saved snapshot", stdout)
		}
		if !strings.Contains(stdout, "as_of: 2026-09-18T12:00:00Z") {
			t.Fatalf("stdout=%q missing the snapshot's as-of clock sample", stdout)
		}
		if env.poller.calls != 0 {
			t.Fatalf("default select polled %d times; must read the saved snapshot only", env.poller.calls)
		}
		if !bytes.Equal(env.stateBytes(t), before) {
			t.Fatal("default select modified state.json; selection is a read-only consumer")
		}
		disk := env.loadDiskState(t)
		assertNoReconcileSideEffects(t, env, disk)
	})

	t.Run("missing state file is an empty snapshot, not an error", func(t *testing.T) {
		env := newSelectionTestEnv(t)

		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal")
		if code != cli.ExitPending {
			t.Fatalf("select with no state: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "status: uncertain") || !strings.Contains(stdout, "evidence: missing") {
			t.Fatalf("stdout=%q, want the missing-evidence uncertain result", stdout)
		}
		if env.poller.calls != 0 {
			t.Fatalf("default select polled %d times", env.poller.calls)
		}
		assertAbsent(t, env.statePath) // a read-only select never creates state
		assertAbsent(t, env.journalPath)
		assertAbsent(t, env.backupsRoot)
		assertAbsent(t, env.stagingRoot)
	})

	t.Run("corrupt state file is fatal and left untouched", func(t *testing.T) {
		env := newSelectionTestEnv(t)
		if err := os.WriteFile(env.statePath, []byte("{not-json"), 0o600); err != nil {
			t.Fatalf("write corrupt state: %v", err)
		}
		before := env.stateBytes(t)

		code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution", "--difficulty", "normal")
		if code != cli.ExitRejected {
			t.Fatalf("select with corrupt state: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout=%q, want empty on a fatal snapshot error", stdout)
		}
		if want := "selection: desired configuration or state is unreadable or invalid\n"; stderr != want {
			t.Fatalf("stderr=%q, want exactly %q (fixed safe sentence, no path or cause)", stderr, want)
		}
		if env.poller.calls != 0 {
			t.Fatalf("select with corrupt state polled %d times", env.poller.calls)
		}
		if !bytes.Equal(env.stateBytes(t), before) {
			t.Fatal("a failed select must not repair, replace, or delete the corrupt state file")
		}
	})
}

// TestSelectIntegrationSecretCanaries proves the assessment path stays
// synthetic end to end: the injected fake is the only assessor ever
// constructed (the automatic factory that resolves the real TYPESAFE_API_KEY
// is never wired), the fake receives the task locally, and neither the task
// text, the environment key, nor any upstream marker reaches the CLI output or
// the persisted state.
func TestSelectIntegrationSecretCanaries(t *testing.T) {
	env := newSelectionTestEnv(t)
	env.seedState(t, &[]quota.QuotaSnapshot{polledSnap("codex", env.now)}[0])

	// Even with a credential present in the environment, nothing in the
	// offline path may consult or emit it.
	t.Setenv("TYPESAFE_API_KEY", selectionCanaryKey)

	code, stdout, stderr := env.runSelect(t, "--policy", env.candidatePath, "--phase", "execution")
	if code != cli.ExitOK {
		t.Fatalf("select: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// The fake assessor was constructed exactly once, for the desired
	// configuration's model pin and timeout, and performed the only
	// "assessment" — locally, with no credential resolution and no network.
	if env.factoryCalls != 1 {
		t.Fatalf("assessor factory called %d times, want exactly 1", env.factoryCalls)
	}
	if env.factoryModel != policy.DocumentedJevModel {
		t.Fatalf("assessor model pin=%q, want the desired configuration pin %q", env.factoryModel, policy.DocumentedJevModel)
	}
	if env.factoryTimeout != policy.DefaultJevTimeout {
		t.Fatalf("assessor timeout=%s, want the desired configuration bound %s", env.factoryTimeout, policy.DefaultJevTimeout)
	}
	if env.assessor.calls != 1 || !env.assessor.lastEnabled {
		t.Fatalf("assessment calls=%d enabled=%t, want exactly one enabled request", env.assessor.calls, env.assessor.lastEnabled)
	}
	if env.assessor.lastPrompt != selectionCanaryPrompt {
		t.Fatalf("fake assessor prompt=%q, want the synthetic task", env.assessor.lastPrompt)
	}
	if !strings.Contains(stdout, "status: confirmed") || !strings.Contains(stdout, "assessed_tier: normal") {
		t.Fatalf("stdout=%q, want a confirmed selection over the assessed tier", stdout)
	}

	// Canary sweep: no prompt, key, or upstream marker in any artifact.
	artifacts := map[string]string{
		"stdout":     stdout,
		"stderr":     stderr,
		"state.json": string(env.stateBytes(t)),
	}
	for name, artifact := range artifacts {
		for _, canary := range []string{selectionCanaryPrompt, selectionCanaryKey, selectionCanaryUpstream} {
			if strings.Contains(artifact, canary) {
				t.Fatalf("%s leaked canary %q:\n%s", name, canary, artifact)
			}
		}
	}
}
