package service

// Routing enable/disable: byte-preserving, atomic edits to desired.yaml that
// change ONLY the top-level routing.enabled field. The raw bytes are edited with
// the document package (comments, key order, quoting, and unmanaged content are
// preserved) and written back with an atomic temp+rename+fsync. This is NOT a
// whole-file reserialization.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/geofffranks/polytoken-quota/internal/document"
)

// RoutingWriteError reports a write failure and whether the destination was
// already replaced. Mutated is true only for post-rename durability failures.
type RoutingWriteError struct {
	Err     error
	Mutated bool
}

func (e *RoutingWriteError) Error() string { return e.Err.Error() }
func (e *RoutingWriteError) Unwrap() error { return e.Err }

// SetRoutingEnabled toggles the routing.enabled field in desired.yaml. It edits
// only that field: every other byte (comments, key order, quoting, unmanaged
// policy content) is preserved. When the desired file is absent it returns a
// clear error rather than creating one. Enabling sets routing.enabled: true;
// disabling sets it to false. The operation is idempotent: setting the field to
// its current value is a successful no-op edit.
//
// It performs no reconcile: it only sets intent; the next reconcile applies it.
func (c *Coordinator) SetRoutingEnabled(ctx context.Context, enabled bool) error {
	var unlock func() error
	if c.Lock != nil {
		var err error
		unlock, err = c.Lock.Lock(ctx)
		if err != nil {
			return fmt.Errorf("routing: acquire lock: %w", err)
		}
		defer func() { _ = unlock() }()
	}
	if c.Policy == nil {
		return errors.New("routing: policy loader unavailable")
	}
	desiredPath, ok := policyDesiredPath(c.Policy)
	if !ok || desiredPath == "" {
		return errors.New("routing: desired.yaml path unavailable")
	}
	raw, err := os.ReadFile(desiredPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("routing: desired.yaml not found at %s", desiredPath)
		}
		return fmt.Errorf("routing: read desired.yaml: %w", err)
	}
	b := enabled
	edited, err := document.EditYAML(raw, []document.Edit{{
		Path: []string{"routing", "enabled"},
		Kind: document.Boolean,
		Bool: &b,
	}})
	if err != nil {
		return fmt.Errorf("routing: edit routing.enabled: %w", err)
	}
	if err := atomicWriteFile(desiredPath, edited, 0o600); err != nil {
		return fmt.Errorf("routing: write desired.yaml: %w", err)
	}
	return nil
}

// policyDesiredPath extracts the desired.yaml path from a FilePolicyLoader. It
// returns ok=false for a loader that does not expose a path (e.g. a test double).
func policyDesiredPath(loader any) (string, bool) {
	type pather interface{ DesiredPath() string }
	if p, ok := loader.(pather); ok {
		return p.DesiredPath(), true
	}
	return "", false
}

// atomicWriteFile writes data to path via a temp file in the same directory
// (mode perm), fsyncs it, renames it over the target, and fsyncs the directory.
// A crash cannot leave a partially written file.
func atomicWriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".desired.yaml.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	if err := syncDirectory(dir); err != nil {
		return &RoutingWriteError{Err: err, Mutated: true}
	}
	return nil
}

// syncDirectory fsyncs the directory holding a newly written/renamed file.
func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir %s: %w", dir, err)
	}
	return nil
}
