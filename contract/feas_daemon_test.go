package contract

// Shared disposable-daemon test harness. It was previously defined in the
// model-groups feasibility suite; the provider-notice contract still uses it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func driveWaitHistory(t *testing.T, d *feasDaemon, wantReply string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, hb := d.do(t, http.MethodGet, "/history", "")
		if strings.Contains(string(hb), wantReply) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("history never contained %q within 30s", wantReply)
}


// --- fixture writing ---------------------------------------------------------

// feasModelYAML renders one version-4 models entry.
func feasModelYAML(name, provider, providerName, class string, enabled bool) string {
	return fmt.Sprintf(`  %s:
    provider: %s
    provider_name: %s
    class: %s
    context_window: 200000
    enabled: %t
`, name, provider, providerName, class, enabled)
}

// feasGlobalConfig renders a synthetic version-4 global config layer with the
// three stub models and the authored group/tier pins used by the daemon cases.
// enabled[m] can disable individual models.
func feasGlobalConfig(stubURL string, enabled map[string]bool) string {
	mk := func(name, class string) string {
		return feasModelYAML("stub/"+name, "stub", name, class, enabled["stub/"+name])
	}
	return fmt.Sprintf(`version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: true
models:
%s%s%smodelgroups:
  failover:
    - stub/m1
    - stub/m2
  polytoken:default_model_full: stub/m3
  polytoken:default_model_mini: stub/m2
`, stubURL, mk("m1", "full"), mk("m2", "mini"), mk("m3", "full"))
}

// --- disposable daemon launcher ----------------------------------------------

// feasDaemon is one throwaway daemon lifecycle for the feasibility probes.
type feasDaemon struct {
	bin       string
	sessionID string
	port      int
	pid       int
	token     string
	sessions  string
}

