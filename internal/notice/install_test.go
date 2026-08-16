package notice

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type installResult struct {
	code  int
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func runInstall(t *testing.T, opts InstallOptions) installResult {
	t.Helper()
	var res installResult
	res.code = Install(opts, &res.stdout, &res.stderr)
	return res
}

func hooksPath(configDir string) string { return filepath.Join(configDir, "hooks.json") }

func readHooksNames(t *testing.T, configDir string) []string {
	t.Helper()
	b, err := os.ReadFile(hooksPath(configDir))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	var names []string
	for _, e := range raw {
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e, &obj); err == nil && obj.Name != "" {
			names = append(names, obj.Name)
			continue
		}
		var s string
		if err := json.Unmarshal(e, &s); err == nil {
			names = append(names, s)
			continue
		}
		names = append(names, string(e))
	}
	return names
}

func writeHooks(t *testing.T, configDir, content string) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath(configDir), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func baseOpts(t *testing.T, configDir string) InstallOptions {
	t.Helper()
	return InstallOptions{
		ConfigDir:   configDir,
		HandlerPath: "/home/dev/bin/polytoken-quota",
		NoticePath:  "/home/dev/.local/polytoken-quota/notice.json",
	}
}

// TestInstallHookCreatesAndIsIdempotent: installing into a missing hooks.json
// creates exactly the two entries with the baked handler argv; a second
// install changes nothing and does not churn backups.
func TestInstallHookCreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	opts := baseOpts(t, dir)

	res := runInstall(t, opts)
	if res.code != 0 {
		t.Fatalf("exit = %d stderr=%s", res.code, res.stderr.String())
	}
	names := readHooksNames(t, dir)
	if len(names) != 2 || names[0] != reloadHookName || names[1] != driftHookName {
		t.Fatalf("names = %v", names)
	}
	b, _ := os.ReadFile(hooksPath(dir))
	var entries []struct {
		Name    string `json:"name"`
		Event   string `json:"event"`
		Handler struct {
			Bash string `json:"bash"`
		} `json:"handler"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantBash := "exec '/home/dev/bin/polytoken-quota' notice-hook --notice '/home/dev/.local/polytoken-quota/notice.json'"
	for _, e := range entries {
		if e.Handler.Bash != wantBash {
			t.Fatalf("%s bash = %q, want %q", e.Name, e.Handler.Bash, wantBash)
		}
	}
	if entries[0].Event != "post_model_turn" || entries[1].Event != "pre_user_prompt" {
		t.Fatalf("events = %+v", entries)
	}

	res2 := runInstall(t, opts)
	if res2.code != 0 {
		t.Fatalf("second install exit = %d", res2.code)
	}
	if got := readHooksNames(t, dir); len(got) != 2 {
		t.Fatalf("second install duplicated entries: %v", got)
	}
	// No prior file existed for the first install and the second is a no-op,
	// so no backup is ever created.
	backups, _ := filepath.Glob(filepath.Join(dir, "hooks.json.bak-*"))
	if len(backups) != 0 {
		t.Fatalf("backups = %v, want none (idempotent no-op)", backups)
	}
}

// TestInstallHookPreservesUnrelatedEntries: existing hooks (objects and
// string negations) keep their relative order around the installed pair, and
// stale same-name entries are replaced in place.
func TestInstallHookPreservesUnrelatedEntries(t *testing.T) {
	dir := t.TempDir()
	writeHooks(t, dir, `[
  {"name": "log-edits", "event": "pre_tool_use", "matcher": "file_*", "handler": {"bash": "exec /bin/true"}},
  "!polytoken-quota-reload",
  {"name": "polytoken-quota-drift", "event": "pre_user_prompt", "handler": {"bash": "exec /old/binary notice-hook"}}
]`)

	res := runInstall(t, baseOpts(t, dir))
	if res.code != 0 {
		t.Fatalf("exit = %d stderr=%s", res.code, res.stderr.String())
	}
	names := readHooksNames(t, dir)
	want := []string{"log-edits", "!polytoken-quota-reload", reloadHookName, driftHookName}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
	// The stale drift entry must have been replaced (not duplicated).
	b, _ := os.ReadFile(hooksPath(dir))
	if strings.Count(string(b), driftHookName) != 1 {
		t.Fatalf("stale entry not replaced: %s", b)
	}
	if strings.Contains(string(b), "/old/binary") {
		t.Fatalf("stale handler survived: %s", b)
	}
}

// TestInstallHookMalformedRefused: an unparseable or invalid hooks.json is
// refused with exit 1 and left byte-identical.
func TestInstallHookMalformedRefused(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       "{bad",
		"not an array":   `{"name": "x"}`,
		"missing event":  `[{"name": "x", "handler": {"bash": "true"}}]`,
		"missing handler": `[{"name": "x", "event": "stop"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeHooks(t, dir, content)
			before, _ := os.ReadFile(hooksPath(dir))

			res := runInstall(t, baseOpts(t, dir))
			if res.code != 1 {
				t.Fatalf("exit = %d, want 1", res.code)
			}
			after, _ := os.ReadFile(hooksPath(dir))
			if string(before) != string(after) {
				t.Fatalf("malformed file modified on refusal")
			}
			if res.stderr.Len() == 0 {
				t.Fatalf("refusal must explain on stderr")
			}
		})
	}
}

