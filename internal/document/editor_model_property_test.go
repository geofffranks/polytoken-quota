package document

// Independent document model and input generators for the editor property
// suite (approved plan stage: YAML/frontmatter editing). The model mirrors
// the documented editor contract, not the implementation: byte-preserving
// span replacement that keeps the existing quoting style, insertions appended
// as the last child of their parent mapping at the parent's existing child
// indentation (document-root insertions land after the last document line),
// block-sequence rewrites at the existing item indentation, removal of the
// key line through the end of its value, and a 2-space-per-level fallback
// when rebuilding a missing key chain under an empty parent. Expected bytes
// come from construction, never from the production span locator.
//
// Deliberate generated scope (matches the reconciler's real file shapes):
// documents use one uniform terminator (LF or CRLF, optional BOM), one
// indentation width per document, and block sequences of single-line items.
// The generated space DOES include documents whose final line lacks a
// terminator (finalNL), plain scalars whose content carries an attached '#'
// (style 'h', e.g. a#b), multi-token plain scalars with internal whitespace
// (style 'p'), and a folded two-line plain scalar (style 'm'): yaml.v3 reads
// all of those as supported plain shapes, and the model mirrors the editor's
// documented behavior for them — an insertion at an unterminated final line
// first terminates that line, an attached-'#'/whitespace value span is
// replaced whole, and a folded scalar's replacement is written inline. Values
// containing '#' are written single-quoted by both sides. Scalar, boolean,
// and nonempty sequence sets onto an existing bare (null-valued) key are in
// the generated space: a bare key gains the documented ": " separation, and a
// sequence set renders the block beneath the key line.

import (
	"bytes"
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"
)

// Hard per-case generator bounds from the approved plan.
const (
	propMaxEditOps  = 64       // history/edit operations per case
	propMaxDocBytes = 32 << 10 // document bytes per case
	propMaxSeqItems = 32       // sequence entries per case
	propMaxModels   = 8        // identity pool size
)

type propKind uint8

const (
	propScalar propKind = iota
	propBool
	propSeq
	propMap
	propNull
)

// propNode is one key in the model tree. comment is the verbatim key-line
// suffix (including its leading space); style records the existing scalar
// spelling so replacement can preserve it ('p' plain, 's' single-quoted,
// 'd' double-quoted, 'h' plain with attached '#' content — a shape only
// plain style can carry, rendered verbatim, and 'm' a folded two-line plain
// scalar rendered across a continuation line indented two past the key).
type propNode struct {
	key      string
	kind     propKind
	indent   int
	comment  string
	style    byte
	scalar   string
	boolV    bool
	items    []string
	seqInd   int // indent of the "- " item lines for propSeq
	children []*propNode
}

// docModel is the whole constructed document: optional BOM, optional header
// comment, root mapping keys, optional footer comment, and keys inserted at
// document-root level by edits (which the editor appends after the last
// document line, i.e. after the footer).
type docModel struct {
	bom, crlf bool
	width     int
	header    string
	footer    string
	injected  string // verbatim block (ambiguity injection) rendered before the footer
	root      []*propNode
	tail      []*propNode

	// finalNL records whether the rendered document ends with a line
	// terminator. When false, the final line is valid YAML without one; an
	// editor insertion at that line must first terminate it.
	finalNL bool

	// Pools used by the edit generator.
	chosenNames []string
	absentName  string
}

var errPropEmptyDoc = errors.New("model: document has no YAML content (comment-only)")

func propNL(crlf bool) string {
	if crlf {
		return "\r\n"
	}
	return "\n"
}

func propSpaces(n int) string { return strings.Repeat(" ", n) }

// needsQuoteModel mirrors the documented scalar-quoting contract: a value
// that plain style would misread as another YAML type, or that contains
// characters breaking an inline plain scalar, must be quoted.
func needsQuoteModel(v string) bool {
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", ".nan", ".inf", "-.inf", "+.inf":
		return true
	}
	switch v[0] {
	case '!', '&', '*', '-', '?', ':', '>', '|', '%', '@', '`', '"', '\'', '#', ',', '[', ']', '{', '}':
		return true
	}
	if propNumericLike(v) {
		return true
	}
	if strings.ContainsAny(v, "\n#:{}[],&*?|>%@`") {
		return true
	}
	return v[0] == ' ' || v[len(v)-1] == ' '
}

