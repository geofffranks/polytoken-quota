package notice

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookActivityAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
		admit              bool
	}{
		{"idle", `{"turn_in_flight":false,"main_provider_wait":null,"env":{"TOKEN":"secret-env"}}`, "idle", 200, true},
		{"idle older snapshot", `{"turn_in_flight":false}`, "idle", 200, true},
		{"active", `{"turn_in_flight":true,"env":{"TOKEN":"secret-env"}}`, "active", 200, false},
		{"wait", `{"turn_in_flight":false,"main_provider_wait":{"phase":"secret-phase"}}`, "provider_wait", 200, false},
		{"unknown wait", `{"turn_in_flight":false,"main_provider_wait":false}`, "provider_wait", 200, false},
		{"null activity", `{"turn_in_flight":null}`, "activity_unknown", 200, false},
		{"missing", `{"env":{"TOKEN":"secret-env"}}`, "activity_unknown", 200, false},
		{"wrong type", `{"turn_in_flight":"secret-env"}`, "activity_unknown", 200, false},
		{"null snapshot", `null`, "activity_unknown", 200, false},
		{"malformed", `{"turn_in_flight":false,"env":"secret-env"`, "invalid_json", 200, false},
		{"trailing", `{"turn_in_flight":false} secret-env`, "invalid_json", 200, false},
		{"oversized", `{"turn_in_flight":false,"env":"` + strings.Repeat("s", activityBodyLimit) + `"}`, "too_large", 200, false},
		{"unavailable", `secret-env`, "http_non_200", 503, false},
		{"unauthorized", `secret-env`, "http_non_200", 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookHarness(t)
			h.server.Close()
			stateCalls := 0
			h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok-1" {
					t.Error("missing own-session credential")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/state":
					stateCalls++
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case r.Method == http.MethodGet && r.URL.Path == "/sync":
					// Unknown legacy activity attempts authority once; unavailable
					// authority must still leave the marker unchanged.
					w.WriteHeader(http.StatusServiceUnavailable)
				case r.Method == http.MethodPost && r.URL.Path == "/reload":
					h.reloadCalls++
					if stateCalls != 1 {
						t.Error("reload before state check")
					}
					_, _ = io.WriteString(w, `{"queued":false}`)
				default:
					t.Error("unexpected request")
				}
			}))
			defer h.server.Close()
			writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, h.server.URL)})
			writeJSON(t, h.statePath(), hookState{ConsumedRevision: 4, LastSeenModel: "secret-model", UpdatedAt: "unchanged"})
			before, err := os.ReadFile(h.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if h.run(t) != 0 || h.stdout.Len() != 0 || stateCalls != 1 {
				t.Fatal("activity check disturbed hook output or request count")
			}
			after, err := os.ReadFile(h.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if tc.admit {
				if h.reloadCalls != 1 || h.consumed(t) != 9 {
					t.Fatal("idle did not reload and consume")
				}
			} else if h.reloadCalls != 0 || !bytes.Equal(before, after) {
				t.Fatal("deferred activity posted reload or changed marker")
			}
			records, raw := diagnosticRecords(t, filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
			foundCheck, foundDeferred := false, false
			deferReason := tc.reason
			if tc.reason == "activity_unknown" && authorityFallbackAllowed([]byte(tc.body)) {
				deferReason = "authority_http_non_200"
			}
			for _, r := range records {
				foundCheck = foundCheck || r.Event == "activity_check" && r.Reason == tc.reason
				foundDeferred = foundDeferred || r.Event == "reload_deferred" && r.Reason == deferReason
			}
			if !foundCheck || foundDeferred == tc.admit {
				t.Fatal("missing activity/deferral reason")
			}
			for _, secret := range []string{"secret-env", "secret-phase", "secret-model", "tok-1", tc.body} {
				if strings.Contains(raw, secret) {
					t.Fatal("state or credential leaked to log")
				}
			}
		})
	}
}

func TestHookActivityTransportAndReadFailure(t *testing.T) {
	for _, reason := range []string{"transport_failed", "read_failed"} {
		t.Run(reason, func(t *testing.T) {
			h := newHookHarness(t)
			defer h.server.Close()
			d := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, Stdout: h.stdout, Environ: func(k string) string { return h.env[k] }}
			d.HTTPClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/state" {
					t.Fatal("posted after failed activity check")
				}
				if reason == "transport_failed" {
					return nil, errors.New("secret-error")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(activityBrokenReader{})}, nil
			})}
			if RunHook(d) != 0 || h.stdout.Len() != 0 || h.consumed(t) != 0 {
				t.Fatal("not silent deferral")
			}
			records, raw := diagnosticRecords(t, diagnosticPath(d))
			found := false
			for _, r := range records {
				found = found || r.Event == "reload_deferred" && r.Reason == reason
			}
			if !found || strings.Contains(raw, "secret-error") {
				t.Fatal("missing deferral or leaked error")
			}
		})
	}
}

type activityBrokenReader struct{}

func (activityBrokenReader) Read([]byte) (int, error) { return 0, errors.New("secret-error") }
