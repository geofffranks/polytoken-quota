package cli

// Verbose reconcile rendering. Under --verbose each target reports only its
// outcome; a pending target additionally shows the full sanitized failing
// surface, labeled by who failed, so the actual error is never buried under
// decision detail. All data is sanitized at the source; only the capture cap
// bounds its length.

import (
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// writeVerboseTrace renders the per-target verbose reconcile report on stdout.
// Applied targets print only their outcome. Pending targets print their
// outcome plus the full sanitized failure — `polytoken doctor failed:` for
// external doctor output, `polytoken-quota validation failed:` for external
// config-validate and quota-own failures — and any retained staging root.
// Transact-level failures that occur before any target exists render as a
// single failure document.
func writeVerboseTrace(w io.Writer, o service.Outcome) {
	if !o.Accepted {
		fmt.Fprintln(w, "=== reconcile ===")
		writeVerboseError(w, o.Error)
		return
	}
	for _, tgt := range o.Targets {
		fmt.Fprintf(w, "=== target %s ===\n", validate.DefaultSanitize([]byte(tgt.TargetID)))
		if tgt.Pending != nil {
			fmt.Fprintf(w, "outcome: pending (stage=%s)\n", validate.DefaultSanitize([]byte(tgt.Pending.Stage)))
		} else {
			fmt.Fprintln(w, "outcome: applied")
		}
		// Provider-only gate summaries report what the pass did (or
		// deliberately did not do) per provider: pace verdicts, pool skips,
		// and axis attribution. Legacy targets carry none, so their output is
		// unchanged.
		for _, g := range tgt.ProviderGates {
			line := "gate: " + validate.DefaultSanitize([]byte(g.Provider)) + " " + validate.DefaultSanitize([]byte(g.Action))
			if g.Axis != "" {
				line += " axis=" + validate.DefaultSanitize([]byte(g.Axis))
			}
			if g.Pace != nil {
				line += fmt.Sprintf(" pace=%d%%", routing.PacePercent(*g.Pace))
			}
			if g.Threshold != nil {
				line += fmt.Sprintf(" threshold=%d%%", routing.PacePercent(*g.Threshold))
			}
			if g.Detail != "" {
				line += " (" + validate.DefaultSanitize([]byte(g.Detail)) + ")"
			}
			fmt.Fprintln(w, line)
		}
		// Skipped tier-default diagnostics are deliberate, operator-actionable
		// information: they explain why the legacy tier selections were left
		// unwritten (the composed config uses modelgroups). They render for
		// applied and pending targets alike.
		for _, sk := range tgt.Skipped {
			fmt.Fprintf(w, "skipped: %s (%s)\n",
				validate.DefaultSanitize([]byte(sk.Field)),
				validate.DefaultSanitize([]byte(sk.Reason)))
		}
		writeVerboseDiagnostic(w, tgt.Diagnostic)
		if tgt.Pending != nil && (tgt.Diagnostic == nil || tgt.Diagnostic.FullOutput == "") {
			// Defensive fallback mirroring summarize: a pending without a full
			// diagnostic still shows its bounded sanitized one-liner as real
			// text (newlines intact), never `%q`-escaped.
			fmt.Fprintf(w, "  reason: %s\n", validate.DefaultSanitize([]byte(tgt.Pending.Summary)))
		}
		if tgt.StagingRoot != "" {
			// Verbatim, not sanitizer-wrapped: the root is a tool-generated
			// path under the OS temp dir, and the operator needs it to inspect
			// the retained candidate — the sanitizer's temp-path rule would
			// erase it.
			fmt.Fprintf(w, "retained staging root: %s\n", tgt.StagingRoot)
		}
	}
	writeVerboseError(w, o.Error)
}

// writeVerboseDiagnostic renders one target's ephemeral full diagnostic with
// real newlines, labeled by who failed: external `polytoken doctor` output, or
// every other reconciliation problem (external `config validate` and
// polytoken-quota's own render/stage/publish errors).
func writeVerboseDiagnostic(w io.Writer, d *validate.CommandDiagnostic) {
	if d == nil || d.FullOutput == "" {
		return
	}
	switch d.Stage {
	case validate.Doctor:
		fmt.Fprintln(w, "polytoken doctor failed:")
	default:
		fmt.Fprintln(w, "polytoken-quota validation failed:")
	}
	for _, line := range strings.Split(strings.TrimRight(d.FullOutput, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

// writeVerboseError renders the outcome-level error — a transact-level failure
// such as policy load, target resolution, or state save — as a reconciliation
// problem outside `polytoken doctor`, with the unbounded sanitizer and real
// newlines.
func writeVerboseError(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(w, "polytoken-quota validation failed:")
	for _, line := range strings.Split(strings.TrimRight(validate.InternalDiagnostic("reconcile", err).FullOutput, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
