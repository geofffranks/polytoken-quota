package notice

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHookActivityDoesNotFollowRedirect(t *testing.T) {
	followed := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		_, _ = io.WriteString(w, `{"turn_in_flight":false}`)
	}))
	defer other.Close()
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/state", http.StatusFound)
	}))
	defer own.Close()
	d := diagnosticDeps(t, hookEventTurnEnd)
	d.HTTPClient = own.Client()
	if d.daemonIdle(portOf(t, own.URL), "secret-token") || followed {
		t.Fatal("activity check followed redirect outside own daemon")
	}
	if d.HTTPClient.CheckRedirect != nil {
		t.Fatal("changed caller redirect policy")
	}
}

func TestHookActivityDeferredRevisionRetriesWhenIdle(t *testing.T) {
	h := newHookHarness(t)
	h.server.Close()
	active := true
	stateCalls := 0
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/state":
			stateCalls++
			if active {
				_, _ = io.WriteString(w, `{"turn_in_flight":true}`)
			} else {
				_, _ = io.WriteString(w, `{"turn_in_flight":false}`)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/reload":
			h.reloadCalls++
			_, _ = io.WriteString(w, `{"queued":false}`)
		default:
			t.Error("unexpected request")
		}
	}))
	defer h.server.Close()
	writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, h.server.URL)})
	if h.run(t) != 0 || stateCalls != 1 || h.reloadCalls != 0 || h.consumed(t) != 0 {
		t.Fatal("active event did not defer")
	}
	active = false
	if h.run(t) != 0 || stateCalls != 2 || h.reloadCalls != 1 || h.consumed(t) != 9 {
		t.Fatal("idle event did not retry deferred revision")
	}
	if h.run(t) != 0 || stateCalls != 2 || h.reloadCalls != 1 {
		t.Fatal("consumed revision requested state")
	}
	h.env["POLYTOKEN_HOOK_EVENT"] = hookEventPrompt
	if h.run(t) != 0 || stateCalls != 2 || h.reloadCalls != 1 {
		t.Fatal("prompt event requested state")
	}
}
