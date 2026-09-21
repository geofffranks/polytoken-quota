package policy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selectionDoc returns a minimal valid desired.yaml with the given selection
// section appended.
func selectionDoc(selection string) string {
	doc := "version: 1\nproviders:\n  codex:\n    models: [codex/m]\n"
	if selection != "" {
		doc += selection
	}
	return doc
}

// wantDefaultSelection asserts the documented defaults: JEV disabled, pinned
// to DocumentedJevModel, bounded by DefaultJevTimeout.
func wantDefaultSelection(t *testing.T, got JevSelectionConfig) {
	t.Helper()
	if got.Enabled {
		t.Fatal("selection.jev should default to disabled")
	}
	if got.Model != DocumentedJevModel {
		t.Fatalf("model = %q, want documented default %q", got.Model, DocumentedJevModel)
	}
	if got.Timeout != DefaultJevTimeout {
		t.Fatalf("timeout = %s, want default %s", got.Timeout, DefaultJevTimeout)
	}
	if DefaultJevTimeout != 10*time.Second {
		t.Fatalf("documented default timeout changed: %s", DefaultJevTimeout)
	}
	if DocumentedJevModel != "jev-1.13.0" {
		t.Fatalf("documented model changed: %s", DocumentedJevModel)
	}
}

// TestLoadSelectionJevDefaults proves the optional selection section resolves
// to the documented defaults when the section, its jev mapping, any key, or
// the whole value is omitted: JEV disabled, DocumentedJevModel pin,
// DefaultJevTimeout.
func TestLoadSelectionJevDefaults(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{name: "section-omitted", doc: selectionDoc("")},
		{name: "section-null", doc: selectionDoc("selection:\n")},
		{name: "jev-omitted", doc: selectionDoc("selection: {}\n")},
		{name: "jev-null", doc: selectionDoc("selection:\n  jev:\n")},
		{name: "jev-empty", doc: selectionDoc("selection:\n  jev: {}\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load(writeTemp(t, tc.doc))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			wantDefaultSelection(t, d.Selection.Jev)
		})
	}
}

// TestLoadSelectionJevExplicit proves explicit keys resolve verbatim, alone or
// combined, including the explicit opt-out staying disabled and an alternative
// versioned model pin being honored.
func TestLoadSelectionJevExplicit(t *testing.T) {
	cases := []struct {
		name      string
		selection string
		want      JevSelectionConfig
	}{
		{
			name:      "enabled-only",
			selection: "selection:\n  jev:\n    enabled: true\n",
			want:      JevSelectionConfig{Enabled: true, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
		},
		{
			name:      "explicit-false",
			selection: "selection:\n  jev:\n    enabled: false\n",
			want:      JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
		},
		{
			name:      "timeout-only",
			selection: "selection:\n  jev:\n    timeout: 45s\n",
			want:      JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: 45 * time.Second},
		},
		{
			name:      "documented-model-explicit",
			selection: "selection:\n  jev:\n    model: jev-1.13.0\n",
			want:      JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
		},
		{
			name:      "alternative-model-pin",
			selection: "selection:\n  jev:\n    model: jev-1.14.0\n",
			want:      JevSelectionConfig{Enabled: false, Model: "jev-1.14.0", Timeout: DefaultJevTimeout},
		},
		{
			name:      "all-keys",
			selection: "selection:\n  jev:\n    enabled: true\n    model: jev-1.14.0\n    timeout: 2m\n",
			want:      JevSelectionConfig{Enabled: true, Model: "jev-1.14.0", Timeout: 2 * time.Minute},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if d.Selection.Jev != tc.want {
				t.Fatalf("selection.jev = %+v, want %+v", d.Selection.Jev, tc.want)
			}
		})
	}
}

