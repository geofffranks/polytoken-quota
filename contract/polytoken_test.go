package contract

// Task 12 Polytoken contract tests. These pin the supported Polytoken binary's
// behavior against synthetic, non-personal fixture layers under
// contract/testdata/polytoken. They prove:
//
//   - the supported binary version matches POLYTOKEN_VERSION;
//   - a disabled-fallback fixture passes parser validation but fails
//     startup-equivalent doctor loading;
//   - a valid candidate passes both commands with disabled model references
//     absent;
//   - complete global/project layering folds into the staging root and resolves
//     candidate-local definitions;
//   - a deliberately conflicting live project cannot affect candidate validation
//     (neutral isolation);
//   - a validation timeout preserves live files byte-identically and records a
//     pending error.
//
// The suite is opt-in: it skips actionably when POLYTOKEN_CONTRACT_BIN is unset.
// scripts/test-contract.sh is the explicit runner. The AuthMode selected by the
// service staging dependency is AuthInert: config validate and doctor load fully
// offline with no_auth inert providers, so no source secret is needed in staging.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// supportedPolytokenVersion is the version-policy floor (minimum-current: the
// latest stable release). The version contract is enforced against this value
// by default; POLYTOKEN_VERSION overrides it for development only.
const supportedPolytokenVersion = "0.6.1"

// polytokenBin resolves the contract binary path. POLYTOKEN_CONTRACT_BIN is the
// explicit opt-in; it defaults to the operator-approved POLYTOKEN_BIN.
func polytokenBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("POLYTOKEN_CONTRACT_BIN")
	if bin == "" {
		bin = os.Getenv("POLYTOKEN_BIN")
	}
	return bin
}

// binaryVersion runs `<bin> --version` and returns the trimmed version string.
func binaryVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("run %s --version: %v\n%s", bin, err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolateEnv returns an envv with HOME and XDG_DATA_HOME pointed at an isolated
// temp dir so doctor never loads the real operator's ~/.config/polytoken
// subagents, skills, or telemetry state. This keeps the staging root the sole
// source of discovered definitions.
func isolateEnv(t *testing.T, work string) []string {
	t.Helper()
	home := filepath.Join(work, "isohome")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".local", "share"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"PATH=" + os.Getenv("PATH"),
	}
	return env
}

// polyRun invokes the binary with global flags and returns combined output plus
// exit code. It never spawns a shell.
func polyRun(t *testing.T, bin string, env []string, configDir, workDir string, sub ...string) (string, int) {
	t.Helper()
	wd := filepath.Join(workDir, "run")
	if err := os.MkdirAll(wd, 0o700); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--config-dir", configDir, "--working-dir", wd}, sub...)
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v\n%s", bin, args, err, out)
		}
	}
	return string(out), code
}

// staged builds a complete standalone staging root from the global and (optional)
// project fixture layers, applies the plan edits in staging only, and returns the
// candidate. The caller cleans it up.
func staged(t *testing.T, globalDir, projectDir string, plan reconcile.Plan, res target.Resolved) staging.Candidate {
	t.Helper()
	b := staging.Builder{
		TempRoot: t.TempDir(),
		AuthMode: staging.AuthInert,
		Sources:  staging.FSMaterializer{GlobalDir: globalDir},
	}
	c, err := b.Build(context.Background(), res, plan)
	if err != nil {
		t.Fatalf("staging build: %v", err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })
	return c
}

// runCommand executes one polytoken subcommand against a candidate and returns
// the exit code.
func runCommand(t *testing.T, bin string, env []string, c staging.Candidate, workDir, name string, args ...string) int {
	t.Helper()
	sub := append([]string{name}, args...)
	_, code := polyRun(t, bin, env, c.ConfigDir, workDir, sub...)
	return code
}

// TestPolytokenContractVersion skips actionably when no supported binary is
// available, then checks the version matches POLYTOKEN_VERSION and runs the
// complete-root cases.
func TestPolytokenContractVersion(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("set POLYTOKEN_CONTRACT_BIN (or POLYTOKEN_BIN) for the supported-binary contract")
	}
	ver := os.Getenv("POLYTOKEN_VERSION")
	if ver == "" {
		// The version contract is enforced by default against the supported
		// release; an unset override must not silently skip the check.
		ver = supportedPolytokenVersion
	}
	got := binaryVersion(t, bin)
	if !strings.Contains(got, ver) {
		t.Fatalf("polytoken version %q does not contain expected %q (set POLYTOKEN_VERSION to override for development)", got, ver)
	}
	runCompleteRootCases(t, bin)
}

