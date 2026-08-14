package cli

// Text renderers (AC.5–AC.8): aligned, colored output using a small
// display-width-aware table helper with no external table dependency.
//
// Renderers never mutate their input reports: they read and format only. Each
// renderer takes a styler (from color.go) so the text is colored when the color
// policy allows it and plain otherwise.

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
	"github.com/mattn/go-runewidth"
)

type tableCell struct {
	text  string
	style func(string) string
}

func displayWidth(text string) int {
	return runewidth.StringWidth(text)
}

func terminalSafe(text string) string {
	var safe strings.Builder
	for _, r := range text {
		switch r {
		case '\n':
			safe.WriteString(`\n`)
		case '\r':
			safe.WriteString(`\r`)
		case '\t':
			safe.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r >= 0x7f && r <= 0x9f:
				fmt.Fprintf(&safe, `\x%02x`, r)
			case unicode.Is(unicode.Cf, r) && r != '\u200c' && r != '\u200d':
				quoted := strconv.QuoteRuneToASCII(r)
				safe.WriteString(quoted[1 : len(quoted)-1])
			default:
				safe.WriteRune(r)
			}
		}
	}
	return safe.String()
}

func writeTable(w io.Writer, rows [][]tableCell) {
	if len(rows) == 0 {
		return
	}
	safeRows := make([][]tableCell, len(rows))
	for row := range rows {
		safeRows[row] = append([]tableCell(nil), rows[row]...)
		for column := range safeRows[row] {
			safeRows[row][column].text = terminalSafe(safeRows[row][column].text)
		}
	}
	rows = safeRows
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for column, cell := range row {
			if width := displayWidth(cell.text); column < len(widths) && width > widths[column] {
				widths[column] = width
			}
		}
	}
	for _, row := range rows {
		for column, cell := range row {
			text := cell.text
			if column < len(row)-1 {
				text += strings.Repeat(" ", widths[column]-displayWidth(cell.text)+2)
			}
			if cell.style != nil {
				text = cell.style(text)
			}
			fmt.Fprint(w, text)
		}
		fmt.Fprintln(w)
	}
}

// formatRFC3339 renders a time as RFC3339 in UTC, or empty when zero.
func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeStatusText renders a status report as aligned columnar text. It
// shows only provider quota/availability/mode/reason and quota windows — no
// routing chains, target tables, or doctor findings.
func writeStatusText(w io.Writer, r service.StatusReport, s styler) {
	if r.AsOf.IsZero() {
		fmt.Fprintln(w, s.dim("status"))
	} else {
		writeTable(w, [][]tableCell{{{text: "as of", style: s.dim}, {text: formatRFC3339(r.AsOf)}}})
	}
	if r.Revision > 0 {
		writeTable(w, [][]tableCell{{{text: "revision", style: s.dim}, {text: fmt.Sprint(r.Revision)}}})
	}
	if len(r.Providers) > 0 {
		fmt.Fprintln(w)
		rows := [][]tableCell{{
			{text: "provider", style: s.dim}, {text: "quota", style: s.dim}, {text: "availability", style: s.dim},
			{text: "mode", style: s.dim}, {text: "reason", style: s.dim},
		}}
		for _, p := range r.Providers {
			reason := p.Reason
			if reason == "" {
				reason = "normal"
			}
			rows = append(rows, []tableCell{
				{text: p.Provider}, {text: string(p.Quota), style: s.quotaStyler(p.Quota)},
				{text: string(p.Availability), style: s.availabilityStyler(p.Availability)},
				{text: string(p.Mode), style: s.modeStyler(p.Mode)}, {text: reason},
			})
		}
		writeTable(w, rows)
	}

	for _, q := range r.Quota {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s %s\n", s.dim("quota"), q.MappingID)
		if !q.CheckedAt.IsZero() {
			fmt.Fprintf(w, "  %s %s\n", s.dim("snapshot:"), fmt.Sprintf("status=%s checked_at=%s", q.Status, formatRFC3339(q.CheckedAt)))
		}
		for _, win := range q.Windows {
			fmt.Fprintf(w, "  %s %s: %s\n", s.dim("window"), win.Name, formatWindow(win))
		}
		if q.Attempt != nil {
			if q.Attempt.Error != "" {
				fmt.Fprintf(w, "  %s status=%s error=%s\n", s.dim("attempt:"), q.Attempt.Status, validate.DefaultSanitize([]byte(q.Attempt.Error)))
			} else if !q.Attempt.CheckedAt.IsZero() {
				fmt.Fprintf(w, "  %s status=%s checked_at=%s\n", s.dim("attempt:"), q.Attempt.Status, formatRFC3339(q.Attempt.CheckedAt))
			}
		}
	}
}

