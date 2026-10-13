package notice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const idleSync = `{"version":1,"daemon_epoch":"epoch-own","cursor":"41","turn":null,"queue":{"revision":"3","items":[]},"interrogatives":[]}`
const idleSummary = `{"version":1,"daemon_epoch":"epoch-own","cursor":"41","busy":false,"attention":false,"history_checkpoint":"checkpoint-own","history_total_items":0}`

func TestHookAuthorityAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, sync, summary, reason string
		status, requests                    int
		admit                               bool
	}{
		{"idle after null", `{"turn_in_flight":null}`, idleSync, idleSummary, "idle_authority", 200, 4, true},
		{"idle after missing", `{}`, idleSync, idleSummary, "idle_authority", 200, 4, true},
		{"legacy active", `{"turn_in_flight":true}`, idleSync, idleSummary, "active", 200, 1, false},
		{"legacy malformed activity", `{"turn_in_flight":"false"}`, idleSync, idleSummary, "activity_unknown", 200, 1, false},
		{"legacy null snapshot", `null`, idleSync, idleSummary, "activity_unknown", 200, 1, false},
		{"legacy wait", `{"turn_in_flight":null,"main_provider_wait":{}}`, idleSync, idleSummary, "activity_unknown", 200, 1, false},
		{"legacy idle", `{"turn_in_flight":false}`, idleSync, idleSummary, "idle", 200, 2, true},
		{"busy", `{}`, idleSync, strings.Replace(idleSummary, `"busy":false`, `"busy":true`, 1), "authority_busy", 200, 3, false},
		{"turn present", `{}`, strings.Replace(idleSync, `"turn":null`, `"turn":{}`, 1), idleSummary, "authority_turn_present", 200, 3, false},
		{"turn missing", `{}`, strings.Replace(idleSync, `"turn":null,`, "", 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"queue pending", `{}`, strings.Replace(idleSync, `"items":[]`, `"items":[{"content":"secret-body"}]`, 1), idleSummary, "authority_queue_pending", 200, 3, false},
		{"queue missing", `{}`, strings.Replace(idleSync, `"queue":{"revision":"3","items":[]},`, "", 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"queue null", `{}`, strings.Replace(idleSync, `"items":[]`, `"items":null`, 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"queue revision missing", `{}`, strings.Replace(idleSync, `"revision":"3",`, "", 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"questions pending", `{}`, strings.Replace(idleSync, `"interrogatives":[]`, `"interrogatives":[{}]`, 1), idleSummary, "authority_attention", 200, 3, false},
		{"questions null", `{}`, strings.Replace(idleSync, `"interrogatives":[]`, `"interrogatives":null`, 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"attention", `{}`, idleSync, strings.Replace(idleSummary, `"attention":false`, `"attention":true`, 1), "authority_attention", 200, 3, false},
		{"busy missing", `{}`, idleSync, strings.Replace(idleSummary, `"busy":false,`, "", 1), "authority_incomplete", 200, 3, false},
		{"attention null", `{}`, idleSync, strings.Replace(idleSummary, `"attention":false`, `"attention":null`, 1), "authority_incomplete", 200, 3, false},
		{"busy wrong type", `{}`, idleSync, strings.Replace(idleSummary, `"busy":false`, `"busy":"false"`, 1), "authority_invalid_json", 200, 3, false},
		{"sync version", `{}`, strings.Replace(idleSync, `"version":1`, `"version":2`, 1), idleSummary, "authority_unsupported_version", 200, 3, false},
		{"summary version", `{}`, idleSync, strings.Replace(idleSummary, `"version":1`, `"version":2`, 1), "authority_unsupported_version", 200, 3, false},
		{"version missing", `{}`, strings.Replace(idleSync, `"version":1,`, "", 1), idleSummary, "authority_unsupported_version", 200, 3, false},
		{"epoch mismatch", `{}`, idleSync, strings.Replace(idleSummary, "epoch-own", "epoch-other", 1), "authority_mismatch", 200, 3, false},
		{"cursor mismatch", `{}`, idleSync, strings.Replace(idleSummary, `"41"`, `"42"`, 1), "authority_mismatch", 200, 3, false},
		{"identity missing", `{}`, strings.Replace(idleSync, `"daemon_epoch":"epoch-own",`, "", 1), idleSummary, "authority_incomplete", 200, 3, false},
		{"history missing", `{}`, idleSync, strings.Replace(idleSummary, `"history_checkpoint":"checkpoint-own",`, "", 1), "authority_incomplete", 200, 3, false},
		{"malformed sync", `{}`, `{"secret":"secret-body"`, idleSummary, "authority_invalid_json", 200, 3, false},
		{"malformed summary", `{}`, idleSync, `null`, "authority_unsupported_version", 200, 3, false},
		{"oversized sync", `{}`, strings.Repeat("s", authorityBodyLimit+1), idleSummary, "authority_too_large", 200, 2, false},
		{"oversized summary", `{}`, idleSync, strings.Repeat("s", authorityBodyLimit+1), "authority_too_large", 200, 3, false},
		{"unavailable", `{}`, "secret-body", idleSummary, "authority_http_non_200", 503, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookHarness(t)
			h.server.Close()
			requests := 0
			h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer tok-1" {
					t.Error("missing own credential")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /state":
					_, _ = io.WriteString(w, tc.legacy)
				case "GET /sync":
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.sync)
				case "GET /sync/summary":
					_, _ = io.WriteString(w, tc.summary)
				case "POST /reload":
					h.reloadCalls++
					_, _ = io.WriteString(w, `{"queued":false}`)
				default:
					t.Error("unexpected request")
				}
			}))
			defer h.server.Close()
			writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, h.server.URL)})
			writeJSON(t, h.statePath(), hookState{ConsumedRevision: 4, UpdatedAt: "unchanged"})
			before, err := os.ReadFile(h.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if h.run(t) != 0 || h.stdout.Len() != 0 || requests != tc.requests {
				t.Fatalf("output/count: requests=%d want=%d", requests, tc.requests)
			}
			after, err := os.ReadFile(h.statePath())
			if err != nil {
				t.Fatal(err)
			}
			if tc.admit {
				if h.reloadCalls != 1 || h.consumed(t) != 9 {
					t.Fatal("idle failed to converge")
				}
			} else if h.reloadCalls != 0 || !bytes.Equal(before, after) {
				t.Fatal("deferred hook changed marker or posted")
			}
			records, raw := diagnosticRecords(t, filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
			found, deferred := false, false
			for _, r := range records {
				found = found || r.Event == "activity_check" && r.Reason == tc.reason
				deferred = deferred || r.Event == "reload_deferred" && r.Reason == tc.reason
			}
			if !found || deferred == tc.admit {
				t.Fatal("missing admission/defer diagnosis")
			}
			for _, secret := range []string{"secret-body", "epoch-own", "checkpoint-own", "tok-1"} {
				if strings.Contains(raw, secret) {
					t.Fatal("authority or credential leaked")
				}
			}
		})
	}
}

