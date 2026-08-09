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

// writeVerboseTrace renders the per-target decision trace for an Outcome. It
// prints provider modes, routing ranking, chain survivor analysis, and produced
// edits. All data is already sanitized at the source.
func writeVerboseTrace(w io.Writer, o service.Outcome) {
	if !o.Accepted {
		fmt.Fprintln(w, "verbose: operation not accepted; no trace")
		return
	}
	for _, tgt := range o.Targets {
		fmt.Fprintf(w, "=== target %s ===\n", validate.DefaultSanitize([]byte(tgt.TargetID)))
		if tgt.Trace == nil {
			fmt.Fprintln(w, "  (no trace data)")
			continue
		}
		writeProviderModes(w, tgt.Trace)
		writeRanking(w, tgt.Trace)
		writeChains(w, tgt.Trace)
		writeEdits(w, tgt.Trace)
	}
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
