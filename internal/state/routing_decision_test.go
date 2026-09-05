package state

// RoutingDecision explanations are re-sanitized immediately before every save
// so a stale or hand-edited state file cannot carry control characters or
// oversized free text into the next process lifetime. This mirrors the store's
// never-trust-repersisted-text posture for snapshot errors and diagnostics.

import (
	"strings"
	"testing"
)

func TestSanitizeSnapshotsSanitizesRoutingDecisionExplanation(t *testing.T) {
	oversized := strings.Repeat("x", HistoryFreeTextBytes+50)
	s := State{Providers: map[string]ProviderState{
		"codex": {Routing: ProviderRouting{Decision: &RoutingDecision{
			Rank:        1,
			Eligible:    true,
			Explanation: "peak, pace 5%\x01leak" + oversized,
		}}},
	}}

	out := sanitizeSnapshots(s)
	d := out.Providers["codex"].Routing.Decision
	if d == nil {
		t.Fatal("routing decision dropped by sanitizer")
	}
	if strings.ContainsRune(d.Explanation, '\x01') {
		t.Fatalf("control character survived sanitization: %q", d.Explanation)
	}
	if len(d.Explanation) > HistoryFreeTextBytes {
		t.Fatalf("explanation not truncated: %d bytes", len(d.Explanation))
	}
	if !strings.HasPrefix(d.Explanation, "peak, pace 5%") {
		t.Fatalf("clean explanation content changed: %q", d.Explanation)
	}
}
