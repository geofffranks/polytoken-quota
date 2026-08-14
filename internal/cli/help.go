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
	"init", "status", "check", "reconcile", "routing", "doctor", "history",
}

// helpDocs maps each command path to its help content. Top-level commands use
// their bare name ("init"); routing subcommands use "routing <sub>".
var helpDocs = map[string]helpDoc{
	"init": {
		short: "Initialize quota state from current configuration",
		long:  "Initialize quota state from the current Polytoken configuration.",
		usage: []string{"polytoken-quota init [--force]"},
		flags: []flagDoc{
			{"--force", "Overwrite existing state without confirmation"},
		},
	},
	"status": {
		short: "Show current quota and routing status",
		long:  "Show current quota, routing, and target status.",
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
		long:  "Apply desired quota state to the live Polytoken configuration.",
		usage: []string{"polytoken-quota reconcile [--dry-run] [--keep-staging] [--verbose]"},
		flags: []flagDoc{
			{"--dry-run", "Preview changes without applying"},
			{"--keep-staging", "Retain staged candidates (requires --dry-run)"},
			{"--verbose", "Show detailed reconciliation trace"},
		},
	},
	"routing": {
		short: "View or modify routing configuration",
		long:  "View effective routing chains or modify routing configuration.",
		usage: []string{
			"polytoken-quota routing [--json]",
			"polytoken-quota routing <subcommand> [options]",
		},
		flags: []flagDoc{
			{"--json", "Output JSON (bare and explain)"},
		},
		subcommands: []subcommandDoc{
			{"explain", "Show routing readiness and selected desired/effective models"},
			{"enable", "Enable routing for a provider"},
			{"disable", "Disable routing for a provider"},
			{"reset", "Reset routing overrides to defaults"},
		},
	},
	"routing explain": {
		short: "Show routing readiness and selected desired/effective models",
		long:  "Show provider readiness as ready or not ready and the selected desired and effective model for each route. Pending targets may not be live; run polytoken-quota doctor to diagnose.",
		usage: []string{"polytoken-quota routing explain [--json]"},
		flags: []flagDoc{
			{"--json", "Output JSON"},
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