// spawnFeasibilityDaemon boots one throwaway daemon via `new --no-attach`
// against an isolated HOME whose global config layer is globalYAML, with a
// neutral working directory (no `.polytoken`). extraArgs may add session
// flags such as --facets-dir / --facet.
//
// Readiness and credential discovery follow the 0.8.15 layout: the session
// registry under --sessions-dir keeps a `starting` entry, while the runtime
// data directory `<parent-of-sessions-dir>/sessions-v1/<sid>/startup.json`
// carries state=ready, pid, port, and credential_file_path.
func spawnFeasibilityDaemon(t *testing.T, work, globalYAML string, extraArgs ...string) *feasDaemon {
	t.Helper()
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}

	home := filepath.Join(work, "isohome")
	cfg := filepath.Join(home, ".config", "polytoken")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(work, "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(work, "sessions")
	logs := filepath.Join(work, "logs")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := append([]string{
		"new", "--no-attach",
		"--sessions-dir", sessions,
		"--log-dir", logs,
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = proj
	cmd.Env = isolateEnv(t, work)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logDump := ""
		if entries, _ := os.ReadDir(logs); len(entries) > 0 {
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".log") {
					continue
				}
				b, rerr := os.ReadFile(filepath.Join(logs, e.Name()))
				if rerr == nil {
					logDump += fmt.Sprintf("\n--- %s ---\n%s", e.Name(), b)
				}
			}
		}
		t.Fatalf("spawn daemon: %v\n%s%s", err, out, logDump)
	}
	re := regexp.MustCompile(`session_id=([A-Za-z0-9_-]+) port=(\d+)`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("spawn daemon: cannot parse session output:\n%s", out)
	}
	d := &feasDaemon{bin: bin, sessionID: m[1], sessions: sessions}
	if _, err := fmt.Sscanf(m[2], "%d", &d.port); err != nil {
		t.Fatalf("spawn daemon: bad port %q: %v", m[2], err)
	}

	// Poll the runtime data startup file for state=ready.
	dataStartup := filepath.Join(filepath.Dir(sessions), "sessions-v1", d.sessionID, "startup.json")
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, rerr := os.ReadFile(dataStartup)
		if rerr == nil {
			var su struct {
				State         string `json:"state"`
				PID           int    `json:"pid"`
				Port          int    `json:"port"`
				CredentialURL string `json:"credential_file_path"`
			}
			if json.Unmarshal(b, &su) == nil && su.State == "ready" && su.PID > 0 {
				d.pid, d.port = su.PID, su.Port
				cb, crerr := os.ReadFile(su.CredentialURL)
				if crerr != nil {
					t.Fatalf("read credential: %v", crerr)
				}
				var cred struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(cb, &cred); err != nil || cred.Token == "" {
					t.Fatalf("parse credential: %v", err)
				}
				d.token = cred.Token
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never reached ready state (looked at %s)", dataStartup)
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Cleanup(func() { terminateFeasibilityDaemon(t, d) })
	return d
}

// terminateFeasibilityDaemon stops the daemon on every exit path: SIGTERM
// first, bounded wait, then SIGKILL.
func terminateFeasibilityDaemon(t *testing.T, d *feasDaemon) {
	t.Helper()
	if d.pid <= 0 || d.pid == os.Getpid() {
		return
	}
	_ = syscall.Kill(d.pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(d.pid, 0) != nil {
			return // reaped
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(d.pid, syscall.SIGKILL)
}

func (d *feasDaemon) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", d.port, path), rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// feasState fetches and decodes GET /state.
func (d *feasDaemon) feasState(t *testing.T) map[string]any {
	t.Helper()
	code, body := d.do(t, http.MethodGet, "/state", "")
	if code != http.StatusOK {
		t.Fatalf("GET /state = %d: %s", code, body)
	}
	var s map[string]any
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decode /state: %v", err)
	}
	return s
}

// groupCandidates extracts one group's ordered candidates from /state.
func groupCandidates(t *testing.T, s map[string]any, name string) []string {
	t.Helper()
	mg, ok := s["model_groups"].(map[string]any)
	if !ok {
		t.Fatal("/state has no model_groups catalog")
	}
	groups, _ := mg["groups"].([]any)
	for _, g := range groups {
		entry := g.(map[string]any)
		if entry["name"] == name {
			cands, _ := entry["candidates"].([]any)
			var out []string
			for _, c := range cands {
				out = append(out, c.(string))
			}
			return out
		}
	}
	t.Fatalf("group %q missing from /state catalog: %v", name, mg)
	return nil
}

// selectModel POSTs one selection and requires HTTP 200.
func (d *feasDaemon) selectModel(t *testing.T, ref string) {
	t.Helper()
	code, body := d.do(t, http.MethodPost, "/model", fmt.Sprintf(`{"model":%q}`, ref))
	if code != http.StatusOK {
		t.Fatalf("POST /model %q = %d: %s", ref, code, body)
	}
}

// driveTurn submits one prompt and polls history until the stub reply text
// appears, then returns.
func (d *feasDaemon) driveTurn(t *testing.T, sp *stubProvider, content, wantReply string) {
	t.Helper()
	code, body := d.do(t, http.MethodPost, "/prompt", fmt.Sprintf(`{"content":%q}`, content))
	if code != http.StatusAccepted {
		t.Fatalf("POST /prompt = %d: %s", code, body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, hb := d.do(t, http.MethodGet, "/history", "")
		if strings.Contains(string(hb), wantReply) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	last := "none"
	if r := sp.lastRequestQuiet(); r != nil {
		last = fmt.Sprintf("model=%q path=%q auth=%q", r.Model, r.Path, r.AuthHeader)
	}
	_, hb := d.do(t, http.MethodGet, "/history", "")
	t.Fatalf("turn never produced %q in history within 30s; last stub request: %s; history tail: %.1200s",
		wantReply, last, hb)
}

// reloadIdle POSTs /reload on an idle session and requires a successful,
// unqueued reload with an empty failed list.
func (d *feasDaemon) reloadIdle(t *testing.T) {
	t.Helper()
	code, body := d.do(t, http.MethodPost, "/reload", "")
	if code != http.StatusOK {
		t.Fatalf("POST /reload = %d: %s", code, body)
	}
	var r struct {
		Queued bool     `json:"queued"`
		Failed []string `json:"failed"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode /reload: %v (%s)", err, body)
	}
	if r.Queued {
		t.Fatalf("reload was queued (session not idle): %s", body)
	}
	if len(r.Failed) != 0 {
		t.Fatalf("reload reported failed stages %v (config re-read rejected); %s", r.Failed, body)
	}
}

// routingSnapshot finds the latest history model_routing_v1 snapshot.
func (d *feasDaemon) routingSnapshot(t *testing.T) map[string]any {
	t.Helper()
	_, body := d.do(t, http.MethodGet, "/history", "")
	var h struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decode /history: %v", err)
	}
	var last map[string]any
	for _, it := range h.Items {
		if it["type"] == "model_routing_v1" {
			last = it
		}
	}
	if last == nil {
		t.Fatal("no model_routing_v1 snapshot in history")
	}
	snapshot, _ := last["snapshot"].(map[string]any)
	return snapshot
}

// --- validation matrix (no daemon) --------------------------------------------

// feasValidate runs `config validate --user` against an isolated single-layer
// root with a neutral working directory and returns (exitOK, combinedOutput).
func feasValidate(t *testing.T, work, configYAML string) (bool, string) {
	t.Helper()
	cfg := filepath.Join(work, "cfg")
	wd := filepath.Join(work, "wd")
	for _, d := range []string{cfg, wd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(polytokenBin(t), "--config-dir", cfg, "--working-dir", wd, "config", "validate", "--user")
	cmd.Env = isolateEnv(t, work)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}
