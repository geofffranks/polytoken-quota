package validate

// These tests verify the bounded startup-equivalent validator. The validator
// runs two Polytoken commands against a staged candidate — `config validate`
// then `doctor`, both with --config-dir/--working-dir pointing at the staged
// roots — via direct executable invocation (no shell), with a shared timeout
// context, bounded combined output, redacted summaries, and staging cleanup on
// every exit path. A package-local commandSpy backs CommandRunner so no real
// binary is invoked for the behavioral tests; ExecRunner is exercised directly
// for the no-shell and output-bounding guarantees.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// --- fake CommandRunner -----------------------------------------------------

// commandSpy is the package-local fake CommandRunner. It records the args of
// every Run call. FailAt (>=0) is the 0-based call index that exits non-zero;
// -1 (the default via newSpy) disables failure. Block makes Run wait on the
// context to exercise the shared timeout path. Stderr is the canned output
// returned with a failure or timeout.
type commandSpy struct {
	Args   [][]string
	Envs   []map[string]string
	FailAt int
	Block  bool
	Stderr []byte
	calls  int
}

func newSpy() *commandSpy { return &commandSpy{FailAt: -1} }

func (s *commandSpy) Run(ctx context.Context, name string, args []string, max int64, env map[string]string) (stdout, stderr []byte, exit int, err error) {
	cp := append([]string(nil), args...)
	s.Args = append(s.Args, cp)
	if env != nil {
		copyEnv := make(map[string]string, len(env))
		for key, value := range env {
			copyEnv[key] = value
		}
		s.Envs = append(s.Envs, copyEnv)
	} else {
		s.Envs = append(s.Envs, nil)
	}
	idx := s.calls
	s.calls++
	canned := append([]byte(nil), s.Stderr...)

	if s.Block {
		<-ctx.Done()
		return nil, canned, 0, ctx.Err()
	}
	if s.FailAt >= 0 && idx == s.FailAt {
		return nil, canned, 1, errors.New("command exited non-zero")
	}
	return nil, nil, 0, nil
}

// --- shared test helpers ----------------------------------------------------

// sanitize aliases the production redactor so the brief's Runner literal stays
// self-describing.
var sanitize = DefaultSanitize

// newRunner wires a Runner with the default output cap and the runner's
// default unbounded redactor, so failure summaries exercise head+tail bounding.
func newRunner(c CommandRunner) Runner {
	return Runner{Binary: "/opt/polytoken", Commands: c, MaxOutput: 4096}
}

// TestExecRunnerDoesNotInheritParentEnvironment proves the validation
// subprocess sees ONLY the explicitly provided variables: an inherited
// credential-shaped variable in the parent process must be invisible to the
// child, so unrelated secrets can never reach the validated binary.
func TestExecRunnerDoesNotInheritParentEnvironment(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	t.Setenv("SECRET_CLOUD_TOKEN", "canary-must-not-leak")
	out, _, exit, err := (ExecRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", "printf %s:%s \"$SECRET_CLOUD_TOKEN\" \"$POLYTOKEN_TEST_ENV\""}, 128, map[string]string{"POLYTOKEN_TEST_ENV": "present"})
	if err != nil || exit != 0 || string(out) != ":present" {
		t.Fatalf("out=%q exit=%d err=%v (inherited secret visible to child?)", out, exit, err)
	}
}

func TestExecRunnerPassesExplicitEnvironment(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	out, _, exit, err := (ExecRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", "printf %s \"$POLYTOKEN_TEST_ENV\""}, 128, map[string]string{"POLYTOKEN_TEST_ENV": "present"})
	if err != nil || exit != 0 || string(out) != "present" {
		t.Fatalf("out=%q exit=%d err=%v", out, exit, err)
	}
}

// candidate returns a lightweight staging candidate (no-op cleanup) for tests
// that do not observe cleanup.
func candidate() staging.Candidate {
	return staging.Candidate{ConfigDir: "/private/config", WorkingDir: "/private/work"}
}

// stubSources is a minimal SourceMaterializer that yields one valid global
// config layer and no project layer, enough for staging.Builder to materialize
// an observable candidate.
type stubSources struct{}

func (stubSources) Global(context.Context) (staging.Layer, error) {
	return staging.Layer{Config: []byte("defaults:\n  full: zai/glm\n")}, nil
}
func (stubSources) Project(context.Context, target.Resolved) (staging.Layer, bool, error) {
	return staging.Layer{}, false, nil
}

