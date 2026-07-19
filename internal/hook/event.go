// Package hook decodes CodexBar hook events delivered on stdin.
//
// This file declares the minimal Event type and Decode used by the CLI shell.
// Task 3 extends Decode to merge the supported CODEXBAR_* environment snapshot
// and to enforce the byte-size limit and validation.
package hook

import (
	"encoding/json"
	"io"
)

// Type is the CodexBar hook event type (e.g. "quota_low").
type Type string

// Event is a decoded CodexBar hook event. The optional pointer fields are
// populated only when CodexBar includes them on the wire.
type Event struct {
	Type      Type    `json:"event"`
	Provider  string  `json:"provider"`
	Timestamp *string `json:"timestamp,omitempty"`
}

// Decode reads a single hook event from r. The env snapshot and maxBytes guard
// are accepted here for interface stability; Task 3 wires env merging and the
// size limit. For now it performs a plain JSON unmarshal into Event.
func Decode(r io.Reader, env map[string]string, maxBytes int64) (Event, error) {
	var e Event
	if err := json.NewDecoder(r).Decode(&e); err != nil {
		return Event{}, err
	}
	return e, nil
}
