package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// providerOnlyDesired builds the in-memory provider-only policy used by the
// command-compatibility matrix: two enrolled Polytoken provider IDs (one with
// explicit quota adapter configuration), a global target, and no models,
// chains, definitions, or projects.
func providerOnlyDesired() policy.Desired {
	return policy.Desired{
		Version: 1,
		Mode:    policy.ModeProviderOnly,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: 30 * time.Minute, BalanceGroup: "default", Weight: 1}},
			"zai":   {},
		},
		Global: policy.Target{ID: "global", Root: "/global", Global: true},
	}
}

func withProviderOnlyPolicy(spy *coordinatorSpy) *coordinatorSpy {
	spy.desired = providerOnlyDesired()
	return spy
}

// TestProviderOnlyCommandCompatibilityMatrix pins the per-command
// provider-only contract: chain-dependent selection/projection returns a clear
// unsupported result wrapping ErrProviderOnlyUnsupported (never a silent
// no-op), while provider-level functions — including the reconcile gate and
// check --reconcile — keep working.
func TestProviderOnlyCommandCompatibilityMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("reconcile apply runs the provider gate", func(t *testing.T) {
		out := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Reconcile(ctx, false, false, false)
		if !out.Accepted {
			t.Fatalf("reconcile rejected under a provider-only policy: %v", out.Error)
		}
		if errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("reconcile is unsupported: %v", out.Error)
		}
		// The spy registers no global target, so the gate records a clear
		// pending reason instead of a silent no-op.
		if out.PendingCount() != 1 || out.Targets[0].Pending == nil ||
			!strings.Contains(out.Targets[0].Pending.Summary, "registered global target") {
			t.Fatalf("out=%+v want one pending naming the missing registered global target", out)
		}
	})

	t.Run("reconcile dry-run runs the provider gate", func(t *testing.T) {
		out := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Reconcile(ctx, true, false, false)
		if !out.Accepted || errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("out=%+v err=%v want accepted gate evaluation", out, out.Error)
		}
		if out.PendingCount() != 1 {
			t.Fatalf("out=%+v want the gate refusal surfaced as pending", out)
		}
	})

	t.Run("check with reconcile runs the provider gate", func(t *testing.T) {
		spy := withProviderOnlyPolicy(newCoordinatorSpy())
		spy.Coordinator.QuotaPoller = &fakePoller{results: map[string]quota.QuotaSnapshot{
			"codex": {MappingID: "codex", Status: quota.SourceFresh, CheckedAt: spy.Coordinator.now()},
		}}
		out := spy.Coordinator.QuotaCheck(ctx, "", true)
		if !out.Accepted || errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("out=%+v err=%v want accepted poll+gate check", out, out.Error)
		}
		if out.PendingCount() != 1 {
			t.Fatalf("out=%+v want the gate refusal surfaced as pending", out)
		}
	})

	t.Run("check poll accepted", func(t *testing.T) {
		spy := withProviderOnlyPolicy(newCoordinatorSpy())
		spy.Coordinator.QuotaPoller = &fakePoller{results: map[string]quota.QuotaSnapshot{
			"codex": {MappingID: "codex", Status: quota.SourceFresh, CheckedAt: spy.Coordinator.now()},
		}}
		out := spy.Coordinator.QuotaCheck(ctx, "", false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("out=%+v want accepted poll-only check", out)
		}
	})

	t.Run("routing disable uses the provider toggle path", func(t *testing.T) {
		out := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Disable(ctx, "codex")
		if out.Accepted || errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("out=%+v want the provider toggle path, not the unsupported refusal", out)
		}
		// The spy registers no global target, so the toggle records a clear
		// refusal instead of a silent no-op.
		if !strings.Contains(out.Error.Error(), "registered global target") {
			t.Fatalf("err=%v want the provider toggle target refusal", out.Error)
		}
	})

	t.Run("routing enable uses the provider toggle path", func(t *testing.T) {
		out := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Enable(ctx, "codex")
		if out.Accepted || errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("out=%+v want the provider toggle path, not the unsupported refusal", out)
		}
		if !strings.Contains(out.Error.Error(), "registered global target") {
			t.Fatalf("err=%v want the provider toggle target refusal", out.Error)
		}
	})

	t.Run("routing reset rejected", func(t *testing.T) {
		out := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Reset(ctx)
		if out.Accepted || !errors.Is(out.Error, ErrProviderOnlyUnsupported) {
			t.Fatalf("out=%+v want unsupported", out)
		}
	})

	t.Run("selection rejected", func(t *testing.T) {
		_, err := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.SelectionSnapshot(ctx)
		if err == nil || !errors.Is(err, ErrProviderOnlyUnsupported) {
			t.Fatalf("err=%v want ErrProviderOnlyUnsupported", err)
		}
	})

	t.Run("status maintained with provider-only flag", func(t *testing.T) {
		report := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Status(ctx, false)
		if report.Error != "" {
			t.Fatalf("status error=%q", report.Error)
		}
		if !report.ProviderOnly {
			t.Fatal("status report does not carry provider_only=true")
		}
		got := map[string]bool{}
		for _, p := range report.Providers {
			got[p.Provider] = true
		}
		if !got["codex"] || !got["zai"] {
			t.Fatalf("providers=%v want enrolled codex and zai", got)
		}
		if len(report.Routes) != 0 {
			t.Fatalf("routes=%d want empty (no chains in provider-only mode)", len(report.Routes))
		}
	})

	t.Run("doctor maintained", func(t *testing.T) {
		report := withProviderOnlyPolicy(newCoordinatorSpy()).Coordinator.Doctor(ctx, false)
		for _, f := range report.Findings {
			if f.Code == "policy-schema" {
				t.Fatalf("doctor flagged a valid provider-only policy: %+v", f)
			}
		}
	})
}