// realCandidate builds a candidate with a real (observable) cleanup so tests can
// assert the staging root is removed.
func realCandidate(t *testing.T) staging.Candidate {
	t.Helper()
	b := staging.Builder{TempRoot: t.TempDir(), AuthMode: staging.AuthInert, Sources: stubSources{}}
	c, err := b.Build(context.Background(), target.Resolved{ID: "global", Global: true}, reconcile.Plan{}, nil)
	if err != nil {
		t.Fatalf("build candidate: %v", err)
	}
	return c
}

// --- behavioral tests -------------------------------------------------------

// TestValidateExactCommandsAndOrder asserts the exact argv and order: config
// validate first, then doctor, both with --config-dir/--working-dir from the
// candidate, and no extra flags.
func TestValidateExactCommandsAndOrder(t *testing.T) {
	spy := newSpy()
	r := Runner{Binary: "/opt/polytoken", Commands: spy, MaxOutput: 4096, Sanitize: sanitize}
	c := staging.Candidate{Root: "/private/root", ConfigDir: "/private/config", UserConfigDir: "/private/user", WorkingDir: "/private/work"}

	got := r.Validate(context.Background(), c, time.Second)
	if !got.ConfigValid || !got.StartupValid {
		t.Fatalf("expected success, got %+v", got)
	}

	want := [][]string{
		{"--config-dir", "/private/config", "--working-dir", "/private/work", "config", "validate", "--user"},
		{"--working-dir", "/private/work", "doctor"},
	}
	if spy.Envs[0]["XDG_CONFIG_HOME"] != "/private/user" || spy.Envs[0]["HOME"] != "/private/root" {
		t.Fatalf("config validate environment=%v", spy.Envs[0])
	}
	if spy.Envs[1]["XDG_CONFIG_HOME"] != "/private/user" || spy.Envs[1]["HOME"] != "/private/root" {
		t.Fatalf("doctor environment=%v", spy.Envs[1])
	}
	if !reflect.DeepEqual(spy.Args, want) {
		t.Fatalf("args\ngot=%v\nwant=%v", spy.Args, want)
	}
}

// TestConfigFailureSkipsDoctor asserts that a non-zero config validate result
// records only the config_validate stage error and never runs doctor.
func TestConfigFailureSkipsDoctor(t *testing.T) {
	spy := &commandSpy{FailAt: 0}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Second)

	if len(spy.Args) != 1 {
		t.Fatalf("expected exactly one command, got %d: %v", len(spy.Args), spy.Args)
	}
	if got.Error == nil || got.Error.Stage != ConfigValidate {
		t.Fatalf("expected config_validate error, got %+v", got)
	}
	if got.ConfigValid {
		t.Fatalf("ConfigValid should be false on config failure: %+v", got)
	}
}

func TestSilentCommandFailureIncludesExitStatus(t *testing.T) {
	spy := &commandSpy{FailAt: 0}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Second)
	if got.Error == nil || got.Error.Summary != "config validate: exited with status 1" {
		t.Fatalf("summary=%q want silent exit diagnostic", got.Error.Summary)
	}
}

func TestWhitespaceOnlyCommandFailureIncludesExitStatus(t *testing.T) {
	spy := &commandSpy{FailAt: 0, Stderr: []byte("\n \t\n")}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Second)
	if got.Error == nil || got.Error.Summary != "config validate: exited with status 1" {
		t.Fatalf("summary=%q want whitespace-only output to use exit diagnostic", got.Error.Summary)
	}
}

// TestDoctorFailureRecordsDoctorStage asserts that when config validate passes
// but doctor fails, the error stage is doctor and ConfigValid is true.
func TestDoctorFailureRecordsDoctorStage(t *testing.T) {
	spy := &commandSpy{FailAt: 1}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Second)

	if len(spy.Args) != 2 {
		t.Fatalf("expected two commands, got %d: %v", len(spy.Args), spy.Args)
	}
	if got.Error == nil || got.Error.Stage != Doctor {
		t.Fatalf("expected doctor error, got %+v", got)
	}
	if !got.ConfigValid {
		t.Fatalf("ConfigValid should be true when config passed: %+v", got)
	}
}

