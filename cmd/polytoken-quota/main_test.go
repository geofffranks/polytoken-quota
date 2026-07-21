package main

import (
	"path/filepath"
	"testing"
)

func TestResolveConfigDefaultsToXDGPolytokenDir(t *testing.T) {
	t.Setenv("POLYTOKEN_QUOTA_HOME", "")
	t.Setenv("POLYTOKEN_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "polytoken")
	if cfg.GlobalDir != want {
		t.Fatalf("global dir=%q want %q", cfg.GlobalDir, want)
	}
}

func TestResolveConfigHonorsConfigDirOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom")
	t.Setenv("POLYTOKEN_CONFIG_DIR", override)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GlobalDir != override {
		t.Fatalf("global dir=%q want %q", cfg.GlobalDir, override)
	}
}
