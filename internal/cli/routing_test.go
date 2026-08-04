package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/service"
)

func TestFormatTimeZeroIsEmpty(t *testing.T) {
	if got := formatTime(time.Time{}); got != "" {
		t.Fatalf("formatTime(zero)=%q, want empty", got)
	}
}

func ioDiscard() io.Writer { return io.Discard }

// serviceQuotaStatusReport aliases the service report type for brevity in test
// doubles.
type serviceQuotaStatusReport = service.QuotaStatusReport

// routing quota CLI tests: dispatch, output rendering, exit codes, JSON
// validity, no-mutation-on-error, and the byte-preserving enable/disable round
// trip against a real desired.yaml via the production *Coordinator.

// TestRoutingExplainDispatch verifies routing explain parses, prints the ranking,
// and exits 0; --json emits valid JSON.
func TestRoutingExplainDispatch(t *testing.T) {
	t.Run("text exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "explain"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !strings.Contains(out.String(), "codex") {
			t.Fatalf("output missing provider: %q", out.String())
		}
		if !strings.Contains(out.String(), "rank=0") {
			t.Fatalf("output missing rank: %q", out.String())
		}
	})
	t.Run("json valid", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "explain", "--json"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		var r map[string]any
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
		if r["enabled"] != true {
			t.Fatalf("want enabled=true; got %v", r["enabled"])
		}
	})
	t.Run("invalid args exit 1 no mutation", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "explain", "--bogus"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 0 {
			t.Fatalf("invalid args mutated %d times", spy.Mutations)
		}
	})
}

// TestRoutingEnableDisable verifies the toggle dispatches with the right intent
// and rejects bad args without mutation.
func TestRoutingEnableDisable(t *testing.T) {
	t.Run("enable sets true", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"routing", "enable"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.RoutingEnabledSet || !spy.RoutingEnabledValue {
			t.Fatalf("enable not invoked with true; set=%v value=%v", spy.RoutingEnabledSet, spy.RoutingEnabledValue)
		}
		if !strings.Contains(out.String(), "routing enabled") {
			t.Fatalf("output: %q", out.String())
		}
	})
	t.Run("disable sets false", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "disable"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !spy.RoutingEnabledSet || spy.RoutingEnabledValue {
			t.Fatalf("disable not invoked with false")
		}
	})
	t.Run("error exits 1", func(t *testing.T) {
		spy := newDepsSpy()
		spy.RoutingToggleErr = errors.New("boom")
		code := Run(context.Background(), []string{"routing", "enable"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
	})
	t.Run("bad args exit 1 no mutation", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"routing", "enable", "--x"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 0 {
			t.Fatalf("mutated %d", spy.Mutations)
		}
	})
}

// TestQuotaStatusDispatch verifies quota status prints sanitized snapshots and
// emits valid JSON; exit 0 when healthy.
func TestQuotaStatusDispatch(t *testing.T) {
	t.Run("text exit 0", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"quota", "status"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !strings.Contains(out.String(), "codex") {
			t.Fatalf("output missing provider: %q", out.String())
		}
		// Sanitized: no secret-bearing words.
		for _, bad := range []string{"bearer", "api_key", "password", "token="} {
			if strings.Contains(strings.ToLower(out.String()), bad) {
				t.Fatalf("output contains %q: %q", bad, out.String())
			}
		}
	})
	t.Run("json valid", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"quota", "status", "--json"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		var r map[string]any
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out.String())
		}
	})
	t.Run("bad args exit 1 no mutation", func(t *testing.T) {
		spy := newDepsSpy()
		code := Run(context.Background(), []string{"quota", "status", "--nope"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d", code)
		}
		if spy.Mutations != 0 {
			t.Fatalf("mutated %d", spy.Mutations)
		}
	})
}

// TestQuotaStatusProblemExit2 verifies the CLI maps a problem flag to exit 2.
func TestQuotaStatusProblemExit2(t *testing.T) {
	deps := Dependencies{
		QuotaStater: problemStater{},
		Environment: func() map[string]string { return map[string]string{} },
	}
	var out bytes.Buffer
	code := Run(context.Background(), []string{"quota", "status"}, strings.NewReader(""), &out, ioDiscard(), deps)
	if code != ExitPending {
		t.Fatalf("exit=%d want %d", code, ExitPending)
	}
	if !strings.Contains(out.String(), "pending problem") {
		t.Fatalf("output should mention problem: %q", out.String())
	}
}

// problemStater returns a quota status report with Problem=true.
type problemStater struct{}

func (problemStater) QuotaStatus(context.Context) serviceQuotaStatusReport {
	return serviceQuotaStatusReport{Problem: true}
}

