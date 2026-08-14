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

// HistoryEventReport is the newest-first event timeline query result.
type HistoryEventReport struct {
	ReportedAt    time.Time
	Events        []state.EventRecord
	OmittedEvents int
}

// HistoryRevisionReport is the event/evidence query result for one revision.
type HistoryRevisionReport struct {
	ReportedAt time.Time
	Revision   uint64
	Events     []state.EventRecord
	Found      bool
}

// HistoryQuerier is the CLI-facing history query interface.
type HistoryQuerier interface {
	Events(limit int) (HistoryEventReport, error)
	RevisionEvents(revision uint64) (HistoryRevisionReport, error)
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

// Events returns the newest N meaningful events as deep copies. A limit of 0 or
// less returns all retained events. It never saves or performs migration.
func (r *HistoryReader) Events(limit int) (HistoryEventReport, error) {
	s, err := r.Store.LoadState()
	if err != nil {
		return HistoryEventReport{}, err
	}
	now := r.Clock()
	events := append([]state.EventRecord(nil), s.EventHistory.Events...)
	if limit > 0 && limit < len(events) {
		events = events[:limit]
	}
	for i := range events {
		events[i] = copyEvent(events[i])
	}
	if events == nil {
		events = []state.EventRecord{}
	}
	return HistoryEventReport{ReportedAt: now, Events: events, OmittedEvents: s.EventHistory.OmittedEvents}, nil
}

// RevisionEvents returns all meaningful events attached to a state revision as
// deep copies. It never saves or performs migration.
func (r *HistoryReader) RevisionEvents(revision uint64) (HistoryRevisionReport, error) {
	s, err := r.Store.LoadState()
	if err != nil {
		return HistoryRevisionReport{}, err
	}
	now := r.Clock()
	var events []state.EventRecord
	for _, event := range s.EventHistory.Events {
		if event.Revision == revision {
			events = append(events, copyEvent(event))
		}
	}
	if events == nil {
		return HistoryRevisionReport{ReportedAt: now, Revision: revision}, nil
	}
	return HistoryRevisionReport{ReportedAt: now, Revision: revision, Events: events, Found: true}, nil
}

func copyEvent(in state.EventRecord) state.EventRecord {
	out := in
	out.Changes = append([]string(nil), in.Changes...)
	for _, p := range []*float64{in.UsagePercent, in.Used, in.Limit} {
		_ = p
	}
	if in.UsagePercent != nil {
		v := *in.UsagePercent
		out.UsagePercent = &v
	}
	if in.Used != nil {
		v := *in.Used
		out.Used = &v
	}
	if in.Limit != nil {
		v := *in.Limit
		out.Limit = &v
	}
	if in.ResetAt != nil {
		v := *in.ResetAt
		out.ResetAt = &v
	}
	if in.OldRank != nil {
		v := *in.OldRank
		out.OldRank = &v
	}
	if in.NewRank != nil {
		v := *in.NewRank
		out.NewRank = &v
	}
	if in.OldEligible != nil {
		v := *in.OldEligible
		out.OldEligible = &v
	}
	if in.NewEligible != nil {
		v := *in.NewEligible
		out.NewEligible = &v
	}
	if in.OldOffPeak != nil {
		v := *in.OldOffPeak
		out.OldOffPeak = &v
	}
	if in.NewOffPeak != nil {
		v := *in.NewOffPeak
		out.NewOffPeak = &v
	}
	return out
}

// Summaries returns the newest N legacy records for compatibility with internal
// callers during the CLI migration. New callers must use Events.
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
