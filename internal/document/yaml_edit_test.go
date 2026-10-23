package document

// These tests verify the byte-preserving editors. They parse exact spans via
// yaml.v3 positions and compare every untouched byte, proving that comments,
// key order, quoting, sibling keys, line endings (LF/CRLF), BOM, and Markdown
// bodies are preserved while only managed spans change. Ambiguous structures
// (duplicate keys, anchors/aliases, merge keys) must be refused.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// golden reads a testdata fixture, failing the test on any I/O error.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// boolPtr returns a pointer to b.
func boolPtr(b bool) *bool { return &b }

// modelEdit returns a polytoken.model scalar edit to v.
func modelEdit(v string) Edit {
	s := v
	return Edit{Path: []string{"polytoken", "model"}, Kind: Scalar, Scalar: &s}
}

// scalarEdit returns a single-segment scalar edit at path to v.
func scalarEdit(path, v string) Edit {
	return Edit{Path: []string{path}, Kind: Scalar, Scalar: strPtr(v)}
}

// enabledPath builds the ["models", base, "enabled"] path.
func enabledPath(base string) []string { return []string{"models", base, "enabled"} }

// TestYAMLEditPreservesUntouchedBytes edits only the boolean value span and
// compares every other byte, including the trailing comment and CRLF endings.
func TestYAMLEditPreservesUntouchedBytes(t *testing.T) {
	in := []byte("# top\r\nmodels:\r\n  codex/gpt:\r\n    enabled: true # keep\r\n    reasoning: high\r\ndefaults: {full: 'codex/gpt'}\r\n")
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("true # keep"), []byte("false # keep"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("edit changed more than the managed span:\n got=%q\nwant=%q", out, want)
	}
}

// TestYAMLEditLFGolden disables the model in the LF fixture and checks the
// comment, sibling key, flow map, and key ordering are byte-identical.
func TestYAMLEditLFGolden(t *testing.T) {
	in := golden(t, "config-lf.yaml")
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("enabled: true # keep this comment"), []byte("enabled: false # keep this comment"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("LF golden mismatch:\n got=%q\nwant=%q", out, want)
	}
}

// TestYAMLEditCRLFGolden proves CRLF line endings are preserved exactly.
func TestYAMLEditCRLFGolden(t *testing.T) {
	in := golden(t, "config-crlf.yaml")
	if bytes.Contains(in, []byte("\n")) && !bytes.Contains(in, []byte("\r\n")) {
		t.Fatal("fixture is not CRLF")
	}
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("enabled: true # keep"), []byte("enabled: false # keep"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("CRLF golden mismatch:\n got=%q\nwant=%q", out, want)
	}
	if bytes.Contains(out, []byte("\n")) && !bytes.Contains(out, []byte("\r\n")) {
		t.Fatal("output has bare LF (CRLF not preserved)")
	}
}

// TestYAMLEditBOMGolden preserves a leading UTF-8 BOM across an edit.
func TestYAMLEditBOMGolden(t *testing.T) {
	in := golden(t, "config-bom.yaml")
	if !bytes.HasPrefix(in, []byte("\xef\xbb\xbf")) {
		t.Fatal("fixture has no BOM")
	}
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("\xef\xbb\xbf")) {
		t.Fatal("BOM was lost")
	}
	if !bytes.Contains(out, []byte("enabled: false")) {
		t.Fatalf("value not edited: %q", out)
	}
}

// TestScalarEditPreservesQuoting changes a single-quoted scalar and checks the
// quotes and sibling flow-style key are preserved.
func TestScalarEditPreservesQuoting(t *testing.T) {
	in := golden(t, "config-lf.yaml")
	out, err := EditYAML(in, []Edit{{Path: []string{"defaults", "full"}, Kind: Scalar, Scalar: strPtr("codex/gpt-5.6-luna")}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("'codex/gpt'"), []byte("'codex/gpt-5.6-luna'"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("quoting not preserved:\n got=%q\nwant=%q", out, want)
	}
}

// TestScalarEditEscapedQuotes proves editing a quoted scalar whose value
// contains escaped quotes (`\"` in double-quoted, `”` in single-quoted style)
// replaces the whole value span instead of truncating at the first escaped
// quote and corrupting trailing bytes.
func TestScalarEditEscapedQuotes(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"double", "defaults:\n  full: \"foo\\\"bar\" # keep\n"},
		{"single", "defaults:\n  full: 'foo''bar' # keep\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EditYAML([]byte(tc.in), []Edit{{Path: []string{"defaults", "full"}, Kind: Scalar, Scalar: strPtr("codex/gpt")}})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out, []byte("bar")) {
				t.Fatalf("stale value bytes left behind: %q", out)
			}
			if !bytes.Contains(out, []byte("# keep")) {
				t.Fatalf("trailing comment lost: %q", out)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("output not reparseable: %v\n%q", err, out)
			}
		})
	}
}

// TestRemoveKeyInFlowMappingFailsClosed proves removing a key whose parent
// mapping is flow-style is refused: whole-line removal would erase unmanaged
// siblings sharing the line.
func TestRemoveKeyInFlowMappingFailsClosed(t *testing.T) {
	in := []byte("defaults: {full: codex/gpt, mini: zai/glm}\n")
	_, err := EditYAML(in, []Edit{{Path: []string{"defaults", "full"}, Remove: true}})
	if err == nil {
		t.Fatal("flow-mapping remove accepted; would have erased sibling mini")
	}
	// A sequence edit to empty (which lowers to Remove) must fail closed too.
	_, err = EditYAML(in, []Edit{{Path: []string{"defaults", "full"}, Kind: Sequence, Sequence: nil}})
	if err == nil {
		t.Fatal("flow-mapping empty-sequence removal accepted")
	}
}

// TestDisableRestoreIsByteIdentical proves a disable->restore cycle whose final
// state equals the original is byte-identical, AND that a transient enabled key
// absent in the baseline is REMOVED (not set to true) on restoration. The
// editor supports a Remove edit; staging (Task 9) decides whether to restore by
// removing a transient key or toggling a pre-existing one based on the baseline.
func TestDisableRestoreIsByteIdentical(t *testing.T) {
	original := golden(t, "absent-enabled.yaml") // baseline has no enabled key
	// Disable: inserts a transient "enabled: false".
	disabled, err := EditYAML(original, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !bytes.Contains(disabled, []byte("enabled: false")) {
		t.Fatalf("disable did not add enabled:false:\n%s", disabled)
	}
	// Restore: remove the transient key entirely (it was absent in the baseline).
	restored, err := EditYAML(disabled, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Remove: true}})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if bytes.Contains(restored, []byte("\n    enabled:")) {
		t.Fatalf("restore left an enabled key in place (should be removed):\n%s", restored)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("disable->restore cycle changed bytes:\n got=%q\nwant=%q", restored, original)
	}
}

// TestDisableRestorePresentKeyIsByteIdentical proves that when the baseline
// already has enabled:true, a disable->restore cycle is byte-identical by
// toggling the value (no transient-key insertion/removal needed).
func TestDisableRestorePresentKeyIsByteIdentical(t *testing.T) {
	original := golden(t, "present-enabled.yaml")
	disabled, err := EditYAML(original, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := EditYAML(disabled, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("present-key cycle changed bytes:\n got=%q\nwant=%q", restored, original)
	}
}

// TestAmbiguousYAMLRefused verifies duplicate keys, anchors/aliases, and merge
// keys are rejected with a structural error, never silently round-tripped.
func TestAmbiguousYAMLRefused(t *testing.T) {
	cases := map[string][]byte{
		"duplicate keys": []byte("a: 1\na: 2\n"),
		"anchor/alias":   []byte("a: &x 1\nb: *x\n"),
		"merge key":      []byte("x: &y {a: 1}\nb: {<<: *y}\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := EditYAML(in, []Edit{scalarEdit("a", "2")})
			if err == nil {
				t.Fatalf("accepted ambiguous input %q", in)
			}
			if !IsAmbiguous(err) {
				t.Fatalf("error not classified ambiguous for %q: %v", in, err)
			}
		})
	}
}

