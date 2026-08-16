package notice

// install-hook: idempotently install (or remove) the two Polytoken hook
// entries that route session events to `notice-hook`. The merged document is
// validated before any write, unrelated entries (including string negations)
// keep their relative order verbatim, and every write is preceded by a
// timestamped backup. All operator-facing strings live here rather than in
// the CLI package: this package is the sanctioned place to name the
// session-convergence mechanism (see the public-surface guard).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Names of the two installed hook entries. Project-level hooks.json files can
// negate either with "!<name>".
const (
	reloadHookName = "polytoken-quota-reload"
	driftHookName  = "polytoken-quota-drift"
)

// InstallOptions configures Install. Empty fields resolve defaults:
// ~/.config/polytoken for the config dir, the running executable for the
// handler path, and the default notice location.
type InstallOptions struct {
	ConfigDir   string
	HandlerPath string
	NoticePath  string
	Remove      bool
	DryRun      bool
}

// Install performs the (idempotent) install or removal and returns the
// process exit code: 0 on success, 1 on refusal (malformed hooks.json,
// unresolvable defaults). Dry runs and no-ops never write.
func Install(opts InstallOptions, stdout, stderr io.Writer) int {
	configDir := opts.ConfigDir
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "install-hook: resolve home:", err)
			return 1
		}
		configDir = filepath.Join(home, ".config", "polytoken")
	}
	handler := opts.HandlerPath
	if handler == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(stderr, "install-hook: resolve executable:", err)
			return 1
		}
		handler = exe
	}
	noticePath := opts.NoticePath
	if noticePath == "" {
		var err error
		noticePath, err = ResolvePath("")
		if err != nil {
			fmt.Fprintln(stderr, "install-hook: resolve notice path:", err)
			return 1
		}
	}

	hooksPath := filepath.Join(configDir, "hooks.json")
	existing, hadFile, err := readHooksDoc(hooksPath)
	if err != nil {
		fmt.Fprintf(stderr, "install-hook: %s: %v\n", hooksPath, err)
		return 1
	}

	merged, foundOurs, err := mergeHookEntries(existing, handler, noticePath, opts.Remove)
	if err != nil {
		fmt.Fprintf(stderr, "install-hook: %v\n", err)
		return 1
	}
	rendered := renderHooksDoc(merged)

	// Removing when nothing of ours is present (or no file exists) is a
	// no-op: no write, no backup churn.
	if opts.Remove && !foundOurs {
		printInstallSummary(stdout, opts, noticePath, true)
		return 0
	}

	// Idempotent no-op: the document already has exactly the intended shape.
	if hadFile {
		if current, rerr := os.ReadFile(hooksPath); rerr == nil && bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(rendered)) {
			printInstallSummary(stdout, opts, noticePath, true)
			return 0
		}
	}

	if opts.DryRun {
		current := ""
		if hadFile {
			b, rerr := os.ReadFile(hooksPath)
			if rerr == nil {
				current = string(b)
			}
		}
		fmt.Fprint(stdout, unifiedDiff(current, string(rendered)))
		printInstallSummary(stdout, opts, noticePath, false)
		return 0
	}

	if err := writeHooksDoc(hooksPath, rendered, hadFile); err != nil {
		fmt.Fprintf(stderr, "install-hook: write: %v\n", err)
		return 1
	}
	printInstallSummary(stdout, opts, noticePath, false)
	return 0
}

