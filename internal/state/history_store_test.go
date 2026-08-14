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

func TestLoadMigratesLegacySchemasToV5EmptyEventHistory(t *testing.T) {
	for schema := 0; schema <= 4; schema++ {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "state.json")
			writeFile(t, p, `{"Schema":`+string(rune('0'+schema))+`,"Revision":9,"Providers":{},"Targets":{}}`)
			s, err := (Store{Path: p}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if s.Schema != CurrentSchema || s.Revision != 9 || len(s.EventHistory.Events) != 0 || s.NextArrivalSequence != 1 {
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

func TestEventHistoryRoundTripAndRetention(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return historyTestTime }, RecoveredRetention: 24 * time.Hour}
	s := newState()
	for i := 1; i <= EventHistoryLimit+5; i++ {
		e := EventRecord{Sequence: uint64(i), Revision: uint64(i), Ordinal: 0, At: historyTestTime.Add(time.Duration(i) * time.Second), RecordedAt: historyTestTime.Add(time.Duration(i) * time.Second), Category: EventHook, Action: "quota_low", Provider: "codex", Result: EventChanged}
		var err error
		s.EventHistory, err = AppendEvent(s.EventHistory, e)
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
	if len(got.EventHistory.Events) != EventHistoryLimit || got.EventHistory.Events[0].Sequence != EventHistoryLimit+5 || got.EventHistory.Events[EventHistoryLimit-1].Sequence != 6 {
		t.Fatalf("history=%+v", got.EventHistory)
	}
	copy := SanitizeEventHistory(got.EventHistory)
	copy.Events[0].Changes = []string{"changed"}
	if len(got.EventHistory.Events[0].Changes) != 0 {
		t.Fatal("deep copy unexpectedly aliases")
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
	writeFile(t, p, `{"Schema":6,"Providers":{},"Targets":{}}`)
	if _, err := (Store{Path: p}).Load(); err == nil {
		t.Fatal("expected unknown schema rejection")
	}
}

func TestSaveSanitizesEventCanaries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return historyTestTime }}
	s := newState()
	s.EventHistory.Events = []EventRecord{{Sequence: 1, Revision: 1, Ordinal: 0, At: historyTestTime, RecordedAt: historyTestTime, Category: EventHook, Action: "quota_low", Provider: "alice", Result: EventFailed, Reason: "Bearer SECRET account=alice", Status: "https://alice:hunter2@example.invalid failed", Explanation: "api_key=CANARY"}}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"SECRET", "hunter2", "CANARY"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("persisted canary %q", bad)
		}
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.EventHistory.Events) != 1 {
		t.Fatal("event history missing")
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
