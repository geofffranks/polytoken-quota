package document

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EditOption configures EditYAML.
type EditOption func(*editOptions)

type editOptions struct {
	scopedAmbiguity bool
}

// EditScopedAmbiguity relaxes the ambiguity refusal from whole-document to
// per-edit-path: anchors, aliases, merge keys, and duplicate keys that do not
// involve an edited path are tolerated, while anything on the path itself — an
// anchor on a key or value node, an alias value, a duplicate key, or a merge
// key in a mapping on the path — is still refused with an ambiguous error (see
// IsAmbiguous). This exists for editors of operator-authored files that use
// YAML anchors for shared blocks (e.g. desired.yaml); byte-preserving editors
// of tool-managed files should keep the strict default.
func EditScopedAmbiguity() EditOption {
	return func(o *editOptions) { o.scopedAmbiguity = true }
}

// EditYAML applies edits to raw YAML bytes using parser-assisted, exact span
// location and minimal byte-level replacement/insertion/removal. It never
// round-trips the document through a generic serializer. Every byte outside the
// addressed spans — comments, key order, quoting, siblings, line endings, and a
// leading BOM — is preserved. Ambiguous structures (duplicate keys,
// aliases/anchors, merge keys) are refused with a structural error; see
// IsAmbiguous. Pass EditScopedAmbiguity to refuse only ambiguity that involves
// an edited path.
//
// Edits are applied in order; later edits operate on the bytes produced by
// earlier ones, so each edit's path is resolved against the current (possibly
// already-edited) text. Callers that want deterministic results for a set of
// independent edits should pass them in a stable order.
func EditYAML(raw []byte, edits []Edit, opts ...EditOption) ([]byte, error) {
	var o editOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	for _, e := range edits {
		out, err := applyOne(raw, e, o.scopedAmbiguity)
		if err != nil {
			return nil, err
		}
		raw = out
	}
	return raw, nil
}

// applyOne parses raw fresh, resolves e.Path, and performs the single edit.
// Re-parsing each edit keeps span offsets correct after prior mutations. When
// scopedAmbiguity is set, ambiguity validation runs along the edit path only.
func applyOne(raw []byte, e Edit, scopedAmbiguity bool) ([]byte, error) {
	if len(e.Path) == 0 {
		return nil, newError("document: edit with empty path")
	}
	d, err := parseDocMode(raw, scopedAmbiguity)
	if err != nil {
		return nil, err
	}
	if scopedAmbiguity {
		if err := d.validateEditPath(e.Path); err != nil {
			return nil, err
		}
	}
	switch {
	case e.Remove:
		return d.removeKey(raw, e.Path)
	case e.Kind == Boolean:
		if e.Bool == nil {
			return nil, newError("document: boolean edit %q missing value", strings.Join(e.Path, "."))
		}
		return d.setBool(raw, e.Path, *e.Bool)
	case e.Kind == Sequence:
		return d.setSequence(raw, e.Path, e.Sequence)
	case e.Kind == Scalar:
		if e.Scalar == nil {
			return nil, newError("document: scalar edit %q missing value", strings.Join(e.Path, "."))
		}
		return d.setScalar(raw, e.Path, *e.Scalar)
	default:
		return nil, newError("document: edit %q has unknown kind", strings.Join(e.Path, "."))
	}
}

// validateEditPath runs the ambiguity refusal along the resolved edit path
// only. It is the scoped-mode counterpart of doc.validate: anchors, aliases,
// merge keys, and duplicate keys elsewhere in the document are tolerated,
// while anything the edit itself would touch or depend on is refused because
// the edit would change or depend on alias-visible values. Specifically,
// refused on the path: an anchor on any key or value node, an alias value, a
// duplicate of a path key within the mapping holding it, an alias key whose
// target name equals a path key, and any merge key in a mapping on the path.
// A path whose prefix is absent (insertion) validates only what resolved.
func (d *doc) validateEditPath(path []string) error {
	cur := d.root
	for cur != nil && cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for i, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			// Not a mapping: resolveValue/setters produce the ordinary
			// wrong-kind error later; nothing ambiguous to report here.
			return nil
		}
		var found, foundKey *yaml.Node
		seen := false
		for j := 0; j+1 < len(cur.Content); j += 2 {
			k := cur.Content[j]
			v := cur.Content[j+1]
			if k == nil {
				continue
			}
			if k.Kind == yaml.ScalarNode && k.Value == "<<" {
				return newAmbiguousError("document: merge key (<<) on the edit path is ambiguous")
			}
			if k.Kind == yaml.AliasNode && k.Alias != nil && k.Alias.Value == seg {
				return newAmbiguousError("document: alias key *%s duplicates edit path key %q", aliasName(d.raw, k), strings.Join(path[:i+1], "."))
			}
			if k.Kind == yaml.ScalarNode && k.Value == seg {
				if seen {
					return newAmbiguousError("document: duplicate key %q on the edit path is ambiguous", strings.Join(path[:i+1], "."))
				}
				seen = true
				found, foundKey = v, k
			}
		}
		if found == nil {
			// Absent: the edit inserts it; nothing further to validate.
			return nil
		}
		if foundKey.Anchor != "" {
			return newAmbiguousError("document: anchor &%s on the edit path is ambiguous", foundKey.Anchor)
		}
		if found.Anchor != "" {
			return newAmbiguousError("document: anchor &%s on the edit path is ambiguous", found.Anchor)
		}
		if found.Kind == yaml.AliasNode {
			return newAmbiguousError("document: alias value (*%s) on the edit path is ambiguous", aliasName(d.raw, found))
		}
		cur = found
	}
	return nil
}

