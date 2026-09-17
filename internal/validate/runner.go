// Package validate runs the bounded startup-equivalent Polytoken validation
// against a staged candidate. It executes exactly two commands — `config
// validate` then `doctor` — against the candidate's staged config and working
// directories via direct executable invocation (no shell), under one shared
// timeout context, with combined output bounded in memory and redacted before
// it is persisted. On any exit path the staging candidate is cleaned up; live
// files are never read or written here.
package validate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/staging"
	"unicode/utf8"
)

// Stage names the validation step a result refers to.
type Stage string

const (
	// ConfigValidate is the `config validate` stage, run first.
	ConfigValidate Stage = "config_validate"
	// Doctor is the `doctor` stage, run only after config validation passes.
	Doctor Stage = "doctor"
)

// CommandRunner runs one external command under ctx, bounding the captured
// stdout and stderr to at most max bytes combined, and returning the captured
// bytes, the process exit code (0 on success, -1 when unavailable), whether
// any captured bytes were dropped or clipped by the bound (so a truncated
// diagnostic never claims completeness), and any error. It is injectable so
// tests drive the validator without a real binary.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, max int64, env map[string]string) (stdout, stderr []byte, exit int, truncated bool, err error)
}

// CommandError is the sanitized, persisted record of one failed stage. Summary
// never contains tokens, auth values, home/temp paths, or account data.
type CommandError struct {
	Stage       Stage
	Summary     string
	TimedOut    bool
	Remediation string
}

// Result is the outcome of validating one candidate. ConfigValid is set when
// `config validate` passes; StartupValid is set when both commands pass. Error
// is non-nil on any failure. Diagnostic carries the full sanitized output of
// the failed stage for ephemeral verbose diagnostics; it is nil on success and
// is never persisted.
type Result struct {
	ConfigValid  bool
	StartupValid bool
	Error        *CommandError
	Diagnostic   *CommandDiagnostic
}

// CommandDiagnostic is the ephemeral, human-readable record of one failed
// step: the full sanitized output — bounded only by the capture cap, never by
// the persisted-summary bound — plus whether capture dropped bytes. It is
// carried out of validate for `reconcile --verbose` rendering and is never
// persisted to state, history, or any other durable artifact.
type CommandDiagnostic struct {
	Stage      Stage
	FullOutput string
	Truncated  bool
	ExitCode   int
}

// InternalDiagnostic builds a CommandDiagnostic for a polytoken-quota-own
// failure (render/stage/publish) that never ran an external command. The
// error chain is redacted with the same unbounded sanitizer as captured
// command output, so verbose diagnostics can show the full sanitized error;
// any persisted summary keeps its separate 1024-byte bound. ExitCode is 0:
// no external process ran.
func InternalDiagnostic(stage Stage, err error) CommandDiagnostic {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return CommandDiagnostic{
		Stage:      stage,
		FullOutput: sanitizeOutput([]byte(msg)),
	}
}

// Runner validates a single staged candidate. Binary is the Polytoken executable
// path; Commands runs each command; MaxOutput bounds combined captured output
// (defaults to 262144 = 256 KiB); Sanitize redacts captured output before
// persistence without applying a length bound (defaults to sanitizeOutput) — the
// runner composes the redactor with the head+tail summary bound itself.
type Runner struct {
	Binary    string
	Commands  CommandRunner
	MaxOutput int64
	Sanitize  func([]byte) string
	Env       map[string]string
	// EnvLookup resolves a catalog auth env-ref name (e.g. NEURALWATT_API_KEY)
	// to its value for the validation subprocess. When nil the process
	// environment (os.Getenv) is used. Only non-empty resolutions are threaded
	// into the child; the parent environment is never inherited wholesale.
	EnvLookup func(string) string
}

// envLookup returns the configured env resolver, defaulting to the process
// environment so production resolves ${VAR} from os.Getenv while tests inject
// a deterministic fake.
func (r Runner) envLookup() func(string) string {
	if r.EnvLookup != nil {
		return r.EnvLookup
	}
	return os.Getenv
}

// defaultMaxOutput bounds the combined stdout+stderr captured per command.
// The bound is a memory guard, not a presentation choice: full sanitized
// output is surfaced to `reconcile --verbose` diagnostics, so it is generous
// (256 KiB, matching the in-repo HistoryRecordEncodedBytes precedent) while
// still bounding a runaway command.
const defaultMaxOutput int64 = 262144

