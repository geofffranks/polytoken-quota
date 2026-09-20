package cli

// help.go — the polytoken-quota help subsystem.
//
// Three help surfaces:
//   - Root help:      polytoken-quota help | --help | -h
//   - Command help:   polytoken-quota help <cmd> | <cmd> --help
//   - Subcommand help: polytoken-quota help routing <sub> | routing <sub> --help
//
// Explicit help requests (--help, -h, help) print to stdout and exit 0.
// Error-triggered usage (no args, unknown command) prints to stderr and exits 1.

import (
	"fmt"
	"io"
)

// flagDoc describes a flag or positional argument for help rendering.
type flagDoc struct {
	name string
	desc string
}

// subcommandDoc describes a subcommand for help listing.
type subcommandDoc struct {
	name  string
	short string
}

// helpDoc is the help content for a command or subcommand.
type helpDoc struct {
	short       string // one-line summary for listings
	long        string // description paragraph
	usage       []string
	args        []flagDoc
	flags       []flagDoc
	subcommands []subcommandDoc
}

// commandOrder is the display order for top-level commands in root help.
var commandOrder = []string{
	"init", "status", "check", "reconcile", "routing", "doctor", "history", "select", "select-eval", "notice-hook", "install-hook",
}

// helpDocs maps each command path to its help content. Top-level commands use
// their bare name ("init"); routing subcommands use "routing <sub>".
var helpDocs = map[string]helpDoc{
	"init": {
		short: "Initialize quota state from current configuration",
		long:  "Initialize quota state from the current Polytoken configuration.",
		usage: []string{"polytoken-quota init [--force]"},
		flags: []flagDoc{
			{"--force", "Overwrite existing state without confirmation; operator selection consent (selection.jev) is preserved, not reset"},
		},
	},
	"status": {
		short: "Show current quota and routing status",
		long:  "Show provider quota and availability, effective routing with skip reasons, next resets, and pending-config warnings.",
		usage: []string{"polytoken-quota status [--json]"},
		flags: []flagDoc{
			{"--json", "Output JSON"},
		},
	},
	"check": {
		short: "Check provider quota availability",
		long:  "Check current provider quota availability.",
		usage: []string{"polytoken-quota check [--provider <id>] [--reconcile] [--json] [--quiet]"},
		flags: []flagDoc{
			{"--provider <id>", "Check a specific provider by mapping ID"},
			{"--reconcile", "Attempt reconciliation after checking"},
			{"--json", "Output JSON"},
			{"--quiet", "Suppress output (exit code only)"},
		},
	},
	"reconcile": {
		short: "Apply desired quota state to live configuration",
		long:  "Apply desired quota state to the live Polytoken configuration. Without --verbose reconcile is silent: output is the exit code only (usage and flag errors still print to stderr).",
		usage: []string{"polytoken-quota reconcile [--dry-run] [--keep-staging] [--verbose]"},
		flags: []flagDoc{
			{"--dry-run", "Preview changes without applying"},
			{"--keep-staging", "Retain staged candidates (requires --dry-run)"},
			{"--verbose", "Print full sanitized per-target diagnostics (external validation output capped at 256 KiB, never un-redacted); retained staging path prints only here"},
		},
	},
	"routing": {
		short: "Modify routing configuration",
		long:  "Modify routing configuration. View routing state with `polytoken-quota status`.",
		usage: []string{
			"polytoken-quota routing <subcommand> [options]",
		},
		subcommands: []subcommandDoc{
			{"enable", "Enable routing for a provider"},
			{"disable", "Disable routing for a provider"},
			{"reset", "Reset routing overrides to defaults"},
		},
	},
	"routing enable": {
		short: "Enable routing for a provider",
		long:  "Enable routing for the specified provider.",
		usage: []string{"polytoken-quota routing enable <provider>"},
		args:  []flagDoc{{"<provider>", "Provider mapping ID"}},
	},
	"routing disable": {
		short: "Disable routing for a provider",
		long:  "Disable routing for the specified provider.",
		usage: []string{"polytoken-quota routing disable <provider>"},
		args:  []flagDoc{{"<provider>", "Provider mapping ID"}},
	},
	"routing reset": {
		short: "Reset routing overrides to defaults",
		long:  "Reset all routing overrides to their default state.",
		usage: []string{"polytoken-quota routing reset"},
	},
	"doctor": {
		short: "Diagnose configuration and quota health",
		long:  "Diagnose configuration and quota health issues.",
		usage: []string{"polytoken-quota doctor [--json]"},
		flags: []flagDoc{
			{"--json", "Output JSON"},
		},
	},
	"history": {
		short: "Show reconcile change history",
		long:  "Show reconcile change history.",
		usage: []string{"polytoken-quota history [--limit N] [--revision N] [--json]"},
		flags: []flagDoc{
			{"--limit N", "Number of recent records (1-100, default 20)"},
			{"--revision N", "Show detail for a specific revision"},
			{"--json", "Output JSON"},
		},
	},
	"notice-hook": {
		short: "Run one in-session hook event (installed by install-hook)",
		long:  "Handle one Polytoken hook event: converge this session's daemon to a newly published reconciliation notice and surface model-drift information. Invoked by the hooks installed with install-hook; not for direct use.",
		usage: []string{"polytoken-quota notice-hook [--notice PATH]"},
		flags: []flagDoc{
			{"--notice PATH", "Notice file to consume (defaults to the published path)"},
		},
	},
	"select": {
		short: "Recommend one registered model for a phase (never launches work)",
		long:  "Recommend one candidate model for a selection phase. The task is read from stdin and disclosed to the configured Jev assessment; --difficulty assigns the difficulty yourself and keeps the task fully local (stdin is never read, no credential or consent is used). Recommendations never launch an agent or reserve quota. Output contains no task text, credential, or raw upstream response.",
		usage: []string{"polytoken-quota select --policy PATH --phase NAME [--difficulty TIER | --min-difficulty TIER] [--exclude-family NAME]... [--refresh] [--json]"},
		flags: []flagDoc{
			{"--policy PATH", "Candidate policy file (version 1; every referenced model must be registered in desired configuration; never names desired.yaml itself)"},
			{"--phase NAME", "Selection phase key in the candidate policy"},
			{"--difficulty TIER", "Explicit difficulty (routine|normal|difficult|very_difficult); bypasses assessment and keeps the task local; conflicts with --min-difficulty"},
			{"--min-difficulty TIER", "Difficulty floor: raises a valid assessment only; never rescues an abstention or failed assessment; conflicts with --difficulty"},
			{"--exclude-family NAME", "Exclude one exact provider family (the prefix before /); repeatable"},
			{"--refresh", "Perform one provider quota poll without reconciliation before snapshotting; default reads the saved snapshot without polling"},
			{"--json", "Output JSON (version 1; nullable model and status)"},
		},
	},
	"select-eval": {
		short: "Run the operator evaluation gate against synthetic fixtures",
		long:  "Run the opt-in operator evaluation: every fixture case is assessed and reported against its expected tier or abstention. Reports case IDs, expected/actual assessment, safe error kinds, confusion counts, under/over-classification and abstention counts/rates, classifier model and rubric versions, latency and token usage. Never echoes task text or raw responses; never polls quota or dispatches agents. Requires --live, the selection.jev.enabled desired configuration (persistent consent), and externally supplied credentials.",
		usage: []string{"polytoken-quota select-eval --policy PATH --fixtures PATH --live [--json]"},
		flags: []flagDoc{
			{"--policy PATH", "Candidate policy file covering every fixture phase/tier"},
			{"--fixtures PATH", "Synthetic fixture document (version 1; non-sensitive synthetic cases)"},
			{"--live", "Explicit gate: perform the remote evaluation; without it nothing is disclosed"},
			{"--json", "Output JSON (version 1 envelope around the report)"},
		},
	},
	"install-hook": {
		short: "Install or remove the in-session Polytoken hook entries",
		long:  "Idempotently add (or remove with --remove) the two hooks.json entries that route session events to notice-hook. Backs up hooks.json before writing and never touches unrelated entries.",
		usage: []string{"polytoken-quota install-hook [--config-dir DIR] [--handler-path PATH] [--notice PATH] [--dry-run] [--remove]"},
		flags: []flagDoc{
			{"--config-dir DIR", "Polytoken config dir (default ~/.config/polytoken)"},
			{"--handler-path PATH", "Handler binary path as seen inside agent containers"},
			{"--notice PATH", "Notice path to bake into the handler (default: configured/default notice location)"},
			{"--dry-run", "Print the diff without writing"},
			{"--remove", "Remove the installed entries"},
		},
	},
}

