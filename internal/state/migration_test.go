package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadRaw unmarshals the persisted file into a State, bypassing the store, so a
// test can inspect the exact on-disk schema and content.
func loadRaw(t *testing.T, path string) State {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return s
}

func TestLoadMigratesV1ToV2(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	// A v1-shaped document: Schema 1, providers/targets/revision preserved.
	v1 := `{
		"Schema": 1,
		"Revision": 42,
		"Providers": {
			"codex": {"Quota": "exhausted", "Availability": "available", "QuotaArrival": 7}
		},
		"Targets": {
			"global": {"AttemptedRevision": 42, "AppliedRevision": 41}
		},
		"RefreshFailed": [{"Code": "refresh_failed", "Provider": "codex", "Summary": "rate limited"}],
		"Recovered": []
	}`
	writeFile(t, p, v1)

	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}
	s, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Schema != CurrentSchema {
		t.Fatalf("schema=%d want %d", s.Schema, CurrentSchema)
	}
	if s.Revision != 42 {
		t.Fatalf("revision=%d want 42", s.Revision)
	}
	codex := s.Providers["codex"]
	if codex.Quota != QuotaExhausted || codex.QuotaArrival != 7 {
		t.Fatalf("codex not preserved: %+v", codex)
	}
	if codex.QuotaSnapshot != nil || codex.QuotaAttempt != nil {
		t.Fatalf("quota fields should be nil after v1 migration: %+v", codex)
	}
	if codex.Routing.LastRank != 0 || !codex.Routing.LastDecisionAt.IsZero() {
		t.Fatalf("routing should be zero after v1 migration: %+v", codex.Routing)
	}
	if s.Providers == nil || s.Targets == nil {
		t.Fatalf("maps should be non-nil: %+v", s)
	}
	if s.RoutingHistory != nil || s.UsageHistory != nil {
		t.Fatalf("history should be nil after v1 migration: %+v", s)
	}
	if len(s.RefreshFailed) != 1 || s.RefreshFailed[0].Provider != "codex" {
		t.Fatalf("refreshfailed not preserved: %+v", s.RefreshFailed)
	}
}

func TestLoadMigratesZeroSchemaToV2(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	v0 := `{"Schema": 0, "Providers": {}, "Targets": {}}`
	writeFile(t, p, v0)
	st := Store{Path: p, Now: time.Now, RecoveredRetention: 24 * time.Hour}
	s, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Schema != CurrentSchema {
		t.Fatalf("schema=%d want %d", s.Schema, CurrentSchema)
	}
}

func TestLoadMissingReturnsFreshV2(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: time.Now, RecoveredRetention: 24 * time.Hour}
	s, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Schema != CurrentSchema {
		t.Fatalf("schema=%d want %d (fresh state)", s.Schema, CurrentSchema)
	}
	if s.Providers == nil || s.Targets == nil {
		t.Fatalf("nil maps: %+v", s)
	}
}

func TestLoadRejectsFutureSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for _, schema := range []int{3, 99} {
		writeFile(t, p, `{"Schema": `+strconv.Itoa(schema)+`, "Providers": {}, "Targets": {}}`)
		st := Store{Path: p, Now: time.Now, RecoveredRetention: 24 * time.Hour}
		if _, err := st.Load(); err == nil {
			t.Fatalf("schema %d: expected error, got nil", schema)
		}
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	writeFile(t, p, "{not valid json")
	st := Store{Path: p, Now: time.Now, RecoveredRetention: 24 * time.Hour}
	if _, err := st.Load(); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestRoundTripV2PreservesSnapshotsHistoryRouting(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}

	used := 8.0
	limit := 10.0
	reset := now.Add(2 * time.Hour)
	snap := &quota.QuotaSnapshot{
		MappingID:    "codex",
		CheckedAt:    now,
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows: []quota.QuotaWindow{{
			Name:    "primary",
			Used:    &used,
			Limit:   &limit,
			ResetAt: &reset,
		}},
	}
	original := State{
		Schema: CurrentSchema,
		Providers: map[string]ProviderState{
			"codex": {
				Quota:         QuotaLow,
				Availability:  Available,
				QuotaSnapshot: snap,
				QuotaAttempt:  snap,
				Routing:       ProviderRouting{LastRank: 1, LastDecisionAt: now, LastAppliedRevision: 9},
			},
		},
		Targets: map[string]TargetState{},
		RoutingHistory: &RoutingHistory{
			LastGoodGlobalRank: []string{"codex", "zai"},
			ComputedAt:         now,
		},
		UsageHistory: &UsageHistory{Weeks: []UsageSample{{
			WeekStart:   now,
			Totals:      map[string]float64{"codex": 0.8},
			SampleCount: 3,
		}}},
	}
	if err := st.Save(original); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Schema != CurrentSchema {
		t.Fatalf("schema=%d want %d", loaded.Schema, CurrentSchema)
	}
	codex := loaded.Providers["codex"]
	if codex.QuotaSnapshot == nil || codex.QuotaSnapshot.MappingID != "codex" {
		t.Fatalf("snapshot not preserved: %+v", codex.QuotaSnapshot)
	}
	if got := codex.QuotaSnapshot.EffectiveRemaining(); got == nil {
		t.Fatal("effective remaining nil after round trip")
	}
	if codex.Routing.LastRank != 1 || codex.Routing.LastAppliedRevision != 9 {
		t.Fatalf("routing not preserved: %+v", codex.Routing)
	}
	if loaded.RoutingHistory == nil || len(loaded.RoutingHistory.LastGoodGlobalRank) != 2 {
		t.Fatalf("routing history not preserved: %+v", loaded.RoutingHistory)
	}
	if loaded.UsageHistory == nil || len(loaded.UsageHistory.Weeks) != 1 {
		t.Fatalf("usage history not preserved: %+v", loaded.UsageHistory)
	}
	if loaded.UsageHistory.Weeks[0].SampleCount != 3 {
		t.Fatalf("usage sample not preserved: %+v", loaded.UsageHistory.Weeks[0])
	}
}

