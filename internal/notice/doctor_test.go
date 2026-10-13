package notice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeDoctorHooks(t *testing.T, dir string, entries []hookRegistration) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHookRegistrationStatus(t *testing.T) {
	const noticePath = "/tmp/quota-notice.json"
	for _, tc := range []struct {
		name        string
		prepare     func(*testing.T, string)
		wantManaged bool
		wantCurrent bool
		wantErr     bool
	}{
		{name: "missing file is opted out"},
		{name: "unrelated only is opted out", prepare: func(t *testing.T, dir string) {
			writeDoctorHooks(t, dir, []hookRegistration{{Name: "other", Event: "session_start", Handler: &struct {
				Bash string `json:"bash"`
			}{Bash: "exec '/other'"}}})
		}},
		{name: "after remove is opted out", prepare: func(t *testing.T, dir string) {
			entries := managedHookRegistrations("/current/bin/polytoken-quota", noticePath)
			merged, _, err := mergeHookEntries(mustMarshalEntries(t, entries), "/current/bin/polytoken-quota", noticePath, true)
			if err != nil {
				t.Fatal(err)
			}
			writeDoctorRawHooks(t, dir, merged)
		}},
		{name: "current plus unrelated", prepare: func(t *testing.T, dir string) {
			entries := managedHookRegistrations("/current/bin/polytoken-quota", noticePath)
			entries = append(entries, hookRegistration{Name: "other", Event: "session_start", Handler: &struct {
				Bash string `json:"bash"`
			}{Bash: "exec '/other'"}})
			writeDoctorHooks(t, dir, entries)
		}, wantManaged: true, wantCurrent: true},
		{name: "partial without baseline", prepare: func(t *testing.T, dir string) {
			writeDoctorHooks(t, dir, managedHookRegistrations("/current/bin/polytoken-quota", noticePath)[:2])
		}, wantManaged: true},
		{name: "stale event", prepare: func(t *testing.T, dir string) {
			entries := managedHookRegistrations("/current/bin/polytoken-quota", noticePath)
			entries[0].Event = "session_start"
			writeDoctorHooks(t, dir, entries)
		}, wantManaged: true},
		{name: "stale command", prepare: func(t *testing.T, dir string) {
			entries := managedHookRegistrations("/current/bin/polytoken-quota", noticePath)
			entries[1].Handler.Bash = "exec '/old/bin' notice-hook --notice '/tmp/old.json'"
			writeDoctorHooks(t, dir, entries)
		}, wantManaged: true},
		{name: "custom handler path", prepare: func(t *testing.T, dir string) {
			entries := managedHookRegistrations("/current/bin/polytoken-quota", noticePath)
			for i := range entries {
				entries[i].Handler.Bash = "exec '/custom/bin with spaces/polytoken-quota' notice-hook --notice " + shellQuote(noticePath)
			}
			writeDoctorHooks(t, dir, entries)
		}, wantManaged: true, wantCurrent: true},
		{name: "malformed document", prepare: func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte("Bearer CANARY-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: true},
		{name: "unreadable directory entry", prepare: func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "hooks.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			managed, current, err := HookRegistrationStatus(dir, noticePath)
			if (err != nil) != tc.wantErr || managed != tc.wantManaged || current != tc.wantCurrent {
				t.Fatalf("status = managed:%v current:%v err:%v; want managed:%v current:%v err:%v", managed, current, err, tc.wantManaged, tc.wantCurrent, tc.wantErr)
			}
		})
	}
}

func mustMarshalEntries(t *testing.T, entries []hookRegistration) []json.RawMessage {
	t.Helper()
	var raw []json.RawMessage
	for _, entry := range entries {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b)
	}
	return raw
}

func writeDoctorRawHooks(t *testing.T, dir string, entries []json.RawMessage) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