// hasHelpFlag reports whether args contains --help or -h anywhere.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// isKnownCommand reports whether name is a recognized top-level command.
func isKnownCommand(name string) bool {
	for _, c := range commandOrder {
		if c == name {
			return true
		}
	}
	return false
}

// runHelp implements the help command and the --help/-h top-level flag.
// With no args it prints root help. With a command name it prints command
// help. With "routing <sub>" it prints routing subcommand help.
func runHelp(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		writeRootHelp(stdout)
		return ExitOK
	}
	cmd := args[0]
	if !isKnownCommand(cmd) {
		fmt.Fprintf(stderr, "unknown command: %s\n", cmd)
		usage(stderr)
		return ExitRejected
	}
	if cmd == "routing" && len(args) > 1 {
		sub := args[1]
		path := "routing " + sub
		if _, ok := helpDocs[path]; !ok {
			fmt.Fprintf(stderr, "unknown routing subcommand: %s\n", sub)
			writeCommandHelp(stderr, "routing")
			return ExitRejected
		}
		writeCommandHelp(stdout, path)
		return ExitOK
	}
	writeCommandHelp(stdout, cmd)
	return ExitOK
}

// writeRootHelp prints the top-level help to w.
func writeRootHelp(w io.Writer) {
	fmt.Fprintln(w, "polytoken-quota reconciles durable quota/availability state")
	fmt.Fprintln(w, "with managed Polytoken model fields.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: polytoken-quota <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")

	nameWidth := 0
	for _, cmd := range commandOrder {
		if len(cmd) > nameWidth {
			nameWidth = len(cmd)
		}
	}
	for _, cmd := range commandOrder {
		doc := helpDocs[cmd]
		fmt.Fprintf(w, "  %-*s  %s\n", nameWidth, cmd, doc.short)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global options:")
	fmt.Fprintln(w, "  --help, -h    Show help")
	fmt.Fprintln(w, "  --version     Print version and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'polytoken-quota help <command>' for details on a specific command.")
}

// writeCommandHelp prints help for a single command or subcommand to w.
func writeCommandHelp(w io.Writer, path string) {
	doc, ok := helpDocs[path]
	if !ok {
		return
	}
	for _, line := range doc.usage {
		fmt.Fprintf(w, "Usage: %s\n", line)
	}
	fmt.Fprintln(w)
	if doc.long != "" {
		fmt.Fprintln(w, doc.long)
		fmt.Fprintln(w)
	}
	if len(doc.args) > 0 {
		fmt.Fprintln(w, "Arguments:")
		writeFlagList(w, doc.args)
		fmt.Fprintln(w)
	}
	if len(doc.subcommands) > 0 {
		fmt.Fprintln(w, "Subcommands:")
		writeSubcommandList(w, doc.subcommands)
		fmt.Fprintln(w)
	}
	// Every command accepts --help/-h.
	flags := make([]flagDoc, 0, len(doc.flags)+1)
	flags = append(flags, doc.flags...)
	flags = append(flags, flagDoc{"--help, -h", "Show this help"})
	fmt.Fprintln(w, "Options:")
	writeFlagList(w, flags)
}

// writeFlagList prints a two-column flag/argument table with dynamic alignment.
func writeFlagList(w io.Writer, items []flagDoc) {
	width := 0
	for _, f := range items {
		if len(f.name) > width {
			width = len(f.name)
		}
	}
	for _, f := range items {
		fmt.Fprintf(w, "  %-*s  %s\n", width, f.name, f.desc)
	}
}

// writeSubcommandList prints a two-column subcommand table with dynamic alignment.
func writeSubcommandList(w io.Writer, subs []subcommandDoc) {
	width := 0
	for _, s := range subs {
		if len(s.name) > width {
			width = len(s.name)
		}
	}
	for _, s := range subs {
		fmt.Fprintf(w, "  %-*s  %s\n", width, s.name, s.short)
	}
}