// writeRoutingText renders bare routing (effective chains only) as aligned text.
func writeRoutingText(w io.Writer, r service.RoutingReport, s styler) {
	enabledText, enabledStyle := "enabled", s.green
	if !r.RoutingEnabled {
		enabledText, enabledStyle = "disabled", s.red
	}
	writeTable(w, [][]tableCell{{{text: "routing", style: s.dim}, {text: enabledText, style: enabledStyle}}})
	if !r.AsOf.IsZero() {
		writeTable(w, [][]tableCell{{{text: "as of", style: s.dim}, {text: formatRFC3339(r.AsOf)}}})
	}
	fmt.Fprintln(w)
	if len(r.Routes) > 0 {
		rows := [][]tableCell{{{text: "target", style: s.dim}, {text: "source", style: s.dim}, {text: "route", style: s.dim}, {text: "effective", style: s.dim}}}
		for _, route := range r.Routes {
			rows = append(rows, []tableCell{{text: route.TargetID}, {text: route.SourcePath}, {text: route.Name}, {text: strings.Join(route.Effective, ", ")}})
		}
		writeTable(w, rows)
	}

	for _, e := range r.Errors {
		fmt.Fprintf(w, "  %s %s\n", s.red("error:"), e.Summary)
	}
}

// writeRoutingExplainText renders compact ranking status and selected route models.
func writeRoutingExplainText(w io.Writer, r service.RoutingExplainReport, s styler) {
	enabledText, enabledStyle := "enabled", s.green
	if !r.RoutingEnabled {
		enabledText, enabledStyle = "disabled", s.red
	}
	writeTable(w, [][]tableCell{{{text: "routing", style: s.dim}, {text: enabledText, style: enabledStyle}}})
	if !r.AsOf.IsZero() {
		writeTable(w, [][]tableCell{{{text: "as of", style: s.dim}, {text: formatRFC3339(r.AsOf)}}})
	}
	if len(r.PendingTargets) > 0 {
		fmt.Fprintf(w, "%s\n", s.yellow("warning: "+routingPendingWarning(r.PendingTargets)))
	}
	fmt.Fprintln(w)
	if len(r.Ranks) > 0 {
		rows := [][]tableCell{{
			{text: "provider", style: s.dim}, {text: "status", style: s.dim}, {text: "reason", style: s.dim},
		}}
		for _, rank := range r.Ranks {
			status := rank.Status
			if status == "" {
				status = "not ready"
				if rank.Eligible {
					status = "ready"
				}
			}
			rows = append(rows, []tableCell{{text: rank.MappingID}, {text: status}, {text: rank.Explanation}})
		}
		writeTable(w, rows)
		fmt.Fprintln(w)
	}
	if len(r.Routes) > 0 {
		rows := [][]tableCell{{
			{text: "route", style: s.dim}, {text: "desired", style: s.dim}, {text: "effective", style: s.dim},
		}}
		for _, route := range r.Routes {
			rows = append(rows, []tableCell{{text: route.Name}, {text: route.Desired}, {text: route.Effective}})
		}
		writeTable(w, rows)
	}

	for _, e := range r.Errors {
		fmt.Fprintf(w, "  %s %s\n", s.red("error:"), e.Summary)
	}
}

