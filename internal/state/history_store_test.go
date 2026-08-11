package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadMigratesLegacySchemasToV4EmptyHistory(t *testing.T) {
	for schema := 0; schema <= 3; schema++ {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "state.json")
			writeFile(t, p, `{"Schema":`+string(rune('0'+schema))+`,"Revision":9,"Providers":{},"Targets":{}}`)
			s, err := (Store{Path: p}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if s.Schema != 4 || s.Revision != 9 || len(s.ReconcileHistory.Records) != 0 {
				t.Fatalf("state=%+v", s)
			}
		})
	}
}

func TestLoadLegacySchemaDiscardsEmbeddedHistoryBeforeValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	writeFile(t, p, `{"Schema":3,"Providers":{},"Targets":{},"ReconcileHistory":{"Records":[{"Revision":1,"Tier":"future"}]}}`)
	s, err := (Store{Path: p}).Load()
	if err != nil {
		t.Fatalf("legacy history must be discarded before validation: %v", err)
	}
	if s.Schema != CurrentSchema || len(s.ReconcileHistory.Records) != 0 {
		t.Fatalf("state=%+v", s)
	}
}

func TestHistoryRoundTripAndRetentionAt100WhenWithinBudget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return historyTestTime }, RecoveredRetention: 24 * time.Hour}
	s := newState()
	for i := 1; i <= HistoryRecordLimit+5; i++ {
		tpl := validHistoryTemplate()
		tpl.Revision = uint64(i)
		r, err := ProjectHistoryRecord(tpl, historyTestTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		s.ReconcileHistory, err = AppendHistory(s.ReconcileHistory, r)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ReconcileHistory.Records) != HistoryRecordLimit || got.ReconcileHistory.Records[0].Revision != 105 || got.ReconcileHistory.Records[99].Revision != 6 {
		t.Fatalf("history=%+v", got.ReconcileHistory)
	}
	copy := DeepCopyReconcileHistory(got.ReconcileHistory)
	copy.Records[0].Targets[0].ID = "changed"
	if got.ReconcileHistory.Records[0].Targets[0].ID == "changed" {
		t.Fatal("deep copy aliases nested state")
	}
}

func TestHistoryAppendDeduplicatesRevision(t *testing.T) {
	r := validHistoryRecord(t)
	h, err := AppendHistory(ReconcileHistory{}, r)
	if err != nil {
		t.Fatal(err)
	}
	replacement := r
	replacement.CompletedAt = replacement.CompletedAt.Add(time.Hour)
	h, err = AppendHistory(h, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Records) != 1 || !h.Records[0].CompletedAt.Equal(replacement.CompletedAt) {
		t.Fatalf("history=%+v", h)
	}
}

func TestLoadRejectsUnknownHistorySchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	writeFile(t, p, `{"Schema":5,"Providers":{},"Targets":{}}`)
	if _, err := (Store{Path: p}).Load(); err == nil {
		t.Fatal("expected unknown schema rejection")
	}
}

func TestSaveSanitizesHistoryCanaries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return historyTestTime }}
	tpl := validHistoryTemplate()
	tpl.Providers[0].Reason = "Bearer SECRET account=alice " + strings.Repeat("é", 600)
	tpl.Targets[0].Edits[0].Detail = "api_key=CANARY " + strings.Repeat("z", 800)
	tpl.Targets[0].Outcome = OutcomePending
	tpl.Targets[0].Pending = &PendingDetail{Stage: PendingPublish, Summary: "https://alice:hunter2@example.invalid failed", Remediation: "token=CANARY retry"}
	r, err := ProjectHistoryRecord(tpl, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	s := newState()
	s.ReconcileHistory, err = AppendHistory(s.ReconcileHistory, r)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a long-lived in-memory caller reintroducing raw text after construction.
	s.ReconcileHistory.Records[0].Providers[0].Reason = "Bearer SAVESECRET account=save-account"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"SECRET", "alice", "hunter2", "CANARY", "SAVESECRET", "save-account"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("persisted canary %q", bad)
		}
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReconcileHistory.Records) != 1 {
		t.Fatal("history missing")
	}
}

func TestHistoryJSONDeterministic(t *testing.T) {
	r := validHistoryRecord(t)
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(r)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("non-deterministic encoding")
	}
}