// TestLoadSelectionJevRejectsInvalid proves the new section is strict without
// tightening legacy sections: unknown keys at either level, non-mapping
// shapes, non-boolean enablement, and invalid timeouts all reject policy load.
// Every selection error is a fixed string that names the allowed grammar and
// never echoes the document-derived value back (asserted via hidden).
func TestLoadSelectionJevRejectsInvalid(t *testing.T) {
	cases := []struct {
		name      string
		selection string
		wantErr   string
		// hidden must not appear anywhere in the error: the invalid config
		// value is not reflected into diagnostics.
		hidden string
	}{
		{
			name:      "unknown-selection-key",
			selection: "selection:\n  bogus: true\n",
			wantErr:   "selection: unknown key (want jev or laya)",
			hidden:    "bogus",
		},
		{
			name:      "unknown-jev-key",
			selection: "selection:\n  jev:\n    attempts: 3\n",
			wantErr:   "selection jev: unknown key (want enabled, model, or timeout)",
			hidden:    "attempts",
		},
		{
			name:      "selection-scalar",
			selection: "selection: 3\n",
			wantErr:   "selection must be a mapping",
		},
		{
			name:      "jev-scalar",
			selection: "selection:\n  jev: fast\n",
			wantErr:   "selection jev must be a mapping",
			hidden:    "fast",
		},
		{
			name:      "enabled-non-bool",
			selection: "selection:\n  jev:\n    enabled: fast\n",
			wantErr:   "selection jev: enabled must be a boolean",
			hidden:    "fast",
		},
		{
			// An explicit null is not a boolean: yaml would silently decode it
			// as the zero value (enabled=false), so it must reject with the
			// same fixed error as any other non-boolean value.
			name:      "enabled-null",
			selection: "selection:\n  jev:\n    enabled: null\n",
			wantErr:   "selection jev: enabled must be a boolean",
			hidden:    "null",
		},
		{
			name:      "enabled-tilde-null",
			selection: "selection:\n  jev:\n    enabled: ~\n",
			wantErr:   "selection jev: enabled must be a boolean",
		},
		{
			name:      "explicit-empty-timeout",
			selection: "selection:\n  jev:\n    timeout: \"\"\n",
			wantErr:   "selection jev timeout must be a positive duration (e.g. 10s)",
		},
		{
			name:      "zero-timeout",
			selection: "selection:\n  jev:\n    timeout: 0s\n",
			wantErr:   "selection jev timeout must be a positive duration (e.g. 10s)",
		},
		{
			name:      "negative-timeout",
			selection: "selection:\n  jev:\n    timeout: -5s\n",
			wantErr:   "selection jev timeout must be a positive duration (e.g. 10s)",
		},
		{
			name:      "unparseable-timeout",
			selection: "selection:\n  jev:\n    timeout: soon\n",
			wantErr:   "selection jev timeout must be a positive duration (e.g. 10s)",
			hidden:    "soon",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err == nil {
				t.Fatal("Load should reject the malformed selection section")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			if tc.hidden != "" && strings.Contains(err.Error(), tc.hidden) {
				t.Fatalf("error %q echoes the rejected config value %q", err, tc.hidden)
			}
		})
	}
}

