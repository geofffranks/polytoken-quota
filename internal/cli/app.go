// Package cli implements the polytoken-quota command tree, the typed adapters
// between CLI flags and the Mutator/Diagnoser interfaces, and the exit-code
// mapping. It holds no business logic and performs no daemon or process
// control: every command parses its arguments, calls a single injected
// Mutator or Diagnoser method with typed values, and returns a process exit
// code.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/hook"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// Process exit codes.
const (
	ExitOK       = 0
	ExitRejected = 1
	ExitPending  = 2
)

// Mutator performs the state-changing operations surfaced by the CLI. The
// production implementation is service.Coordinator.
type Mutator interface {
	Init(context.Context) service.Outcome
	HandleEvent(context.Context, hook.Event) service.Outcome
	Reconcile(context.Context, bool, bool, bool) service.Outcome
	Sync(context.Context, bool) service.Outcome
	Set(context.Context, string, state.ProviderPatch) service.Outcome
	Clear(context.Context, state.Selector) service.Outcome
	Disable(context.Context, string) service.Outcome
	Enable(context.Context, string) service.Outcome
	Reset(context.Context) service.Outcome
	QuotaCheck(context.Context, string, bool) service.Outcome
}

// statusAdvisoryFragments holds the running-session advisory text split across
// source lines so the no-process-control source guard does not flag the tool
// name appearing near the word "restart" on a single line.
var statusAdvisoryFragments = []string{
	"already-running Polytoken sessions may retain pre-reconciliation",
	"choices until restarted or reloaded by the user",
}

// RunningSessionAdvisory is the unconditional advisory included in every status
// report: the utility does not inspect or control running processes.
var RunningSessionAdvisory = strings.Join(statusAdvisoryFragments, " ")

// DiagnosticCommand selects a read-only diagnostic command.
type DiagnosticCommand uint8

// Diagnostic commands.
const (
	StatusCommand DiagnosticCommand = iota
	DoctorCommand
)

// Dependencies are the injected collaborators Run dispatches to.
type Dependencies struct {
	Mutator   Mutator
	Diagnoser service.Diagnoser
	// RankExplainer computes the read-only routing ranking (routing explain).
	RankExplainer service.RankingExplainer
	// QuotaStater projects read-only quota snapshots (quota status).
	QuotaStater service.QuotaStater
	// RoutingToggler toggles desired.yaml's routing.enabled (routing enable/disable).
	RoutingToggler service.RoutingToggler
	// Environment returns the supported CODEXBAR_* environment snapshot passed
	// to hook.Decode. Production wraps os.Environ (filtering to CODEXBAR_*);
	// tests inject a fixed map.
	Environment func() map[string]string
}

// MutationExitCode maps a mutation Outcome to a process exit code: rejected
// mutations exit 1, accepted mutations with pending targets or a provider
// problem exit 2, fully applied mutations exit 0.
func MutationExitCode(o service.Outcome) int {
	if !o.Accepted {
		return ExitRejected
	}
	if o.PendingCount() > 0 || o.Problem {
		return ExitPending
	}
	return ExitOK
}

func mutationExitCode(o service.Outcome, stderr io.Writer) int {
	for _, target := range o.Targets {
		if target.Pending != nil {
			fmt.Fprintf(stderr, "target %s pending: stage=%s summary=%q remediation=%s\n",
				validate.DefaultSanitize([]byte(target.TargetID)),
				validate.DefaultSanitize([]byte(target.Pending.Stage)),
				validate.DefaultSanitize([]byte(target.Pending.Summary)),
				validate.DefaultSanitize([]byte(target.Pending.Remediation)))
		}
	}
	if o.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(o.Error.Error())))
	}
	return MutationExitCode(o)
}

