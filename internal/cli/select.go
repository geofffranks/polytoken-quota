package cli

// select.go — the `select` and `select-eval` commands.
//
// Both commands hold no business logic: they parse flags (and, for select
// without an explicit difficulty, the bounded task on stdin), call one
// injected runner with typed values, render a safe fixed output, and map the
// outcome to a process exit code. Task text, credentials, and raw remote
// responses never reach either output; every free string is sanitized.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofffranks/polytoken-quota/internal/selection"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// --- select ---

// runSelect handles select --policy PATH --phase NAME [--difficulty TIER |
// --min-difficulty TIER] [--exclude-family NAME]... [--refresh] [--json].
// Without an explicit difficulty the task is read from stdin and disclosed to
// the configured assessment; --difficulty keeps the task fully local.
func runSelect(ctx context.Context, args []string, deps Dependencies, stdin io.Reader, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		writeCommandHelp(stdout, "select")
		return ExitOK
	}
	req, jsonOut, ok := parseSelectFlags(args)
	if !ok {
		return selectFatal(stdout, stderr, jsonOut, "select: invalid arguments")
	}
	if req.Tier != "" && req.MinTier != "" {
		return selectFatal(stdout, stderr, jsonOut, "select: --difficulty and --min-difficulty conflict")
	}
	if req.Tier != "" && !selection.ValidTier(req.Tier) {
		return selectFatal(stdout, stderr, jsonOut, fmt.Sprintf("select: unknown difficulty %q", req.Tier))
	}
	if req.MinTier != "" && !selection.ValidTier(req.MinTier) {
		return selectFatal(stdout, stderr, jsonOut, fmt.Sprintf("select: unknown difficulty floor %q", req.MinTier))
	}
	if req.PolicyPath == "" {
		return selectFatal(stdout, stderr, jsonOut, "select: --policy is required")
	}
	if req.Phase == "" {
		return selectFatal(stdout, stderr, jsonOut, "select: --phase is required")
	}
	// The explicit-tier path keeps the task local: stdin is never read.
	if req.Tier == "" {
		prompt, err := readSelectTask(stdin)
		if err != nil {
			return selectFatal(stdout, stderr, jsonOut, err.Error())
		}
		req.Prompt = prompt
	}
	if deps.Select == nil {
		return selectFatal(stdout, stderr, jsonOut, "select: selection is unavailable")
	}
	outcome, err := deps.Select.Run(ctx, req)
	if err != nil {
		return selectFatal(stdout, stderr, jsonOut, selectFatalMessage(err))
	}
	if jsonOut {
		encodeJSON(stdout, selectEnvelope(outcome, ""))
	} else {
		writeSelectText(stdout, outcome)
	}
	return selectExitCode(outcome.Status)
}

// parseSelectFlags parses select arguments. On a parse error ok is false but
// jsonOut is preserved so a rejected --json invocation still writes exactly
// one JSON object.
//
// Every value flag must carry a real value: a value flag at the end of the
// arguments, followed by another flag, or given an explicitly empty value is
// a parse error — never a silently empty string. For --difficulty and
// --min-difficulty that rule is the disclosure gate: SelectRequest cannot
// distinguish an empty explicit tier from "tier not given", and the latter
// reads stdin and discloses the task to remote assessment instead of the
// explicit local tier the operator asked for.
func parseSelectFlags(args []string) (req selection.SelectRequest, jsonOut, ok bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--refresh":
			req.RefreshFirst = true
			continue
		case "--json":
			jsonOut = true
			continue
		}
		name, value, found := flagValue(args, &i, arg)
		if !found {
			return req, jsonOut, false
		}
		switch name {
		case "--policy":
			req.PolicyPath = value
		case "--phase":
			req.Phase = value
		case "--difficulty":
			req.Tier = selection.Tier(value)
		case "--min-difficulty":
			req.MinTier = selection.Tier(value)
		case "--exclude-family":
			// Values are nonempty by construction (flagValue), so a
			// malformed exclusion can never silently narrow the review.
			req.ExcludedFamilies = append(req.ExcludedFamilies, value)
		default:
			return req, jsonOut, false
		}
	}
	return req, jsonOut, true
}

