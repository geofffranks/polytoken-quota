package cli

// Public-surface guard (AC.11): proves the source/help/output contain no
// obsolete public path (removed commands hook/sync/quota/state) and no
// running-session restart/reload advisory. It complements the lower-level
// safety suites (TestNoProcessControl); it only scans non-test source so its
// own banned-fragment list does not create a self-referential false positive.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// bannedAdvisoryFragments are the running-session advisory strings that must be
// absent from all non-test source. Prose fragments are matched on word
// boundaries so they do not match legitimate substrings (e.g. "reload" inside
// "preloaded"); identifiers are matched verbatim.
var bannedAdvisoryFragments = []string{
	"running_session_advisory",
	"RunningSessionAdvisory",
	`\brestart\b`,
	`\breload\b`,
	`already-running`,
	`may retain pre-reconciliation`,
}

// removedCommandNames are the former top-level commands that must not appear as
// real commands in the usage/help text, nor be suggested to users as runnable
// commands anywhere in the source.
var removedCommandNames = []string{
	"hook",
	"sync",
	"quota",
	"state",
}

// polytokenPrefix is the binary-name stem that precedes "quota" in
// "polytoken-quota". Used to exclude false positives where a removed-command
// invocation regex matches inside the binary name (e.g. "polytoken-quota check").
const polytokenPrefix = "polytoken-"

// removedCommandInvocations are the backticked removed-command spellings that
// must never be presented to a user as a runnable command. Each is matched on
// word boundaries; "polytoken-quota" (the binary name) is excluded separately.
// This avoids false positives on the pervasive domain vocabulary (struct tags
// like json:"quota", fsync error messages like "sync dir", UI column labels,
// quota-class strings) by targeting the specific obsolete *invocation* forms
// rather than bare words.
var removedCommandInvocations = []string{
	`\bquota check\b`,
	`\bquota status\b`,
	`\bsync --`,
	`\bstate set\b`,
	`\bstate clear\b`,
	"`hook`",
}

// publicSurfaceExcludedDirs are the internal/ packages excluded from the
// removed-command invocation scan.
var publicSurfaceExcludedDirs = map[string]bool{}

func publicSurfaceRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	cliDir := filepath.Dir(thisFile)
	internalDir := filepath.Dir(cliDir)
	return internalDir
}

// walkGoSources walks dir and invokes fn for every non-test .go file.
func walkGoSources(t *testing.T, dir string, fn func(path string, b []byte)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fn(path, b)
		return nil
	})
	if err != nil {
		t.Fatalf("walkGoSources %s: %v", dir, err)
	}
}