// TestLoadSelectionJevRejectsDuplicateKeys proves the strict decoding rejects
// duplicated selection/jev keys instead of the yaml last-wins default, at both
// nesting levels and for every jev key.
func TestLoadSelectionJevRejectsDuplicateKeys(t *testing.T) {
	cases := []struct {
		name      string
		selection string
		wantErr   string
	}{
		{
			name:      "duplicate-jev",
			selection: "selection:\n  jev: {}\n  jev: {}\n",
			wantErr:   "selection: duplicate jev key",
		},
		{
			name:      "duplicate-enabled",
			selection: "selection:\n  jev:\n    enabled: true\n    enabled: false\n",
			wantErr:   "selection jev: duplicate enabled key",
		},
		{
			name:      "duplicate-model",
			selection: "selection:\n  jev:\n    model: jev-1.13.0\n    model: jev-1.14.0\n",
			wantErr:   "selection jev: duplicate model key",
		},
		{
			name:      "duplicate-timeout",
			selection: "selection:\n  jev:\n    timeout: 10s\n    timeout: 45s\n",
			wantErr:   "selection jev: duplicate timeout key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err == nil {
				t.Fatal("Load should reject the duplicated key")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadSelectionJevModelMustBeVersionedPin proves an explicit model pin is
// validated against the documented jev-X.Y.Z grammar — not accepted as any
// non-empty string — and that an explicit empty pin is rejected rather than
// interpreted as omitted. The documented default DocumentedJevModel itself
// satisfies the grammar.
func TestLoadSelectionJevModelMustBeVersionedPin(t *testing.T) {
	if !ValidJevPin(DocumentedJevModel) {
		t.Fatalf("documented model %q must satisfy the pin grammar", DocumentedJevModel)
	}
	cases := []struct {
		name  string
		model string
	}{
		{name: "missing-prefix", model: "1.13.0"},
		{name: "missing-component", model: "jev-1.13"},
		{name: "extra-component", model: "jev-1.13.0.1"},
		{name: "v-prefixed", model: "jev-v1.13.0"},
		{name: "non-numeric", model: "jev-x.y.z"},
		{name: "prerelease-suffix", model: "jev-1.13.0-rc1"},
		{name: "explicit-empty", model: `""`},
		{name: "explicit-blank", model: `"  "`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := selectionDoc("selection:\n  jev:\n    model: " + tc.model + "\n")
			_, err := Load(writeTemp(t, doc))
			if err == nil {
				t.Fatalf("model pin %s should be rejected", tc.model)
			}
			if !strings.Contains(err.Error(), "selection jev model must be a versioned pin like jev-1.13.0") {
				t.Fatalf("error %q is not the fixed pin-grammar message", err)
			}
		})
	}
}

// TestSelectionJevConfigRoundtrip is the named config roundtrip proof:
// marshalDesired(Load(doc)) re-loads to the identical resolved SelectionConfig
// for explicit and default policies, and a policy at the documented defaults
// serializes with no selection section at all (first-init bytes are unchanged
// by the new section).
func TestSelectionJevConfigRoundtrip(t *testing.T) {
	cases := []struct {
		name        string
		selection   string
		want        JevSelectionConfig
		wantInBytes string // non-empty: the rendered section must contain it
		wantOmitted bool   // the rendered section must be absent
	}{
		{
			name:        "defaults-omit-section",
			selection:   "",
			want:        JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
			wantOmitted: true,
		},
		{
			name:        "enabled-true",
			selection:   "selection:\n  jev:\n    enabled: true\n",
			want:        JevSelectionConfig{Enabled: true, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
			wantInBytes: "enabled: true",
		},
		{
			name:        "custom-timeout",
			selection:   "selection:\n  jev:\n    timeout: 45s\n",
			want:        JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: 45 * time.Second},
			wantInBytes: "timeout: 45s",
		},
		{
			name:        "explicit-defaults-stay-omitted",
			selection:   "selection:\n  jev:\n    model: jev-1.13.0\n    timeout: 10s\n",
			want:        JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: DefaultJevTimeout},
			wantOmitted: true,
		},
		{
			name:        "all-explicit",
			selection:   "selection:\n  jev:\n    enabled: true\n    model: jev-1.14.0\n    timeout: 1m30s\n",
			want:        JevSelectionConfig{Enabled: true, Model: "jev-1.14.0", Timeout: 90 * time.Second},
			wantInBytes: "model: jev-1.14.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err != nil {
				t.Fatalf("first Load: %v", err)
			}
			if first.Selection.Jev != tc.want {
				t.Fatalf("first load = %+v, want %+v", first.Selection.Jev, tc.want)
			}
			data, err := marshalDesired(first)
			if err != nil {
				t.Fatalf("marshalDesired: %v", err)
			}
			contains := bytes.Contains(data, []byte("selection:"))
			if tc.wantOmitted && contains {
				t.Fatalf("default selection should be omitted, got:\n%s", data)
			}
			if tc.wantInBytes != "" {
				if !contains || !bytes.Contains(data, []byte(tc.wantInBytes)) {
					t.Fatalf("rendered section missing %q, got:\n%s", tc.wantInBytes, data)
				}
			}
			path := filepath.Join(t.TempDir(), "desired.yaml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			second, err := Load(path)
			if err != nil {
				t.Fatalf("second Load: %v", err)
			}
			if second.Selection != first.Selection {
				t.Fatalf("roundtrip changed selection: %+v != %+v", second.Selection, first.Selection)
			}
		})
	}
}