// flagValue splits one value-flag token into its flag name and value. The
// value may be attached ("--flag=value") or the next token ("--flag value");
// *i is advanced past a consumed token. found is false when arg is not a
// value flag, carries no value (trailing flag, or the next token is itself a
// flag), or carries an explicitly empty value: a value flag never degrades
// to a silently empty string, which for the difficulty flags would look
// exactly like "tier not given" downstream.
func flagValue(args []string, i *int, arg string) (name, value string, found bool) {
	for _, form := range [...]struct{ prefix, flag string }{
		{"--policy=", "--policy"},
		{"--phase=", "--phase"},
		{"--difficulty=", "--difficulty"},
		{"--min-difficulty=", "--min-difficulty"},
		{"--exclude-family=", "--exclude-family"},
		{"--fixtures=", "--fixtures"},
	} {
		if strings.HasPrefix(arg, form.prefix) {
			// "--flag=" with nothing attached is an explicit empty value.
			return form.flag, strings.TrimPrefix(arg, form.prefix), arg != form.prefix
		}
	}
	switch arg {
	case "--policy", "--phase", "--difficulty", "--min-difficulty", "--exclude-family", "--fixtures":
		if *i+1 >= len(args) || args[*i+1] == "" || strings.HasPrefix(args[*i+1], "-") {
			return arg, "", false
		}
		*i++
		return arg, args[*i], true
	default:
		return "", "", false
	}
}

// readSelectTask reads the bounded task text from stdin: nonempty, valid
// UTF-8, at most selection.MaxPromptBytes. Rejected tasks are not truncated.
func readSelectTask(stdin io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, selection.MaxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("select: read task: %w", err)
	}
	if len(raw) > selection.MaxPromptBytes {
		return "", fmt.Errorf("select: task exceeds the %d KiB limit; use explicit difficulty for large tasks", selection.MaxPromptBytes/1024)
	}
	prompt := string(raw)
	if len(prompt) == 0 {
		return "", fmt.Errorf("select: task is empty; provide a task on stdin or use --difficulty")
	}
	if !utf8.ValidString(prompt) {
		return "", fmt.Errorf("select: task is not valid UTF-8")
	}
	return prompt, nil
}

// selectExitCode maps a selection status to its process exit code: confirmed
// exits 0; uncertain, no_selection, and assessment_unavailable are accepted
// results requiring caller handling (exit 2); fatal failures are errors,
// never statuses (exit 1).
func selectExitCode(status selection.SelectStatus) int {
	switch status {
	case selection.SelectConfirmed:
		return ExitOK
	case selection.SelectUncertain, selection.SelectNoSelection, selection.SelectAssessmentUnavailable:
		return ExitPending
	default:
		return ExitRejected
	}
}

// selectFatal reports a fatal select failure: sanitized message on stderr,
// or the JSON error envelope on stdout under --json. Exit 1.
func selectFatal(stdout, stderr io.Writer, jsonOut bool, msg string) int {
	if jsonOut {
		encodeJSON(stdout, selectOutcomeJSON{Version: selectJSONVersion, Status: selectStatusError, Error: msg})
		return ExitRejected
	}
	fmt.Fprintln(stderr, msg)
	return ExitRejected
}

func sanitizeMessage(msg string) string {
	return validate.DefaultSanitize([]byte(msg))
}

// selectFatalMessage masks a fatal selection error for display: classified
// failures render their fixed safe sentence — never paths, configuration
// values, or raw causes. Anything unclassified collapses to a generic
// sentence.
func selectFatalMessage(err error) string {
	var fe *selection.FatalError
	if errors.As(err, &fe) {
		return fe.Error()
	}
	return "select: selection failed"
}

// evalFatalMessage masks a fatal select-eval error the same way.
func evalFatalMessage(err error) string {
	var fe *selection.FatalError
	if errors.As(err, &fe) {
		return fe.Error()
	}
	return "select-eval: evaluation failed"
}

// writeSelectText renders the human selection output. Uncertainty is always
// labeled; no prompt text, credential, or raw remote content can appear.
func writeSelectText(w io.Writer, o selection.SelectOutcome) {
	fmt.Fprintf(w, "status: %s\n", o.Status)
	switch o.Status {
	case selection.SelectUncertain:
		fmt.Fprintf(w, "evidence: %s\n", sanitizeMessage(o.Result.Evidence))
	case selection.SelectAssessmentUnavailable:
		if o.Abstained {
			fmt.Fprintln(w, "assessment: abstained")
		}
	}
	fmt.Fprintf(w, "phase: %s\n", sanitizeMessage(o.Phase))
	if o.Tier != "" {
		fmt.Fprintf(w, "tier: %s\n", sanitizeMessage(string(o.Tier)))
	}
	if o.AssessedTier != "" {
		fmt.Fprintf(w, "assessed_tier: %s\n", sanitizeMessage(string(o.AssessedTier)))
	}
	if o.Result.Reference != "" {
		fmt.Fprintf(w, "model: %s\n", sanitizeMessage(o.Result.Reference))
	}
	if o.Result.Mapping != "" {
		fmt.Fprintf(w, "mapping: %s\n", sanitizeMessage(o.Result.Mapping))
	}
	if o.Result.Headroom != nil {
		fmt.Fprintf(w, "headroom: %.0f%%\n", *o.Result.Headroom*100)
	}
	if !o.AsOf.IsZero() {
		fmt.Fprintf(w, "as_of: %s\n", o.AsOf.UTC().Format(time.RFC3339))
	}
}

