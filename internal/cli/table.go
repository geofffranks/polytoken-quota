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

// writeMergedStatusText renders the merged status report: a header line with
// routing enablement and one global last-checked timestamp, a provider table
// (consolidated STATUS, raw window numbers, next reset), a route table with
// synthesized skip reasons, and a pending-config warning. It reads and
// formats only; it never mutates the report.
func writeMergedStatusText(w io.Writer, r service.MergedStatusReport, s styler) {
	enabledText, enabledStyle := "enabled", s.green
	if !r.RoutingEnabled {
		enabledText, enabledStyle = "disabled", s.red
	}
	checked := "never"
	if !r.LastChecked.IsZero() {
		checked = r.LastChecked.UTC().Format("2006-01-02 15:04 UTC")
	}
	fmt.Fprintf(w, "%s %s    %s %s\n", s.dim("routing:"), enabledStyle(enabledText), s.dim("last checked:"), checked)

	if len(r.Providers) > 0 {
		fmt.Fprintln(w)
		rows := [][]tableCell{{
			{text: "PROVIDER", style: s.dim}, {text: "STATUS", style: s.dim},
			{text: "QUOTA", style: s.dim}, {text: "NEXT RESET", style: s.dim},
		}}
		for _, p := range r.Providers {
			quota, quotaStyle := formatMergedWindows(p.Windows, s)
			rows = append(rows, []tableCell{
				{text: p.Provider},
				{text: p.Status, style: s.mergedStatusStyler(p.Status)},
				{text: quota, style: quotaStyle},
				{text: formatMergedReset(p.NextResetAt)},
			})
		}
		writeTable(w, rows)
	}

	if len(r.Routes) > 0 {
		fmt.Fprintln(w)
		rows := [][]tableCell{{
			{text: "ROUTE", style: s.dim}, {text: "DESIRED", style: s.dim},
			{text: "EFFECTIVE", style: s.dim}, {text: "REASON", style: s.dim},
		}}
		for _, route := range r.Routes {
			reason := formatSkipReasons(route.Skipped)
			if route.ProjectionError {
				reason = "projection unavailable"
			}
			rows = append(rows, []tableCell{
				{text: route.Name},
				{text: topModel(route.Desired)},
				{text: topModel(route.Effective)},
				{text: reason, style: s.dim},
			})
		}
		writeTable(w, rows)
	}

	for _, e := range r.Errors {
		fmt.Fprintf(w, "  %s %s\n", s.red("error:"), e.Summary)
	}

	if len(r.PendingTargets) > 0 {
		fmt.Fprintf(w, "%s\n", s.yellow(fmt.Sprintf(
			"warning: %d target(s) pending — shown values may not be live; run polytoken-quota doctor",
			len(r.PendingTargets))))
	}
}

// formatMergedWindows renders one provider's raw quota numbers: "name used/limit"
// per window joined with ", ", falling back to usage percent, then "no data".
func formatMergedWindows(windows []service.QuotaWindowReport, s styler) (string, func(string) string) {
	if len(windows) == 0 {
		return "no data", s.dim
	}
	parts := make([]string, 0, len(windows))
	for _, win := range windows {
		switch {
		case win.Used != nil && win.Limit != nil:
			parts = append(parts, fmt.Sprintf("%s %g/%g", win.Name, *win.Used, *win.Limit))
		case win.UsagePercent != nil:
			parts = append(parts, fmt.Sprintf("%s %g%%", win.Name, *win.UsagePercent))
		default:
			parts = append(parts, win.Name)
		}
	}
	return strings.Join(parts, ", "), nil
}

// formatMergedReset renders the earliest upcoming reset, or an em dash when
// unknown.
func formatMergedReset(reset *time.Time) string {
	if reset == nil {
		return "—"
	}
	return reset.UTC().Format("2006-01-02 15:04 UTC")
}

// chainText renders a model chain, or "none" for an empty chain.
func chainText(chain []string) string {
	if len(chain) == 0 {
		return "none"
	}
	return strings.Join(chain, ", ")
}

// topModel renders only the first model in a route for the compact human table.
// The complete chain remains available through the report and status JSON.
func topModel(chain []string) string {
	if len(chain) == 0 {
		return "none"
	}
	return chain[0]
}

// formatSkipReasons renders skipped models as "model skipped: reason" joined
// with "; ".
func formatSkipReasons(skipped []service.SkippedModel) string {
	if len(skipped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(skipped))
	for _, s := range skipped {
		parts = append(parts, fmt.Sprintf("%s skipped: %s", s.Model, s.Reason))
	}
	return strings.Join(parts, "; ")
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
