package cli

// The reconcile output contract: without --verbose reconcile is fully silent
// (exit code only, stdout AND stderr) across the whole outcome matrix, with
// the documented exception of pre-outcome usage/flag errors; --verbose renders
// one coherent per-target document on stdout. check's output is pinned by a
// golden test and its --json projection gains no diagnostic fields.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// pendingTarget builds a pending global target outcome with a full diagnostic.
func pendingTarget(stage, full string, truncated bool) service.TargetOutcome {
	return service.TargetOutcome{
		TargetID: "global",
		Pending: &state.ApplyFailure{
			Stage:       stage,
			Summary:     "doctor: bounded one-liner",
			Remediation: "re-run with `reconcile --dry-run --keep-staging`",
		},
		Diagnostic: &validate.CommandDiagnostic{
			Stage:      validate.Stage(stage),
			FullOutput: full,
			Truncated:  truncated,
			ExitCode:   1,
		},
	}
}

// TestReconcileQuietIsSilent is the AC3 matrix: without --verbose, reconcile
// emits zero bytes on stdout and stderr across the outcome matrix, and the
// exit codes are unchanged.
func TestReconcileQuietIsSilent(t *testing.T) {
	long := strings.Repeat("doctor detail line\n", 300)
	cases := []struct {
		name string
		args []string
		out  service.Outcome
		code int
	}{
		{
			name: "accepted applied",
			args: []string{"reconcile"},
			out:  service.Outcome{Accepted: true},
			code: ExitOK,
		},
		{
			name: "dry-run accepted applied",
			args: []string{"reconcile", "--dry-run"},
			out:  service.Outcome{Accepted: true},
			code: ExitOK,
		},
		{
			name: "dry-run pending",
			args: []string{"reconcile", "--dry-run"},
			out: service.Outcome{
				Accepted: true,
				Targets:  []service.TargetOutcome{pendingTarget("doctor", long, false)},
			},
			code: ExitOK,
		},
		{
			name: "dry-run pending with retained staging",
			args: []string{"reconcile", "--dry-run", "--keep-staging"},
			out: service.Outcome{
				Accepted: true,
				Targets: []service.TargetOutcome{func() service.TargetOutcome {
					tgt := pendingTarget("doctor", long, false)
					tgt.StagingRoot = "/tmp/quota-stage-global"
					return tgt
				}()},
			},
			code: ExitOK,
		},
		{
			name: "non-dry-run pending exits 2",
			args: []string{"reconcile"},
			out: service.Outcome{
				Accepted: true,
				Targets:  []service.TargetOutcome{pendingTarget("doctor", long, false)},
			},
			code: ExitPending,
		},
		{
			name: "hard error",
			args: []string{"reconcile"},
			out:  service.Outcome{Error: errors.New("mutation failed badly")},
			code: ExitRejected,
		},
		{
			name: "transact failure in dry-run",
			args: []string{"reconcile", "--dry-run"},
			out:  service.Outcome{Error: errors.New("policy load failed")},
			code: ExitRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &strings.Builder{}
			stderr := &strings.Builder{}
			spy := &outcomeSpy{outcome: tc.out}
			code := Run(context.Background(), tc.args, strings.NewReader(""), stdout, stderr, spy.Dependencies())
			if code != tc.code {
				t.Fatalf("exit=%d want %d", code, tc.code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout not empty: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr not empty: %q", stderr.String())
			}
		})
	}
}

