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

// TestProviderOwnershipGateAxisMigrates proves the additive gate-attribution
// schema (axis, threshold, engaged revision) migrates and round-trips: a
// legacy state file without the fields loads cleanly with legacy-claim
// semantics, a signal-attributed record persists and reloads unchanged, and a
// hand-edited unknown axis value degrades to the legacy axis (never
// signal-held, attribution cleared) instead of failing the load.
func TestProviderOwnershipGateAxisMigrates(t *testing.T) {
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
	// A signal-attributed claim written on this schema round trips exactly.
	signal := ProviderOwnership{
		BaselinePresent: true, BaselineValue: true, Owned: true,
		Axis: OwnershipAxisSignal, Threshold: -0.25, EngagedRevision: 9,
	}
	loaded = loaded.WithOwnership("prov-signal", signal)
	if !loaded.ProviderOwnership["prov-signal"].SignalHeld() {
		t.Fatalf("signal claim not signal-held: %+v", loaded.ProviderOwnership["prov-signal"])
	}
	if err := st.Save(loaded); err != nil {
		t.Fatal(err)
	}
	again, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.OwnershipOf("prov-signal")
	if !ok || got != signal {
		t.Fatalf("signal claim round trip = %+v (ok=%v), want %+v", got, ok, signal)
	}

	// A hand-edited unknown axis value loads as a legacy claim: still owned
	// expected-off, never signal-held, attribution fields cleared.
	handEdited := `{"Schema":5,"Revision":4,"Providers":{},"Targets":{},` +
		`"ProviderOwnership":{"prov-x":{"BaselinePresent":true,"BaselineValue":true,"Owned":true,` +
		`"Axis":"bogus","Threshold":3.5,"EngagedRevision":12}}}`
	if err := os.WriteFile(p, []byte(handEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	degraded, err := st.Load()
	if err != nil {
		t.Fatalf("unknown axis must not fail the load: %v", err)
	}
	got, ok = degraded.OwnershipOf("prov-x")
	want := ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true}
	if !ok || got != want {
		t.Fatalf("unknown axis record = %+v (ok=%v), want legacy %+v", got, ok, want)
	}
	if got.SignalHeld() {
		t.Fatalf("unknown axis record must never be signal-held")
	}
	// The degraded record persists canonically as legacy.
	if err := st.Save(degraded); err != nil {
		t.Fatal(err)
	}
	reread, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = reread.OwnershipOf("prov-x"); got != want {
		t.Fatalf("degraded record did not persist canonically: %+v", got)
	}

	// An empty axis stays legacy, and reserve/disabled axes round trip without
	// becoming signal-held.
	mixed := `{"Schema":5,"Revision":4,"Providers":{},"Targets":{},` +
		`"ProviderOwnership":{"prov-legacy":{"Owned":true},` +
		`"prov-reserve":{"Owned":true,"Axis":"reserve"},` +
		`"prov-disabled":{"Owned":true,"Axis":"disabled"}}}`
	if err := os.WriteFile(p, []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	mixedLoaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for id, wantAxis := range map[string]string{
		"prov-legacy": OwnershipAxisLegacy, "prov-reserve": OwnershipAxisReserve, "prov-disabled": OwnershipAxisDisabled,
	} {
		got, ok := mixedLoaded.OwnershipOf(id)
		if !ok || got.Axis != wantAxis {
			t.Fatalf("%s axis = %+v (ok=%v), want %q", id, got, ok, wantAxis)
		}
		if got.SignalHeld() {
			t.Fatalf("%s must never be signal-held", id)
		}
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