// resolveValue walks the node tree following path and returns the value node at
// path plus the (key, value) node pair on the final mapping. exists reports
// whether the final key is present.
func (d *doc) resolveValue(path []string) (val *yaml.Node, keyNode *yaml.Node, parent *yaml.Node, exists bool, err error) {
	cur := d.root
	for cur != nil && cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	parent = cur
	for i, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil, nil, parent, false, newError("document: path %q is not a mapping", strings.Join(path[:i+1], "."))
		}
		var found *yaml.Node
		var foundKey *yaml.Node
		for j := 0; j+1 < len(cur.Content); j += 2 {
			k := cur.Content[j]
			v := cur.Content[j+1]
			if k != nil && k.Kind == yaml.ScalarNode && k.Value == seg {
				found, foundKey = v, k
				break
			}
		}
		if found == nil {
			return nil, nil, parent, false, nil
		}
		parent = cur
		keyNode = foundKey
		val = found
		cur = found
	}
	return val, keyNode, parent, true, nil
}

// setScalar replaces the scalar value at path with v, preserving any surrounding
// quoting style. If the key is absent, it is inserted at the parent mapping's
// indentation (or the document's for a top-level key). A bare key (`k:` with no
// value) gains the documented ": " separation instead of a glued `k:v`.
func (d *doc) setScalar(raw []byte, path []string, v string) ([]byte, error) {
	val, _, _, exists, err := d.resolveValue(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return d.insertKey(raw, path, []byte(encodeScalar(v, 0)))
	}
	if val == nil || val.Kind != yaml.ScalarNode {
		return nil, newError("document: %q is not a scalar", strings.Join(path, "."))
	}
	if isImplicitNull(val) {
		// `key:` with no value: the null node occupies no bytes, so splice the
		// separated value in place (any trailing comment keeps its spacing).
		start := d.offset(val.Line, val.Column)
		return splice(raw, start, start, []byte(" "+encodeScalar(v, 0))), nil
	}
	start, end := d.valueSpan(val)
	return splice(raw, start, end, []byte(emitScalarValue(val, v))), nil
}

// setBool replaces the boolean value at path with v. If the key is absent, it is
// inserted at the parent mapping's indentation as a plain "true"/"false".
func (d *doc) setBool(raw []byte, path []string, v bool) ([]byte, error) {
	val, _, _, exists, err := d.resolveValue(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		rep := "false"
		if v {
			rep = "true"
		}
		return d.insertKey(raw, path, []byte(rep))
	}
	// Guard against a nil value node before valueSpan dereferences it.
	// resolveValue maps a nil value node to not-found (exists=false), so this
	// guard is unreachable through the normal edit path today; it mirrors
	// setScalar's guard and protects against a future refactor that changes
	// that mapping.
	if val == nil {
		return nil, newError("document: %q has no value node", strings.Join(path, "."))
	}
	rep := "false"
	if v {
		rep = "true"
	}
	if isImplicitNull(val) {
		// `key:` with no value: write the separated literal, not `k:true`.
		start := d.offset(val.Line, val.Column)
		return splice(raw, start, start, []byte(" "+rep)), nil
	}
	if val.Kind != yaml.ScalarNode || val.Tag != "!!bool" {
		// Allow editing a bool that happens to be untyped-tagged; still replace
		// the span. But refuse sequences/mappings.
		if val.Kind == yaml.MappingNode || val.Kind == yaml.SequenceNode {
			return nil, newError("document: %q is not a boolean", strings.Join(path, "."))
		}
	}
	start, end := d.valueSpan(val)
	return splice(raw, start, end, []byte(rep)), nil
}

