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
	events        service.HistoryEventReport
	revision      service.HistoryRevisionReport
	eventsFn      func(limit int) (service.HistoryEventReport, error)
	revisionFn    func(rev uint64) (service.HistoryRevisionReport, error)
	eventCalls    int
	revisionCalls int
	lastLimit     int
	lastRev       uint64
}

func (f *fakeHistoryQuerier) Events(limit int) (service.HistoryEventReport, error) {
	f.eventCalls++
	f.lastLimit = limit
	if f.eventsFn != nil {
		return f.eventsFn(limit)
	}
	return f.events, nil
}

func (f *fakeHistoryQuerier) RevisionEvents(revision uint64) (service.HistoryRevisionReport, error) {
	f.revisionCalls++
	f.lastRev = revision
	if f.revisionFn != nil {
		return f.revisionFn(revision)
	}
	return f.revision, nil
}

func sampleEventReport(n int) service.HistoryEventReport {
	events := make([]state.EventRecord, n)
	for i := range events {
		events[i] = state.EventRecord{Sequence: uint64(100 - i), Revision: uint64(100 - i), Ordinal: 0, At: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC), RecordedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC), Category: state.EventHook, Action: "quota_low", Provider: "codex", Result: state.EventChanged, Reason: "reserve"}
	}
	return service.HistoryEventReport{ReportedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC), Events: events}
}

func sampleDetailRecord() state.ReconcileRecord {
	return state.ReconcileRecord{
		Revision:    42,
		CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		Trigger: state.Trigger{Kind: state.TriggerHook, Hook: &state.HookEvidence{
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

// --- Timeline mode tests ---

func TestHistoryTimelineEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{events: service.HistoryEventReport{ReportedAt: time.Now(), Events: []state.EventRecord{}}}
	code := runHistory(nil, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No provider or routing events recorded.") {
		t.Fatalf("output=%s", stdout.String())
	}
}

func TestHistoryTimelineRendersEvents(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{events: sampleEventReport(3)}
	code := runHistory(nil, depsWithHistory(q), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{"EVENT HISTORY", "WHEN", "quota_low", "codex", "reserve"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestHistoryTimelineRespectsLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{events: sampleEventReport(10)}
	runHistory([]string{"--limit", "3"}, depsWithHistory(q), &stdout, &stderr)
	if q.lastLimit != 3 {
		t.Fatalf("limit=%d", q.lastLimit)
	}
}

func TestHistoryRevisionFoundAndNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{revision: service.HistoryRevisionReport{Revision: 42, ReportedAt: time.Now(), Found: true, Events: []state.EventRecord{{Revision: 42, Sequence: 1, Ordinal: 0, At: time.Now(), RecordedAt: time.Now(), Category: state.EventHook, Action: "quota_reached", Provider: "zai", Result: state.EventChanged, Reason: "disabled"}}}}
	if code := runHistory([]string{"--revision", "42"}, depsWithHistory(q), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Revision: 42") || !strings.Contains(stdout.String(), "quota_reached") {
		t.Fatalf("output=%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	q.revision = service.HistoryRevisionReport{Revision: 999}
	if code := runHistory([]string{"--revision", "999"}, depsWithHistory(q), &stdout, &stderr); code != ExitRejected || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestHistoryJSONEventReports(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{events: sampleEventReport(2)}
	if code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result["events"].([]interface{}); !ok {
		t.Fatalf("events=%v", result)
	}
	if _, ok := result["reported_at"]; !ok {
		t.Fatal("reported_at missing")
	}
	stdout.Reset()
	q.events = service.HistoryEventReport{ReportedAt: time.Now(), Events: nil}
	if code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	json.Unmarshal(stdout.Bytes(), &result)
	if result["events"] == nil {
		t.Fatal("events must be an empty array")
	}
}

func TestHistoryJSONNotFoundAndLoadError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	q := &fakeHistoryQuerier{revision: service.HistoryRevisionReport{Revision: 999}}
	if code := runHistory([]string{"--revision", "999", "--json"}, depsWithHistory(q), &stdout, &stderr); code != ExitRejected {
		t.Fatalf("exit=%d", code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result["error"]; !ok {
		t.Fatal("error missing")
	}
	stdout.Reset()
	q.eventsFn = func(int) (service.HistoryEventReport, error) {
		return service.HistoryEventReport{}, errors.New("state load failed")
	}
	if code := runHistory([]string{"--json"}, depsWithHistory(q), &stdout, &stderr); code != ExitRejected {
		t.Fatalf("exit=%d", code)
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
		eventsFn: func(int) (service.HistoryEventReport, error) {
			return service.HistoryEventReport{}, fmt.Errorf("disk read error")
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
