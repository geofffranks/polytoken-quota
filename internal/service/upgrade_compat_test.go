package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func TestUpgradeCompat(t *testing.T) {
	t.Parallel()

	legacy := "version: 1\n" + "codexbar_" + "providers: [codex]\n" + "polytoken_" + "providers: [codex]\n" + `providers:
  codex:
    models: [codex/gpt]
    quota:
      adapter: codex
      freshness_ttl: 30m
routing:
  enabled: true
global:
  id: global
  root: /tmp/upgrade-compat-global
  full: [codex/gpt]
`
	clean := `version: 1
providers:
  codex:
    models: [codex/gpt]
    quota:
      adapter: codex
      freshness_ttl: 30m
routing:
  enabled: true
global:
  id: global
  root: /tmp/upgrade-compat-global
  full: [codex/gpt]
`

	legacyDesired := loadUpgradePolicy(t, legacy)
	cleanDesired := loadUpgradePolicy(t, clean)
	if !reflect.DeepEqual(legacyDesired, cleanDesired) {
		t.Fatalf("legacy provider-list keys changed loaded policy:\nlegacy=%+v\nclean=%+v", legacyDesired, cleanDesired)
	}
	if _, ok := legacyDesired.Providers[policy.MappingID("codex")]; !ok {
		t.Fatalf("loaded policy lost mapping ID codex: %+v", legacyDesired.Providers)
	}

	prior := state.State{
		Schema:   state.CurrentSchema,
		Revision: 1,
		Providers: map[string]state.ProviderState{
			"codex": {QuotaSnapshot: upgradeSnapshot("codex")},
		},
		Targets: map[string]state.TargetState{},
	}

	legacySpy := newUpgradeCoordinator(t, legacyDesired, prior)
	cleanSpy := newUpgradeCoordinator(t, cleanDesired, prior)

	legacyOut := legacySpy.Coordinator.QuotaCheck(context.Background(), "", true)
	cleanOut := cleanSpy.Coordinator.QuotaCheck(context.Background(), "", true)
	if legacyOut.Error != nil || cleanOut.Error != nil {
		t.Fatalf("quota check/reconcile errors: legacy=%v clean=%v", legacyOut.Error, cleanOut.Error)
	}
	if !legacyOut.Accepted || !cleanOut.Accepted || legacyOut.PendingCount() != 0 || cleanOut.PendingCount() != 0 {
		t.Fatalf("quota check/reconcile outcomes: legacy=%+v clean=%+v", legacyOut, cleanOut)
	}
	if len(legacyOut.Targets) != 1 || legacyOut.Targets[0].TargetID != "global" {
		t.Fatalf("legacy reconcile targets=%+v", legacyOut.Targets)
	}
	if !reflect.DeepEqual(legacyOut.Targets, cleanOut.Targets) {
		t.Fatalf("legacy reconcile differs from clean baseline:\nlegacy=%+v\nclean=%+v", legacyOut.Targets, cleanOut.Targets)
	}

	for name, spy := range map[string]*coordinatorSpy{"legacy": legacySpy, "clean": cleanSpy} {
		if _, ok := spy.LastSaved.Providers["codex"]; !ok {
			t.Fatalf("%s state is not keyed by mapping ID codex: %+v", name, spy.LastSaved.Providers)
		}
		if _, ok := spy.LastSaved.Providers["codex-mapping"]; ok {
			t.Fatalf("%s state unexpectedly used provider-list key codex-mapping: %+v", name, spy.LastSaved.Providers)
		}
		if spy.LastSaved.Providers["codex"].QuotaSnapshot == nil {
			t.Fatalf("%s state lost quota snapshot: %+v", name, spy.LastSaved.Providers["codex"])
		}
	}

	legacyRanks, legacyRanking := ComputeRanking(legacyDesired, legacySpy.LastSaved, upgradeNow)
	cleanRanks, cleanRanking := ComputeRanking(cleanDesired, cleanSpy.LastSaved, upgradeNow)
	if !reflect.DeepEqual(legacyRanking, cleanRanking) || !reflect.DeepEqual(legacyRanks, cleanRanks) || legacyRanks[policy.MappingID("codex")] != 0 {
		t.Fatalf("ranking differs from clean baseline: legacy=%v/%v clean=%v/%v", legacyRanking, legacyRanks, cleanRanking, cleanRanks)
	}

	t.Run("schema4_hook_history_is_pruned", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		fixture := map[string]any{
			"Schema":    4,
			"Revision":  7,
			"Providers": map[string]any{"codex": map[string]any{}},
			"Targets":   map[string]any{},
			"ReconcileHistory": map[string]any{
				"Records": []map[string]any{
					{"Revision": 8, "Trigger": map[string]any{"Kind": "hook"}},
				},
			},
		}
		data, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := (state.Store{Path: path}).Load()
		if err != nil {
			t.Fatalf("schema-4 state with hook history failed to load: %v", err)
		}
		if loaded.Schema != state.CurrentSchema || loaded.ReconcileHistory.OmittedHistoryRecords != 1 {
			t.Fatalf("schema/history migration=%+v", loaded)
		}
		if len(loaded.ReconcileHistory.Records) != 0 {
			t.Fatalf("schema-4 history after pruning=%+v", loaded.ReconcileHistory.Records)
		}
	})
}

var upgradeNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func loadUpgradePolicy(t *testing.T, raw string) policy.Desired {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	desired, err := policy.Load(path)
	if err != nil {
		t.Fatalf("policy.Load: %v", err)
	}
	return desired
}

func upgradeSnapshot(mappingID string) *quota.QuotaSnapshot {
	used, limit := 20.0, 100.0
	return &quota.QuotaSnapshot{
		MappingID:    mappingID,
		CheckedAt:    time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows:      []quota.QuotaWindow{{Name: "daily", Used: &used, Limit: &limit}},
	}
}

func newUpgradeCoordinator(t *testing.T, desired policy.Desired, prior state.State) *coordinatorSpy {
	t.Helper()
	spy := newQuotaCheckSpy().withTargets("global", validTargetKey)
	spy.Coordinator.Policy = fixedUpgradePolicy{desired: desired}
	pollerOf(spy).results["codex"] = *upgradeSnapshot("codex")
	spy.Coordinator.State = seededStateStore{state: prior, spy: spy}
	spy.Coordinator.Publish = seededPublisher{state: prior}
	return spy
}

type fixedUpgradePolicy struct{ desired policy.Desired }

func (p fixedUpgradePolicy) LoadPolicy() (policy.Desired, error) { return p.desired, nil }
func (fixedUpgradePolicy) DesiredExists() bool                   { return true }
