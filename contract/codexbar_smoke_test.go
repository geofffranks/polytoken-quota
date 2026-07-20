package contract

// Task 14 optional CodExBar `hooks test` smoke test. CodExBar (0.44.0+) can run
// a synthetic hook delivery to verify its hook wiring end to end. This test is
// strictly opt-in: it skips unless an explicit CODEXBAR_TEST_BIN environment
// variable names a supported binary, and it never invokes a live account or
// provider — it delivers a synthetic provider/event only. Without the guard it
// is a no-op so the default `go test ./...` suite never touches a live CodExBar
// installation.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// smokeTimeout bounds the optional CodExBar hooks-test smoke command.
const smokeTimeout = 60 * time.Second

// TestCodexBarHooksTestSmoke runs `codexbar hooks test` (or the binary's
// equivalent smoke command) against a synthetic provider/event when an explicit
// binary is provided via CODEXBAR_TEST_BIN. It asserts the smoke command exits 0
// without invoking any live account or provider. It skips otherwise.
func TestCodexBarHooksTestSmoke(t *testing.T) {
	bin := os.Getenv("CODEXBAR_TEST_BIN")
	if bin == "" {
		t.Skip("set CODEXBAR_TEST_BIN to a supported codexbar binary to run the hooks-test smoke")
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("CODEXBAR_TEST_BIN=%q not on PATH: %v", bin, err)
	}
	// Isolate HOME and XDG so the smoke never loads the operator's real config.
	work := t.TempDir()
	env := append(os.Environ(),
		"HOME="+work,
		"XDG_CONFIG_HOME="+work,
		"XDG_DATA_HOME="+work,
	)
	// `codexbar hooks test` delivers a synthetic hook event end to end. The
	// command and its arguments are the documented CodExBar smoke surface; it
	// uses a synthetic provider, never a live account.
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "hooks", "test")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("codexbar hooks test failed (exit %v):\n%s", err, out)
	}
}
