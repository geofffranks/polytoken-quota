package cli

// routing CLI tests for the diagnostic redesign: dispatch, rendering, exit
// codes, JSON validity, and no-mutation-on-error.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/service"
)

// TestRoutingBareDispatch verifies routing (effective chains) dispatches and
// exits 0; --json emits valid JSON.
func TestRoutingBareDispatch(t *testing.T) {
	t.Run("text exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !strings.Contains(out.String(), "routing") {
			t.Fatalf("output missing routing: %q", out.String())
		}
	})

	t.Run("json valid", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		var r map[string]any
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		if r["routing_enabled"] != false {
			t.Fatalf("want routing_enabled=false for zero snapshot; got %v", r["routing_enabled"])
		}
		if _, ok := r["routes"]; !ok {
			t.Fatal("missing routes key")
		}
		if _, ok := r["errors"]; !ok {
			t.Fatal("missing errors key")
		}
	})
}

// TestRoutingExplainDispatch verifies routing explain parses, prints, and
// exits 0; --json emits valid JSON.
func TestRoutingJSONFailuresEmitOneEnvelope(t *testing.T) {
	commands := []struct {
		name      string
		baseArgs  []string
		arrayKeys []string
	}{
		{name: "bare", baseArgs: []string{"routing"}, arrayKeys: []string{"routes", "errors"}},
		{name: "explain", baseArgs: []string{"routing", "explain"}, arrayKeys: []string{"ranks", "routes", "errors"}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			cases := []struct {
				name string
				args []string
				deps Dependencies
			}{
				{name: "invalid arguments", args: append(append([]string{}, command.baseArgs...), "--json", "--bogus"), deps: newDepsSpy().Dependencies()},
				{name: "snapshot builder unavailable", args: append(append([]string{}, command.baseArgs...), "--json"), deps: Dependencies{}},
				{name: "fatal snapshot", args: append(append([]string{}, command.baseArgs...), "--json"), deps: Dependencies{SnapshotBuilder: &service.Coordinator{}}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					var stdout, stderr bytes.Buffer
					code := Run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, tc.deps)
					if code != ExitRejected {
						t.Fatalf("exit=%d want 1", code)
					}
					if stderr.Len() != 0 {
						t.Fatalf("stderr=%q want empty", stderr.String())
					}
					if strings.Contains(stdout.String(), "\x1b[") {
						t.Fatalf("stdout contains ANSI: %q", stdout.String())
					}
					decoder := json.NewDecoder(&stdout)
					var envelope map[string]any
					if err := decoder.Decode(&envelope); err != nil {
						t.Fatalf("invalid JSON object: %v\n%s", err, stdout.String())
					}
					var extra any
					if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
						t.Fatalf("stdout contains more than one JSON object: err=%v extra=%v", err, extra)
					}
					errorText, ok := envelope["error"].(string)
					if !ok || strings.TrimSpace(errorText) == "" {
						t.Fatalf("missing non-empty error: %v", envelope)
					}
					if strings.Contains(errorText, "\x1b[") || strings.Contains(errorText, "CANARY") {
						t.Fatalf("error not sanitized: %q", errorText)
					}
					for _, key := range command.arrayKeys {
						value, ok := envelope[key].([]any)
						if !ok || len(value) != 0 {
							t.Fatalf("%s=%T(%v), want []", key, envelope[key], envelope[key])
						}
					}
				})
			}
		})
	}
}

func TestRoutingExplainDispatch(t *testing.T) {
	t.Run("text exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "explain"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})

	t.Run("json valid", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "explain", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		var r map[string]any
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		for _, key := range []string{"routing_enabled", "ranks", "routes", "errors"} {
			if _, ok := r[key]; !ok {
				t.Fatalf("JSON missing %q: %s", key, out.String())
			}
		}
	})

	t.Run("invalid args exit 1 no mutation", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "explain", "--bogus"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 0 {
			t.Fatalf("invalid args mutated %d times", spy.Mutations)
		}
	})
}

