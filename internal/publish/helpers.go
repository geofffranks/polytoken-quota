package publish

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"time"

	"github.com/geofffranks/codexbar-hooks/internal/state"
)

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func hexDecode(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func parseMode(m uint32) fs.FileMode { return fs.FileMode(m) }

// toIntended projects a state.TargetState onto the durable journal's intended
// outcome. It carries only the fields needed to reconstruct the committed
// target outcome; no secrets or raw config.
func toIntended(t state.TargetState) intendedTarget {
	out := intendedTarget{
		AttemptedRevision: t.AttemptedRevision,
		AppliedRevision:   t.AppliedRevision,
		AttemptedAtUnix:   t.AttemptedAt.UnixNano(),
		AppliedAtUnix:     t.AppliedAt.UnixNano(),
	}
	pending := t.Pending != nil
	if pending {
		val := true
		out.Pending = &val
		out.Stage = t.Pending.Stage
		out.Summary = t.Pending.Summary
		out.LiveStatus = t.Pending.LiveStatus
	} else {
		val := false
		out.Pending = &val
	}
	return out
}

// fromIntended reconstructs a state.TargetState from the journal's intended
// projection.
func fromIntended(it intendedTarget) state.TargetState {
	t := state.TargetState{
		AttemptedRevision: it.AttemptedRevision,
		AppliedRevision:   it.AppliedRevision,
		AttemptedAt:       unixNanoTime(it.AttemptedAtUnix),
		AppliedAt:         unixNanoTime(it.AppliedAtUnix),
	}
	if it.Pending != nil && *it.Pending {
		t.Pending = &state.ApplyFailure{
			Stage:      it.Stage,
			Summary:    it.Summary,
			LiveStatus: it.LiveStatus,
		}
	}
	return t
}

func unixNanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// sha256OfFile computes the SHA-256 digest of the file at path.
func sha256OfFile(fs DurableFS, path string) ([32]byte, error) {
	if fs == nil {
		fs = OSFS{}
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256Bytes(data), nil
}

// fileMode reports the permission bits of the file at path, or 0600 on error.
func fileMode(fs DurableFS, path string) fs.FileMode {
	info, err := fs.Stat(path)
	if err != nil {
		return 0o600
	}
	return info.Mode().Perm()
}

// ensureNoSymlink rejects path if it (or any parent up to root) is a symlink,
// defending against TOCTOU path escape immediately before a live-file write. It
// returns an error if path is a symlink or escapes root after lexical cleanup.
func ensureNoSymlink(root, path string) error {
	cleanRoot, err := filepathAbs(root)
	if err != nil {
		return err
	}
	cleanPath, err := filepathAbs(path)
	if err != nil {
		return err
	}
	if !isWithin(cleanRoot, cleanPath) {
		return ErrPathEscape
	}
	return walkNoSymlink(cleanRoot, cleanPath)
}

// errStep wraps a filesystem error with the durable-step name for diagnosis.
func errStep(step string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("publish: %s: %w", step, err)
}
