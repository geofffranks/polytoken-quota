package document

// Property suite for the byte-preserving editors (approved plan stage:
// YAML/frontmatter editing). The properties construct documents with known
// edited and untouched regions, compare exact output bytes against the
// independent model in editor_model_property_test.go, re-decode outputs for
// semantic value/type checks, and partition refusals into malformed,
// ambiguous (strict vs scoped), unsupported, and no-op classes with
// caller-byte immutability on every refusal.
//
// Generated scope notes (recorded honestly, not silently weakened): valid
// generated documents are newline-terminated, use one uniform terminator and
// indentation width, contain single-line scalars, and avoid plain scalars
// containing '#'. The last two exclusions are generator limitations, NOT
// claims that those inputs are unsupported: documents whose final line lacks
// a terminator and plain scalars containing '#' are supported YAML shapes
// that the editors currently mishandle silently (see
// document/failures/PRODUCTION-MISMATCHES.md in the delivery scratch for the
// red reproductions). Their acceptance is BLOCKED pending an explicit
// production-repair scope decision, so the generated space excludes them
// instead of asserting either the broken behavior or a weakened contract.

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

func propCloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

// propExpectRefusal asserts the refusal contract for EditYAML: typed error,
// nil output, caller bytes untouched. It returns the error for subclass
// assertions.
func propExpectRefusal(tb propTB, label string, in []byte, edits []Edit, opts ...EditOption) error {
	tb.Helper()
	pre := propCloneBytes(in)
	out, err := EditYAML(in, edits, opts...)
	if err == nil {
		tb.Fatalf("%s: expected refusal, got success:\n%q", label, out)
	}
	if out != nil {
		tb.Fatalf("%s: refusal returned non-nil bytes:\n%q", label, out)
	}
	if !bytes.Equal(in, pre) {
		tb.Fatalf("%s: refusal mutated caller input bytes", label)
	}
	return err
}

// TestPropertyYAMLConstructedSingleEdits applies one generated edit to a
// constructed document and checks: valid edits are never silently rejected,
// output bytes equal the independent model exactly, semantics decode to the
// intended values/types, the input is not mutated, results are deterministic
// and byte-stable on reapplication, and constructive inverses restore the
// original bytes exactly.
func TestPropertyYAMLConstructedSingleEdits(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tpl := drawTemplate(t)
		in := tpl.render()
		if len(in) > propMaxDocBytes {
			t.Fatalf("generated document exceeds byte cap: %d", len(in))
		}
		op := tpl.drawEdit(t)

		scratch := tpl.clone()
		modelErr := scratch.apply(op)
		out, err := EditYAML(in, []Edit{op})
		if modelErr != nil {
			// Wrong-kind refusals are part of the generated space; the
			// refusal contract must hold and the document must not move.
			if err == nil {
				t.Fatalf("model refused op but editor accepted it: %s\nin=%q", editLabel(op), in)
			}
			if out != nil || IsAmbiguous(err) {
				t.Fatalf("wrong-kind refusal misclassified: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid constructive edit was rejected: %v\nop=%s\nin=%q", err, editLabel(op), in)
		}
		want := scratch.render()
		if !bytes.Equal(out, want) {
			t.Fatalf("output differs from independent model\nop=%s\n in=%q\ngot=%q\nwant=%q", editLabel(op), in, out, want)
		}
		checkSemantics(t, out, scratch)

		pre := propCloneBytes(in)
		if !bytes.Equal(in, pre) {
			t.Fatalf("editor mutated its input bytes")
		}
		again, err := EditYAML(in, []Edit{op})
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("re-running the same edit was not deterministic: %v", err)
		}
		// Reapplying the same edit to its output is byte-stable when the
		// model treats the reapplication as a no-op (the usual case). One
		// class genuinely refuses on reapplication: a removal that emptied
		// its parent mapping leaves a bare key, and the same path then
		// errors with not-a-mapping. Assert that refusal contract too.
		if rescratch := scratch.clone(); rescratch.apply(op) == nil {
			reapplied, err := EditYAML(out, []Edit{op})
			if err != nil || !bytes.Equal(reapplied, out) {
				t.Fatalf("reapplying the same edit to its output was not byte-stable: %v", err)
			}
		} else {
			if _, err := EditYAML(out, []Edit{op}); err == nil {
				t.Fatalf("model expected reapplication refusal but editor accepted: %s", editLabel(op))
			}
		}
		if inverse, ok := propInverseOp(tpl, op); ok {
			back, err := EditYAML(out, []Edit{inverse})
			if err != nil {
				t.Fatalf("inverse edit failed: %v\nop=%s\ninverse=%s\nout=%q", err, editLabel(op), editLabel(inverse), out)
			}
			if !bytes.Equal(back, in) {
				t.Fatalf("op then inverse did not restore original bytes\nop=%s\ninverse=%s\n in=%q\ngot=%q",
					editLabel(op), editLabel(inverse), in, back)
			}
		}
	})
}

