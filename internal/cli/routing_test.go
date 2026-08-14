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
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TestRoutingDisplaySubcommandsRemoved verifies the routing display surfaces
// were removed (merged into `status`): bare routing, routing --json, and
// routing explain fail strictly without invoking any dependency.
func TestRoutingDisplaySubcommandsRemoved(t *testing.T) {
	for _, args := range [][]string{
		{"routing"},
		{"routing", "--json"},
		{"routing", "explain"},
		{"routing", "explain", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			spy := newDepsSpy()
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
			if code != ExitRejected {
				t.Fatalf("exit=%d want 1", code)
			}
			if spy.Mutations != 0 {
				t.Fatalf("removed display command invoked a dependency %d times", spy.Mutations)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q want empty", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("stderr missing rejection message")
			}
		})
	}
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

func TestCheckPrintsProviderAttemptStatuses(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{
		Accepted: true,
		Revision: 5,
		ProviderAttempts: []service.QuotaAttemptDiagnostic{
			{MappingID: "codex", Status: "fresh"},
			{MappingID: "zai", Status: "failed", Error: "request timed out"},
		},
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"check"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d want %d", code, ExitOK)
	}
	for _, want := range []string{"codex", "fresh", "zai", "failed", "request timed out"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q: %q", want, stdout.String())
		}
	}
}

func TestCheckQuietSuppressesAllOutput(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{
		Accepted: true,
		Revision: 5,
		Problem:  true,
		Targets: []service.TargetOutcome{{TargetID: "global", Pending: &state.ApplyFailure{
			Stage: "config_validate", Summary: "invalid model", Remediation: "inspect config",
		}}},
		ProviderAttempts: []service.QuotaAttemptDiagnostic{{MappingID: "zai", Status: "failed", Error: "request timed out"}},
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"check", "--quiet"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want %d", code, ExitPending)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("quiet check wrote output: stdout=%q stderr=%q", stdout.String(), stderr.String())
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
