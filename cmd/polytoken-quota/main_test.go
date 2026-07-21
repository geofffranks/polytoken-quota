package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigRejectsInvalidExplicitPolytokenBinary(t *testing.T) {
	t.Setenv("POLYTOKEN_BINARY", filepath.Join(t.TempDir(), "missing", "polytoken"))
	if _, err := resolveConfig(); err == nil {
		t.Fatal("accepted invalid explicit Polytoken binary")
	}
}

func TestResolveConfigUsesPolytokenFromPATHWhenPinnedPathUnavailable(t *testing.T) {
	t.Setenv("POLYTOKEN_QUOTA_HOME", "")
	t.Setenv("POLYTOKEN_CONFIG_DIR", "")
	t.Setenv("POLYTOKEN_BINARY", "")
	oldDefault := defaultPolytokenBinary
	defaultPolytokenBinary = filepath.Join(t.TempDir(), "missing", "polytoken")
	t.Cleanup(func() { defaultPolytokenBinary = oldDefault })
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "polytoken")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PolytokenBin != bin {
		t.Fatalf("polytoken binary=%q want PATH binary %q", cfg.PolytokenBin, bin)
	}
}

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

func TestLoadPolytokenEnvParsesAndPreservesExportedOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "polytoken.env")
	if err := os.WriteFile(path, []byte("# comment\nFOO=from-file\nexport BAR='quoted value'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadPolytokenEnv(path, []string{"FOO=from-process", "KEEP=value"})
	if err != nil {
		t.Fatal(err)
	}
	if got["FOO"] != "from-process" || got["BAR"] != "quoted value" || got["KEEP"] != "value" {
		t.Fatalf("env=%v", got)
	}
}

func TestLoadPolytokenEnvDoesNotLetEmptyInheritedValueMaskFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "polytoken.env")
	if err := os.WriteFile(path, []byte("CLAUDE_CODE_OAUTH_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadPolytokenEnv(path, []string{"CLAUDE_CODE_OAUTH_TOKEN="})
	if err != nil {
		t.Fatal(err)
	}
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "from-file" {
		t.Fatalf("token env=%q want file value", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}
}

func TestLoadPolytokenEnvRejectsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "polytoken.env")
	if err := os.WriteFile(path, []byte("not-an-assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPolytokenEnv(path, nil); err == nil {
		t.Fatal("accepted malformed env file")
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
