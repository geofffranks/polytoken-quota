package cli

// help_test.go — tests for the polytoken-quota help subsystem.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// runHelpCapture invokes Run with the given args and captures stdout/stderr/exit.
func runHelpCapture(t *testing.T, args []string) (stdout, stderr string, code int) {
	t.Helper()
	var out, err bytes.Buffer
	code = Run(context.Background(), args, io.Reader(strings.NewReader("")), &out, &err, Dependencies{})
	return out.String(), err.String(), code
}

// --- Root help ---

func TestRootHelpViaHelpCommand(t *testing.T) {
	stdout, stderr, code := runHelpCapture(t, []string{"help"})
	if code != ExitOK {
		t.Fatalf("help: expected exit %d, got %d (stderr=%q)", ExitOK, code, stderr)
	}
	if stderr != "" {
		t.Errorf("help: expected empty stderr, got %q", stderr)
	}
	assertRootHelp(t, stdout)
}

func TestRootHelpViaDashDashHelp(t *testing.T) {
	stdout, _, code := runHelpCapture(t, []string{"--help"})
	if code != ExitOK {
		t.Fatalf("--help: expected exit %d, got %d", ExitOK, code)
	}
	assertRootHelp(t, stdout)
}

func TestRootHelpViaShortH(t *testing.T) {
	stdout, _, code := runHelpCapture(t, []string{"-h"})
	if code != ExitOK {
		t.Fatalf("-h: expected exit %d, got %d", ExitOK, code)
	}
	assertRootHelp(t, stdout)
}

func assertRootHelp(t *testing.T, help string) {
	t.Helper()
	for _, cmd := range commandOrder {
		if !strings.Contains(help, cmd) {
			t.Errorf("root help missing command %q:\n%s", cmd, help)
		}
	}
	for _, want := range []string{"Usage:", "--help, -h", "--version", "Commands:"} {
		if !strings.Contains(help, want) {
			t.Errorf("root help missing %q:\n%s", want, help)
		}
	}
}

func TestRootHelpOmitsRemovedCommands(t *testing.T) {
	stdout, _, _ := runHelpCapture(t, []string{"help"})
	for _, removed := range []string{"hook", "sync", "quota", "state"} {
		// Check that removed commands don't appear as standalone command entries
		// (the "Commands:" section). They may appear inside other words but not
		// as command listing entries like "\n  hook  ".
		if strings.Contains(stdout, "  "+removed+"  ") || strings.Contains(stdout, "  "+removed+" ") {
			t.Errorf("root help lists removed command %q:\n%s", removed, stdout)
		}
	}
}

// --- Per-command help via `help <cmd>` ---

func TestCommandHelpViaHelpSubcommand(t *testing.T) {
	for _, cmd := range commandOrder {
		t.Run(cmd, func(t *testing.T) {
			stdout, stderr, code := runHelpCapture(t, []string{"help", cmd})
			if code != ExitOK {
				t.Fatalf("help %s: expected exit %d, got %d (stderr=%q)", cmd, ExitOK, code, stderr)
			}
			if stderr != "" {
				t.Errorf("help %s: expected empty stderr, got %q", cmd, stderr)
			}
			doc, ok := helpDocs[cmd]
			if !ok {
				t.Fatalf("no help doc for %s", cmd)
			}
			for _, usage := range doc.usage {
				if !strings.Contains(stdout, usage) {
					t.Errorf("help %s: missing usage line %q:\n%s", cmd, usage, stdout)
				}
			}
			if !strings.Contains(stdout, "Options:") {
				t.Errorf("help %s: missing Options section:\n%s", cmd, stdout)
			}
			if !strings.Contains(stdout, "--help, -h") {
				t.Errorf("help %s: missing --help flag in Options:\n%s", cmd, stdout)
			}
		})
	}
}

// --- Per-command help via `<cmd> --help` / `-h` ---

func TestCommandHelpViaFlag(t *testing.T) {
	cases := []struct {
		args []string
		path string
	}{
		{[]string{"init", "--help"}, "init"},
		{[]string{"init", "-h"}, "init"},
		{[]string{"status", "--help"}, "status"},
		{[]string{"check", "--help"}, "check"},
		{[]string{"reconcile", "--help"}, "reconcile"},
		{[]string{"doctor", "--help"}, "doctor"},
		{[]string{"history", "--help"}, "history"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			stdout, stderr, code := runHelpCapture(t, tc.args)
			if code != ExitOK {
				t.Fatalf("expected exit %d, got %d (stderr=%q)", ExitOK, code, stderr)
			}
			if stderr != "" {
				t.Errorf("expected empty stderr, got %q", stderr)
			}
			doc := helpDocs[tc.path]
			if !strings.Contains(stdout, doc.usage[0]) {
				t.Errorf("missing usage line %q:\n%s", doc.usage[0], stdout)
			}
		})
	}
}

func TestHelpFlagDoesNotMutate(t *testing.T) {
	spy := newDepsSpy()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--help"},
		io.Reader(strings.NewReader("")), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d", ExitOK, code)
	}
	if spy.Mutations != 0 {
		t.Errorf("init --help must not invoke mutator, got %d mutations", spy.Mutations)
	}
}

func TestHelpFlagPrecedenceOverInvalidArgs(t *testing.T) {
	// --help should win even when other args are invalid.
	stdout, stderr, code := runHelpCapture(t, []string{"check", "--bogus", "--help"})
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d (stderr=%q)", ExitOK, code, stderr)
	}
	if !strings.Contains(stdout, "polytoken-quota check") {
		t.Errorf("expected check help on stdout, got:\n%s", stdout)
	}
}