// TestTimeoutAndRedaction asserts that an expired shared timeout context kills
// the run, records TimedOut, and that the persisted summary is redacted of
// tokens and home paths present in captured output.
func TestTimeoutAndRedaction(t *testing.T) {
	spy := &commandSpy{Block: true, Stderr: []byte("token=SECRET /home/alice")}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Nanosecond)

	if got.Error == nil || !got.Error.TimedOut {
		t.Fatalf("expected a timeout error, got %+v", got)
	}
	if strings.Contains(got.Error.Summary, "SECRET") {
		t.Fatalf("summary leaked token value: %q", got.Error.Summary)
	}
	if strings.Contains(got.Error.Summary, "alice") {
		t.Fatalf("summary leaked home path: %q", got.Error.Summary)
	}
	if got.Error.Stage != ConfigValidate {
		t.Fatalf("expected config_validate stage, got %q", got.Error.Stage)
	}
	if got.Error.Remediation == "" {
		t.Fatalf("expected non-empty remediation")
	}
}

// TestCleanupOnEveryResult asserts the staging candidate is cleaned up on every
// result path: success, config failure, doctor failure, and timeout.
func TestCleanupOnEveryResult(t *testing.T) {
	cases := []struct {
		name string
		spy  *commandSpy
	}{
		{"success", newSpy()},
		{"config_failure", &commandSpy{FailAt: 0}},
		{"doctor_failure", &commandSpy{FailAt: 1}},
		{"timeout", &commandSpy{Block: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := realCandidate(t)
			root := c.Root
			newRunner(tc.spy).Validate(context.Background(), c, 100*time.Millisecond)
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging root %q was not cleaned up: %v", root, err)
			}
		})
	}
}

// --- redaction tests --------------------------------------------------------

// TestSanitizeRedactsSecrets asserts the production redactor strips tokens,
// auth values, home/temp paths, account data, and long opaque blobs from the
// persisted summary.
func TestSanitizeRedactsSecrets(t *testing.T) {
	cases := map[string]string{
		"token assignment": "token=SECRET-abc123 /home/alice",
		"api key":          "api_key: sk-live-1234567890wxyz",
		"bearer header":    "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig",
		"home path":        "loading /home/alice/.polytoken/config.yaml",
		"temp path":        "wrote /tmp/quota-stage-xyz/config.yaml done",
		"email":            "account alice@example.com registered",
		"long blob":        "key=AKIAIOSFODNN7EXAMPLE0123456789abcdefghij",
		// Quoted/JSON spellings and additional credential key names.
		"json quoted key":  `parse error near {"api_key":"sk-live-short"}`,
		"json spaced":      `"refresh_token" : "SECRET-rt-1"`,
		"client secret":    "client_secret=SECRET-cs-2 rejected",
		"private key":      "private_key: SECRET-pk-3",
		"x-api-key header": "x-api-key: SECRET-xk-4",
		"cookie":           `Cookie: session=SECRET-ck-5`,
		"url userinfo":     "GET https://alice:SECRET-pw-6@api.test/v1 failed",
	}
	forbidden := []string{
		"SECRET", "alice", "sk-live-", "eyJhbGci", "example.com",
		"AKIAIOSFODNN7EXAMPLE", "quota-stage-xyz",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := DefaultSanitize([]byte(in))
			for _, f := range forbidden {
				if !strings.Contains(in, f) {
					continue // only forbid fragments actually present in this input
				}
				if strings.Contains(out, f) {
					t.Fatalf("sanitize leaked %q\ninput =%q\noutput=%q", f, in, out)
				}
			}
		})
	}
}

// --- bounding tests ---------------------------------------------------------

// TestBoundedCombinedOutput asserts the shared capture budget bounds the total
// of stdout and stderr to the configured maximum, even when both streams are
// written far past the limit.
func TestBoundedCombinedOutput(t *testing.T) {
	const max int64 = 64
	budget := &captureBudget{remaining: max}
	out := &boundedWriter{budget: budget}
	errw := &boundedWriter{budget: budget}

	out.Write(bytes.Repeat([]byte("o"), 1000))
	errw.Write([]byte("error: boom\n"))

	combined := append(append([]byte(nil), out.bytes()...), errw.bytes()...)
	if int64(len(combined)) > max {
		t.Fatalf("combined output %d exceeds max %d", len(combined), max)
	}
	if int64(len(combined)) != max {
		t.Fatalf("expected combined to use the full budget %d, got %d", max, len(combined))
	}
}