// setSequence replaces the sequence at path with the items. It rewrites the
// whole list block in place using the same dash style and indentation as the
// existing list, or inserts a new block list at the parent's indentation when
// the key is absent. An empty list removes the key (matching the reconciler's
// Remove semantics for fallback_models).
func (d *doc) setSequence(raw []byte, path []string, items []string) ([]byte, error) {
	val, keyNode, _, exists, err := d.resolveValue(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		if len(items) == 0 {
			return raw, nil // nothing to insert
		}
		return d.insertSequenceKey(raw, path, items)
	}
	if len(items) == 0 {
		return d.removeKey(raw, path)
	}
	if val.Kind != yaml.SequenceNode {
		if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
			// A null-valued key (`key:` bare, or an explicit null spelling)
			// becomes the parent of a new block sequence under the key line.
			return d.sequenceUnderKey(raw, val, keyNode, items)
		}
		return nil, newError("document: %q is not a sequence", strings.Join(path, "."))
	}
	// A flow-style sequence (`k: [a, b]`) shares the key's line, so a block-style
	// rewrite would overwrite the key and emit invalid YAML. Editing flow style
	// in place is not supported; fail closed rather than corrupt the output.
	if val.Style&yaml.FlowStyle != 0 {
		return nil, newError("document: %q is a flow-style sequence (not editable; use block style)", strings.Join(path, "."))
	}
	start, end := d.sequenceSpan(val)
	// The existing list items occupy whole lines; preserve that layout by
	// rendering the replacement items with the same dash indentation as the
	// existing first item. Compute the indent directly from the item's line
	// bytes (count leading spaces) rather than column arithmetic.
	ind := d.lineIndent(d.offset(val.Line, 1))
	if len(val.Content) > 0 && val.Content[0] != nil {
		itemLineStart := d.offset(val.Content[0].Line, 1)
		dashIndent := d.lineIndent(itemLineStart)
		if dashIndent > ind {
			ind = dashIndent
		}
	}
	block := sequenceBlock(items, ind, string(d.newline))
	return splice(raw, start, end, block), nil
}

// sequenceUnderKey renders items as a block sequence beneath an existing key
// whose value is null. The key line is untouched except that an explicit null
// spelling (null, ~, quoted "") is dropped from it, back to the separating
// whitespace after the colon; a bare key keeps its line verbatim, including
// any trailing comment. The block lands on the lines following the key, at
// the key's indentation plus two spaces.
func (d *doc) sequenceUnderKey(raw []byte, val, keyNode *yaml.Node, items []string) ([]byte, error) {
	if keyNode == nil {
		return nil, newError("document: cannot locate key line for the null value")
	}
	out, err := d.appendSequenceBlock(raw, keyNode.Line, keyNode, items)
	if err != nil {
		return nil, err
	}
	if isImplicitNull(val) {
		return out, nil
	}
	// Explicit null: remove the null token so the key reads bare above its
	// new block, trimming back over its separating whitespace to the colon.
	vs, ve := d.valueSpan(val)
	ws := vs
	for ws > 0 && (raw[ws-1] == ' ' || raw[ws-1] == '\t') {
		ws--
	}
	if ws == 0 || raw[ws-1] != ':' {
		ws = vs // defensive: only strip separation space that follows a colon
	}
	return splice(out, ws, ve, nil), nil
}

// appendSequenceBlock inserts the item lines of a block sequence (one
// "- item" line per item, at the key's indentation plus two) directly after
// the given key line. When the key line is the document's unterminated final
// line, it is terminated first so the block starts on its own line.
func (d *doc) appendSequenceBlock(raw []byte, keyLine int, keyNode *yaml.Node, items []string) ([]byte, error) {
	itemInd := d.keyIndent(keyNode) + 2
	insertAt := d.endOfLineWithNewline(keyLine)
	nl := string(d.newline)
	pad := strings.Repeat(" ", itemInd)
	var b strings.Builder
	if d.finalLineUnterminated(raw, insertAt) {
		b.WriteString(nl)
	}
	for _, it := range items {
		b.WriteString(pad)
		b.WriteString("- ")
		b.WriteString(encodeScalar(it, itemInd))
		b.WriteString(nl)
	}
	return splice(raw, insertAt, insertAt, []byte(b.String())), nil
}

