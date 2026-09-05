package policy

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotConfigDir is the path-free classification returned when a project root
// is neither a Polytoken configuration directory nor a project directory whose
// .polytoken subdirectory is one. Root paths never appear in errors: the
// target layer's sanitization contract treats them as private.
var ErrNotConfigDir = errors.New("root is not a Polytoken configuration directory (no config.yaml in the root or its .polytoken subdirectory)")

// CanonicalProjectRoot resolves a registered project target root to its
// Polytoken configuration directory. A project may be registered either
// directly at its configuration directory (one holding config.yaml) or at the
// project directory, in which case its .polytoken subdirectory is the
// configuration directory and is returned instead. An empty or error result
// leaves the caller's root untouched.
func CanonicalProjectRoot(root string) (string, error) {
	cleaned := filepath.Clean(root)
	if hasConfigYAML(cleaned) {
		return cleaned, nil
	}
	appended := filepath.Join(cleaned, ".polytoken")
	if hasConfigYAML(appended) {
		return appended, nil
	}
	return "", ErrNotConfigDir
}

// hasConfigYAML reports whether dir holds a regular config.yaml file. Symlinked
// configs are accepted here for detection; the strict regular-file rejection
// stays with the layer readers that stage or edit them.
func hasConfigYAML(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "config.yaml"))
	return err == nil && info.Mode().IsRegular()
}
