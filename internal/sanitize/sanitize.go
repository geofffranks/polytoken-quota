// Package sanitize contains shared output-boundary sanitizers for identifiers and
// other low-risk diagnostic values.
package sanitize

import "unicode/utf8"

const maxIdentifierBytes = 256

// Identifier returns value when it is a bounded ASCII identifier composed only
// of letters, digits, '.', '_', '-', and '/'. Invalid, empty, control-bearing,
// or overlong values become the safe diagnostic placeholder.
func Identifier(value string) string {
	if value == "" || len(value) > maxIdentifierBytes || !utf8.ValidString(value) {
		return "<invalid>"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return "<invalid>"
	}
	return value
}