func TestRoutingTogglePostRenameWarningIsAccepted(t *testing.T) {
	spy := newDepsSpy()
	spy.RoutingToggleErr = &service.RoutingWriteError{Err: errors.New("sync dir failed"), Mutated: true}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"routing", "enable"}, strings.NewReader(""), &out, &stderr, spy.Dependencies())
	if code != ExitOK || !strings.Contains(stderr.String(), "durability warning") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

// TestRoutingEnableDisableBytePreserving verifies the production
// SetRoutingEnabled edits ONLY routing.enabled in a real desired.yaml: comments,
// key order, and all other content are preserved.
func TestRoutingEnableDisableBytePreserving(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.yaml")
	original := strings.Join([]string{
		"# my desired policy",
		"version: 1",
		"providers:",
		"  codex:",
		"    codexbar_providers: [codex]",
		"    polytoken_providers: [codex]",
		"    models:",
		"      - codex/gpt",
		"routing:",
		"  enabled: false",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	coord := prodCoordForToggle(path)

	// Disable is a no-op (already false); still exit 0.
	if err := coord.SetRoutingEnabled(context.Background(), false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("idempotent disable changed bytes:\n%s", string(got))
	}

	// Enable: only routing.enabled flips to true.
	if err := coord.SetRoutingEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(original, "enabled: false", "enabled: true", 1)
	if string(got) != want {
		t.Fatalf("enable changed more than routing.enabled:\nwant:\n%s\ngot:\n%s", want, string(got))
	}

	// Re-disable restores the original exactly.
	if err := coord.SetRoutingEnabled(context.Background(), false); err != nil {
		t.Fatalf("re-disable: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("re-disable did not restore original:\n%s", string(got))
	}
}

// TestRoutingEnableNoFile errors when desired.yaml is absent.
func TestRoutingEnableNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.yaml") // not created
	coord := prodCoordForToggle(path)
	err := coord.SetRoutingEnabled(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error; got %v", err)
	}
}

// prodCoordForToggle builds a minimal production *Coordinator wired only for the
// routing enable/disable path (a real FilePolicyLoader pointing at path). All
// other dependencies are nil; SetRoutingEnabled uses only Policy + the document
// editor.
func prodCoordForToggle(path string) *service.Coordinator {
	return &service.Coordinator{
		Policy: service.FilePolicyLoader{Path: path},
	}
}

// --- quota check CLI tests (Task 8b) ---

// TestQuotaCheckDispatch verifies quota check parses flags, calls the
// coordinator, prints sanitized output, and returns the correct exit code.
func TestQuotaCheckDispatch(t *testing.T) {
	t.Run("clean exit 0 text", func(t *testing.T) {
		spy := newDepsSpy()
		var out bytes.Buffer
		code := Run(context.Background(), []string{"quota", "check"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitOK {
			t.Fatalf("exit=%d want 0", code)
		}
		if !strings.Contains(out.String(), "quota check: accepted=true") {
			t.Fatalf("output missing accepted line: %q", out.String())
		}
		if !strings.Contains(out.String(), "restarted or reloaded by the user") {
			t.Fatalf("output missing advisory: %q", out.String())
		}
		if spy.Mutations != 1 {
			t.Fatalf("mutations=%d want 1", spy.Mutations)
		}
	})

	t.Run("problem exit 2", func(t *testing.T) {
		spy := newDepsSpy()
		spy.QuotaCheckSet = true
		spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 3, Problem: true}
		var out bytes.Buffer
		code := Run(context.Background(), []string{"quota", "check"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
		if code != ExitPending {
			t.Fatalf("exit=%d want 2", code)
		}
		if !strings.Contains(out.String(), "pending problem") {
			t.Fatalf("output missing problem: %q", out.String())
		}
	})

	t.Run("rejected exit 1", func(t *testing.T) {
		spy := newDepsSpy()
		spy.QuotaCheckSet = true
		spy.QuotaCheckOutcome = service.Outcome{Accepted: false, Error: errors.New("service: acquire lock: busy")}
		var stderr bytes.Buffer
		code := Run(context.Background(), []string{"quota", "check"}, strings.NewReader(""), ioDiscard(), &stderr, spy.Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want 1", code)
		}
		if !strings.Contains(stderr.String(), "acquire lock") {
			t.Fatalf("stderr missing error: %q", stderr.String())
		}
	})
}

// TestQuotaCheckJSON verifies --json produces valid JSON with the expected fields.
func TestQuotaCheckJSON(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 5, Problem: true}
	var out bytes.Buffer
	code := Run(context.Background(), []string{"quota", "check", "--json"}, strings.NewReader(""), &out, ioDiscard(), spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want 2", code)
	}
	var parsed struct {
		Accepted bool   `json:"accepted"`
		Revision uint64 `json:"revision"`
		Problem  bool   `json:"problem"`
		Advisory string `json:"advisory"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if !parsed.Accepted || parsed.Revision != 5 || !parsed.Problem {
		t.Fatalf("unexpected JSON: %+v", parsed)
	}
	if parsed.Advisory == "" {
		t.Fatal("advisory missing")
	}
}

func TestQuotaCheckJSONRejectedPathsEmitEnvelope(t *testing.T) {
	t.Run("invalid args", func(t *testing.T) {
		var out bytes.Buffer
		code := Run(context.Background(), []string{"quota", "check", "--json", "--bogus"}, strings.NewReader(""), &out, ioDiscard(), newDepsSpy().Dependencies())
		if code != ExitRejected {
			t.Fatalf("exit=%d want rejected", code)
		}
		var got quotaCheckJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("invalid envelope: %v (%q)", err, out.String())
		}
		if got.Accepted || got.Error == "" || got.Advisory == "" {
			t.Fatalf("envelope=%+v", got)
		}
	})
	t.Run("missing mutator", func(t *testing.T) {
		var out bytes.Buffer
		deps := Dependencies{}
		code := Run(context.Background(), []string{"quota", "check", "--json"}, strings.NewReader(""), &out, ioDiscard(), deps)
		if code != ExitRejected {
			t.Fatalf("exit=%d want rejected", code)
		}
		var got quotaCheckJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("invalid envelope: %v (%q)", err, out.String())
		}
		if got.Accepted || got.Error == "" || got.Advisory == "" {
			t.Fatalf("envelope=%+v", got)
		}
	})
}

// TestQuotaCheckProviderFlag verifies --provider is forwarded to the coordinator.
func TestQuotaCheckProviderFlag(t *testing.T) {
	spy := newDepsSpy()
	Run(context.Background(), []string{"quota", "check", "--provider", "zai"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
	if spy.QuotaCheckProvider != "zai" {
		t.Fatalf("provider=%q want zai", spy.QuotaCheckProvider)
	}
}

func TestQuotaCheckProviderRejectsMissingOrFlagLikeValue(t *testing.T) {
	for _, args := range [][]string{
		{"quota", "check", "--provider", "--json"},
		{"quota", "check", "--provider", "--reconcile"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, spy.Dependencies())
			if code != ExitRejected || spy.Mutations != 0 {
				t.Fatalf("code=%d mutations=%d, want rejected without mutation", code, spy.Mutations)
			}
			if args[len(args)-1] == "--json" {
				lines := strings.Split(strings.TrimSpace(out.String()), "\n")
				if len(lines) != 1 {
					t.Fatalf("output lines=%d, want exactly one envelope: %q", len(lines), out.String())
				}
				var got quotaCheckJSON
				if err := json.Unmarshal([]byte(lines[0]), &got); err != nil || got.Accepted || got.Error != "quota check: invalid arguments" || got.Advisory == "" {
					t.Fatalf("envelope=%+v err=%v", got, err)
				}
			} else if !strings.Contains(stderr.String(), "quota check: invalid arguments") {
				t.Fatalf("stderr=%q, want invalid-arguments diagnostic", stderr.String())
			}
		})
	}
}

// TestQuotaCheckReconcileFlag verifies --reconcile is forwarded as true.
func TestQuotaCheckReconcileFlag(t *testing.T) {
	spy := newDepsSpy()
	Run(context.Background(), []string{"quota", "check", "--reconcile"}, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
	if !spy.QuotaCheckReconcile {
		t.Fatal("reconcile=false want true")
	}
}

// TestQuotaCheckInvalidArgs verifies invalid arguments exit 1 without mutation.
func TestQuotaCheckInvalidArgs(t *testing.T) {
	cases := [][]string{
		{"quota", "check", "--bogus"},
		{"quota", "check", "--provider"},
		{"quota", "check", "positional"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			spy := newDepsSpy()
			code := Run(context.Background(), args, strings.NewReader(""), ioDiscard(), ioDiscard(), spy.Dependencies())
			if code != ExitRejected {
				t.Fatalf("args=%v exit=%d want 1", args, code)
			}
			if spy.Mutations != 0 {
				t.Fatalf("invalid args mutated: %d", spy.Mutations)
			}
		})
	}
}

// TestQuotaCheckNoSecrets verifies no credentials appear in output (defense in
// depth — the spy returns clean output, this guards the rendering path).
func TestQuotaCheckNoSecrets(t *testing.T) {
	spy := newDepsSpy()
	spy.QuotaCheckSet = true
	spy.QuotaCheckOutcome = service.Outcome{Accepted: true, Revision: 1, Error: errors.New("bounded http: api_key=sk-live-1234567890wxyz")}
	var out, stderr bytes.Buffer
	Run(context.Background(), []string{"quota", "check"}, strings.NewReader(""), &out, &stderr, spy.Dependencies())
	combined := out.String() + stderr.String()
	for _, secret := range []string{"sk-live-1234567890wxyz", "Bearer abc123", "password=hunter2"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret %q leaked in output: %s", secret, combined)
		}
	}
}