// keyIndent returns the indentation of the line carrying the given key node;
// it is the base indentation for blocks written beneath that key.
func (d *doc) keyIndent(keyNode *yaml.Node) int {
	if keyNode == nil {
		return 0
	}
	return d.lineIndent(d.offset(keyNode.Line, 1))
}

// insertSequenceKey inserts a new block-sequence key (e.g. fallback_models)
// with its items rendered as indented dash lines beneath the key. The key is
// placed as the final child of its parent mapping at the parent's child
// indentation.
func (d *doc) insertSequenceKey(raw []byte, path []string, items []string) ([]byte, error) {
	// Ensure ancestors exist, mirroring insertKey.
	for i := 1; i < len(path); i++ {
		if _, _, _, exists, err := d.resolveValue(path[:i]); err != nil {
			return nil, err
		} else if !exists {
			out, err := d.insertKey(raw, path[:i], nil)
			if err != nil {
				return nil, err
			}
			raw = out
			d2, err := parseDocMode(raw, d.scoped)
			if err != nil {
				return nil, err
			}
			*d = *d2
		}
	}
	key := path[len(path)-1]
	ind := d.indentForInsert(path)
	parentLine := d.parentInsertLine(path[:len(path)-1])
	insertAt := d.endOfLineWithNewline(parentLine)
	nl := string(d.newline)
	pad := strings.Repeat(" ", ind)
	itemInd := ind + 2
	itemPad := strings.Repeat(" ", itemInd)
	var b strings.Builder
	if d.finalLineUnterminated(raw, insertAt) {
		// The insertion point is the document's unterminated final line;
		// terminate it so the new key starts on its own line.
		b.WriteString(nl)
	}
	b.WriteString(pad)
	b.WriteString(key + ":")
	b.WriteString(nl)
	for _, it := range items {
		b.WriteString(itemPad)
		b.WriteString("- ")
		b.WriteString(encodeScalar(it, itemInd))
		b.WriteString(nl)
	}
	return splice(raw, insertAt, insertAt, []byte(b.String())), nil
}

// removeKey deletes the final key of path together with its value (and, for a
// sequence/list value, its child lines). The full key line including its
// trailing newline is removed; for a key whose value spans multiple lines, all
// of those lines are removed. Sibling keys are untouched.
func (d *doc) removeKey(raw []byte, path []string) ([]byte, error) {
	val, keyNode, parent, exists, err := d.resolveValue(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return raw, nil
	}
	if keyNode == nil {
		return nil, newError("document: cannot locate key line for %q", strings.Join(path, "."))
	}
	// A flow/inline parent mapping (`defaults: {full: x, mini: y}`) shares one
	// physical line between the addressed key and its unmanaged siblings, so
	// whole-line removal would erase the siblings too. Comma-aware span surgery
	// is not supported; fail closed rather than corrupt unmanaged fields.
	if parent != nil && parent.Kind == yaml.MappingNode && parent.Style&yaml.FlowStyle != 0 {
		return nil, newError("document: %q is inside a flow-style mapping (not removable; use block style)", strings.Join(path, "."))
	}
	lineStart := d.offset(keyNode.Line, 1)
	// End of the removed region: end of the last line occupied by the value,
	// including the later lines of a multiline scalar value.
	lineEnd := d.endOfLineWithNewline(d.valueBlockEnd(val))
	return splice(raw, lineStart, lineEnd, nil), nil
}

// valueSpan returns the byte range [start,end) covering a scalar value node,
// including its surrounding quotes for quoted scalars. yaml.v3 reports the
// node column at the opening quote for single/double-quoted scalars and at the
// first value byte for plain scalars, so unquoted values need no adjustment.
//
// Plain scalars span their complete source region: the first line's content
// plus, in block context, every folded continuation line indented deeper than
// the value's own key line. That covers multiline plain scalars (which
// yaml.v3 reports at their first line/column only) and the content lines of
// literal/folded block scalars, whose indicator the plain scan starts at.
// Interior line breaks, blank lines, and continuation indentation belong to
// the region; trailing whitespace on each line and a whitespace-delimited
// comment do not. In flow context the region ends at the first flow
// indicator instead.
func (d *doc) valueSpan(n *yaml.Node) (int, int) {
	start := d.offset(n.Line, n.Column)
	if n.Style == yaml.DoubleQuotedStyle {
		// start points at the opening quote. A backslash escapes the next
		// byte (including `\"`), so an escaped quote never ends the span.
		end := start + 1
		for end < len(d.raw) {
			switch d.raw[end] {
			case '\\':
				end += 2
				continue
			case '"':
				return start, end + 1
			}
			end++
		}
		return start, end
	}
	if n.Style == yaml.SingleQuotedStyle {
		// start points at the opening quote. A doubled quote ('') is an
		// escaped quote inside the scalar, not the closing quote.
		end := start + 1
		for end < len(d.raw) {
			if d.raw[end] == '\'' {
				if end+1 < len(d.raw) && d.raw[end+1] == '\'' {
					end += 2
					continue
				}
				return start, end + 1
			}
			end++
		}
		return start, end
	}
	if n.Kind != yaml.ScalarNode {
		// Non-scalar value reached through the generic fingerprint path: keep
		// the historical inline scan for that shape.
		end := start
		for end < len(d.raw) && !isValueTerminator(d.raw[end]) {
			if end == start && d.raw[end] == '#' {
				break
			}
			end++
		}
		return start, end
	}
	if d.inFlowContext(n) {
		return d.flowPlainSpan(start)
	}
	return d.blockPlainSpan(n, start)
}

