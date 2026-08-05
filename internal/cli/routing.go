package cli

// routing and quota command dispatch: argument parsing, rendering, and exit-code
// mapping for the read-only (explain, status) and simple-mutation (enable,
// disable) commands. The commands hold no business logic; they call a single
// injected service method and render its sanitized report.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// runRouting dispatches the routing subcommands.
func runRouting(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "routing requires a subcommand: explain, enable, or disable")
		return ExitRejected
	}
	switch args[0] {
	case "explain":
		return runRoutingExplain(ctx, args[1:], deps, stdout, stderr)
	case "enable":
		return runRoutingToggle(ctx, args[1:], deps, true, stdout, stderr)
	case "disable":
		return runRoutingToggle(ctx, args[1:], deps, false, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown routing subcommand: %s\n", args[0])
		return ExitRejected
	}
}

// runRoutingExplain prints the routing ranking with explanations. --json emits
// structured JSON. It is read-only: it never mutates policy or state.
func runRoutingExplain(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "routing explain: invalid arguments")
		return ExitRejected
	}
	if deps.RankExplainer == nil {
		fmt.Fprintln(stderr, "routing explain: ranking explainer unavailable")
		return ExitRejected
	}
	report := deps.RankExplainer.RankingExplain(ctx)
	if report.Error != "" {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(report.Error)))
		return ExitRejected
	}
	writeRoutingExplain(stdout, report, jsonOut)
	return ExitOK
}

// writeRoutingExplain renders the ranking report as text or JSON.
func writeRoutingExplain(w io.Writer, r service.RankingReport, jsonOut bool) {
	if jsonOut {
		r.Advisory = RunningSessionAdvisory
		data, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintln(w, "{}")
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}
	state := "disabled"
	if r.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(w, "routing: %s\n", state)
	for _, e := range r.Entries {
		elig := "eligible"
		if !e.Eligible {
			elig = "ineligible"
		}
		fmt.Fprintf(w, "  %s: rank=%d off_peak=%t %s (%s)\n",
			e.MappingID, e.Rank, e.OffPeak, elig, e.Explanation)
	}
	fmt.Fprintln(w, RunningSessionAdvisory)
}

// runRoutingEnable toggles routing.enabled via the byte-preserving editor.
func runRoutingToggle(ctx context.Context, args []string, deps Dependencies, enabled bool, stdout, stderr io.Writer) int {
	_, ok := parseBoolFlags(args)
	if !ok {
		label := "enable"
		if !enabled {
			label = "disable"
		}
		fmt.Fprintf(stderr, "routing %s: invalid arguments\n", label)
		return ExitRejected
	}
	if deps.RoutingToggler == nil {
		label := "enable"
		if !enabled {
			label = "disable"
		}
		fmt.Fprintf(stderr, "routing %s: routing toggler unavailable\n", label)
		return ExitRejected
	}
	if err := deps.RoutingToggler.SetRoutingEnabled(ctx, enabled); err != nil {
		var writeErr *service.RoutingWriteError
		if !errors.As(err, &writeErr) || !writeErr.Mutated {
			fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(err.Error())))
			return ExitRejected
		}
		fmt.Fprintln(stderr, "routing change accepted with durability warning: "+validate.DefaultSanitize([]byte(err.Error())))
	}
	if enabled {
		fmt.Fprintln(stdout, "routing enabled")
	} else {
		fmt.Fprintln(stdout, "routing disabled")
	}
	fmt.Fprintln(stdout, "the next reconcile applies this setting")
	return ExitOK
}

// runQuota dispatches the quota subcommands.
func runQuota(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "quota requires a subcommand: status or check")
		return ExitRejected
	}
	switch args[0] {
	case "status":
		return runQuotaStatus(ctx, args[1:], deps, stdout, stderr)
	case "check":
		return runQuotaCheck(ctx, args[1:], deps, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown quota subcommand: %s\n", args[0])
		return ExitRejected
	}
}

