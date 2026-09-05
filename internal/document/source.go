// Package document implements targeted, byte-preserving editors for the
// polytoken-quota reconciler. It applies exact key-level edits to YAML
// (config.yaml) and Markdown-frontmatter (facet/subagent definition) files
// while preserving every unmanaged byte: comments, key order, quoting style,
// sibling keys, line endings (LF/CRLF), a UTF-8 BOM, and Markdown bodies.
//
// The editors never round-trip an entire document through a generic serializer.
// Instead they use gopkg.in/yaml.v3 to build a syntax tree whose nodes carry
// 1-based line/column positions, map those positions back to exact byte spans
// in the original text, and perform minimal byte-level replacement,
// insertion, or removal. Ambiguous structures — duplicate keys, YAML
// aliases/anchors, and merge keys — are refused rather than silently
// round-tripped.
//
// This package is standalone: staging translates the abstract
// reconcile.FieldEdit into a document.Edit. FingerprintManaged hashes only the
// managed spans of a file and feeds drift detection.
package document

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"gopkg.in/yaml.v3"
)

// Kind classifies a managed edit's value representation.
type Kind uint8

const (
	// Scalar edits replace a single string value (e.g. defaults.full,
	// polytoken.model, autonomous_permission_matcher.classifier_model).
	Scalar Kind = iota
	// Sequence edits replace an ordered list (polytoken.fallback_models).
	Sequence
	// Boolean edits replace a single bool value (models.<name>.enabled).
	Boolean
)

// Edit is one byte-level managed change addressed at a key path. Exactly one of
// Scalar, Sequence, or Bool carries the new value; Remove signals that the key
// (and its value span, or, for a list, its block) should be deleted entirely.
type Edit struct {
	Path     []string
	Kind     Kind
	Scalar   *string
	Sequence []string
	Bool     *bool
	Remove   bool
}

// Source holds the raw bytes, filesystem mode, detected newline, and (for
// frontmatter documents) the byte bounds of the YAML frontmatter block. It is
// produced by staging when materializing a candidate file and is consumed when
// applying edits; the editors here also accept raw bytes directly.
type Source struct {
	Bytes            []byte
	Mode             fs.FileMode
	Newline          []byte // "\n", "\r\n", or "\n" when unknown
	FrontmatterStart int    // byte offset of the opening "---" line, inclusive
	FrontmatterEnd   int    // byte offset just past the closing "---" line
}

// editError is a typed error for ordinary editor failures: parse errors,
// missing values, wrong-kind paths, and refused (but structurally valid) edits
// such as flow-style sequences. It is NOT structural ambiguity; use
// IsAmbiguous to distinguish genuine ambiguity (duplicate keys, anchors/aliases,
// merge keys), which is reported as *ambiguousError.
type editError struct{ msg string }

func (e *editError) Error() string { return e.msg }

// newError returns a structural edit error wrapping msg.
func newError(format string, args ...any) error {
	return &editError{msg: fmt.Sprintf(format, args...)}
}

// ambiguousError marks a structurally ambiguous document that the editor refused
// to touch: duplicate keys, anchors/aliases, or merge keys.
type ambiguousError struct{ msg string }

func (e *ambiguousError) Error() string { return e.msg }

// newAmbiguousError returns an error classifying the document as structurally
// ambiguous.
func newAmbiguousError(format string, args ...any) error {
	return &ambiguousError{msg: fmt.Sprintf(format, args...)}
}

// IsAmbiguous reports whether err indicates a structurally ambiguous document
// (duplicate keys, anchors/aliases, or merge keys) that the editor refused to
// touch. Ordinary failures — parse errors, missing values, wrong-kind paths,
// and refused-but-valid structures such as flow-style sequences — are NOT
// ambiguous and return false here. Callers use this to surface a policy/source
// error rather than silently round-tripping.
func IsAmbiguous(err error) bool {
	var ae *ambiguousError
	return errors.As(err, &ae)
}

// doc is the parsed representation of a YAML byte slice: the yaml.v3 node tree
// and the byte offset of each 1-based line start. Line offsets account for a
// leading UTF-8 BOM so that a (line, column) pair maps to the correct byte
// even when the document begins with a BOM. When scoped is set, parse-time
// whole-tree ambiguity validation is skipped in favor of per-edit-path
// validation (validateEditPath), tolerating anchors and other ambiguity that
// does not involve the edited path.
type doc struct {
	root    *yaml.Node
	lines   []int // lines[line] = byte offset of that line's first byte (1-based)
	raw     []byte
	newline []byte
	scoped  bool
}

// utf8BOM is the 3-byte UTF-8 byte-order mark.
const utf8BOM = "\xef\xbb\xbf"

// parseDoc parses raw YAML into a doc, recording line offsets (BOM-aware) and
// detecting a leading BOM. The first parse runs yaml.v3 over raw; a second
// validation pass over the node tree rejects ambiguous structures. Strict
// (whole-tree) validation is the default; see parseDocMode.
func parseDoc(raw []byte) (*doc, error) {
	return parseDocMode(raw, false)
}