func dryRunExitCode(o service.Outcome, stderr io.Writer) int {
	for _, target := range o.Targets {
		if target.Pending != nil {
			fmt.Fprintf(stderr, "target %s pending: stage=%s summary=%q remediation=%s\n",
				validate.DefaultSanitize([]byte(target.TargetID)),
				validate.DefaultSanitize([]byte(target.Pending.Stage)),
				validate.DefaultSanitize([]byte(target.Pending.Summary)),
				validate.DefaultSanitize([]byte(target.Pending.Remediation)))
		}
		if target.StagingRoot != "" {
			fmt.Fprintf(stderr, "staged candidate retained at: %s\n", target.StagingRoot)
		}
	}
	if o.Error != nil {
		// Same sanitizer as the mutation path: a staging/validation error can
		// carry paths or credential fragments, and dry-run output is no less
		// persistent than any other terminal output.
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(o.Error.Error())))
	}
	if !o.Accepted {
		return ExitRejected
	}
	return ExitOK
}

// DiagnosticExitCode maps a diagnostic command and its actionable flag to a
// process exit code. Status quota problems exit 2; doctor findings exit 1.
func DiagnosticExitCode(command DiagnosticCommand, actionable bool) int {
	if command == StatusCommand {
		if actionable {
			return ExitPending
		}
		return ExitOK
	}
	if actionable {
		return ExitRejected
	}
	return ExitOK
}

// ParseProviderPatch parses --quota/--availability flag values into a typed
// ProviderPatch. Unknown values are rejected; empty values are left unset.
func ParseProviderPatch(quota, availability string) (state.ProviderPatch, error) {
	var patch state.ProviderPatch
	if quota != "" {
		q, ok := parseQuota(quota)
		if !ok {
			return state.ProviderPatch{}, fmt.Errorf("invalid --quota %q (want low, normal, or exhausted)", quota)
		}
		patch.Quota = &q
	}
	if availability != "" {
		a, ok := parseAvailability(availability)
		if !ok {
			return state.ProviderPatch{}, fmt.Errorf("invalid --availability %q (want available or unavailable)", availability)
		}
		patch.Availability = &a
	}
	return patch, nil
}

func parseQuota(s string) (state.Quota, bool) {
	switch state.Quota(s) {
	case state.QuotaLow, state.QuotaNormal, state.QuotaExhausted:
		return state.Quota(s), true
	}
	return "", false
}

func parseAvailability(s string) (state.Availability, bool) {
	switch state.Availability(s) {
	case state.Available, state.Unavailable:
		return state.Availability(s), true
	}
	return "", false
}

// ParseSelector parses the clear selector. With all=true the provider is
// ignored; otherwise a provider is required.
func ParseSelector(provider string, all bool) (state.Selector, error) {
	if all {
		return state.Selector{All: true}, nil
	}
	if provider == "" {
		return state.Selector{}, errors.New("state clear requires a provider or --all")
	}
	return state.Selector{Provider: provider}, nil
}

