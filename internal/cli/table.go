package cli

// Tabwriter text renderers (AC.5–AC.8): aligned, colored output using
// text/tabwriter with no external table dependency.
//
// Renderers never mutate their input reports: they read and format only. Each
// renderer takes a styler (from color.go) so the text is colored when the color
// policy allows it and plain otherwise.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// formatRFC3339 renders a time as RFC3339 in UTC, or empty when zero.
func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeStatusText renders a status report as aligned tab-separated text. It
// shows only provider quota/availability/mode/reason and quota windows — no
// routing chains, target tables, doctor findings, or running-session advisory.
func writeStatusText(w io.Writer, r service.StatusReport, s styler) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if r.AsOf.IsZero() {
		fmt.Fprintf(tw, "%s\n", s.dim("status"))
	} else {
		fmt.Fprintf(tw, "%s\t%s\n", s.dim("as of"), formatRFC3339(r.AsOf))
	}
	if r.Revision > 0 {
		fmt.Fprintf(tw, "%s\t%d\n", s.dim("revision"), r.Revision)
	}
	if len(r.Providers) > 0 {
		fmt.Fprintln(tw)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.dim("provider"), s.dim("quota"), s.dim("availability"), s.dim("mode"), s.dim("reason"))
		for _, p := range r.Providers {
			reason := p.Reason
			if reason == "" {
				reason = "normal"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				p.Provider,
				s.styleQuota(p.Quota),
				s.styleAvailability(p.Availability),
				s.styleMode(p.Mode),
				reason,
			)
		}
	}
	_ = tw.Flush()

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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	enabled := s.green("enabled")
	if !r.RoutingEnabled {
		enabled = s.red("disabled")
	}
	fmt.Fprintf(tw, "%s\t%s\n", s.dim("routing"), enabled)
	if !r.AsOf.IsZero() {
		fmt.Fprintf(tw, "%s\t%s\n", s.dim("as of"), formatRFC3339(r.AsOf))
	}
	fmt.Fprintln(tw)
	if len(r.Routes) > 0 {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.dim("target"), s.dim("route"), s.dim("effective"))
		for _, route := range r.Routes {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", route.TargetID, route.Name, strings.Join(route.Effective, ", "))
		}
	}
	_ = tw.Flush()

	for _, e := range r.Errors {
		fmt.Fprintf(w, "  %s %s\n", s.red("error:"), e.Summary)
	}
}

// writeRoutingExplainText renders the full ranking + desired/effective chains.
func writeRoutingExplainText(w io.Writer, r service.RoutingExplainReport, s styler) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	enabled := s.green("enabled")
	if !r.RoutingEnabled {
		enabled = s.red("disabled")
	}
	fmt.Fprintf(tw, "%s\t%s\n", s.dim("routing"), enabled)
	if !r.AsOf.IsZero() {
		fmt.Fprintf(tw, "%s\t%s\n", s.dim("as of"), formatRFC3339(r.AsOf))
	}
	fmt.Fprintln(tw)
	if len(r.Ranks) > 0 {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.dim("provider"), s.dim("rank"), s.dim("off_peak"), s.dim("eligible"), s.dim("reason"))
		for _, rank := range r.Ranks {
			fmt.Fprintf(tw, "%s\t%d\t%t\t%t\t%s\n",
				rank.MappingID, rank.Rank, rank.OffPeak, rank.Eligible, rank.Explanation)
		}
		fmt.Fprintln(tw)
	}
	if len(r.Routes) > 0 {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			s.dim("target"), s.dim("route"), s.dim("desired"), s.dim("effective"))
		for _, route := range r.Routes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				route.TargetID, route.Name, strings.Join(route.Desired, ", "), strings.Join(route.Effective, ", "))
		}
	}
	_ = tw.Flush()

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
	fmt.Fprintln(w, "This tool performs no automatic CodexBar edit; register it manually in")
	fmt.Fprintln(w, "CodexBar settings.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "To refresh an existing policy later, run: polytoken-quota init --force")
}