// truncationMarker returns the terminal marker line appended to full
// diagnostic output when capture actually dropped or clipped bytes. It states
// the capture cap that applied — the number of dropped bytes is unknowable
// once gone — so the marker always reflects the cap that actually bounded the
// run, not a fixed constant.
func truncationMarker(max int64) string {
	return fmt.Sprintf("…[output truncated at %d bytes]…", max)
}

// fullOutput composes terminal full diagnostic output: the sanitized text,
// with the truncation marker appended as a terminal line when and only when
// capture dropped or clipped bytes. The marker names the cap that applied.
func fullOutput(max int64, sanitized string, truncated bool) string {
	if !truncated {
		return sanitized
	}
	if sanitized != "" && !strings.HasSuffix(sanitized, "\n") {
		sanitized += "\n"
	}
	return sanitized + truncationMarker(max) + "\n"
}

// maxSummaryBytes bounds the length of a persisted sanitized summary.
const maxSummaryBytes = 1024

// summaryElision marks elided middle content in a bounded summary.
const summaryElision = "\n…[truncated]…\n"

// boundSummary keeps the head and the tail of an over-long summary: the head
// identifies the command context, while the tail carries where failing
// commands report their actual error. Cuts respect UTF-8 rune boundaries and
// the result never exceeds maxSummaryBytes.
func boundSummary(s string) string {
	if len(s) <= maxSummaryBytes {
		return s
	}
	tailLen := maxSummaryBytes - maxSummaryBytes/4 - len(summaryElision)
	head := trimTrailingPartialRune(s[:maxSummaryBytes/4])
	tail := trimLeadingPartialRune(s[len(s)-tailLen:])
	return head + summaryElision + tail
}

// trimTrailingPartialRune drops trailing bytes until the string is valid UTF-8.
func trimTrailingPartialRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// trimLeadingPartialRune drops leading bytes until the string is valid UTF-8.
func trimLeadingPartialRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}

// Validate runs `config validate` then (on success) `doctor` against the staged
// candidate and returns a sanitized Result. Both commands share one timeout
// context derived from parent + timeout, are invoked as direct executables (no
// shell) with --config-dir/--working-dir from the candidate, and skip doctor
// entirely after a config failure. The candidate is cleaned up on every exit
// path.
func (r Runner) Validate(ctx context.Context, c staging.Candidate, timeout time.Duration) Result {
	max := r.maxOutput()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Stage 1: config validate.
	out, errOut, exit, truncated, runErr := r.Commands.Run(runCtx, r.Binary, configValidateArgs(c), max, doctorEnv(r.Env, c, r.envLookup()))
	if runErr != nil || exit != 0 {
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = c.Cleanup()
		cmdErr, diag := r.fail(ConfigValidate, out, errOut, exit, truncated, timedOut)
		return Result{Error: cmdErr, Diagnostic: diag}
	}

	// Stage 2: doctor (only after config validation passed).
	out, errOut, exit, truncated, runErr = r.Commands.Run(runCtx, r.Binary, doctorArgs(c), max, doctorEnv(r.Env, c, r.envLookup()))
	if runErr != nil || exit != 0 {
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = c.Cleanup()
		cmdErr, diag := r.fail(Doctor, out, errOut, exit, truncated, timedOut)
		return Result{ConfigValid: true, Error: cmdErr, Diagnostic: diag}
	}

	_ = c.Cleanup()
	return Result{ConfigValid: true, StartupValid: true}
}

// fail builds the sanitized persisted CommandError and the ephemeral full
// CommandDiagnostic for one failed stage. The persisted summary bound applies
// to the final composed string so the coarse command prefix fits; the
// diagnostic's full output is bounded only by the capture cap (plus the
// truncation marker when capture dropped bytes), and both carry the same
// injectable sanitizer so tests and production share one redaction path.
func (r Runner) fail(stage Stage, stdout, stderr []byte, exit int, truncated bool, timedOut bool) (*CommandError, *CommandDiagnostic) {
	combined := append(append([]byte(nil), stdout...), stderr...)
	sanitized := r.sanitizer()(combined)
	return &CommandError{
			Stage:       stage,
			Summary:     boundSummary(summarize(stage, sanitized, exit)),
			TimedOut:    timedOut,
			Remediation: remediation(stage, timedOut),
		},
		&CommandDiagnostic{
			Stage:      stage,
			FullOutput: fullOutput(r.maxOutput(), sanitized, truncated),
			Truncated:  truncated,
			ExitCode:   exit,
		}
}

