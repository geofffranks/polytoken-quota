package validate

// These tests pin the diagnostic quality of persisted failure summaries: the
// tail of a failing command's output must survive the summary bound (doctor
// reports its actual error at the end), ANSI escapes must be stripped, and the
// remediation must point at a way to actually obtain the staged candidate.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// longDoctorOutput builds combined output shaped like a real staged doctor run:
// many INFO preamble lines and the actual error at the very end.
func longDoctorOutput() []byte {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("\x1b[2m2026-09-05T12:04:19Z\x1b[0m \x1b[32m INFO\x1b[0m polytoken live dynamic catalog fetched provider=neuralwatt\n")
	}
	b.WriteString("Error: startup validation failed: disabled default model referenced by subagent registry\n")
	return []byte(b.String())
}

// TestDoctorFailureSummaryKeepsTailOfOutput proves the persisted summary of a
// failed doctor stage keeps the error at the end of the output, strips ANSI
// escapes, and stays within the summary bound.
func TestDoctorFailureSummaryKeepsTailOfOutput(t *testing.T) {
	spy := newSpy()
	spy.FailAt = 1
	spy.Stderr = longDoctorOutput()
	r := newRunner(spy)

	got := r.Validate(context.Background(), candidate(), time.Second)
	if got.Error == nil || got.Error.Stage != Doctor {
		t.Fatalf("result=%+v", got)
	}
	s := got.Error.Summary
	if !strings.Contains(s, "disabled default model referenced") {
		t.Fatalf("summary lost the error tail:\n%q", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Fatalf("summary kept ANSI escapes:\n%q", s)
	}
	if !strings.Contains(s, "[truncated]") {
		t.Fatalf("summary lacks an elision marker:\n%q", s)
	}
	if len(s) > maxSummaryBytes {
		t.Fatalf("summary exceeds bound: %d bytes", len(s))
	}
}

// TestDefaultSanitizeStripsANSIEscapes proves the production redactor removes
// CSI color/emphasis sequences instead of persisting them.
func TestDefaultSanitizeStripsANSIEscapes(t *testing.T) {
	got := DefaultSanitize([]byte("\x1b[2m2026-09-05T12:04:19Z\x1b[0m \x1b[32m INFO\x1b[0m ready \x1b[3mprovider\x1b[0m=neuralwatt\n"))
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ANSI escapes survived sanitization: %q", got)
	}
	if !strings.Contains(got, "INFO") || !strings.Contains(got, "provider=neuralwatt") {
		t.Fatalf("sanitization destroyed non-escape content: %q", got)
	}
}

// TestBoundSummaryKeepsRuneBoundaries proves the head+tail bounding never cuts
// through a UTF-8 sequence and always keeps the tail.
func TestBoundSummaryKeepsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("→", 300) + "MIDDLE" + strings.Repeat("→", 800) + "END-MARKER"
	got := boundSummary(s)
	if len(got) > maxSummaryBytes {
		t.Fatalf("bounded summary exceeds bound: %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("bounded summary split a UTF-8 sequence: %q", got)
	}
	if !strings.HasSuffix(got, "END-MARKER") {
		t.Fatalf("bounded summary lost the tail: %q", got)
	}
	if !strings.HasPrefix(got, "→") {
		t.Fatalf("bounded summary lost the head: %q", got)
	}
}

// TestRemediationPointsAtRetainedStaging proves validation remediation directs
// the operator at `reconcile --dry-run --keep-staging`, which genuinely retains
// the staged candidate for inspection, and leaves timeout guidance untouched.
func TestRemediationPointsAtRetainedStaging(t *testing.T) {
	for _, stage := range []Stage{ConfigValidate, Doctor} {
		got := remediation(stage, false)
		if !strings.Contains(got, "--keep-staging") {
			t.Fatalf("stage %s remediation lacks keep-staging guidance: %q", stage, got)
		}
	}
	if got := remediation(Doctor, true); !strings.Contains(got, "timeout") {
		t.Fatalf("timeout remediation changed: %q", got)
	}
}