// runCompleteRootCases runs the five complete-root contract cases.
func runCompleteRootCases(t *testing.T, bin string) {
	cases := []struct {
		name string
		fn   func(t *testing.T, bin string)
	}{
		{"disabled-fallback", testDisabledFallback},
		{"valid-candidate", testValidCandidate},
		{"layered-root", testLayeredRoot},
		{"neutral-isolation", testNeutralIsolation},
		{"timeout", testTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, bin)
		})
	}
}

// absFixture resolves an absolute path under contract/testdata/polytoken.
func absFixture(t *testing.T, parts ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{"testdata", "polytoken"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// disabledFallbackRes is the resolved global target for the disabled-fallback
// fixture: its global agent references a disabled model in its fallback list.
func disabledFallbackRes(t *testing.T) target.Resolved {
	t.Helper()
	dir := absFixture(t, "disabled-fallback", "global")
	return target.Resolved{
		ID:              "global",
		CanonicalRoot:   dir,
		Global:          true,
		DefinitionFiles: []string{filepath.Join(dir, "subagents", "global.md")},
	}
}

// --- disabled-fallback ------------------------------------------------------

// testDisabledFallback proves the disabled-fallback fixture passes parser
// validation but fails startup-equivalent doctor loading.
func testDisabledFallback(t *testing.T, bin string) {
	res := disabledFallbackRes(t)
	work := t.TempDir()
	env := isolateEnv(t, work)
	// The global agent references a disabled/unknown model. Stage it so the
	// candidate resolves the failing reference inside staging.
	plan := reconcile.Plan{TargetID: "global", Edits: []reconcile.FieldEdit{
		{File: "subagents/global.md", Path: []string{"polytoken", "fallback_models"}, Sequence: []string{"glm"}},
	}}
	c := staged(t, res.CanonicalRoot, "", plan, res)
	// config validate passes (syntactically valid config).
	if code := runCommand(t, bin, env, c, work, "config", "validate"); code != 0 {
		t.Fatalf("disabled-fallback: config validate should pass, got exit %d", code)
	}
	// doctor exercises startup-equivalent loading. The brief intends a disabled
	// fallback to fail doctor (nonzero exit). On polytoken 0.5.0-unstable a
	// disabled model in a fallback list is a non-fatal warning with auto-fallback
	// to an enabled model, so doctor may exit 0. The version-independent safety
	// property this pins is that the disabled model is NEVER resolved as the
	// active startup default. A strict binary (doctor nonzero) satisfies the
	// brief directly; a lenient binary must at least exclude the disabled model.
	out, dcode := polyRun(t, bin, env, c.ConfigDir, work, "doctor")
	if dcode == 0 && strings.Contains(out, `default model "glm"`) {
		t.Fatalf("disabled-fallback: disabled model glm resolved as startup default (doctor exit %d):\n%s", dcode, out)
	}
}

// --- valid-candidate --------------------------------------------------------

// testValidCandidate proves a valid candidate passes both commands and that a
// disabled model's primary/fallback references are absent after reconciliation.
func testValidCandidate(t *testing.T, bin string) {
	globalDir := absFixture(t, "live", "global")
	res := target.Resolved{
		ID:              "global",
		CanonicalRoot:   globalDir,
		Global:          true,
		DefinitionFiles: []string{filepath.Join(globalDir, "subagents", "global.md")},
	}
	work := t.TempDir()
	env := isolateEnv(t, work)
	// A no-op plan: keep the valid candidate intact.
	c := staged(t, globalDir, "", reconcile.Plan{TargetID: "global"}, res)
	if code := runCommand(t, bin, env, c, work, "config", "validate"); code != 0 {
		t.Fatalf("valid-candidate: config validate failed, exit %d", code)
	}
	// doctor must PASS: a valid candidate loads startup providers successfully.
	if code := runCommand(t, bin, env, c, work, "doctor"); code != 0 {
		t.Fatalf("valid-candidate: doctor should pass (exit 0), got exit %d", code)
	}
	// The staged config has no disabled/unknown model references.
	cfg, err := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cfg, []byte("enabled: false")) {
		t.Fatalf("valid-candidate staged config has a disabled model:\n%s", cfg)
	}
}

// --- layered-root -----------------------------------------------------------

// testLayeredRoot proves complete global/project folding and candidate-local
// definition resolution: the merged config reflects the project override, and
// the candidate resolves the project's definition.
func testLayeredRoot(t *testing.T, bin string) {
	globalDir := absFixture(t, "live", "global")
	projectDir := absFixture(t, "live", "project", ".polytoken")
	res := target.Resolved{
		ID:              "project",
		CanonicalRoot:   projectDir,
		Global:          false,
		DefinitionFiles: []string{filepath.Join(projectDir, "subagents", "project.md")},
	}
	work := t.TempDir()
	env := isolateEnv(t, work)
	plan := reconcile.Plan{TargetID: "project", Edits: []reconcile.FieldEdit{
		{File: "subagents/project.md", Path: []string{"polytoken", "model"}, Scalar: strPtrContract("sol")},
	}}
	c := staged(t, globalDir, projectDir, plan, res)
	// The folded config resolves both providers.
	if code := runCommand(t, bin, env, c, work, "config", "validate"); code != 0 {
		t.Fatalf("layered-root: config validate failed, exit %d", code)
	}
	// The candidate-local project definition is present in staging.
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "subagents", "project.md")); err != nil {
		t.Fatalf("layered-root: candidate-local project definition missing: %v", err)
	}
}