// propNumericLike reports whether v looks like an integer/float enough that
// plain style would re-type it.
func propNumericLike(v string) bool {
	if v == "" {
		return false
	}
	dots := 0
	i := 0
	if v[0] == '+' || v[0] == '-' {
		i = 1
	}
	for ; i < len(v); i++ {
		c := v[i]
		if c == '.' {
			dots++
			if dots > 1 {
				return false
			}
			continue
		}
		if c < '0' || c > '9' {
			if c == 'e' || c == 'E' {
				continue
			}
			return false
		}
	}
	return i > 0 && (v[0] >= '0' && v[0] <= '9' || (len(v) > 1 && (v[0] == '+' || v[0] == '-')))
}

// spellScalar renders a scalar value in the node's existing style; a plain
// value that would be misread is emitted single-quoted, matching the
// documented replacement contract.
func spellScalar(n *propNode) string {
	switch n.style {
	case 's':
		return "'" + strings.ReplaceAll(n.scalar, "'", "''") + "'"
	case 'd':
		r := strings.ReplaceAll(n.scalar, "\\", "\\\\")
		r = strings.ReplaceAll(r, "\"", "\\\"")
		return "\"" + r + "\""
	case 'h':
		// Plain with attached '#' content (e.g. a#b): valid plain YAML that
		// reads back verbatim. Only single-token attached-hash values carry
		// 'h'; a replacement that needs quoting flips the style to 's'.
		return n.scalar
	default:
		if needsQuoteModel(n.scalar) {
			return "'" + strings.ReplaceAll(n.scalar, "'", "''") + "'"
		}
		return n.scalar
	}
}

