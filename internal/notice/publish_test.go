package notice

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolvePathPrefersConfigured: a configured notice path is used verbatim.
func TestResolvePathPrefersConfigured(t *testing.T) {
	got, err := ResolvePath("/custom/notice.json")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != "/custom/notice.json" {
		t.Fatalf("ResolvePath = %q, want configured path", got)
	}
}

// TestResolvePathDefault: an empty configured path resolves to the default
// under the user's home directory.
func TestResolvePathDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	want := filepath.Join(home, ".local", "polytoken-quota", "notice.json")
	if got != want {
		t.Fatalf("ResolvePath = %q, want %q", got, want)
	}
}

// TestPublishWritesAtomicallyAndRestrictively: Publish creates the parent
// directory, writes the data, leaves no temp files, and uses restrictive
// modes. A second Publish over the same path replaces the content.
func TestPublishWritesAtomicallyAndRestrictively(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notices", "notice.json")

	if err := Publish(path, []byte(`{"revision":1}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"revision":1}` {
		t.Fatalf("content = %q, want notice JSON", data)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("notice mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("notice dir mode = %v, want 0700", di.Mode().Perm())
	}

	if err := Publish(path, []byte(`{"revision":2}`)); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != `{"revision":2}` {
		t.Fatalf("content after second publish = %q, want revision 2", data)
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("expected no leftover temp files, found %d entries", len(entries))
	}
}

// TestPublishFailurePropagates: a Publish to an unwritable target returns an
// error rather than silently succeeding.
func TestPublishFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	// A path held open by a directory we then make read-only is hard to force on
	// root; instead write into a location that cannot exist as a file because a
	// parent is a regular file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	bad := filepath.Join(blocker, "nested", "notice.json")
	if err := Publish(bad, []byte("{}")); err == nil {
		t.Fatalf("Publish into a file-as-directory should fail, got nil")
	}
}