// blockPlainSpan spans the source region of a plain (or block-indicator)
// scalar value in block context. Continuation lines are those indented deeper
// than the value's own line (the key line); blank lines join the region only
// when a continuation follows them, and a comment line ends the scan (yaml
// rejects a continuation after one). Trailing whitespace on each line and any
// whitespace-delimited comment stay outside the span.
func (d *doc) blockPlainSpan(n *yaml.Node, start int) (int, int) {
	end := d.lineContentEnd(start)
	keyIndent := d.lineIndent(d.offset(n.Line, 1))
	for line := n.Line + 1; line < len(d.lines); line++ {
		lineStart := d.lines[line]
		if lineStart >= len(d.raw) {
			break // nothing follows the final line terminator
		}
		i := lineStart
		for i < len(d.raw) && d.raw[i] == ' ' {
			i++
		}
		lineEnd := i
		for lineEnd < len(d.raw) && d.raw[lineEnd] != '\n' && d.raw[lineEnd] != '\r' {
			lineEnd++
		}
		if i == lineEnd {
			continue // blank or whitespace-only line: folded newline candidate
		}
		if d.raw[i] == '#' {
			break // a comment line cannot precede a continuation
		}
		if i-lineStart <= keyIndent {
			break // at or outside the mapping's indentation: not a continuation
		}
		// The line continues the scalar up to its content end: a
		// whitespace-delimited comment and trailing whitespace stay outside.
		end = d.lineContentEnd(lineStart)
	}
	return start, end
}

// flowPlainSpan spans a plain scalar inside a flow-style collection. Flow
// indicators (, [ ] { }) end the scalar, as does a whitespace-preceded '#'
// comment; internal whitespace and a '#' attached to content stay inside it.
// The scan continues across line breaks (a flow scalar may fold over lines)
// and trims trailing whitespace, including the final line break when the
// scalar closes on a later line.
func (d *doc) flowPlainSpan(start int) (int, int) {
	end := start
	for end < len(d.raw) {
		b := d.raw[end]
		if b == ',' || b == '[' || b == ']' || b == '{' || b == '}' {
			break
		}
		if b == '#' && (end == start || d.raw[end-1] == ' ' || d.raw[end-1] == '\t') {
			break
		}
		end++
	}
	for end > start && (d.raw[end-1] == ' ' || d.raw[end-1] == '\t') {
		end--
	}
	if end > start && d.raw[end-1] == '\n' {
		end--
		if end > start && d.raw[end-1] == '\r' {
			end--
		}
	}
	return start, end
}

// lineContentEnd returns the offset just past a line's content, scanning from
// offset o to the line terminator, a whitespace-delimited '#' comment, and
// then back over trailing spaces and tabs. A '#' at o itself begins a comment
// (an empty value).
func (d *doc) lineContentEnd(o int) int {
	end := o
	for end < len(d.raw) && d.raw[end] != '\n' && d.raw[end] != '\r' {
		if d.raw[end] == '#' && (end == o || d.raw[end-1] == ' ' || d.raw[end-1] == '\t') {
			break
		}
		end++
	}
	for end > o && (d.raw[end-1] == ' ' || d.raw[end-1] == '\t') {
		end--
	}
	return end
}

// inFlowContext reports whether n sits inside a flow-style mapping or
// sequence between the document root and n. Flow context changes which bytes
// terminate a plain scalar and disables block-style line continuation.
func (d *doc) inFlowContext(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	var walk func(cur *yaml.Node, flow bool) bool
	walk = func(cur *yaml.Node, flow bool) bool {
		if cur == nil {
			return false
		}
		if cur == n {
			return flow
		}
		childFlow := flow ||
			(cur.Kind == yaml.MappingNode || cur.Kind == yaml.SequenceNode) &&
				cur.Style&yaml.FlowStyle != 0
		for _, c := range cur.Content {
			if walk(c, childFlow) {
				return true
			}
		}
		return false
	}
	return walk(d.root, false)
}