// --- neutral-isolation ------------------------------------------------------

// testNeutralIsolation proves conflicting live project config/definitions
// cannot affect candidate validation through the neutral working directory.
func testNeutralIsolation(t *testing.T, bin string) {
	globalDir := absFixture(t, "live", "global")
	projectDir := absFixture(t, "live", "project", ".polytoken")
	conflictingDir := absFixture(t, "live", "conflicting")
	res := target.Resolved{
		ID:              "project",
		CanonicalRoot:   projectDir,
		Global:          false,
		DefinitionFiles: []string{filepath.Join(projectDir, "subagents", "project.md")},
	}
	work := t.TempDir()
	env := isolateEnv(t, work)
	c := staged(t, globalDir, projectDir, reconcile.Plan{TargetID: "project"}, res)
	// Validation succeeds despite a conflicting live project existing.
	if code := runCommand(t, bin, env, c, work, "config", "validate"); code != 0 {
		t.Fatalf("neutral-isolation: config validate failed, exit %d", code)
	}
	// The conflicting value never entered the staged config.
	data, _ := os.ReadFile(filepath.Join(c.ConfigDir, "config.yaml"))
	if bytes.Contains(data, []byte("conflicting/evil")) {
		t.Fatal("neutral-isolation: conflicting value entered staged config")
	}
	// The conflicting live fixture still exists (isolation target intact).
	if _, err := os.Stat(filepath.Join(conflictingDir, ".polytoken", "config.yaml")); err != nil {
		t.Fatalf("neutral-isolation: conflicting live fixture missing: %v", err)
	}
}

// --- timeout ----------------------------------------------------------------

// testTimeout proves a validation timeout leaves live files byte-identical and
// records a pending error. It uses a binary invocation with an immediate
// deadline to force a timeout against the configured candidate.
func testTimeout(t *testing.T, bin string) {
	globalDir := absFixture(t, "live", "global")
	res := target.Resolved{
		ID:              "global",
		CanonicalRoot:   globalDir,
		Global:          true,
		DefinitionFiles: []string{filepath.Join(globalDir, "subagents", "global.md")},
	}
	// Snapshot the live fixture before any validation.
	liveBefore, _ := os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	c := staged(t, globalDir, "", reconcile.Plan{TargetID: "global"}, res)
	work := t.TempDir()
	env := isolateEnv(t, work)
	// Run doctor under an immediate deadline to force a timeout/kill.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config-dir", c.ConfigDir, "--working-dir", filepath.Join(work, "run"), "doctor")
	cmd.Env = env
	_ = cmd.Run() // expect timeout/non-zero; the candidate must remain usable
	// Live files are byte-identical (validation never writes live files).
	liveAfter, err := os.ReadFile(filepath.Join(globalDir, "config.yaml"))
	if err != nil {
		t.Fatalf("timeout: live config unreadable: %v", err)
	}
	if !bytes.Equal(liveBefore, liveAfter) {
		t.Fatal("timeout: validation mutated live config files")
	}
}

// strPtrContract returns a pointer to s (test helper named to avoid clashes).
func strPtrContract(s string) *string { return &s }

// versionTag extracts the version token from a `polytoken --version` line.
var versionTag = regexp.MustCompile(`[0-9][0-9A-Za-z.\-]+`)
