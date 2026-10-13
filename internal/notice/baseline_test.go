package notice

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freshHook(t *testing.T) *hookHarness {
	t.Helper()
	h := newHookHarness(t)
	t.Cleanup(h.server.Close)
	if err := os.Remove(h.statePath()); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHookFirstObservationBaseline(t *testing.T) {
	for _, event := range []string{hookEventStart, hookEventPrompt, hookEventTurnEnd} {
		t.Run(event, func(t *testing.T) {
			h := freshHook(t)
			h.env["POLYTOKEN_HOOK_EVENT"] = event
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path == "/state" {
					io.WriteString(w, `{"turn_in_flight":false,"main_provider_wait":null}`)
				} else {
					io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, server.URL)})
			if h.run(t) != 0 || requests != 0 {
				t.Fatalf("baseline requests=%d", requests)
			}
			st := readHookState(h.statePath())
			if st.BaselineRevision == nil || *st.BaselineRevision != 9 || st.ConsumedRevision != 0 {
				t.Fatalf("baseline %+v", st)
			}
			if event == hookEventStart && h.stdout.String() != "{\"outcome\":\"allow\"}\n" {
				t.Fatal(h.stdout.String())
			}
			h.env["POLYTOKEN_HOOK_EVENT"] = hookEventTurnEnd
			h.run(t)
			if requests != 0 {
				t.Fatal("same revision contacted daemon")
			}
			writeJSON(t, h.noticePath, map[string]any{"schema": 1, "revision": 10})
			h.run(t)
			if requests != 2 || h.consumed(t) != 10 {
				t.Fatalf("new revision requests=%d consumed=%d", requests, h.consumed(t))
			}
		})
	}
}

func TestHookBaselineNewerDefersUntilIdle(t *testing.T) {
	h := freshHook(t)
	busy := true
	gets, posts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			gets++
			if busy {
				io.WriteString(w, `{"turn_in_flight":true}`)
			} else {
				io.WriteString(w, `{"turn_in_flight":false,"main_provider_wait":null}`)
			}
			return
		}
		posts++
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, server.URL)})
	h.env["POLYTOKEN_HOOK_EVENT"] = hookEventStart
	h.run(t)
	if gets != 0 || posts != 0 {
		t.Fatal("baseline HTTP")
	}
	writeJSON(t, h.noticePath, map[string]any{"schema": 1, "revision": 10})
	h.env["POLYTOKEN_HOOK_EVENT"] = hookEventPrompt
	h.run(t)
	if gets != 0 || posts != 0 {
		t.Fatal("prompt HTTP")
	}
	h.env["POLYTOKEN_HOOK_EVENT"] = hookEventTurnEnd
	h.run(t)
	if gets != 1 || posts != 0 || h.consumed(t) != 0 {
		t.Fatal("busy did not defer")
	}
	busy = false
	h.run(t)
	if gets != 2 || posts != 1 || h.consumed(t) != 10 {
		t.Fatal("idle did not consume")
	}
}

func TestHookBaselineMissingMalformedAndZero(t *testing.T) {
	for _, notice := range []string{"", `{bad`, `{"schema":1}`, `{"schema":1,"revision":null}`} {
		t.Run(notice, func(t *testing.T) {
			h := freshHook(t)
			h.env["POLYTOKEN_HOOK_EVENT"] = hookEventStart
			if notice == "" {
				os.Remove(h.noticePath)
			} else if err := os.WriteFile(h.noticePath, []byte(notice), 0600); err != nil {
				t.Fatal(err)
			}
			h.run(t)
			if st := readHookState(h.statePath()); st.BaselineRevision != nil {
				t.Fatal("invalid notice initialized baseline")
			}
			writeJSON(t, h.noticePath, map[string]any{"schema": 1, "revision": 0})
			h.run(t)
			st := readHookState(h.statePath())
			if st.BaselineRevision == nil || *st.BaselineRevision != 0 {
				t.Fatal("zero baseline not explicit")
			}
			h.env["POLYTOKEN_HOOK_EVENT"] = hookEventTurnEnd
			writeJSON(t, h.noticePath, map[string]any{"schema": 1, "revision": 1})
			h.run(t)
			if h.consumed(t) != 1 {
				t.Fatal("zero baseline swallowed later notice")
			}
		})
	}
}