// TestMalformedYAMLRefused verifies unparseable YAML returns an error.
func TestMalformedYAMLRefused(t *testing.T) {
	in := []byte("models: [unclosed\n")
	_, err := EditYAML(in, []Edit{scalarEdit("models", "x")})
	if err == nil {
		t.Fatal("accepted malformed YAML")
	}
}

// TestSequenceEditPreservesBlockStyle replaces a fallback_models list and
// checks only the item spans change, preserving the key and surrounding bytes.
func TestSequenceEditPreservesBlockStyle(t *testing.T) {
	in := []byte("polytoken:\n  model: codex/gpt\n  fallback_models:\n    - minime/gemma\n    - zai/glm-5.2\n  other: keep\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: []string{"minime/gemma", "codex/gpt-5.6-sol"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte("    - zai/glm-5.2"), []byte("    - codex/gpt-5.6-sol"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("sequence edit changed more than the item:\n got=%q\nwant=%q", out, want)
	}
}

// TestSequenceEditRemovesAllItems sets an empty sequence and expects the key to
// be removed entirely.
func TestSequenceEditRemovesAllItems(t *testing.T) {
	in := []byte("polytoken:\n  model: codex/gpt\n  fallback_models:\n    - minime/gemma\n  other: keep\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: nil}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("fallback_models")) {
		t.Fatalf("key not removed:\n%s", out)
	}
	if !bytes.Contains(out, []byte("other: keep")) || !bytes.Contains(out, []byte("model: codex/gpt")) {
		t.Fatalf("sibling keys lost:\n%s", out)
	}
}

// TestSequenceEditInsertsAbsentKey adds a fallback_models list where the key
// did not exist.
func TestSequenceEditInsertsAbsentKey(t *testing.T) {
	in := []byte("polytoken:\n  model: codex/gpt\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: []string{"minime/gemma"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("fallback_models:\n    - minime/gemma")) {
		t.Fatalf("sequence not inserted correctly:\n%s", out)
	}
	// Must still parse.
	if _, err := EditYAML(out, nil); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, out)
	}
}

// TestFileModePreservation writes a fixture with a specific mode, applies an
// in-memory edit, and confirms the caller can preserve the source mode. The
// editor itself is byte-only; this test documents the Source.Mode contract.
func TestFileModePreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	in := []byte("models:\n  codex/gpt:\n    enabled: true\n")
	if err := os.WriteFile(path, in, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode()
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	// A real publisher would write `out` with `mode`. We verify the contract:
	// the mode is independent of the edited bytes.
	if err := os.WriteFile(path, out, mode.Perm()); err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode().Perm() != mode.Perm() {
		t.Fatalf("mode changed: got %v want %v", got.Mode(), mode)
	}
}