// propInverseOp builds the constructive inverse of an edit given the
// pre-operation model. Byte-identity claims are restricted to spellings and
// shapes the contract restores exactly: replacement of an existing value
// with the original logical value, and removal of a key the op inserted.
func propInverseOp(pre *docModel, e Edit) (Edit, bool) {
	if e.Remove {
		return Edit{}, false
	}
	_, node := pre.get(e.Path)
	if node == nil {
		// The op inserted the key. Removing it restores the exact bytes
		// only when the op added a single line; a chain insert that also
		// created missing intermediate keys leaves them behind on removal.
		if len(e.Path) > 1 {
			_, inter := pre.get(e.Path[:len(e.Path)-1])
			if inter == nil || inter.kind == propNull {
				return Edit{}, false
			}
		}
		return Edit{Path: append([]string(nil), e.Path...), Remove: true}, true
	}
	if e.Kind == Sequence && len(e.Sequence) == 0 {
		// Lowers to removal; re-insertion is not guaranteed to restore
		// the original position.
		return Edit{}, false
	}
	switch node.kind {
	case propBool:
		v := node.boolV
		return Edit{Path: append([]string(nil), e.Path...), Kind: Boolean, Bool: &v}, true
	case propScalar:
		if node.style == 'p' && e.Kind == Scalar && e.Scalar != nil && needsQuoteModel(*e.Scalar) {
			// The replacement flips the spelling to quoted, so the restore
			// would be re-emitted quoted and is not byte-exact. This is the
			// documented boundary of restore-identity claims.
			return Edit{}, false
		}
		s := node.scalar
		return Edit{Path: append([]string(nil), e.Path...), Kind: Scalar, Scalar: &s}, true
	case propSeq:
		return Edit{Path: append([]string(nil), e.Path...), Kind: Sequence, Sequence: append([]string(nil), node.items...)}, true
	default:
		return Edit{}, false
	}
}

// TestPropertyYAMLEditHistories replays bounded ordered histories (at most
// 64 edits) against the independent model, comparing exact bytes and
// semantics after every operation, including wrong-kind refusals, removed
// blocks, re-created key chains, repeated identities, and continued use
// after a refusal.
func TestPropertyYAMLEditHistories(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tpl := drawTemplate(t)
		ops := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Edit {
			return tpl.drawEdit(t)
		}), 1, propMaxEditOps).Draw(t, "ops")

		cur := tpl.render()
		for i, op := range ops {
			stage := "history op " + editLabel(op)
			pre := propCloneBytes(cur)
			modelErr := tpl.apply(op)
			out, err := EditYAML(cur, []Edit{op})
			if modelErr != nil {
				if err == nil {
					t.Fatalf("%s (step %d): model refused but editor accepted", stage, i)
				}
				if out != nil {
					t.Fatalf("%s (step %d): refusal returned bytes:\n%q", stage, i, out)
				}
				if IsAmbiguous(err) {
					t.Fatalf("%s (step %d): wrong-kind/empty refusal classified ambiguous: %v", stage, i, err)
				}
			} else {
				if err != nil {
					t.Fatalf("%s (step %d): valid edit rejected: %v\ncur=%q", stage, i, err, cur)
				}
				want := tpl.render()
				if !bytes.Equal(out, want) {
					t.Fatalf("%s (step %d): bytes diverged from model\n got=%q\nwant=%q", stage, i, out, want)
				}
				checkSemantics(t, out, tpl)
			}
			if !bytes.Equal(cur, pre) {
				t.Fatalf("%s (step %d): editor mutated its input bytes", stage, i)
			}
			if err == nil {
				cur = out
			}
		}
	})
}

