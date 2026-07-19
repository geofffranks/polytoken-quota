package document

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EditYAML applies edits to raw YAML bytes using parser-assisted, exact span
// location and minimal byte-level replacement/insertion/removal. It never
// round-trips the document through a generic serializer. Every byte outside the
// addressed spans — comments, key order, quoting, siblings, line endings, and a
// leading BOM — is preserved. Ambiguous structures (duplicate keys,
// aliases/anchors, merge keys) are refused with a structural error; see
// IsAmbiguous.
//
// Edits are applied in order; later edits operate on the bytes produced by
// earlier ones, so each edit's path is resolved against the current (possibly
// already-edited) text. Callers that want deterministic results for a set of
// independent edits should pass them in a stable order.
func EditYAML(raw []byte, edits []Edit) ([]byte, error) {
	for _, e := range edits {
		out, err := applyOne(raw, e)
		if err != nil {
			return nil, err
		}
		raw = out
	}
	return raw, nil
}

// applyOne parses raw fresh, resolves e.Path, and performs the single edit.
// Re-parsing each edit keeps span offsets correct after prior mutations.
func applyOne(raw []byte, e Edit) ([]byte, error) {
	if len(e.Path) == 0 {
		return nil, newError("document: edit with empty path")
	}
	d, err := parseDoc(raw)
	if err != nil {
		return nil, err
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
// indentation (or the document's for a top-level key).
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
	if val == nil || val.Kind != yaml.ScalarNode || val.Tag != "!!bool" {
		// Allow editing a bool that happens to be untyped-tagged; still replace
		// the span. But refuse sequences/mappings.
		if val != nil && (val.Kind == yaml.MappingNode || val.Kind == yaml.SequenceNode) {
			return nil, newError("document: %q is not a boolean", strings.Join(path, "."))
		}
	}
	start, end := d.valueSpan(val)
	rep := "false"
	if v {
		rep = "true"
	}
	return splice(raw, start, end, []byte(rep)), nil
}

// setSequence replaces the sequence at path with the items. It rewrites the
// whole list block in place using the same dash style and indentation as the
// existing list, or inserts a new block list at the parent's indentation when
// the key is absent. An empty list removes the key (matching the reconciler's
// Remove semantics for fallback_models).
func (d *doc) setSequence(raw []byte, path []string, items []string) ([]byte, error) {
	val, _, _, exists, err := d.resolveValue(path)
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
		return nil, newError("document: %q is not a sequence", strings.Join(path, "."))
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
			d2, err := parseDoc(raw)
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
	val, keyNode, _, exists, err := d.resolveValue(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return raw, nil
	}
	if keyNode == nil {
		return nil, newError("document: cannot locate key line for %q", strings.Join(path, "."))
	}
	lineStart := d.offset(keyNode.Line, 1)
	// End of the removed region: end of the last line occupied by the value.
	lastLine := val.Line
	if val.Content != nil {
		for _, c := range allNodes(val) {
			if c != nil && c.Line > lastLine {
				lastLine = c.Line
			}
		}
	}
	// Erase from lineStart up to and including the newline that ends lastLine.
	lineEnd := d.endOfLineWithNewline(lastLine)
	return splice(raw, lineStart, lineEnd, nil), nil
}

// valueSpan returns the byte range [start,end) covering a scalar value node,
// including its surrounding quotes for quoted scalars. yaml.v3 reports the
// node column at the opening quote for single/double-quoted scalars and at the
// first value byte for plain scalars, so unquoted values need no adjustment.
func (d *doc) valueSpan(n *yaml.Node) (int, int) {
	start := d.offset(n.Line, n.Column)
	if n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle {
		// start already points at the opening quote.
		quote := d.raw[start]
		end := start + 1
		for end < len(d.raw) {
			if d.raw[end] == quote {
				end++
				break
			}
			end++
		}
		return start, end
	}
	// Plain scalar: span the run of non-space, non-newline bytes from start.
	end := start
	for end < len(d.raw) && !isValueTerminator(d.raw[end]) {
		end++
	}
	return start, end
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

// indentForInsert returns the indentation a new key under the parent of path
// should use: one level (2 spaces) deeper than the parent mapping, or 0 for a
// top-level key. Polytoken config uses 2-space indentation throughout.
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
	if parent.Kind != yaml.MappingNode || len(parent.Content) == 0 {
		return (len(path) - 1) * 2
	}
	firstKey := parent.Content[0]
	if firstKey == nil {
		return (len(path) - 1) * 2
	}
	return d.lineIndent(d.offset(firstKey.Line, 1))
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
			d2, err := parseDoc(raw)
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
	lastLine := val.Line
	for _, c := range allNodes(val) {
		if c != nil && c.Line > lastLine {
			lastLine = c.Line
		}
	}
	return lastLine
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
func isValueTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '#', ',', '[', ']', '{', '}':
		return true
	}
	return false
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