// legacyMigrationFixture is a legacy desired.yaml carrying exactly the fields
// quota authored in legacy mode: defaults/classifier chains, a definition
// chain, enumerated models, a registered project, and operator-authored
// operational settings a migration must preserve. %GLOBAL% and %PROJ% are
// replaced with real fixture directories.
const legacyMigrationFixture = `version: 1
providers:
  codex:
    models:
      - codex/gpt-5.6-sol
global:
  root: %GLOBAL%
  full: [codex/gpt-5.6-sol]
  definitions:
    - path: facets/reader.md
      chain: [codex/gpt-5.6-sol]
projects:
  - id: proj
    root: %PROJ%
    mini: [codex/gpt-5.6-sol]
operational:
  validation_timeout: 45s
  notice_path: /tmp/pq-notice.json
`

// newMigrationFixture wires a fully real Coordinator over temp directories:
// a legacy desired.yaml, a global Polytoken config dir with two providers, a
// real state store, journal, and backup root. It returns the coordinator, the
// desired path, the global dir, the registered project dir, and the exact
// legacy bytes.
func newMigrationFixture(t *testing.T) (*Coordinator, string, string, string, []byte) {
	t.Helper()
	home := t.TempDir()
	globalDir := filepath.Join(home, "polytoken-config")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "providers:\n  codex:\n    base_url: http://127.0.0.1:9\n  team-llm:\n    base_url: http://127.0.0.1:9\n"
	if err := os.WriteFile(filepath.Join(globalDir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	desiredPath := filepath.Join(home, "desired.yaml")
	legacy := strings.ReplaceAll(legacyMigrationFixture, "%GLOBAL%", globalDir)
	projectDir := filepath.Join(home, "proj")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"), []byte("providers:\n  proj-llm:\n    base_url: http://127.0.0.1:9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy = strings.ReplaceAll(legacy, "%PROJ%", projectDir)
	if err := os.WriteFile(desiredPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Path: filepath.Join(home, "state.json"), RecoveredRetention: 24 * time.Hour}
	journalPath := filepath.Join(home, "journal", "apply.json")
	coord := &Coordinator{
		Lock:         realFlock{path: filepath.Join(home, "lock", "apply.lock")},
		Policy:       FilePolicyLoader{Path: desiredPath},
		PolicyWriter: policy.NewWriter(desiredPath),
		State:        StoreState{Store: store},
		Publish: PublisherAdapter{Publisher: publish.Publisher{
			Locker:      realFlock{path: filepath.Join(home, "lock", "apply.lock")},
			State:       store,
			JournalPath: journalPath,
			Backups:     publish.BackupStore{Root: filepath.Join(home, "backups"), Limit: 2},
			ManagedRoot: globalDir,
		}},
		Sources:     policy.FilesystemSourceReader{GlobalDir: globalDir, DesiredPath: desiredPath},
		JournalPath: journalPath,
		BackupsPath: filepath.Join(home, "backups"),
	}
	return coord, desiredPath, globalDir, projectDir, []byte(legacy)
}

// TestProviderOnlyMigrationPreviewAndLegacyBytePreservation proves the
// migration preview is read-only and complete: it enrolls exactly the live
// provider IDs, enumerates the legacy quota-authored edits that persist as
// operator-owned (defaults, definition chains, models), surfaces the
// backup/journal/policy-backup references, and leaves every byte on disk
// untouched. It then proves the real migration preserves the Polytoken
// configuration bytes and the replaced policy file.
func TestProviderOnlyMigrationPreviewAndLegacyBytePreservation(t *testing.T) {
	ctx := context.Background()
	coord, desiredPath, globalDir, projectDir, legacyBytes := newMigrationFixture(t)

	configBefore, err := os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	p, err := coord.PreviewProviderOnlyMigration(ctx)
	if err != nil {
		t.Fatalf("PreviewProviderOnlyMigration: %v", err)
	}
	if len(p.EnrolledProviders) != 2 || p.EnrolledProviders[0] != "codex" || p.EnrolledProviders[1] != "team-llm" {
		t.Fatalf("enrolled=%v want codex and team-llm", p.EnrolledProviders)
	}
	if p.GlobalRoot != globalDir {
		t.Fatalf("global root=%q want %q", p.GlobalRoot, globalDir)
	}
	classes := map[string]bool{}
	for _, e := range p.LegacyOwnedEdits {
		classes[e.Field] = true
	}
	for _, want := range []string{"defaults.full", "definition chain", "models"} {
		if !classes[want] {
			t.Fatalf("preview missing legacy owned edit class %q; got %+v", want, p.LegacyOwnedEdits)
		}
	}
	if p.BackupsPath == "" || p.JournalPath == "" || p.PolicyBackupPath != desiredPath+policy.PolicyBackupSuffix {
		t.Fatalf("preview references=%+v", p)
	}
	if len(p.Rollback) == 0 {
		t.Fatal("preview carries no rollback guidance")
	}

	// The preview is read-only: neither the Polytoken config nor the policy
	// changed.
	configAfter, err := os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Fatal("preview mutated the Polytoken config")
	}
	if got, err := os.ReadFile(desiredPath); err != nil || string(got) != string(legacyBytes) {
		t.Fatal("preview mutated desired.yaml")
	}

	// The real migration replaces only the policy file, preserves the operator
	// operational section, keeps the legacy policy bytes, and surfaces the
	// preview on the outcome.
	out := coord.InitWithOptions(ctx, InitOptions{Force: true, ProviderOnly: true})
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v want accepted migration", out)
	}
	if out.Migration == nil || len(out.Migration.LegacyOwnedEdits) == 0 {
		t.Fatalf("migration outcome carries no preview: %+v", out.Migration)
	}
	loaded, err := policy.Load(desiredPath)
	if err != nil {
		t.Fatalf("reloaded migrated policy: %v", err)
	}
	if !loaded.ProviderOnly() {
		t.Fatalf("migrated policy mode=%q", loaded.Mode)
	}
	if len(loaded.Providers) != 2 || loaded.Global.Root != globalDir {
		t.Fatalf("migrated policy=%+v", loaded)
	}
	// The migration preserves the explicitly registered project root as an
	// id/root-only target — never chains, definitions, or unregistered roots.
	if len(loaded.Projects) != 1 {
		t.Fatalf("migrated projects=%+v want the registered root preserved", loaded.Projects)
	}
	reg := loaded.Projects[0]
	if reg.ID != "proj" || reg.Root != projectDir || reg.Global ||
		len(reg.Definitions) != 0 || len(reg.Full) != 0 || len(reg.Mini) != 0 ||
		len(reg.Nano) != 0 || len(reg.Classifier) != 0 {
		t.Fatalf("migrated project=%+v want id/root-only registration at %s", reg, projectDir)
	}
	if loaded.Operational.NoticePath != "/tmp/pq-notice.json" {
		t.Fatalf("operational section not preserved: %+v", loaded.Operational)
	}
	backup, err := os.ReadFile(desiredPath + policy.PolicyBackupSuffix)
	if err != nil {
		t.Fatalf("preserved policy: %v", err)
	}
	if string(backup) != string(legacyBytes) {
		t.Fatal("preserved policy bytes differ from the replaced legacy policy")
	}
	configAfter, err = os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Fatal("migration mutated the Polytoken config")
	}
}

