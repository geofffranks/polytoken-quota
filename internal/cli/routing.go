package cli

// Routing command dispatch: the mapping-atomic enable/disable/reset mutations.
// The routing display surfaces (bare routing and routing explain) were removed
// — their data now lives in `polytoken-quota status`, so those forms fail
// strictly like any unknown subcommand.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// runRouting dispatches the routing subcommands. Only the mutations remain;
// bare routing, --json, and explain are rejected without invoking any
// dependency.
func runRouting(ctx context.Context, args []string, deps Dependencies, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		writeCommandHelp(stdout, "routing")
		return ExitOK
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "routing: requires a subcommand: enable, disable, reset (view routing with `polytoken-quota status`)")
		return ExitRejected
	}
	switch args[0] {
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

// runRoutingMutate handles `routing enable/disable <provider>`.
func runRoutingMutate(ctx context.Context, args []string, deps Dependencies, enabled bool, stdout, stderr io.Writer) int {
	if hasHelpFlag(args) {
		if enabled {
			writeCommandHelp(stdout, "routing enable")
		} else {
			writeCommandHelp(stdout, "routing disable")
		}
		return ExitOK
	}
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
	if hasHelpFlag(args) {
		writeCommandHelp(stdout, "routing reset")
		return ExitOK
	}
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
