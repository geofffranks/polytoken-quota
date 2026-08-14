package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
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

func TestStatusTextANSIAlignment(t *testing.T) {
	report := service.StatusReport{
		AsOf: time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC),
		Providers: []service.ProviderStatus{
			{Provider: "minime", Quota: state.QuotaLow, Availability: state.Available, Mode: state.ModeReserve, Reason: "quota_low"},
			{Provider: "a-much-longer-provider", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal, Reason: "normal"},
		},
	}
	assertStyledLayoutMatchesPlain(t, func(out *bytes.Buffer, s styler) { writeStatusText(out, report, s) })
}

func TestStatusTextProviderColumnsArePadded(t *testing.T) {
	// Regression: styled cells (quota/availability/mode) lost their column
	// padding because their style closures discarded the padded input text,
	// producing "normalavailablenormalnormal" instead of aligned columns.
	report := service.StatusReport{
		Providers: []service.ProviderStatus{
			{Provider: "codex", Quota: state.QuotaNormal, Availability: state.Available, Mode: state.ModeNormal, Reason: ""},
			{Provider: "neuralwatt", Quota: state.QuotaLow, Availability: state.Available, Mode: state.ModeReserve, Reason: "quota_low"},
		},
	}
	wantRows := []string{
		"provider    quota   availability  mode     reason",
		"codex       normal  available     normal   normal",
		"neuralwatt  low     available     reserve  quota_low",
	}
	for _, enabled := range []bool{false, true} {
		var out bytes.Buffer
		writeStatusText(&out, report, styler{enabled: enabled})
		got := visibleText(out.String())
		for _, want := range wantRows {
			if !strings.Contains(got, want) {
				t.Fatalf("styled=%v: aligned row missing from output\n  want %q\n  got\n%s", enabled, want, got)
			}
		}
	}
}

func TestRoutingTextANSIAlignment(t *testing.T) {
	report := service.RoutingReport{RoutingEnabled: true, Routes: []service.RouteProjection{
		{TargetID: "global", SourcePath: "config.yaml", Name: "classifier", Effective: []string{"minime/qwen"}},
		{TargetID: "project-with-long-id", SourcePath: "subagents/researcher.md", Name: "Researcher", Effective: []string{"codex/gpt", "minime/qwen"}},
	}}
	assertStyledLayoutMatchesPlain(t, func(out *bytes.Buffer, s styler) { writeRoutingText(out, report, s) })
}

func TestRoutingExplainTextANSIAlignment(t *testing.T) {
	report := service.RoutingExplainReport{
		RoutingEnabled: true,
		Ranks: []service.ExplainRankProjection{
			{MappingID: "codex", Status: "ready", Eligible: true, Explanation: "healthy"},
			{MappingID: "a-much-longer-provider", Status: "ready", OffPeak: true, Eligible: true, Explanation: "off peak"},
		},
		Routes: []service.ExplainRouteProjection{
			{Name: "classifier", Desired: "minime/qwen", Effective: "minime/qwen"},
			{Name: "Researcher", Desired: "codex/gpt", Effective: "codex/gpt"},
		},
	}
	assertStyledLayoutMatchesPlain(t, func(out *bytes.Buffer, s styler) { writeRoutingExplainText(out, report, s) })
}