// TestLegacyForceInitOverProviderOnlyRejected proves a plain legacy forced
// init can never silently downgrade a provider-only installation: replacing
// provider-only mode requires --provider-only.
func TestLegacyForceInitOverProviderOnlyRejected(t *testing.T) {
	home := t.TempDir()
	desiredPath := filepath.Join(home, "desired.yaml")
	doc := `version: 1
mode: provider-only
providers:
  codex: {}
global:
  root: /global
`
	if err := os.WriteFile(desiredPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := newCoordinatorSpy()
	spy.Coordinator.Policy = FilePolicyLoader{Path: desiredPath}
	spy.Coordinator.PolicyWriter = policy.NewWriter(desiredPath)
	spy.Coordinator.Sources = testSourceReader{}
	out := spy.Coordinator.InitWithOptions(context.Background(), InitOptions{Force: true})
	if out.Accepted {
		t.Fatal("legacy forced init replaced a provider-only policy")
	}
	if !strings.Contains(out.Error.Error(), "rerun with --provider-only") {
		t.Fatalf("err=%v want the provider-only replacement guidance", out.Error)
	}
	// The policy file is untouched after the rejection.
	if got, err := os.ReadFile(desiredPath); err != nil || string(got) != doc {
		t.Fatal("rejected init mutated desired.yaml")
	}
}