// TestRoutingEnableDisable verifies enable/disable dispatch with a provider
// argument and rejects bad args without mutation.
func TestRoutingEnableDisable(t *testing.T) {
	t.Run("enable dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "enable", "codex"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if spy.EnableProvider != "codex" {
			t.Fatalf("enable provider=%q want codex", spy.EnableProvider)
		}
		if !strings.Contains(out.String(), "routing enabled") {
			t.Fatalf("output: %q", out.String())
		}
	})

	t.Run("disable dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "disable", "zai"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if spy.DisableProvider != "zai" {
			t.Fatalf("disable provider=%q want zai", spy.DisableProvider)
		}
	})

	t.Run("no provider exits 1 no mutation", func(t *testing.T) {
		for _, args := range [][]string{
			{"routing", "enable"},
			{"routing", "disable"},
		} {
			spy := newDepsSpy()
			code := Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
			if code != ExitRejected {
				t.Fatalf("args=%v exit=%d want 1", args, code)
			}
			if spy.Mutations != 0 {
				t.Fatalf("mutated %d", spy.Mutations)
			}
		}
	})

	t.Run("error exits 1", func(t *testing.T) {
		spy := &outcomeSpy{outcome: service.Outcome{Accepted: false, Error: errors.New("mapping not configured")}}
		code := Run(context.Background(), []string{"routing", "enable", "bogus"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
	})
}

// TestRoutingResetDispatch verifies reset dispatches correctly.
func TestRoutingResetDispatch(t *testing.T) {
	t.Run("dispatches", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "reset"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.ResetCalled {
			t.Fatal("reset not called")
		}
		if !strings.Contains(out.String(), "routing reset") {
			t.Fatalf("output: %q", out.String())
		}
	})

	t.Run("extra args rejected", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "reset", "extra"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 0 {
			t.Fatalf("mutated %d", spy.Mutations)
		}
	})
}

// --- top-level check command tests ---

// TestCheckDispatch verifies check (promoted to top-level) parses flags, calls
// the coordinator, and returns the correct exit code.
func TestCheckDispatch(t *testing.T) {
	t.Run("clean exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"check"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d want 0", code)
		}
		if spy.Mutations != 1 {
			t.Fatalf("mutations=%d want 1", spy.Mutations)
		}
	})

	t.Run("problem exit 2", func(t *testing.T) {
		spy := newDepsSpy()
		spy.QuotaCheckSet = true
		spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 3, Problem: true}
		code := Run(context.Background(), []string{"check"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
		if code != ExitPending {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("rejected exit 1", func(t *testing.T) {
		spy := newDepsSpy()
		spy.QuotaCheckSet = true
		spy.QuotaCheckOutcome = service.Outcome{Accepted: false, Error: errors.New("acquire lock: busy")}
		var stderr bytes.Buffer
		code := Run(context.Background(), []string{"check"}, strings.NewReader(""), io.Discard, &stderr, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want 1", code)
		}
		if !strings.Contains(stderr.String(), "acquire lock") {
			t.Fatalf("stderr missing error: %q", stderr.String())
		}
	})
}

func TestCheckJSON(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 5, Problem: true}
	var out bytes.Buffer
	code := Run(context.Background(), []string{"check", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want 2", code)
	}
	var parsed struct {
		Accepted bool   `json:"accepted"`
		Revision uint64 `json:"revision"`
		Problem  bool   `json:"problem"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if !parsed.Accepted || parsed.Revision != 5 || !parsed.Problem {
		t.Fatalf("unexpected JSON: %+v", parsed)
	}
}

func TestCheckProviderFlag(t *testing.T) {
	spy := newDepsSpy()
	Run(context.Background(), []string{"check", "--provider", "zai"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
	if spy.QuotaCheckProvider != "zai" {
		t.Fatalf("provider=%q want zai", spy.QuotaCheckProvider)
	}
}

func TestCheckReconcileFlag(t *testing.T) {
	spy := newDepsSpy()
	Run(context.Background(), []string{"check", "--reconcile"}, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
	if !spy.QuotaCheckReconcile {
		t.Fatal("reconcile=false want true")
	}
}

func TestCheckInvalidArgs(t *testing.T) {
	cases := [][]string{
		{"check", "--bogus"},
		{"check", "--provider"},
		{"check", "positional"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			code := Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, spy.Dependencies())
			if code != ExitRejected {
				t.Fatalf("args=%v exit=%d want 1", args, code)
			}
			if spy.Mutations != 0 {
				t.Fatalf("invalid args mutated: %d", spy.Mutations)
			}
		})
	}
}

func TestCheckJSONRejectedEmitsEnvelope(t *testing.T) {
	t.Run("invalid args", func(t *testing.T) {
		var out bytes.Buffer
		code := Run(context.Background(), []string{"check", "--json", "--bogus"}, strings.NewReader(""), &out, io.Discard, newDepsSpy().Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want rejected", code)
		}
		var got mutationJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("invalid envelope: %v (%q)", err, out.String())
		}
		if got.Accepted || got.Error == "" {
			t.Fatalf("envelope=%+v", got)
		}
	})
}

// TestCheckNoSecrets verifies no credentials appear in output.
func TestCheckNoSecrets(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 1, Error: errors.New("bounded http: api_key=sk-live-1234567890wxyz")}
	var out, stderr bytes.Buffer
	Run(context.Background(), []string{"check"}, strings.NewReader(""), &out, &stderr, spy.Dependencies())
	combined := out.String() + stderr.String()
	for _, secret := range []string{"sk-live-1234567890wxyz", "Bearer abc123", "password=hunter2"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret %q leaked in output: %s", secret, combined)
		}
	}
}