// --- Routing help ---

func TestRoutingHelpViaFlag(t *testing.T) {
	stdout, stderr, code := runHelpCapture(t, []string{"routing", "--help"})
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d", ExitOK, code)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
	for _, want := range []string{"Subcommands:", "explain", "enable", "disable", "reset"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("routing help missing %q:\n%s", want, stdout)
		}
	}
}

func TestRoutingHelpViaHelpCommand(t *testing.T) {
	stdout, _, code := runHelpCapture(t, []string{"help", "routing"})
	if code != ExitOK {
		t.Fatalf("expected exit %d, got %d", ExitOK, code)
	}
	for _, want := range []string{"Subcommands:", "explain", "enable", "disable", "reset"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help routing missing %q:\n%s", want, stdout)
		}
	}
}

func TestRoutingSubcommandHelpViaFlag(t *testing.T) {
	cases := []struct {
		args []string
		path string
	}{
		{[]string{"routing", "explain", "--help"}, "routing explain"},
		{[]string{"routing", "enable", "--help"}, "routing enable"},
		{[]string{"routing", "disable", "--help"}, "routing disable"},
		{[]string{"routing", "reset", "--help"}, "routing reset"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			stdout, stderr, code := runHelpCapture(t, tc.args)
			if code != ExitOK {
				t.Fatalf("expected exit %d, got %d (stderr=%q)", ExitOK, code, stderr)
			}
			if stderr != "" {
				t.Errorf("expected empty stderr, got %q", stderr)
			}
			doc := helpDocs[tc.path]
			if !strings.Contains(stdout, doc.usage[0]) {
				t.Errorf("missing usage %q:\n%s", doc.usage[0], stdout)
			}
		})
	}
}

func TestRoutingSubcommandHelpViaHelpCommand(t *testing.T) {
	cases := []struct {
		args []string
		path string
	}{
		{[]string{"help", "routing", "explain"}, "routing explain"},
		{[]string{"help", "routing", "enable"}, "routing enable"},
		{[]string{"help", "routing", "disable"}, "routing disable"},
		{[]string{"help", "routing", "reset"}, "routing reset"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			stdout, stderr, code := runHelpCapture(t, tc.args)
			if code != ExitOK {
				t.Fatalf("expected exit %d, got %d (stderr=%q)", ExitOK, code, stderr)
			}
			if stderr != "" {
				t.Errorf("expected empty stderr, got %q", stderr)
			}
			doc := helpDocs[tc.path]
			if !strings.Contains(stdout, doc.usage[0]) {
				t.Errorf("missing usage %q:\n%s", doc.usage[0], stdout)
			}
		})
	}
}

// --- Error cases ---

func TestHelpUnknownCommand(t *testing.T) {
	stdout, stderr, code := runHelpCapture(t, []string{"help", "bogus"})
	if code != ExitRejected {
		t.Fatalf("expected exit %d, got %d", ExitRejected, code)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout for unknown command help, got %q", stdout)
	}
	if !strings.Contains(stderr, "unknown command: bogus") {
		t.Errorf("expected 'unknown command: bogus' in stderr, got %q", stderr)
	}
}

func TestHelpUnknownRoutingSubcommand(t *testing.T) {
	stdout, stderr, code := runHelpCapture(t, []string{"help", "routing", "bogus"})
	if code != ExitRejected {
		t.Fatalf("expected exit %d, got %d", ExitRejected, code)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "unknown routing subcommand: bogus") {
		t.Errorf("expected 'unknown routing subcommand' in stderr, got %q", stderr)
	}
}

func TestUsageGoesToStderr(t *testing.T) {
	stdout, stderr, code := runHelpCapture(t, []string{})
	if code != ExitRejected {
		t.Fatalf("no-args: expected exit %d, got %d", ExitRejected, code)
	}
	if stdout != "" {
		t.Errorf("no-args: expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "commands:") {
		t.Errorf("no-args: expected usage on stderr, got %q", stderr)
	}
}

func TestUsageMentionsHelp(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()
	if !strings.Contains(out, "help") {
		t.Errorf("usage should mention help:\n%s", out)
	}
}

// --- Content spot-checks ---

func TestCheckHelpListsAllFlags(t *testing.T) {
	stdout, _, _ := runHelpCapture(t, []string{"help", "check"})
	for _, flag := range []string{"--provider", "--reconcile", "--json", "--quiet", "--help, -h"} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("check help missing flag %q:\n%s", flag, stdout)
		}
	}
}

func TestReconcileHelpListsAllFlags(t *testing.T) {
	stdout, _, _ := runHelpCapture(t, []string{"help", "reconcile"})
	for _, flag := range []string{"--dry-run", "--keep-staging", "--verbose", "--help, -h"} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("reconcile help missing flag %q:\n%s", flag, stdout)
		}
	}
}

func TestHistoryHelpListsAllFlags(t *testing.T) {
	stdout, _, _ := runHelpCapture(t, []string{"help", "history"})
	for _, flag := range []string{"--limit", "--revision", "--json", "--help, -h"} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("history help missing flag %q:\n%s", flag, stdout)
		}
	}
}

func TestRoutingEnableHelpShowsArgument(t *testing.T) {
	stdout, _, _ := runHelpCapture(t, []string{"routing", "enable", "--help"})
	if !strings.Contains(stdout, "Arguments:") {
		t.Errorf("routing enable help should list Arguments:\n%s", stdout)
	}
	if !strings.Contains(stdout, "<provider>") {
		t.Errorf("routing enable help should mention <provider>:\n%s", stdout)
	}
}