// TestLegacySectionsStayLenient proves the strict selection decoding did not
// tighten legacy parsing: an unknown legacy key is still ignored by Load.
func TestLegacySectionsStayLenient(t *testing.T) {
	doc := selectionDoc("") + "legacy_extra:\n  someday: true\n"
	d, err := Load(writeTemp(t, doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Version != 1 {
		t.Fatalf("version = %d, want 1", d.Version)
	}
}

// TestLoadSelectionLayaExplicit proves the laya section resolves its keys
// and defaults the omitted timeout.
func TestLoadSelectionLayaExplicit(t *testing.T) {
	d, err := Load(writeTemp(t, selectionDoc("selection:\n  laya:\n    enabled: true\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := LayaSelectionConfig{Enabled: true, Timeout: DefaultLayaTimeout}
	if d.Selection.Laya != want {
		t.Fatalf("laya = %+v, want %+v", d.Selection.Laya, want)
	}
	// The jev backend keeps its own defaults alongside an enabled laya.
	if d.Selection.Jev.Enabled {
		t.Fatal("jev must stay disabled when only laya is configured")
	}
	d, err = Load(writeTemp(t, selectionDoc("selection:\n  laya:\n    enabled: true\n    timeout: 3s\n")))
	if err != nil {
		t.Fatalf("Load with timeout: %v", err)
	}
	if d.Selection.Laya.Timeout != 3*time.Second {
		t.Fatalf("timeout = %s, want 3s", d.Selection.Laya.Timeout)
	}
}

// TestLoadSelectionLayaRejectsInvalid proves strict decoding for the laya
// section: fixed error text, no echo of the rejected value.
func TestLoadSelectionLayaRejectsInvalid(t *testing.T) {
	cases := []struct {
		name      string
		selection string
		wantErr   string
		hidden    string
	}{
		{
			name:      "enabled-not-boolean",
			selection: "selection:\n  laya:\n    enabled: sometimes\n",
			wantErr:   "selection laya: enabled must be a boolean",
			hidden:    "sometimes",
		},
		{
			name:      "unknown-key",
			selection: "selection:\n  laya:\n    model: laya-1\n",
			wantErr:   "selection laya: unknown key (want enabled or timeout)",
			hidden:    "laya-1",
		},
		{
			name:      "unparseable-timeout",
			selection: "selection:\n  laya:\n    timeout: soon\n",
			wantErr:   "selection laya timeout must be a positive duration (e.g. 5s)",
			hidden:    "soon",
		},
		{
			name:      "zero-timeout",
			selection: "selection:\n  laya:\n    timeout: 0s\n",
			wantErr:   "selection laya timeout must be a positive duration (e.g. 5s)",
		},
		{
			name:      "empty-timeout",
			selection: "selection:\n  laya:\n    timeout: \"\"\n",
			wantErr:   "selection laya timeout must be a positive duration (e.g. 5s)",
		},
		{
			name:      "not-a-mapping",
			selection: "selection:\n  laya: true\n",
			wantErr:   "selection laya must be a mapping",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err == nil {
				t.Fatal("Load should reject the malformed laya section")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			if tc.hidden != "" && strings.Contains(err.Error(), tc.hidden) {
				t.Fatalf("error %q echoes the rejected config value %q", err, tc.hidden)
			}
		})
	}
}

// TestLoadSelectionLayaRejectsDuplicateKeys proves last-wins decoding is
// refused for every laya key.
func TestLoadSelectionLayaRejectsDuplicateKeys(t *testing.T) {
	docs := map[string]string{
		"enabled": "selection:\n  laya:\n    enabled: true\n    enabled: false\n",
		"timeout": "selection:\n  laya:\n    timeout: 1s\n    timeout: 2s\n",
	}
	for key, doc := range docs {
		t.Run(key, func(t *testing.T) {
			_, err := Load(writeTemp(t, selectionDoc(doc)))
			if err == nil {
				t.Fatalf("duplicate %s key should be rejected", key)
			}
			if !strings.Contains(err.Error(), "duplicate "+key+" key") {
				t.Fatalf("error %q does not mention the duplicate %s key", err, key)
			}
		})
	}
}

// TestLoadSelectionRejectsBothBackendsEnabled proves the mutual-exclusion
// rule: enabling jev and laya together is a load error, not a precedence
// question.
func TestLoadSelectionRejectsBothBackendsEnabled(t *testing.T) {
	doc := "selection:\n  jev:\n    enabled: true\n  laya:\n    enabled: true\n"
	_, err := Load(writeTemp(t, selectionDoc(doc)))
	if err == nil {
		t.Fatal("Load should reject jev and laya both enabled")
	}
	if !strings.Contains(err.Error(), "jev and laya cannot both be enabled") {
		t.Fatalf("error %q does not mention the mutual exclusion rule", err)
	}
}

// TestSelectionLayaConfigRoundtrip proves rendered laya settings survive a
// re-import and defaults stay omitted.
func TestSelectionLayaConfigRoundtrip(t *testing.T) {
	cases := []struct {
		name        string
		selection   string
		want        LayaSelectionConfig
		wantInBytes string
		wantOmitted bool
	}{
		{
			name:        "defaults-omit-section",
			selection:   "",
			want:        LayaSelectionConfig{Enabled: false, Timeout: DefaultLayaTimeout},
			wantOmitted: true,
		},
		{
			name:        "enabled-true",
			selection:   "selection:\n  laya:\n    enabled: true\n",
			want:        LayaSelectionConfig{Enabled: true, Timeout: DefaultLayaTimeout},
			wantInBytes: "enabled: true",
		},
		{
			name:        "custom-timeout",
			selection:   "selection:\n  laya:\n    timeout: 8s\n",
			want:        LayaSelectionConfig{Enabled: false, Timeout: 8 * time.Second},
			wantInBytes: "timeout: 8s",
		},
		{
			name:        "explicit-defaults-stay-omitted",
			selection:   "selection:\n  laya:\n    timeout: 5s\n",
			want:        LayaSelectionConfig{Enabled: false, Timeout: DefaultLayaTimeout},
			wantOmitted: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := Load(writeTemp(t, selectionDoc(tc.selection)))
			if err != nil {
				t.Fatalf("first Load: %v", err)
			}
			if first.Selection.Laya != tc.want {
				t.Fatalf("first load = %+v, want %+v", first.Selection.Laya, tc.want)
			}
			data, err := marshalDesired(first)
			if err != nil {
				t.Fatalf("marshalDesired: %v", err)
			}
			contains := bytes.Contains(data, []byte("selection:"))
			if tc.wantOmitted && contains {
				t.Fatalf("default selection should be omitted, got:\n%s", data)
			}
			if tc.wantInBytes != "" {
				if !contains || !bytes.Contains(data, []byte(tc.wantInBytes)) {
					t.Fatalf("rendered section missing %q, got:\n%s", tc.wantInBytes, data)
				}
			}
			second, err := Load(writeTemp(t, string(data)))
			if err != nil {
				t.Fatalf("reload rendered policy: %v\n%s", err, data)
			}
			if second.Selection.Laya != tc.want {
				t.Fatalf("round-trip = %+v, want %+v", second.Selection.Laya, tc.want)
			}
		})
	}
}
