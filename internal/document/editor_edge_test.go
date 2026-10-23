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

	t.Run("value set onto a bare key writes the separated spelling", func(t *testing.T) {
		// yaml.v3 parses `k:` as a mapping entry whose value is an
		// untyped null occupying no bytes. A value set must write
		// ": v" separation — the previous behavior spliced onto the empty
		// null span and glued `k:v`, which no longer decodes as that key.
		cases := []struct {
			name  string
			in    string
			edit  Edit
			want  string
		}{
			{"scalar", "extended:\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v\n"},
			{"bool nested", "top:\n  sub:\n",
				Edit{Path: []string{"top", "sub"}, Kind: Boolean, Bool: boolPtr(true)},
				"top:\n  sub: true\n"},
			{"trailing comment keeps its spacing", "extended: # keep\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v # keep\n"},
			{"trailing spaces preserved outside the span", "extended:  \n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v  \n"},
			{"sibling untouched", "extended:\nother: 1\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v\nother: 1\n"},
			{"crlf document", "extended:\r\nother: 1\r\n",
				Edit{Path: []string{"extended"}, Kind: Boolean, Bool: boolPtr(true)},
				"extended: true\r\nother: 1\r\n"},
			{"unterminated final line stays unterminated", "extended:",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v"},
			{"quoted key", "\"extended\":\nother: 1\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"\"extended\": v\nother: 1\n"},
			{"explicit null spelling replaced in place", "extended: null\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: v\n"},
			{"quoted empty null keeps quoting style", "extended: \"\"\n",
				Edit{Path: []string{"extended"}, Kind: Scalar, Scalar: strPtr("v")},
				"extended: \"v\"\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := []byte(tc.in)
				pre := propCloneBytes(in)
				out, err := EditYAML(in, []Edit{tc.edit})
				if err != nil {
					t.Fatalf("valid bare-key set rejected: %v", err)
				}
				if got, want := string(out), tc.want; got != want {
					t.Fatalf("bare-key set corrupted the document:\n got=%q\nwant=%q", got, want)
				}
				if !bytes.Equal(in, pre) {
					t.Fatal("edit mutated caller bytes")
				}
				var doc yaml.Node
				if err := yaml.Unmarshal(out, &doc); err != nil {
					t.Fatalf("output does not parse: %v\n%q", err, out)
				}
			})
		}
		// The recorded chain witness: a chain-created parent whose last
		// child is removed becomes bare, and the next set must not glue.
		out, err := EditYAML([]byte("a: 1\n"), []Edit{
			{Path: []string{"extended", "deep"}, Kind: Boolean, Bool: boolPtr(true)},
			{Path: []string{"extended", "deep"}, Remove: true},
			{Path: []string{"extended"}, Kind: Boolean, Bool: boolPtr(false)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "a: 1\nextended: false\n"; got != want {
			t.Fatalf("chain bare-key set:\n got=%q\nwant=%q", got, want)
		}
		// Reapplying a bare-key set to its own output is byte-stable.
		again, err := EditYAML(out, []Edit{{Path: []string{"extended"}, Kind: Boolean, Bool: boolPtr(false)}})
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("bare-key set reapply not byte-stable: %v", err)
		}
	})

	t.Run("sequence set onto a null-valued key renders the block beneath it", func(t *testing.T) {
		cases := []struct {
			name  string
			in    string
			edit  Edit
			want  string
		}{
			{"bare key", "extended:\nother: 1\n",
				Edit{Path: []string{"extended"}, Kind: Sequence, Sequence: []string{"a", "b"}},
				"extended:\n  - a\n  - b\nother: 1\n"},
			{"bare key keeps its comment", "extended: # keep\n",
				Edit{Path: []string{"extended"}, Kind: Sequence, Sequence: []string{"a"}},
				"extended: # keep\n  - a\n"},
			{"explicit null token dropped", "extended: null\nother: 1\n",
				Edit{Path: []string{"extended"}, Kind: Sequence, Sequence: []string{"a"}},
				"extended:\n  - a\nother: 1\n"},
			{"nested bare key", "top:\n  sub:\n",
				Edit{Path: []string{"top", "sub"}, Kind: Sequence, Sequence: []string{"a"}},
				"top:\n  sub:\n    - a\n"},
			{"unterminated final key line is terminated", "extended:",
				Edit{Path: []string{"extended"}, Kind: Sequence, Sequence: []string{"a"}},
				"extended:\n  - a\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out, err := EditYAML([]byte(tc.in), []Edit{tc.edit})
				if err != nil {
					t.Fatalf("valid null-key sequence set rejected: %v", err)
				}
				if got, want := string(out), tc.want; got != want {
					t.Fatalf("null-key sequence set:\n got=%q\nwant=%q", got, want)
				}
				var doc yaml.Node
				if err := yaml.Unmarshal(out, &doc); err != nil {
					t.Fatalf("output does not parse: %v\n%q", err, out)
				}
			})
		}
		// An empty sequence still removes the key line (Remove semantics).
		out, err := EditYAML([]byte("extended:\nother: 1\n"), []Edit{{Path: []string{"extended"}, Kind: Sequence}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "other: 1\n"; got != want {
			t.Fatalf("empty sequence on bare key:\n got=%q\nwant=%q", got, want)
		}
		// A string-scalar and a mapping target stay refused.
		for _, tc := range []struct {
			name string
			in   string
		}{
			{"string scalar", "a: s\n"},
			{"mapping", "a:\n  b: 1\n"},
		} {
			if _, err := EditYAML([]byte(tc.in), []Edit{{Path: []string{"a"}, Kind: Sequence, Sequence: []string{"x"}}}); err == nil {
				t.Fatalf("%s: sequence set unexpectedly accepted", tc.name)
			}
		}
	})

	t.Run("plain scalar spans cover internal whitespace and folded lines", func(t *testing.T) {
		// yaml.v3 reads the whole internal-whitespace run of a plain scalar
		// (and a folded continuation, indented deeper than the key line) as
		// one value; the previous locator truncated at the first whitespace
		// byte and left stale bytes behind.
		cases := []struct {
			name  string
			in    string
			edit  Edit
			want  string
		}{
			{"internal whitespace", "model: hello world\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\n"},
			{"comment after whitespace preserved", "model: hello world # keep\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new # keep\n"},
			{"replacement carries whitespace", "model: old\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("hello world")},
				"model: hello world\n"},
			{"folded continuation", "model: hello\n  world\nother: 1\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\nother: 1\n"},
			{"blank line inside the fold", "model: hello\n\n  world\nother: 1\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\nother: 1\n"},
			{"nested fold and sibling", "top:\n  model: hello\n    world\n  next: 1\n",
				Edit{Path: []string{"top", "model"}, Kind: Scalar, Scalar: strPtr("x")},
				"top:\n  model: x\n  next: 1\n"},
			{"crlf fold", "model: hello\r\n  world\r\nother: 1\r\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\r\nother: 1\r\n"},
			{"dash line is a continuation", "model: hello\n  - item\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\n"},
			{"deeper sibling edit keeps the fold", "model: hello\n  world\nother: 1\n",
				Edit{Path: []string{"other"}, Kind: Scalar, Scalar: strPtr("next")},
				"model: hello\n  world\nother: next\n"},
			{"attached '#' with internal whitespace", "model: two w#rds\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\n"},
			{"flow indicators are block-scalar content", "model: a,b [c] {d}\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")},
				"model: new\n"},
			{"flow mapping member whitespace", "d: {k: hello world, m: y}\n",
				Edit{Path: []string{"d", "k"}, Kind: Scalar, Scalar: strPtr("new")},
				"d: {k: new, m: y}\n"},
			{"flow mapping member folded across lines", "d: {k: hello\n  world}\n",
				Edit{Path: []string{"d", "k"}, Kind: Scalar, Scalar: strPtr("new")},
				"d: {k: new}\n"},
			{"flow comment line survives", "d: {\n  k: hello world # c\n  ,\n  m: y}\n",
				Edit{Path: []string{"d", "k"}, Kind: Scalar, Scalar: strPtr("new")},
				"d: {\n  k: new # c\n  ,\n  m: y}\n"},
			{"literal block indicator and content replaced", "model: |\n  hello\nother: 1\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("x")},
				"model: x\nother: 1\n"},
			{"folded block indicator and content replaced", "model: >\n  hello\nother: 1\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("x")},
				"model: x\nother: 1\n"},
			{"chomped literal block replaced", "model: |-\n  hello\nother: 1\n",
				Edit{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("x")},
				"model: x\nother: 1\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := []byte(tc.in)
				pre := propCloneBytes(in)
				out, err := EditYAML(in, []Edit{tc.edit})
				if err != nil {
					t.Fatalf("valid whitespace-span edit rejected: %v", err)
				}
				if got, want := string(out), tc.want; got != want {
					t.Fatalf("span did not cover the whole value:\n in=%q\n got=%q\nwant=%q", in, got, want)
				}
				if !bytes.Equal(in, pre) {
					t.Fatal("edit mutated caller bytes")
				}
				var doc yaml.Node
				if err := yaml.Unmarshal(out, &doc); err != nil {
					t.Fatalf("output does not parse: %v\n%q", err, out)
				}
			})
		}
		// Trailing whitespace on the value's final line sits outside the
		// span and is preserved, exactly as for a single-line value.
		out, err := EditYAML([]byte("model: hello  \n  world  \nother: 1\n"),
			[]Edit{{Path: []string{"model"}, Kind: Scalar, Scalar: strPtr("new")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "model: new  \nother: 1\n"; got != want {
			t.Fatalf("final-line trailing whitespace not preserved:\n got=%q\nwant=%q", got, want)
		}
		// Removal and insertion-after account for the value's whole region:
		// the continuation lines move with the key.
		out, err = EditYAML([]byte("model: hello\n  world\nother: 1\n"),
			[]Edit{{Path: []string{"model"}, Remove: true}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "other: 1\n"; got != want {
			t.Fatalf("multiline value removal left stale bytes:\n got=%q\nwant=%q", got, want)
		}
		out, err = EditYAML([]byte("polytoken:\n  model: hello\n    world\n"),
			[]Edit{{Path: []string{"polytoken", "extra"}, Kind: Scalar, Scalar: strPtr("v")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(out), "polytoken:\n  model: hello\n    world\n  extra: v\n"; got != want {
			t.Fatalf("insertion landed inside the folded value:\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("FingerprintManaged distinguishes whitespace-bearing values", func(t *testing.T) {
		paths := [][]string{{"model"}}
		f := func(doc string) [32]byte {
			h, err := FingerprintManaged([]byte(doc), paths)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		if f("model: hello world\n") == f("model: hello\n") {
			t.Fatal("whitespace value collided with its first-token truncation")
		}
		if f("model: hello world\n") == f("model: world hello\n") {
			t.Fatal("different whitespace values collided")
		}
		if f("model: a,b\n") == f("model: a\n") {
			t.Fatal("comma-bearing value collided with its truncation")
		}
		if f("model: hello\n  world\n") != f("model: hello\n  world\nother: 1\n") {
			t.Fatal("folded value fingerprint changed when only unmanaged bytes changed")
		}
		if f("model: hello\n  world\n") == f("model: hello\n") {
			t.Fatal("folded value collided with its first line")
		}
		bare, bareComment := f("extended:\n"), f("extended: # c\n")
		if bare != bareComment {
			t.Fatal("bare null fingerprint changed under a comment")
		}
		extPaths := [][]string{{"extended"}}
		set, err := FingerprintManaged([]byte("extended: v\n"), extPaths)
		if err != nil {
			t.Fatal(err)
		}
		bareExt, err := FingerprintManaged([]byte("extended:\n"), extPaths)
		if err != nil {
			t.Fatal(err)
		}
		if set == bareExt {
			t.Fatal("bare null and set value share a fingerprint")
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