// writeDoctorText renders the doctor report grouped and sorted by severity.
// When there are no warning/error findings it prints a healthy summary.
func writeDoctorText(w io.Writer, r doctor.Report, s styler) {
	if !r.Actionable() {
		if len(r.Recovered) > 0 {
			fmt.Fprintf(w, "%s: %d %s\n", s.green("healthy"), len(r.Recovered), s.dim("recovered error(s) within retention"))
		} else {
			fmt.Fprintln(w, s.green("healthy"))
		}
		return
	}

	// Group and sort findings: errors first, then warnings, then info.
	sorted := append([]doctor.Finding(nil), r.Findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return severityRank(sorted[i].Severity) < severityRank(sorted[j].Severity)
	})

	for _, f := range sorted {
		if f.Severity != doctor.Warning && f.Severity != doctor.Error {
			continue
		}
		fmt.Fprintf(w, "[%s]\n", s.severity(f.Severity))
		fmt.Fprintf(w, "  %s\t%s\n", s.dim("code"), f.Code)
		fmt.Fprintf(w, "  %s\t%s\n", s.dim("message"), f.Message)
		if f.TargetID != "" {
			fmt.Fprintf(w, "  %s\t%s\n", s.dim("target"), f.TargetID)
		}
		if f.File != "" {
			fmt.Fprintf(w, "  %s\t%s\n", s.dim("file"), f.File)
		}
		if f.Chain != "" {
			fmt.Fprintf(w, "  %s\t%s\n", s.dim("chain"), f.Chain)
		}
		if f.Remediation != "" {
			fmt.Fprintf(w, "  %s\t%s\n", s.dim("remediation"), f.Remediation)
		}
		fmt.Fprintln(w)
	}
	if len(r.Recovered) > 0 {
		fmt.Fprintf(w, "%s: %d %s\n", s.dim("recovered"), len(r.Recovered), s.dim("recovered error(s) within retention"))
	}
}

// severityRank orders Error before Warning before Info for sorted display.
func severityRank(sev doctor.Severity) int {
	switch sev {
	case doctor.Error:
		return 0
	case doctor.Warning:
		return 1
	default:
		return 2
	}
}

// writeMutationText renders a mutation outcome as text.
func writeMutationText(w io.Writer, o service.Outcome, label string, s styler) {
	if o.Error != nil {
		fmt.Fprintln(w, validate.DefaultSanitize([]byte(o.Error.Error())))
		return
	}
	fmt.Fprintf(w, "%s: %s revision=%d\n", label, s.green("accepted"), o.Revision)
	if label != "check" || len(o.ProviderAttempts) == 0 {
		return
	}
	fmt.Fprintln(w)
	rows := [][]tableCell{{
		{text: "mapping", style: s.dim},
		{text: "status", style: s.dim},
		{text: "error", style: s.dim},
	}}
	for _, attempt := range o.ProviderAttempts {
		statusStyle := s.green
		if attempt.Status != "fresh" && attempt.Status != "partial" {
			statusStyle = s.red
		}
		rows = append(rows, []tableCell{
			{text: attempt.MappingID},
			{text: attempt.Status, style: statusStyle},
			{text: attempt.Error},
		})
	}
	writeTable(w, rows)
}

// formatWindow renders a sanitized window summary (shared with old code).
func formatWindow(win service.QuotaWindowReport) string {
	var parts []string
	if win.Used != nil {
		parts = append(parts, fmt.Sprintf("used=%g", *win.Used))
	}
	if win.Limit != nil {
		parts = append(parts, fmt.Sprintf("limit=%g", *win.Limit))
	}
	if win.UsagePercent != nil {
		parts = append(parts, fmt.Sprintf("usage=%g%%", *win.UsagePercent))
	}
	if win.Remaining != nil {
		parts = append(parts, fmt.Sprintf("remaining=%d%%", int(*win.Remaining*100)))
	}
	if len(parts) == 0 {
		return "no data"
	}
	return strings.Join(parts, " ")
}

// writeInitText prints the post-init guidance after a successful create or
// forced import. No sync/hook references remain: just init --force.
func writeInitText(w io.Writer, forced bool) {
	if forced {
		fmt.Fprintln(w, "desired.yaml updated.")
	} else {
		fmt.Fprintln(w, "desired.yaml created.")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Review the generated policy, then run: polytoken-quota reconcile")
	fmt.Fprintln(w, "To refresh an existing policy later, run: polytoken-quota init --force")
}
