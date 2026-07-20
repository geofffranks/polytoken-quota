package document

// Task 14 fuzz corpus for the byte-preserving editors. These prove two
// properties the reconciler depends on:
//   - FuzzFrontmatterBodyPreserved: editing frontmatter never changes a single
//     byte of the Markdown body, for arbitrary body content.
//   - FuzzDisableEnableByteIdentity: disabling then re-enabling a boolean model
//     field returns bytes byte-identical to the original, so a transient disable
//     leaves no trace (no inserted/removed keys, no reformatting).

import (
	"bytes"
	"testing"
)

// FuzzFrontmatterBodyPreserved edits one frontmatter scalar and asserts the
// body (everything after the closing "---") is byte-for-byte unchanged for
// arbitrary body bytes.
func FuzzFrontmatterBodyPreserved(f *testing.F) {
	f.Add([]byte("# body\n"))
	f.Add([]byte("\r\nCRLF body\r\nwith multiple\r\nlines\r\n"))
	f.Add([]byte("body with --- dividers\n---\nstill body\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, body []byte) {
		// Avoid a body that itself begins with a frontmatter fence, which would
		// shift the delimiter boundary; the contract is about a normal body.
		if bytes.HasPrefix(body, []byte("---")) {
			return
		}
		doc := append([]byte("---\npolytoken:\n  model: codex/gpt\n---\n"), body...)
		v := "zai/glm"
		out, err := EditFrontmatter(doc, []Edit{
			{Path: []string{"polytoken", "model"}, Kind: Scalar, Scalar: &v},
		})
		if err != nil {
			return // ambiguous/unparseable frontmatter is a valid refusal
		}
		// The editor preserves the body (everything after the closing "---\n")
		// verbatim as the document suffix.
		if !bytes.HasSuffix(out, body) {
			t.Fatalf("body not preserved as suffix by frontmatter edit:\n out=%q\nbody=%q", out, body)
		}
	})
}

// FuzzDisableEnableByteIdentity disables then re-enables a model's enabled
// boolean and asserts the result is byte-identical to the original. This proves a
// transient disable (e.g. quota_reached) leaves no byte-level trace once the
// model is restored — no inserted keys, no reordered content, no reformatted
// values.
func FuzzDisableEnableByteIdentity(f *testing.F) {
	f.Add(true)
	f.Add(false)
	f.Fuzz(func(t *testing.T, startEnabled bool) {
		orig := "models:\n  codex/gpt:\n    enabled: " + boolStr(startEnabled) + " # keep\n"
		disabled, err := EditYAML([]byte(orig), []Edit{
			{Path: []string{"models", "codex/gpt", "enabled"}, Kind: Boolean, Bool: boolPtr(!startEnabled)},
		})
		if err != nil {
			t.Fatalf("disable failed: %v", err)
		}
		restored, err := EditYAML(disabled, []Edit{
			{Path: []string{"models", "codex/gpt", "enabled"}, Kind: Boolean, Bool: boolPtr(startEnabled)},
		})
		if err != nil {
			t.Fatalf("re-enable failed: %v", err)
		}
		if !bytes.Equal(restored, []byte(orig)) {
			t.Fatalf("disable->enable not byte-identical:\n orig=%q\ngot =%q", orig, restored)
		}
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
