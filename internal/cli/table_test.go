package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleText(text string) string {
	return ansiPattern.ReplaceAllString(text, "")
}

func assertStyledLayoutMatchesPlain(t *testing.T, render func(*bytes.Buffer, styler)) {
	t.Helper()
	var plain, styled bytes.Buffer
	render(&plain, styler{})
	render(&styled, styler{enabled: true})
	if got, want := visibleText(styled.String()), plain.String(); got != want {
		t.Fatalf("visible styled layout differs from plain layout:\n--- styled visible ---\n%s\n--- plain ---\n%s", got, want)
	}
}

func TestTableWriterUsesTerminalDisplayWidth(t *testing.T) {
	rows := [][]tableCell{
		{{text: "name"}, {text: "value"}},
		{{text: "café"}, {text: "accent"}},
		{{text: "e\u0301"}, {text: "combining"}},
		{{text: "模型"}, {text: "cjk"}},
		{{text: "가나"}, {text: "hangul"}},
		{{text: "ＡＢ"}, {text: "fullwidth"}},
		{{text: "👍🏽"}, {text: "modifier"}},
		{{text: "👩‍💻"}, {text: "zwj"}},
	}
	var out bytes.Buffer
	writeTable(&out, rows)
	want := "name  value\n" +
		"café  accent\n" +
		"é     combining\n" +
		"模型  cjk\n" +
		"가나  hangul\n" +
		"ＡＢ  fullwidth\n" +
		"👍🏽    modifier\n" +
		"👩‍💻    zwj\n"
	if got := out.String(); got != want {
		t.Fatalf("terminal-width layout mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestTableWriterEscapesTerminalActiveControls(t *testing.T) {
	rows := [][]tableCell{
		{{text: "source"}, {text: "route"}},
		{{text: "agents/evil\nrow\tcol\x1b[31mred\x1b]0;title\a\u202Espoof\u200B.md"}, {text: "classifier"}},
	}
	original := rows[1][0].text
	var out bytes.Buffer
	writeTable(&out, rows)
	if rows[1][0].text != original {
		t.Fatalf("writeTable mutated caller cell: got %q want %q", rows[1][0].text, original)
	}
	got := out.String()
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("control characters injected rows: %q", got)
	}
	for _, forbidden := range []string{"\t", "\x1b", "\a", "\u202E", "\u200B"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("terminal-active control %q survived: %q", forbidden, got)
		}
	}
	for _, escaped := range []string{`\n`, `\t`, `\x1b`, `\x07`, `\u202e`, `\u200b`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("missing visible escape %q: %q", escaped, got)
		}
	}
}

func TestMergedStatusTextLayout(t *testing.T) {
	used, limit := 41.0, 80.0
	reset := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	report := service.MergedStatusReport{
		RoutingEnabled: true,
		LastChecked:    time.Date(2026, 8, 14, 9, 12, 0, 0, time.UTC),
		Providers: []service.MergedStatusProvider{
			{Provider: "zai", Status: "available", Rank: 1, OffPeak: true, Eligible: true, Reason: "off-peak, pace 109%", Windows: []service.QuotaWindowReport{
				{Name: "5h", Used: &used, Limit: &limit},
				{Name: "weekly", Used: &used, Limit: &limit},
			}, NextResetAt: &reset},
			{Provider: "minime", Status: "enabled", Rank: 3, Reason: "not configured"},
		},
		Routes: []service.MergedStatusRoute{
			{Name: "global", TargetID: "global", SourcePath: "config.yaml", Desired: []string{"glm-4.6", "gpt-5.2", "sonnet"},
				Effective: []string{"glm-4.6"},
				Skipped:   []service.SkippedModel{{Model: "gpt-5.2", Reason: "quota exhausted"}, {Model: "sonnet", Reason: "manual disable"}}},
			{Name: "work-api", TargetID: "work", SourcePath: "subagents/work-api.md", Desired: []string{"glm-4.6"}, Effective: []string{"glm-4.6"}},
		},
		PendingTargets: []string{"work"},
	}
	var out bytes.Buffer
	writeMergedStatusText(&out, report, styler{})
	got := collapseSpaces(out.String())

	for _, want := range []string{
		"routing: enabled",
		"last checked: 2026-08-14 09:12 UTC",
		"PROVIDER STATUS REASON QUOTA NEXT RESET",
		"zai available off-peak, pace 109% 5h 41/80, weekly 41/80 2026-08-15 00:00 UTC",
		"minime enabled not configured no data —",
		"TARGET SOURCE ROUTE DESIRED EFFECTIVE",
		"global config.yaml global glm-4.6 glm-4.6",
		"work subagents/work-api.md work-api glm-4.6 glm-4.6",
		"warning: 1 target(s) pending — shown values may not be live; run polytoken-quota doctor",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized output missing %q\noutput:\n%s", want, got)
		}
	}
}

