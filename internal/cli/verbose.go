package cli

// Verbose reconcile trace rendering. Writes the decision data captured in
// TargetOutcome.Trace as human-readable text on stdout so the user can see
// exactly why each target's config did or did not change.

import (
	"fmt"
	"io"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// writeVerboseTrace renders the full per-target verbose reconcile report on
// stdout: per target the outcome, the full sanitized validation output (or the
// sanitized error chain of a polytoken-quota-own failure), remediation, any
// retained staging root, and — when traces were populated — the decision data
// (provider modes, routing ranking, chain survivors, edits). Transact-level
// failures that occur before any target exists render as a single error
// document. All data is sanitized at the source; only the capture cap bounds
// its length.
func writeVerboseTrace(w io.Writer, o service.Outcome) {
	if !o.Accepted {
		fmt.Fprintln(w, "=== reconcile ===")
		fmt.Fprintln(w, "outcome: not accepted")
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
			// diagnostic still shows its bounded sanitized one-liner.
			fmt.Fprintf(w, "  summary: %q\n", validate.DefaultSanitize([]byte(tgt.Pending.Summary)))
		}
		if tgt.Pending != nil && tgt.Pending.Remediation != "" {
			fmt.Fprintf(w, "remediation: %s\n", validate.DefaultSanitize([]byte(tgt.Pending.Remediation)))
		}
		if tgt.StagingRoot != "" {
			// Verbatim, not sanitizer-wrapped: the root is a tool-generated
			// path under the OS temp dir (the previous stderr line printed it
			// raw too), and the operator needs it to inspect the retained
			// candidate — the sanitizer's temp-path rule would erase it.
			fmt.Fprintf(w, "retained staging root: %s\n", tgt.StagingRoot)
		}
		if tgt.Trace != nil {
			writeProviderModes(w, tgt.Trace)
			writeRanking(w, tgt.Trace)
			writeChains(w, tgt.Trace)
			writeEdits(w, tgt.Trace)
		}
	}
	writeVerboseError(w, o.Error)
}

// writeVerboseDiagnostic renders one target's ephemeral full diagnostic.
// External validation output (config_validate/doctor stages) is labeled as
// validation output; any other stage is a polytoken-quota-own failure and is
// labeled as an error. The truncation marker, when present, is the diagnostic's
// terminal line and renders with the rest.
func writeVerboseDiagnostic(w io.Writer, d *validate.CommandDiagnostic) {
	if d == nil || d.FullOutput == "" {
		return
	}
	switch d.Stage {
	case validate.ConfigValidate, validate.Doctor:
		fmt.Fprintf(w, "validation output (%s, sanitized):\n", d.Stage)
	default:
		fmt.Fprintf(w, "error (%s, sanitized):\n", d.Stage)
	}
	for _, line := range strings.Split(strings.TrimSuffix(d.FullOutput, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

// writeVerboseError renders the outcome-level error — a transact-level failure
// such as policy load, target resolution, or state save — with the unbounded
// sanitizer, so verbose output is not squeezed into the persisted-summary
// bound.
func writeVerboseError(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(w, "error (sanitized):")
	fmt.Fprintf(w, "    %s\n", validate.InternalDiagnostic("reconcile", err).FullOutput)
}

func writeProviderModes(w io.Writer, tr *service.ReconcileTrace) {
	if len(tr.ProviderModes) == 0 {
		return
	}
	fmt.Fprintln(w, "  provider modes:")
	for _, pm := range tr.ProviderModes {
		fmt.Fprintf(w, "    %s: %s (%s)\n",
			validate.DefaultSanitize([]byte(pm.MappingID)),
			pm.Mode, pm.Reason)
	}
}

func writeRanking(w io.Writer, tr *service.ReconcileTrace) {
	if len(tr.Ranking) == 0 {
		return
	}
	fmt.Fprintln(w, "  routing ranking:")
	for _, e := range tr.Ranking {
		elig := "eligible"
		if !e.Eligible {
			elig = "ineligible"
		}
		fmt.Fprintf(w, "    %s: rank=%d %s — %s\n",
			validate.DefaultSanitize([]byte(e.MappingID)),
			e.Rank, elig, e.Explanation)
	}
}

func writeChains(w io.Writer, tr *service.ReconcileTrace) {
	if len(tr.Chains) == 0 {
		return
	}
	fmt.Fprintln(w, "  chains:")
	for _, ch := range tr.Chains {
		fmt.Fprintf(w, "    %s:\n", validate.DefaultSanitize([]byte(ch.Name)))
		fmt.Fprintf(w, "      desired:  %s\n", strings.Join(ch.Desired, " → "))
		fmt.Fprintf(w, "      survived: %s\n", strings.Join(ch.Survived, " → "))
		if len(ch.Dropped) > 0 {
			fmt.Fprintf(w, "      dropped:  %s\n", strings.Join(ch.Dropped, ", "))
		}
	}
}

func writeEdits(w io.Writer, tr *service.ReconcileTrace) {
	if len(tr.Edits) == 0 {
		fmt.Fprintln(w, "  edits: (none)")
		return
	}
	fmt.Fprintln(w, "  edits:")
	for _, ed := range tr.Edits {
		fmt.Fprintf(w, "    %s %s: %s = %s\n",
			validate.DefaultSanitize([]byte(ed.File)),
			ed.Action,
			strings.Join(ed.Path, "."),
			validate.DefaultSanitize([]byte(ed.Detail)))
	}
}
