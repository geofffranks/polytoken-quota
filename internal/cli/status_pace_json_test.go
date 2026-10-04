package cli

// Synthetic JSON projection tests for the per-provider pace signal,
// explicit freshness, and as_of in the status --json envelope: a real zero
// signal stays present, a non-computable one is omitted, freshness is always
// one of fresh/stale/missing, as_of identifies the evaluation instant (and is
// omitted only when no evaluation happened), and the new fields leave every
// pre-existing row field — including state, rank, and gate attribution —
// untouched. All inputs are synthetic reports; no config or live providers.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// runStatusJSON renders one synthetic merged status report through the real
// status --json path and returns the raw envelope bytes. exitCode is the
// expected process exit (0 for a healthy report, 1 for a fatal error).
func runStatusJSON(t *testing.T, exitCode int, report service.MergedStatusReport) []byte {
	t.Helper()
	spy := newDepsSpy()
	spy.StatusReportValue = report
	var out bytes.Buffer
	code := Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &out, io.Discard, spy.Dependencies())
	if code != exitCode {
		t.Fatalf("exit=%d want %d\n%s", code, exitCode, out.String())
	}
	return out.Bytes()
}

// TestStatusJSONSignalZeroPresentAbsentOmitted proves the optional pace signal
// is structured, not stringly: a real computed zero is present as 0, and a
// non-computable one is omitted from the row — while freshness stays explicit
// as one of fresh/stale/missing.
func TestStatusJSONSignalZeroPresentAbsentOmitted(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	raw := runStatusJSON(t, 0, service.MergedStatusReport{
		RoutingEnabled: true,
		AsOf:           asOf,
		Providers: []service.MergedStatusProvider{
			{Provider: "onpace", Status: service.StatusAvailable, Freshness: "fresh", Signal: fptr(0)},
			{Provider: "nodata", Status: service.StatusEnabled, Freshness: "missing"},
			{Provider: "aged", Status: service.StatusAvailable, Freshness: "stale", Signal: fptr(-1.5)},
		},
	})
	out := string(raw)
	if strings.Count(out, `"signal":`) != 2 {
		t.Fatalf("signal key count = %d, want exactly the two computable ones:\n%s", strings.Count(out, `"signal":`), out)
	}
	if !strings.Contains(out, `"signal":0`) {
		t.Fatalf("envelope missing the real zero signal:\n%s", out)
	}

	var parsed struct {
		Providers []struct {
			Provider  string   `json:"provider"`
			Freshness string   `json:"freshness"`
			Signal    *float64 `json:"signal"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(parsed.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(parsed.Providers))
	}
	zero, absent, stale := parsed.Providers[0], parsed.Providers[1], parsed.Providers[2]
	if zero.Signal == nil || *zero.Signal != 0 || zero.Freshness != "fresh" {
		t.Fatalf("onpace row = %+v, want present zero signal and fresh", zero)
	}
	if absent.Signal != nil || absent.Freshness != "missing" {
		t.Fatalf("nodata row = %+v, want omitted signal and missing", absent)
	}
	if stale.Signal == nil || *stale.Signal != -1.5 || stale.Freshness != "stale" {
		t.Fatalf("aged row = %+v, want signal -1.5 and stale", stale)
	}
}

// TestStatusJSONAsOfIdentifiesEvaluationInstant proves the top-level as_of is
// the envelope's evaluation instant in RFC3339 UTC, and that envelopes without
// an evaluation instant (e.g. a fatal error) keep the previous shape with no
// as_of key at all.
func TestStatusJSONAsOfIdentifiesEvaluationInstant(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 30, 0, 0, time.FixedZone("shifted", 2*3600))
	raw := runStatusJSON(t, 0, service.MergedStatusReport{AsOf: asOf})
	if !strings.Contains(string(raw), `"as_of":"2026-10-02T10:30:00Z"`) {
		t.Fatalf("as_of missing or not RFC3339 UTC:\n%s", raw)
	}
	var parsed struct {
		AsOf string `json:"as_of"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, string(raw))
	}
	if parsed.AsOf != "2026-10-02T10:30:00Z" {
		t.Fatalf("as_of = %q, want the shifted-zone instant rendered in UTC", parsed.AsOf)
	}

	fatal := runStatusJSON(t, 1, service.MergedStatusReport{Error: "state unreadable"})
	if strings.Contains(string(fatal), `"as_of"`) {
		t.Fatalf("fatal envelope must keep the previous shape without as_of:\n%s", fatal)
	}
}

