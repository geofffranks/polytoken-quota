package validate

// These tests pin the honest-truncation contract: the capture budget reports
// whether any byte was dropped or clipped, the flag is exact (a run that fills
// the cap precisely is NOT truncated), and the terminal truncation marker
// appears in composed full output if and only if capture dropped bytes.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestCaptureBudgetTruncatedFlag covers the drop/clip detection and the
// composed full-output marker: exact-cap fill must not claim truncation,
// while a clipped write and a fully dropped write both must.
func TestCaptureBudgetTruncatedFlag(t *testing.T) {
	t.Run("exact_cap_fill_is_not_truncated", func(t *testing.T) {
		budget := &captureBudget{remaining: 64}
		w := &boundedWriter{budget: budget}
		if n, err := w.Write(bytes.Repeat([]byte("x"), 64)); n != 64 || err != nil {
			t.Fatalf("write: n=%d err=%v", n, err)
		}
		if budget.isTruncated() {
			t.Fatalf("exact-cap fill reported truncated=true")
		}
		if got := fullOutput(defaultMaxOutput, "out", budget.isTruncated()); strings.Contains(got, truncationMarker(defaultMaxOutput)) {
			t.Fatalf("exact-cap fill composed a truncation marker: %q", got)
		}
	})

	t.Run("clipped_write_is_truncated", func(t *testing.T) {
		budget := &captureBudget{remaining: 64}
		w := &boundedWriter{budget: budget}
		if n, err := w.Write(bytes.Repeat([]byte("x"), 100)); n != 100 || err != nil {
			t.Fatalf("write must report the full length: n=%d err=%v", n, err)
		}
		if len(w.bytes()) != 64 {
			t.Fatalf("clipped capture kept %d bytes, want 64", len(w.bytes()))
		}
		if !budget.isTruncated() {
			t.Fatalf("clipped write reported truncated=false")
		}
		got := fullOutput(defaultMaxOutput, "out", budget.isTruncated())
		if !strings.HasSuffix(got, truncationMarker(defaultMaxOutput)+"\n") {
			t.Fatalf("clipped output missing terminal truncation marker: %q", got)
		}
	})

	t.Run("dropped_write_is_truncated", func(t *testing.T) {
		budget := &captureBudget{remaining: 8}
		w := &boundedWriter{budget: budget}
		if _, err := w.Write(bytes.Repeat([]byte("x"), 8)); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if n, err := w.Write([]byte("overflow")); n != 8 || err != nil {
			t.Fatalf("dropped write must report the full length: n=%d err=%v", n, err)
		}
		if !budget.isTruncated() {
			t.Fatalf("dropped write reported truncated=false")
		}
	})

	t.Run("empty_write_at_exhausted_budget_is_not_truncated", func(t *testing.T) {
		budget := &captureBudget{remaining: 0}
		w := &boundedWriter{budget: budget}
		if n, err := w.Write(nil); n != 0 || err != nil {
			t.Fatalf("empty write: n=%d err=%v", n, err)
		}
		if budget.isTruncated() {
			t.Fatalf("empty write reported truncated=true")
		}
	})

	t.Run("marker_states_the_applied_capture_cap", func(t *testing.T) {
		if want := "…[output truncated at 262144 bytes]…"; truncationMarker(defaultMaxOutput) != want {
			t.Fatalf("truncationMarker(defaultMaxOutput)=%q want %q", truncationMarker(defaultMaxOutput), want)
		}
		// The marker names the cap that bound the run, not a fixed constant.
		small := truncationMarker(64)
		if !strings.Contains(small, "64") || !strings.HasPrefix(small, "…[output truncated at ") {
			t.Fatalf("truncationMarker(%d)=%q does not state its cap", 64, small)
		}
	})

	t.Run("full_output_without_truncation_is_unchanged", func(t *testing.T) {
		if got := fullOutput(128, "plain output", false); got != "plain output" {
			t.Fatalf("fullOutput=%q want unchanged input", got)
		}
	})
}

// TestExecRunnerTruncatedFlag exercises the production runner's honest
// truncation report: output exactly filling the cap is not truncated; output
// past the cap is.
func TestExecRunnerTruncatedFlag(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("/bin/echo unavailable on this platform")
	}
	r := ExecRunner{}

	t.Run("exact_cap_fill", func(t *testing.T) {
		// /bin/echo writes the argument plus one newline: 15+1 = 16 bytes.
		_, _, _, truncated, err := r.Run(context.Background(), "/bin/echo", []string{strings.Repeat("x", 15)}, 16, nil)
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		if truncated {
			t.Fatalf("exact-cap fill reported truncated=true")
		}
	})

	t.Run("beyond_cap", func(t *testing.T) {
		stdout, _, _, truncated, err := r.Run(context.Background(), "/bin/echo", []string{strings.Repeat("x", 500)}, 16, nil)
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		if !truncated {
			t.Fatalf("clipped output reported truncated=false")
		}
		if int64(len(stdout)) != 16 {
			t.Fatalf("captured %d bytes, want the 16-byte cap", len(stdout))
		}
	})
}
