package service

// Regression test: a successful reconcile must NOT publish the inert secret
// placeholder into the live config. Before the fix, the coordinator read staged
// candidate files from ConfigDir (which carries "inert-validation-placeholder"
// under AuthInert) as the source for publication. The publisher then renamed
// those inert-redacted files over the live config, clobbering real auth values
// like "${ZAI_API_KEY}".

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/hook"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// TestPublishPreservesRealSecrets verifies that after a successful reconcile,
// the live config.yaml retains real auth values rather than being clobbered
// with "inert-validation-placeholder".
func TestPublishPreservesRealSecrets(t *testing.T) {
	root := t.TempDir()

	// Source layer with real auth values (env-var references are typical in
	// production).
	sourceDir := filepath.Join(root, "source")
	writeFile(t, filepath.Join(sourceDir, "config.yaml"),
		"models:\n"+
			"  codex/gpt:\n    enabled: true\n"+
			"  zai/glm:\n    enabled: true\n"+
			"providers:\n"+
			"  codex:\n    auth:\n      key: \"${CODEX_API_KEY}\"\n"+
			"  zai:\n    auth:\n      key: \"${ZAI_API_KEY}\"\n"+
			"defaults:\n  full: codex/gpt\n")

	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "lock", "apply.lock")
	journalPath := filepath.Join(root, "journal", "apply.json")
	backupRoot := filepath.Join(root, "backups")
	stageTmp := filepath.Join(root, "stage")

	clock := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }
	store := state.Store{Path: statePath, Now: clock, RecoveredRetention: 24 * time.Hour}

	prior := state.State{
		Schema:    1,
		Revision:  1,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

	pub := publish.Publisher{
		Locker:      publish.NewFileLock(lockPath),
		State:       store,
		JournalPath: journalPath,
		Backups:     publish.BackupStore{Root: backupRoot, Limit: 3},
		ManagedRoot: sourceDir,
		Clock:       clock,
	}
	builder := staging.Builder{
		TempRoot: stageTmp,
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: sourceDir},
	}
	runner := validate.Runner{Binary: "polytoken", Commands: fakeCommandRunner{}}

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				CodexBarProviders:  []string{"codex"},
				PolytokenProviders: []string{"codex"},
				Models:             map[string]policy.ModelBaseline{"codex/gpt": {Enabled: true}},
			},
			"zai": {
				CodexBarProviders:  []string{"zai"},
				PolytokenProviders: []string{"zai"},
				Models:             map[string]policy.ModelBaseline{"zai/glm": {Enabled: true}},
			},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   sourceDir,
			Global: true,
		},
	}

	coord := &Coordinator{
		Lock:         publish.NewFileLock(lockPath),
		Policy:       fixedPolicyLoader{desired: desired},
		PolicyWriter: nilPolicyWriter{},
		State:        StoreState{Store: store},
		Targets:      NewTargetRegistry(),
		Builder:      NewReconciler(),
		Stage:        StagingStager{Builder: builder},
		Validate:     ValidateRunner{Runner: runner},
		Publish:      PublisherAdapter{Publisher: pub},
		Clock:        fixedClock{t: clock()},
	}

	// Drive a hook event that triggers a full reconcile+publish.
	ev := hook.Event{
		Type:      hook.QuotaReached,
		Provider:  "codex",
		Timestamp: clock().Add(time.Second),
	}
	out := coord.HandleEvent(context.Background(), ev)
	if !out.Accepted {
		t.Fatalf("event not accepted: %+v err=%v", out, out.Error)
	}
	if out.PendingCount() != 0 {
		for _, tgt := range out.Targets {
			if tgt.Pending != nil {
				t.Logf("pending: %+v", *tgt.Pending)
			}
		}
		t.Fatalf("pending targets after publish")
	}

	// The live config must retain the real auth values.
	liveConfig, err := os.ReadFile(filepath.Join(sourceDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read live config: %v", err)
	}
	live := string(liveConfig)

	// Must NOT contain the inert placeholder.
	if strings.Contains(live, inertPlaceholder) {
		t.Fatalf("live config was clobbered with inert placeholder:\n%s", live)
	}
	// Must retain the real auth values.
	if !strings.Contains(live, "${CODEX_API_KEY}") {
		t.Fatalf("live config lost CODEX_API_KEY:\n%s", live)
	}
	if !strings.Contains(live, "${ZAI_API_KEY}") {
		t.Fatalf("live config lost ZAI_API_KEY:\n%s", live)
	}
}

// inertPlaceholder matches the constant in staging/builder.go.
const inertPlaceholder = "inert-validation-placeholder"
