package service

// State-only history reader. Loads durable reconcile history from state
// without touching any live collaborator (no policy, targets, lock, recovery,
// reconciler, publisher, or process surface). Each invocation loads state
// exactly once and samples the clock exactly once.

import (
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// HistorySummary is the summary view of one history record.
type HistorySummary struct {
	Revision    uint64
	CompletedAt time.Time
	Trigger     state.Trigger
	Applied     int
	Pending     int
}

// HistorySummaryReport is the result of a summary query.
type HistorySummaryReport struct {
	ReportedAt time.Time
	Records    []HistorySummary
}

// HistoryDetailReport is the result of a detail query for one revision.
// Found is false when the revision is absent from retained history.
type HistoryDetailReport struct {
	ReportedAt time.Time
	Record     state.ReconcileRecord
	Found      bool
}

// HistoryQuerier is the CLI-facing history query interface.
type HistoryQuerier interface {
	Summaries(limit int) (HistorySummaryReport, error)
	Detail(revision uint64) (HistoryDetailReport, error)
}

// HistoryReader reads durable reconcile history from state. It loads state
// exactly once per invocation and samples the clock exactly once. It does not
// acquire the mutation lock, invoke recovery or reconciliation, load policy,
// resolve targets, read live Polytoken files, inspect processes, or mutate
// state.
type HistoryReader struct {
	Store StateStore
	Clock func() time.Time
}

// NewHistoryReader creates a reader backed by the given store and clock.
func NewHistoryReader(store StateStore, clock func() time.Time) *HistoryReader {
	if clock == nil {
		clock = time.Now
	}
	return &HistoryReader{Store: store, Clock: clock}
}

// Summaries returns the newest N records as deep-copied summaries. A limit
// of 0 or less returns all retained records.
func (r *HistoryReader) Summaries(limit int) (HistorySummaryReport, error) {
	s, err := r.Store.LoadState()
	if err != nil {
		return HistorySummaryReport{}, err
	}
	now := r.Clock()
	records := s.ReconcileHistory.Records
	if limit > 0 && limit < len(records) {
		records = records[:limit]
	}
	summaries := make([]HistorySummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, HistorySummary{
			Revision:    rec.Revision,
			CompletedAt: rec.CompletedAt,
			Trigger:     rec.Trigger,
			Applied:     rec.Counts.Applied,
			Pending:     rec.Counts.Pending,
		})
	}
	return HistorySummaryReport{
		ReportedAt: now,
		Records:    summaries,
	}, nil
}

// Detail returns the full record for one revision as a deep copy. Found is
// false when the revision is absent from retained history.
func (r *HistoryReader) Detail(revision uint64) (HistoryDetailReport, error) {
	s, err := r.Store.LoadState()
	if err != nil {
		return HistoryDetailReport{}, err
	}
	now := r.Clock()
	for _, rec := range s.ReconcileHistory.Records {
		if rec.Revision == revision {
			return HistoryDetailReport{
				ReportedAt: now,
				Record:     state.DeepCopyHistoryRecord(rec),
				Found:      true,
			}, nil
		}
	}
	return HistoryDetailReport{ReportedAt: now}, nil
}
