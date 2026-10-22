package document

// Curated deterministic interaction and boundary cases complementing the
// generated properties. Each case documents a contract point with exact
// expected bytes: spelling normalization within the managed span, batch
// ordering, insertion indentation for empty parents and mixed line
// terminators, API misuse refusals, genuine absence vs an explicit boolean,
// the generator caps, and frontmatter delimiter boundaries.

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEditorContractEdges is the curated companion to the generated suite.
func TestEditorContractEdges(t *testing.T) {
	t.Run("quoted bool replaced in place", func(t *testing.T) {
		out, err := EditYAML([]byte("enabled: \"true\"\n"), []Edit{{Path: []string{"enabled"}, Kind: Boolean, Bool: boolPtr(false)}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "enabled: false\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	})

	t.Run("bool replace normalizes spelling", func(t *testing.T) {
		// The managed span is replaced with the canonical literal; a
		// non-canonical original spelling ("True") is not restored by a
		// later toggle cycle, so restore-identity claims apply only to
		// canonical baseline spellings.
		out, err := EditYAML([]byte("enabled: True\n"), []Edit{{Path: []string{"enabled"}, Kind: Boolean, Bool: boolPtr(true)}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "enabled: true\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	})

	t.Run("bool edit replaces any scalar", func(t *testing.T) {
		// Sequences and mappings are refused, but a plain string scalar is
		// replaced by the boolean literal, matching the untyped-scalar
		// allowance in the editor contract.
		out, err := EditYAML([]byte("model: sure\n"), []Edit{{Path: []string{"model"}, Kind: Boolean, Bool: boolPtr(true)}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "model: true\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	})

	t.Run("repeated insert in one batch resolves in order", func(t *testing.T) {
		out, err := EditYAML([]byte("a: 1\n"), []Edit{
			{Path: []string{"b"}, Kind: Scalar, Scalar: strPtr("x")},
			{Path: []string{"b"}, Kind: Scalar, Scalar: strPtr("y")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "a: 1\nb: y\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	})

	t.Run("insert under empty mapping is refused (fail closed)", func(t *testing.T) {
		// An existing-but-empty mapping is a null-valued key: typed setters
		// refuse deeper paths instead of guessing an insertion layout.
		// Insertion is supported when the parent path is missing entirely
		// (chain insert), exercised by the generated properties.
		in := []byte("top:\nafter: 1\n")
		pre := propCloneBytes(in)
		out, err := EditYAML(in, []Edit{{Path: []string{"top", "b"}, Kind: Scalar, Scalar: strPtr("x")}})
		if err == nil {
			t.Fatalf("expected refusal inserting under a null-valued key, got:\n%s", out)
		}
		if IsAmbiguous(err) {
			t.Fatalf("empty-mapping refusal misclassified as ambiguous: %v", err)
		}
		if !bytes.Equal(in, pre) {
			t.Fatal("refusal mutated caller bytes")
		}
	})

	t.Run("mixed terminators: inserted lines reuse the first CRLF", func(t *testing.T) {
		out, err := EditYAML([]byte("a: 1\r\nb:\n  d: 1\n"), []Edit{{Path: []string{"b", "c"}, Kind: Boolean, Bool: boolPtr(true)}})
		if err != nil {
			t.Fatal(err)
		}
		// The inserted line uses CRLF (first terminator detected); every
		// original byte, including the LF after "d: 1", is untouched.
		if got, want := string(out), "a: 1\r\nb:\n  d: 1\n  c: true\r\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	})

	t.Run("api misuse refused", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			edit Edit
		}{
			{"empty path", "a: 1\n", Edit{Kind: Scalar, Scalar: strPtr("x")}},
			{"bool without value", "a: 1\n", Edit{Path: []string{"a"}, Kind: Boolean}},
			{"scalar without value", "a: 1\n", Edit{Path: []string{"a"}, Kind: Scalar}},
			{"unknown kind", "a: 1\n", Edit{Path: []string{"a"}, Kind: Kind(99), Scalar: strPtr("x")}},
			{"remove takes precedence", "a: 1\nb: 2\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("x"), Remove: true}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out, err := EditYAML([]byte(tc.in), []Edit{tc.edit})
				if tc.name == "remove takes precedence" {
					if err != nil {
						t.Fatal(err)
					}
					if got, want := string(out), "b: 2\n"; got != want {
						t.Fatalf("got=%q want=%q", got, want)
					}
					return
				}
				if err == nil {
					t.Fatalf("expected refusal, got %q", out)
				}
				if IsAmbiguous(err) {
					t.Fatalf("api misuse misclassified as ambiguous: %v", err)
				}
			})
		}
	})

	t.Run("genuine absence vs explicit bool", func(t *testing.T) {
		in := []byte("models:\n  m:\n    reasoning: high\n")
		disabled, err := EditYAML(in, []Edit{{Path: enabledPath("m"), Kind: Boolean, Bool: boolPtr(false)}})
		if err != nil {
			t.Fatal(err)
		}
		var disDoc yaml.Node
		if err := yaml.Unmarshal(disabled, &disDoc); err != nil {
			t.Fatal(err)
		}
		en := findKeyPath(t, &disDoc, "models", "m", "enabled")
		if en == nil || en.Tag != "!!bool" || en.Value != "false" {
			t.Fatalf("expected explicit bool false after disable, got %+v", en)
		}
		restored, err := EditYAML(disabled, []Edit{{Path: enabledPath("m"), Remove: true}})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restored, in) {
			t.Fatalf("remove of inserted key not byte-exact:\n got=%q\nwant=%q", restored, in)
		}
		var backDoc yaml.Node
		if err := yaml.Unmarshal(restored, &backDoc); err != nil {
			t.Fatal(err)
		}
		if en := findKeyPath(t, &backDoc, "models", "m", "enabled"); en != nil {
			t.Fatalf("expected genuine absence after remove, got %+v", en)
		}
	})

	t.Run("generator caps respected on a large case", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("polytoken:\n  fallback_models:\n")
		for i := 0; i < propMaxSeqItems; i++ {
			b.WriteString("    - model-" + strings.Repeat("x", 8) + "/" + string(rune('a'+i%26)) + "\n")
		}
		b.WriteString("models:\n")
		for _, name := range propModelNamePool {
			b.WriteString("  " + name + ":\n    enabled: true\n    reasoning: high\n")
		}
		in := []byte(b.String())
		if len(in) > propMaxDocBytes {
			t.Fatalf("large case exceeds cap: %d", len(in))
		}
		items := make([]string, 0, propMaxSeqItems)
		for i := 0; i < propMaxSeqItems; i++ {
			items = append(items, "zai/glm-5.2-r"+string(rune('a'+i%26)))
		}
		out, err := EditYAML(in, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: items}})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte("    enabled: true")) {
			t.Fatalf("unrelated sibling content damaged:\n%s", out)
		}
		reapplied, err := EditYAML(out, []Edit{{Path: []string{"polytoken", "fallback_models"}, Kind: Sequence, Sequence: items}})
		if err != nil || !bytes.Equal(reapplied, out) {
			t.Fatalf("large sequence reapply not byte-stable: %v", err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("output does not parse: %v", err)
		}
	})

	t.Run("frontmatter boundaries", func(t *testing.T) {
		// A top-level scalar key is replaced in place by the frontmatter
		// edit; a body line of "---" after the closing fence stays body.
		in := []byte("---\nmodel: a/b\n---  \nbody\n---\nstill body\n")
		out, err := EditFrontmatter(in, []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("c/d")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "---\nmodel: c/d\n---  \nbody\n---\nstill body\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
		// An empty frontmatter block cannot be edited (nothing to parse).
		propExpectRefusal(t, "empty frontmatter block", []byte("---\n---\nbody\n"), []Edit{modelEdit("c/d")})
	})

	// Repairs for the two supported shapes the editors previously corrupted
	// (insertion onto an unterminated final line; plain scalars with
	// attached '#' content). Each case pins exact bytes.
	t.Run("insertion into an unterminated final line starts a new line", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			edit Edit
			want string
		}{
			{"bool insert", "a: 1\nb: 2",
				Edit{Path: []string{"c"}, Kind: Boolean, Bool: boolPtr(true)},
				"a: 1\nb: 2\nc: true\n"},
			{"scalar insert under existing parent", "top:\n  a: 1",
				Edit{Path: []string{"top", "b"}, Kind: Scalar, Scalar: strPtr("x")},
				"top:\n  a: 1\n  b: x\n"},
			{"missing-parent chain insert", "a: 1\nb: 2",
				Edit{Path: []string{"top", "sub"}, Kind: Boolean, Bool: boolPtr(true)},
				"a: 1\nb: 2\ntop:\n  sub: true\n"},
			{"sequence insert", "a: 1\nb: 2",
				Edit{Path: []string{"fallback_models"}, Kind: Sequence, Sequence: []string{"minime/gemma"}},
				"a: 1\nb: 2\nfallback_models:\n  - minime/gemma\n"},
			{"crlf document", "a: 1\r\nb: 2",
				Edit{Path: []string{"c"}, Kind: Boolean, Bool: boolPtr(false)},
				"a: 1\r\nb: 2\r\nc: false\r\n"},
			{"single-line document", "a: 1",
				Edit{Path: []string{"b"}, Kind: Boolean, Bool: boolPtr(true)},
				"a: 1\nb: true\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := []byte(tc.in)
				pre := propCloneBytes(in)
				out, err := EditYAML(in, []Edit{tc.edit})
				if err != nil {
					t.Fatalf("valid insertion rejected: %v", err)
				}
				if got, want := string(out), tc.want; got != want {
					t.Fatalf("insertion corrupted the document:\n got=%q\nwant=%q", got, want)
				}
				if !bytes.Equal(in, pre) {
					t.Fatal("edit mutated caller bytes")
				}
				if _, err := EditYAML(out, nil); err != nil {
					t.Fatalf("output does not parse: %v\n%q", err, out)
				}
			})
		}
		t.Run("replacement of an unterminated final value adds no terminator", func(t *testing.T) {
			out, err := EditYAML([]byte("a: 1\nb: 2"), []Edit{{Path: []string{"b"}, Kind: Boolean, Bool: boolPtr(false)}})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(out), "a: 1\nb: false"; got != want {
				t.Fatalf("got=%q want=%q", got, want)
			}
		})
		t.Run("removal of the final unterminated key keeps prior bytes", func(t *testing.T) {
			out, err := EditYAML([]byte("a: 1\nb: 2"), []Edit{{Path: []string{"b"}, Remove: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(out), "a: 1\n"; got != want {
				t.Fatalf("got=%q want=%q", got, want)
			}
		})
	})

	t.Run("plain scalar with attached '#' is replaced exactly", func(t *testing.T) {
		// yaml.v3 reads the value of `model: a#b` as the string a#b: a '#'
		// begins a comment only at the value start or after whitespace.
		out, err := EditYAML([]byte("model: a#b\n"), []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "model: new\n"; got != want {
			t.Fatalf("replacement kept stale bytes:\n got=%q\nwant=%q", got, want)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if v := findKeyPath(t, &doc, "model"); v == nil || v.Tag != "!!str" || v.Value != "new" {
			t.Fatalf("decoded %s(%v), want str(new)", v.Tag, v.Value)
		}

		// The same replacement on an unterminated final line.
		out, err = EditYAML([]byte("a: 1\nmodel: a#b"), []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "a: 1\nmodel: new"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}

		// A whitespace-delimited comment after the value stays untouched.
		out, err = EditYAML([]byte("model: a#b # keep me\n"), []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "model: new # keep me\n"; got != want {
			t.Fatalf("comment not preserved:\n got=%q\nwant=%q", got, want)
		}

		// The same span boundary applies through the frontmatter editor.
		out, err = EditFrontmatter([]byte("---\nmodel: a#b\n---\nbody\n"), []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("c/d")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "---\nmodel: c/d\n---\nbody\n"; got != want {
			t.Fatalf("frontmatter got=%q want=%q", got, want)
		}

		// A replacement value containing '#' is written single-quoted so it
		// keeps its string meaning.
		out, err = EditYAML([]byte("model: old\n"), []Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("v#2")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "model: 'v#2'\n"; got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if v := findKeyPath(t, &doc, "model"); v == nil || v.Tag != "!!str" || v.Value != "v#2" {
			t.Fatalf("decoded %s(%v), want str(v#2)", v.Tag, v.Value)
		}
	})

	t.Run("FingerprintManaged distinguishes values with attached '#'", func(t *testing.T) {
		paths := [][]string{{"model"}}
		f := func(doc string) ([32]byte, error) { return FingerprintManaged([]byte(doc), paths) }
		ab, err := f("model: a#b\n")
		if err != nil {
			t.Fatal(err)
		}
		ac, err := f("model: a#c\n")
		if err != nil {
			t.Fatal(err)
		}
		if ab == ac {
			t.Fatal("a#b and a#c collide: drift detection cannot tell the values apart")
		}
		abAgain, err := f("other: 1\nmodel: a#b\n")
		if err != nil {
			t.Fatal(err)
		}
		if abAgain != ab {
			t.Fatal("fingerprint changed when only unmanaged bytes changed")
		}
		// A whitespace-delimited comment is not part of the managed span.
		withComment, err := f("model: a#b # note\n")
		if err != nil {
			t.Fatal(err)
		}
		if withComment != ab {
			t.Fatal("whitespace-delimited comment changed the managed fingerprint")
		}
		// A value whose '#' is comment-position still hashes the value bytes
		// only (here: no value at all).
		nullValue, err := f("model: # just a comment\n")
		if err != nil {
			t.Fatal(err)
		}
		emptyish, err := f("model:\n")
		if err != nil {
			t.Fatal(err)
		}
		if nullValue != emptyish {
			t.Fatal("comment-only and bare null values fingerprint differently")
		}
	})
}

// findKeyPath walks the decoded document along path and returns the value
// node of the final key, or nil when absent.
func findKeyPath(t *testing.T, root *yaml.Node, path ...string) *yaml.Node {
	t.Helper()
	cur := root
	if cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for _, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == seg {
				next = cur.Content[j+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}