// readHooksDoc loads and validates the existing hooks.json. A missing file is
// an empty document; anything unparseable or shape-invalid is an error.
func readHooksDoc(path string) ([]json.RawMessage, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, true, fmt.Errorf("not a JSON array: %w", err)
	}
	for i, e := range entries {
		if err := validateHookEntry(e); err != nil {
			return nil, true, fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return entries, true, nil
}

// validateHookEntry accepts the two legal entry shapes: a string negation
// ("!name") or an object carrying name, event, and handler.bash strings.
func validateHookEntry(e json.RawMessage) error {
	trimmed := bytes.TrimSpace(e)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("invalid string entry: %w", err)
		}
		return nil
	}
	var obj struct {
		Name    string `json:"name"`
		Event   string `json:"event"`
		Handler *struct {
			Bash string `json:"bash"`
		} `json:"handler"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return fmt.Errorf("entry must be a string negation or an object: %w", err)
	}
	if obj.Name == "" || obj.Event == "" || obj.Handler == nil || obj.Handler.Bash == "" {
		return fmt.Errorf("object entries require name, event, and handler.bash")
	}
	return nil
}

// mergeHookEntries returns the merged document and whether any managed entry
// was present in the existing document. Every non-managed entry keeps its
// original order; on install the two managed entries are appended (stale
// same-name entries are thereby replaced); on remove they are dropped.
func mergeHookEntries(existing []json.RawMessage, handlerPath, noticePath string, remove bool) ([]json.RawMessage, bool, error) {
	ours := map[string]bool{reloadHookName: true, driftHookName: true}
	foundOurs := false
	out := make([]json.RawMessage, 0, len(existing)+2)
	for _, e := range existing {
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e, &obj); err == nil && ours[obj.Name] {
			foundOurs = true
			continue
		}
		out = append(out, e)
	}
	if remove {
		return out, foundOurs, nil
	}
	bash := fmt.Sprintf("exec %s notice-hook --notice %s", shellQuote(handlerPath), shellQuote(noticePath))
	for _, entry := range []struct{ name, event string }{
		{reloadHookName, "post_model_turn"},
		{driftHookName, "pre_user_prompt"},
	} {
		raw, err := json.Marshal(map[string]any{
			"name":    entry.name,
			"event":   entry.event,
			"handler": map[string]string{"bash": bash},
		})
		if err != nil {
			return nil, false, err
		}
		out = append(out, raw)
	}
	return out, foundOurs, nil
}

// shellQuote single-quotes a path for the handler's bash line so whitespace
// and shell metacharacters in operator-supplied paths cannot word-split or
// execute.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func renderHooksDoc(entries []json.RawMessage) []byte {
	if entries == nil {
		entries = []json.RawMessage{}
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return []byte("[]\n")
	}
	return append(b, '\n')
}

// writeHooksDoc backs up the existing file (when present) then atomically
// replaces it.
func writeHooksDoc(path string, rendered []byte, hadFile bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if hadFile {
		backup := fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())
		current, err := os.ReadFile(path)
		if err == nil {
			_ = os.WriteFile(backup, current, 0o600)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(rendered); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// printInstallSummary emits the operator caveats: the session pickup
// boundary, project negation, and the container-visibility advisory (always,
// because the notice path's mount state cannot be verified from here).
func printInstallSummary(w io.Writer, opts InstallOptions, noticePath string, noop bool) {
	verb := "installed"
	if opts.Remove {
		verb = "removed"
		if noop {
			fmt.Fprintln(w, "hooks: already absent; nothing to do")
			return
		}
	} else if noop {
		fmt.Fprintln(w, "hooks: already installed; nothing to do")
	}
	fmt.Fprintf(w, "hooks: %s %s (%s) and %s (%s) in hooks.json\n",
		verb, reloadHookName, "post_model_turn", driftHookName, "pre_user_prompt")
	fmt.Fprintln(w, "existing sessions pick up new hook entries at their next config reload; new sessions immediately")
	fmt.Fprintln(w, "projects can negate either entry in .polytoken/hooks.json with \"!"+reloadHookName+"\" / \"!"+driftHookName+"\"")
	fmt.Fprintf(w, "notice path %s cannot be verified visible inside agent containers — bind-mount it at the same path (or set operational.notice_path to a shared location)\n", noticePath)
}

// unifiedDiff renders a minimal line-based unified diff between two documents.
func unifiedDiff(oldDoc, newDoc string) string {
	oldLines := strings.Split(strings.TrimRight(oldDoc, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(newDoc, "\n"), "\n")
	if oldDoc == "" {
		oldLines = nil
	}
	if newDoc == "" {
		newLines = nil
	}
	var out []string
	lcs := lcsTable(oldLines, newLines)
	var walk func(i, j int)
	walk = func(i, j int) {
		switch {
		case i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j]:
			out = append(out, " "+oldLines[i])
			walk(i+1, j+1)
		case i < len(oldLines) && j < len(newLines):
			// Choose deletion or advancement by the LCS table.
			if lcs[i+1][j] >= lcs[i][j+1] {
				out = append(out, "-"+oldLines[i])
				walk(i+1, j)
			} else {
				out = append(out, "+"+newLines[j])
				walk(i, j+1)
			}
		case i < len(oldLines):
			out = append(out, "-"+oldLines[i])
			walk(i+1, j)
		case j < len(newLines):
			out = append(out, "+"+newLines[j])
			walk(i, j+1)
		}
	}
	walk(0, 0)
	return strings.Join(out, "\n") + "\n"
}

func lcsTable(a, b []string) [][]int {
	t := make([][]int, len(a)+1)
	for i := range t {
		t[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	return t
}
