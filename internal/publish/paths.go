package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned when a managed path escapes its root after
// canonicalization, or when a managed file is a symlink.
var ErrPathEscape = errors.New("publish: managed path escapes root or is a symlink")

func errEscape(path string) error { return ErrPathEscape }

// filepathAbs returns the absolute, cleaned form of path.
func filepathAbs(path string) (string, error) {
	return filepath.Abs(filepath.Clean(path))
}

// isWithin reports whether path is equal to or nested lexically beneath root.
// Both must be cleaned absolute paths.
func isWithin(root, path string) bool {
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "" {
		return true
	}
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return false
	}
	// filepath.Rel of a sibling outside root begins with "..".
	return !strings.HasPrefix(filepath.ToSlash(rel), "../")
}

// walkNoSymlink walks each path component from root to target and returns
// ErrPathEscape if any segment along the way is a symlink. This is the TOCTOU
// defense: a path that was clean at validation time may have been replaced by a
// symlink immediately before write.
func walkNoSymlink(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	cur := root
	segments := strings.Split(rel, "/")
	for i, seg := range segments {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Intermediate missing segment is fine: the writer will create
				// it; what matters is that no existing prefix is a symlink.
				continue
			}
			return err
		}
		isLast := i == len(segments)-1
		if info.Mode()&os.ModeSymlink != 0 {
			// A symlink on the path is only tolerated when it is a final target
			// that the publisher will replace wholesale? No: managed files must
			// never be symlinks. Reject in all cases.
			_ = isLast
			return ErrPathEscape
		}
	}
	return nil
}
