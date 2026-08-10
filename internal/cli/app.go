// Package cli implements the polytoken-quota diagnostic command tree, the typed
// adapters between CLI flags and the Mutator/Diagnoser/SnapshotBuilder
// interfaces, and the exit-code mapping. It holds no business logic and performs
// no daemon or process control: every command parses its arguments, calls a
// single injected method with typed values, and returns a process exit code.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
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
	InitWithOptions(context.Context, service.InitOptions) service.Outcome
	Reconcile(context.Context, bool, bool, bool) service.Outcome
	Disable(context.Context, string) service.Outcome
	Enable(context.Context, string) service.Outcome
	Reset(context.Context) service.Outcome
	QuotaCheck(context.Context, string, bool) service.Outcome
}

// DiagnosticCommand selects a read-only diagnostic command.
type DiagnosticCommand uint8

// Diagnostic commands.
const (
	StatusCommand DiagnosticCommand = iota
	DoctorCommand
)

// Dependencies are the injected collaborators Run dispatches to.
type Dependencies struct {
	Mutator         Mutator
	Diagnoser       service.Diagnoser
	SnapshotBuilder service.SnapshotBuilder
	Environment     func() map[string]string
}

// removedCommands are the former top-level commands that are now strictly
// rejected (exit 1, no mutation). They are checked before any dispatch.
var removedCommands = map[string]bool{
	"hook":  true,
	"sync":  true,
	"quota": true,
	"state": true,
}

// removedTopLevelToggles are the former top-level enable/disable/reset commands.
var removedTopLevelToggles = map[string]bool{
	"enable":  true,
	"disable": true,
	"reset":   true,
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

// Run parses args, dispatches to the injected collaborators, and returns the
// process exit code. Unknown or removed commands exit 1 and never invoke the
// mutator: all parsing and validation completes before any mutating call.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps Dependencies) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitRejected
	}
	if removedCommands[args[0]] || removedTopLevelToggles[args[0]] {
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		usage(stderr)
		return ExitRejected
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], deps, stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], deps, stdout, stderr)
	case "check":
		return runCheck(ctx, args[1:], deps, stdout, stderr)
	case "reconcile":
		return runReconcile(ctx, args[1:], deps, stdout, stderr)
	case "routing":
		return runRouting(ctx, args[1:], deps, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], deps, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		usage(stderr)
		return ExitRejected
	}
}

// --- init (AC.2) ---

// runInit handles init [--force].
func runInit(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	force, ok := parseInitFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "init: invalid arguments")
		return ExitRejected
	}
	if deps.Mutator == nil {
		fmt.Fprintln(stderr, "init: mutator unavailable")
		return ExitRejected
	}
	out := deps.Mutator.InitWithOptions(ctx, service.InitOptions{Force: force})
	if out.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	code := MutationExitCode(out)
	if code == ExitOK {
		writeInitText(stdout, force)
	}
	return code
}

func parseInitFlags(args []string) (force, ok bool) {
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		default:
			return false, false
		}
	}
	return force, true
}

// --- status (AC.5) ---

// runStatus handles status [--json].
func runStatus(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "status: invalid arguments")
		return ExitRejected
	}
	if deps.Diagnoser == nil {
		fmt.Fprintln(stderr, "status: diagnoser unavailable")
		return ExitRejected
	}
	report := deps.Diagnoser.Status(ctx, jsonOut)
	if report.Error != "" {
		if jsonOut {
			encodeJSON(stdout, statusEnvelope(report))
		} else {
			fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(report.Error)))
		}
		return ExitRejected
	}
	s := newStyler(stdout, jsonOut)
	if jsonOut {
		encodeJSON(stdout, statusEnvelope(report))
	} else {
		writeStatusText(stdout, report, s)
	}
	return DiagnosticExitCode(StatusCommand, report.Problem)
}

// --- check (promoted from quota check) ---