// isImplicitNull reports whether n is the valueless `key:` form: a plain
// empty null node occupying no bytes of its own. Explicit null spellings
// (null, ~, quoted "") carry bytes and keep the ordinary span path.
func isImplicitNull(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode &&
		n.Tag == "!!null" && n.Value == "" && n.Style == 0
}

// sequenceSpan returns the byte range covering all lines of a block sequence
// node (from the first item line through the end of the last item's line,
// excluding its trailing newline so the surrounding structure is preserved).
func (d *doc) sequenceSpan(n *yaml.Node) (int, int) {
	start := d.offset(n.Line, 1)
	lastLine := n.Line
	for _, c := range allNodes(n) {
		if c != nil && c.Line > lastLine {
			lastLine = c.Line
		}
	}
	end := d.endOfLine(lastLine) // exclude trailing newline
	return start, end
}

// endOfLine returns the offset just past the last byte of the given line
// (excluding the line terminator).
func (d *doc) endOfLine(line int) int {
	start := d.offset(line, 1)
	end := start
	for end < len(d.raw) && d.raw[end] != '\n' && d.raw[end] != '\r' {
		end++
	}
	return end
}

// endOfLineWithNewline returns the offset just past the line terminator that
// ends the given line, consuming a CRLF or LF pair if present. If the line is
// the final line with no terminator, it returns len(raw).
func (d *doc) endOfLineWithNewline(line int) int {
	end := d.endOfLine(line)
	if end < len(d.raw) && d.raw[end] == '\r' {
		end++
	}
	if end < len(d.raw) && d.raw[end] == '\n' {
		end++
	}
	return end
}

// lineIndent returns the number of leading space bytes on the line that begins
// at offset lineStart.
func (d *doc) lineIndent(lineStart int) int {
	n := 0
	for lineStart+n < len(d.raw) && d.raw[lineStart+n] == ' ' {
		n++
	}
	return n
}

// indentForInsert returns the indentation (in spaces) at which the final
// segment of path should be placed. When the parent mapping at path[:len-1] is
// populated, the new key matches the indentation of its existing siblings.
// When the parent is null or empty (a freshly inserted intermediate key with no
// children yet), the indentation is derived from the document's existing
// structure so the result is correct for any indentation width.
func (d *doc) indentForInsert(path []string) int {
	if len(path) <= 1 {
		return 0
	}
	// The parent mapping (whose value is at path[:len-1]) already contains the
	// new key's siblings; the new key must sit at the SAME indentation as those
	// existing keys, not one level deeper.
	parent, _, _, exists, err := d.resolveValue(path[:len(path)-1])
	if err != nil || !exists || parent == nil {
		return (len(path) - 1) * 2
	}
	if parent.Kind == yaml.DocumentNode {
		if len(parent.Content) > 0 {
			parent = parent.Content[0]
		}
	}
	if parent.Kind == yaml.MappingNode && len(parent.Content) > 0 {
		firstKey := parent.Content[0]
		if firstKey != nil {
			return d.lineIndent(d.offset(firstKey.Line, 1))
		}
	}
	// Parent exists but is null or empty (e.g. a freshly inserted intermediate
	// key). Derive the correct indentation from the surrounding structure
	// instead of assuming a fixed step.
	return d.deriveIndent(path)
}

// deriveIndent computes the indentation for a new key whose parent mapping is
// null or empty. It examines the grandparent mapping (path[:len-2]) for a
// sibling of the parent key whose value has populated children — since valid
// YAML uses consistent indentation within a nesting level, that sibling's child
// indentation is the correct indent for the new key. If no sibling has
// children, it derives the indentation step from the grandparent's key-to-child
// delta and adds it to the parent key's own indentation. The final fallback
// assumes a 2-space step per level.
func (d *doc) deriveIndent(path []string) int {
	if len(path) < 3 {
		return (len(path) - 1) * 2
	}
	gp, gpKey, _, gpExists, gpErr := d.resolveValue(path[:len(path)-2])
	if gpErr == nil && gpExists && gp != nil && gp.Kind == yaml.MappingNode {
		// A sibling whose value is a populated mapping reveals the child
		// indentation directly.
		for j := 0; j+1 < len(gp.Content); j += 2 {
			v := gp.Content[j+1]
			if v != nil && v.Kind == yaml.MappingNode && len(v.Content) > 0 {
				if fc := v.Content[0]; fc != nil {
					return d.lineIndent(d.offset(fc.Line, 1))
				}
			}
		}
		// No sibling has children. Derive the step from the grandparent's
		// own key-to-child indentation delta and apply it to the parent key.
		_, pKey, _, pExists, _ := d.resolveValue(path[:len(path)-1])
		if pExists && pKey != nil && gpKey != nil &&
			len(gp.Content) > 0 && gp.Content[0] != nil {
			pIndent := d.lineIndent(d.offset(pKey.Line, 1))
			gpIndent := d.lineIndent(d.offset(gpKey.Line, 1))
			childIndent := d.lineIndent(d.offset(gp.Content[0].Line, 1))
			if step := childIndent - gpIndent; step > 0 {
				return pIndent + step
			}
		}
	}
	return (len(path) - 1) * 2
}

