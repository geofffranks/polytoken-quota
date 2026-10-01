package service

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// preflightRecheckFixture builds a private staging root with one source file
// and returns its bytes-backed snapshot holder plus the file path.
func preflightRecheckFixture(t *testing.T) (ProviderPreflight, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ProviderPreflight{Snapshots: []ProviderSourceSnapshot{
		{TargetID: "global", Path: path, SHA256: sha256.Sum256(data)},
	}}, path
}

func TestRecheckProviderPreflightAcceptsUnchangedSources(t *testing.T) {
	snapshots, _ := preflightRecheckFixture(t)
	if err := RecheckProviderPreflight(snapshots); err != nil {
		t.Fatalf("unchanged source refused: %v", err)
	}
}

func TestRecheckProviderPreflightDetectsStaleSource(t *testing.T) {
	snapshots, path := preflightRecheckFixture(t)
	if err := os.WriteFile(path, []byte("version: 4\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecheckProviderPreflight(snapshots); err == nil {
		t.Fatal("stale source snapshot was accepted")
	}
}

func TestRecheckProviderPreflightDetectsRemovedSource(t *testing.T) {
	snapshots, path := preflightRecheckFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := RecheckProviderPreflight(snapshots); err == nil {
		t.Fatal("removed source snapshot was accepted")
	}
}