// TestInstallHookRemoveAndDryRun: --remove deletes exactly the two entries
// (absence is success); --dry-run changes nothing and prints a diff plus the
// container-visibility advisory; install always prints the advisory.
func TestInstallHookRemoveAndDryRun(t *testing.T) {
	t.Run("remove deletes exactly ours", func(t *testing.T) {
		dir := t.TempDir()
		opts := baseOpts(t, dir)
		if res := runInstall(t, opts); res.code != 0 {
			t.Fatalf("install: %d", res.code)
		}
		remove := opts
		remove.Remove = true
		if res := runInstall(t, remove); res.code != 0 {
			t.Fatalf("remove: %d %s", res.code, res.stderr.String())
		}
		if got := readHooksNames(t, dir); len(got) != 0 {
			t.Fatalf("remove left entries: %v", got)
		}
	})
	t.Run("remove with absent entries succeeds without write", func(t *testing.T) {
		dir := t.TempDir()
		writeHooks(t, dir, `[{"name": "keep", "event": "stop", "handler": {"bash": "true"}}]`)
		opts := baseOpts(t, dir)
		opts.Remove = true
		if res := runInstall(t, opts); res.code != 0 {
			t.Fatalf("remove-absent exit = %d", res.code)
		}
		if got := readHooksNames(t, dir); len(got) != 1 || got[0] != "keep" {
			t.Fatalf("remove-absent changed file: %v", got)
		}
	})
	t.Run("dry run changes nothing and prints diff plus advisory", func(t *testing.T) {
		dir := t.TempDir()
		writeHooks(t, dir, `[{"name": "keep", "event": "stop", "handler": {"bash": "true"}}]`)
		opts := baseOpts(t, dir)
		opts.DryRun = true
		res := runInstall(t, opts)
		if res.code != 0 {
			t.Fatalf("exit = %d", res.code)
		}
		after, _ := os.ReadFile(hooksPath(dir))
		if !strings.Contains(string(after), `"keep"`) || strings.Contains(string(after), reloadHookName) {
			t.Fatalf("dry-run modified hooks.json: %s", after)
		}
		out := res.stdout.String()
		if !strings.Contains(out, "+") || !strings.Contains(out, "-") {
			t.Fatalf("dry-run output lacks diff markers:\n%s", out)
		}
		if !strings.Contains(out, "bind-mount") {
			t.Fatalf("dry-run lacks container-visibility advisory:\n%s", out)
		}
	})
	t.Run("install prints advisory and load caveat", func(t *testing.T) {
		dir := t.TempDir()
		res := runInstall(t, baseOpts(t, dir))
		out := res.stdout.String()
		if !strings.Contains(out, "bind-mount") || !strings.Contains(out, "next config") {
			t.Fatalf("install output lacks caveats:\n%s", out)
		}
	})
}

// TestInstallHookBackup: writing over an existing file leaves a timestamped
// backup with the prior content.
func TestInstallHookBackup(t *testing.T) {
	dir := t.TempDir()
	writeHooks(t, dir, `[{"name": "prior", "event": "stop", "handler": {"bash": "true"}}]`)
	if res := runInstall(t, baseOpts(t, dir)); res.code != 0 {
		t.Fatalf("exit = %d", res.code)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "hooks.json.bak-*"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	b, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(b), `"prior"`) {
		t.Fatalf("backup lacks prior content: %s", b)
	}
}
