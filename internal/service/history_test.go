package service

import (
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// countingStore counts LoadState calls for verification.
type countingStore struct {
	state    state.State
	loadErr  error
	loadCall int
}

func (s *countingStore) LoadState() (state.State, error) {
	s.loadCall++
	if s.loadErr != nil {
		return state.State{}, s.loadErr
	}
	return s.state, nil
}

func (s *countingStore) Save(st state.State) error { return nil }

// TestHistoryReadsStateExactlyOnceAndNothingElse verifies that Summaries and
// Detail each load state exactly once and sample the clock exactly once.
func TestHistoryReadsStateExactlyOnceAndNothingElse(t *testing.T) {
	store := &countingStore{state: state.State{
		ReconcileHistory: state.ReconcileHistory{
			Records: []state.ReconcileRecord{
				{Revision: 1, CompletedAt: time.Now().UTC(), Counts: state.AuthoritativeTargetCounts{Total: 1, Applied: 1}},
			},
		},
	}}
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	}
	reader := NewHistoryReader(store, clock)

	// Summaries: one load, one clock
	report, err := reader.Summaries(10)
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	if store.loadCall != 1 {
		t.Errorf("LoadState calls after Summaries: got %d, want 1", store.loadCall)
	}
	if clockCalls != 1 {
		t.Errorf("Clock calls after Summaries: got %d, want 1", clockCalls)
	}
	if len(report.Records) != 1 {
		t.Errorf("Records: got %d, want 1", len(report.Records))
	}

	// Detail: separate call, one load, one clock
	store.loadCall = 0
	clockCalls = 0
	detail, err := reader.Detail(1)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if store.loadCall != 1 {
		t.Errorf("LoadState calls after Detail: got %d, want 1", store.loadCall)
	}
	if clockCalls != 1 {
		t.Errorf("Clock calls after Detail: got %d, want 1", clockCalls)
	}
	if !detail.Found {
		t.Error("Detail should find revision 1")
	}
}

// TestHistoryReportsAreDeepCopies verifies that returned records do not alias
// the persisted state's slices.
func TestHistoryReportsAreDeepCopies(t *testing.T) {
	store := &countingStore{state: state.State{
		ReconcileHistory: state.ReconcileHistory{
			Records: []state.ReconcileRecord{
				{Revision: 1, CompletedAt: time.Now().UTC(), Counts: state.AuthoritativeTargetCounts{Total: 1, Applied: 1}},
				{Revision: 2, CompletedAt: time.Now().UTC(), Counts: state.AuthoritativeTargetCounts{Total: 2, Pending: 1}},
			},
		},
	}}
	reader := NewHistoryReader(store, func() time.Time { return time.Now() })

	report, _ := reader.Summaries(10)
	// Mutate the returned record
	if len(report.Records) > 0 {
		report.Records[0].Revision = 999
	}

	// Reload and verify the original is untouched
	store.loadCall = 0
	report2, _ := reader.Summaries(10)
	if report2.Records[0].Revision != 1 {
		t.Errorf("deep copy failed: original revision mutated to %d", report2.Records[0].Revision)
	}

	// Detail deep copy
	store.loadCall = 0
	detail, _ := reader.Detail(2)
	if detail.Found {
		detail.Record.Revision = 999
		store.loadCall = 0
		detail2, _ := reader.Detail(2)
		if detail2.Record.Revision != 2 {
			t.Errorf("detail deep copy failed: original revision mutated to %d", detail2.Record.Revision)
		}
	}
}

// TestHistoryQuerySelectionAndErrors verifies limit selection, missing
// revision, and load errors.
func TestHistoryQuerySelectionAndErrors(t *testing.T) {
	records := make([]state.ReconcileRecord, 5)
	for i := range records {
		records[i] = state.ReconcileRecord{
			Revision:    uint64(5 - i), // newest-first: 5,4,3,2,1
			CompletedAt: time.Now().UTC(),
			Counts:      state.AuthoritativeTargetCounts{Total: 1, Applied: 1},
		}
	}
	store := &countingStore{state: state.State{
		ReconcileHistory: state.ReconcileHistory{Records: records},
	}}
	reader := NewHistoryReader(store, func() time.Time { return time.Now() })

	// Limit selection: newest-first, so limit 2 returns revisions 5 and 4
	report, _ := reader.Summaries(2)
	if len(report.Records) != 2 {
		t.Fatalf("limit 2: got %d records, want 2", len(report.Records))
	}
	if report.Records[0].Revision != 5 || report.Records[1].Revision != 4 {
		t.Errorf("limit selection: revisions %d, %d; want 5, 4", report.Records[0].Revision, report.Records[1].Revision)
	}

	// Limit 0 returns all
	store.loadCall = 0
	report, _ = reader.Summaries(0)
	if len(report.Records) != 5 {
		t.Errorf("limit 0: got %d records, want 5", len(report.Records))
	}

	// Missing revision
	store.loadCall = 0
	detail, _ := reader.Detail(999)
	if detail.Found {
		t.Error("revision 999 should not be found")
	}

	// Load error
	store.loadCall = 0
	store.loadErr = errLoadFail
	_, err := reader.Summaries(10)
	if err == nil {
		t.Error("expected error from failed load")
	}
}

var errLoadFail = &testError{"load failed"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
