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
