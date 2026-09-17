package validate

// These tests pin the CommandDiagnostic carriage contract: a failed stage
// surfaces the FULL sanitized output (bounded only by the capture cap, never
// by the persisted-summary bound) alongside the honest truncation flag, while
// the persisted CommandError.Summary keeps its 1024-byte head+tail bound. The
// diagnostic is nil on success.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestValidateDiagnosticCarriesFullOutput proves the ephemeral diagnostic
// carries the complete sanitized output while the persisted summary stays
// bounded, and that success leaves the diagnostic nil.
func TestValidateDiagnosticCarriesFullOutput(t *testing.T) {
	long := strings.Repeat("error line\n", 400) // 4400 bytes, sanitizer-inert
	spy := &commandSpy{FailAt: 1, Stderr: []byte(long)}
	r := newRunner(spy)

	got := r.Validate(context.Background(), candidate(), time.Second)
	if got.Error == nil || got.Diagnostic == nil {
		t.Fatalf("expected error and diagnostic, got %+v", got)
	}
	if got.Diagnostic.Stage != Doctor {
		t.Fatalf("diagnostic stage=%q want doctor", got.Diagnostic.Stage)
	}
	if got.Diagnostic.ExitCode != 1 {
		t.Fatalf("diagnostic exit=%d want 1", got.Diagnostic.ExitCode)
	}
	if got.Diagnostic.Truncated {
		t.Fatalf("in-budget capture reported truncated=true")
	}
	// Full: the entire sanitized output survives, unbounded and marker-free.
	if got.Diagnostic.FullOutput != long {
		t.Fatalf("full output length=%d want %d (bounded or altered?)", len(got.Diagnostic.FullOutput), len(long))
	}
	if strings.Contains(got.Diagnostic.FullOutput, summaryElision) || strings.Contains(got.Diagnostic.FullOutput, truncationMarker()) {
		t.Fatalf("full output carries a summary-bound or truncation marker")
	}
	// Persisted: the summary keeps the 1024-byte head+tail bound.
	if len(got.Error.Summary) > maxSummaryBytes {
		t.Fatalf("summary length=%d exceeds bound %d", len(got.Error.Summary), maxSummaryBytes)
	}
	if !strings.Contains(got.Error.Summary, summaryElision) {
		t.Fatalf("over-long summary lost its head+tail elision marker: %q", got.Error.Summary)
	}

	// Success: no diagnostic.
	ok := newRunner(newSpy()).Validate(context.Background(), candidate(), time.Second)
	if ok.Error != nil || ok.Diagnostic != nil {
		t.Fatalf("success carried an error or diagnostic: %+v", ok)
	}
}

// TestValidateDiagnosticTruncationMarker proves the truncation marker is
// appended to the full output if and only if the capture budget reported
// dropped bytes.
func TestValidateDiagnosticTruncationMarker(t *testing.T) {
	spy := &commandSpy{FailAt: 0, Stderr: []byte("boom\n"), Truncated: true}
	got := newRunner(spy).Validate(context.Background(), candidate(), time.Second)
	if got.Diagnostic == nil {
		t.Fatal("expected a diagnostic")
	}
	if !got.Diagnostic.Truncated {
		t.Fatalf("diagnostic lost the runner's truncation flag: %+v", got.Diagnostic)
	}
	if !strings.HasSuffix(got.Diagnostic.FullOutput, truncationMarker()+"\n") {
		t.Fatalf("truncated capture did not append the terminal marker: %q", got.Diagnostic.FullOutput)
	}
	if !strings.Contains(got.Diagnostic.FullOutput, "boom") {
		t.Fatalf("truncated capture lost captured content: %q", got.Diagnostic.FullOutput)
	}
}

// TestInternalDiagnosticSanitizesUnbounded proves the exported constructor for
// quota-own failures applies the unbounded production sanitizer: secret-shaped
// content is redacted and no 1024-byte bound is applied.
func TestInternalDiagnosticSanitizesUnbounded(t *testing.T) {
	long := strings.Repeat("render failed at /home/alice/x/", 80) + " token=SECRET end"
	d := InternalDiagnostic("render", context.DeadlineExceeded)
	if d.Stage != "render" || d.ExitCode != 0 || d.Truncated {
		t.Fatalf("unexpected internal diagnostic fields: %+v", d)
	}
	// context.DeadlineExceeded text is sanitized-inert and short.
	if d.FullOutput == "" {
		t.Fatal("empty full output")
	}

	longDiag := InternalDiagnostic("stage", errString(long))
	if strings.Contains(longDiag.FullOutput, "SECRET") {
		t.Fatalf("internal diagnostic leaked a token: %q", longDiag.FullOutput)
	}
	if strings.Contains(longDiag.FullOutput, "/home/alice") {
		t.Fatalf("internal diagnostic leaked a home path: %q", longDiag.FullOutput)
	}
	if len(longDiag.FullOutput) <= maxSummaryBytes {
		t.Fatalf("internal diagnostic was 1024-bounded: %d bytes", len(longDiag.FullOutput))
	}
}

// errString is a stand-in error type carrying a canned message.
type errString string

func (e errString) Error() string { return string(e) }
