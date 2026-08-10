package cli

// Routing command dispatch (AC.6, AC.7): bare routing (effective chains only),
// routing explain (full ranks + desired/effective chains), and the mapping-atomic
// enable/disable/reset mutations. The read-only views come from the diagnostic
// snapshot's selector methods; the mutations call the Mutator.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// runRouting dispatches the routing subcommands.
func runRouting(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runRoutingBare(ctx, nil, deps, stdout, stderr)
	}
	switch args[0] {
	case "--json":
		return runRoutingBare(ctx, args, deps, stdout, stderr)
	case "explain":
		return runRoutingExplain(ctx, args[1:], deps, stdout, stderr)
	case "enable":
		return runRoutingMutate(ctx, args[1:], deps, true, stdout, stderr)
	case "disable":
		return runRoutingMutate(ctx, args[1:], deps, false, stdout, stderr)
	case "reset":
		return runRoutingReset(ctx, args[1:], deps, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown routing subcommand: %s\n", args[0])
		return ExitRejected
	}
}

// runRoutingBare handles `routing [--json]` — effective chains only.
func runRoutingBare(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "routing: invalid arguments")
		return ExitRejected
	}
	if deps.SnapshotBuilder == nil {
		fmt.Fprintln(stderr, "routing: snapshot builder unavailable")
		return ExitRejected
	}
	snapshot := deps.SnapshotBuilder.BuildDiagnosticSnapshot(ctx)
	report := snapshot.RoutingView()
	if report.Error != "" {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(report.Error)))
		return ExitRejected
	}
	s := newStyler(stdout, jsonOut)
	if jsonOut {
		encodeJSON(stdout, routingEnvelope(report))
	} else {
		writeRoutingText(stdout, report, s)
	}
	return routingExitCode(report.Error, report.Partial)
}

// runRoutingExplain handles `routing explain [--json]` — full ranks + chains.
func runRoutingExplain(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	jsonOut, ok := parseBoolFlags(args, "--json")
	if !ok {
		fmt.Fprintln(stderr, "routing explain: invalid arguments")
		return ExitRejected
	}
	if deps.SnapshotBuilder == nil {
		fmt.Fprintln(stderr, "routing explain: snapshot builder unavailable")
		return ExitRejected
	}
	snapshot := deps.SnapshotBuilder.BuildDiagnosticSnapshot(ctx)
	report := snapshot.RoutingExplainView()
	if report.Error != "" {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(report.Error)))
		return ExitRejected
	}
	s := newStyler(stdout, jsonOut)
	if jsonOut {
		encodeJSON(stdout, routingExplainEnvelope(report))
	} else {
		writeRoutingExplainText(stdout, report, s)
	}
	return routingExitCode(report.Error, report.Partial)
}

// runRoutingMutate handles `routing enable/disable <provider>`.
func runRoutingMutate(ctx context.Context, args []string, deps Dependencies, enabled bool, stdout, stderr io.Writer) int {
	provider, ok := parseRoutingToggleFlags(args)
	if !ok {
		label := "enable"
		if !enabled {
			label = "disable"
		}
		fmt.Fprintf(stderr, "routing %s: requires exactly one provider\n", label)
		return ExitRejected
	}
	if deps.Mutator == nil {
		label := "enable"
		if !enabled {
			label = "disable"
		}
		fmt.Fprintf(stderr, "routing %s: mutator unavailable\n", label)
		return ExitRejected
	}
	var out service.Outcome
	if enabled {
		out = deps.Mutator.Enable(ctx, provider)
	} else {
		out = deps.Mutator.Disable(ctx, provider)
	}
	if out.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	code := MutationExitCode(out)
	if code == ExitOK {
		if enabled {
			fmt.Fprintln(stdout, "routing enabled")
		} else {
			fmt.Fprintln(stdout, "routing disabled")
		}
	}
	return code
}

// runRoutingReset handles `routing reset`.
func runRoutingReset(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "routing reset: takes no arguments")
		return ExitRejected
	}
	if deps.Mutator == nil {
		fmt.Fprintln(stderr, "routing reset: mutator unavailable")
		return ExitRejected
	}
	out := deps.Mutator.Reset(ctx)
	if out.Error != nil {
		fmt.Fprintln(stderr, validate.DefaultSanitize([]byte(out.Error.Error())))
	}
	code := MutationExitCode(out)
	if code == ExitOK {
		fmt.Fprintln(stdout, "routing reset")
	}
	return code
}

// parseRoutingToggleFlags parses a single positional provider argument. No
// flags are accepted; exactly one non-empty, non-flag-like provider is required.
func parseRoutingToggleFlags(args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	if args[0] == "" || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	return args[0], true
}

// routingExitCode maps the routing report's error/partial state to an exit
// code: fatal error or partial (malformed definition with remaining routes)
// exits 1; a clean complete report exits 0.
func routingExitCode(reportError string, partial bool) int {
	if reportError != "" || partial {
		return ExitRejected
	}
	return ExitOK
}