// parseQuotaCheckFlags parses the quota check flags: --provider <id>, --json, and
// --reconcile. It returns the provider filter, whether JSON output is requested,
// whether reconcile is requested, and whether the flags were valid. An invalid
// flag or a --provider without a value yields ok=false.
func containsFlag(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func parseQuotaCheckFlags(args []string) (provider string, jsonOut, reconcile, ok bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			if i+1 >= len(args) {
				return "", jsonOut, reconcile, false
			}
			if strings.HasPrefix(args[i+1], "--") {
				return "", containsFlag(args[i+1:], "--json"), false, false
			}
			// An empty or whitespace-only ID is a caller mistake (e.g. an
			// unset shell variable), not an unfiltered check; reject it like
			// positional provider parsing does.
			if strings.TrimSpace(args[i+1]) == "" {
				return "", false, false, false
			}
			provider = args[i+1]
			i++
		case "--json":
			jsonOut = true
		case "--reconcile":
			reconcile = true
		default:
			return "", false, false, false
		}
	}
	return provider, jsonOut, reconcile, true
}

// runQuotaCheck polls provider quota adapters and prints sanitized diagnostics.
// It is a one-shot mutation: it calls the coordinator's QuotaCheck and maps the
// outcome to exit code 0 (clean), 2 (accepted with pending provider/target
// problems), or 1 (rejected, no mutation). It performs no daemon or process
// control. --json emits structured diagnostics; --provider filters to one
// mapping; --reconcile triggers the full stage/validate/publish flow.
func runQuotaCheck(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	provider, jsonOut, reconcile, ok := parseQuotaCheckFlags(args)
	if !ok {
		message := "quota check: invalid arguments"
		if containsFlag(args, "--json") {
			writeQuotaCheck(stdout, service.Outcome{Accepted: false, Error: errors.New(message)}, true)
		} else {
			fmt.Fprintln(stderr, message)
		}
		return ExitRejected
	}
	if deps.Mutator == nil {
		message := "quota check: mutator unavailable"
		if jsonOut {
			writeQuotaCheck(stdout, service.Outcome{Accepted: false, Error: errors.New(message)}, true)
		} else {
			fmt.Fprintln(stderr, message)
		}
		return ExitRejected
	}
	out := deps.Mutator.QuotaCheck(ctx, provider, reconcile)
	if out.Error != nil && !jsonOut {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	writeQuotaCheck(stdout, out, jsonOut)
	return MutationExitCode(out)
}

// writeQuotaCheck renders sanitized quota check diagnostics as text or JSON. It
// never prints credentials, auth headers, or raw response bodies — only
// sanitized status, revision, and per-provider summaries.
func writeQuotaCheck(w io.Writer, o service.Outcome, jsonOut bool) {
	if jsonOut {
		report := quotaCheckJSON{Accepted: o.Accepted, Revision: o.Revision, Problem: o.Problem, Advisory: RunningSessionAdvisory, Attempts: o.ProviderAttempts}
		if o.Error != nil {
			report.Error = validate.DefaultSanitize([]byte(o.Error.Error()))
		}
		for _, target := range o.Targets {
			d := quotaTargetDiagnostic{TargetID: validate.DefaultSanitize([]byte(target.TargetID)), Pending: target.Pending != nil}
			if target.Pending != nil {
				d.Stage = validate.DefaultSanitize([]byte(target.Pending.Stage))
			}
			report.Targets = append(report.Targets, d)
		}
		data, err := json.Marshal(report)
		if err != nil {
			fmt.Fprintln(w, "{}")
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}
	fmt.Fprintf(w, "quota check: accepted=%t revision=%d\n", o.Accepted, o.Revision)
	if o.Problem {
		fmt.Fprintln(w, "one or more providers have a pending problem")
	}
	for _, t := range o.Targets {
		if t.Pending != nil {
			fmt.Fprintf(w, "  target %s pending: stage=%s\n",
				validate.DefaultSanitize([]byte(t.TargetID)),
				validate.DefaultSanitize([]byte(t.Pending.Stage)))
		}
	}
	fmt.Fprintln(w, RunningSessionAdvisory)
}

// quotaCheckJSON is the structured diagnostic DTO for quota check --json.
type quotaCheckJSON struct {
	Accepted bool                             `json:"accepted"`
	Revision uint64                           `json:"revision"`
	Problem  bool                             `json:"problem"`
	Error    string                           `json:"error,omitempty"`
	Attempts []service.QuotaAttemptDiagnostic `json:"attempts,omitempty"`
	Targets  []quotaTargetDiagnostic          `json:"targets,omitempty"`
	Advisory string                           `json:"advisory"`
}

type quotaTargetDiagnostic struct {
	TargetID string `json:"target_id"`
	Pending  bool   `json:"pending"`
	Stage    string `json:"stage,omitempty"`
}

// runQuotaStatus prints sanitized per-provider quota snapshots, attempts, and
// routing metadata. --json emits structured JSON. Exit 2 when any provider has a
// pending problem; observations are still shown.
func runQuotaStatus(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "quota status: invalid arguments")
		return ExitRejected
	}
	if deps.QuotaStater == nil {
		fmt.Fprintln(stderr, "quota status: quota stater unavailable")
		return ExitRejected
	}
	report := deps.QuotaStater.QuotaStatus(ctx)
	if report.Error != "" {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(report.Error)))
		return ExitRejected
	}
	writeQuotaStatus(stdout, report, jsonOut)
	if report.Problem {
		return ExitPending
	}
	return ExitOK
}

