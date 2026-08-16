package notice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestExecuteOnChangeDeliversArgvEnvStdin: the action receives literal argv,
// its configured env additions (plus only the minimal inherited set), and the
// notice document verbatim on stdin.
func TestExecuteOnChangeDeliversArgvEnvStdin(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "captured.txt")
	script := writeScript(t, dir, "capture.sh", `
echo "argv:$1:$2" >> "$OUT"
echo "env:${CONFIG_SCOPE}:" >> "$OUT"
if [ -n "$PQ_CANARY_LEAK" ]; then echo "env-leak" >> "$OUT"; fi
if [ -z "$PATH" ] || [ -z "$HOME" ]; then echo "missing-minimal" >> "$OUT"; fi
cat >> "$OUT"
`)
	notice := []byte(`{"schema":1,"revision":42}`)
	t.Setenv("PQ_CANARY_LEAK", "secret-value")

	res := ExecuteOnChange(context.Background(), []OnChangeSpec{{
		Run:  script,
		Args: []string{"--scope", "global"},
		Env:  map[string]string{"CONFIG_SCOPE": "staging", "OUT": out},
	}}, notice, 30*time.Second)

	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("result = %+v, want one success", res)
	}
	got := read(t, out)
	wantPrefix := "argv:--scope:global\nenv:staging:\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("capture = %q, want prefix %q", got, wantPrefix)
	}
	if strings.Contains(got, "env-leak") || strings.Contains(got, "missing-minimal") {
		t.Fatalf("environment isolation violated: %q", got)
	}
	if !strings.HasSuffix(got, string(notice)) {
		t.Fatalf("stdin payload = %q, want notice %q appended verbatim", got, notice)
	}
}

// TestExecuteOnChangeTimeoutKills: an action exceeding its per-action timeout
// is killed and reported as timed out without failing the batch.
func TestExecuteOnChangeTimeoutKills(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "slow.sh", "sleep 30\n")
	start := time.Now()
	res := ExecuteOnChange(context.Background(), []OnChangeSpec{
		{Run: script, Timeout: 150 * time.Millisecond},
	}, []byte("{}"), 30*time.Second)
	elapsed := time.Since(start)
	if len(res) != 1 || !res[0].TimedOut || res[0].Err == nil {
		t.Fatalf("result = %+v, want one timed-out failure", res)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced; took %s", elapsed)
	}
}

// TestExecuteOnChangeBudgetSkipsRemaining: once the aggregate budget is
// exhausted, unstarted actions are skipped and reported.
func TestExecuteOnChangeBudgetSkipsRemaining(t *testing.T) {
	dir := t.TempDir()
	slow := writeScript(t, dir, "slow.sh", "sleep 1\n")
	ran := filepath.Join(dir, "second-ran")
	second := writeScript(t, dir, "second.sh", "touch "+ran+"\n")

	res := ExecuteOnChange(context.Background(), []OnChangeSpec{
		{Run: slow, Timeout: 5 * time.Second},
		{Run: second, Timeout: 5 * time.Second},
	}, []byte("{}"), 300*time.Millisecond)
	if len(res) != 2 {
		t.Fatalf("results = %+v, want two", res)
	}
	if res[0].Err != nil || res[0].Skipped {
		t.Fatalf("first = %+v, want success", res[0])
	}
	if !res[1].Skipped {
		t.Fatalf("second = %+v, want budget skip", res[1])
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatalf("skipped action must not run")
	}
}

// TestExecuteOnChangeSpawnFailureReported: a missing executable is reported as
// a per-action failure, not a panic.
func TestExecuteOnChangeSpawnFailureReported(t *testing.T) {
	res := ExecuteOnChange(context.Background(), []OnChangeSpec{
		{Run: filepath.Join(t.TempDir(), "does-not-exist")},
	}, []byte("{}"), 5*time.Second)
	if len(res) != 1 || res[0].Err == nil || res[0].TimedOut || res[0].Skipped {
		t.Fatalf("result = %+v, want one spawn failure", res)
	}
}