// insertKey inserts a new "key: value" (possibly multi-line) entry at path. The
// entry is inserted as the last child of the parent mapping (so key order is
// extended, not reordered) at the parent's child indentation, on its own line.
func (d *doc) insertKey(raw []byte, path []string, value []byte) ([]byte, error) {
	// Ensure every ancestor up to (but excluding) the final segment exists.
	for i := 1; i < len(path); i++ {
		if _, _, _, exists, err := d.resolveValue(path[:i]); err != nil {
			return nil, err
		} else if !exists {
			// Create the intermediate mapping as an empty "key:" line, then
			// re-parse so subsequent inserts resolve against updated bytes.
			out, err := d.insertKey(raw, path[:i], nil)
			if err != nil {
				return nil, err
			}
			raw = out
			d2, err := parseDocMode(raw, d.scoped)
			if err != nil {
				return nil, err
			}
			*d = *d2
		}
	}
	key := path[len(path)-1]
	ind := d.indentForInsert(path)
	parentLine := d.parentInsertLine(path[:len(path)-1])
	insertAt := d.endOfLineWithNewline(parentLine)
	pad := strings.Repeat(" ", ind)
	rendered := pad + key + ":"
	if len(value) > 0 {
		rendered += " " + string(value)
	}
	if d.finalLineUnterminated(raw, insertAt) {
		// The insertion point is the document's unterminated final line;
		// terminate it so the new entry starts on its own line.
		rendered = string(d.newline) + rendered
	}
	rendered += string(d.newline)
	return splice(raw, insertAt, insertAt, []byte(rendered)), nil
}

// parentInsertLine returns the line number after which a new child key under
// parentPath should be inserted. For a present parent mapping it is the last
// line occupied by the parent's existing children (so the new key becomes the
// final sibling). For the document root it is the last content line.
func (d *doc) parentInsertLine(parentPath []string) int {
	if len(parentPath) == 0 {
		// Insert after the last line of the document body.
		return len(d.lines) - 1
	}
	val, _, _, exists, err := d.resolveValue(parentPath)
	if err != nil || !exists || val == nil {
		return len(d.lines) - 1
	}
	return d.valueBlockEnd(val)
}

// valueBlockEnd returns the last line occupied by val's source region: for a
// scalar, the end of its value span (a multiline plain, quoted, or block
// scalar may occupy several lines that yaml.v3 does not report); for mappings
// and sequences, the last line of any descendant.
func (d *doc) valueBlockEnd(val *yaml.Node) int {
	lastLine := val.Line
	for _, c := range allNodes(val) {
		if c == nil {
			continue
		}
		line := c.Line
		if c.Kind == yaml.ScalarNode {
			if _, end := d.valueSpan(c); end > d.offset(line, 1) {
				if le := d.lineOf(end); le > line {
					line = le
				}
			}
		}
		if line > lastLine {
			lastLine = line
		}
	}
	return lastLine
}

// lineOf returns the 1-based line containing byte offset o.
func (d *doc) lineOf(o int) int {
	line := 1
	for line+1 < len(d.lines) && d.lines[line+1] <= o {
		line++
	}
	return line
}

// allNodes returns val and every descendant node (depth-first).
func allNodes(val *yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		out = append(out, n)
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(val)
	return out
}

// emitScalarValue renders the new value v using the quoting style of the
// existing node n, so a quoted scalar stays quoted and a plain scalar stays
// plain.
func emitScalarValue(n *yaml.Node, v string) string {
	switch n.Style {
	case yaml.SingleQuotedStyle:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case yaml.DoubleQuotedStyle:
		// Escape backslashes and double quotes for double-quoted style.
		r := strings.ReplaceAll(v, "\\", "\\\\")
		r = strings.ReplaceAll(r, "\"", "\\\"")
		return "\"" + r + "\""
	default:
		return encodeScalar(v, 0)
	}
}