// --- select-eval ---

// runSelectEval handles select-eval --policy PATH --fixtures PATH --live
// [--json]. --live is the explicit disclosure gate; without it the command
// does nothing remote and fails fast.
func runSelectEval(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		writeCommandHelp(stdout, "select-eval")
		return ExitOK
	}
	var policyPath, fixturesPath string
	live, jsonOut, ok := false, false, true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--live":
			live = true
			continue
		case "--json":
			jsonOut = true
			continue
		}
		name, value, found := flagValue(args, &i, arg)
		if !found {
			ok = false
			break
		}
		switch name {
		case "--policy":
			policyPath = value
		case "--fixtures":
			fixturesPath = value
		default:
			ok = false
		}
		if !ok {
			break
		}
	}
	if !ok || policyPath == "" || fixturesPath == "" {
		return evalFatal(stdout, stderr, jsonOut, "select-eval: invalid arguments")
	}
	if !live {
		return evalFatal(stdout, stderr, jsonOut, "select-eval: --live is required to run the evaluation")
	}
	if deps.SelectEval == nil {
		return evalFatal(stdout, stderr, jsonOut, "select-eval: evaluation is unavailable")
	}
	report, err := deps.SelectEval.RunEval(ctx, selection.EvalInvocation{
		PolicyPath:   policyPath,
		FixturesPath: fixturesPath,
	})
	if err != nil {
		return evalFatal(stdout, stderr, jsonOut, evalFatalMessage(err))
	}
	if jsonOut {
		encodeJSON(stdout, evalEnvelope(*report))
	} else {
		writeEvalText(stdout, report)
	}
	if report.Matches == report.Total && report.PolicyFailures == 0 {
		return ExitOK
	}
	return ExitPending
}

// evalFatal reports a fatal select-eval failure. Exit 1.
func evalFatal(stdout, stderr io.Writer, jsonOut bool, msg string) int {
	if jsonOut {
		encodeJSON(stdout, evalReportJSON{Version: selectJSONVersion, Error: msg})
		return ExitRejected
	}
	fmt.Fprintln(stderr, msg)
	return ExitRejected
}

// writeEvalText renders the human evaluation summary: aggregate counts and
// rates, then one safe line per mismatch or error. Matched cases are counted,
// not listed.
func writeEvalText(w io.Writer, r *selection.Report) {
	fmt.Fprintf(w, "model: %s\n", sanitizeMessage(r.Model))
	fmt.Fprintf(w, "rubric: %s\n", sanitizeMessage(r.Rubric))
	fmt.Fprintf(w, "cases: %d matched: %d errors: %d policy_rejected: %d\n",
		r.Total, r.Matches, r.Errors, r.PolicyFailures)
	fmt.Fprintf(w, "under: %d (%.1f%%) over: %d (%.1f%%) abstained: %d (%.1f%%)\n",
		r.UnderCount, r.UnderRate*100, r.OverCount, r.OverRate*100, r.AbstainedCount, r.AbstentionRate*100)
	fmt.Fprintf(w, "latency_ms: p50 %.0f p95 %.0f max %.0f\n",
		r.Latency.P50MS, r.Latency.P95MS, r.Latency.MaxMS)
	for i := range r.Records {
		rec := &r.Records[i]
		if rec.Match && !rec.PolicyRejected {
			continue
		}
		line := fmt.Sprintf("review id=%s phase=%s expected=%s actual=%s",
			sanitizeMessage(rec.ID), sanitizeMessage(rec.Phase),
			evalOutcomeText(rec.Expected), evalOutcomeText(rec.Actual))
		if rec.Err != "" {
			line += fmt.Sprintf(" error=%s", sanitizeMessage(rec.Err))
		}
		if rec.PolicyRejected {
			line += " policy_rejected=true"
		}
		fmt.Fprintln(w, line)
	}
}

// evalOutcomeText renders one classification as tier-or-abstention text.
func evalOutcomeText(o selection.Outcome) string {
	if o.Abstained {
		return selection.AbstentionOption
	}
	return string(o.Tier)
}
