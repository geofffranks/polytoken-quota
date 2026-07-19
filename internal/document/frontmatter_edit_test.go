package document

import (
	"bytes"
	"testing"
)

// markdownBody returns the bytes after the frontmatter block (everything from
// the closing "---" line onward), i.e. the prose body that must never change.
func markdownBody(in []byte) []byte {
	_, _, bodyStart, ok := locateFrontmatter(in)
	if !ok {
		return in
	}
	return in[bodyStart:]
}

// TestFrontmatterBodyUnchanged edits polytoken.model in a CRLF Markdown
// document and proves the body bytes are identical.
func TestFrontmatterBodyUnchanged(t *testing.T) {
	in := golden(t, "agent-crlf.md")
	out, err := EditFrontmatter(in, []Edit{modelEdit("healthy/a")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(markdownBody(out), markdownBody(in)) {
		t.Fatalf("body changed:\n got=%q\nwant=%q", markdownBody(out), markdownBody(in))
	}
	// And the model value itself changed.
	if !bytes.Contains(out, []byte("model: healthy/a")) {
		t.Fatalf("model value not edited:\n%s", out)
	}
}

// TestFrontmatterLFGolden edits the frontmatter of an LF doc whose body
// contains YAML-like text and a stray "---" divider, proving none of it moves.
func TestFrontmatterLFGolden(t *testing.T) {
	in := golden(t, "agent-lf.md")
	bodyBefore := markdownBody(in)
	out, err := EditFrontmatter(in, []Edit{modelEdit("codex/gpt-5.6-luna")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(markdownBody(out), bodyBefore) {
		t.Fatalf("body changed:\n got=%q\nwant=%q", markdownBody(out), bodyBefore)
	}
	// The body's embedded YAML-like "model: minime/gemma" must be untouched.
	if bytes.Count(out, []byte("codex/gpt-5.6-luna"), ) != 1 {
		t.Fatalf("expected exactly one edited model occurrence:\n%s", out)
	}
}

// TestFrontmatterSequenceEdit edits fallback_models in frontmatter and keeps
// the body intact.
func TestFrontmatterSequenceEdit(t *testing.T) {
	in := golden(t, "agent-lf.md")
	bodyBefore := markdownBody(in)
	out, err := EditFrontmatter(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: []string{"codex/gpt-5.6-sol"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(markdownBody(out), bodyBefore) {
		t.Fatal("body changed after sequence edit")
	}
	if !bytes.Contains(out, []byte("- codex/gpt-5.6-sol")) {
		t.Fatalf("sequence not edited:\n%s", out)
	}
}

// TestFrontmatterNoBlock errors when there is no frontmatter.
func TestFrontmatterNoBlock(t *testing.T) {
	in := []byte("# just markdown\nno frontmatter here\n")
	_, err := EditFrontmatter(in, []Edit{modelEdit("a/b")})
	if err == nil {
		t.Fatal("accepted document without frontmatter")
	}
}

// FuzzEdit proves EditFrontmatter never mutates the Markdown body (when it
// succeeds) and never crashes on arbitrary/malformed input.
func FuzzEdit(f *testing.F) {
	f.Add([]byte("---\npolytoken:\n  model: a/b\n---\nbody\n"))
	f.Add([]byte("---\r\npolytoken:\r\n  model: a/b\r\n---\r\nbody\r\n"))
	f.Add([]byte("no frontmatter at all"))
	f.Add([]byte("---\nmodel: a/b\n---\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := EditFrontmatter(in, []Edit{modelEdit("c/d")})
		if err != nil {
			return // malformed/ambiguous inputs are valid refusals
		}
		if !bytes.Equal(markdownBody(in), markdownBody(out)) {
			t.Fatalf("body changed:\n in =%q\nout=%q", in, out)
		}
	})
}
