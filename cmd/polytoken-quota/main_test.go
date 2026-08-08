package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPrintVersionIfRequestedPrintsDefaultVersion(t *testing.T) {
	var output bytes.Buffer
	oldVersion := Version
	Version = "dev"
	t.Cleanup(func() { Version = oldVersion })

	if !printVersionIfRequested([]string{"--version"}, &output) {
		t.Fatal("--version was not handled")
	}
	if got, want := output.String(), "polytoken-quota dev\n"; got != want {
		t.Fatalf("version output=%q want %q", got, want)
	}
}

func TestPrintVersionIfRequestedUsesLinkTimeOverrideValue(t *testing.T) {
	var output bytes.Buffer
	oldVersion := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = oldVersion })

	if !printVersionIfRequested([]string{"--version"}, &output) {
		t.Fatal("--version was not handled")
	}
	if got, want := output.String(), "polytoken-quota v1.2.3\n"; got != want {
		t.Fatalf("version output=%q want %q", got, want)
	}
}

func TestPrintVersionIfRequestedIgnoresOtherArguments(t *testing.T) {
	var output bytes.Buffer
	if printVersionIfRequested([]string{"status"}, &output) {
		t.Fatal("non-version arguments were handled as --version")
	}
	if output.Len() != 0 {
		t.Fatalf("unexpected output=%q", output.String())
	}
}

func TestResolveConfigRejectsInvalidExplicitPolytokenBinary(t *testing.T) {
	t.Setenv("POLYTOKEN_BINARY", filepath.Join(t.TempDir(), "missing", "polytoken"))
	if _, err := resolveConfig(); err == nil {
		t.Fatal("accepted invalid explicit Polytoken binary")
	}
}

func TestResolveConfigUsesPolytokenFromPATHWhenNoExplicitBinary(t *testing.T) {
	t.Setenv("POLYTOKEN_QUOTA_HOME", "")
	t.Setenv("POLYTOKEN_CONFIG_DIR", "")
	t.Setenv("POLYTOKEN_BINARY", "")
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
	got, err := loadPolytokenEnv(path, []string{"FOO=from-process", "UNRELATED=value"})
	if err != nil {
		t.Fatal(err)
	}
	if got["FOO"] != "from-process" || got["BAR"] != "quoted value" {
		t.Fatalf("env=%v", got)
	}
	// An inherited variable neither allowlisted nor named in the file must
	// not be forwarded to the validation subprocess.
	if _, leaked := got["UNRELATED"]; leaked {
		t.Fatalf("unrelated inherited variable forwarded: %v", got)
	}
}

// TestLoadPolytokenEnvExcludesInheritedSecrets proves credential-shaped
// inherited variables (cloud tokens, CI secrets, provider keys) never reach
// the validation environment unless explicitly named in polytoken.env, while
// baseline plumbing (PATH) and POLYTOKEN_* variables pass through.
func TestLoadPolytokenEnvExcludesInheritedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "polytoken.env")
	if err := os.WriteFile(path, []byte("OPTED_IN_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inherited := []string{
		"AWS_SECRET_ACCESS_KEY=secret",
		"GITHUB_TOKEN=secret",
		"OPENAI_API_KEY=secret",
		"PATH=/usr/bin",
		"POLYTOKEN_DEBUG=1",
		"OPTED_IN_TOKEN=from-process",
	}
	got, err := loadPolytokenEnv(path, inherited)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY"} {
		if _, leaked := got[key]; leaked {
			t.Fatalf("inherited secret %s forwarded: %v", key, got)
		}
	}
	if got["PATH"] != "/usr/bin" || got["POLYTOKEN_DEBUG"] != "1" {
		t.Fatalf("allowlisted variables missing: %v", got)
	}
	if got["OPTED_IN_TOKEN"] != "from-process" {
		t.Fatalf("file-named variable should keep process value: %v", got)
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