func TestHookAuthorityDeferredRevisionRetriesOnLaterIdleEvent(t *testing.T) {
	h := newHookHarness(t)
	h.server.Close()
	active := true
	gets := 0
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/state":
			gets++
			if active {
				_, _ = io.WriteString(w, `{"turn_in_flight":true}`)
			} else {
				_, _ = io.WriteString(w, `{"turn_in_flight":null}`)
			}
		case "/sync":
			gets++
			_, _ = io.WriteString(w, idleSync)
		case "/sync/summary":
			gets++
			_, _ = io.WriteString(w, idleSummary)
		case "/reload":
			h.reloadCalls++
			_, _ = io.WriteString(w, `{"queued":false}`)
		default:
			t.Error("unexpected request")
		}
	}))
	defer h.server.Close()
	writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, h.server.URL)})
	if h.run(t) != 0 || gets != 1 || h.reloadCalls != 0 || h.consumed(t) != 0 {
		t.Fatal("active did not defer")
	}
	active = false
	if h.run(t) != 0 || gets != 4 || h.reloadCalls != 1 || h.consumed(t) != 9 {
		t.Fatal("later authority idle did not converge")
	}
	if h.run(t) != 0 || gets != 4 || h.reloadCalls != 1 {
		t.Fatal("consumed marker repeated authority read")
	}
	h.env["POLYTOKEN_HOOK_EVENT"] = hookEventPrompt
	if h.run(t) != 0 || gets != 4 || h.reloadCalls != 1 {
		t.Fatal("prompt requested authority")
	}
}

func TestHookAuthorityFailuresAndDeadline(t *testing.T) {
	for _, stage := range []string{"/sync", "/sync/summary"} {
		for _, reason := range []string{"authority_unavailable", "authority_read_failed", "authority_http_non_200"} {
			t.Run(stage+reason, func(t *testing.T) {
				d := diagnosticDeps(t, hookEventTurnEnd)
				d.diagnostics = newHookDiagnostics(d, hookEventTurnEnd)
				calls := 0
				d.HTTPClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					deadline, ok := r.Context().Deadline()
					if !ok || time.Until(deadline) > 5*time.Second {
						t.Error("missing bounded deadline")
					}
					body := `{}`
					if r.URL.Path == "/sync" {
						body = idleSync
					}
					if r.URL.Path == stage {
						if reason == "authority_unavailable" {
							return nil, errors.New("secret-error")
						}
						if reason == "authority_read_failed" {
							return &http.Response{StatusCode: 200, Body: io.NopCloser(activityBrokenReader{})}, nil
						}
						return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("secret-error"))}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				if d.daemonIdle(1, "secret-token") {
					t.Fatal("failed observation admitted")
				}
				want := 2
				if stage == "/sync/summary" {
					want = 3
				}
				if calls != want {
					t.Fatal("unbounded request count")
				}
				records, raw := diagnosticRecords(t, diagnosticPath(d))
				found := false
				for _, r := range records {
					found = found || r.Event == "reload_deferred" && r.Reason == reason
				}
				if !found || strings.Contains(raw, "secret-error") || strings.Contains(raw, "secret-token") {
					t.Fatal("failure missing or leaked")
				}
			})
		}
	}
}

func TestAuthorityExpiredContextDoesNotRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reason, _ := authorityIdle(ctx, server.Client(), portOf(t, server.URL), "secret-token")
	if reason != "authority_unavailable" || calls != 0 {
		t.Fatal("expired context attempted request")
	}
}