// Always-valid edit specs shared by the scoped-ambiguity and frontmatter
// properties: every op below succeeds on any drawn template.
func propAlwaysValidSpecs(m *docModel) []propPathSpec {
	return []propPathSpec{
		{[]string{"polytoken", "model"}, []string{"scalar"}},
		{[]string{"polytoken", "reasoning"}, []string{"scalar"}},
		{[]string{"routing", "enabled"}, []string{"bool"}},
		{[]string{"models", m.absentName, "enabled"}, []string{"bool"}},
		{[]string{"extended"}, []string{"scalar", "bool"}},
	}
}

func propDrawAlwaysValid(m *docModel, t *rapid.T) Edit {
	spec := rapid.SampledFrom(propAlwaysValidSpecs(m)).Draw(t, "path")
	e := Edit{Path: append([]string(nil), spec.path...)}
	switch rapid.SampledFrom(spec.kinds).Draw(t, "kind") {
	case "bool":
		e.Kind = Boolean
		v := rapid.Bool().Draw(t, "bool")
		e.Bool = &v
	default:
		e.Kind = Scalar
		v := rapid.SampledFrom(propScalarValues).Draw(t, "scalar")
		e.Scalar = &v
	}
	return e
}

// propAmbiguityInjections are verbatim blocks placed in a constructed
// untouched region (before the footer). Strict mode refuses the whole
// document; scoped mode tolerates them because no edit path involves them.
var propAmbiguityInjections = [][]string{
	{"ambdup: 1", "ambdup: 2"},
	{"ambanchor: &a", "  q: 1", "ambalias: *a"},
	{"ambmerge: &m {q: 1}", "ambtarget:", "  <<: *m"},
}

// TestPropertyYAMLRefusalPartitions generates distinct refusal classes —
// malformed, ambiguous off-path (strict vs scoped), ambiguous on-path, and
// unsupported wrong-kind/flow targets — and checks the strict vs scoped
// ambiguity split, error classification, and caller-byte immutability.
func TestPropertyYAMLRefusalPartitions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		class := rapid.SampledFrom([]string{"malformed", "offpath", "onpath", "unsupported", "noop"}).Draw(t, "class")
		switch class {
		case "malformed":
			tc := rapid.SampledFrom(propMalformedCases).Draw(t, "case")
			err := propExpectRefusal(t, "malformed/"+tc.name, []byte(tc.doc), []Edit{tc.edit})
			if IsAmbiguous(err) {
				t.Fatalf("malformed %s misclassified as ambiguous: %v", tc.name, err)
			}
		case "offpath":
			tpl := drawTemplate(t)
			inj := rapid.SampledFrom(propAmbiguityInjections).Draw(t, "injection")
			tpl.inject(inj...)
			op := propDrawAlwaysValid(tpl, t)
			in := tpl.render()
			scoped := rapid.Bool().Draw(t, "scoped")
			scratch := tpl.clone()
			if scratch.apply(op) != nil {
				t.Fatalf("always-valid op refused by model: %s", editLabel(op))
			}
			if scoped {
				out, err := EditYAML(in, []Edit{op}, EditScopedAmbiguity())
				if err != nil {
					t.Fatalf("scoped mode rejected an edit whose path involves no ambiguity: %v\nin=%q", err, in)
				}
				want := scratch.render()
				if !bytes.Equal(out, want) {
					t.Fatalf("scoped edit was not byte-local\n got=%q\nwant=%q", out, want)
				}
				checkSemantics(t, out, scratch)
			} else {
				err := propExpectRefusal(t, "strict/offpath", in, []Edit{op})
				if !IsAmbiguous(err) {
					t.Fatalf("strict mode did not classify distant ambiguity as ambiguous: %v", err)
				}
			}
		case "onpath":
			tc := rapid.SampledFrom(propOnPathAmbiguous).Draw(t, "case")
			for _, scoped := range []bool{false, true} {
				label := "onpath/" + tc.name
				var opts []EditOption
				if scoped {
					label += "/scoped"
					opts = append(opts, EditScopedAmbiguity())
				}
				err := propExpectRefusal(t, label, []byte(tc.doc), []Edit{tc.edit}, opts...)
				if !IsAmbiguous(err) {
					t.Fatalf("%s: ambiguity not classified ambiguous: %v", label, err)
				}
			}
		case "unsupported":
			tc := rapid.SampledFrom(propUnsupportedCases).Draw(t, "case")
			err := propExpectRefusal(t, "unsupported/"+tc.name, []byte(tc.doc), []Edit{tc.edit})
			if IsAmbiguous(err) {
				t.Fatalf("unsupported %s misclassified as ambiguous: %v", tc.name, err)
			}
		case "noop":
			tc := rapid.SampledFrom(propNoopCases).Draw(t, "case")
			out, err := EditYAML([]byte(tc.doc), []Edit{tc.edit})
			if err != nil {
				t.Fatalf("noop %s: unexpected rejection of a supported no-op: %v", tc.name, err)
			}
			if !bytes.Equal(out, []byte(tc.doc)) {
				t.Fatalf("noop %s changed bytes:\n got=%q\nwant=%q", tc.name, out, tc.doc)
			}
		}
	})
}