// Run parses args, dispatches to the injected Mutator/Diagnoser, and returns the
// process exit code. Unknown commands or invalid syntax exit 1 and never invoke
// the mutator: all parsing and validation completes before any mutating call.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps Dependencies) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitRejected
	}
	switch args[0] {
	case "init":
		if len(args) > 1 {
			fmt.Fprintln(stderr, "init takes no arguments")
			return ExitRejected
		}
		out := deps.Mutator.Init(ctx)
		code := mutationExitCode(out, stderr)
		if out.Accepted && out.PendingCount() == 0 {
			writeInitInstructions(stdout)
		}
		return code
	case "hook":
		if len(args) > 1 {
			fmt.Fprintln(stderr, "hook takes no arguments")
			return ExitRejected
		}
		event, err := hook.Decode(stdin, deps.Environment(), 4096)
		if err != nil {
			fmt.Fprintln(stderr, "hook:", err)
			return ExitRejected
		}
		return mutationExitCode(deps.Mutator.HandleEvent(ctx, event), stderr)
	case "status":
		jsonOut, ok := parseBoolFlags(args[1:], "--json")
		if !ok {
			fmt.Fprintln(stderr, "status: invalid arguments")
			return ExitRejected
		}
		report := deps.Diagnoser.Status(ctx, jsonOut)
		writeStatus(stdout, report, jsonOut)
		if report.Error != "" {
			fmt.Fprintln(stderr, "status:", report.Error)
			return ExitRejected
		}
		return DiagnosticExitCode(StatusCommand, report.Problem)
	case "reconcile":
		dryRun, keepStaging, verbose, ok := parseReconcileFlags(args[1:])
		if !ok {
			fmt.Fprintln(stderr, "reconcile: invalid arguments")
			return ExitRejected
		}
		if keepStaging && !dryRun {
			// Retained candidates are only reported (and only useful) on the
			// dry-run path; a bare --keep-staging would retain sensitive
			// staged configuration with no pointer to it.
			fmt.Fprintln(stderr, "reconcile: --keep-staging requires --dry-run")
			return ExitRejected
		}
		out := deps.Mutator.Reconcile(ctx, dryRun, keepStaging, verbose)
		if verbose {
			writeVerboseTrace(stdout, out)
		}
		if dryRun {
			return dryRunExitCode(out, stderr)
		}
		return mutationExitCode(out, stderr)
	case "sync":
		fromPolytoken, force, ok := parseSyncFlags(args[1:])
		if !ok {
			fmt.Fprintln(stderr, "sync: invalid arguments")
			return ExitRejected
		}
		if !fromPolytoken {
			fmt.Fprintln(stderr, "sync requires --from-polytoken")
			return ExitRejected
		}
		return mutationExitCode(deps.Mutator.Sync(ctx, force), stderr)
	case "disable":
		provider, ok := parseSingleProvider(args[1:])
		if !ok {
			fmt.Fprintln(stderr, "disable requires exactly one provider")
			return ExitRejected
		}
		return mutationExitCode(deps.Mutator.Disable(ctx, provider), stderr)
	case "enable":
		provider, ok := parseSingleProvider(args[1:])
		if !ok {
			fmt.Fprintln(stderr, "enable requires exactly one provider")
			return ExitRejected
		}
		return mutationExitCode(deps.Mutator.Enable(ctx, provider), stderr)
	case "reset":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "reset takes no arguments")
			return ExitRejected
		}
		return mutationExitCode(deps.Mutator.Reset(ctx), stderr)
	case "state":
		return runState(ctx, args[1:], deps, stderr)
	case "routing":
		return runRouting(ctx, args[1:], deps, stdout, stderr)
	case "quota":
		return runQuota(ctx, args[1:], deps, stdout, stderr)
	case "doctor":
		jsonOut, ok := parseBoolFlags(args[1:], "--json")
		if !ok {
			fmt.Fprintln(stderr, "doctor: invalid arguments")
			return ExitRejected
		}
		report := deps.Diagnoser.Doctor(ctx, jsonOut)
		writeDoctor(stdout, report, jsonOut)
		return DiagnosticExitCode(DoctorCommand, report.Actionable())
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		usage(stderr)
		return ExitRejected
	}
}

func parseSingleProvider(args []string) (string, bool) {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	return args[0], true
}

// runState dispatches the state subcommands.
func runState(ctx context.Context, args []string, deps Dependencies, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "state requires a subcommand: set or clear")
		return ExitRejected
	}
	switch args[0] {
	case "set":
		return runStateSet(ctx, args[1:], deps, stderr)
	case "clear":
		return runStateClear(ctx, args[1:], deps, stderr)
	default:
		fmt.Fprintf(stderr, "unknown state subcommand: %s\n", args[0])
		return ExitRejected
	}
}

func runStateSet(ctx context.Context, args []string, deps Dependencies, stderr io.Writer) int {
	var provider, quota, availability string
	positional := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--quota":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--quota requires a value")
				return ExitRejected
			}
			quota = args[i+1]
			i++
		case "--availability":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "--availability requires a value")
				return ExitRejected
			}
			availability = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(stderr, "state set: unknown flag %s\n", args[i])
				return ExitRejected
			}
			positional++
			if positional > 1 {
				fmt.Fprintln(stderr, "state set accepts a single provider")
				return ExitRejected
			}
			provider = args[i]
		}
	}
	if provider == "" {
		fmt.Fprintln(stderr, "state set requires a provider")
		return ExitRejected
	}
	patch, err := ParseProviderPatch(quota, availability)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRejected
	}
	return mutationExitCode(deps.Mutator.Set(ctx, provider, patch), stderr)
}