// runCheck handles check [--provider <id>] [--reconcile] [--json].
func runCheck(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	provider, jsonOut, reconcile, ok := parseCheckFlags(args)
	if !ok {
		emitMutationError(stdout, stderr, "check: invalid arguments", jsonOut)
		return ExitRejected
	}
	if deps.Mutator == nil {
		emitMutationError(stdout, stderr, "check: mutator unavailable", jsonOut)
		return ExitRejected
	}
	out := deps.Mutator.QuotaCheck(ctx, provider, reconcile)
	if out.Error != nil && !jsonOut {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	s := newStyler(stdout, jsonOut)
	if jsonOut {
		encodeJSON(stdout, mutationEnvelope(out))
	} else {
		writePendingTargets(out, stderr)
		writeMutationText(stdout, out, "check", s)
	}
	return MutationExitCode(out)
}

// parseCheckFlags parses check arguments. On a parse error ok is false, but the
// jsonOut/reconcile flags seen so far are preserved so the caller can still
// decide whether to emit a JSON error envelope (AC.9: every --json invocation
// writes exactly one JSON object, including rejected outcomes).
func parseCheckFlags(args []string) (provider string, jsonOut, reconcile, ok bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			if i+1 >= len(args) {
				return provider, jsonOut, reconcile, false
			}
			if strings.HasPrefix(args[i+1], "--") {
				return provider, jsonOut, reconcile, false
			}
			if strings.TrimSpace(args[i+1]) == "" {
				return provider, jsonOut, reconcile, false
			}
			provider = args[i+1]
			i++
		case "--json":
			jsonOut = true
		case "--reconcile":
			reconcile = true
		default:
			return provider, jsonOut, reconcile, false
		}
	}
	return provider, jsonOut, reconcile, true
}

func emitMutationError(stdout, stderr io.Writer, msg string, jsonOut bool) {
	if jsonOut {
		encodeJSON(stdout, mutationEnvelope(service.Outcome{Accepted: false, Error: errors.New(msg)}))
		return
	}
	fmt.Fprintln(stderr, msg)
}

// --- reconcile ---

// runReconcile handles reconcile [--dry-run] [--keep-staging] [--verbose].
func runReconcile(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	dryRun, keepStaging, verbose, ok := parseReconcileFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "reconcile: invalid arguments")
		return ExitRejected
	}
	if keepStaging && !dryRun {
		fmt.Fprintln(stderr, "reconcile: --keep-staging requires --dry-run")
		return ExitRejected
	}
	if deps.Mutator == nil {
		fmt.Fprintln(stderr, "reconcile: mutator unavailable")
		return ExitRejected
	}
	out := deps.Mutator.Reconcile(ctx, dryRun, keepStaging, verbose)
	if verbose {
		writeVerboseTrace(stdout, out)
	}
	if dryRun {
		return dryRunExitCode(out, stderr)
	}
	if out.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	return MutationExitCode(out)
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

func dryRunExitCode(o service.Outcome, stderr io.Writer) int {
	writePendingTargets(o, stderr)
	for _, target := range o.Targets {
		if target.StagingRoot != "" {
			fmt.Fprintf(stderr, "staged candidate retained at: %s\n", target.StagingRoot)
		}
	}
	if o.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(o.Error.Error())))
	}
	if !o.Accepted {
		return ExitRejected
	}
	return ExitOK
}

// writePendingTargets prints each pending target's stage/summary/remediation to
// stderr, sanitized via validate.DefaultSanitize. Shared by runCheck and
// runReconcile's dry-run path so both surfaces explain why an accepted outcome
// is still pending.
func writePendingTargets(o service.Outcome, stderr io.Writer) {
	for _, target := range o.Targets {
		if target.Pending != nil {
			fmt.Fprintf(stderr, "target %s pending: stage=%s summary=%q remediation=%s\n",
				validate.DefaultSanitize([]byte(target.TargetID)),
				validate.DefaultSanitize([]byte(target.Pending.Stage)),
				validate.DefaultSanitize([]byte(target.Pending.Summary)),
				validate.DefaultSanitize([]byte(target.Pending.Remediation)))
		}
	}
}

// --- doctor (AC.8) ---

// runDoctor handles doctor [--json].
func runDoctor(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "doctor: invalid arguments")
		return ExitRejected
	}
	if deps.Diagnoser == nil {
		fmt.Fprintln(stderr, "doctor: diagnoser unavailable")
		return ExitRejected
	}
	report := deps.Diagnoser.Doctor(ctx, jsonOut)
	s := newStyler(stdout, jsonOut)
	if jsonOut {
		encodeJSON(stdout, doctorEnvelope(report, time.Now().UTC()))
	} else {
		writeDoctorText(stdout, report, s)
	}
	return DiagnosticExitCode(DoctorCommand, report.Actionable())
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

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: polytoken-quota <command> [options]")
	fmt.Fprintln(w, "commands: init, status, check, reconcile, routing, doctor")
}
