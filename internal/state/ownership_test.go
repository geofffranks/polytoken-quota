package state

// Provider-ownership metadata persistence tests: the additive sanitized schema
// (baseline absent/false/true, owned-expected-off, conflict marker), its
// save/load round trip, key sanitization, legacy-file migration, and
// copy-on-write helpers. All fixtures are synthetic.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownershipTestStore(t *testing.T) (Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	return Store{Path: p, Now: func() time.Time { return now }, RecoveredRetention: 24 * time.Hour}, p
}

// TestProviderOwnershipBaselineVariantsRoundTrip proves every baseline shape —
// key absent, explicit false, explicit true — plus owned-expected-off and the
// conflict marker survive a save/load round trip unchanged.
func TestProviderOwnershipBaselineVariantsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		want ProviderOwnership
	}{
		{"baseline-absent", ProviderOwnership{BaselinePresent: false, Owned: true}},
		{"baseline-false", ProviderOwnership{BaselinePresent: true, BaselineValue: false, Owned: true}},
		{"baseline-true", ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true}},
		{"conflict", ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Conflict: true}},
		{"baseline-only", ProviderOwnership{BaselinePresent: true, BaselineValue: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, p := ownershipTestStore(t)
			in := newState()
			in.Revision = 7
			in = in.WithOwnership("prov-a", tc.want)
			if err := st.Save(in); err != nil {
				t.Fatal(err)
			}
			out, err := st.Load()
			if err != nil {
				t.Fatal(err)
			}
			got, ok := out.OwnershipOf("prov-a")
			if !ok {
				t.Fatalf("ownership record missing after round trip")
			}
			if got != tc.want {
				t.Fatalf("round trip = %+v, want %+v", got, tc.want)
			}
			// The persisted file carries the field under its Go field name and
			// only boolean facts.
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var disk struct {
				ProviderOwnership map[string]ProviderOwnership `json:"ProviderOwnership"`
			}
			if err := json.Unmarshal(data, &disk); err != nil {
				t.Fatal(err)
			}
			if disk.ProviderOwnership["prov-a"] != tc.want {
				t.Fatalf("on-disk record = %+v, want %+v", disk.ProviderOwnership["prov-a"], tc.want)
			}
		})
	}
}

// TestProviderOwnershipLegacyStateMigratesWithoutField proves a legacy state
// file without the field loads cleanly (nil ownership), and that ownership
// written afterwards persists and the file stays on the current schema.
func TestProviderOwnershipLegacyStateMigratesWithoutField(t *testing.T) {
	st, p := ownershipTestStore(t)
	legacy := `{"Schema":4,"Revision":3,"Providers":{},"Targets":{}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProviderOwnership != nil {
		t.Fatalf("legacy state carried ownership: %+v", loaded.ProviderOwnership)
	}
	if loaded.Schema != CurrentSchema {
		t.Fatalf("legacy schema not migrated: %d", loaded.Schema)
	}
	loaded = loaded.WithOwnership("prov-b", ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true})
	if err := st.Save(loaded); err != nil {
		t.Fatal(err)
	}
	again, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.OwnershipOf("prov-b"); !ok {
		t.Fatalf("ownership lost across legacy migrate + save")
	}
}

// TestProviderOwnershipKeysSanitizedOnSave proves a hand-edited state file
// cannot carry control-character or oversized provider-ID keys into the next
// persist: keys are stripped and bounded before the bytes are written, values
// preserved.
func TestProviderOwnershipKeysSanitizedOnSave(t *testing.T) {
	st, _ := ownershipTestStore(t)
	in := newState()
	in.ProviderOwnership = map[string]ProviderOwnership{
		"prov\x00ctl": {BaselinePresent: true, Owned: true},
		strings.Repeat("x", OwnershipKeyBytes+64): {BaselinePresent: false},
	}
	if err := st.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ProviderOwnership) != 2 {
		t.Fatalf("entries = %d, want 2", len(out.ProviderOwnership))
	}
	ctl, ok := out.ProviderOwnership["provctl"]
	if !ok || !ctl.BaselinePresent || !ctl.Owned {
		t.Fatalf("control-character key not sanitized: %+v", out.ProviderOwnership)
	}
	for k := range out.ProviderOwnership {
		if len(k) > OwnershipKeyBytes {
			t.Fatalf("key length %d exceeds bound %d", len(k), OwnershipKeyBytes)
		}
		if strings.ContainsAny(k, "\x00\n\r\t") {
			t.Fatalf("key %q still carries control bytes", k)
		}
	}
}

// TestWithOwnershipCopyOnWrite proves the mutation helpers never alias or
// mutate the input state.
func TestWithOwnershipCopyOnWrite(t *testing.T) {
	base := newState()
	base = base.WithOwnership("p1", ProviderOwnership{BaselinePresent: true, BaselineValue: true})
	snapshot := base.ProviderOwnership["p1"]

	next := base.WithOwnership("p1", ProviderOwnership{Owned: true})
	if base.ProviderOwnership["p1"] != snapshot {
		t.Fatalf("input state mutated by WithOwnership")
	}
	if next.ProviderOwnership["p1"].Owned != true {
		t.Fatalf("copy did not adopt the new record")
	}

	empty := newState()
	derived := empty.WithOwnership("p2", ProviderOwnership{})
	if empty.ProviderOwnership != nil {
		t.Fatalf("nil map mutated on input state")
	}
	if _, ok := derived.OwnershipOf("p2"); !ok {
		t.Fatalf("lazy map init failed")
	}
}
