package contract

// T5 opt-in real-binary contracts for the staging read-allowlist. They run the
// EXACT production validation command pair from internal/validate/runner.go
// against staged candidates built from fixture roots that deliberately contain
// foreign files — proving validation fidelity for the managed surface
// (config.yaml, facets/**/*.md, subagents/**/*.md) and that nothing outside
// the allowlist reaches a staged tree or affects the result.
//
// The production pair, mirroring configValidateArgs/doctorArgs/doctorEnv:
//
//	<bin> --config-dir <ConfigDir> --working-dir <WorkingDir> config validate --user
//	<bin> --working-dir <WorkingDir> doctor
//
// with XDG_CONFIG_HOME=<UserConfigDir> and HOME=<Root>. The shared polyRun /
// isolateEnv helpers are deliberately NOT used here: they pass --config-dir to
// doctor and point XDG_CONFIG_HOME at an empty tree, so doctor would never read
// the staged user tree.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/target"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// allowlistEnv threads the subprocess environment exactly as doctorEnv does:
// the base vars (PATH), then the staged XDG_CONFIG_HOME and HOME. The parent
// environment is never inherited wholesale.
func allowlistEnv(c staging.Candidate) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"XDG_CONFIG_HOME=" + c.UserConfigDir,
		"HOME=" + c.Root,
	}
}