// TestStatusJSONSignalLeavesRowShapeUnchanged proves adding freshness and
// signal to a fully-attributed row changes nothing else: the exact key set of
// the provider row is the previous shape plus freshness and signal, and the
// status/rank/gate values render exactly as before.
func TestStatusJSONSignalLeavesRowShapeUnchanged(t *testing.T) {
	asOf := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	checked := asOf.Add(-time.Minute)
	raw := runStatusJSON(t, 0, service.MergedStatusReport{
		RoutingEnabled: true,
		AsOf:           asOf,
		LastChecked:    checked,
		Providers: []service.MergedStatusProvider{{
			Provider: "gp", Status: service.StatusGated, Rank: 2, OffPeak: true, Eligible: false,
			Reason:    "signal-gated (-0.90 <= +0.00); peak, signal -0.90",
			Freshness: "fresh", Signal: fptr(-0.9), CheckedAt: checked, Availability: "unavailable",
			Gate: &service.GateReport{Axis: state.OwnershipAxisSignal, Signal: fptr(-0.9), Threshold: fptr(0), EngagedRevision: 7},
		}},
		Routes:         []service.MergedStatusRoute{},
		PendingTargets: []string{},
	})
	out := string(raw)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	topKeys := make([]string, 0, len(top))
	for k := range top {
		topKeys = append(topKeys, k)
	}
	sortStrings(topKeys)
	wantTop := []string{
		"as_of", "errors", "last_checked", "pending_targets", "problem",
		"provider_only", "providers", "routes", "routing_enabled",
	}
	if strings.Join(topKeys, ",") != strings.Join(wantTop, ",") {
		t.Fatalf("top-level keys = %v, want %v", topKeys, wantTop)
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(top["providers"], &rows); err != nil {
		t.Fatalf("invalid providers: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("providers = %d, want 1", len(rows))
	}
	rowKeys := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		rowKeys = append(rowKeys, k)
	}
	sortStrings(rowKeys)
	wantRow := []string{
		"availability", "checked_at", "eligible", "freshness", "off_peak",
		"provider", "rank", "reason", "signal", "status", "windows",
	}
	if strings.Join(rowKeys, ",") != strings.Join(wantRow, ",") {
		t.Fatalf("provider row keys = %v, want the previous shape plus freshness and signal: %v", rowKeys, wantRow)
	}

	var parsed []struct {
		Provider  string   `json:"provider"`
		Status    string   `json:"status"`
		Rank      int      `json:"rank"`
		OffPeak   bool     `json:"off_peak"`
		Eligible  bool     `json:"eligible"`
		Reason    string   `json:"reason"`
		Freshness string   `json:"freshness"`
		Signal    *float64 `json:"signal"`
	}
	if err := json.Unmarshal(top["providers"], &parsed); err != nil {
		t.Fatalf("invalid providers: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("providers = %d, want 1", len(parsed))
	}
	p := parsed[0]
	if p.Provider != "gp" || p.Status != "gated" || p.Rank != 2 || !p.OffPeak || p.Eligible {
		t.Fatalf("identity/state changed: %+v", p)
	}
	// The gate keeps rendering through the reason text exactly as before; the
	// envelope deliberately carries no gate object.
	if p.Reason != "signal-gated (-0.90 <= +0.00); peak, signal -0.90" {
		t.Fatalf("reason changed: %q", p.Reason)
	}
	if p.Signal == nil || *p.Signal != -0.9 || p.Freshness != "fresh" {
		t.Fatalf("signal/freshness wrong: %+v", p)
	}
}

// sortStrings sorts a string slice in place (local helper so this file stays
// self-contained).
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// fptr is a local float pointer helper for this file.
func fptr(v float64) *float64 { return &v }
