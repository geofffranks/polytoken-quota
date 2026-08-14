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
	report, err := deps.HistoryQuerier.Summaries(flags.limit)
	if err != nil {
		if flags.json {
			writeHistoryJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, err.Error())
		}
		return ExitRejected
	}

	if flags.json {
		writeHistoryJSONSummary(stdout, report)
		return ExitOK
	}

	if len(report.Records) == 0 {
		fmt.Fprintln(stdout, "No reconcile changes recorded.")
		return ExitOK
	}

	fmt.Fprintf(stdout, "Reported at: %s\n\n", report.ReportedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(stdout, "%-8s %-20s %-18s %-8s %-8s\n", "REV", "COMPLETED", "TRIGGER", "APPLIED", "PENDING")
	for _, r := range report.Records {
		fmt.Fprintf(stdout, "%-8d %-20s %-18s %-8d %-8d\n",
			r.Revision,
			r.CompletedAt.Format("2006-01-02 15:04:05"),
			string(r.Trigger.Kind),
			r.Applied,
			r.Pending,
		)
	}
	return ExitOK
}

func runHistoryDetail(flags historyFlags, deps Dependencies, stdout, stderr io.Writer) int {
	report, err := deps.HistoryQuerier.Detail(uint64(flags.revision))
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

func writeHistoryDetailHuman(w io.Writer, report service.HistoryDetailReport) {
	r := report.Record
	fmt.Fprintf(w, "Revision:     %d\n", r.Revision)
	fmt.Fprintf(w, "Completed:    %s\n", r.CompletedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "Trigger:      %s\n", string(r.Trigger.Kind))
	fmt.Fprintf(w, "Tier:         %s\n", string(r.Tier))
	fmt.Fprintf(w, "Targets:      %d total, %d applied, %d pending\n", r.Counts.Total, r.Counts.Applied, r.Counts.Pending)
	if r.DetailTruncated {
		fmt.Fprintf(w, "              %d targets omitted (detail truncated)\n", r.Counts.Omitted)
	}
	if r.Trigger.Hook != nil {
		fmt.Fprintf(w, "Hook Event:   %s on %s\n", r.Trigger.Hook.Event, r.Trigger.Hook.Provider)
	}
	if r.Trigger.MappingID != "" {
		fmt.Fprintf(w, "Mapping:      %s\n", r.Trigger.MappingID)
	}
	fmt.Fprintln(w)

	if r.Tier == "full" {
		for _, p := range r.Providers {
			fmt.Fprintf(w, "  Provider %-12s mode=%-10s reason=%s\n", p.MappingID, p.Mode, p.Reason)
		}
	}

	for _, t := range r.Targets {
		outcome := "applied"
		if t.Outcome == state.OutcomePending {
			outcome = "pending"
		}
		fmt.Fprintf(w, "  Target %-20s %s\n", t.ID, outcome)
		for _, c := range t.Chains {
			fmt.Fprintf(w, "    Chain %-30s effective=%v\n", c.Name, c.Effective)
		}
		for _, e := range t.Edits {
			fmt.Fprintf(w, "    Edit %-20s %s = %s\n", e.File, e.Action, e.Detail)
		}
	}

	for _, ct := range r.CompactTargets {
		outcome := "applied"
		if ct.Outcome == state.OutcomePending {
			outcome = "pending"
		}
		fmt.Fprintf(w, "  Target %-20s %s\n", ct.ID, outcome)
	}
}

func writeHistoryJSONSummary(w io.Writer, report service.HistorySummaryReport) {
	type jsonRec struct {
		Revision    uint64 `json:"revision"`
		CompletedAt string `json:"completed_at"`
		Trigger     string `json:"trigger"`
		Applied     int    `json:"applied"`
		Pending     int    `json:"pending"`
	}
	type jsonReport struct {
		ReportedAt string    `json:"reported_at"`
		Records    []jsonRec `json:"records"`
	}
	recs := make([]jsonRec, 0, len(report.Records))
	for _, r := range report.Records {
		recs = append(recs, jsonRec{
			Revision:    r.Revision,
			CompletedAt: r.CompletedAt.Format(time.RFC3339Nano),
			Trigger:     string(r.Trigger.Kind),
			Applied:     r.Applied,
			Pending:     r.Pending,
		})
	}
	if recs == nil {
		recs = []jsonRec{}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(jsonReport{
		ReportedAt: report.ReportedAt.Format(time.RFC3339Nano),
		Records:    recs,
	})
}

func writeHistoryJSONDetail(w io.Writer, report service.HistoryDetailReport) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(report.Record)
}

func writeHistoryJSONError(w io.Writer, err error) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"error": err.Error()})
}
