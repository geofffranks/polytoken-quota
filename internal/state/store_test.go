package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveredRetentionAgesByClock(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	retention := 24 * time.Hour
	s := seedState()
	s.Recovered = []ApplyFailure{
		{TargetID: "within", ResolvedAt: now.Add(-retention + time.Second)},
		{TargetID: "expired", ResolvedAt: now.Add(-retention)},
	}
	got := PruneRecovered(s, now, retention)
	if len(got.Recovered) != 1 || got.Recovered[0].TargetID != "within" {
		t.Fatalf("recovered=%+v", got)
	}
}

func TestStoreDoesNotPersistSecrets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}
	if err := st.Save(seedState()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, forbidden := range []string{"account", "api_key", "auth"} {
		if bytes.Contains(bytes.ToLower(b), []byte(forbidden)) {
			t.Fatalf("persisted %s", forbidden)
		}
	}
}

func TestStoreRoundTripPreservesAllFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}

	original := State{
		Schema:   1,
		Revision: 42,
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaExhausted, Availability: Available, QuotaAt: now, QuotaArrival: 7},
			"zai":   {Quota: QuotaNormal, Availability: Unavailable, AvailabilityAt: now, AvailabilityArrival: 9},
		},
		Targets: map[string]TargetState{
			"global": {
				AttemptedRevision: 42,
				AppliedRevision:   41,
				AttemptedAt:       now,
				AppliedAt:         now.Add(-time.Hour),
				Pending: &ApplyFailure{
					TargetID:               "global",
					Stage:                  "doctor",
					File:                   "~/.config/polytoken/config.yaml",
					Chain:                  "defaults.full",
					Summary:                "zai/glm-5.2 disabled but referenced",
					Remediation:            "remove from fallback_models",
					LastSuccessfulRevision: 41,
					AttemptedRevision:      42,
					LastSuccessfulAt:       now.Add(-time.Hour),
					AttemptedAt:            now,
					ResolvedAt:             time.Time{},
					Reproduces:             true,
					LiveStatus:             "drifted",
				},
			},
		},
		RefreshFailed: []Diagnostic{{Code: "refresh_failed", Provider: "codex", Summary: "rate limited", At: now}},
		Recovered:     []ApplyFailure{{TargetID: "old", Stage: "publish", ResolvedAt: now.Add(-time.Hour)}},
	}
	if err := st.Save(original); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Schema != 1 || loaded.Revision != 42 {
		t.Fatalf("schema=%d revision=%d", loaded.Schema, loaded.Revision)
	}
	codex := loaded.Providers["codex"]
	if codex.Quota != QuotaExhausted || !codex.QuotaAt.Equal(now) || codex.QuotaArrival != 7 {
		t.Fatalf("codex=%+v", codex)
	}
	if EffectiveMode(codex) != ModeDisabled {
		t.Fatalf("codex mode=%s", EffectiveMode(codex))
	}
	if EffectiveMode(loaded.Providers["zai"]) != ModeDisabled {
		t.Fatalf("zai mode=%s", EffectiveMode(loaded.Providers["zai"]))
	}
	g := loaded.Targets["global"]
	if g.AttemptedRevision != 42 || g.AppliedRevision != 41 {
		t.Fatalf("target revisions=%+v", g)
	}
	if !g.AttemptedAt.Equal(now) || !g.AppliedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("target timestamps=%+v", g)
	}
	if g.Pending == nil {
		t.Fatal("pending nil")
	}
	pf := g.Pending
	if pf.TargetID != "global" || pf.Stage != "doctor" || pf.Chain != "defaults.full" || pf.Summary == "" || pf.Remediation == "" {
		t.Fatalf("pending=%+v", pf)
	}
	if pf.LastSuccessfulRevision != 41 || pf.AttemptedRevision != 42 {
		t.Fatalf("pending revisions=%+v", pf)
	}
	if !pf.LastSuccessfulAt.Equal(now.Add(-time.Hour)) || !pf.AttemptedAt.Equal(now) {
		t.Fatalf("pending timestamps=%+v", pf)
	}
	if !pf.ResolvedAt.IsZero() || !pf.Reproduces || pf.LiveStatus != "drifted" {
		t.Fatalf("pending resolved/repro/status=%+v", pf)
	}
	if len(loaded.Recovered) != 1 || loaded.Recovered[0].TargetID != "old" || !loaded.Recovered[0].ResolvedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("recovered=%+v", loaded.Recovered)
	}
	if len(loaded.RefreshFailed) != 1 || loaded.RefreshFailed[0].Provider != "codex" || loaded.RefreshFailed[0].Summary != "rate limited" {
		t.Fatalf("refreshfailed=%+v", loaded.RefreshFailed)
	}
}

func TestStoreSaveAtomicAndMode0600(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}
	if err := st.Save(seedState()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode() & 0o777; mode != 0o600 {
		t.Fatalf("mode=%o want 0600", mode)
	}
	// Atomic write leaves no temporary files behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entries=%v want only state.json", names)
	}
}

func TestStoreLoadMissingReturnsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: time.Now, RecoveredRetention: 24 * time.Hour}
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Providers == nil || s.Targets == nil {
		t.Fatalf("nil maps: %+v", s)
	}
	if len(s.Providers) != 0 || s.Revision != 0 {
		t.Fatalf("non-empty state: %+v", s)
	}
}

func TestStoreSavePrunesRecovered(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}
	s := seedState()
	s.Recovered = []ApplyFailure{
		{TargetID: "fresh", ResolvedAt: now.Add(-time.Hour)},
		{TargetID: "stale", ResolvedAt: now.Add(-48 * time.Hour)},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Recovered) != 1 || loaded.Recovered[0].TargetID != "fresh" {
		t.Fatalf("recovered=%+v", loaded.Recovered)
	}
}
