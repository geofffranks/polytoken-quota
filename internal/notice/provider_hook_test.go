package notice

import (
	"net/http"
	"strings"
	"testing"
)

func TestProviderOnlyNoticeDoesNotAddPromptClaims(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	h.setNotice(t, map[string]any{
		"schema": 1, "revision": 14, "provider_only": true,
		"providers": []any{map[string]any{"id": "stub", "enabled": false}},
	})
	out := h.runPrompt(t, "stub/model", "")
	if outcome, context := decodeDecision(t, out); outcome != "accept" || context != "" {
		t.Fatalf("provider-only notice prompt outcome=%q context=%q; want non-blocking accept without model claims", outcome, context)
	}
}

func TestProviderOnlyNoticeDefersConflictAndRetries(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	h.setNotice(t, map[string]any{"schema": 1, "revision": 14, "provider_only": true, "providers": []any{}})
	h.status = http.StatusConflict
	h.env["POLYTOKEN_HOOK_EVENT"] = "post_model_turn"
	if code := h.run(t); code != 0 || h.consumed(t) != 0 {
		t.Fatalf("409 hook code=%d consumed=%d; want deferred marker", code, h.consumed(t))
	}
	h.status = http.StatusOK
	if code := h.run(t); code != 0 || h.consumed(t) != 14 {
		t.Fatalf("retry hook code=%d consumed=%d; want consumed revision 14", code, h.consumed(t))
	}
	if strings.Contains(h.stdout.String(), "fallback") {
		t.Fatalf("hook emitted unsupported fallback claim: %q", h.stdout.String())
	}
}
