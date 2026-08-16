package notice

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookHarness builds a fake session directory with startup/credential
// artifacts and a configurable reload endpoint, plus the environment the
// daemon would provide to a hook handler.
type hookHarness struct {
	sessionsDir string
	sessionID   string
	noticePath  string
	server      *httptest.Server
	reloadCalls int
	status      int
	stdout      *strings.Builder
	env         map[string]string
}

func newHookHarness(t *testing.T) *hookHarness {
	h := &hookHarness{
		sessionID: "sess-1",
		status:    http.StatusOK,
		stdout:    &strings.Builder{},
		env:       map[string]string{"POLYTOKEN_HOOK_EVENT": "post_model_turn", "POLYTOKEN_SESSION_ID": "sess-1"},
	}
	dir := t.TempDir()
	h.sessionsDir = filepath.Join(dir, "sessions")
	sessDir := filepath.Join(h.sessionsDir, h.sessionID)
	if err := os.MkdirAll(filepath.Join(sessDir, "polytoken-quota"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.reloadCalls++
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(`{"facets_changed":true}`))
	}))
	writeJSON(t, filepath.Join(sessDir, "startup.json"), map[string]any{"session_id": h.sessionID, "pid": 5, "port": portOf(t, h.server.URL)})
	writeJSON(t, filepath.Join(sessDir, "credential.json"), map[string]any{"version": 1, "kind": "polytoken-daemon-credential", "token": "tok-1"})
	h.noticePath = filepath.Join(dir, "notice.json")
	writeJSON(t, h.noticePath, map[string]any{"schema": 1, "revision": 9})
	return h
}

func (h *hookHarness) run(t *testing.T) int {
	t.Helper()
	return RunHook(HookDeps{
		NoticePath:  h.noticePath,
		SessionsDir: h.sessionsDir,
		HTTPClient:  h.server.Client(),
		Stdout:      h.stdout,
		Environ:     func(k string) string { return h.env[k] },
	})
}

func (h *hookHarness) statePath() string {
	return filepath.Join(h.sessionsDir, h.sessionID, "polytoken-quota", "state.json")
}

func (h *hookHarness) consumed(t *testing.T) uint64 {
	t.Helper()
	b, err := os.ReadFile(h.statePath())
	if err != nil {
		return 0
	}
	var st struct {
		ConsumedRevision uint64 `json:"consumed_revision"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state parse: %v", err)
	}
	return st.ConsumedRevision
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	idx := strings.LastIndex(url, ":")
	if idx < 0 {
		t.Fatalf("no port in %s", url)
	}
	var port int
	if _, err := fmt.Sscanf(url[idx+1:], "%d", &port); err != nil {
		t.Fatalf("port in %s: %v", url, err)
	}
	return port
}

// TestHookReloadAdvancesOnSuccess: a new notice revision triggers exactly one
// authenticated POST /reload; the consumed marker advances; a second fire for
// the same revision reloads nothing.
func TestHookReloadAdvancesOnSuccess(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()

	if code := h.run(t); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if h.reloadCalls != 1 {
		t.Fatalf("reload calls = %d, want 1", h.reloadCalls)
	}
	if got := h.consumed(t); got != 9 {
		t.Fatalf("consumed = %d, want 9", got)
	}
	if code := h.run(t); code != 0 || h.reloadCalls != 1 {
		t.Fatalf("second fire: code=%d calls=%d, want 0/1", code, h.reloadCalls)
	}
}

// TestHookReloadConflictDoesNotAdvance: 409 (turn in flight) and 422 leave
// the marker unadvanced so the next event retries; 401 never advances.
func TestHookReloadConflictDoesNotAdvance(t *testing.T) {
	for _, status := range []int{http.StatusConflict, 422, http.StatusUnauthorized} {
		t.Run(map[int]string{409: "conflict", 422: "unavailable-default", 401: "unauthorized"}[status], func(t *testing.T) {
			h := newHookHarness(t)
			defer h.server.Close()
			h.status = status

			if code := h.run(t); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if h.reloadCalls != 1 {
				t.Fatalf("calls = %d, want 1 attempt", h.reloadCalls)
			}
			if got := h.consumed(t); got != 0 {
				t.Fatalf("consumed = %d, want 0 (not advanced)", got)
			}
			// Recovery: once the daemon accepts, the retry converges.
			h.status = http.StatusOK
			if code := h.run(t); code != 0 {
				t.Fatalf("retry exit = %d", code)
			}
			if h.reloadCalls != 2 || h.consumed(t) != 9 {
				t.Fatalf("retry: calls=%d consumed=%d, want 2/9", h.reloadCalls, h.consumed(t))
			}
		})
	}
}

// TestHookSilentNoOps: missing notice, corrupt notice, stale revision,
// missing session artifacts, unreachable daemon, and unknown events are all
// silent no-ops with no HTTP call and no state write.
func TestHookSilentNoOps(t *testing.T) {
	t.Run("missing notice", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		os.Remove(h.noticePath)
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
	t.Run("corrupt notice", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		if err := os.WriteFile(h.noticePath, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
	t.Run("stale revision", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		writeJSON(t, h.statePath(), map[string]any{"consumed_revision": 10})
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
	t.Run("missing session artifacts", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		os.Remove(filepath.Join(h.sessionsDir, h.sessionID, "startup.json"))
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
	t.Run("unreachable daemon", func(t *testing.T) {
		h := newHookHarness(t)
		h.server.Close()
		if code := h.run(t); code != 0 {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("unknown event", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		h.env["POLYTOKEN_HOOK_EVENT"] = "subagent_start"
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
	t.Run("missing session id", func(t *testing.T) {
		h := newHookHarness(t)
		defer h.server.Close()
		delete(h.env, "POLYTOKEN_SESSION_ID")
		if code := h.run(t); code != 0 || h.reloadCalls != 0 {
			t.Fatalf("code=%d calls=%d", code, h.reloadCalls)
		}
	})
}

// TestHookPromptEventAccepts: pre_user_prompt currently acknowledges with
// allow/accept semantics and performs no reload (warning tiers land with the
// warnings task).
func TestHookPromptEventAccepts(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	h.env["POLYTOKEN_HOOK_EVENT"] = "pre_user_prompt"

	if code := h.run(t); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.reloadCalls != 0 {
		t.Fatalf("prompt event must not reload, calls=%d", h.reloadCalls)
	}
	out := strings.TrimSpace(h.stdout.String())
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("stdout = %q, want JSON decision object", out)
	}
	var decision struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(out), &decision); err != nil || decision.Outcome != "accept" {
		t.Fatalf("decision = %q (%v), want accept", out, err)
	}
}