// TestExecRunnerDirectInvocation exercises the production runner against a real
// direct executable (/bin/echo, no shell) and asserts the captured output is
// bounded to the configured maximum.
func TestExecRunnerDirectInvocation(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("/bin/echo unavailable on this platform")
	}
	const max int64 = 128
	big := strings.Repeat("x", 5000)
	r := ExecRunner{}

	stdout, stderr, exit, err := r.Run(context.Background(), "/bin/echo", []string{big}, max, nil)
	if err != nil || exit != 0 {
		t.Fatalf("echo: exit=%d err=%v", exit, err)
	}

	combined := append(append([]byte(nil), stdout...), stderr...)
	if int64(len(combined)) > max {
		t.Fatalf("combined output %d exceeds max %d", len(combined), max)
	}
}

// TestValidateThreadsCatalogAuthEnvRefs proves the validator resolves a staged
// candidate's catalog auth env refs and threads exactly the present ones into
// BOTH the config-validate and doctor subprocess environments, so polytoken can
// expand them. Unset/empty refs are not threaded.
func TestValidateThreadsCatalogAuthEnvRefs(t *testing.T) {
	spy := newSpy()
	lookup := func(name string) string {
		if name == "NEURALWATT_API_KEY" {
			return "resolved-neuralwatt-key"
		}
		return ""
	}
	r := Runner{Binary: "/opt/polytoken", Commands: spy, MaxOutput: 4096, Sanitize: sanitize, EnvLookup: lookup}
	c := staging.Candidate{
		Root:          "/private/root",
		ConfigDir:     "/private/config",
		UserConfigDir: "/private/user",
		WorkingDir:    "/private/work",
		AuthEnvRefs:   []string{"NEURALWATT_API_KEY", "UNSET_PROVIDER_KEY"},
	}
	if got := r.Validate(context.Background(), c, time.Second); !got.ConfigValid || !got.StartupValid {
		t.Fatalf("expected success, got %+v", got)
	}
	if len(spy.Envs) != 2 {
		t.Fatalf("expected two subprocess invocations, got %d", len(spy.Envs))
	}
	for i, env := range spy.Envs {
		if env["NEURALWATT_API_KEY"] != "resolved-neuralwatt-key" {
			t.Fatalf("env[%d] did not thread resolved catalog auth ref: %v", i, env)
		}
		if _, ok := env["UNSET_PROVIDER_KEY"]; ok {
			t.Fatalf("env[%d] threaded an unset/empty ref", i)
		}
	}
}

// TestValidateEnvLookupDefaultsToOsGetenv proves that when no EnvLookup is
// configured the validator resolves catalog auth refs from the process
// environment (the production path).
func TestValidateEnvLookupDefaultsToOsGetenv(t *testing.T) {
	t.Setenv("PQ_TEST_CATALOG_KEY", "from-process-env")
	spy := newSpy()
	r := newRunner(spy) // no EnvLookup set
	c := staging.Candidate{
		Root:          "/private/root",
		ConfigDir:     "/private/config",
		UserConfigDir: "/private/user",
		WorkingDir:    "/private/work",
		AuthEnvRefs:   []string{"PQ_TEST_CATALOG_KEY"},
	}
	r.Validate(context.Background(), c, time.Second)
	if spy.Envs[0]["PQ_TEST_CATALOG_KEY"] != "from-process-env" {
		t.Fatalf("default lookup did not resolve ref from process env: %v", spy.Envs[0])
	}
}

// TestValidateNoEnvRefsLeavesEnvIsolated proves a candidate with no catalog auth
// refs produces a subprocess env containing only the explicit base vars plus
// XDG_CONFIG_HOME/HOME — env isolation unchanged for static-only configs.
func TestValidateNoEnvRefsLeavesEnvIsolated(t *testing.T) {
	t.Setenv("LEAKED_SECRET", "must-not-appear")
	spy := newSpy()
	r := Runner{Binary: "/opt/polytoken", Commands: spy, MaxOutput: 4096, Sanitize: sanitize, Env: map[string]string{"PATH": "/usr/bin"}}
	c := staging.Candidate{Root: "/r", ConfigDir: "/c", UserConfigDir: "/u", WorkingDir: "/w"}
	r.Validate(context.Background(), c, time.Second)
	for i, env := range spy.Envs {
		if _, ok := env["LEAKED_SECRET"]; ok {
			t.Fatalf("env[%d] leaked an inherited secret", i)
		}
		if env["PATH"] != "/usr/bin" || env["XDG_CONFIG_HOME"] != "/u" || env["HOME"] != "/r" {
			t.Fatalf("env[%d] missing base vars: %v", i, env)
		}
	}
}
