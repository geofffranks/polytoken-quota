package notice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptHarness reuses the fake-daemon harness with a richer notice document
// and prompt-event environment.
func (h *hookHarness) runPrompt(t *testing.T, model, facet string) string {
	t.Helper()
	h.env["POLYTOKEN_HOOK_EVENT"] = "pre_user_prompt"
	h.env["POLYTOKEN_MODEL_NAME"] = model
	if facet == "" {
		delete(h.env, "POLYTOKEN_FACET_NAME")
	} else {
		h.env["POLYTOKEN_FACET_NAME"] = facet
	}
	h.stdout.Reset()
	if code := h.run(t); code != 0 {
		t.Fatalf("exit = %d, want 0 (fail-open)", code)
	}
	return h.stdout.String()
}

func (h *hookHarness) setNotice(t *testing.T, doc map[string]any) {
	t.Helper()
	writeJSON(t, h.noticePath, doc)
}

func (h *hookHarness) setState(t *testing.T, st map[string]any) {
	t.Helper()
	writeJSON(t, h.statePath(), st)
}

func driftNotice() map[string]any {
	return map[string]any{
		"schema":          1,
		"revision":        9,
		"routing_enabled": true,
		"targets": []any{
			map[string]any{
				"id":   "global",
				"kind": "global",
				"chains": []any{
					map[string]any{"name": "full", "models": []any{"codex/head-model", "zai/in-chain"}},
				},
			},
			map[string]any{
				"id":    "work-api",
				"kind":  "definition",
				"file":  "subagents/work-api.md",
				"facet": "work-api",
				"chain": []any{"minime/facet-model"},
			},
		},
		"disabled_models": []any{"zai/dead-model"},
	}
}