// TestFingerprintManagedStable hashes only the managed spans and is stable
// across key reordering of unmanaged keys.
func TestFingerprintManagedStable(t *testing.T) {
	a := []byte("models:\n  codex/gpt:\n    enabled: true\nother: 1\n")
	b := []byte("other: 1\nmodels:\n  codex/gpt:\n    enabled: true\n")
	paths := [][]string{enabledPath("codex/gpt")}
	fa, err := FingerprintManaged(a, paths)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := FingerprintManaged(b, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatal("fingerprint changed when only unmanaged key order changed")
	}
	// Changing the managed value changes the fingerprint.
	c := []byte("models:\n  codex/gpt:\n    enabled: false\n")
	fc, err := FingerprintManaged(c, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fc == fa {
		t.Fatal("fingerprint did not change when managed value changed")
	}
}

// TestRemoveSequenceKeepsSibling removes a multi-line block-sequence key and
// confirms the following sibling key survives intact.
func TestRemoveSequenceKeepsSibling(t *testing.T) {
	in := []byte("polytoken:\n  model: codex/gpt\n  fallback_models:\n    - minime/gemma\n    - zai/glm\n  reasoning: high\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: nil}})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("polytoken:\n  model: codex/gpt\n  reasoning: high\n")
	if !bytes.Equal(out, want) {
		t.Fatalf("got=%q\nwant=%q", out, want)
	}
}

// TestScalarEditIdempotent proves setting a scalar to its current value is a
// no-op (byte-identical), which keeps disable->restore stable.
func TestScalarEditIdempotent(t *testing.T) {
	in := []byte("models:\n  codex/gpt:\n    enabled: false\n")
	out, err := EditYAML(in, []Edit{{Path: enabledPath("codex/gpt"), Kind: Boolean, Bool: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("not idempotent:\n got=%q\nwant=%q", out, in)
	}
}

// TestBoolLikeScalarQuotedOnInsert proves a value that would be misread as a
// boolean is single-quoted when inserted, preserving its string meaning.
func TestBoolLikeScalarQuotedOnInsert(t *testing.T) {
	in := []byte("polytoken:\n  model: old\n")
	out, err := EditYAML(in, []Edit{modelEdit("true")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("model: 'true'")) {
		t.Fatalf("bool-like value not quoted:\n%s", out)
	}
	// And the result must still parse.
	if _, err := EditYAML(out, nil); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
}

// TestSequenceEditFlowStyleRefused proves a flow-style sequence
// (`fallback_models: [a, b]`) is REFUSED rather than silently corrupted. The
// editor's sequence span begins at the key's line; for a flow sequence the key
// and value share that line, so a block-style rewrite would overwrite the key
// and emit invalid YAML. The fail-closed contract returns an error and leaves
// the bytes untouched. A flow sequence is a valid, unambiguous structure, so
// the error must NOT be classified as ambiguous.
func TestSequenceEditFlowStyleRefused(t *testing.T) {
	in := golden(t, "flow-sequence.yaml")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: []string{"minime/gemma", "codex/gpt-5.6-sol"}}})
	if err == nil {
		t.Fatalf("flow-style sequence edit was accepted (silent corruption):\n%s", out)
	}
	// On error EditYAML returns nil bytes; the caller keeps its original input
	// untouched (the standard "discard the nil, keep what you had" contract).
	// The original must still parse cleanly — i.e. nothing was corrupted.
	if out != nil {
		t.Fatalf("flow-style edit returned non-nil bytes despite refusing:\n got=%q", out)
	}
	if _, perr := EditYAML(in, nil); perr != nil {
		t.Fatalf("original input corrupted by refused edit: %v", perr)
	}
	if IsAmbiguous(err) {
		t.Fatalf("flow-sequence refusal misclassified as ambiguous: %v", err)
	}
}

// TestSequenceEditBlockStyleStillWorks confirms that ordinary block-style
// sequence edits still succeed after the flow-style refusal was added, and that
// only the managed item spans change.
func TestSequenceEditBlockStyleStillWorks(t *testing.T) {
	in := []byte("polytoken:\n  model: codex/gpt\n  fallback_models:\n    - minime/gemma\n    - zai/glm-5.2\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: []string{"minime/gemma", "codex/gpt-5.6-sol"}}})
	if err != nil {
		t.Fatalf("block-style sequence edit failed: %v", err)
	}
	want := bytes.Replace(in, []byte("zai/glm-5.2"), []byte("codex/gpt-5.6-sol"), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("block-style edit changed more than the managed span:\n got=%q\nwant=%q", out, want)
	}
}

// TestIsAmbiguousSyntaxNotStructural proves a malformed-YAML *syntax* error is
// NOT reported as ambiguous. IsAmbiguous must report only genuine structural
// ambiguity (duplicate keys, anchors/aliases, merge keys); parse failures and
// wrong-kind paths are ordinary errors.
func TestIsAmbiguousSyntaxNotStructural(t *testing.T) {
	cases := map[string]struct {
		in   []byte
		edit Edit
	}{
		"unclosed flow":   {[]byte("models: [unclosed\n"), scalarEdit("a", "2")},
		"tab indent":      {[]byte("a:\n\tb: 1\n"), scalarEdit("a", "2")},
		"bad mapping":     {[]byte("a: b: c\n"), scalarEdit("a", "2")},
		"wrong-kind path": {[]byte("models: codex/gpt\n"), Edit{Path: []string{"models", "enabled"}, Kind: Scalar, Scalar: strPtr("2")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := EditYAML(tc.in, []Edit{tc.edit})
			if err == nil {
				t.Fatalf("expected an error for %q", name)
			}
			if IsAmbiguous(err) {
				t.Fatalf("non-ambiguous error misclassified as ambiguous (%s): %v", name, err)
			}
		})
	}
}

// TestIsAmbiguousDuplicateKeyIsStructural proves a genuine duplicate-key
// document IS still reported as ambiguous after the error-type split, while a
// plain missing-value path is not.
func TestIsAmbiguousDuplicateKeyIsStructural(t *testing.T) {
	_, err := EditYAML([]byte("a: 1\na: 2\n"), []Edit{scalarEdit("a", "3")})
	if err == nil {
		t.Fatal("duplicate-key document was accepted")
	}
	if !IsAmbiguous(err) {
		t.Fatalf("duplicate-key document not classified ambiguous: %v", err)
	}
}

// routingEdit returns a routing.enabled boolean edit set to v.
func routingEdit(v bool) Edit {
	return Edit{Path: []string{"routing", "enabled"}, Kind: Boolean, Bool: boolPtr(v)}
}

// TestEditScopedAmbiguityToleratesDistantStructures proves that with the
// EditScopedAmbiguity option, anchors, aliases, merge keys, and duplicate keys
// that do not involve the edited path are ignored, and the edit remains
// byte-local: every byte outside the managed span, including the anchor
// syntax itself, is preserved.
func TestEditScopedAmbiguityToleratesDistantStructures(t *testing.T) {
	cases := map[string]struct {
		in, want string
		edit     Edit
	}{
		"distant anchor and alias": {
			in:   "providers:\n  openai: &base\n    models:\n      - gpt\n  anthropic: *base\nrouting:\n  enabled: true\n",
			want: "providers:\n  openai: &base\n    models:\n      - gpt\n  anthropic: *base\nrouting:\n  enabled: false\n",
			edit: routingEdit(false),
		},
		"distant merge key": {
			in:   "defaults: &d\n  full: x\nother:\n  <<: *d\nrouting:\n  enabled: true\n",
			want: "defaults: &d\n  full: x\nother:\n  <<: *d\nrouting:\n  enabled: false\n",
			edit: routingEdit(false),
		},
		"distant duplicate keys": {
			in:   "a: 1\na: 2\nrouting:\n  enabled: true\n",
			want: "a: 1\na: 2\nrouting:\n  enabled: false\n",
			edit: routingEdit(false),
		},
		"distant alias value": {
			in:   "a: &v 1\nb: *v\nrouting:\n  enabled: true\n",
			want: "a: &v 1\nb: *v\nrouting:\n  enabled: false\n",
			edit: routingEdit(false),
		},
		"distant anchor with CRLF": {
			in:   "providers:\n  openai: &base\n    quota: 1\nrouting:\r\n  enabled: true\r\n",
			want: "providers:\n  openai: &base\n    quota: 1\nrouting:\r\n  enabled: false\r\n",
			edit: routingEdit(false),
		},
		"distant anchor scalar edit": {
			in:   "a: &v 1\nb: *v\nrouting:\n  note: 'old'\n",
			want: "a: &v 1\nb: *v\nrouting:\n  note: 'new'\n",
			edit: Edit{Path: []string{"routing", "note"}, Kind: Scalar, Scalar: strPtr("new")},
		},
		"insertion with distant anchor": {
			in:   "p: &p\n  x: 1\nrouting:\n  other: 1\n",
			want: "p: &p\n  x: 1\nrouting:\n  other: 1\n  enabled: false\n",
			edit: routingEdit(false),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := EditYAML([]byte(tc.in), []Edit{tc.edit}, EditScopedAmbiguity())
			if err != nil {
				t.Fatalf("scoped edit rejected distant ambiguity: %v", err)
			}
			if string(out) != tc.want {
				t.Fatalf("edit was not byte-local:\n got=%q\nwant=%q", out, tc.want)
			}
		})
	}
}

// TestEditScopedAmbiguityRefusesOnEditPath proves the scoped mode still refuses
// — as structurally ambiguous — any anchor, alias, merge key, or duplicate key
// that involves the edited path itself. An edit there would change or depend on
// alias-visible values, so the fail-closed contract holds on the path.
func TestEditScopedAmbiguityRefusesOnEditPath(t *testing.T) {
	cases := map[string][]byte{
		"anchor on routing value": []byte("routing: &r\n  enabled: true\n"),
		"anchor on enabled value": []byte("routing:\n  enabled: &e true\n"),
		"anchor on routing key":   []byte("&r routing:\n  enabled: true\n"),
		"alias value for enabled": []byte("d: &d true\nrouting:\n  enabled: *d\n"),
		"duplicate routing keys":  []byte("routing:\n  enabled: true\nrouting:\n  enabled: false\n"),
		"duplicate enabled keys":  []byte("routing:\n  enabled: true\n  enabled: false\n"),
		"merge key in routing":    []byte("b: &b {enabled: true}\nrouting:\n  <<: *b\n"),
		"merge key at root":       []byte("b: &b {routing: {enabled: true}}\n<<: *b\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := EditYAML(in, []Edit{routingEdit(false)}, EditScopedAmbiguity())
			if err == nil {
				t.Fatalf("path-involved ambiguity was accepted:\n%s", out)
			}
			if out != nil {
				t.Fatalf("refused edit returned non-nil bytes:\n got=%q", out)
			}
			if !IsAmbiguous(err) {
				t.Fatalf("path-involved ambiguity not classified ambiguous: %v", err)
			}
		})
	}
}

// TestEditScopedAmbiguityStrictDefaultUnchanged proves the option is opt-in:
// without it, the same distant-ambiguity documents are still refused whole.
func TestEditScopedAmbiguityStrictDefaultUnchanged(t *testing.T) {
	cases := map[string][]byte{
		"anchor and alias":       []byte("providers:\n  openai: &base\n    quota: 1\nrouting:\n  enabled: true\n"),
		"merge key":              []byte("defaults: &d\n  full: x\nother:\n  <<: *d\nrouting:\n  enabled: true\n"),
		"distant duplicate keys": []byte("a: 1\na: 2\nrouting:\n  enabled: true\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := EditYAML(in, []Edit{routingEdit(false)}); !IsAmbiguous(err) {
				t.Fatalf("strict mode no longer refuses %s: %v", name, err)
			}
		})
	}
}