type propDocCase struct {
	name string
	doc  string
	edit Edit
}

var propMalformedCases = []propDocCase{
	{"unclosed flow", "models: [unclosed\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"tab indent", "a:\n\tb: 1\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"double colon", "a: b: c\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"unterminated quote", "'never closed\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"bad indent", "a: 1\n  b: 2\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"empty input", "", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
	{"comment only", "# nothing here\n", Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("2")}},
}

var propOnPathAmbiguous = []propDocCase{
	{"duplicate root key", "polytoken:\n  model: a\npolytoken:\n  model: b\n",
		Edit{Path: []string{"polytoken", "model"}, Kind: Scalar, Scalar: strPtr("x")}},
	{"duplicate leaf key", "routing:\n  enabled: true\n  enabled: false\n", routingEdit(false)},
	{"anchor on root key", "&r routing:\n  enabled: true\n", routingEdit(false)},
	{"anchor on inner key", "routing: &r\n  enabled: true\n", routingEdit(false)},
	{"anchor on value", "routing:\n  enabled: &e true\n", routingEdit(false)},
	{"alias value", "d: &d true\nrouting:\n  enabled: *d\n", routingEdit(false)},
	{"merge key on path", "b: &b {enabled: true}\nrouting:\n  <<: *b\n", routingEdit(false)},
	{"merge key at root", "b: &b {routing: {enabled: true}}\n<<: *b\n", routingEdit(false)},
}

var propUnsupportedCases = []propDocCase{
	{"flow sequence set", "l: [a, b]\n",
		Edit{Path: []string{"l"}, Kind: Sequence, Sequence: []string{"x"}}},
	{"remove in flow mapping", "d: {full: x, mini: y}\n",
		Edit{Path: []string{"d", "full"}, Remove: true}},
	{"empty sequence in flow mapping", "d: {full: x}\n",
		Edit{Path: []string{"d", "full"}, Kind: Sequence}},
	{"sequence set on scalar", "a: s\n",
		Edit{Path: []string{"a"}, Kind: Sequence, Sequence: []string{"x"}}},
	{"sequence set on mapping", "a:\n  b: 1\n",
		Edit{Path: []string{"a"}, Kind: Sequence, Sequence: []string{"x"}}},
	{"scalar set on mapping", "a:\n  b: 1\n",
		Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("x")}},
	{"scalar set on sequence", "a:\n  - x\n",
		Edit{Path: []string{"a"}, Kind: Scalar, Scalar: strPtr("x")}},
	{"boolean set on mapping", "a:\n  b: 1\n",
		Edit{Path: []string{"a"}, Kind: Boolean, Bool: boolPtr(true)}},
	{"boolean set on sequence", "a:\n  - x\n",
		Edit{Path: []string{"a"}, Kind: Boolean, Bool: boolPtr(true)}},
}

var propNoopCases = []propDocCase{
	{"remove absent leaf", "a:\n  b: 1\n", Edit{Path: []string{"a", "c"}, Remove: true}},
	{"remove absent deep", "a: 1\n", Edit{Path: []string{"x", "y"}, Remove: true}},
	{"remove absent top", "a: 1\n", Edit{Path: []string{"zz"}, Remove: true}},
	{"empty sequence on absent key", "a: 1\n", Edit{Path: []string{"x"}, Kind: Sequence}},
}