func decodeDecision(t *testing.T, out string) (string, string) {
	t.Helper()
	var d struct {
		Outcome          string `json:"outcome"`
		AdditionalContext string `json:"additional_context"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &d); err != nil {
		t.Fatalf("decision parse %q: %v", out, err)
	}
	return d.Outcome, d.AdditionalContext
}

// TestHookDriftTiers: in-chain sessions stay silent; disabled models get the
// actionable warning; enabled off-chain models get the informational wording;
// both fire at most once per consumed revision.
func TestHookDriftTiers(t *testing.T) {
	newH := func(t *testing.T) *hookHarness {
		h := newHookHarness(t)
		t.Cleanup(h.server.Close)
		h.setNotice(t, driftNotice())
		h.setState(t, map[string]any{"consumed_revision": 9})
		return h
	}

	t.Run("in-chain silent", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "zai/in-chain", ""))
		if ctxText != "" {
			t.Fatalf("in-chain model must not warn: %q", ctxText)
		}
	})
	t.Run("head in-chain silent", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "codex/head-model", ""))
		if ctxText != "" {
			t.Fatalf("chain head must not warn: %q", ctxText)
		}
	})
	t.Run("disabled actionable once per revision", func(t *testing.T) {
		h := newH(t)
		outcome, ctxText := decodeDecision(t, h.runPrompt(t, "zai/dead-model", ""))
		if outcome != "accept" {
			t.Fatalf("outcome = %q, want accept (non-blocking)", outcome)
		}
		if !strings.Contains(ctxText, "[polytoken-quota]") || !strings.Contains(ctxText, "disabled") || !strings.Contains(ctxText, "codex/head-model") {
			t.Fatalf("actionable warning = %q", ctxText)
		}
		if !strings.Contains(ctxText, "uncached") {
			t.Fatalf("warning must carry the uncached-context caveat: %q", ctxText)
		}
		_, again := decodeDecision(t, h.runPrompt(t, "zai/dead-model", ""))
		if again != "" {
			t.Fatalf("warning repeated within one revision: %q", again)
		}
	})
	t.Run("off-chain informational", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "anthropic/manual-pick", ""))
		if !strings.Contains(ctxText, "outside the configured chain") || !strings.Contains(ctxText, "optional") {
			t.Fatalf("informational warning = %q", ctxText)
		}
	})
	t.Run("facet match uses definition chain", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "minime/facet-model", "work-api"))
		if ctxText != "" {
			t.Fatalf("in-facet-chain model must not warn: %q", ctxText)
		}
	})
	t.Run("absent facet falls back to global full chain", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "minime/facet-model", ""))
		if !strings.Contains(ctxText, "outside the configured chain") {
			t.Fatalf("facet model off global chain should warn informationally: %q", ctxText)
		}
	})
	t.Run("no-match facet falls back to global full chain", func(t *testing.T) {
		h := newH(t)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "zai/in-chain", "unknown-facet"))
		if ctxText != "" {
			t.Fatalf("global in-chain model must not warn: %q", ctxText)
		}
	})
	t.Run("null chain entries skipped", func(t *testing.T) {
		h := newH(t)
		notice := driftNotice()
		notice["targets"] = []any{
			map[string]any{
				"id": "global", "kind": "global",
				"chains": []any{map[string]any{"name": "full", "models": []any{nil, "zai/in-chain"}}},
			},
		}
		h.setNotice(t, notice)
		_, ctxText := decodeDecision(t, h.runPrompt(t, "zai/in-chain", ""))
		if ctxText != "" {
			t.Fatalf("null entries must not break chain membership: %q", ctxText)
		}
	})
}

// TestHookForcedSwitchNote: a model change since the previous prompt is
// attributed to a reconciliation only when a newer revision was consumed in
// between; the note fires once per occurrence.
func TestHookForcedSwitchNote(t *testing.T) {
	t.Run("forced switch after consumed revision", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		h.setNotice(t, driftNotice())
		h.setState(t, map[string]any{"consumed_revision": 8, "last_seen_model": "zai/dead-model", "last_prompt_consumed": 8})

		// Turn ends: reload converges the session to revision 9.
		h.env["POLYTOKEN_HOOK_EVENT"] = "post_model_turn"
		delete(h.env, "POLYTOKEN_MODEL_NAME")
		if code := h.run(t); code != 0 || h.reloadCalls != 1 {
			t.Fatalf("reload code=%d calls=%d", code, h.reloadCalls)
		}

		// Daemon fell back; next prompt sees a different model + newer consumed.
		_, ctxText := decodeDecision(t, h.runPrompt(t, "codex/head-model", ""))
		if !strings.Contains(ctxText, "zai/dead-model") || !strings.Contains(ctxText, "uncached") {
			t.Fatalf("forced-switch note = %q", ctxText)
		}
		// Once per occurrence: the next prompt with the same model is silent
		// on this note (and in-chain, so fully silent).
		_, again := decodeDecision(t, h.runPrompt(t, "codex/head-model", ""))
		if again != "" {
			t.Fatalf("forced note repeated: %q", again)
		}
	})
	t.Run("voluntary switch is not forced", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		h.setNotice(t, driftNotice())
		h.setState(t, map[string]any{"consumed_revision": 9, "last_seen_model": "zai/in-chain", "last_prompt_consumed": 9})

		_, ctxText := decodeDecision(t, h.runPrompt(t, "codex/head-model", ""))
		if strings.Contains(ctxText, "forced") || strings.Contains(ctxText, "changed from") {
			t.Fatalf("voluntary switch must not produce a forced note: %q", ctxText)
		}
	})
}

// TestHookPromptFailOpen: missing model env and corrupt notices fail open with
// a plain accept and never warn.
func TestHookPromptFailOpen(t *testing.T) {
	t.Run("missing model env", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		h.setNotice(t, driftNotice())
		out := h.runPrompt(t, "", "")
		outcome, ctxText := decodeDecision(t, out)
		if outcome != "accept" || ctxText != "" {
			t.Fatalf("decision = %q ctx = %q, want bare accept", outcome, ctxText)
		}
	})
	t.Run("corrupt notice", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		if err := os.WriteFile(h.noticePath, []byte("{bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := h.runPrompt(t, "zai/dead-model", "")
		outcome, ctxText := decodeDecision(t, out)
		if outcome != "accept" || ctxText != "" {
			t.Fatalf("decision = %q ctx = %q, want bare accept", outcome, ctxText)
		}
	})
	t.Run("missing state file still warns and creates state", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		h.setNotice(t, driftNotice())
		if err := os.Remove(filepath.Join(h.sessionsDir, h.sessionID, "polytoken-quota", "state.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		_, ctxText := decodeDecision(t, h.runPrompt(t, "zai/dead-model", ""))
		if ctxText == "" {
			t.Fatalf("missing state must still warn")
		}
		if h.consumed(t) != 0 {
			t.Fatalf("prompt path must not advance consumed revision")
		}
	})
}