// encodeScalar renders v as a plain scalar when it is safe, otherwise as a
// single-quoted scalar. A value that would be misread as another YAML type
// (bool, null, number, or containing a special indicator) is quoted. ind is the
// indentation hint (unused for inline scalar values).
func encodeScalar(v string, ind int) string {
	if needsQuotes(v) {
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return v
}

// needsQuotes reports whether v must be quoted to preserve its exact string
// meaning in YAML.
func needsQuotes(v string) bool {
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", ".nan", ".inf", "-.inf", "+.inf":
		return true
	}
	// Leading indicators that YAML would treat specially.
	switch v[0] {
	case '!', '&', '*', '-', '?', ':', '>', '|', '%', '@', '`', '"', '\'', '#', ',', '[', ']', '{', '}':
		return true
	}
	// Looks like a number.
	if looksNumeric(v) {
		return true
	}
	// Contains characters that break inline plain scalars.
	if strings.ContainsAny(v, "\n#:{}[],&*?|>%@`") {
		return true
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' {
		return true
	}
	return false
}

// looksNumeric is a conservative check for integer/float-like strings.
func looksNumeric(v string) bool {
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

// isValueTerminator reports whether a byte ends an inline plain scalar value.
// It is only used for the non-scalar generic fingerprint path: real scalar
// spans are computed by blockPlainSpan/flowPlainSpan, which treat '#' the way
// yaml.v3 does (a comment begins only at the value start or after
// whitespace).
func isValueTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', '[', ']', '{', '}':
		return true
	}
	return false
}

// finalLineUnterminated reports whether an insertion at offset would be glued
// onto the document's final line because that line has no terminator. An
// entry inserted there must emit the document's newline first so it starts on
// its own line.
func (d *doc) finalLineUnterminated(raw []byte, offset int) bool {
	return offset == len(raw) && len(raw) > 0 && raw[len(raw)-1] != '\n'
}

// splice returns raw with bytes [start,end) replaced by rep.
func splice(raw []byte, start, end int, rep []byte) []byte {
	if start < 0 {
		start = 0
	}
	if end > len(raw) {
		end = len(raw)
	}
	if start > end {
		start = end
	}
	out := make([]byte, 0, len(raw)-(end-start)+len(rep))
	out = append(out, raw[:start]...)
	out = append(out, rep...)
	out = append(out, raw[end:]...)
	return out
}

// sequenceBlock renders items as a block sequence, one per line, each at the
// given indentation with "- " prefix, using nl as the line terminator. The
// first item begins immediately (no leading newline) so the result can be
// spliced in place of an existing list body.
func sequenceBlock(items []string, ind int, nl string) []byte {
	pad := strings.Repeat(" ", ind)
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString(nl)
		}
		b.WriteString(pad)
		b.WriteString("- ")
		b.WriteString(encodeScalar(it, ind))
	}
	return []byte(b.String())
}

// FingerprintManaged computes the SHA-256 hash of only the managed spans of raw
// addressed by paths. Managed spans are the exact value bytes of the scalar or
// sequence at each path (including quotes for quoted scalars). Missing paths
// contribute an empty span. The hash is order-independent: paths are sorted
// before hashing so two files with the same managed values (in any key order)
// share a fingerprint. This feeds drift detection.
func FingerprintManaged(raw []byte, paths [][]string) ([32]byte, error) {
	d, err := parseDoc(raw)
	if err != nil {
		return [32]byte{}, err
	}
	sorted := make([][]string, len(paths))
	copy(sorted, paths)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Join(sorted[i], ".") < strings.Join(sorted[j], ".")
	})
	h := sha256.New()
	for _, p := range sorted {
		val, _, _, exists, err := d.resolveValue(p)
		if err != nil {
			return [32]byte{}, err
		}
		fmt.Fprintf(h, "%s=", strings.Join(p, "."))
		if !exists || val == nil {
			h.Write([]byte("\x00"))
			continue
		}
		switch val.Kind {
		case yaml.ScalarNode:
			start, end := d.valueSpan(val)
			h.Write(raw[start:end])
		case yaml.SequenceNode:
			start, end := d.sequenceSpan(val)
			h.Write(raw[start:end])
		default:
			start, end := d.valueSpan(val)
			h.Write(raw[start:end])
		}
		h.Write([]byte("\n"))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