func (r Runner) maxOutput() int64 {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return defaultMaxOutput
}

func (r Runner) sanitizer() func([]byte) string {
	if r.Sanitize != nil {
		return r.Sanitize
	}
	return sanitizeOutput
}

// configValidateArgs and doctorArgs are the exact, ordered argv passed to the
// Polytoken binary for each stage. Global flags precede the subcommand.
func configValidateArgs(c staging.Candidate) []string {
	return []string{"--config-dir", c.ConfigDir, "--working-dir", c.WorkingDir, "config", "validate", "--user"}
}

func doctorArgs(c staging.Candidate) []string {
	return []string{"--working-dir", c.WorkingDir, "doctor"}
}

// doctorEnv builds the exact subprocess environment for a validation command:
// the explicit base vars, the staged XDG_CONFIG_HOME and HOME, and — for
// catalog/dynamic providers whose real auth is preserved in the staged config —
// the resolved ${VAR} references so polytoken can expand them. Only refs that
// resolve to a non-empty value are added; the parent process environment is
// never inherited wholesale, so unrelated inherited secrets stay out.
func doctorEnv(base map[string]string, c staging.Candidate, lookup func(string) string) map[string]string {
	env := make(map[string]string, len(base)+2+len(c.AuthEnvRefs))
	for key, value := range base {
		env[key] = value
	}
	env["XDG_CONFIG_HOME"] = c.UserConfigDir
	env["HOME"] = c.Root
	for _, ref := range c.AuthEnvRefs {
		if v := lookup(ref); v != "" {
			env[ref] = v
		}
	}
	return env
}

// summarize prefixes the sanitized output with the coarse command name so the
// persisted record identifies which step failed.
func summarize(stage Stage, sanitized string, exit int) string {
	cmd := coarseCommand(stage)
	if strings.TrimSpace(sanitized) != "" {
		return cmd + ": " + sanitized
	}
	if exit >= 0 {
		return fmt.Sprintf("%s: exited with status %d", cmd, exit)
	}
	return cmd + ": command failed without diagnostic output"
}

func coarseCommand(stage Stage) string {
	switch stage {
	case ConfigValidate:
		return "config validate"
	case Doctor:
		return "doctor"
	}
	return string(stage)
}

// remediation returns a short, safe hint for the consuming coordinator/doctor.
func remediation(stage Stage, timedOut bool) string {
	switch {
	case stage == ConfigValidate && timedOut:
		return "increase the validation timeout or reduce startup config cost"
	case stage == ConfigValidate:
		return "re-run with `reconcile --dry-run --keep-staging` to inspect the retained staged config.yaml"
	case stage == Doctor && timedOut:
		return "increase the validation timeout or reduce startup check cost"
	case stage == Doctor:
		return "re-run with `reconcile --dry-run --keep-staging` and run `polytoken doctor` against the retained staged config and working dirs"
	}
	return ""
}

// --- production CommandRunner (direct exec, no shell, bounded output) -------

// ExecRunner is the production CommandRunner backed by exec.CommandContext. The
// binary is invoked directly — no shell is ever spawned. When the context is
// cancelled (e.g. timeout), CommandContext kills the child process. Combined
// stdout+stderr is bounded to max bytes during capture so memory stays bounded
// even for runaway output.
type ExecRunner struct{}

// Run invokes name with args directly under ctx. When env is non-nil the
// child receives EXACTLY those variables: the parent process environment is
// never merged in, so inherited credentials and account-bearing variables
// cannot leak into the validation subprocess. Callers own including PATH and
// any other variables the binary genuinely needs.
func (ExecRunner) Run(ctx context.Context, name string, args []string, max int64, env map[string]string) (stdout, stderr []byte, exit int, truncated bool, err error) {
	if max <= 0 {
		max = defaultMaxOutput
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = exactEnvironment(env)
	}
	budget := &captureBudget{remaining: max}
	out, errw := &boundedWriter{budget: budget}, &boundedWriter{budget: budget}
	cmd.Stdout = out
	cmd.Stderr = errw

	runErr := cmd.Run()
	stdout, stderr = out.bytes(), errw.bytes()
	truncated = budget.isTruncated()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
		return stdout, stderr, exit, truncated, runErr
	}
	return stdout, stderr, 0, truncated, nil
}

