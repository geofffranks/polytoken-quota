//go:build darwin || linux

package target

import (
	"errors"
	"io"
	"os"
)

// readResolvedDefinition opens the policy-relative path through descriptor-
// backed os.Root containment, then verifies that the opened descriptors still
// identify the exact root and file approved by Resolve before reading from the
// file descriptor. Concurrent renames or symlink swaps cannot redirect the read.
func readResolvedDefinition(definition ResolvedDefinition) ([]byte, error) {
	root, err := os.OpenRoot(definition.canonicalRoot)
	if err != nil {
		return nil, errors.New("open root failed")
	}
	defer root.Close()

	currentRootInfo, err := root.Stat(".")
	if err != nil || definition.rootInfo == nil || !os.SameFile(definition.rootInfo, currentRootInfo) {
		return nil, errors.New("root changed")
	}

	file, err := root.Open(definition.PolicyPath)
	if err != nil {
		return nil, errors.New("anchored open failed")
	}
	defer file.Close()

	currentFileInfo, err := file.Stat()
	if err != nil || definition.fileInfo == nil || !os.SameFile(definition.fileInfo, currentFileInfo) {
		return nil, errors.New("definition changed")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.New("read failed")
	}
	return raw, nil
}
