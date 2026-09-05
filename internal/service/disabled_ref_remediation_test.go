package service

// pq-m4k9 (B): when doctor fails because a definition references a disabled /
// unknown model (polytoken's subagent registry hard-fails on that), the pending
// remediation must name the offending file, not just send the operator around
// the keep-staging loop.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

func TestDoctorRemediationNamesStaleDisabledRef(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	writeFile(t, filepath.Join(globalDir, "config.yaml"),
		"models:\n  codex/gpt-5.6-sol:\n    enabled: true\n  codex/gpt-5.6-luna:\n    enabled: false\n"+
			"providers:\n  codex:\n    api_key: inert\n")
	// Managed field matches desired (sol); the body still references a model
	// the candidate disables — content the plan never touches.
	writeFile(t, filepath.Join(globalDir, "subagents", "software-architect.md"),
		"---\npolytoken:\n  model: codex/gpt-5.6-sol\n---\nFor quick turns use codex/gpt-5.6-luna.\n")

	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "lock", "apply.lock")
	journalPath := filepath.Join(root, "journal", "apply.json")
	stageTmp := filepath.Join(root, "stage")
	when := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	store := state.Store{Path: statePath, Now: func() time.Time { return when }, RecoveredRetention: 24 * time.Hour}

	desired := policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {Models: map[string]policy.ModelBaseline{
				"codex/gpt-5.6-sol":  {Enabled: true},
				"codex/gpt-5.6-luna": {Enabled: false, HadEnabledKey: true},
			}},
		},
		Global: policy.Target{
			ID:     "global",
			Root:   globalDir,
			Global: true,
			Definitions: []policy.Definition{{
				Path:  filepath.Join("subagents", "software-architect.md"),
				Chain: policy.Chain{"codex/gpt-5.6-sol(high)"},
			}},
		},
	}

	coord := &Coordinator{
		Lock:         publish.NewFileLock(lockPath),
		Policy:       fixedPolicyLoader{desired: desired},
		PolicyWriter: nilPolicyWriter{},
		State:        StoreState{Store: store},
		Targets:      NewTargetRegistry(),
		Builder:      NewReconciler(),
		Stage: StagingStager{Builder: staging.Builder{
			TempRoot: stageTmp,
			AuthMode: staging.AuthInert,
			Sources:  staging.FSMaterializer{GlobalDir: globalDir},
		}},
		Validate: ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: projectDoctorFailRunner{}}},
		Publish: PublisherAdapter{Publisher: publish.Publisher{
			Locker:      publish.NewFileLock(lockPath),
			State:       store,
			JournalPath: journalPath,
			Backups:     publish.BackupStore{Root: filepath.Join(root, "backups"), Limit: 3},
			ManagedRoot: globalDir,
			Clock:       func() time.Time { return when },
		}},
		Clock: fixedClock{t: when},
	}

	out := coord.Reconcile(context.Background(), true /*dry-run*/, false /*keep-staging*/, false)
	if len(out.Targets) != 1 || out.Targets[0].Pending == nil {
		t.Fatalf("expected one pending target: %+v", out.Targets)
	}
	rem := out.Targets[0].Pending.Remediation
	if !strings.Contains(rem, "software-architect.md") || !strings.Contains(rem, "codex/gpt-5.6-luna") {
		t.Fatalf("remediation does not name the stale file+model: %q", rem)
	}
}