// writeQuotaStatus renders the quota status report as text or JSON.
func writeQuotaStatus(w io.Writer, r service.QuotaStatusReport, jsonOut bool) {
	if jsonOut {
		r.Advisory = RunningSessionAdvisory
		data, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintln(w, "{}")
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}
	if r.Revision > 0 {
		fmt.Fprintf(w, "revision: %d\n", r.Revision)
	}
	for _, p := range r.Providers {
		fmt.Fprintf(w, "  %s:\n", p.MappingID)
		if p.Status != "" {
			fmt.Fprintf(w, "    snapshot: status=%s checked_at=%s\n", p.Status, formatTime(p.CheckedAt))
		}
		for _, win := range p.Windows {
			fmt.Fprintf(w, "    window %s: %s\n", win.Name, formatWindow(win))
		}
		if p.Attempt != nil {
			if p.Attempt.Error != "" {
				fmt.Fprintf(w, "    attempt: status=%s error=%s\n", p.Attempt.Status, quota.SanitizeText(p.Attempt.Error))
			} else {
				fmt.Fprintf(w, "    attempt: status=%s\n", p.Attempt.Status)
			}
		}
		if !p.LastDecisionAt.IsZero() {
			fmt.Fprintf(w, "    routing: last_rank=%d last_decision_at=%s\n", p.LastRank, formatTime(p.LastDecisionAt))
		}
	}
	if r.Problem {
		fmt.Fprintln(w, "one or more providers have a pending problem")
	}
	fmt.Fprintln(w, RunningSessionAdvisory)
}

// formatTime renders a time as RFC3339, or empty when zero.
func formatTime(t interface {
	IsZero() bool
	Format(string) string
}) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z07:00")
}

// formatWindow renders a sanitized window summary.
func formatWindow(w service.QuotaWindowReport) string {
	var parts []string
	if w.Used != nil {
		parts = append(parts, fmt.Sprintf("used=%g", *w.Used))
	}
	if w.Limit != nil {
		parts = append(parts, fmt.Sprintf("limit=%g", *w.Limit))
	}
	if w.UsagePercent != nil {
		parts = append(parts, fmt.Sprintf("usage=%g%%", *w.UsagePercent))
	}
	if w.Remaining != nil {
		parts = append(parts, fmt.Sprintf("remaining=%d%%", int(*w.Remaining*100)))
	}
	if len(parts) == 0 {
		return "no data"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}