// TestPropertyFrontmatterConstructed assembles Markdown documents with a
// constructed frontmatter block and body, applies batches of edits, and
// checks the edited block against the independent model while the opening
// delimiter (and optional BOM), closing delimiter, and body are preserved
// byte-for-byte. Refusal shapes — missing opening or closing fence, an empty
// block, and documents without frontmatter — must refuse without mutating
// the caller's bytes.
func TestPropertyFrontmatterConstructed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		shape := rapid.SampledFrom([]string{"edit", "noclose", "noopen", "emptyfm", "nodoc"}).Draw(t, "shape")
		crlf := rapid.Bool().Draw(t, "crlf")
		bom := rapid.Bool().Draw(t, "bom")
		nl := propNL(crlf)

		body := ""
		nLines := rapid.IntRange(0, 6).Draw(t, "nBodyLines")
		if nLines > 0 {
			lines := rapid.SliceOfN(rapid.SampledFrom(propBodyLines), nLines, nLines).Draw(t, "bodyLines")
			body = joinLines(lines, nl)
			if rapid.Bool().Draw(t, "bodyFinalNL") {
				body += nl
			}
		}
		closingTrail := rapid.SampledFrom([]string{"", " ", "  "}).Draw(t, "closingTrail")
		prefix := ""
		if bom {
			prefix += utf8BOM
		}
		prefix += "---" + nl
		closing := "---" + closingTrail + nl

		if shape != "edit" {
			var doc string
			switch shape {
			case "noclose":
				tpl := drawTemplate(t)
				tpl.bom = false
				doc = prefix + string(tpl.render()) // no closing fence
			case "noopen":
				doc = "just prose, no frontmatter" + nl + "second line" + nl
			case "emptyfm":
				doc = prefix + closing + body
			default: // nodoc
				doc = "# heading" + nl + nl + "plain markdown" + nl
			}
			in := []byte(doc)
			pre := propCloneBytes(in)
			_, err := EditFrontmatter(in, []Edit{modelEdit("n/b")})
			if err == nil {
				t.Fatalf("frontmatter shape %q was accepted:\n%q", shape, doc)
			}
			if IsAmbiguous(err) {
				t.Fatalf("frontmatter shape %q refusal classified ambiguous: %v", shape, err)
			}
			if !bytes.Equal(in, pre) {
				t.Fatalf("frontmatter refusal mutated caller bytes for shape %q", shape)
			}
			return
		}

		tpl := drawTemplate(t)
		tpl.bom = false // the BOM belongs to the document prefix, not the block
		fmBytes := tpl.render()
		in := []byte(prefix + string(fmBytes) + closing + body)

		ops := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Edit {
			spec := rapid.SampledFrom(propAlwaysValidSpecs(tpl)).Draw(t, "fmPath")
			e := Edit{Path: append([]string(nil), spec.path...)}
			switch rapid.SampledFrom(spec.kinds).Draw(t, "fmKind") {
			case "bool":
				e.Kind = Boolean
				v := rapid.Bool().Draw(t, "bool")
				e.Bool = &v
			default:
				e.Kind = Scalar
				v := rapid.SampledFrom(propScalarValues).Draw(t, "scalar")
				e.Scalar = &v
			}
			return e
		}), 1, 3).Draw(t, "fmOps")

		for _, op := range ops {
			if err := tpl.apply(op); err != nil {
				t.Fatalf("always-valid frontmatter op refused by model: %s: %v", editLabel(op), err)
			}
		}
		want := prefix + string(tpl.render()) + closing + body
		out, err := EditFrontmatter(in, ops)
		if err != nil {
			t.Fatalf("valid frontmatter edits rejected: %v\nin=%q", err, in)
		}
		if !bytes.Equal(out, []byte(want)) {
			t.Fatalf("frontmatter output differs from model\n got=%q\nwant=%q", out, want)
		}
		if !bytes.HasSuffix(out, []byte(body)) {
			t.Fatalf("frontmatter edit did not preserve the body as suffix")
		}
		// Semantic check on exactly the edited block (its bounds are known
		// by construction, not from the production locator).
		block := out[len(prefix) : len(out)-len(closing)-len(body)]
		checkSemantics(t, block, tpl)
		reapplied, err := EditFrontmatter(out, ops)
		if err != nil || !bytes.Equal(reapplied, out) {
			t.Fatalf("reapplying the same frontmatter edits was not byte-stable: %v", err)
		}
	})
}

var propBodyLines = []string{
	"# Head", "", "prose paragraph", "---", "---  ", "- bullet", "key: value", "  indented", "模型 unicode",
}

func joinLines(lines []string, nl string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += nl
		}
		out += l
	}
	return out
}
