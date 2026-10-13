package notice

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// An idle observation is not atomic with reload. The daemon may accept a
// queued reload after a turn starts; keep HTTP-200 marker policy and diagnose it.
func TestHookAuthorityGETToPOSTRaceKeepsAcceptancePolicy(t *testing.T) {
	h := newHookHarness(t)
	h.server.Close()
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /state":
			_, _ = io.WriteString(w, `{"turn_in_flight":null}`)
		case "GET /sync":
			_, _ = io.WriteString(w, idleSync)
		case "GET /sync/summary":
			_, _ = io.WriteString(w, idleSummary)
		case "POST /reload":
			h.reloadCalls++
			_, _ = io.WriteString(w, `{"queued":true}`)
		default:
			t.Error("unexpected request")
		}
	}))
	defer h.server.Close()
	writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, h.server.URL)})
	if h.run(t) != 0 || h.reloadCalls != 1 || h.consumed(t) != 9 {
		t.Fatal("HTTP-200 acceptance policy changed")
	}
	records, _ := diagnosticRecords(t, filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
	found := false
	for _, r := range records {
		found = found || r.Event == "reload_result" && r.Reason == "http_200_queued_unexpected"
	}
	if !found {
		t.Fatal("queued race acceptance not diagnosed")
	}
}

func TestHookAuthorityDoesNotFollowRedirect(t *testing.T) {
	for _, path := range []string{"/sync", "/sync/summary"} {
		t.Run(path, func(t *testing.T) {
			followed := false
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				followed = true
				_, _ = io.WriteString(w, idleSummary)
			}))
			defer other.Close()
			own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					http.Redirect(w, r, other.URL+path, http.StatusFound)
				} else if r.URL.Path == "/state" {
					_, _ = io.WriteString(w, `{}`)
				} else {
					_, _ = io.WriteString(w, idleSync)
				}
			}))
			defer own.Close()
			d := diagnosticDeps(t, hookEventTurnEnd)
			d.HTTPClient = own.Client()
			if d.daemonIdle(portOf(t, own.URL), "secret-token") || followed {
				t.Fatal("authority followed redirect outside own daemon")
			}
		})
	}
}
