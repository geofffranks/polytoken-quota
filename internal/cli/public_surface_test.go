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
// absent from all non-test source in the cli and service packages. Prose
// fragments are matched on word boundaries so they do not match legitimate
// substrings (e.g. "reload" inside "preloaded"); identifiers are matched
// verbatim.
var bannedAdvisoryFragments = []string{
	"running_session_advisory",
	"RunningSessionAdvisory",
	`\brestart\b`,
	`\breload\b`,
	`already-running`,
	`may retain pre-reconciliation`,
}

// removedCommandNames are the former top-level commands that must not appear as
// real commands in the usage/help text.
var removedCommandNames = []string{
	"hook",
	"sync",
	"quota",
	"state",
}

func publicSurfaceSourceDirs(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	cliDir := filepath.Dir(thisFile)
	root := filepath.Dir(cliDir)
	return []string{
		filepath.Join(root, "cli"),
		filepath.Join(root, "service"),
	}
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

// TestPublicSurfaceHasNoObsoleteCommandsOrAdvisory guards AC.11: no non-test
// source in internal/cli or internal/service carries the running-session
// restart/reload advisory, and the usage text lists only the current command
// set (no obsolete hook/sync/quota/state commands).
func TestPublicSurfaceHasNoObsoleteCommandsOrAdvisory(t *testing.T) {
	t.Run("no advisory fragments in source", func(t *testing.T) {
		// Compile each fragment once; identifiers compile as plain regex.
		patterns := make([]*regexp.Regexp, 0, len(bannedAdvisoryFragments))
		for _, frag := range bannedAdvisoryFragments {
			patterns = append(patterns, regexp.MustCompile("(?i)"+frag))
		}
		for _, dir := range publicSurfaceSourceDirs(t) {
			walkGoSources(t, dir, func(path string, b []byte) {
				for _, pat := range patterns {
					if pat.Match(b) {
						t.Errorf("banned advisory fragment %q present in %s", pat.String(), path)
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