func TestMergedStatusTextHeaderVariants(t *testing.T) {
	var out bytes.Buffer
	writeMergedStatusText(&out, service.MergedStatusReport{RoutingEnabled: false}, styler{})
	got := collapseSpaces(out.String())
	for _, want := range []string{"routing: disabled", "last checked: never"} {
		if !strings.Contains(got, want) {
			t.Fatalf("header missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "warning:") {
		t.Fatalf("pending warning shown without pending targets: %q", got)
	}
}

func TestMergedStatusTextStatusColors(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   string
	}{
		{"available", "\x1b[32m"},
		{"disabled", "\x1b[31m"},
		{"unavailable", "\x1b[31m"},
		{"enabled", "\x1b[33m"},
	} {
		report := service.MergedStatusReport{Providers: []service.MergedStatusProvider{{Provider: "p", Status: tc.status}}}
		var out bytes.Buffer
		writeMergedStatusText(&out, report, styler{enabled: true})
		// Styles wrap the padded cell text, so match the color prefix + status word.
		if !strings.Contains(out.String(), tc.code+tc.status) {
			t.Fatalf("status %s not colored %q in output:\n%s", tc.status, tc.code, out.String())
		}
	}
}

func TestMergedStatusTextProjectionErrorAndWindowFormats(t *testing.T) {
	pct := 55.5
	reset := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	report := service.MergedStatusReport{
		RoutingEnabled: true,
		Providers: []service.MergedStatusProvider{
			{Provider: "codex", Status: "available", Rank: 0, Eligible: true, Reason: "peak", Windows: []service.QuotaWindowReport{
				{Name: "5h", UsagePercent: &pct},
			}, NextResetAt: &reset},
		},
		Routes: []service.MergedStatusRoute{
			{Name: "broken", TargetID: "global", SourcePath: "config.yaml", Desired: []string{"a/full"}, ProjectionError: true},
		},
		Errors: []service.DiagnosticError{{Scope: service.ErrorScopeRoute, TargetID: "global", Summary: "route projection unavailable"}},
	}
	var out bytes.Buffer
	writeMergedStatusText(&out, report, styler{})
	got := collapseSpaces(out.String())
	for _, want := range []string{
		"codex available peak 5h 55.5% 2026-08-20 00:00 UTC",
		"global config.yaml broken a/full none",
		"error: route projection unavailable",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized output missing %q\noutput:\n%s", want, got)
		}
	}
}

func TestMergedStatusTextANSIAlignment(t *testing.T) {
	used, limit := 41.0, 80.0
	reset := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	report := service.MergedStatusReport{
		RoutingEnabled: true,
		LastChecked:    time.Date(2026, 8, 14, 9, 12, 0, 0, time.UTC),
		Providers: []service.MergedStatusProvider{
			{Provider: "zai", Status: "available", Windows: []service.QuotaWindowReport{{Name: "5h", Used: &used, Limit: &limit}}, NextResetAt: &reset},
			{Provider: "a-much-longer-provider", Status: "disabled", Windows: []service.QuotaWindowReport{{Name: "5h", Used: &used, Limit: &limit}}, NextResetAt: &reset},
		},
		Routes: []service.MergedStatusRoute{
			{Name: "global", Desired: []string{"glm-4.6", "gpt-5.2"}, Effective: []string{"glm-4.6"},
				Skipped: []service.SkippedModel{{Model: "gpt-5.2", Reason: "quota exhausted"}}},
		},
		PendingTargets: []string{"work"},
	}
	assertStyledLayoutMatchesPlain(t, func(out *bytes.Buffer, s styler) { writeMergedStatusText(out, report, s) })
}

func TestMergedStatusTextUsesHiddenRankForProviderOrder(t *testing.T) {
	report := service.MergedStatusReport{
		Providers: []service.MergedStatusProvider{
			{Provider: "zai", Status: "available", Rank: 1, OffPeak: true, Eligible: true, Reason: "off-peak, pace 109%"},
			{Provider: "codex", Status: "available", Rank: 0, Eligible: true, Reason: "peak, pace 50%"},
		},
	}
	var out bytes.Buffer
	writeMergedStatusText(&out, report, styler{})
	got := collapseSpaces(out.String())
	if !strings.Contains(got, "PROVIDER STATUS REASON QUOTA NEXT RESET") {
		t.Fatalf("compact provider header missing: %q", got)
	}
	for _, hidden := range []string{"RANK", "OFF_PEAK", "ELIGIBLE"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("provider table exposed hidden field %q: %q", hidden, got)
		}
	}
	if strings.Index(got, "codex available") > strings.Index(got, "zai available") {
		t.Fatalf("providers were not sorted by hidden rank: %q", got)
	}
}

func TestMergedStatusTextShowsFirstModelOnlyInRouteChains(t *testing.T) {
	report := service.MergedStatusReport{Routes: []service.MergedStatusRoute{{
		Name:      "full",
		Desired:   []string{"codex/first", "neuralwatt/second", "zai/third"},
		Effective: []string{"codex/first", "neuralwatt/second", "zai/third"},
	}}}
	var out bytes.Buffer
	writeMergedStatusText(&out, report, styler{})
	got := collapseSpaces(out.String())
	if !strings.Contains(got, "full codex/first codex/first") {
		t.Fatalf("route row did not show first desired/effective models: %q", got)
	}
	for _, model := range []string{"neuralwatt/second", "zai/third"} {
		if strings.Contains(got, model) {
			t.Fatalf("route row rendered non-first model %q: %q", model, got)
		}
	}
}

func collapseSpaces(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