// TestBoolInsertMissingModelWideIndent reproduces the bug where inserting a
// boolean under a model absent from a 4-space-indented document placed the key
// at the model-key indentation (4) instead of the model-child indentation (8).
// The old fallback (len(path)-1)*2 = 4 matched the model-key level, making
// "enabled" a sibling of the model names rather than a child.
func TestBoolInsertMissingModelWideIndent(t *testing.T) {
	in := []byte("models:\n" +
		"    codex/gpt-5.4:\n" +
		"        enabled: false\n" +
		"permissions: {}\n")
	out, err := EditYAML(in, []Edit{
		{Path: enabledPath("neuralwatt/deepseek-v4-flash"), Kind: Boolean, Bool: boolPtr(true)},
	})
	if err != nil {
		t.Fatalf("insert under missing model failed: %v", err)
	}
	// The new enabled must be at 8-space indent (child of the model), not
	// 4-space (sibling of model keys under models:). Anchor with \n so a
	// 4-space pattern does not match inside the 8-space line.
	if bytes.Contains(out, []byte("\n    enabled: true\n")) {
		t.Fatalf("enabled inserted at model-key level (4-space):\n%s", out)
	}
	if !bytes.Contains(out, []byte("\n        enabled: true\n")) {
		t.Fatalf("enabled not at model-child level (8-space):\n%s", out)
	}
	if _, err := EditYAML(out, nil); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
}

// TestBoolInsertMultipleMissingModelsNoDuplicate inserts enabled for three
// models absent from a 4-space-indented document. Before the fix, the third
// insertion failed with "duplicate key enabled" because the first two placed
// enabled at the models-mapping level (4-space), creating siblings.
func TestBoolInsertMultipleMissingModelsNoDuplicate(t *testing.T) {
	in := []byte("models:\n" +
		"    codex/gpt-5.4:\n" +
		"        enabled: false\n" +
		"permissions: {}\n")
	edits := []Edit{
		{Path: enabledPath("neuralwatt/a"), Kind: Boolean, Bool: boolPtr(true)},
		{Path: enabledPath("neuralwatt/b"), Kind: Boolean, Bool: boolPtr(true)},
		{Path: enabledPath("neuralwatt/c"), Kind: Boolean, Bool: boolPtr(false)},
	}
	out, err := EditYAML(in, edits)
	if err != nil {
		t.Fatalf("inserting enabled for multiple missing models failed: %v", err)
	}
	// No enabled key should appear at the 4-space model-key level.
	if bytes.Contains(out, []byte("\n    enabled: ")) {
		t.Fatalf("enabled key at wrong indentation level:\n%s", out)
	}
	if _, err := EditYAML(out, nil); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
}

// TestBoolInsertMissingModelNonStandardIndent verifies the indentation is
// derived from siblings rather than a fixed step. With 3-space indentation the
// old fallback (len(path)-1)*2 = 4 would not match the actual child indent (6).
func TestBoolInsertMissingModelNonStandardIndent(t *testing.T) {
	in := []byte("models:\n" +
		"   codex/gpt-5.4:\n" +
		"      enabled: false\n" +
		"permissions: {}\n")
	out, err := EditYAML(in, []Edit{
		{Path: enabledPath("neuralwatt/deepseek-v4-flash"), Kind: Boolean, Bool: boolPtr(true)},
	})
	if err != nil {
		t.Fatalf("insert under missing model failed: %v", err)
	}
	// Sibling children are at 6-space indent; the new key must match.
	if !bytes.Contains(out, []byte("      enabled: true\n")) {
		t.Fatalf("enabled not at sibling-child indentation (6-space):\n%s", out)
	}
	if _, err := EditYAML(out, nil); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
}

// ----- Block scalar regions (literal/folded) --------------------------------