// stripDecls removes package declarations and comments from source bytes so the
// guard scans only string literals for removed-command invocations. Package
// declarations and doc comments legitimately use quota/sync/hook as internal
// operation names (e.g. transactQuotaCheck, QuotaStatus) and must not be flagged.
func stripDecls(b []byte) []byte {
	var kept []string
	inBlockComment := false
	for _, line := range strings.Split(string(b), "\n") {
		// Track multi-line block comments (/* ... */).
		if inBlockComment {
			if idx := strings.Index(line, "*/"); idx >= 0 {
				inBlockComment = false
				line = line[idx+2:]
			} else {
				continue
			}
		}
		// Strip a single-line block comment that opens without closing.
		if sIdx := strings.Index(line, "/*"); sIdx >= 0 {
			if eIdx := strings.Index(line[sIdx+2:], "*/"); eIdx >= 0 {
				line = line[:sIdx] + line[sIdx+2+eIdx+2:]
			} else {
				line = line[:sIdx]
				inBlockComment = true
			}
		}
		trim := strings.TrimSpace(line)
		// Drop package declarations and line comments so only code remains.
		if strings.HasPrefix(trim, "package ") || strings.HasPrefix(trim, "//") {
			continue
		}
		// Strip trailing line comments (after code) to avoid flagging
		// references inside // annotations.
		if idx := indexLineComment(line); idx >= 0 {
			line = line[:idx]
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}

// indexLineComment returns the byte index of a "//" line comment start in line,
// or -1 if none. It skips "//" inside string literals.
func indexLineComment(line string) int {
	inString := false
	for i := 0; i < len(line)-1; i++ {
		c := line[i]
		if c == '"' {
			inString = !inString
		}
		if !inString && c == '/' && line[i+1] == '/' {
			return i
		}
	}
	return -1
}

// TestPublicSurfaceHasNoObsoleteCommandsOrAdvisory guards AC.11: no non-test
// source under internal/ carries the running-session restart/reload advisory or
// removed-command invocations, and the usage text lists only the current command
// set (no obsolete hook/sync/quota/state commands).
func TestPublicSurfaceHasNoObsoleteCommandsOrAdvisory(t *testing.T) {
	internalDir := publicSurfaceRoot(t)

	t.Run("no advisory fragments in source", func(t *testing.T) {
		// Compile each fragment once; identifiers compile as plain regex.
		patterns := make([]*regexp.Regexp, 0, len(bannedAdvisoryFragments))
		for _, frag := range bannedAdvisoryFragments {
			patterns = append(patterns, regexp.MustCompile("(?i)"+frag))
		}
		// Scan all internal/ non-test packages.
		entries, err := os.ReadDir(internalDir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", internalDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(internalDir, e.Name())
			walkGoSources(t, dir, func(path string, b []byte) {
				for _, pat := range patterns {
					if pat.Match(b) {
						t.Errorf("banned advisory fragment %q present in %s", pat.String(), path)
					}
				}
			})
		}
	})

	t.Run("no removed-command invocations in source", func(t *testing.T) {
		// Scan all internal/ non-test packages except the excluded ones.
		entries, err := os.ReadDir(internalDir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", internalDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() || publicSurfaceExcludedDirs[e.Name()] {
				continue
			}
			dir := filepath.Join(internalDir, e.Name())
			walkGoSources(t, dir, func(path string, b []byte) {
				stripped := stripDecls(b)
				for _, invocation := range removedCommandInvocations {
					re := regexp.MustCompile(invocation)
					for _, loc := range re.FindAllIndex(stripped, -1) {
						// Exclude matches that are part of the binary name
						// "polytoken-quota". The regex may match starting at
						// "quota" inside "polytoken-quota check", so check both
						// the excerpt prefix and a "polytoken-" lookback.
						excerpt := string(stripped[loc[0]:])
						if strings.HasPrefix(excerpt, "polytoken-quota ") {
							continue
						}
						if loc[0] >= len(polytokenPrefix) && string(stripped[loc[0]-len(polytokenPrefix):loc[0]]) == polytokenPrefix {
							continue
						}
						t.Errorf("removed-command invocation %q present in %s", invocation, path)
					}
				}
			})
		}
	})

	t.Run("usage lists only current commands", func(t *testing.T) {
		var stderr bytes.Buffer
		Run(context.Background(), []string{}, io.Reader(strings.NewReader("")), io.Discard, &stderr, Dependencies{})
		help := stderr.String()
		wantCommands := []string{"init", "status", "check", "reconcile", "routing", "doctor"}
		// Isolate the commands-listing line so the program name
		// ("polytoken-quota") is not mistaken for the removed "quota" command.
		var commandsLine string
		for _, line := range strings.Split(help, "\n") {
			if strings.HasPrefix(line, "commands:") {
				commandsLine = line
				break
			}
		}
		for _, c := range wantCommands {
			if !strings.Contains(commandsLine, c) {
				t.Errorf("usage missing expected command %q: %q", c, help)
			}
		}
		// Removed commands must not appear in the commands listing.
		for _, c := range removedCommandNames {
			re := regexp.MustCompile(`\b` + regexp.QuoteMeta(c) + `\b`)
			if re.MatchString(commandsLine) {
				t.Errorf("usage must not list removed command %q: %q", c, help)
			}
		}
	})
}