// parseDocMode parses raw YAML like parseDoc. When scopedAmbiguity is false the
// whole tree is validated up front and any anchor, alias, merge key, or
// duplicate key refuses the document. When it is true, the up-front pass is
// skipped; callers validate ambiguity per edit path via validateEditPath, so
// ambiguity far from the edited path is tolerated.
func parseDocMode(raw []byte, scopedAmbiguity bool) (*doc, error) {
	var root yaml.Node
	dec := yaml.NewDecoder(newReader(raw))
	dec.KnownFields(false)
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, newError("document: empty input")
		}
		return nil, newError("document: parse: %v", err)
	}
	d := &doc{
		root:    &root,
		raw:     raw,
		newline: detectNewline(raw),
		lines:   lineOffsets(raw),
		scoped:  scopedAmbiguity,
	}
	if !scopedAmbiguity {
		if err := d.validate(d.root); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// lineOffsets maps each 1-based line number to the byte offset of its first
// byte. When the document begins with a UTF-8 BOM, line 1 starts after the
// 3-byte BOM so that yaml.v3's (line, column) positions (which ignore the BOM
// on line 1) resolve to the correct byte. Only '\n' terminates a line; a
// preceding '\r' remains part of the previous line, which is correct for CRLF.
func lineOffsets(raw []byte) []int {
	start := 0
	if len(raw) >= 3 && string(raw[:3]) == utf8BOM {
		start = 3
	}
	lines := []int{0, start} // index 0 unused
	for i := start; i < len(raw); i++ {
		if raw[i] == '\n' {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// offset maps a 1-based (line, column) to a byte offset within d.raw, using the
// recorded (BOM-aware) line starts. Column is 1-based and counts bytes from the
// start of the line.
func (d *doc) offset(line, column int) int {
	if line < 1 {
		line = 1
	}
	if line >= len(d.lines) {
		line = len(d.lines) - 1
	}
	off := d.lines[line] + column - 1
	if off > len(d.raw) {
		off = len(d.raw)
	}
	return off
}

// validate walks the node tree and refuses structurally ambiguous forms:
// aliases/anchors (which round-trip non-locally) and merge keys (<<). Duplicate
// mapping keys are detected in findValue via a dedicated scan because yaml.v3
// keeps both entries in Content rather than erroring.
func (d *doc) validate(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.AliasNode:
		return newAmbiguousError("document: alias node (*%s) is ambiguous", aliasName(d.raw, n))
	}
	if n.Anchor != "" {
		return newAmbiguousError("document: anchor &%s is ambiguous", n.Anchor)
	}
	if n.Kind == yaml.MappingNode {
		if err := checkDuplicateKeys(n); err != nil {
			return err
		}
	}
	for _, c := range n.Content {
		if err := d.validate(c); err != nil {
			return err
		}
	}
	return nil
}

// aliasName extracts the alias target name for a diagnostic by scanning the
// raw bytes at the alias position.
func aliasName(raw []byte, n *yaml.Node) string {
	off := -1
	if n.Line >= 1 {
		// Approximate; column may be off by BOM on line 1 but this is only a
		// diagnostic.
		off = bestOffset(raw, n.Line, n.Column)
	}
	if off < 0 || off >= len(raw) || raw[off] != '*' {
		return "?"
	}
	end := off + 1
	for end < len(raw) && !isYAMLSpace(raw[end]) {
		end++
	}
	return string(raw[off+1 : end])
}

// bestOffset is a non-BOM-aware offset used only for diagnostics.
func bestOffset(raw []byte, line, column int) int {
	start := 0
	if len(raw) >= 3 && string(raw[:3]) == utf8BOM {
		start = 3
	}
	cur := 1
	pos := start
	for pos < len(raw) && cur < line {
		if raw[pos] == '\n' {
			cur++
		}
		pos++
	}
	return pos + column - 1
}

func isYAMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == ',' || b == ']' || b == '}'
}

// checkDuplicateKeys scans a mapping node's key/value pairs and errors on any
// repeated key. yaml.v3 retains duplicate keys as successive Content pairs
// rather than treating them as an error, so this is required to honor the
// refusal contract.
func checkDuplicateKeys(m *yaml.Node) error {
	seen := make(map[string]int, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i]
		val := m.Content[i+1]
		if key == nil || key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value == "<<" {
			return newAmbiguousError("document: merge key (<<) is ambiguous")
		}
		// yaml.v3 retains duplicate keys as successive Content pairs rather than
		// erroring, so detect textual duplicates explicitly.
		if _, dup := seen[key.Value]; dup {
			return newAmbiguousError("document: duplicate key %q", key.Value)
		}
		if val != nil && val.Kind == yaml.AliasNode {
			return newAmbiguousError("document: alias value for key %q is ambiguous", key.Value)
		}
		seen[key.Value] = i
	}
	return nil
}