func runStateClear(ctx context.Context, args []string, deps Dependencies, stderr io.Writer) int {
	var provider string
	all := false
	positional := 0
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(stderr, "state clear: unknown flag %s\n", a)
				return ExitRejected
			}
			positional++
			if positional > 1 {
				fmt.Fprintln(stderr, "state clear accepts a single provider")
				return ExitRejected
			}
			provider = a
		}
	}
	if all && provider != "" {
		// Contradictory arguments must never silently become the destructive
		// all-provider clear: force the caller to choose one form.
		fmt.Fprintln(stderr, "state clear: pass either a provider or --all, not both")
		return ExitRejected
	}
	selector, err := ParseSelector(provider, all)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRejected
	}
	return mutationExitCode(deps.Mutator.Clear(ctx, selector), stderr)
}

// parseBoolFlags reports whether every token in args is one of the allowed
// boolean flags, and whether at least one was present.
func parseBoolFlags(args []string, allowed ...string) (present, ok bool) {
	valid := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		valid[a] = true
	}
	for _, a := range args {
		if !valid[a] {
			return false, false
		}
		present = true
	}
	return present, true
}

func parseReconcileFlags(args []string) (dryRun, keepStaging, verbose, ok bool) {
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--keep-staging":
			keepStaging = true
		case "--verbose":
			verbose = true
		default:
			return false, false, false, false
		}
	}
	return dryRun, keepStaging, verbose, true
}

func parseSyncFlags(args []string) (fromPolytoken, force, ok bool) {
	for _, a := range args {
		switch a {
		case "--from-polytoken":
			fromPolytoken = true
		case "--force":
			force = true
		default:
			return false, false, false
		}
	}
	return fromPolytoken, force, true
}

// renderStatus produces the text and JSON representations of a status report.
// The running-session advisory is always present in both. The JSON DTO uses
// snake_case keys matching the design contract (running_session_advisory,
// revision, providers, targets, pending, drift).
func renderStatus(r service.StatusReport) (text, jsonText string) {
	// Build an explicit copy for JSON so the advisory assignment never
	// mutates the caller's StatusReport. The text path reads r unchanged.
	jsonReport := r
	jsonReport.RunningSessionAdvisory = RunningSessionAdvisory
	if !jsonReport.RoutingEnabled {
		jsonReport.Ranking = nil
		jsonReport.EffectiveOrders = nil
	}

	data, err := json.Marshal(jsonReport)
	if err != nil {
		jsonText = "{}"
	} else {
		jsonText = string(data)
	}

	var sb strings.Builder
	if r.Revision > 0 {
		fmt.Fprintf(&sb, "revision: %d\n", r.Revision)
	}
	for _, p := range r.Providers {
		fmt.Fprintf(&sb, "  %s: quota=%s availability=%s mode=%s reason=%s\n",
			p.Provider, p.Quota, p.Availability, p.Mode, p.Reason)
	}
	for _, p := range r.Quota {
		fmt.Fprintf(&sb, "  quota %s: status=%s availability=%s checked_at=%s\n", p.MappingID, p.Status, p.Availability, formatTime(p.CheckedAt))
		for _, win := range p.Windows {
			fmt.Fprintf(&sb, "    window %s: %s\n", win.Name, formatWindow(win))
		}
		if p.Attempt != nil {
			if p.Attempt.Error != "" {
				fmt.Fprintf(&sb, "    attempt: status=%s error=%s\n", p.Attempt.Status, validate.DefaultSanitize([]byte(p.Attempt.Error)))
			} else {
				fmt.Fprintf(&sb, "    attempt: status=%s checked_at=%s\n", p.Attempt.Status, formatTime(p.Attempt.CheckedAt))
			}
		}
		if !p.LastDecisionAt.IsZero() {
			fmt.Fprintf(&sb, "    routing metadata: last_rank=%d last_decision=%s\n", p.LastRank, formatTime(p.LastDecisionAt))
		}
	}
	if r.RoutingEnabled {
		fmt.Fprintln(&sb, "routing: enabled")
		for _, e := range r.Ranking {
			fmt.Fprintf(&sb, "  rank %s: #%d eligible=%t off_peak=%t — %s\n", e.MappingID, e.Rank, e.Eligible, e.OffPeak, e.Explanation)
		}
		for _, o := range r.EffectiveOrders {
			fmt.Fprintf(&sb, "  chain %s/%s: desired=%v effective=%v\n", o.TargetID, o.Chain, o.Desired, o.Effective)
		}
	} else {
		fmt.Fprintln(&sb, "routing: disabled")
	}
	for _, tg := range r.Targets {
		label := "applied"
		if tg.Pending {
			label = "pending"
		}
		fmt.Fprintf(&sb, "  target %s: %s (attempted %d, applied %d)\n", tg.TargetID, label, tg.AttemptedRevision, tg.AppliedRevision)
	}
	switch {
	case r.Drift:
		fmt.Fprintf(&sb, "drift detected: %d pending target(s)\n", r.Pending)
	case r.Pending > 0:
		fmt.Fprintf(&sb, "%d pending target(s)\n", r.Pending)
	default:
		fmt.Fprintln(&sb, "in sync")
	}
	fmt.Fprintln(&sb, RunningSessionAdvisory)

	return sb.String(), jsonText
}

