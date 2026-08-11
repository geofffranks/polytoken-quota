package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// fakeHistoryQuerier implements service.HistoryQuerier for testing.
type fakeHistoryQuerier struct {
	summaries service.HistorySummaryReport
	detail    service.HistoryDetailReport
	summaryFn func(limit int) (service.HistorySummaryReport, error)
	detailFn  func(rev uint64) (service.HistoryDetailReport, error)
	summaryCalls int
	detailCalls  int
	lastLimit    int
	lastRev      uint64
}

func (f *fakeHistoryQuerier) Summaries(limit int) (service.HistorySummaryReport, error) {
	f.summaryCalls++
	f.lastLimit = limit
	if f.summaryFn != nil {
		return f.summaryFn(limit)
	}
	return f.summaries, nil
}

func (f *fakeHistoryQuerier) Detail(revision uint64) (service.HistoryDetailReport, error) {
	f.detailCalls++
	f.lastRev = revision
	if f.detailFn != nil {
		return f.detailFn(revision)
	}
	return f.detail, nil
}

func sampleSummaryReport(n int) service.HistorySummaryReport {
	recs := make([]service.HistorySummary, n)
	for i := range recs {
		recs[i] = service.HistorySummary{
			Revision:    uint64(100 - i),
			CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
			Trigger:     state.Trigger{Kind: state.TriggerReconcile},
			Applied:     2,
			Pending:     1,
		}
	}
	return service.HistorySummaryReport{
		ReportedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC),
		Records:    recs,
	}
}

func sampleDetailRecord() state.ReconcileRecord {
	return state.ReconcileRecord{
		Revision:    42,
		CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		Trigger:     state.Trigger{Kind: state.TriggerHook, Hook: &state.HookEvidence{
			Event:     state.HookQuotaLow,
			Provider:  "codex",
			Timestamp: time.Date(2026, 8, 11, 11, 55, 0, 0, time.UTC),
		}},
		Tier: state.TierFull,
		Counts: state.AuthoritativeTargetCounts{
			Total: 2, Applied: 1, Pending: 1,
		},
		Targets: []state.TargetDetail{
			{ID: "global", Outcome: state.OutcomeApplied},
			{ID: "project-a", Outcome: state.OutcomePending, Pending: &state.PendingDetail{
				Stage: state.PendingPublishPrepare, Summary: "backup unavailable",
			}},
		},
	}
}

// --- Flag parsing tests (AC.9) ---

func TestHistoryFlagParsingDefaultLimit(t *testing.T) {
	f, ok := parseHistoryFlags(nil)
	if !ok {
		t.Fatal("expected ok for no flags")
	}
	if f.limit != 20 {
		t.Fatalf("default limit = %d, want 20", f.limit)
	}
	if f.hasLimit || f.hasRev || f.json {
		t.Fatal("expected all flags false by default")
	}
}

func TestHistoryFlagParsingLimitRange(t *testing.T) {
	valid := []string{"1", "50", "100"}
	for _, v := range valid {
		f, ok := parseHistoryFlags([]string{"--limit", v})
		if !ok {
			t.Fatalf("--limit %s should be valid", v)
		}
		if !f.hasLimit {
			t.Fatalf("hasLimit should be true for --limit %s", v)
		}
	}
	invalid := []string{"0", "-1", "101", "abc"}
	for _, v := range invalid {
		_, ok := parseHistoryFlags([]string{"--limit", v})
		if ok {
			t.Fatalf("--limit %s should be rejected", v)
		}
	}
}

func TestHistoryFlagParsingEqualsSyntax(t *testing.T) {
	f, ok := parseHistoryFlags([]string{"--limit=5", "--json"})
	if !ok {
		t.Fatal("--limit=5 --json should be valid")
	}
	if f.limit != 5 || !f.json {
		t.Fatalf("limit=%d json=%v, want 5/true", f.limit, f.json)
	}
	f2, ok := parseHistoryFlags([]string{"--revision=7"})
	if !ok {
		t.Fatal("--revision=7 should be valid")
	}
	if f2.revision != 7 || !f2.hasRev {
		t.Fatalf("revision=%d hasRev=%v, want 7/true", f2.revision, f2.hasRev)
	}
}