// TestReconcileUsageErrorsStillPrint is AC4: the documented exception —
// pre-outcome usage/flag errors stay on stderr even without --verbose.
func TestReconcileUsageErrorsStillPrint(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"invalid arguments", []string{"reconcile", "--bogus"}, "invalid arguments"},
		{"keep-staging without dry-run", []string{"reconcile", "--keep-staging"}, "requires --dry-run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &strings.Builder{}
			stderr := &strings.Builder{}
			code := Run(context.Background(), tc.args, strings.NewReader(""), stdout, stderr, newDepsSpy().Dependencies())
			if code != ExitRejected {
				t.Fatalf("exit=%d want %d", code, ExitRejected)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr=%q missing %q", stderr.String(), tc.want)
			}
		})
	}

	t.Run("mutator unavailable", func(t *testing.T) {
		stderr := &strings.Builder{}
		code := Run(context.Background(), []string{"reconcile"}, strings.NewReader(""), io.Discard, stderr, Dependencies{
			Environment: func() map[string]string { return nil },
		})
		if code != ExitRejected || !strings.Contains(stderr.String(), "mutator unavailable") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
}

// TestReconcileVerboseDoctorFailurePrintsFullOutput is AC1: a doctor-stage
// pending under --dry-run --verbose prints the full sanitized external doctor
// output on stdout — no 1024-byte head+tail elision.
func TestReconcileVerboseDoctorFailurePrintsFullOutput(t *testing.T) {
	long := strings.Repeat("doctor detail line\n", 300) // 3800 bytes > 1024
	spy := &outcomeSpy{outcome: service.Outcome{
		Accepted: true,
		Targets:  []service.TargetOutcome{pendingTarget("doctor", long, false)},
	}}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code := Run(context.Background(), []string{"reconcile", "--dry-run", "--verbose"}, strings.NewReader(""), stdout, stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "=== target global ===") || !strings.Contains(out, "outcome: pending (stage=doctor)") {
		t.Fatalf("missing target document header:\n%s", out)
	}
	if !strings.Contains(out, "validation output (doctor, sanitized):") {
		t.Fatalf("missing validation output section:\n%s", out)
	}
	if got := strings.Count(out, "doctor detail line"); got != 300 {
		t.Fatalf("full doctor output not rendered in full: %d of 300 detail lines in %d bytes of stdout:\n%s", got, len(out), out)
	}
	if strings.Count(out, "validation output (doctor, sanitized):") != 1 {
		t.Fatalf("validation output section rendered more than once:\n%s", out)
	}
	if strings.Contains(out, "…[truncated]…") {
		t.Fatalf("verbose document must not carry the persisted-summary elision marker:\n%s", out)
	}
}

// TestReconcileVerboseQuotaOwnError is AC2: quota-own failures (render/stage/
// publish) render their full sanitized error chain in the per-target document.
func TestReconcileVerboseQuotaOwnError(t *testing.T) {
	for _, stage := range []string{"render", "stage", "publish"} {
		t.Run(stage, func(t *testing.T) {
			chain := strings.Repeat("error: candidate write failed at step ", 60) + stage
			spy := &outcomeSpy{outcome: service.Outcome{
				Accepted: true,
				Targets: []service.TargetOutcome{{
					TargetID: "global",
					Pending: &state.ApplyFailure{
						Stage:       stage,
						Summary:     "bounded summary",
						Remediation: "resolve the pending error and re-run reconcile",
					},
					Diagnostic: &validate.CommandDiagnostic{
						Stage:      validate.Stage(stage),
						FullOutput: chain,
					},
				}},
			}}
			stdout := &strings.Builder{}
			code := Run(context.Background(), []string{"reconcile", "--verbose"}, strings.NewReader(""), stdout, io.Discard, spy.Dependencies())
			if code != ExitPending {
				t.Fatalf("exit=%d want %d", code, ExitPending)
			}
			out := stdout.String()
			if !strings.Contains(out, "error ("+stage+", sanitized):") {
				t.Fatalf("missing quota-own error section:\n%s", out)
			}
			if !strings.Contains(out, chain) {
				t.Fatalf("full error chain not rendered:\n%s", out)
			}
		})
	}
}

// TestReconcileVerboseDryRunTransactError is AC2's transact-level half: a
// pre-target failure (policy load / target resolution) under --dry-run
// --verbose renders the sanitized error on stdout and exits 1.
func TestReconcileVerboseDryRunTransactError(t *testing.T) {
	spy := &outcomeSpy{outcome: service.Outcome{Error: errors.New("resolve targets: root missing")}}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code := Run(context.Background(), []string{"reconcile", "--dry-run", "--verbose"}, strings.NewReader(""), stdout, stderr, spy.Dependencies())
	if code != ExitRejected {
		t.Fatalf("exit=%d want %d", code, ExitRejected)
	}
	if stderr.Len() != 0 {
		t.Fatalf("verbose document must stay on stdout, got stderr=%q", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"=== reconcile ===", "outcome: not accepted", "error (sanitized):", "resolve targets: root missing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("transact error document missing %q:\n%s", want, out)
		}
	}
}

// TestCheckOutputUnchangedGolden is AC5: pins check's non-JSON output —
// including the shared writePendingTargets one-liners — so the reconcile
// silencing cannot drift it.
func TestCheckOutputUnchangedGolden(t *testing.T) {
	spy := &outcomeSpy{outcome: service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{
			{TargetID: "global"},
			{TargetID: "proj", Pending: &state.ApplyFailure{
				TargetID: "proj", Stage: "doctor",
				Summary:     `doctor: model check failed`,
				Remediation: "re-run with `reconcile --dry-run --keep-staging` and run `polytoken doctor` against the retained staged config and working dirs",
			}},
		},
	}}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	code := Run(context.Background(), []string{"check"}, strings.NewReader(""), stdout, stderr, spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want %d (stderr=%q)", code, ExitPending, stderr.String())
	}
	want := "target proj pending: stage=doctor summary=\"doctor: model check failed\" " +
		"remediation=re-run with `reconcile --dry-run --keep-staging` and run `polytoken doctor` against the retained staged config and working dirs\n"
	if stderr.String() != want {
		t.Fatalf("check pending line drifted:\ngot=%q\nwant=%q", stderr.String(), want)
	}
}

// TestCheckJSONProjectionHasNoDiagnosticFields extends AC6 to the check --json
// projection: the hand-built envelope gains no diagnostic keys.
func TestCheckJSONProjectionHasNoDiagnosticFields(t *testing.T) {
	spy := &outcomeSpy{outcome: service.Outcome{
		Accepted: true,
		Targets: []service.TargetOutcome{{
			TargetID:   "proj",
			Pending:    &state.ApplyFailure{TargetID: "proj", Stage: "doctor", Summary: "bounded"},
			Diagnostic: &validate.CommandDiagnostic{Stage: validate.Doctor, FullOutput: "huge output"},
		}},
	}}
	stdout := &strings.Builder{}
	code := Run(context.Background(), []string{"check", "--json"}, strings.NewReader(""), stdout, io.Discard, spy.Dependencies())
	if code != ExitPending {
		t.Fatalf("exit=%d want %d", code, ExitPending)
	}
	raw := stdout.String()
	for _, forbidden := range []string{"FullOutput", "Diagnostic", "Truncated"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("check --json carries diagnostic field %q: %s", forbidden, raw)
		}
	}
}
