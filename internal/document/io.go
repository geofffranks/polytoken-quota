package document

import (
	"bytes"
	"io"
)

// newReader returns a byte reader over raw. It is a thin wrapper used so the
// YAML decoder reads exact bytes (no transformation) and so call sites stay
// terse.
func newReader(raw []byte) io.Reader { return bytes.NewReader(raw) }

// detectNewline reports the dominant line ending of raw: "\r\n" if any CRLF is
// present, otherwise "\n". The first occurrence is used as the canonical
// newline for any newly inserted lines so that inserted content matches the
// surrounding document.
func detectNewline(raw []byte) []byte {
	if bytes.Index(raw, []byte("\r\n")) >= 0 {
		return []byte("\r\n")
	}
	return []byte("\n")
}
