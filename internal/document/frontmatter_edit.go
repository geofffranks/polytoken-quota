package document

import (
	"bytes"
	"strings"
)

// EditFrontmatter applies edits to the YAML frontmatter block of a Markdown
// document while leaving the Markdown body byte-for-byte unchanged. The
// frontmatter is the YAML between an opening "---" line at the very start of
// the document (optionally preceded only by a UTF-8 BOM) and the next "---"
// line. Only the bytes of that YAML block are edited; everything from the
// closing "---" onward, including the body and any later "---" dividers, is
// preserved exactly.
//
// If no frontmatter block is present the function returns an error rather than
// synthesizing one.
func EditFrontmatter(raw []byte, edits []Edit) ([]byte, error) {
	fmStart, fmEnd, bodyStart, ok := locateFrontmatter(raw)
	if !ok {
		return nil, newError("document: no YAML frontmatter block found")
	}
	// The frontmatter YAML content lies in (fmStart, fmEnd) i.e. between the
	// opening and closing delimiter lines. We parse and edit a copy that begins
	// at column 1 of the first content line.
	block := raw[fmStart:fmEnd]
	out, err := EditYAML(block, edits)
	if err != nil {
		return nil, err
	}
	// Reassemble: opening delimiter + body-of-edits + closing delimiter + body.
	var b bytes.Buffer
	b.Write(raw[:fmStart])     // BOM (if any) + opening "---\n"
	b.Write(out)               // edited YAML block (ends with a newline)
	b.Write(raw[fmEnd:bodyStart]) // closing "---\n"
	b.Write(raw[bodyStart:])   // untouched Markdown body
	return b.Bytes(), nil
}

// locateFrontmatter finds the frontmatter block. It returns:
//   - fmStart: byte offset of the first YAML content line after the opening "---";
//   - fmEnd:   byte offset of the closing "---" line (start of that line);
//   - bodyStart: byte offset of the first body line after the closing "---";
//   - ok:      false if there is no valid frontmatter block.
//
// A BOM may precede the opening delimiter; otherwise the document must begin
// with "---" on the first line.
func locateFrontmatter(raw []byte) (fmStart, fmEnd, bodyStart int, ok bool) {
	pos := 0
	if bytes.HasPrefix(raw, []byte(utf8BOM)) {
		pos = len(utf8BOM)
	}
	openLine, openLen := lineAt(raw, pos)
	if !isFrontmatterDelimiter(openLine) {
		return 0, 0, 0, false
	}
	contentStart := pos + openLen
	// Scan subsequent lines for the closing "---" delimiter at column 1.
	scan := contentStart
	for scan < len(raw) {
		line, lineLen := lineAt(raw, scan)
		if isFrontmatterDelimiter(line) {
			fmStart = contentStart
			fmEnd = scan
			bodyStart = scan + lineLen
			return fmStart, fmEnd, bodyStart, true
		}
		scan += lineLen
		if lineLen == 0 {
			break
		}
	}
	return 0, 0, 0, false
}

// lineAt returns the line (without its terminator) beginning at offset and the
// total number of bytes consumed (line bytes plus a CRLF or LF terminator).
func lineAt(raw []byte, off int) (line []byte, consumed int) {
	if off > len(raw) {
		return nil, 0
	}
	end := off
	for end < len(raw) && raw[end] != '\n' && raw[end] != '\r' {
		end++
	}
	line = raw[off:end]
	consumed = end - off
	if consumed+off < len(raw) && raw[off+consumed] == '\r' {
		consumed++
	}
	if consumed+off < len(raw) && raw[off+consumed] == '\n' {
		consumed++
	}
	return line, consumed
}

// isFrontmatterDelimiter reports whether line is exactly "---" (optionally with
// trailing whitespace), the frontmatter fence marker.
func isFrontmatterDelimiter(line []byte) bool {
	s := strings.TrimRight(string(line), " \t\r")
	return s == "---"
}
