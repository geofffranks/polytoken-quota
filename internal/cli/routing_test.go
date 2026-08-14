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

func TestRoutingBareTextShowsTargetAndSource(t *testing.T) {
	routes := []service.RouteProjection{
		{TargetID: "global", Name: "classifier", SourcePath: "config.yaml", Effective: []string{"minime/qwen"}},
		{TargetID: "global", Name: "Researcher", SourcePath: "subagents/researcher.md", Desired: []string{"codex/gpt"}, Effective: []string{"codex/gpt"}},
	}
	var out bytes.Buffer
	writeRoutingText(&out, service.RoutingReport{Routes: routes}, styler{})
	text := out.String()
	if !strings.Contains(text, "target  source") || !strings.Contains(text, "global  config.yaml") || !strings.Contains(text, "global  subagents/researcher.md") {
		t.Fatalf("bare routing text does not expose target and source:\n%s", text)
	}
}

func TestRoutingExplainTextOmitsTargetAndSource(t *testing.T) {
	var out bytes.Buffer
	writeRoutingExplainText(&out, service.RoutingExplainReport{
		Ranks:  []service.ExplainRankProjection{{MappingID: "codex", Eligible: true, Status: "ready", Explanation: "peak"}},
		Routes: []service.ExplainRouteProjection{{Name: "full", Desired: "codex/gpt", Effective: "zai/glm"}},
	}, styler{})
	text := out.String()
	if strings.Contains(text, "target") || strings.Contains(text, "source") || strings.Contains(text, "rank") || strings.Contains(text, "off_peak") || strings.Contains(text, "eligible") {
		t.Fatalf("explain text contains internal columns:\n%s", text)
	}
	for _, want := range []string{"provider", "status", "reason", "route", "desired", "effective", "ready", "codex/gpt", "zai/glm"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain text missing %q:\n%s", want, text)
		}
	}
}

func TestRoutingJSONPreservesBareRouteContract(t *testing.T) {
	route := service.RouteProjection{TargetID: "global", Name: "classifier", SourcePath: "config.yaml", Desired: []string{"codex/gpt"}, Effective: []string{"minime/qwen"}}
	raw, err := json.Marshal(routingEnvelope(service.RoutingReport{Routes: []service.RouteProjection{route}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"target_id":"global"`) || !strings.Contains(text, `"source":"config.yaml"`) || !strings.Contains(text, `"desired":["codex/gpt"]`) {
		t.Fatalf("bare routing JSON contract changed: %s", text)
	}
}

func TestRoutingExplainJSONUsesScalarRouteContract(t *testing.T) {
	raw, err := json.Marshal(routingExplainEnvelope(service.RoutingExplainReport{
		Ranks:  []service.ExplainRankProjection{{MappingID: "codex", Rank: 0, Eligible: true, Status: "ready", Explanation: "peak"}},
		Routes: []service.ExplainRouteProjection{{Name: "full", Desired: "codex/gpt", Effective: "minime/qwen"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"desired":"codex/gpt"`, `"effective":"minime/qwen"`, `"status":"ready"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain JSON missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "target_id") || strings.Contains(text, "source") || strings.Contains(text, `"desired":[`) {
		t.Fatalf("explain JSON leaked bare route fields: %s", text)
	}
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

func TestRoutingExplainReadinessProjection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eligible bool
		want     string
	}{
		{name: "ready", eligible: true, want: "ready"},
		{name: "not ready", eligible: false, want: "not ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeRoutingExplainText(&out, service.RoutingExplainReport{
				Ranks: []service.ExplainRankProjection{{MappingID: "provider", Eligible: tc.eligible, Status: tc.want, Explanation: "reason"}},
			}, styler{})
			if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "reason") {
				t.Fatalf("output missing status/reason:\n%s", out.String())
			}
		})
	}
}

func TestRoutingExplainPendingTargetsAndWarning(t *testing.T) {
	r := service.RoutingExplainReport{PendingTargets: []string{"project-a", "global"}}
	var text bytes.Buffer
	writeRoutingExplainText(&text, r, styler{})
	if !strings.Contains(text.String(), "routing data may not be live") || !strings.Contains(text.String(), "project-a, global") || !strings.Contains(text.String(), "polytoken-quota doctor") {
		t.Fatalf("pending warning missing:\n%s", text.String())
	}
	encoded, err := json.Marshal(routingExplainEnvelope(r))
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(encoded)
	if !strings.Contains(jsonText, `"pending_targets":["project-a","global"]`) || !strings.Contains(jsonText, `"warning":"routing data may not be live`) {
		t.Fatalf("pending JSON warning missing: %s", jsonText)
	}
	encoded, err = json.Marshal(routingExplainEnvelope(service.RoutingExplainReport{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"pending_targets":[]`) || strings.Contains(string(encoded), `"warning"`) {
		t.Fatalf("empty pending JSON contract wrong: %s", encoded)
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
