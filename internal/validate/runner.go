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
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/staging"
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
// bytes, the process exit code (0 on success, -1 when unavailable), and any
// error. It is injectable so tests drive the validator without a real binary.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, max int64, env map[string]string) (stdout, stderr []byte, exit int, err error)
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
// is non-nil on any failure.
type Result struct {
	ConfigValid  bool
	StartupValid bool
	Error        *CommandError
}

// Runner validates a single staged candidate. Binary is the Polytoken executable
// path; Commands runs each command; MaxOutput bounds combined captured output
// (defaults to 4096); Sanitize redacts captured output before persistence
// (defaults to DefaultSanitize).
type Runner struct {
	Binary    string
	Commands  CommandRunner
	MaxOutput int64
	Sanitize  func([]byte) string
	Env       map[string]string
}

// defaultMaxOutput bounds the combined stdout+stderr captured per command.
const defaultMaxOutput int64 = 4096

// maxSummaryBytes bounds the length of a persisted sanitized summary.
const maxSummaryBytes = 1024

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
	out, errOut, exit, runErr := r.Commands.Run(runCtx, r.Binary, configValidateArgs(c), max, doctorEnv(r.Env, c))
	if runErr != nil || exit != 0 {
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = c.Cleanup()
		return Result{Error: r.fail(ConfigValidate, out, errOut, exit, timedOut)}
	}

	// Stage 2: doctor (only after config validation passed).
	out, errOut, exit, runErr = r.Commands.Run(runCtx, r.Binary, doctorArgs(c), max, doctorEnv(r.Env, c))
	if runErr != nil || exit != 0 {
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = c.Cleanup()
		return Result{ConfigValid: true, Error: r.fail(Doctor, out, errOut, exit, timedOut)}
	}

	_ = c.Cleanup()
	return Result{ConfigValid: true, StartupValid: true}
}

// fail builds a sanitized CommandError for one failed stage.
func (r Runner) fail(stage Stage, stdout, stderr []byte, exit int, timedOut bool) *CommandError {
	combined := append(append([]byte(nil), stdout...), stderr...)
	return &CommandError{
		Stage:       stage,
		Summary:     summarize(stage, r.sanitizer()(combined), exit),
		TimedOut:    timedOut,
		Remediation: remediation(stage, timedOut),
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
	return DefaultSanitize
}

// configValidateArgs and doctorArgs are the exact, ordered argv passed to the
// Polytoken binary for each stage. Global flags precede the subcommand.
func configValidateArgs(c staging.Candidate) []string {
	return []string{"--config-dir", c.ConfigDir, "--working-dir", c.WorkingDir, "config", "validate", "--user"}
}

func doctorArgs(c staging.Candidate) []string {
	return []string{"--working-dir", c.WorkingDir, "doctor"}
}

func doctorEnv(base map[string]string, c staging.Candidate) map[string]string {
	env := make(map[string]string, len(base)+2)
	for key, value := range base {
		env[key] = value
	}
	env["XDG_CONFIG_HOME"] = c.UserConfigDir
	env["HOME"] = c.Root
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
		return "inspect the staged config.yaml for schema or startup errors"
	case stage == Doctor && timedOut:
		return "increase the validation timeout or reduce startup check cost"
	case stage == Doctor:
		return "run `polytoken doctor` against the staged config and working dirs"
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
func (ExecRunner) Run(ctx context.Context, name string, args []string, max int64, env map[string]string) (stdout, stderr []byte, exit int, err error) {
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
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
		return stdout, stderr, exit, runErr
	}
	return stdout, stderr, 0, nil
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
// stdout+stderr. Both streams draw from it, bounding the total.
type captureBudget struct {
	mu        sync.Mutex
	remaining int64
}

// boundedWriter appends to an internal buffer until the shared budget is
// exhausted, then silently drops further bytes (still reporting a full write so
// the command is not perturbed by an error).
type boundedWriter struct {
	budget *captureBudget
	buf    bytes.Buffer
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	if w.budget.remaining <= 0 {
		return len(p), nil
	}
	n := int64(len(p))
	if n > w.budget.remaining {
		n = w.budget.remaining
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
	s := string(b)
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
	if len(s) > maxSummaryBytes {
		s = s[:maxSummaryBytes]
	}
	return s
}

var (
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
