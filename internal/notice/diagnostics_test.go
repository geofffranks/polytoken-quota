package notice

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func diagnosticDeps(t *testing.T, event string) HookDeps {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(filepath.Join(root, "sess-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	return HookDeps{SessionsDir: root, Stdout: &bytes.Buffer{}, Environ: func(k string) string {
		switch k {
		case "POLYTOKEN_SESSION_ID":
			return "sess-1"
		case "POLYTOKEN_HOOK_EVENT":
			return event
		case "POLYTOKEN_MODEL_NAME":
			return "secret-model"
		}
		return ""
	}}
}

func diagnosticRecords(t *testing.T, path string) ([]diagnosticRecord, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []diagnosticRecord
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var r diagnosticRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("invalid record: %v", err)
		}
		records = append(records, r)
	}
	return records, string(b)
}

func diagnosticPath(d HookDeps) string {
	return filepath.Join(d.SessionsDir, "sess-1", "polytoken-quota", diagnosticFile)
}

func TestHookDiagnosticReloadPolicyAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		consumed   uint64
		parse      string
	}{
		{"queued", `{"queued":true,"failed":[],"failed_details":["secret-error"],"token":"secret-token"}`, 200, 9, "parsed"},
		{"partial", `{"queued":false,"failed":["providers","secret-model","providers"],"facets_changed":true,"failed_details":["secret-error"]}`, 200, 9, "parsed"},
		{"bad", `secret-body`, 200, 9, "invalid_json"},
		{"oversized", strings.Repeat("s", reloadBodyLimit+1), 200, 9, "too_large"},
		{"busy", `{"queued":true}`, 409, 0, "parsed"},
		{"empty", ``, 200, 9, "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnosticDeps(t, hookEventTurnEnd)
			d.NoticePath = filepath.Join(t.TempDir(), "notice.json")
			writeJSON(t, d.NoticePath, map[string]any{"schema": 1, "revision": 9})
			session := filepath.Join(d.SessionsDir, "sess-1")
			if err := writeHookState(d.statePath(session), hookState{BaselineRevision: new(uint64)}); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, filepath.Join(session, "startup.json"), map[string]any{"port": 5})
			writeJSON(t, filepath.Join(session, "credential.json"), map[string]any{"token": "secret-token"})
			d.HTTPClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && r.URL.Path == "/state" && r.Header.Get("Authorization") == "Bearer secret-token" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"turn_in_flight":false,"env":{"TOKEN":"secret-env"}}`)), Header: make(http.Header)}, nil
				}
				if r.Method != "POST" || r.URL.Path != "/reload" || r.Header.Get("Authorization") != "Bearer secret-token" {
					t.Fatal("request changed")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			if RunHook(d) != 0 {
				t.Fatal("not fail open")
			}
			if d.Stdout.(*bytes.Buffer).Len() != 0 {
				t.Fatal("diagnostics reached stdout")
			}
			if got := readHookState(d.statePath(session)).ConsumedRevision; got != tc.consumed {
				t.Fatalf("consumed %d", got)
			}
			records, raw := diagnosticRecords(t, diagnosticPath(d))
			for _, secret := range []string{"secret-token", "secret-model", "secret-body", "secret-error", "secret-env", d.NoticePath, d.SessionsDir} {
				if strings.Contains(raw, secret) {
					t.Fatalf("leaked %q", secret)
				}
			}
			if records[0].Event != "hook_started" || records[len(records)-1].Event != "hook_finished" {
				t.Fatal("missing boundaries")
			}
			var result *diagnosticRecord
			for i := range records {
				r := &records[i]
				if r.Invocation != records[0].Invocation || len(r.Invocation) != 32 || r.Timestamp == "" || r.Build.GoVersion == "" {
					t.Fatal("missing identity")
				}
				if r.Event == "reload_result" {
					result = r
				}
			}
			if result == nil || result.HTTPStatus != tc.status || result.Response.Parse != tc.parse {
				t.Fatal("missing response summary")
			}
			if tc.name == "queued" && (result.Response.Queued == nil || !*result.Response.Queued || result.Reason != "http_200_queued_unexpected") {
				t.Fatal("queued not recorded")
			}
			if tc.name == "partial" && (result.Response.FailedCount != 3 || result.Response.UnknownFailedCount != 1 || len(result.Response.Failed) != 1) {
				t.Fatal("failed not filtered")
			}
		})
	}
}

func TestHookDiagnosticPromptAndNoops(t *testing.T) {
	d := diagnosticDeps(t, hookEventPrompt)
	d.NoticePath = filepath.Join(t.TempDir(), "notice.json")
	writeJSON(t, d.NoticePath, map[string]any{"schema": 1, "revision": 9, "disabled_models": []string{"secret-model"}})
	RunHook(d)
	var decision map[string]any
	if json.Unmarshal(d.Stdout.(*bytes.Buffer).Bytes(), &decision) != nil || decision["outcome"] != "accept" {
		t.Fatal("prompt changed")
	}
	records, raw := diagnosticRecords(t, diagnosticPath(d))
	if strings.Contains(raw, "secret-model") || strings.Contains(raw, "additional_context") {
		t.Fatal("prompt leaked")
	}
	found := false
	for _, r := range records {
		if r.Event == "prompt_warnings" && r.WarningCount == 1 && len(r.WarningCategories) == 1 && r.WarningCategories[0] == "disabled_model" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing warning category")
	}
	// A second prompt suppresses the same warning, just as before diagnostics.
	d.Stdout.(*bytes.Buffer).Reset()
	RunHook(d)
	if d.Stdout.(*bytes.Buffer).String() != "{\"outcome\":\"accept\"}\n" {
		t.Fatal("repeated warning")
	}
	records, _ = diagnosticRecords(t, diagnosticPath(d))
	found = false
	for _, r := range records {
		if r.Reason == "warning_already_reported" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing noop")
	}
}

func TestHookDiagnosticUnavailableLogIsSilent(t *testing.T) {
	for _, mode := range []string{"directory", "symlink", "hardlink", "public", "oversized", "quota_symlink", "lock_symlink", "locked"} {
		t.Run(mode, func(t *testing.T) {
			d := diagnosticDeps(t, hookEventPrompt)
			d.NoticePath = filepath.Join(t.TempDir(), "missing.json")
			quota := filepath.Dir(diagnosticPath(d))
			if err := os.Mkdir(quota, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory":
				if err := os.Mkdir(diagnosticPath(d), 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(outside, diagnosticPath(d)); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, diagnosticPath(d)); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(diagnosticPath(d), []byte("untouched"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(diagnosticPath(d), bytes.Repeat([]byte("x"), diagnosticLimit+1), 0o600); err != nil {
					t.Fatal(err)
				}
			case "quota_symlink":
				if err := os.Remove(quota); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), quota); err != nil {
					t.Fatal(err)
				}
			case "lock_symlink":
				if err := os.Symlink(outside, filepath.Join(quota, ".diagnostics.lock")); err != nil {
					t.Fatal(err)
				}
			case "locked":
				f := privateDiagnosticFile(mustOpenRoot(t, quota), ".diagnostics.lock")
				if f == nil {
					t.Fatal("lock")
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			if RunHook(d) != 0 || d.Stdout.(*bytes.Buffer).String() != "{\"outcome\":\"accept\"}\n" {
				t.Fatal("log failure affected prompt")
			}
			b, err := os.ReadFile(outside)
			if err != nil || string(b) != "untouched" {
				t.Fatal("outside modified")
			}
		})
	}
}

func mustOpenRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestHookDiagnosticReloadWithUnavailableLog(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	path := filepath.Join(filepath.Dir(h.statePath()), diagnosticFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if h.run(t) != 0 || h.reloadCalls != 1 || h.consumed(t) != 9 || h.stdout.Len() != 0 {
		t.Fatal("unavailable log affected reload policy")
	}
}

func TestHookDiagnosticAvailabilityReasons(t *testing.T) {
	for _, tc := range []struct{ name, artifact, contents, reason string }{
		{"notice_missing", "notice", "", "missing"},
		{"notice_invalid", "notice", "{secret", "invalid_json"},
		{"notice_schema", "notice", `{"schema":99,"revision":8}`, "unsupported_schema"},
		{"startup_missing", "startup", "", "missing"},
		{"startup_invalid", "startup", "{secret", "invalid_json"},
		{"startup_port", "startup", `{"port":0}`, "invalid_port"},
		{"credential_missing", "credential", "", "missing"},
		{"credential_invalid", "credential", "{secret", "invalid_json"},
		{"credential_empty", "credential", `{"token":""}`, "empty_token"},
		{"state_invalid", "state", "{secret", "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnosticDeps(t, hookEventTurnEnd)
			d.diagnostics = newHookDiagnostics(d, hookEventTurnEnd)
			session := filepath.Join(d.SessionsDir, "sess-1")
			path := filepath.Join(session, tc.artifact+".json")
			if tc.contents != "" {
				if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.artifact {
			case "notice":
				d.NoticePath = path
				d.noticeRevision()
			case "startup":
				d.readPort(session, "session")
			case "credential":
				d.readToken(session, "session")
			case "state":
				d.readState(path)
			}
			records, raw := diagnosticRecords(t, diagnosticPath(d))
			if records[0].Reason != tc.reason || strings.Contains(raw, "secret") || strings.Contains(raw, path) {
				t.Fatal("unsafe or missing reason")
			}
		})
	}
}

func TestHookDiagnosticSessionSanitization(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", "/absolute", "a/b", "a\\b", "bad\nname", strings.Repeat("a", 129)} {
		d := diagnosticDeps(t, hookEventPrompt)
		d.Environ = func(string) string { return id }
		if newHookDiagnostics(d, hookEventPrompt) != nil {
			t.Fatalf("accepted %q", id)
		}
	}
}

func TestDiagnosticRotationAndConcurrency(t *testing.T) {
	// Subprocesses exercise the same lock used by independently invoked hooks.
	if os.Getenv("QUOTA_DIAGNOSTIC_TEST_ROOT") != "" {
		l := &hookDiagnostics{root: os.Getenv("QUOTA_DIAGNOSTIC_TEST_ROOT"), session: "sess-1"}
		for i := 0; i < 300; i++ {
			l.append([]byte("{\"event\":\"child\"}\n"))
		}
		return
	}
	d := diagnosticDeps(t, hookEventTurnEnd)
	l := newHookDiagnostics(d, hookEventTurnEnd)
	line := []byte("{\"event\":\"rotation\",\"padding\":\"" + strings.Repeat("x", 1000) + "\"}\n")
	for i := 0; i < 200; i++ {
		l.append(line)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.append(line)
			}
		}()
	}
	commands := make([]*exec.Cmd, 3)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestDiagnosticRotationAndConcurrency$")
		commands[i].Env = append(os.Environ(), "QUOTA_DIAGNOSTIC_TEST_ROOT="+d.SessionsDir)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{diagnosticFile, diagnosticFile + ".1"} {
		path := filepath.Join(filepath.Dir(diagnosticPath(d)), name)
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > diagnosticLimit || st.Mode().Perm() != 0o600 {
			t.Fatal("unbounded or nonprivate log")
		}
		diagnosticRecords(t, path)
	}
	st, err := os.Stat(filepath.Dir(diagnosticPath(d)))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatal("nonprivate directory")
	}
}

func TestReloadDiagnosticBoundedAndReadFailure(t *testing.T) {
	var b boundedReloadBody
	b.Write([]byte(`{"queued":true}`))
	if got := parseReloadDiagnostic(b, errors.New("secret-error")); got.Parse != "read_failed" || got.Queued != nil {
		t.Fatal("unsafe read error")
	}
	b.Write(bytes.Repeat([]byte("x"), reloadBodyLimit*2))
	if len(b.bytes) != reloadBodyLimit || !b.oversized {
		t.Fatal("unbounded capture")
	}
}

func TestHookDiagnosticTransportFailure(t *testing.T) {
	d := diagnosticDeps(t, hookEventTurnEnd)
	d.diagnostics = newHookDiagnostics(d, hookEventTurnEnd)
	d.HTTPClient = &http.Client{Transport: diagnosticTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret-error") })}
	if d.postReload(5, "secret-token") {
		t.Fatal("transport succeeded")
	}
	records, raw := diagnosticRecords(t, diagnosticPath(d))
	if strings.Contains(raw, "secret-error") || strings.Contains(raw, "secret-token") {
		t.Fatal("leaked")
	}
	if records[len(records)-1].Reason != "transport_failed" {
		t.Fatal("missing failure")
	}
}

func TestHookDiagnosticStateReadFailure(t *testing.T) {
	d := diagnosticDeps(t, hookEventPrompt)
	d.NoticePath = filepath.Join(t.TempDir(), "missing")
	quota := filepath.Dir(diagnosticPath(d))
	if err := os.Mkdir(quota, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(quota, "state.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	d.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	if RunHook(d) != 0 || d.Stdout.(*bytes.Buffer).String() != "{\"outcome\":\"accept\"}\n" {
		t.Fatal("marker failure affected hook")
	}
	records, _ := diagnosticRecords(t, diagnosticPath(d))
	found := false
	for _, r := range records {
		if r.Event == "state_recovery" && r.Reason == "unsafe_state" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing recovery diagnostic")
	}
}