func exactEnvironment(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// captureBudget is the shared remaining byte budget for one command's combined
// stdout+stderr. Both streams draw from it, bounding the total. truncated
// records whether any write was dropped entirely or clipped partway — it is
// written under the same mutex and read only after the command has exited and
// both writer goroutines have joined.
type captureBudget struct {
	mu        sync.Mutex
	remaining int64
	truncated bool
}

// isTruncated reports whether capture dropped or clipped any bytes.
func (b *captureBudget) isTruncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// boundedWriter appends to an internal buffer until the shared budget is
// exhausted, then records the drop or clip and discards further bytes (still
// reporting a full write so the command is not perturbed by an error).
type boundedWriter struct {
	budget *captureBudget
	buf    bytes.Buffer
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	if w.budget.remaining <= 0 {
		if len(p) > 0 {
			w.budget.truncated = true
		}
		return len(p), nil
	}
	n := int64(len(p))
	if n > w.budget.remaining {
		n = w.budget.remaining
		w.budget.truncated = true
	}
	w.buf.Write(p[:n])
	w.budget.remaining -= n
	return len(p), nil
}

func (w *boundedWriter) bytes() []byte { return w.buf.Bytes() }

// --- redaction --------------------------------------------------------------

// DefaultSanitize redacts secret-bearing and path-identifying content from
// captured command output so a persisted CommandError.Summary never leaks
// tokens, auth values, home or temp paths, account data, or arbitrary stderr.
// It is deliberately conservative: credential assignments, bearer tokens, home
// and temp paths, account identifiers, and long opaque blobs are replaced with
// placeholders, and the result is bounded in length.
func DefaultSanitize(b []byte) string {
	return truncateSummary(sanitizeOutput(b))
}

// truncateSummary hard-bounds a sanitized string at maxSummaryBytes.
func truncateSummary(s string) string {
	if len(s) > maxSummaryBytes {
		s = s[:maxSummaryBytes]
	}
	return s
}

// sanitizeOutput redacts secret-bearing and path-identifying content from
// captured command output without applying a length bound. ANSI escape
// sequences are stripped first so color codes cannot fragment the other
// patterns. The summary bound is a separate concern (boundSummary) so a
// failure summary can keep both the head and the tail of long output.
func sanitizeOutput(b []byte) string {
	s := reANSI.ReplaceAllString(string(b), "")
	// Redact URL userinfo (scheme://user:pass@host) before anything else can
	// split the credential across other patterns.
	s = reURLCred.ReplaceAllString(s, "${1}<redacted>@")
	s = reEmail.ReplaceAllString(s, "<account>")
	// Redact bearer tokens BEFORE credential assignments: a header like
	// "Authorization: Bearer <token>" is matched by reCredAssign first (it
	// treats "Authorization" as the key and "Bearer" as its value), which would
	// consume the "Bearer" word and leave the secret token bare for reBearer.
	// Running reBearer first redacts the whole "Bearer <token>" span.
	s = reBearer.ReplaceAllString(s, "${1} <redacted>")
	s = reCredAssign.ReplaceAllString(s, "${1}=<redacted>")
	s = reHome.ReplaceAllString(s, "<home>")
	s = reTemp.ReplaceAllString(s, "<tmp>")
	s = reBlob.ReplaceAllString(s, "<redacted>")
	return s
}

var (
	reANSI  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	reEmail = regexp.MustCompile(`(?i)\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	// reCredAssign matches credential-shaped key/value assignments in plain,
	// quoted, and JSON/YAML spellings: the key may be bare or quoted, the
	// separator may be = or :, and the value may be a quoted string or a bare
	// token. The key set covers the common credential names, not only api_key.
	reCredAssign = regexp.MustCompile(`(?i)["']?(api[_-]?key|api[_-]?secret|apikey|x-api-key|access[_-]?token|auth[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|client[_-]?secret|private[_-]?key|secret|password|passwd|token|authorization|credentials?|cookie|set-cookie)["']?\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s,}\]]+)`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._\-+/=]+`)
	reURLCred    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)
	reHome       = regexp.MustCompile(`/home/[^\s]+|/Users/[^\s]+`)
	reTemp       = regexp.MustCompile(`/tmp/[^\s]+|/var/folders/[^\s]+`)
	reBlob       = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)
)