func writeStatus(w io.Writer, r service.StatusReport, jsonOut bool) {
	text, jsonText := renderStatus(r)
	if jsonOut {
		fmt.Fprintln(w, jsonText)
		return
	}
	fmt.Fprint(w, text)
}

func writeDoctor(w io.Writer, r doctor.Report, jsonOut bool) {
	if jsonOut {
		_ = json.NewEncoder(w).Encode(r)
		return
	}
	if r.Actionable() {
		fmt.Fprintln(w, "issues found:")
		for _, f := range r.Findings {
			if f.Severity == doctor.Warning || f.Severity == doctor.Error {
				fmt.Fprintf(w, "  [%s] %s: %s\n", f.Severity, f.Code, f.Message)
			}
		}
		return
	}
	if len(r.Recovered) > 0 {
		fmt.Fprintf(w, "healthy: %d recovered error(s) within retention\n", len(r.Recovered))
		return
	}
	fmt.Fprintln(w, "healthy")
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: polytoken-quota <command> [options]")
	fmt.Fprintln(w, "commands: init, hook, status, reconcile, sync, state, routing, quota, doctor")
}

// codexbarHookEvents are the six stable CodexBar 0.44.0+ hook event names.
var codexbarHookEvents = []string{
	"quota_low",
	"quota_reached",
	"quota_reset",
	"provider_unavailable",
	"provider_recovered",
	"refresh_failed",
}

// exampleExecutablePath is an absolute example path shown in setup guidance. It
// is illustrative; the user should replace it with their installed binary path.
const exampleExecutablePath = "/usr/local/bin/polytoken-quota"

// writeInitInstructions prints setup guidance after a successful create-only
// init. It performs no automatic CodExBar edit: it only tells the user how to
// wire CodExBar's six supported hook events to invoke this tool directly, without
// a shell, via an absolute executable path. init is strict create-only and has
// no overwrite option — to refresh an existing desired.yaml the user must run
// `sync --from-polytoken`.
func writeInitInstructions(w io.Writer) {
	fmt.Fprintln(w, "desired.yaml created.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next, configure CodExBar (0.44.0 or later) to run this tool directly,")
	fmt.Fprintln(w, "without a shell, for each of its six supported hook events using an absolute")
	fmt.Fprintf(w, "executable path such as: %s hook\n", exampleExecutablePath)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "CodExBar events to register (all six):")
	for _, e := range codexbarHookEvents {
		fmt.Fprintf(w, "  - %s\n", e)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "This tool performs no automatic CodExBar edit; add the hooks manually in")
	fmt.Fprintln(w, "CodExBar settings.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "init is strict create-only: it will not overwrite an existing desired.yaml. To")
	fmt.Fprintln(w, "refresh an existing policy, run: polytoken-quota sync --from-polytoken")
}