func TestHistoryFlagParsingMutualExclusion(t *testing.T) {
	_, ok := parseHistoryFlags([]string{"--limit", "5", "--revision", "3"})
	if ok {
		t.Fatal("--limit and --revision should be mutually exclusive")
	}
	_, ok = parseHistoryFlags([]string{"--limit=5", "--revision=3"})
	if ok {
		t.Fatal("--limit= and --revision= should be mutually exclusive")
	}
}

func TestHistoryFlagParsingRevisionValidation(t *testing.T) {
	_, ok := parseHistoryFlags([]string{"--revision", "0"})
	if ok {
		t.Fatal("--revision 0 should be rejected")
	}
	_, ok = parseHistoryFlags([]string{"--revision", "-1"})
	if ok {
		t.Fatal("--revision -1 should be rejected")
	}
}

func TestHistoryFlagParsingUnknownFlag(t *testing.T) {
	_, ok := parseHistoryFlags([]string{"--bogus"})
	if ok {
		t.Fatal("unknown flag should be rejected")
	}
}

// --- Summary mode tests (AC.9) ---

func TestHistorySummaryEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{summaries: service.HistorySummaryReport{
		ReportedAt: time.Now(),
		Records:    []service.HistorySummary{},
	}}
	code := runHistory(nil, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No reconcile changes recorded.") {
		t.Fatalf("expected empty message, got: %s", stdout.String())
	}
}

func TestHistorySummaryRendersRecords(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{summaries: sampleSummaryReport(3)}
	code := runHistory(nil, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "REV") {
		t.Fatal("missing header row")
	}
	// Should show revision 100, 99, 98
	for _, rev := range []string{"100", "99", "98"} {
		if !strings.Contains(out, rev) {
			t.Fatalf("missing revision %s in output: %s", rev, out)
		}
	}
	if !strings.Contains(out, "reconcile") {
		t.Fatal("missing trigger kind")
	}
}

func TestHistorySummaryRespectsLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{summaries: sampleSummaryReport(10)}
	deps := depsWithHistory(q)
	runHistory([]string{"--limit", "3"}, deps, &stdout, &stderr)
	if q.lastLimit != 3 {
		t.Fatalf("limit passed to querier = %d, want 3", q.lastLimit)
	}
}

// --- Detail mode tests (AC.9) ---

func TestHistoryDetailFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{detail: service.HistoryDetailReport{
		ReportedAt: time.Now(),
		Record:     sampleDetailRecord(),
		Found:      true,
	}}
	code := runHistory([]string{"--revision", "42"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Revision:     42") {
		t.Fatalf("missing revision in detail output: %s", out)
	}
	if !strings.Contains(out, "full") {
		t.Fatalf("missing tier in detail output: %s", out)
	}
	if !strings.Contains(out, "global") {
		t.Fatalf("missing target ID in detail output: %s", out)
	}
}

func TestHistoryDetailNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{detail: service.HistoryDetailReport{Found: false}}
	code := runHistory([]string{"--revision", "999"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitRejected {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("expected not-found error, got: %s", stderr.String())
	}
}

func TestHistoryDetailAggregateTier(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rec := state.ReconcileRecord{
		Revision:        10,
		CompletedAt:     time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		Trigger:         state.Trigger{Kind: state.TriggerReconcile},
		Tier:            state.TierAggregate,
		DetailTruncated: true,
		Counts: state.AuthoritativeTargetCounts{Total: 70, Applied: 60, Pending: 10, Omitted: 6},
		CompactTargets: []state.CompactTarget{
			{ID: "global", Outcome: state.OutcomeApplied},
		},
	}
	q := &fakeHistoryQuerier{detail: service.HistoryDetailReport{
		ReportedAt: time.Now(),
		Record:     rec,
		Found:      true,
	}}
	code := runHistory([]string{"--revision", "10"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "aggregate") {
		t.Fatalf("missing tier in output: %s", out)
	}
	if !strings.Contains(out, "detail truncated") || !strings.Contains(out, "omitted") {
		t.Fatalf("expected truncation indicator with omitted count: %s", out)
	}
}

// --- JSON tests (AC.10) ---

func TestHistoryJSONSummary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{summaries: sampleSummaryReport(2)}
	code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	recs, ok := result["records"].([]interface{})
	if !ok {
		t.Fatalf("records not an array: %v", result)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if _, ok := result["reported_at"]; !ok {
		t.Fatal("missing reported_at field")
	}
}

func TestHistoryJSONEmptyRecords(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{summaries: service.HistorySummaryReport{
		ReportedAt: time.Now(),
		Records:    nil, // nil should become empty array
	}}
	code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var result map[string]interface{}
	json.Unmarshal(stdout.Bytes(), &result)
	recs, _ := result["records"].([]interface{})
	if recs == nil {
		t.Fatal("records should be an empty array, not null")
	}
	if len(recs) != 0 {
		t.Fatalf("expected 0 records, got %d", len(recs))
	}
}

func TestHistoryJSONDetail(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{detail: service.HistoryDetailReport{
		ReportedAt: time.Now(),
		Record:     sampleDetailRecord(),
		Found:      true,
	}}
	code := runHistory([]string{"--revision", "42", "--json"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var result state.ReconcileRecord
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if result.Revision != 42 {
		t.Fatalf("revision = %d, want 42", result.Revision)
	}
	if result.Tier != state.TierFull {
		t.Fatalf("tier = %s, want full", result.Tier)
	}
	if len(result.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(result.Targets))
	}
}

func TestHistoryJSONNotFoundError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{detail: service.HistoryDetailReport{Found: false}}
	code := runHistory([]string{"--revision", "999", "--json"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitRejected {
		t.Fatalf("exit = %d, want 1", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("error output must be valid JSON: %v\n%s", err, stdout.String())
	}
	if _, ok := result["error"]; !ok {
		t.Fatalf("expected error key, got: %v", result)
	}
}

func TestHistoryJSONLoadError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{
		summaryFn: func(int) (service.HistorySummaryReport, error) {
			return service.HistorySummaryReport{}, errors.New("state load failed")
		},
	}
	code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr)
	if code != ExitRejected {
		t.Fatalf("exit = %d, want 1", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("error output must be valid JSON: %v", err)
	}
	if !strings.Contains(result["error"].(string), "state load failed") {
		t.Fatalf("error message mismatch: %v", result["error"])
	}
}

// --- Nil querier test ---

func TestHistoryNilQuerier(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := Dependencies{}
	code := runHistory(nil, deps, &stdout, &stderr)
	if code != ExitRejected {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unavailable") {
		t.Fatalf("expected unavailable message, got: %s", stderr.String())
	}
}

// --- Human error output ---

func TestHistoryHumanLoadError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{
		summaryFn: func(int) (service.HistorySummaryReport, error) {
			return service.HistorySummaryReport{}, fmt.Errorf("disk read error")
		},
	}
	code := runHistory(nil, depsWithHistory(q), &stdout, &stderr)
	if code != ExitRejected {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "disk read error") {
		t.Fatalf("expected error in stderr: %s", stderr.String())
	}
	if stdout.Len() > 0 {
		t.Fatalf("stdout should be empty on error, got: %s", stdout.String())
	}
}

// --- Public surface test (AC.11) ---

func TestUsageListsHistory(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	if !strings.Contains(buf.String(), "history") {
		t.Fatalf("usage text should list history command:\n%s", buf.String())
	}
}

// depsWithHistory builds Dependencies with only the history querier set.
func depsWithHistory(q service.HistoryQuerier) Dependencies {
	return Dependencies{HistoryQuerier: q}
}
