package notice

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type foreignFileInfo struct {
	os.FileInfo
	owner syscall.Stat_t
}

func (f foreignFileInfo) Sys() any { return &f.owner }

func TestHookStateOwnershipChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := foreignFileInfo{FileInfo: st, owner: *st.Sys().(*syscall.Stat_t)}
	foreign.owner.Uid = uint32(os.Getuid() + 1)
	if privateStateInfo(foreign) {
		t.Fatal("foreign-owned state accepted")
	}
	st, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	foreign = foreignFileInfo{FileInfo: st, owner: *st.Sys().(*syscall.Stat_t)}
	foreign.owner.Uid = uint32(os.Getuid() + 1)
	if ownedDirectory(foreign) {
		t.Fatal("foreign-owned directory accepted")
	}
}

func TestHookDiagnosticEventAttribution(t *testing.T) {
	for _, event := range []string{hookEventStart, hookEventPrompt, hookEventTurnEnd, "unsupported"} {
		t.Run(event, func(t *testing.T) {
			h := freshHook(t)
			h.env["POLYTOKEN_HOOK_EVENT"] = event
			h.run(t)
			records, _ := diagnosticRecords(t, filepath.Join(filepath.Dir(h.statePath()), diagnosticFile))
			want := event
			if event == "unsupported" {
				want = "unknown"
			}
			for _, r := range records {
				if r.HookEvent != want {
					t.Fatalf("event attribution %q want %q", r.HookEvent, want)
				}
			}
		})
	}
}

func TestHookRejectsUnsafeStatePaths(t *testing.T) {
	for _, event := range []string{hookEventStart, hookEventPrompt, hookEventTurnEnd} {
		for _, attack := range []string{"traversal", "absolute", "slash", "symlink-session", "symlink-quota", "symlink-lock", "dangling-lock", "hardlink-lock", "public-lock", "directory-lock", "foreign-lock", "foreign-quota", "foreign-state", "public-root", "public-session", "public-quota", "symlink-state", "hardlink-state", "public-state"} {
			t.Run(event+"/"+attack, func(t *testing.T) {
				h := newHookHarness(t)
				t.Cleanup(h.server.Close)
				h.env["POLYTOKEN_HOOK_EVENT"] = event
				h.env["POLYTOKEN_MODEL_NAME"] = "model"
				session := filepath.Join(h.sessionsDir, h.sessionID)
				quota := filepath.Dir(h.statePath())
				outside := t.TempDir()
				canary := filepath.Join(outside, "canary")
				if err := os.WriteFile(canary, []byte(`{"baseline_revision":0}`), 0600); err != nil {
					t.Fatal(err)
				}
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				switch attack {
				case "traversal":
					h.env["POLYTOKEN_SESSION_ID"] = "../../" + filepath.Base(outside)
				case "absolute":
					h.env["POLYTOKEN_SESSION_ID"] = outside
				case "slash":
					h.env["POLYTOKEN_SESSION_ID"] = "sess-1/nested"
				case "symlink-session":
					must(os.Rename(session, session+"-original"))
					must(os.Symlink(outside, session))
				case "symlink-quota":
					must(os.Rename(quota, quota+"-original"))
					must(os.Symlink(outside, quota))
				case "symlink-lock":
					must(os.Symlink(canary, filepath.Join(quota, "state.lock")))
				case "dangling-lock":
					must(os.Symlink(filepath.Join(outside, "missing"), filepath.Join(quota, "state.lock")))
				case "hardlink-lock":
					must(os.Link(canary, filepath.Join(quota, "state.lock")))
				case "public-lock":
					must(os.WriteFile(filepath.Join(quota, "state.lock"), nil, 0644))
				case "directory-lock":
					must(os.Mkdir(filepath.Join(quota, "state.lock"), 0700))
				case "foreign-lock":
					must(os.WriteFile(filepath.Join(quota, "state.lock"), nil, 0600))
					if err := os.Chown(filepath.Join(quota, "state.lock"), os.Getuid()+1, -1); err != nil {
						t.Skip("host does not permit foreign-owner fixture")
					}
				case "foreign-quota":
					if err := os.Chown(quota, os.Getuid()+1, -1); err != nil {
						t.Skip("host does not permit foreign-owner fixture")
					}
					defer os.Chown(quota, os.Getuid(), -1)
				case "foreign-state":
					if err := os.Chown(h.statePath(), os.Getuid()+1, -1); err != nil {
						t.Skip("host does not permit foreign-owner fixture")
					}
				case "public-root":
					must(os.Chmod(h.sessionsDir, 0777))
					defer os.Chmod(h.sessionsDir, 0700)
				case "public-session":
					must(os.Chmod(session, 0777))
					defer os.Chmod(session, 0700)
				case "public-quota":
					must(os.Chmod(quota, 0755))
					defer os.Chmod(quota, 0700)
				case "symlink-state":
					must(os.Remove(h.statePath()))
					must(os.Symlink(canary, h.statePath()))
				case "hardlink-state":
					must(os.Remove(h.statePath()))
					must(os.Link(canary, h.statePath()))
				case "public-state":
					must(os.Chmod(h.statePath(), 0644))
				}
				requests := 0
				var out bytes.Buffer
				d := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, Stdout: &out, Environ: func(k string) string { return h.env[k] }, HTTPClient: &http.Client{Transport: diagnosticTransport(func(*http.Request) (*http.Response, error) {
					requests++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})}}
				if RunHook(d) != 0 || requests != 0 {
					t.Fatalf("unsafe path exit or HTTP requests=%d", requests)
				}
				want := ""
				if event == hookEventStart {
					want = "{\"outcome\":\"allow\"}\n"
				}
				if event == hookEventPrompt {
					want = "{\"outcome\":\"accept\"}\n"
				}
				if out.String() != want {
					t.Fatalf("stdout=%q want=%q", out.String(), want)
				}
				b, err := os.ReadFile(canary)
				must(err)
				if string(b) != `{"baseline_revision":0}` {
					t.Fatal("outside canary changed")
				}
				entries, err := os.ReadDir(outside)
				must(err)
				if len(entries) != 1 || entries[0].Name() != "canary" {
					t.Fatalf("outside writes: %v", entries)
				}
				if attack == "slash" {
					if _, err := os.Lstat(filepath.Join(session, "nested")); !os.IsNotExist(err) {
						t.Fatal("nested unsafe session created")
					}
				}
			})
		}
	}
}

func TestHookStateWritesStayInOpenedQuota(t *testing.T) {
	h := newHookHarness(t)
	defer h.server.Close()
	d := HookDeps{SessionsDir: h.sessionsDir, Environ: func(k string) string { return h.env[k] }}
	unlock, ok := d.lockState()
	if !ok {
		t.Fatal("lock unavailable")
	}
	defer unlock()
	quota := filepath.Dir(h.statePath())
	original := quota + "-original"
	if err := os.Rename(quota, original); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, quota); err != nil {
		t.Fatal(err)
	}
	if err := d.writeState(hookState{ConsumedRevision: 12}); err != nil {
		t.Fatal(err)
	}
	if got := readHookState(filepath.Join(original, "state.json")).ConsumedRevision; got != 12 {
		t.Fatal("write missed opened root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("swapped directory received write")
	}
	st, valid := d.readState(h.statePath())
	if !valid || st.ConsumedRevision != 12 {
		t.Fatal("read followed swapped path")
	}
}