// TestScalarEditBlockLiteralHashContent pins the block scalar contract: a
// literal/folded block body is a content region, so a '#' line inside it is
// scalar text, not a comment, and the value's span covers the whole region.
// Replacing or removing the value must leave no orphan body lines behind.
func TestScalarEditBlockLiteralHashContent(t *testing.T) {
	in := []byte("model: |\n  # content\n  next\nother: 1\n")
	out, err := EditYAML(in, []Edit{scalarEdit("model", "new")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "model: new\nother: 1\n"; got != want {
		t.Fatalf("block literal replace:\n got=%q\nwant=%q", got, want)
	}
	// Removing the value drops the whole region, leaving the sibling.
	out, err = EditYAML(in, []Edit{{Path: []string{"model"}, Remove: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "other: 1\n"; got != want {
		t.Fatalf("block literal remove:\n got=%q\nwant=%q", got, want)
	}
}

// TestBlockScalarRegionExactPreservation exercises replace and remove across
// the literal/folded shapes whose spans the plain-scalar scan historically
// got wrong: hash-first content, inline '#', blank lines, explicit
// indentation indicators (including one where auto-detection would pick the
// wrong indent), chomping indicators, a header comment, and nesting.
// Expectations are exact bytes; every case must also re-parse.
func TestBlockScalarRegionExactPreservation(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		path        []string
		wantSet     string // after replacing the value with "new"
		wantRemoved string // after removing the key
	}{
		{
			name:        "hash-first literal body",
			in:          "model: |\n  # content\n  next\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "inline hash stays content",
			in:          "model: |\n  a # not a comment\n  b\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "blank lines inside the body",
			in:          "model: |\n  a\n\n  # c\n  b\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name: "explicit indicator with auto-detect trap",
			// |2 fixes the block indent at 2: "    # four" (indent 4) and
			// "  x" (indent 2) are both content; auto-detection from the
			// first non-empty line would have picked 4 and cut "  x" off.
			in:          "model: |2\n    # four\n  x\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "explicit indicator under a nested key",
			in:          "top:\n  model: |2\n    # deep\n    v\n  keep: 1\n",
			path:        []string{"top", "model"},
			wantSet:     "top:\n  model: new\n  keep: 1\n",
			wantRemoved: "top:\n  keep: 1\n",
		},
		{
			name:        "folded style",
			in:          "model: >\n  # folded\n  text\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "strip chomping indicator",
			in:          "model: |-\n  # x\n  y\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			// The header comment sits on the key line physically between the
			// indicator and the body, so it is part of the replaced region
			// (like the quotes of a quoted scalar) and goes with the value.
			name:        "header comment is part of the region",
			in:          "model: | # header\n  a\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			out, err := EditYAML(in, []Edit{{Path: tc.path, Kind: Scalar, Scalar: strPtr("new")}})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), tc.wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("replaced output does not parse: %v", err)
			}
			out, err = EditYAML(in, []Edit{{Path: tc.path, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), tc.wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("removed output does not parse: %v", err)
			}
		})
	}
}

// TestBlockScalarRegionFingerprintAndInsert proves the managed fingerprint
// hashes the whole block region (not just the indicator) and that inserting
// a sibling key next to a block scalar keeps every byte of the region.
func TestBlockScalarRegionFingerprintAndInsert(t *testing.T) {
	paths := [][]string{{"model"}}
	a := []byte("model: |\n  # content\n  next\nother: 1\n")
	// Same managed bytes in a different document position with different
	// sibling content: the model fingerprint must not move.
	b := []byte("other: 2\nmodel: |\n  # content\n  next\n")
	// A changed '#' line is content, so it must change the fingerprint.
	c := []byte("model: |\n  # CHANGED\n  next\nother: 1\n")
	fpA, err := FingerprintManaged(a, paths)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := FingerprintManaged(b, paths)
	if err != nil {
		t.Fatal(err)
	}
	fpC, err := FingerprintManaged(c, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpA != fpB {
		t.Fatal("same block content fingerprinted differently across sibling changes")
	}
	if fpA == fpC {
		t.Fatal("changed '#' block content did not change the fingerprint")
	}
	// Editing the sibling leaves the block region (and its fingerprint) alone.
	out, err := EditYAML(a, []Edit{scalarEdit("other", "nine")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "model: |\n  # content\n  next\nother: nine\n"; got != want {
		t.Fatalf("sibling edit touched the block region:\n got=%q\nwant=%q", got, want)
	}
	fpOut, err := FingerprintManaged(out, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpOut != fpA {
		t.Fatal("sibling edit changed the model fingerprint")
	}
	// Inserting an absent key appends after the block scalar's sibling,
	// byte-identical for the existing lines.
	out, err = EditYAML(a, []Edit{scalarEdit("extra", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "model: |\n  # content\n  next\nother: 1\nextra: x\n"; got != want {
		t.Fatalf("insert beside a block scalar:\n got=%q\nwant=%q", got, want)
	}
}

// ----- Sequence refusal inside flow collections ------------------------------

// TestSequenceEditFlowNullRefused refuses a sequence set whose null-valued
// target sits inside a flow-style collection: rendering a block sequence
// under the key line would break the flow context and produce invalid YAML.
// The refusal must happen before any byte changes, so the input is intact
// after the failed edit.
func TestSequenceEditFlowNullRefused(t *testing.T) {
	in := []byte("d: {k: null, m: y}\n")
	out, err := EditYAML(in, []Edit{{Path: []string{"d", "k"}, Kind: Sequence, Sequence: []string{"a", "b"}}})
	if err == nil {
		t.Fatalf("flow-enclosed null accepted, produced:\n%s", out)
	}
	if out != nil {
		t.Fatalf("refused edit returned modified bytes:\n%s", out)
	}
	if !bytes.Equal(in, []byte("d: {k: null, m: y}\n")) {
		t.Fatalf("refused edit mutated the input:\n%s", in)
	}
	// The same key outside a flow collection still converts (control).
	out, err = EditYAML([]byte("d:\n  k: null\n  m: y\n"), []Edit{{Path: []string{"d", "k"}, Kind: Sequence, Sequence: []string{"a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "d:\n  k:\n    - a\n  m: y\n"; got != want {
		t.Fatalf("block control case:\n got=%q\nwant=%q", got, want)
	}
}

// ----- Sequence onto a null on its own line ----------------------------------

// TestSequenceEditUnderLaterLineNull covers the null value spelled on its
// own line below the key: the null's line must be removed BEFORE the item
// block is inserted, or the insertion shifts the removal offsets and the
// output keeps the null and loses the first item.
func TestSequenceEditUnderLaterLineNull(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain null below the key",
			in:   "k:\n  null\nother: 1\n",
			want: "k:\n  - a\n  - b\nother: 1\n",
		},
		{
			name: "tilde below the key with a comment above",
			// The comment line above the null is untouched; the null's own
			// line (indent plus token) is what disappears.
			in:   "k:\n  # note\n  ~\nother: 1\n",
			want: "k:\n  - a\n  - b\n  # note\nother: 1\n",
		},
		{
			name: "null with trailing comment on its own line",
			// The comment documents the removed null, so it goes with it.
			in:   "k:\n  null # trailing\nother: 1\n",
			want: "k:\n  - a\n  - b\nother: 1\n",
		},
		{
			name: "null on the unterminated final line",
			in:   "other: 0\nk:\n  null",
			want: "other: 0\nk:\n  - a\n  - b\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EditYAML([]byte(tc.in), []Edit{{Path: []string{"k"}, Kind: Sequence, Sequence: []string{"a", "b"}}})
			if err != nil {
				t.Fatalf("later-line null sequence set rejected: %v", err)
			}
			if got, want := string(out), tc.want; got != want {
				t.Fatalf("later-line null sequence set:\n got=%q\nwant=%q", got, want)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatalf("output does not parse: %v\n%q", err, out)
			}
		})
	}
	// CRLF variant: the removed null line takes its CRLF terminator with it.
	in := "k:\r\n  null\r\nother: 1\r\n"
	out, err := EditYAML([]byte(in), []Edit{{Path: []string{"k"}, Kind: Sequence, Sequence: []string{"a", "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "k:\r\n  - a\r\n  - b\r\nother: 1\r\n"; got != want {
		t.Fatalf("CRLF later-line null sequence set:\n got=%q\nwant=%q", got, want)
	}
}

// ----- Block scalar empty bodies (sibling-swallow regressions) ---------------

// TestBlockScalarEmptyBodyKeepsSibling pins the empty-body boundary: a block
// scalar whose body is empty (no non-blank line indented deeper than the key
// before a sibling or dedented line) ends at the key line. Setting a value or
// removing the key must leave the sibling — and everything after it — intact.
// Expectations are exact bytes; every result must also re-parse.
func TestBlockScalarEmptyBodyKeepsSibling(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		path        []string
		wantSet     string
		wantRemoved string
	}{
		{
			name:        "empty inferred body at top level",
			in:          "model: |\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "empty inferred body nested under a key",
			in:          "top:\n  model: >\n  keep: 1\noutside: 2\n",
			path:        []string{"top", "model"},
			wantSet:     "top:\n  model: new\n  keep: 1\noutside: 2\n",
			wantRemoved: "top:\n  keep: 1\noutside: 2\n",
		},
		{
			name:        "empty explicit-indicator body",
			in:          "model: |2\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			// The blank line belongs to the block (chomping applies to it),
			// so both edits consume it; the sibling still survives.
			name:        "empty body with blank line before sibling",
			in:          "model: |\n\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "empty body before dedented comment",
			in:          "model: |\n# c\nother: 1\n",
			path:        []string{"model"},
			wantSet:     "model: new\n# c\nother: 1\n",
			wantRemoved: "# c\nother: 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			out, err := EditYAML(in, []Edit{{Path: tc.path, Kind: Scalar, Scalar: strPtr("new")}})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), tc.wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("replaced output does not parse: %v", err)
			}
			out, err = EditYAML(in, []Edit{{Path: tc.path, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), tc.wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("removed output does not parse: %v", err)
			}
		})
	}
}

// TestBlockScalarEmptyBodyFingerprintAndInsert proves the empty body's
// fingerprint covers only the key line (a swallowed sibling line would move
// it) and that inserting a sibling key lands at the sibling position instead
// of inside the region a broken scanner would swallow.
func TestBlockScalarEmptyBodyFingerprintAndInsert(t *testing.T) {
	paths := [][]string{{"model"}}
	empty := []byte("model: |\nother: 1\n")
	nonEmpty := []byte("model: |\n  x\nother: 1\n")
	fpEmpty, err := FingerprintManaged(empty, paths)
	if err != nil {
		t.Fatal(err)
	}
	fpNonEmpty, err := FingerprintManaged(nonEmpty, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpEmpty == fpNonEmpty {
		t.Fatal("empty and non-empty block bodies fingerprinted the same")
	}
	// The same empty body in a different sibling context: fingerprint must
	// not move.
	moved := []byte("other: 2\nmodel: |\n")
	fpMoved, err := FingerprintManaged(moved, paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpMoved != fpEmpty {
		t.Fatal("empty body fingerprint moved across sibling changes")
	}
	// Inserting a sibling at top level appends after the existing sibling.
	out, err := EditYAML(empty, []Edit{scalarEdit("extra", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "model: |\nother: 1\nextra: x\n"; got != want {
		t.Fatalf("insert beside an empty-body block:\n got=%q\nwant=%q", got, want)
	}
	// Inserting under a nested empty-body block must insert at the nested
	// indentation, before the dedented sibling — not after it.
	nested := []byte("top:\n  model: |\noutside: 2\n")
	out, err = EditYAML(nested, []Edit{{Path: []string{"top", "extra"}, Kind: Scalar, Scalar: strPtr("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "top:\n  model: |\n  extra: x\noutside: 2\n"; got != want {
		t.Fatalf("insert under a nested empty-body block:\n got=%q\nwant=%q", got, want)
	}
}

// ----- Block scalar trailing whitespace (semantic-data regressions) ---------

// TestBlockScalarTrailingWhitespaceIsSemantic pins that terminal spaces and
// tabs on a block body line, and blank lines after the last content line,
// are scalar source bytes — not presentation: they are part of the parsed
// value (under keep chomping verbatim), they change the managed fingerprint,
// and replacing the value consumes the complete body (including those bytes)
// without leaving orphan lines or touching the following sibling.
func TestBlockScalarTrailingWhitespaceIsSemantic(t *testing.T) {
	paths := [][]string{{"model"}}
	// Terminal spaces on the last content line change the value ("a\n" vs
	// "a  \n") and must change the fingerprint.
	fpA, err := FingerprintManaged([]byte("model: |\n  a\n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	fpSpaces, err := FingerprintManaged([]byte("model: |\n  a  \n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpA == fpSpaces {
		t.Fatal("terminal body spaces did not change the fingerprint")
	}
	// A keep-chomp trailing blank line is kept in the value and must change
	// the fingerprint.
	fpB, err := FingerprintManaged([]byte("model: |+\n  a\n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	fpBlank, err := FingerprintManaged([]byte("model: |+\n  a\n\n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpB == fpBlank {
		t.Fatal("keep-chomp trailing blank line did not change the fingerprint")
	}
	// Same for CRLF documents.
	fpCRLF, err := FingerprintManaged([]byte("model: |\r\n  a\r\n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	fpCRLFSpaces, err := FingerprintManaged([]byte("model: |\r\n  a  \r\n"), paths)
	if err != nil {
		t.Fatal(err)
	}
	if fpCRLF == fpCRLFSpaces {
		t.Fatal("CRLF terminal body spaces did not change the fingerprint")
	}

	// The parsed value independently proves the bytes are scalar data: keep
	// chomping preserves the terminal spaces and every kept line's newline.
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("model: |+\n  a  \n  \n\nother: 1\n"), &root); err != nil {
		t.Fatal(err)
	}
	if v := findKeyPath(t, &root, "model"); v == nil || v.Value != "a  \n\n\n" {
		t.Fatalf("keep-chomp semantic value = %v, want %q", v, "a  \n\n\n")
	}

	// Replacement and removal must consume the complete body — terminal
	// spaces, whitespace-only lines, and kept blank lines included — with no
	// leftovers, while the sibling survives untouched.
	cases := []struct {
		name        string
		in          string
		wantSet     string
		wantRemoved string
	}{
		{
			name:        "keep body with terminal spaces and kept blanks",
			in:          "model: |+\n  a  \n  \n\nother: 1\n",
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "trailing blank line before sibling (clip)",
			in:          "model: |\n  a\n\nother: 1\n",
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "trailing blanks at terminated EOF",
			in:          "other: 1\nmodel: |+\n  a\n\n",
			wantSet:     "other: 1\nmodel: new\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "terminal spaces on unterminated final line",
			in:          "model: |\n  a  ",
			wantSet:     "model: new",
			wantRemoved: "",
		},
		{
			name:        "CRLF body with terminal spaces and kept blank",
			in:          "model: |+\r\n  a  \r\n\r\nother: 1\r\n",
			wantSet:     "model: new\r\nother: 1\r\n",
			wantRemoved: "other: 1\r\n",
		},
		{
			// The header comment sits between the indicator and the body, so
			// the whole region including it is replaced; the body's trailing
			// spaces and blank line are consumed with it.
			name:        "header comment with spaces and kept blanks",
			in:          "model: | # h\n  a  \n\nother: 1\n",
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
		{
			name:        "folded body with terminal spaces and kept blank",
			in:          "model: >+\n  a  \n\nother: 1\n",
			wantSet:     "model: new\nother: 1\n",
			wantRemoved: "other: 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			out, err := EditYAML(in, []Edit{scalarEdit("model", "new")})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), tc.wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			// The replacement value must parse back as the plain scalar we
			// set, and a sibling must keep its value.
			var root yaml.Node
			if err := yaml.Unmarshal(out, &root); err != nil {
				t.Fatalf("replaced output does not parse: %v", err)
			}
			if v := findKeyPath(t, &root, "model"); v == nil || v.Value != "new" {
				t.Fatalf("replaced model value = %v, want new", v)
			}
			if v := findKeyPath(t, &root, "other"); v != nil && v.Value != "1" {
				t.Fatalf("sibling other value = %v, want 1", v)
			}
			out, err = EditYAML(in, []Edit{{Path: []string{"model"}, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), tc.wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
			if len(out) > 0 {
				if _, err := EditYAML(out, nil); err != nil {
					t.Fatalf("removed output does not parse: %v", err)
				}
			}
		})
	}
}

// TestBlockScalarFinalTerminatorFingerprintAndEdits pins the newline
// semantics of the block region's final content line. yaml.v3 parses that
// line's break as part of the value (clip/keep: "a\n" terminated vs "a"
// unterminated at EOF), so the managed span must carry the terminator:
// terminated and unterminated documents fingerprint differently, while a
// terminator outside the region — a following sibling's own line break —
// moves nothing. Replacement re-emits the consumed separation; removal and
// insertion address whole lines, so no byte is lost or duplicated.
func TestBlockScalarFinalTerminatorFingerprintAndEdits(t *testing.T) {
	paths := [][]string{{"model"}}
	fp := func(doc string) [32]byte {
		t.Helper()
		h, err := FingerprintManaged([]byte(doc), paths)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	// Terminated vs unterminated at EOF: different parsed values (a\n vs
	// a) under clip and keep, so the fingerprints must differ — LF, CRLF,
	// folded, and multi-line alike.
	for _, tc := range []struct{ name, term, unterm, termValue string }{
		{"clip lf", "model: |\n  a\n", "model: |\n  a", "a\n"},
		{"keep lf", "model: |+\n  a\n", "model: |+\n  a", "a\n"},
		{"clip crlf", "model: |\r\n  a\r\n", "model: |\r\n  a", "a\n"},
		{"keep crlf", "model: |+\r\n  a\r\n", "model: |+\r\n  a", "a\n"},
		{"folded lf", "model: >\n  a\n", "model: >\n  a", "a\n"},
		{"multiline lf", "model: |\n  a\n  b\n", "model: |\n  a\n  b", "a\nb\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Ground the inequality in the parsed values it protects: the
			// terminated document really does carry the extra newline.
			var root yaml.Node
			if err := yaml.Unmarshal([]byte(tc.term), &root); err != nil {
				t.Fatal(err)
			}
			if v := findKeyPath(t, &root, "model"); v == nil || v.Value != tc.termValue {
				t.Fatalf("terminated value = %v, want %q", v, tc.termValue)
			}
			if err := yaml.Unmarshal([]byte(tc.unterm), &root); err != nil {
				t.Fatal(err)
			}
			if v := findKeyPath(t, &root, "model"); v == nil || v.Value == tc.termValue {
				t.Fatalf("unterminated value %v should differ from terminated %q", v, tc.termValue)
			}
			if got, want := fp(tc.term), fp(tc.unterm); got == want {
				t.Fatalf("terminated %q and unterminated %q share a fingerprint", tc.term, tc.unterm)
			}
		})
	}

	// A tail's own terminator is unmanaged: the block parses to the same
	// value with or without the document's final line break, and the
	// fingerprint must not move.
	for _, tc := range []struct{ name, term, unterm string }{
		{"clip lf", "model: |\n  a\nother: 1\n", "model: |\n  a\nother: 1"},
		{"keep lf", "model: |+\n  a\nother: 1\n", "model: |+\n  a\nother: 1"},
		{"clip crlf", "model: |\r\n  a\r\nother: 1\r\n", "model: |\r\n  a\r\nother: 1"},
		{"blank line before sibling lf", "model: |\n  a\n\nother: 1\n", "model: |\n  a\n\nother: 1"},
	} {
		t.Run("unmanaged tail terminator "+tc.name, func(t *testing.T) {
			if got, want := fp(tc.term), fp(tc.unterm); got != want {
				t.Fatalf("unmanaged tail terminator moved the fingerprint:\n %q\nvs\n %q", tc.term, tc.unterm)
			}
		})
	}

	// Empty and whitespace-only bodies have no content line to terminate.
	// The empty body's span is the bare indicator, so it must keep matching
	// across the document's final terminator (both documents parse "").
	if got, want := fp("model: |\n"), fp("model: |"); got != want {
		t.Fatal("empty body fingerprint changed across the document's final newline")
	}
	if got, want := fp("model: |\r\n"), fp("model: |"); got != want {
		t.Fatal("empty CRLF body fingerprint changed across the document's final newline")
	}

	// Exact replacement, removal, and insertion bytes. The span's consumed
	// terminator is re-emitted on replacement — the sibling keeps its own
	// separation, an unterminated final sibling stays unterminated, and a
	// terminated file stays terminated at EOF — while insertion appends
	// after the last line, terminating an unterminated one first.
	for _, tc := range []struct {
		name        string
		in          string
		wantSet     string
		wantRemoved string
		wantInsert  string
	}{
		{"lf sibling terminated", "model: |\n  a\nother: 1\n",
			"model: new\nother: 1\n", "other: 1\n",
			"model: |\n  a\nother: 1\nextra: x\n"},
		{"lf sibling unterminated", "model: |\n  a\nother: 1",
			"model: new\nother: 1", "other: 1",
			"model: |\n  a\nother: 1\nextra: x\n"},
		{"lf eof terminated", "model: |\n  a\n",
			"model: new\n", "",
			"model: |\n  a\nextra: x\n"},
		{"lf eof unterminated", "model: |\n  a",
			"model: new", "",
			"model: |\n  a\nextra: x\n"},
		{"crlf sibling terminated", "model: |\r\n  a\r\nother: 1\r\n",
			"model: new\r\nother: 1\r\n", "other: 1\r\n",
			"model: |\r\n  a\r\nother: 1\r\nextra: x\r\n"},
		{"crlf sibling unterminated", "model: |\r\n  a\r\nother: 1",
			"model: new\r\nother: 1", "other: 1",
			"model: |\r\n  a\r\nother: 1\r\nextra: x\r\n"},
		{"crlf eof terminated", "model: |\r\n  a\r\n",
			"model: new\r\n", "",
			"model: |\r\n  a\r\nextra: x\r\n"},
		{"crlf eof unterminated", "model: |\r\n  a",
			"model: new", "",
			"model: |\r\n  a\r\nextra: x\r\n"},
		{"crlf eof unterminated keep", "model: |+\r\n  a",
			"model: new", "",
			"model: |+\r\n  a\r\nextra: x\r\n"},
	} {
		t.Run("edits "+tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			out, err := EditYAML(in, []Edit{scalarEdit("model", "new")})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), tc.wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			var root yaml.Node
			if err := yaml.Unmarshal(out, &root); err != nil {
				t.Fatalf("replaced output does not parse: %v", err)
			}
			if v := findKeyPath(t, &root, "model"); v == nil || v.Value != "new" {
				t.Fatalf("replaced model value = %v, want new", v)
			}
			if v := findKeyPath(t, &root, "other"); v != nil && v.Value != "1" {
				t.Fatalf("sibling other value = %v, want 1", v)
			}
			out, err = EditYAML(in, []Edit{{Path: []string{"model"}, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), tc.wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
			if len(out) > 0 {
				if _, err := EditYAML(out, nil); err != nil {
					t.Fatalf("removed output does not parse: %v", err)
				}
			}
			out, err = EditYAML(in, []Edit{scalarEdit("extra", "x")})
			if err != nil {
				t.Fatalf("insert rejected: %v", err)
			}
			if got, want := string(out), tc.wantInsert; got != want {
				t.Fatalf("insert:\n got=%q\nwant=%q", got, want)
			}
			if _, err := EditYAML(out, nil); err != nil {
				t.Fatalf("inserted output does not parse: %v", err)
			}
		})
	}

	// A whitespace-only body's lines are blank block bytes whose terminators
	// the region already carries: replacement and removal consume them up to
	// the sibling, unchanged by the final-content-line semantics.
	for _, tc := range []struct{ name, in, wantSet, wantRemoved string }{
		{"lf", "model: |\n  \n\nother: 1\n", "model: new\nother: 1\n", "other: 1\n"},
		{"crlf", "model: |\r\n  \r\n\r\nother: 1\r\n", "model: new\r\nother: 1\r\n", "other: 1\r\n"},
	} {
		t.Run("whitespace-only body "+tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			out, err := EditYAML(in, []Edit{scalarEdit("model", "new")})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), tc.wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			out, err = EditYAML(in, []Edit{{Path: []string{"model"}, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), tc.wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
		})
	}
}

// ----- Block scalar shapes (constructed combinations) -----------------------

// TestBlockScalarShapeCombinations replaces and removes across constructed
// block scalar shapes — style (literal/folded) x chomping (clip/keep/strip) x
// indentation (inferred/explicit 2) x body (empty/plain/terminal-space/
// inner-blank) x tail (sibling/blank-then-sibling/terminated EOF) x line
// terminator (LF/EOF-unterminated) — in flat and nested wrappers.
// Expectations are built from the axes (never from the production scanner):
// exact set/remove bytes, sibling survival, and that each non-empty output
// re-parses. Newline semantics are part of the axes: a block line's
// terminator — the final content line's included — is a block byte, so with
// an EOF tail the terminated and unterminated variants fingerprint
// differently (clip/keep parse different values there), while a tail's own
// terminator is unmanaged and moves nothing. For every shape it also asserts
// that terminal body spaces and a trailing blank line change the managed
// fingerprint, and that inserting an unrelated sibling keeps every block
// byte identical.
func TestBlockScalarShapeCombinations(t *testing.T) {
	styles := []struct{ name, marker string }{
		{"literal", "|"},
		{"folded", ">"},
	}
	chompings := []struct{ name, marker string }{
		{"clip", ""},
		{"keep", "+"},
		{"strip", "-"},
	}
	indents := []struct{ name, marker string }{
		{"inferred", ""},
		{"explicit2", "2"},
	}
	bodies := []struct {
		name  string
		lines []string
	}{
		{"empty", nil},
		{"plain", []string{"a", "b"}},
		{"terminal-space", []string{"a", "b  "}},
		{"inner-blank", []string{"a", "", "b"}},
	}
	tails := []struct {
		name       string
		src        string // appended after the body
		setTail    string // what follows the replaced key line
		removeRest string // what remains after removal
	}{
		{"sibling", "other: 1\n", "other: 1\n", "other: 1\n"},
		// The blank line is a block byte (chomping applies to it), so both
		// edits consume it and the sibling directly follows the key line.
		{"blank-then-sibling", "\nother: 1\n", "other: 1\n", "other: 1\n"},
		{"eof-terminated", "", "", ""},
	}
	// build renders the document for the given axes; spacesLast appends two
	// terminal spaces to the last non-empty body line, extraBlank inserts one
	// more blank line after the body.
	build := func(header string, body struct {
		name  string
		lines []string
	}, tail struct {
		name       string
		src        string
		setTail    string
		removeRest string
	}, unterminated, spacesLast, extraBlank bool) string {
		src := header + "\n"
		for i, l := range body.lines {
			if l == "" {
				src += "\n"
				continue
			}
			if spacesLast && i == len(body.lines)-1 {
				l += "  "
			}
			src += "  " + l + "\n"
		}
		if extraBlank {
			src += "\n"
		}
		src += tail.src
		if unterminated && len(src) > 0 && src[len(src)-1] == '\n' {
			src = src[:len(src)-1]
		}
		return src
	}
	run := func(name, in, wantSet, wantRemoved string, path []string) {
		t.Run(name, func(t *testing.T) {
			raw := []byte(in)
			out, err := EditYAML(raw, []Edit{{Path: path, Kind: Scalar, Scalar: strPtr("new")}})
			if err != nil {
				t.Fatalf("replace rejected: %v", err)
			}
			if got, want := string(out), wantSet; got != want {
				t.Fatalf("replace:\n got=%q\nwant=%q", got, want)
			}
			var root yaml.Node
			if err := yaml.Unmarshal(out, &root); err != nil {
				t.Fatalf("replaced output does not parse: %v", err)
			}
			if v := findKeyPath(t, &root, path...); v == nil || v.Value != "new" {
				t.Fatalf("replaced value at %v = %v, want new", path, v)
			}
			out, err = EditYAML(raw, []Edit{{Path: path, Remove: true}})
			if err != nil {
				t.Fatalf("remove rejected: %v", err)
			}
			if got, want := string(out), wantRemoved; got != want {
				t.Fatalf("remove:\n got=%q\nwant=%q", got, want)
			}
			if len(out) > 0 {
				if _, err := EditYAML(out, nil); err != nil {
					t.Fatalf("removed output does not parse: %v", err)
				}
			}
		})
	}
	for _, style := range styles {
		for _, chomp := range chompings {
			for _, indent := range indents {
				for _, body := range bodies {
					for _, tail := range tails {
						for _, unterminated := range []bool{false, true} {
							header := "model: " + style.marker + indent.marker + chomp.marker
							src := build(header, body, tail, unterminated, false, false)
							wantSet := "model: new\n" + tail.setTail
							wantRemoved := tail.removeRest
							if unterminated {
								// The document's final line is unterminated:
								// the key line when no tail follows, otherwise
								// the sibling line. Either way nothing at EOF
								// is terminated, and a following sibling keeps
								// its unterminated final byte.
								wantSet = "model: new"
								if tail.setTail != "" {
									wantSet = "model: new\n" + strings.TrimSuffix(tail.setTail, "\n")
								}
								wantRemoved = strings.TrimSuffix(tail.removeRest, "\n")
							}
							label := style.name + "/" + chomp.name + "/" + indent.name + "/" +
								body.name + "/" + tail.name
							run(label+"/lf", src, wantSet, wantRemoved, []string{"model"})

							// Terminal body spaces and a trailing blank line
							// are block bytes: each must move the fingerprint.
							paths := [][]string{{"model"}}
							fpSrc, err := FingerprintManaged([]byte(src), paths)
							if err != nil {
								t.Fatalf("%s: fingerprint: %v", label, err)
							}
							if len(body.lines) > 0 && body.lines[len(body.lines)-1] != "" {
								spaced := build(header, body, tail, unterminated, true, false)
								fpSpaced, err := FingerprintManaged([]byte(spaced), paths)
								if err != nil {
									t.Fatalf("%s: fingerprint: %v", label, err)
								}
								if fpSpaced == fpSrc {
									t.Fatalf("%s: terminal body spaces did not change the fingerprint", label)
								}
							}
							// The final content line's terminator is a
							// block byte: with no tail, an unterminated
							// document ends the region at the line's last
							// byte while its terminated counterpart
							// carries the break (clip/keep parse
							// different values there). A tail's own
							// terminator is unmanaged and must not move
							// the fingerprint. The empty body has no
							// content line, so its span is the bare
							// indicator either way and both sides skip.
							if len(body.lines) > 0 {
								terminatedSrc := build(header, body, tail, false, false, false)
								fpTerm, err := FingerprintManaged([]byte(terminatedSrc), paths)
								if err != nil {
									t.Fatalf("%s: fingerprint: %v", label, err)
								}
								if tail.src == "" {
									if unterminated && fpTerm == fpSrc {
										t.Fatalf("%s: terminating the final content line did not change the fingerprint", label)
									}
								} else if fpTerm != fpSrc {
									t.Fatalf("%s: the tail's unmanaged terminator moved the fingerprint", label)
								}
							}
							// An extra trailing blank line is a block byte.
							// With a tail it trails the body like any kept
							// blank; with no tail it becomes the final
							// content line's own terminator — the same
							// a\n-vs-a byte the check above pins. Only the
							// empty body has no content line to terminate:
							// there the extra blank degenerates into the
							// header line's own unmanaged terminator.
							if !(unterminated && tail.src == "" && len(body.lines) == 0) {
								blanked := build(header, body, tail, unterminated, false, true)
								fpBlanked, err := FingerprintManaged([]byte(blanked), paths)
								if err != nil {
									t.Fatalf("%s: fingerprint: %v", label, err)
								}
								if fpBlanked == fpSrc {
									t.Fatalf("%s: trailing blank line did not change the fingerprint", label)
								}
							}

							// Inserting an unrelated sibling keeps every block
							// byte identical: the entry appends after the last
							// line (terminating an unterminated final line).
							wantInsert := src + "extra: x\n"
							if unterminated {
								wantInsert = src + "\nextra: x\n"
							}
							out, err := EditYAML([]byte(src), []Edit{scalarEdit("extra", "x")})
							if err != nil {
								t.Fatalf("%s: insert rejected: %v", label, err)
							}
							if got, want := string(out), wantInsert; got != want {
								t.Fatalf("%s: insert beside the block:\n got=%q\nwant=%q", label, got, want)
							}

							// Nested variant: wrap in a top-level mapping one
							// indentation level deeper.
							ind := func(s string) string {
								out := "top:\n"
								for len(s) > 0 {
									nl := -1
									for i := 0; i < len(s); i++ {
										if s[i] == '\n' {
											nl = i
											break
										}
									}
									if nl < 0 {
										out += "  " + s
										break
									}
									out += "  " + s[:nl+1]
									s = s[nl+1:]
								}
								return out
							}
							wrappedSet, wrappedRemoved := ind(wantSet), ind(wantRemoved)
							if wantSet == "" {
								wrappedSet = "top:\n"
							}
							if wantRemoved == "" {
								wrappedRemoved = "top:\n"
							}
							run(label+"/nested", ind(src), wrappedSet, wrappedRemoved, []string{"top", "model"})
						}
					}
				}
			}
		}
	}
}
