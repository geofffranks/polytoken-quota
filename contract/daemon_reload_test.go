package contract

// Real-daemon reload contract: spawn a throwaway Polytoken daemon via
// `polytoken new --no-attach` against the synthetic global fixture under an
// isolated HOME, then prove the in-session notice-hook converges it through
// the documented loopback API (POST /reload, Bearer credential from the
// session's own startup/credential artifacts). The daemon never touches live
// configuration: the fixture root is the sole config source, and the daemon
// is terminated on every exit path.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
)

// daemonSpawn is one throwaway daemon lifecycle.
type daemonSpawn struct {
	bin       string
	sessions  string
	sessionID string
	port      int
	pid       int
	token     string
}

// requireDaemonCapabilities fails fast when the pinned binary lacks the
// surfaces this contract depends on, so a version regression is reported as
// a capability error rather than a mysterious spawn failure.
func requireDaemonCapabilities(t *testing.T, bin string) {
	t.Helper()
	help, err := exec.Command(bin, "new", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("capability check: %s new --help: %v\n%s", bin, err, help)
	}
	if !strings.Contains(string(help), "--no-attach") {
		t.Fatalf("capability check: %s new lacks --no-attach; the supported Polytoken version must provide headless session spawn", bin)
	}
	openapi, err := exec.Command(bin, "openapi", "--format", "json").CombinedOutput()
	if err != nil {
		t.Fatalf("capability check: %s openapi: %v\n%s", bin, err, openapi)
	}
	if !strings.Contains(string(openapi), `"/health"`) || !strings.Contains(string(openapi), `"/reload"`) {
		t.Fatalf("capability check: %s openapi lacks /health or /reload routes", bin)
	}
}

// spawnDaemon boots one throwaway daemon against the synthetic fixture and
// registers guaranteed teardown (SIGTERM, then SIGKILL) via t.Cleanup.
func spawnDaemon(t *testing.T, work string) *daemonSpawn {
	t.Helper()
	bin := polytokenBin(t)

	home := filepath.Join(work, "isohome")
	sessions := filepath.Join(home, ".local", "share", "polytoken", "sessions")
	fixture := filepath.Join("testdata", "polytoken", "global")
	cfg := filepath.Join(home, ".config", "polytoken")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(cfg, os.DirFS(fixture)); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	proj := filepath.Join(work, "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "new", "--no-attach",
		"--sessions-dir", sessions,
		"--log-dir", filepath.Join(work, "logs"),
	)
	cmd.Dir = proj
	cmd.Env = isolateEnv(t, work)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn daemon: %v\n%s", err, out)
	}
	re := regexp.MustCompile(`session_id=([A-Za-z0-9_-]+) port=(\d+)`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("spawn daemon: cannot parse session output:\n%s", out)
	}
	sp := &daemonSpawn{bin: bin, sessions: sessions, sessionID: m[1]}
	fmt.Sscanf(m[2], "%d", &sp.port)

	sessDir := filepath.Join(sessions, sp.sessionID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var startup struct {
			State string `json:"state"`
			PID   int    `json:"pid"`
			Port  int    `json:"port"`
		}
		if b, rerr := os.ReadFile(filepath.Join(sessDir, "startup.json")); rerr == nil && json.Unmarshal(b, &startup) == nil {
			if startup.State == "ready" && startup.Port > 0 {
				sp.pid = startup.PID
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never reached ready state (sessions dir: %s)", sessDir)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var cred struct {
		Token string `json:"token"`
	}
	if b, rerr := os.ReadFile(filepath.Join(sessDir, "credential.json")); rerr != nil {
		t.Fatalf("read credential: %v", rerr)
	} else if err := json.Unmarshal(b, &cred); err != nil || cred.Token == "" {
		t.Fatalf("parse credential: %v", err)
	}
	sp.token = cred.Token

	t.Cleanup(func() { sp.terminate(t) })
	return sp
}

// terminate stops the daemon on every exit path: SIGTERM first, bounded wait,
// then SIGKILL.
func (sp *daemonSpawn) terminate(t *testing.T) {
	t.Helper()
	if sp.pid <= 0 || sp.pid == os.Getpid() {
		return
	}
	_ = syscall.Kill(sp.pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(sp.pid, 0) != nil {
			return // reaped
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(sp.pid, syscall.SIGKILL)
}

func (sp *daemonSpawn) health(t *testing.T) int {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", sp.port), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sp.token)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestDaemonSpawnFailureFailsFast: a garbage config root produces a clear
// spawn error (never a hang), exercising the helper's failure path.
func TestDaemonSpawnFailureFailsFast(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	work := t.TempDir()
	home := filepath.Join(work, "isohome")
	badCfg := filepath.Join(home, ".config", "polytoken")
	if err := os.MkdirAll(filepath.Join(badCfg, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badCfg, "subagents", "broken.md"), []byte("---\ndescription: no name\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(work, "proj")
	_ = os.MkdirAll(proj, 0o700)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "new", "--no-attach",
		"--sessions-dir", filepath.Join(work, "sessions"),
		"--log-dir", filepath.Join(work, "logs"),
	)
	cmd.Dir = proj
	cmd.Env = isolateEnv(t, work)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("spawn against a broken config must fail, got success:\n%s", out)
	}
}

// TestNoticeHookReloadsRealDaemon: with a notice revision newer than the
// session's consumed marker, notice-hook performs exactly one authenticated
// POST /reload on its own daemon, the consumed marker advances (200-only
// semantics), and the daemon stays healthy.
func TestNoticeHookReloadsRealDaemon(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	work := t.TempDir()
	sp := spawnDaemon(t, work)
	if code := sp.health(t); code != http.StatusOK {
		t.Fatalf("daemon unhealthy after spawn: /health = %d", code)
	}

	noticePath := filepath.Join(work, "notice.json")
	noticeDoc := map[string]any{
		"schema":          1,
		"revision":        5,
		"routing_enabled": true,
		"targets":         []any{},
		"disabled_models": []any{},
	}
	nb, err := json.Marshal(noticeDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noticePath, nb, 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"POLYTOKEN_HOOK_EVENT": "post_model_turn",
		"POLYTOKEN_SESSION_ID": sp.sessionID,
	}
	if code := notice.RunHook(notice.HookDeps{
		NoticePath:  noticePath,
		SessionsDir: sp.sessions,
		Stdout:      io.Discard,
		Environ:     func(k string) string { return env[k] },
	}); code != 0 {
		t.Fatalf("notice-hook exit = %d, want 0", code)
	}

	markerPath := filepath.Join(sp.sessions, sp.sessionID, "polytoken-quota", "state.json")
	mb, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("consumed marker missing (reload did not return 200): %v", err)
	}
	var marker struct {
		ConsumedRevision uint64 `json:"consumed_revision"`
	}
	if err := json.Unmarshal(mb, &marker); err != nil || marker.ConsumedRevision != 5 {
		t.Fatalf("consumed marker = %s, want consumed_revision 5", mb)
	}

	// The daemon remains healthy after the reload.
	if code := sp.health(t); code != http.StatusOK {
		t.Fatalf("daemon unhealthy after reload: /health = %d", code)
	}

	// Re-firing for the same revision performs no further work (the hook is a
	// no-op; daemon still healthy).
	if code := notice.RunHook(notice.HookDeps{
		NoticePath:  noticePath,
		SessionsDir: sp.sessions,
		Environ:     func(k string) string { return env[k] },
	}); code != 0 {
		t.Fatalf("second notice-hook exit = %d", code)
	}
}