// spellPlain renders a value for a freshly written plain context (inserted
// leaf values and sequence items): quoted only when plain style would
// misread it.
func spellPlain(v string) string {
	if needsQuoteModel(v) {
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return v
}

func propRenderNode(b *strings.Builder, n *propNode, nl string) {
	b.WriteString(propSpaces(n.indent))
	b.WriteString(n.key)
	b.WriteString(":")
	switch n.kind {
	case propScalar:
		if n.style == 'm' {
			// A folded two-line plain scalar: the first token on the key
			// line, the second on a continuation line indented two past the
			// key. yaml.v3 folds it to the single spaced string in n.scalar.
			first, second, ok := strings.Cut(n.scalar, " ")
			if !ok {
				panic("model: 'm' scalar must contain exactly one space: " + n.scalar)
			}
			b.WriteString(" ")
			b.WriteString(first)
			b.WriteString(nl)
			b.WriteString(propSpaces(n.indent + 2))
			b.WriteString(second)
			break
		}
		b.WriteString(" ")
		b.WriteString(spellScalar(n))
	case propBool:
		if n.boolV {
			b.WriteString(" true")
		} else {
			b.WriteString(" false")
		}
	}
	b.WriteString(n.comment)
	b.WriteString(nl)
	if n.kind == propSeq {
		for _, it := range n.items {
			b.WriteString(propSpaces(n.seqInd))
			b.WriteString("- ")
			b.WriteString(spellPlain(it))
			b.WriteString(nl)
		}
	}
	for _, c := range n.children {
		propRenderNode(b, c, nl)
	}
}

// render produces the exact expected document bytes.
func (m *docModel) render() []byte {
	nl := propNL(m.crlf)
	var b strings.Builder
	if m.bom {
		b.WriteString(utf8BOM)
	}
	if m.header != "" {
		b.WriteString(m.header)
		b.WriteString(nl)
	}
	for _, n := range m.root {
		propRenderNode(&b, n, nl)
	}
	if m.injected != "" {
		b.WriteString(m.injected)
	}
	if m.footer != "" {
		b.WriteString(m.footer)
		b.WriteString(nl)
	}
	for _, n := range m.tail {
		propRenderNode(&b, n, nl)
	}
	out := b.String()
	if !m.finalNL && strings.HasSuffix(out, nl) {
		// The document's final line carries no terminator. Every element
		// above renders terminated, so exactly the last terminator is
		// dropped; when an edit later inserts at that line, the model
		// sets finalNL (apply), reproducing the editor's separator.
		out = out[:len(out)-len(nl)]
	}
	return []byte(out)
}

// clone deep-copies the model so a property can apply an operation to a
// scratch copy while keeping the pre-operation state for inverse reasoning.
func (m *docModel) clone() *docModel {
	cp := &docModel{
		bom: m.bom, crlf: m.crlf, width: m.width,
		header: m.header, footer: m.footer, injected: m.injected,
		finalNL:     m.finalNL,
		chosenNames: append([]string(nil), m.chosenNames...),
		absentName:  m.absentName,
	}
	for _, n := range m.root {
		cp.root = append(cp.root, clonePropNode(n))
	}
	for _, n := range m.tail {
		cp.tail = append(cp.tail, clonePropNode(n))
	}
	return cp
}

func clonePropNode(n *propNode) *propNode {
	cp := *n
	cp.items = append([]string(nil), n.items...)
	cp.children = nil
	for _, c := range n.children {
		cp.children = append(cp.children, clonePropNode(c))
	}
	return &cp
}

// get resolves a path against the model tree (root and tail together),
// returning the node and its parent; node is nil when absent.
func (m *docModel) get(path []string) (parent *propNode, node *propNode) {
	list := &m.root
	parent = nil
	for i, seg := range path {
		var found *propNode
		for _, n := range *list {
			if n.key == seg {
				found = n
				break
			}
		}
		if found == nil && i == 0 {
			// Root-level keys may also live in the tail.
			for _, n := range m.tail {
				if n.key == seg {
					found = n
					break
				}
			}
		}
		if found == nil {
			return parent, nil
		}
		if i == len(path)-1 {
			return parent, found
		}
		parent = found
		list = &found.children
	}
	return nil, nil
}

// createIndent computes the line indent for a node about to be created at
// prefix, mirroring the documented insertion rules: sibling indentation
// under a populated parent, the grandparent-derived child indentation when
// the parent exists but is empty, and a 2-space-per-level fallback for
// rebuilt chains.
func (m *docModel) createIndent(prefix []string, parentNode *propNode) int {
	if parentNode == nil {
		return 0
	}
	if parentNode.kind == propMap && len(parentNode.children) > 0 {
		return parentNode.children[0].indent
	}
	if len(prefix) < 3 {
		return (len(prefix) - 1) * 2
	}
	_, gp := m.get(prefix[:len(prefix)-2])
	if gp != nil && gp.kind == propMap {
		for _, sib := range gp.children {
			if sib.kind == propMap && len(sib.children) > 0 {
				return sib.children[0].indent
			}
		}
		if len(gp.children) > 0 {
			if step := gp.children[0].indent - gp.indent; step > 0 {
				if _, pk := m.get(prefix[:len(prefix)-1]); pk != nil {
					return pk.indent + step
				}
			}
		}
	}
	return (len(prefix) - 1) * 2
}

// insertPath creates the leaf node at path, building any missing
// intermediates as bare keys. Every root-level creation — including the
// missing first segment of a deeper path — goes to the tail, matching the
// editor's document-root insertion point after the last document line.
func (m *docModel) insertPath(path []string, leaf *propNode) {
	// A leaf or chain whose first missing segment is created at the tail
	// lands after the document's final line: an unterminated final line is
	// terminated first, so the model marks the result terminated.
	rootLevel := len(path) == 1
	var parentNode *propNode
	list := &m.tail
	for i, seg := range path[:len(path)-1] {
		prefix := path[:i+1]
		var found *propNode
		if i == 0 {
			_, found = m.get([]string{seg})
		} else {
			for _, n := range parentNode.children {
				if n.key == seg {
					found = n
					break
				}
			}
		}
		if found == nil {
			found = &propNode{key: seg, kind: propNull, indent: m.createIndent(prefix, parentNode)}
			if i == 0 {
				rootLevel = true
			}
			*list = append(*list, found)
		}
		if found.kind == propNull {
			found.kind = propMap
		}
		parentNode = found
		list = &found.children
	}
	if rootLevel {
		m.finalNL = true
	}
	leaf.indent = m.createIndent(path, parentNode)
	if leaf.kind == propSeq {
		leaf.seqInd = leaf.indent + 2
	}
	*list = append(*list, leaf)
}

// apply mirrors one Edit against the model. It returns errPropEmptyDoc when
// the document has no YAML content left (every further edit is refused with
// an empty-input error), and a plain refusal sentinel is unnecessary: any
// wrong-kind target is expressed by returning an error and leaving the model
// unchanged, matching the editor's refusal contract.
func (m *docModel) apply(e Edit) error {
	if len(e.Path) == 0 {
		return errors.New("model: empty path")
	}
	if len(m.root) == 0 && len(m.tail) == 0 {
		return errPropEmptyDoc
	}
	parent, node := m.get(e.Path)
	if node == nil && parent != nil && parent.kind != propMap {
		// A path that would resolve through a bare key (a mapping whose
		// children were all removed) or through a scalar/boolean/sequence
		// value refuses: the editor's resolver reports it as not-a-mapping.
		return errors.New("model: path crosses a non-mapping value")
	}
	switch {
	case e.Remove:
		if node == nil {
			return nil // removing an absent key is a no-op
		}
		m.deleteNode(parent, node)
		return nil
	case e.Kind == Boolean:
		if e.Bool == nil {
			return errors.New("model: bool edit missing value")
		}
		if node == nil {
			m.insertPath(e.Path, &propNode{key: e.Path[len(e.Path)-1], kind: propBool, boolV: *e.Bool})
			return nil
		}
		if node.kind == propSeq || node.kind == propMap {
			return errors.New("model: not a boolean")
		}
		node.kind = propBool
		node.boolV = *e.Bool
		node.style = 'p' // boolean replacement always writes the plain literal
		return nil
	case e.Kind == Sequence:
		if node == nil {
			if len(e.Sequence) == 0 {
				return nil // empty sequence on an absent key is a no-op
			}
			items := append([]string(nil), e.Sequence...)
			m.insertPath(e.Path, &propNode{key: e.Path[len(e.Path)-1], kind: propSeq, items: items})
			return nil
		}
		if len(e.Sequence) == 0 {
			m.deleteNode(parent, node)
			return nil
		}
		if node.kind == propNull {
			// A bare key gains the sequence block beneath its line at the
			// key's indentation plus two. When the key is the document's
			// final line, the block also terminates it.
			node.kind = propSeq
			node.items = append([]string(nil), e.Sequence...)
			node.seqInd = node.indent + 2
			if !m.finalNL && m.isLastNode(node) {
				m.finalNL = true
			}
			return nil
		}
		if node.kind != propSeq {
			return errors.New("model: not a sequence")
		}
		node.items = append([]string(nil), e.Sequence...)
		return nil
	case e.Kind == Scalar:
		if e.Scalar == nil {
			return errors.New("model: scalar edit missing value")
		}
		if node == nil {
			st := byte('p')
			if needsQuoteModel(*e.Scalar) {
				st = 's'
			}
			m.insertPath(e.Path, &propNode{key: e.Path[len(e.Path)-1], kind: propScalar, scalar: *e.Scalar, style: st})
			return nil
		}
		if node.kind == propSeq || node.kind == propMap {
			return errors.New("model: not a scalar")
		}
		node.kind = propScalar
		node.scalar = *e.Scalar
		if node.style == 'm' {
			// The folded two-line spelling is replaced by an inline write.
			node.style = 'p'
		}
		if node.style != 's' && node.style != 'd' && needsQuoteModel(*e.Scalar) {
			// A plain node whose replacement needs quoting is re-parsed as
			// single-quoted, so later replacements inherit that style.
			node.style = 's'
		}
		return nil
	default:
		return errors.New("model: unknown kind")
	}
}

func (m *docModel) deleteNode(parent *propNode, node *propNode) {
	if parent == nil {
		// Top level: the node sits in root or tail.
		for i, n := range m.root {
			if n == node {
				m.root = append(m.root[:i], m.root[i+1:]...)
				return
			}
		}
		for i, n := range m.tail {
			if n == node {
				m.tail = append(m.tail[:i], m.tail[i+1:]...)
				return
			}
		}
		return
	}
	for i, n := range parent.children {
		if n == node {
			parent.children = append(parent.children[:i], parent.children[i+1:]...)
			if len(parent.children) == 0 && parent.kind == propMap {
				// A mapping stripped of its last child becomes a bare key.
				parent.kind = propNull
			}
			return
		}
	}
}

// isLastNode reports whether node renders as the document's final line (the
// last tail key, or the last root key when the tail is empty).
func (m *docModel) isLastNode(node *propNode) bool {
	if len(m.tail) > 0 {
		return m.tail[len(m.tail)-1] == node
	}
	return len(m.root) > 0 && m.root[len(m.root)-1] == node
}

// ---------------------------------------------------------------------------
// Constructed template and edit generation

var propScalarValues = []string{
	"codex/gpt-5.6-luna", "zai/glm-5.2-mini", "plain-word",
	"true", "yes", "null", "123", "3.14", "", " lead", "trail ", "a:b", "*star", "-dash",
	// Attached-'#' values: a plain write would re-read the '#' as a comment
	// start, so both model and editor emit these single-quoted.
	"v#2", "code#extra",
	// Multi-token plain scalars: yaml.v3 reads the whole internal-whitespace
	// run as one value, and the editor replaces the whole span.
	"hello world", "two plain words",
}

// propPlainHashValues are values an existing node may carry in plain style
// with an attached '#': scalars that yaml.v3 reads back exactly (a '#'
// begins a comment only after whitespace or at the value start). The last
// entry also carries internal whitespace, covering the combined shape.
var propPlainHashValues = []string{"a#b", "mini#1", "two w#rds"}

var propSeqItemValues = []string{"minime/gemma", "zai/glm-5.2", "codex/gpt-5.6-sol", "yes", "42", "mini#2"}

var propModelNamePool = []string{
	"codex/gpt", "zai/glm-5.2", "minime/gemma", "openai/o3",
	"anthropic/claude", "google/gemini", "xai/grok", "neuralwatt/deepseek",
}

type propPathSpec struct {
	path  []string
	kinds []string // "scalar", "bool", "seq", "seqempty", "remove"
}

// drawTemplate constructs a fresh document model with known edited and
// untouched regions.
func drawTemplate(t *rapid.T) *docModel {
	w := rapid.IntRange(2, 4).Draw(t, "width")
	crlf := rapid.Bool().Draw(t, "crlf")
	bom := rapid.Bool().Draw(t, "bom")
	n := rapid.IntRange(1, 4).Draw(t, "nModels")
	names := rapid.SliceOfNDistinct(rapid.SampledFrom(propModelNamePool), n, n, func(s string) string { return s }).Draw(t, "names")
	rest := propPoolExcept(names)
	absent := rapid.SampledFrom(rest).Draw(t, "absentModel")
	fallbackPresent := rapid.Bool().Draw(t, "fallbackPresent")
	nItems := rapid.IntRange(1, 3).Draw(t, "nItems")
	fallbackItems := rapid.SliceOfN(rapid.SampledFrom(propSeqItemValues), nItems, nItems).Draw(t, "fallbackItems")
	model0EnabledPresent := rapid.Bool().Draw(t, "model0EnabledPresent")
	model0Enabled := rapid.Bool().Draw(t, "model0Enabled")
	primaryComment := rapid.Bool().Draw(t, "primaryComment")
	reasoningComment := rapid.Bool().Draw(t, "reasoningComment")
	enabledComment := rapid.Bool().Draw(t, "enabledComment")
	special := rapid.SampledFrom([]int{0, 1, 2}).Draw(t, "specialSpelling")
	finalNL := rapid.Bool().Draw(t, "finalNL")
	hashPrimary := rapid.Bool().Draw(t, "hashPrimary")

	m := &docModel{
		bom: bom, crlf: crlf, width: w,
		header:      "# managed document",
		footer:      "# end of managed document",
		finalNL:     finalNL,
		chosenNames: append([]string(nil), names...),
		absentName:  absent,
	}

	poly := &propNode{key: "polytoken", kind: propMap, indent: 0}
	modelVal := propScalarValues[rapid.IntRange(0, 2).Draw(t, "primaryValue")]
	pModel := &propNode{key: "model", kind: propScalar, indent: w, scalar: modelVal, style: 'p'}
	if hashPrimary {
		// A pre-existing plain scalar with attached '#' content: the editor
		// must replace (or keep) the whole attached-hash span.
		pModel.scalar = propPlainHashValues[rapid.IntRange(0, len(propPlainHashValues)-1).Draw(t, "hashValue")]
		pModel.style = 'h'
	}
	if primaryComment {
		pModel.comment = " # primary"
	}
	poly.children = append(poly.children, pModel)

	pReason := &propNode{key: "reasoning", kind: propScalar, indent: w}
	multilinePlain := rapid.Bool().Draw(t, "multilinePlain")
	if multilinePlain {
		// A folded two-line plain scalar: two tokens across a continuation
		// line, read back by yaml.v3 as one spaced string.
		pReason.scalar, pReason.style = "high plus", 'm'
	} else {
		switch special {
		case 0:
			pReason.scalar, pReason.style = "high", 'p'
		case 1:
			pReason.scalar, pReason.style = "it's fine", 's'
		default:
			pReason.scalar, pReason.style = `say "hi"`, 'd'
		}
	}
	if reasoningComment {
		pReason.comment = " # reasoning"
	}
	poly.children = append(poly.children, pReason)

	if fallbackPresent {
		poly.children = append(poly.children, &propNode{
			key: "fallback_models", kind: propSeq, indent: w,
			items: append([]string(nil), fallbackItems...), seqInd: 2 * w,
		})
	}
	m.root = append(m.root, poly)

	models := &propNode{key: "models", kind: propMap, indent: 0}
	for i, name := range names {
		mn := &propNode{key: name, kind: propMap, indent: w}
		if i > 0 || model0EnabledPresent {
			v := true
			if i == 0 {
				v = model0Enabled
			}
			en := &propNode{key: "enabled", kind: propBool, indent: 2 * w, boolV: v}
			if enabledComment {
				en.comment = " # state"
			}
			mn.children = append(mn.children, en)
		}
		mn.children = append(mn.children, &propNode{key: "reasoning", kind: propScalar, indent: 2 * w, scalar: "high", style: 'p'})
		models.children = append(models.children, mn)
	}
	m.root = append(m.root, models)

	m.root = append(m.root, &propNode{key: "routing", kind: propMap, indent: 0, children: []*propNode{
		{key: "enabled", kind: propBool, indent: w, boolV: true},
	}})
	return m
}

func propPoolExcept(names []string) []string {
	var out []string
	for _, c := range propModelNamePool {
		taken := false
		for _, n := range names {
			if n == c {
				taken = true
				break
			}
		}
		if !taken {
			out = append(out, c)
		}
	}
	return out
}

// pathSpecs builds the edit-path pool for a template, including absent
// targets (insertions and chain rebuilds) and wrong-kind combinations that
// must be refused.
func (m *docModel) pathSpecs() []propPathSpec {
	specs := []propPathSpec{
		{[]string{"polytoken", "model"}, []string{"scalar", "bool", "remove"}},
		{[]string{"polytoken", "reasoning"}, []string{"scalar", "remove"}},
		{[]string{"polytoken", "fallback_models"}, []string{"seq", "seqempty", "scalar", "bool", "remove"}},
		{[]string{"polytoken", "extra"}, []string{"scalar", "bool", "remove"}},
		{[]string{"polytoken"}, []string{"remove"}},
		{[]string{"routing", "enabled"}, []string{"bool", "scalar", "remove"}},
		{[]string{"extended"}, []string{"scalar", "bool", "seq", "seqempty", "remove"}},
		{[]string{"extended", "deep"}, []string{"scalar", "bool", "remove"}},
		{[]string{"models", m.absentName, "enabled"}, []string{"bool"}},
	}
	for _, name := range m.chosenNames {
		specs = append(specs,
			propPathSpec{[]string{"models", name, "enabled"}, []string{"bool", "scalar", "remove"}},
			propPathSpec{[]string{"models", name, "reasoning"}, []string{"scalar", "bool", "remove"}},
			propPathSpec{[]string{"models", name}, []string{"remove"}},
		)
	}
	return specs
}

// drawEdit draws one bounded edit against the template's path pool.
// History ops are drawn against the pristine template and replayed against
// the mutating model, so state-dependent acceptance is evaluated at replay
// time by the model itself in TestPropertyYAMLEditHistories.
func (m *docModel) drawEdit(t *rapid.T) Edit {
	spec := rapid.SampledFrom(m.pathSpecs()).Draw(t, "path")
	kind := rapid.SampledFrom(spec.kinds).Draw(t, "kind")
	e := Edit{Path: append([]string(nil), spec.path...)}
	switch kind {
	case "scalar":
		e.Kind = Scalar
		v := rapid.SampledFrom(propScalarValues).Draw(t, "scalar")
		e.Scalar = &v
	case "bool":
		e.Kind = Boolean
		v := rapid.Bool().Draw(t, "bool")
		e.Bool = &v
	case "seq":
		e.Kind = Sequence
		n := rapid.IntRange(1, 3).Draw(t, "nItems")
		e.Sequence = rapid.SliceOfN(rapid.SampledFrom(propSeqItemValues), n, n).Draw(t, "items")
	case "seqempty":
		e.Kind = Sequence
	case "remove":
		e.Remove = true
	}
	return e
}

// editLabel renders an edit readably for failure output.
func editLabel(e Edit) string {
	var b strings.Builder
	b.WriteString(strings.Join(e.Path, "."))
	switch {
	case e.Remove:
		b.WriteString(" remove")
	case e.Kind == Boolean && e.Bool != nil:
		b.WriteString(" = ")
		b.WriteString(boolStr(*e.Bool))
	case e.Kind == Scalar && e.Scalar != nil:
		b.WriteString(" = ")
		b.WriteString(*e.Scalar)
	case e.Kind == Sequence:
		b.WriteString(" = [")
		b.WriteString(strings.Join(e.Sequence, ", "))
		b.WriteString("]")
	default:
		b.WriteString(" (unset value)")
	}
	return b.String()
}

// inject adds a verbatim block (used for ambiguity injection) that renders
// before the footer, with the document's own line terminator.
func (m *docModel) inject(lines ...string) {
	nl := propNL(m.crlf)
	m.injected = strings.Join(lines, nl) + nl
}

// checkSemantics decodes out with a plain yaml.Node decode (independent of
// the editor's internals) and asserts every model key carries the intended
// semantic value and type.
func checkSemantics(tb propTB, out []byte, m *docModel) {
	tb.Helper()
	payload := out
	if bytes.HasPrefix(payload, []byte(utf8BOM)) {
		payload = payload[len(utf8BOM):]
	}
	var root yaml.Node
	if err := yaml.Unmarshal(payload, &root); err != nil {
		tb.Fatalf("output does not decode: %v\n%q", err, out)
	}
	top := &root
	if top.Kind == yaml.DocumentNode {
		if len(top.Content) == 0 {
			tb.Fatalf("decoded document is empty\n%q", out)
		}
		top = top.Content[0]
	}
	for _, n := range m.root {
		propCheckNode(tb, top, n, out)
	}
	for _, n := range m.tail {
		propCheckNode(tb, top, n, out)
	}
}

func propCheckNode(tb propTB, dm *yaml.Node, pn *propNode, out []byte) {
	tb.Helper()
	if dm == nil || dm.Kind != yaml.MappingNode {
		tb.Fatalf("model key %q: decoded parent is not a mapping\n%q", pn.key, out)
	}
	var val *yaml.Node
	for j := 0; j+1 < len(dm.Content); j += 2 {
		if dm.Content[j].Value == pn.key {
			val = dm.Content[j+1]
			break
		}
	}
	if val == nil {
		tb.Fatalf("model key %q missing from decoded output\n%q", pn.key, out)
	}
	switch pn.kind {
	case propBool:
		if val.Tag != "!!bool" || val.Value != boolStr(pn.boolV) {
			tb.Fatalf("key %q: decoded %s(%s) want bool(%s)\n%q", pn.key, val.Tag, val.Value, boolStr(pn.boolV), out)
		}
	case propScalar:
		if val.Tag != "!!str" || val.Value != pn.scalar {
			tb.Fatalf("key %q: decoded %s(%q) want str(%q)\n%q", pn.key, val.Tag, val.Value, pn.scalar, out)
		}
	case propSeq:
		if val.Tag != "!!seq" || len(val.Content) != len(pn.items) {
			tb.Fatalf("key %q: decoded %s with %d items want seq with %d items\n%q", pn.key, val.Tag, len(val.Content), len(pn.items), out)
		}
		for i, it := range pn.items {
			if val.Content[i].Tag != "!!str" || val.Content[i].Value != it {
				tb.Fatalf("key %q item %d: decoded %s(%q) want str(%q)\n%q", pn.key, i, val.Content[i].Tag, val.Content[i].Value, it, out)
			}
		}
	case propMap, propNull:
		for _, c := range pn.children {
			propCheckNode(tb, val, c, out)
		}
	}
}

// propTB is the minimal failure interface shared by *testing.T and *rapid.T.
type propTB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}