// runRaw executes the binary with exact argv and env, returning combined
// output and exit code. It never spawns a shell.
func runRaw(t *testing.T, bin string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// runProductionPair runs the exact two-command validation pair against a
// candidate, returning both outputs and exit codes.
func runProductionPair(t *testing.T, bin string, c staging.Candidate) (validateOut, doctorOut string, vCode, dCode int) {
	t.Helper()
	env := allowlistEnv(c)
	validateOut, vCode = runRaw(t, bin, env, "--config-dir", c.ConfigDir, "--working-dir", c.WorkingDir, "config", "validate", "--user")
	doctorOut, dCode = runRaw(t, bin, env, "--working-dir", c.WorkingDir, "doctor")
	return validateOut, doctorOut, vCode, dCode
}

// allowlistConfig is the synthetic no_auth config used by the allowlist
// fixtures: fully offline validation and startup loading (same shape as the
// live fixture config).
const allowlistConfig = "providers:\n" +
	"  codex:\n" +
	"    kind:\n" +
	"      type: custom_open_ai_compatible\n" +
	"    url: https://api.codex.test\n" +
	"    auth:\n" +
	"      type: no_auth\n" +
	"  zai:\n" +
	"    kind:\n" +
	"      type: custom_open_ai_compatible\n" +
	"    url: https://api.zai.test\n" +
	"    auth:\n" +
	"      type: no_auth\n" +
	"models:\n" +
	"  sol:\n" +
	"    provider: codex\n" +
	"    provider_name: sol-1\n" +
	"    class: full\n" +
	"    variant: openai\n" +
	"    context_window: 200000\n" +
	"    max_tokens: 100000\n" +
	"    enabled: true\n" +
	"  glm:\n" +
	"    provider: zai\n" +
	"    provider_name: glm-1\n" +
	"    class: full\n" +
	"    variant: openai\n" +
	"    context_window: 200000\n" +
	"    max_tokens: 100000\n" +
	"    enabled: true\n" +
	"defaults:\n" +
	"  full: glm\n"

func allowlistDefinition(name, model string) string {
	return "---\nname: " + name + "\npolytoken:\n  model: " + model + "\n  fallback_models:\n    - sol\ndescription: synthetic\n---\n# Body.\n"
}

// writeAllowlistFixture lays down a config root with the managed surface
// (config.yaml, facets/x.md, subagents/y.md) plus — when foreign is true —
// exactly the foreign material from the real incident shape: a watchdog.env,
// Pushover-style credentials, a skills/ tree, and loose root files.
func writeAllowlistFixture(t *testing.T, dir string, foreign bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(allowlistConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "facets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facets", "x.md"), []byte(allowlistDefinition("x", "glm")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subagents", "y.md"), []byte(allowlistDefinition("y", "glm")), 0o600); err != nil {
		t.Fatal(err)
	}
	if !foreign {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "watchdog.env"), []byte("PUSHOVER_TOKEN=synthetic-contract-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"api_key":"synthetic-contract-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "debug"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "debug", "SKILL.md"), []byte("# debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt_history"), []byte("history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertForeignAbsent fails if any foreign rel exists in either staged tree.
func assertForeignAbsent(t *testing.T, c staging.Candidate) {
	t.Helper()
	for _, rel := range []string{"watchdog.env", "credentials.json", "skills/debug/SKILL.md", "prompt_history"} {
		for _, tree := range []string{c.ConfigDir, filepath.Join(c.UserConfigDir, "polytoken")} {
			if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Fatalf("foreign file %s present in staged tree %s", rel, tree)
			}
		}
	}
}

// TestAllowlistContractValidatesManagedSurface proves a root that carries the
// managed surface PLUS the real incident's foreign files validates cleanly
// through the exact production command pair, and the foreign files are absent
// from both staged trees.
func TestAllowlistContractValidatesManagedSurface(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("set POLYTOKEN_CONTRACT_BIN (or POLYTOKEN_BIN) for the supported-binary contract")
	}
	globalDir := filepath.Join(t.TempDir(), "global")
	writeAllowlistFixture(t, globalDir, true)
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	c := staged(t, globalDir, "", reconcile.Plan{TargetID: res.ID}, res)
	assertForeignAbsent(t, c)
	// The managed surface is present in the staged tree.
	for _, rel := range []string{"facets/x.md", "subagents/y.md"} {
		if _, err := os.Stat(filepath.Join(c.ConfigDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("managed definition %s missing from staging: %v", rel, err)
		}
	}
	vOut, dOut, vCode, dCode := runProductionPair(t, bin, c)
	if vCode != 0 {
		t.Fatalf("config validate --user failed (exit %d):\n%s", vCode, vOut)
	}
	if dCode != 0 {
		t.Fatalf("doctor failed (exit %d):\n%s", dCode, dOut)
	}
}

// TestAllowlistContractInvalidModelRefFailsDoctor proves the narrowed mirror
// still catches stale model references: a staged subagent referencing an
// unknown model fails doctor, and the sanitized output keeps the model name
// actionable.
func TestAllowlistContractInvalidModelRefFailsDoctor(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("set POLYTOKEN_CONTRACT_BIN (or POLYTOKEN_BIN) for the supported-binary contract")
	}
	globalDir := filepath.Join(t.TempDir(), "global")
	writeAllowlistFixture(t, globalDir, true)
	if err := os.WriteFile(filepath.Join(globalDir, "subagents", "stale.md"),
		[]byte(allowlistDefinition("stale", "phantom-model")), 0o600); err != nil {
		t.Fatal(err)
	}
	res := target.Resolved{ID: "global", CanonicalRoot: globalDir, Global: true}
	c := staged(t, globalDir, "", reconcile.Plan{TargetID: res.ID}, res)
	_, dOut, _, dCode := runProductionPair(t, bin, c)
	if dCode == 0 {
		t.Fatalf("doctor passed with an unknown model reference in a staged subagent:\ndoctor:\n%s", dOut)
	}
	// The failure detail names the unknown model (so pq's summaries and the
	// operator can act on it).
	if !strings.Contains(dOut, "phantom-model") {
		t.Fatalf("doctor output does not name the unknown model:\n%s", dOut)
	}
	// Sanitization yields non-empty, path-safe text. (DefaultSanitize bounds at
	// its 1024-byte head, and doctor's warning preamble can push the error line
	// past that bound — hence no model-name assertion on the sanitized text.)
	sanitized := validate.DefaultSanitize([]byte(dOut))
	if strings.TrimSpace(sanitized) == "" {
		t.Fatal("sanitized doctor output is empty")
	}
	if strings.Contains(sanitized, c.Root) || strings.Contains(sanitized, os.TempDir()) {
		t.Fatalf("sanitized doctor output leaks staging paths:\n%s", sanitized)
	}
}

// TestAllowlistContractCollisionProjectWins pins the merge semantics (D7): the
// same rel path and same logical definition name across the global and project
// layers resolve to the project's file in the staged trees, and the pair still
// validates. This pins static expectation only — resolver parity for same-name
// definitions in different trees is explicitly not claimed.
func TestAllowlistContractCollisionProjectWins(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("set POLYTOKEN_CONTRACT_BIN (or POLYTOKEN_BIN) for the supported-binary contract")
	}
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	projectDir := filepath.Join(root, "project", ".polytoken")
	writeAllowlistFixture(t, globalDir, false)
	if err := os.WriteFile(filepath.Join(globalDir, "subagents", "shared.md"),
		[]byte(allowlistDefinition("shared", "glm")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"),
		[]byte("defaults:\n  full: sol\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "subagents", "shared.md"),
		[]byte(allowlistDefinition("shared", "sol")), 0o600); err != nil {
		t.Fatal(err)
	}
	res := target.Resolved{ID: "project", CanonicalRoot: projectDir, Global: false}
	c := staged(t, globalDir, projectDir, reconcile.Plan{TargetID: res.ID}, res)
	// Static expectation: the staged rel path carries the PROJECT content.
	data, err := os.ReadFile(filepath.Join(c.ConfigDir, "subagents", "shared.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "model: sol") || strings.Contains(string(data), "model: glm") {
		t.Fatalf("collision did not resolve project-over-global:\n%s", data)
	}
	vOut, dOut, vCode, dCode := runProductionPair(t, bin, c)
	if vCode != 0 {
		t.Fatalf("config validate --user failed (exit %d):\n%s", vCode, vOut)
	}
	if dCode != 0 {
		t.Fatalf("doctor failed (exit %d):\n%s", dCode, dOut)
	}
}