func TestSavePrunesUsageHistoryToFiveWeeks(t *testing.T) {
	// Monday 2026-07-13 00:00 UTC; now is mid-week of that week.
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}

	// Seed 7 weekly samples ending in the current week.
	weeks := make([]UsageSample, 7)
	// Use explicit Monday-anchored week starts so the window is deterministic.
	currentMonday := weekStart(now)
	for i := 0; i < 7; i++ {
		weeks[i] = UsageSample{WeekStart: currentMonday.AddDate(0, 0, -7*i), SampleCount: i}
	}
	s := State{
		Schema:       CurrentSchema,
		Providers:    map[string]ProviderState{},
		Targets:      map[string]TargetState{},
		UsageHistory: &UsageHistory{Weeks: weeks},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	raw := loadRaw(t, p)
	if raw.UsageHistory == nil {
		t.Fatal("usage history nil after save")
	}
	if got := len(raw.UsageHistory.Weeks); got != 5 {
		t.Fatalf("weeks=%d want 5", got)
	}
	// The five most recent (offsets 0..4) survive; offsets 5,6 are pruned.
	for i, w := range raw.UsageHistory.Weeks {
		if w.SampleCount >= 5 {
			t.Fatalf("pruned week survived at index %d: %+v", i, w)
		}
	}
}

func TestPruneUsageHistoryNilSafe(t *testing.T) {
	s := State{Providers: map[string]ProviderState{}, Targets: map[string]TargetState{}}
	got := PruneUsageHistory(s, time.Now())
	if got.UsageHistory != nil {
		t.Fatalf("expected nil usage history, got %+v", got.UsageHistory)
	}
}

func TestSaveSanitizesSnapshotErrorBeforePersist(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}

	// A raw secret-bearing error string that should never reach the file.
	raw := "GET https://alice:hunter2@provider.example/v1/quota failed"
	s := State{
		Schema:    CurrentSchema,
		Providers: map[string]ProviderState{},
		Targets:   map[string]TargetState{},
	}
	s.Providers["codex"] = ProviderState{
		QuotaSnapshot: &quota.QuotaSnapshot{MappingID: "codex", Error: raw},
		QuotaAttempt:  &quota.QuotaSnapshot{MappingID: "codex", Error: raw, Status: quota.SourceFailed},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"hunter2", "alice"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("persisted state contains secret %q", forbidden)
		}
	}
}

func TestSanitizedSnapshotErrorPersists(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}

	// An already-sanitized, secret-free error persists unchanged.
	clean := "quota query for [redacted] denied: 401"
	s := State{
		Schema:    CurrentSchema,
		Providers: map[string]ProviderState{},
		Targets:   map[string]TargetState{},
	}
	s.Providers["codex"] = ProviderState{
		QuotaSnapshot: &quota.QuotaSnapshot{MappingID: "codex", Error: clean},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	codex := loaded.Providers["codex"]
	if codex.QuotaSnapshot == nil || codex.QuotaSnapshot.Error != clean {
		t.Fatalf("sanitized error not preserved: %+v", codex.QuotaSnapshot)
	}
}

func TestSaveFreshStatePersistsCurrentSchema(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}
	// Construct with a stale schema value; Save must persist CurrentSchema.
	s := State{Schema: 1, Providers: map[string]ProviderState{}, Targets: map[string]TargetState{}}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	raw := loadRaw(t, p)
	if raw.Schema != CurrentSchema {
		t.Fatalf("persisted schema=%d want %d", raw.Schema, CurrentSchema)
	}
}
