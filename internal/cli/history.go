package cli

// History command implementation: parses --limit, --revision, --json and
// renders summary or detail reports in human or JSON format.

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// historyFlags holds parsed history command flags.
type historyFlags struct {
	limit    int
	revision int64
	hasLimit bool
	hasRev   bool
	json     bool
}

func parseHistoryFlags(args []string) (historyFlags, bool) {
	f := historyFlags{limit: 20}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			f.json = true
		case "--limit":
			if i+1 >= len(args) {
				return f, false
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 100 {
				return f, false
			}
			f.limit = n
			f.hasLimit = true
			i++
		case "--revision":
			if i+1 >= len(args) {
				return f, false
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || n < 1 {
				return f, false
			}
			f.revision = n
			f.hasRev = true
			i++
		default:
			if len(args[i]) > 8 && args[i][:8] == "--limit=" {
				n, err := strconv.Atoi(args[i][8:])
				if err != nil || n < 1 || n > 100 {
					return f, false
				}
				f.limit = n
				f.hasLimit = true
			} else if len(args[i]) > 11 && args[i][:11] == "--revision=" {
				n, err := strconv.ParseInt(args[i][11:], 10, 64)
				if err != nil || n < 1 {
					return f, false
				}
				f.revision = n
				f.hasRev = true
			} else {
				return f, false
			}
		}
	}
	if f.hasLimit && f.hasRev {
		return f, false // mutually exclusive
	}
	return f, true
}

func runHistory(args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		writeCommandHelp(stdout, "history")
		return ExitOK
	}
	if deps.HistoryQuerier == nil {
		fmt.Fprintln(stderr, "history: history querier unavailable")
		return ExitRejected
	}
	flags, ok := parseHistoryFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "history: invalid arguments")
		fmt.Fprintln(stderr, "usage: polytoken-quota history [--limit N] [--revision N] [--json]")
		return ExitRejected
	}

	if flags.hasRev {
		return runHistoryDetail(flags, deps, stdout, stderr)
	}
	return runHistorySummary(flags, deps, stdout, stderr)
}

func runHistorySummary(flags historyFlags, deps Dependencies, stdout, stderr io.Writer) int {
	report, err := deps.HistoryQuerier.Events(flags.limit)
	if err != nil {
		if flags.json {
			writeHistoryJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, err.Error())
		}
		return ExitRejected
	}
	if flags.json {
		writeHistoryJSONEvents(stdout, report)
		return ExitOK
	}
	if len(report.Events) == 0 {
		fmt.Fprintln(stdout, "No provider or routing events recorded.")
		return ExitOK
	}
	fmt.Fprintf(stdout, "EVENT HISTORY\nReported at: %s\n\n", report.ReportedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(stdout, "%-20s %-10s %-24s %s\n", "WHEN", "PROVIDER", "EVENT", "RESULT")
	for _, event := range report.Events {
		writeHistoryEventHuman(stdout, event)
	}
	return ExitOK
}

func runHistoryDetail(flags historyFlags, deps Dependencies, stdout, stderr io.Writer) int {
	report, err := deps.HistoryQuerier.RevisionEvents(uint64(flags.revision))
	if err != nil {
		if flags.json {
			writeHistoryJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, err.Error())
		}
		return ExitRejected
	}
	if !report.Found {
		msg := fmt.Sprintf("history: revision %d not found", flags.revision)
		if flags.json {
			writeHistoryJSONError(stdout, fmt.Errorf("%s", msg))
		} else {
			fmt.Fprintln(stderr, msg)
		}
		return ExitRejected
	}

	if flags.json {
		writeHistoryJSONDetail(stdout, report)
		return ExitOK
	}

	writeHistoryDetailHuman(stdout, report)
	return ExitOK
}

func writeHistoryEventHuman(w io.Writer, event state.EventRecord) {
	provider := event.Provider
	if provider == "" {
		provider = event.MappingID
	}
	result := event.Reason
	if result == "" {
		result = string(event.Result)
	}
	if event.AfterMode != "" && event.BeforeMode != event.AfterMode {
		result = fmt.Sprintf("%s; mode %s -> %s", result, event.BeforeMode, event.AfterMode)
	}
	if event.OldRank != nil && event.NewRank != nil && *event.OldRank != *event.NewRank {
		result = fmt.Sprintf("%s; rank %d -> %d", result, *event.OldRank, *event.NewRank)
	}
	if event.Applied > 0 || event.Pending > 0 {
		result = fmt.Sprintf("%s; applied=%d pending=%d", result, event.Applied, event.Pending)
	}
	fmt.Fprintf(w, "%-20s %-10s %-24s %s\n", event.At.Format("2006-01-02 15:04:05"), provider, event.Action, result)
}

func writeHistoryDetailHuman(w io.Writer, report service.HistoryRevisionReport) {
	fmt.Fprintf(w, "Revision: %d\n", report.Revision)
	fmt.Fprintf(w, "Events:   %d\n\n", len(report.Events))
	for _, event := range report.Events {
		writeHistoryEventHuman(w, event)
	}
}

func writeHistoryJSONEvents(w io.Writer, report service.HistoryEventReport) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		ReportedAt    string              `json:"reported_at"`
		OmittedEvents int                 `json:"omitted_events,omitempty"`
		Events        []state.EventRecord `json:"events"`
	}{report.ReportedAt.Format(time.RFC3339Nano), report.OmittedEvents, nonNilEvents(report.Events)})
}

func writeHistoryJSONDetail(w io.Writer, report service.HistoryRevisionReport) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		ReportedAt string              `json:"reported_at"`
		Revision   uint64              `json:"revision"`
		Events     []state.EventRecord `json:"events"`
	}{report.ReportedAt.Format(time.RFC3339Nano), report.Revision, nonNilEvents(report.Events)})
}

func nonNilEvents(events []state.EventRecord) []state.EventRecord {
	if events == nil {
		return []state.EventRecord{}
	}
	return events
}

func writeHistoryJSONError(w io.Writer, err error) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"error": err.Error()})
}
