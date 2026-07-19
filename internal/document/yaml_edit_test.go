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
	"testing"
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