func TestHookLegacyBaselinePreservesConsumption(t *testing.T) {
	for _, state := range []map[string]any{
		{"consumed_revision": 8, "last_seen_model": "old", "last_prompt_consumed": 7},
		{"last_seen_model": "old"},
		{},
	} {
		h := freshHook(t)
		writeJSON(t, h.statePath(), state)
		h.run(t)
		st := readHookState(h.statePath())
		want := uint64(9)
		if _, consumed := state["consumed_revision"]; consumed {
			want = 8
			if h.consumed(t) != 9 {
				t.Fatal("legacy new notice swallowed")
			}
		} else if h.reloadCalls != 0 {
			t.Fatal("legacy prompt-only state reloaded baseline")
		}
		if st.BaselineRevision == nil || *st.BaselineRevision != want || st.LastSeenModel != stateString(state, "last_seen_model") {
			t.Fatalf("legacy state %+v", st)
		}
	}
}

func stateString(state map[string]any, key string) string { s, _ := state[key].(string); return s }

func TestHookStateRecoveryNeverOverwrites(t *testing.T) {
	for _, event := range []string{hookEventStart, hookEventPrompt, hookEventTurnEnd} {
		for _, content := range []string{`{bad`, `null`, `{"baseline_revision":null}`, `{"consumed_revision":"bad"}`} {
			t.Run(event+content, func(t *testing.T) {
				h := freshHook(t)
				h.env["POLYTOKEN_HOOK_EVENT"] = event
				h.env["POLYTOKEN_MODEL_NAME"] = "model"
				if err := os.WriteFile(h.statePath(), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				h.run(t)
				got, _ := os.ReadFile(h.statePath())
				if string(got) != content || h.reloadCalls != 0 {
					t.Fatal("damaged state changed or reloaded")
				}
				log, _ := os.ReadFile(filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
				if !bytes.Contains(log, []byte(`"event":"state_recovery"`)) {
					t.Fatal("no recovery diagnostic")
				}
			})
		}
	}
	for _, kind := range []string{"directory", "dangling", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			h := freshHook(t)
			switch kind {
			case "directory":
				if err := os.Mkdir(h.statePath(), 0700); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), h.statePath()); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				writeJSON(t, h.statePath(), map[string]any{"consumed_revision": 8})
				os.Chmod(h.statePath(), 0000)
				defer os.Chmod(h.statePath(), 0600)
				if _, err := os.ReadFile(h.statePath()); err == nil {
					t.Skip("host can read mode-000 file")
				}
			}
			h.run(t)
			if h.reloadCalls != 0 {
				t.Fatal("unreadable state reloaded")
			}
			if _, err := os.Lstat(h.statePath()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHookBaselineWriteFailureDoesNotAdvance(t *testing.T) {
	h := freshHook(t)
	d := HookDeps{SessionsDir: h.sessionsDir, NoticePath: h.noticePath, Environ: func(k string) string { return h.env[k] }}
	unlock, ok := d.lockState()
	if !ok {
		t.Fatal("lock unavailable")
	}
	defer unlock()
	// A destination directory refuses the rooted write, independent of uid.
	if err := os.Mkdir(h.statePath(), 0700); err != nil {
		t.Fatal(err)
	}
	st := hookState{ConsumedRevision: 8}
	if _, ok := d.ensureBaseline(&st, 9); ok || st.BaselineRevision != nil || st.ConsumedRevision != 8 {
		t.Fatalf("advanced on write failure: %+v", st)
	}
	if h.reloadCalls != 0 {
		t.Fatal("write failure reloaded")
	}
}

func TestHookBaselineHandlerWriteFailureNoHTTP(t *testing.T) {
	for _, event := range []string{hookEventStart, hookEventPrompt, hookEventTurnEnd} {
		t.Run(event, func(t *testing.T) {
			h := freshHook(t)
			h.env["POLYTOKEN_HOOK_EVENT"] = event
			h.env["POLYTOKEN_MODEL_NAME"] = "model"
			requests := 0
			d := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, Stdout: h.stdout, Environ: func(k string) string { return h.env[k] }, HTTPClient: &http.Client{Transport: diagnosticTransport(func(*http.Request) (*http.Response, error) { requests++; return nil, nil })}}
			// Obstruct the destination only after the successful missing-state read.
			clockCalls := 0
			d.Now = func() time.Time {
				clockCalls++
				// Start/state/notice diagnostics precede the baseline timestamp.
				if clockCalls == 4 {
					if err := os.Mkdir(h.statePath(), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return time.Now()
			}
			RunHook(d)
			if requests != 0 || h.reloadCalls != 0 {
				t.Fatal("baseline write failure contacted daemon")
			}
			st, err := os.Stat(h.statePath())
			if err != nil || !st.IsDir() {
				t.Fatal("failed write replaced destination")
			}
			log, _ := os.ReadFile(filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
			if !bytes.Contains(log, []byte(`"event":"baseline_init","reason":"write_failed"`)) {
				t.Fatal("missing baseline failure diagnostic")
			}
			if err := os.Remove(h.statePath()); err != nil {
				t.Fatal(err)
			}
			d.Now = nil
			RunHook(d)
			if st := readHookState(h.statePath()); st.BaselineRevision == nil || *st.BaselineRevision != 9 || st.ConsumedRevision != 0 || requests != 0 {
				t.Fatal("retry did not baseline silently")
			}
		})
	}
}

func TestHookConcurrentBaselineSkipsPrompt(t *testing.T) {
	h := freshHook(t)
	entered, release := make(chan struct{}), make(chan struct{})
	deps := func(event string) HookDeps {
		return HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, Stdout: io.Discard, Environ: func(k string) string {
			switch k {
			case "POLYTOKEN_HOOK_EVENT":
				return event
			case "POLYTOKEN_MODEL_NAME":
				return "model"
			default:
				return h.env[k]
			}
		}}
	}
	start := deps(hookEventStart)
	clockCalls := 0
	start.Now = func() time.Time {
		clockCalls++
		if clockCalls == 4 {
			close(entered)
			<-release
		}
		return time.Now()
	}
	done := make(chan struct{})
	go func() { RunHook(start); close(done) }()
	<-entered
	RunHook(deps(hookEventPrompt))
	close(release)
	<-done
	RunHook(deps(hookEventPrompt))
	st := readHookState(h.statePath())
	if st.BaselineRevision == nil || *st.BaselineRevision != 9 || st.LastSeenModel != "model" || st.ConsumedRevision != 0 || h.reloadCalls != 0 {
		t.Fatalf("baseline/prompt transition %+v", st)
	}
}

func TestHookConcurrentReloadSkipsPromptAndBaseline(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			io.WriteString(w, `{"turn_in_flight":false,"main_provider_wait":null}`)
			return
		}
		close(entered)
		<-release
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	writeJSON(t, filepath.Join(h.sessionsDir, h.sessionID, "startup.json"), map[string]any{"port": portOf(t, server.URL)})
	deps := func(event string, out io.Writer) HookDeps {
		return HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, HTTPClient: server.Client(), Stdout: out, Environ: func(k string) string {
			switch k {
			case "POLYTOKEN_HOOK_EVENT":
				return event
			case "POLYTOKEN_MODEL_NAME":
				return "model"
			default:
				return h.env[k]
			}
		}}
	}
	done := make(chan struct{})
	go func() { RunHook(deps(hookEventTurnEnd, io.Discard)); close(done) }()
	<-entered
	var prompt, start strings.Builder
	RunHook(deps(hookEventPrompt, &prompt))
	RunHook(deps(hookEventStart, &start))
	close(release)
	<-done
	if h.consumed(t) != 9 || prompt.String() != "{\"outcome\":\"accept\"}\n" || start.String() != "{\"outcome\":\"allow\"}\n" {
		t.Fatal("concurrent handler lost consumption or failed closed")
	}
	RunHook(deps(hookEventPrompt, io.Discard))
	if h.consumed(t) != 9 || readHookState(h.statePath()).LastPromptConsumed != 9 {
		t.Fatal("later prompt lost consumption")
	}
}
