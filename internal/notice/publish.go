package notice

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultNoticeFile is the notice filename; the directory is the user's
// local polytoken-quota state directory (distinct from the ~/.polytoken-quota
// policy home, so the notice can be mounted into agent containers without
// exposing the policy).
const defaultNoticeFile = "notice.json"

// ResolvePath returns the effective notice file path: the configured path if
// non-empty, otherwise the default under the user's home directory
// (~/.local/polytoken-quota/notice.json).
func ResolvePath(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("notice: resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "polytoken-quota", defaultNoticeFile), nil
}

// Publish atomically writes the notice document to path with restrictive
// permissions, creating parent directories as needed. It writes to a
// same-directory temporary file, fsyncs it, then renames over path, so
// consumers never observe a partially written notice.
func Publish(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("notice: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".notice-*")
	if err != nil {
		return fmt.Errorf("notice: create temp: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("notice: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("notice: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("notice: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("notice: close temp: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("notice: rename: %w", err)
	}
	return nil
}
