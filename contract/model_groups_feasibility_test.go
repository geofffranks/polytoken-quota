package contract

// Feasibility task 1 (scope quota-model-groups-20260926): synthetic, private
// version-4 Polytoken fixtures against the supported binary in a neutral
// working directory, plus disposable-daemon probes. The suite proves, with
// live observation rather than API-schema inference:
//
//   - provider enable semantics: `providers.<id>.enabled: false` makes the
//     provider's models unavailable, which fails `config validate` when a
//     group leaf or tier default depends on them;
//   - authored model-group leaf rules: a group whose every leaf is
//     confirmed unavailable is rejected; mixed valid/invalid leaves are
//     accepted with unavailable leaves skipped; a bare name in a leaf never
//     resolves as a group reference even when a group of that name exists;
//   - mixed defaults: explicit `polytoken:default_model_full` /
//     `polytoken:default_model_mini` tier pins load, and the unset nano tier
//     resolves through the mini tier (published in the daemon group catalog);
//   - same-name global/project groups are concatenated, global leaves first,
//     rather than replacing the project definition as documented;
//   - a disposable daemon (`new --no-attach` under an isolated HOME) exposes
//     an active-model observation API (GET /state) and accepts selections
//     (POST /model) for concrete models and prefixed group references;
//   - the daemon executes turns against a loopback no-auth stub provider
//     with no external network: the provider request carries the resolved
//     concrete model and no Authorization header, and the assistant reply
//     lands in session history;
//   - after the session's active model is disabled in config, a SUCCESSFUL
//     idle reload (POST /reload, empty failed list) continues the session
//     automatically: the routing snapshot records transition reason
//     `reload_reconciliation` and the next turn executes on the re-resolved
//     model with no manual reselect. A reload that would leave the config
//     invalid (no full-class default) is rejected wholesale and the session
//     keeps the stale active model — validation passing is NOT the proof;
//     the proof is the observed route change and the post-reload turn.
//   - same-name layering at the routing/turn level with a provider-level
//     disable: with every provider enabled the concatenated catalog keeps the
//     duplicate leaf at both positions and the group-pinned turn executes on
//     the global head; disabling the provider that owns BOTH global leaves
//     (`providers.<id>.enabled: false`) keeps the required full-tier default
//     valid, the successful idle reload drops every disabled-provider leaf —
//     including the duplicate project-position copy — and the next turn
//     executes on the project-only provider's leaf with history preserved;
//   - a table-driven matrix proves the same reload continuity for the four
//     remaining selection channels — ordinary default route (no explicit
//     selection), a group-pinned facet, a concrete facet pin, and a manual
//     selection: in each case disabling the provider that owns the selected
//     model reloads successfully (failed list empty) and the next turn runs
//     with no manual reselect. The serving model is proven from the recorded
//     provider request and the session history, never from /state alone, and
//     the pre-reload reply survives in history alongside the new one.
//
// Isolation: every case runs with HOME/XDG pointed at a fresh temp root, a
// neutral working directory containing no `.polytoken`, and the only HTTP
// traffic is this test process talking to its own in-process stub bound to
// 127.0.0.1 and the throwaway daemon's loopback API. No live configuration,
// credentials, or external provider requests are involved.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/groupsafety"
)

// --- in-process loopback stub provider --------------------------------------

// stubProviderReq is one recorded provider request.
type stubProviderReq struct {
	Method     string
	Path       string
	AuthHeader string
	Model      string
	Stream     bool
}

// stubProvider is a minimal OpenAI chat-completions compatible provider bound
// to the loopback interface. It answers streaming requests with a short SSE
// completion and non-streaming requests with plain JSON. It never performs an
// outbound request of its own.
type stubProvider struct {
	srv  *httptest.Server
	URL  string // base URL up to and including /v1
	mu   chan struct{}
	reqs []stubProviderReq
}

func newStubProvider(t *testing.T) *stubProvider {
	t.Helper()
	sp := &stubProvider{mu: make(chan struct{}, 1)}
	sp.mu <- struct{}{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		<-sp.mu
		sp.reqs = append(sp.reqs, stubProviderReq{
			Method:     r.Method,
			Path:       r.URL.Path,
			AuthHeader: r.Header.Get("Authorization"),
			Model:      body.Model,
			Stream:     body.Stream,
		})
		sp.mu <- struct{}{}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			chunk := func(content string, finish any) {
				payload := map[string]any{
					"id": "chatcmpl-stub", "object": "chat.completion.chunk",
					"created": 1700000000, "model": body.Model,
					"choices": []map[string]any{
						{"index": 0, "delta": deltaFor(content), "finish_reason": finish},
					},
				}
				b, _ := json.Marshal(payload)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			chunk("", nil)
			chunk("stub-reply-model="+body.Model, nil)
			chunk("", "stop")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-stub", "object": "chat.completion",
			"created": 1700000000, "model": body.Model,
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant", "content": "stub-reply-model=" + body.Model,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := func(id string) map[string]any {
			return map[string]any{
				"id": id, "object": "model",
				"created": 1700000000, "owned_by": "stub",
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{entry("m1"), entry("m2"), entry("m3")},
		})
	})
	// Some client code paths request the catalog relative to the base origin
	// rather than the /v1 base; serve both so the stub never 404s a probe.
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := func(id string) map[string]any {
			return map[string]any{
				"id": id, "object": "model",
				"created": 1700000000, "owned_by": "stub",
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{entry("m1"), entry("m2"), entry("m3")},
		})
	})
	sp.srv = httptest.NewServer(mux) // binds 127.0.0.1 only
	sp.URL = sp.srv.URL + "/v1"
	t.Cleanup(sp.srv.Close)
	return sp
}

func deltaFor(content string) map[string]any {
	d := map[string]any{}
	if content != "" {
		d["content"] = content
	}
	return d
}

// requestCount returns how many provider requests have been recorded.
func (sp *stubProvider) requestCount() int {
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	return len(sp.reqs)
}

// driveWaitHistory polls session history until the given reply text appears.
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

// lastRequest returns the most recent recorded provider request.
func (sp *stubProvider) lastRequest(t *testing.T) stubProviderReq {
	t.Helper()
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	if len(sp.reqs) == 0 {
		t.Fatal("stub provider recorded no requests")
	}
	return sp.reqs[len(sp.reqs)-1]
}

// lastRequestQuiet returns nil when nothing was recorded (non-fatal variant
// used inside timeout diagnostics).
func (sp *stubProvider) lastRequestQuiet() *stubProviderReq {
	<-sp.mu
	defer func() { sp.mu <- struct{}{} }()
	if len(sp.reqs) == 0 {
		return nil
	}
	r := sp.reqs[len(sp.reqs)-1]
	return &r
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

func TestModelGroupsValidationMatrix(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}

	base := func(models, groups string) string {
		return fmt.Sprintf(`version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: http://127.0.0.1:1/v1
    auth:
      type: no_auth
    enabled: %t
models:
%smodelgroups:
%s`, true, models, groups)
	}

	t.Run("provider-disabled-makes-models-unavailable", func(t *testing.T) {
		work := t.TempDir()
		cfg := fmt.Sprintf(`version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: http://127.0.0.1:1/v1
    auth:
      type: no_auth
    enabled: false
models:
%smodelgroups:
  failover: stub/m1
  polytoken:default_model_full: stub/m1
`, feasModelYAML("stub/m1", "stub", "m1", "full", true))
		ok, out := feasValidate(t, work, cfg)
		if ok {
			t.Fatalf("provider disabled: validate should fail, output:\n%s", out)
		}
		if !strings.Contains(out, "unavailable") {
			t.Fatalf("provider disabled: expected unavailable-leaf diagnostic, output:\n%s", out)
		}
	})

	t.Run("all-leaves-unavailable-rejected", func(t *testing.T) {
		work := t.TempDir()
		cfg := base(feasModelYAML("stub/m1", "stub", "m1", "full", true),
			"  bad: [ghost/nope, absent/also]\n  polytoken:default_model_full: stub/m1\n")
		ok, out := feasValidate(t, work, cfg)
		if ok {
			t.Fatalf("all-invalid leaves: validate should fail, output:\n%s", out)
		}
		if !strings.Contains(out, "no available model-group leaves") {
			t.Fatalf("all-invalid leaves: expected no-available-leaves diagnostic, output:\n%s", out)
		}
	})

	t.Run("mixed-valid-invalid-leaves-accepted", func(t *testing.T) {
		work := t.TempDir()
		cfg := base(feasModelYAML("stub/m1", "stub", "m1", "full", true),
			"  mixed: [ghost/nope, stub/m1]\n  polytoken:default_model_full: stub/m1\n")
		ok, out := feasValidate(t, work, cfg)
		if !ok {
			t.Fatalf("mixed leaves: validate should pass with unavailable leaves skipped, output:\n%s", out)
		}
	})

	t.Run("bare-name-leaf-never-resolves-as-group", func(t *testing.T) {
		work := t.TempDir()
		cfg := base(feasModelYAML("stub/m1", "stub", "m1", "full", true),
			"  real: stub/m1\n  baregroup: [real]\n  polytoken:default_model_full: stub/m1\n")
		ok, out := feasValidate(t, work, cfg)
		if ok {
			t.Fatalf("bare-name leaf: validate should fail (a bare name is a concrete model reference, not a group ref), output:\n%s", out)
		}
		if !strings.Contains(out, "no available model-group leaves") {
			t.Fatalf("bare-name leaf: expected no-available-leaves diagnostic, output:\n%s", out)
		}
	})
}

// --- daemon observation, selection, turns, reload continuity -------------------

func TestDisposableDaemonActiveModelTurnsAndReloadContinuity(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	cfgYAML := feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": true, "stub/m2": true, "stub/m3": true})
	d := spawnFeasibilityDaemon(t, work, cfgYAML)

	// Observation: active model and the published group catalog.
	s := d.feasState(t)
	if active, _ := s["active_model"].(string); active == "" {
		t.Fatalf("GET /state exposes no active_model: %v", s)
	}
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"stub/m1", "stub/m2"}) {
		t.Fatalf("authored group catalog = %v, want [stub/m1 stub/m2]", got)
	}
	// Mixed defaults: the explicit mini pin publishes as its own reserved
	// group and the mini-tier reserved consumers inherit it; the full-tier
	// consumers carry the explicit full pin.
	if got := groupCandidates(t, s, "polytoken:default_model_mini"); !equalStrings(got, []string{"stub/m2"}) {
		t.Fatalf("mini tier pin = %v, want [stub/m2]", got)
	}
	if got := groupCandidates(t, s, "polytoken:general-purpose-mini"); !equalStrings(got, []string{"stub/m2"}) {
		t.Fatalf("mini-tier reserved group = %v, want [stub/m2]", got)
	}
	if got := groupCandidates(t, s, "polytoken:general-purpose"); !equalStrings(got, []string{"stub/m3"}) {
		t.Fatalf("full-tier reserved group = %v, want [stub/m3]", got)
	}

	// Manual concrete selection.
	d.selectModel(t, "stub/m2")
	if s = d.feasState(t); s["active_model"] != "stub/m2" {
		t.Fatalf("after concrete selection active = %v, want stub/m2", s["active_model"])
	}

	// Bare group name is rejected with a typed error.
	if code, body := d.do(t, http.MethodPost, "/model", `{"model":"failover"}`); code != http.StatusBadRequest {
		t.Fatalf("bare group name should be 400 invalid_model_reference, got %d: %s", code, body)
	}

	// Group pin: move off the head leaf first so the pin is a real change.
	d.selectModel(t, "stub/m3")
	d.selectModel(t, "mg:failover")
	if s = d.feasState(t); s["active_model"] != "stub/m1" {
		t.Fatalf("after group pin active = %v, want group head stub/m1", s["active_model"])
	}
	snap := d.routingSnapshot(t)
	route, _ := snap["route"].(map[string]any)
	target, _ := route["target"].(map[string]any)
	if target["kind"] != "group" || target["group"] != "failover" {
		t.Fatalf("routing target = %v, want kind=group group=failover", target)
	}

	// Turn-driving against the loopback no-auth stub: the group head leaf is
	// the concrete model the provider sees, with no Authorization header.
	d.driveTurn(t, stub, "Say hi once. Do not use tools.", "stub-reply-model=m1")
	req := stub.lastRequest(t)
	if req.Model != "m1" {
		t.Fatalf("provider saw model %q, want m1 (group head leaf)", req.Model)
	}
	if req.AuthHeader != "" {
		t.Fatalf("no_auth provider request carried Authorization: %q", req.AuthHeader)
	}
	if !strings.HasPrefix(req.Path, "/v1/") {
		t.Fatalf("unexpected provider path %q", req.Path)
	}

	// Reload continuity, part 1: an invalid post-disable config (no
	// full-class default left) is rejected wholesale and the session keeps
	// the stale active model. Config validation alone proves nothing here;
	// the observed unchanged route is the proof.
	disabledYAML := feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": false, "stub/m2": true, "stub/m3": false})
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(disabledYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := d.do(t, http.MethodPost, "/reload", "")
	if code != http.StatusOK {
		t.Fatalf("POST /reload (invalid config) = %d: %s", code, body)
	}
	var rj struct {
		Failed []string `json:"failed"`
	}
	_ = json.Unmarshal(body, &rj)
	if s = d.feasState(t); s["active_model"] != "stub/m1" {
		t.Fatalf("rejected reload should leave the stale active model in place, got %v", s["active_model"])
	}

	// Reload continuity, part 2: a config that stays valid (m3 keeps the
	// full tier alive) but disables the session-active model must converge
	// automatically on a successful idle reload — no manual reselect.
	validDisabledYAML := feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": false, "stub/m2": true, "stub/m3": true})
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(validDisabledYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	d.reloadIdle(t)

	s = d.feasState(t)
	resolved := s["active_model"].(string)
	if resolved == "stub/m1" {
		t.Fatalf("successful idle reload left the disabled model active: %v", s["active_model"])
	}
	snap = d.routingSnapshot(t)
	if reason, _ := snap["transition_reason"].(string); reason != "reload_reconciliation" {
		t.Fatalf("post-reload transition_reason = %v, want reload_reconciliation", snap["transition_reason"])
	}
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"stub/m2"}) {
		t.Fatalf("post-reload failover catalog = %v, want disabled leaf dropped: [stub/m2]", got)
	}

	// The session continues automatically: a new prompt is served by a model
	// other than the disabled one (the pinned group's next available leaf,
	// observed as stub/m2), with no manual reselect between the reload and
	// the turn. The precise re-resolution target is policy, so the contract
	// pins only the continuity property: the disabled model never serves.
	before := stub.requestCount()
	d.do(t, http.MethodPost, "/prompt", `{"content":"Again, no tools."}`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if now := stub.requestCount(); now > before {
			req = stub.lastRequest(t)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no provider request within 30s after reload (active model was %q)", resolved)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if req.Model == "m1" {
		t.Fatalf("disabled model stub/m1 served the post-reload turn")
	}
	if req.AuthHeader != "" {
		t.Fatalf("no_auth provider request carried Authorization: %q", req.AuthHeader)
	}
	driveWaitHistory(t, d, "stub-reply-model="+req.Model)
}

// --- shadowing and facet pin ---------------------------------------------------

// TestModelGroupsShadowingGlobalProject pins the observed reversed-layer
// group merge: the daemon's published catalog lists the user/global leaves
// first and appends the project leaves, preserving duplicates
// (global [m1 m3] + project [m1 m2] → [m1 m3 m1 m2]). Note: the authored
// config schema describes modelgroups layering as "the winning group value
// replaces the complete lower-priority value"; the observed published
// catalog instead concatenates layers. Tasks 2–5 must treat this observed
// behavior, not the schema prose, as the contract.
func TestModelGroupsShadowingGlobalProject(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	globalYAML := strings.Replace(feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": true, "stub/m2": true, "stub/m3": true}),
		"  failover:\n    - stub/m1\n    - stub/m2\n", "  failover:\n    - stub/m1\n    - stub/m3\n", 1)

	// Project layer present at spawn: shadows (loses ordering to) the global
	// group definition and re-shares leaf stub/m1.
	proj := filepath.Join(work, "proj", ".polytoken")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	projectYAML := fmt.Sprintf(`version: 4
modelgroups:
  failover:
    - stub/m1
    - stub/m2
`)
	if err := os.WriteFile(filepath.Join(proj, "config.yaml"), []byte(projectYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	d := spawnFeasibilityDaemon(t, work, globalYAML)

	s := d.feasState(t)
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"stub/m1", "stub/m3", "stub/m1", "stub/m2"}) {
		t.Fatalf("group shadowing catalog = %v, want global leaves first then project leaves with duplicates preserved [stub/m1 stub/m3 stub/m1 stub/m2]", got)
	}
}

// TestFacetPinConcreteModel proves a facet's `polytoken.model` frontmatter
// pins the concrete session model at startup and the pinned model serves the
// turn.
func TestFacetPinConcreteModel(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	facets := filepath.Join(work, "facets")
	if err := os.MkdirAll(facets, 0o700); err != nil {
		t.Fatal(err)
	}
	facetYAML := "---\nname: pinned\ndescription: synthetic pinned facet\npolytoken:\n  model: stub/m2\n---\n\nPinned-facet body.\n"
	if err := os.WriteFile(filepath.Join(facets, "pinned.md"), []byte(facetYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": true, "stub/m2": true, "stub/m3": true})
	d := spawnFeasibilityDaemon(t, work, cfgYAML,
		"--facets-dir", facets, "--facet", "pinned")

	s := d.feasState(t)
	if s["active_facet"] != "pinned" {
		t.Fatalf("active_facet = %v, want pinned", s["active_facet"])
	}
	if s["active_model"] != "stub/m2" {
		t.Fatalf("facet pin should set active model at startup, got %v, want stub/m2", s["active_model"])
	}
	d.driveTurn(t, stub, "hi", "stub-reply-model=m2")
	if req := stub.lastRequest(t); req.Model != "m2" {
		t.Fatalf("facet-pinned turn executed on model %q, want m2", req.Model)
	}
}

// --- same-name layers with a provider-level disable ----------------------------

// feasLayeredGlobalConfig renders the global layer for the same-name layering
// probe. Provider gp owns both global failover leaves; provider pp owns the
// tier-default models, so disabling gp via providers.gp.enabled keeps the
// required full-tier default valid.
func feasLayeredGlobalConfig(stubURL string, providers map[string]bool) string {
	return fmt.Sprintf(`version: 4
providers:
  gp:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
  pp:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
models:
%s%s%s%smodelgroups:
  failover:
    - gp/g1
    - gp/g3
  polytoken:default_model_full: pp/p2
  polytoken:default_model_mini: pp/p4
`, stubURL, providers["gp"], stubURL, providers["pp"],
		feasModelYAML("gp/g1", "gp", "g1", "full", true),
		feasModelYAML("gp/g3", "gp", "g3", "full", true),
		feasModelYAML("pp/p2", "pp", "p2", "full", true),
		feasModelYAML("pp/p4", "pp", "p4", "mini", true))
}

// feasLayeredProjectConfig renders the same-name project-layer failover group:
// its first leaf duplicates the global head leaf and its second leaf is the
// project-only provider's model.
func feasLayeredProjectConfig() string {
	return `version: 4
modelgroups:
  failover:
    - gp/g1
    - pp/p2
`
}

// TestModelGroupsProviderDisableAcrossSameNameLayers pins the same-name
// global-first concatenation at the routing/turn level when the provider
// owning BOTH global leaves is disabled via providers.<id>.enabled: false.
// With every provider enabled the duplicate gp/g1 leaf is preserved at both
// concatenated positions and the pinned group's turn executes on the global
// head; after the successful idle reload the only servable leaf is the
// project-only provider's model, the required full-tier default (pp/p2) keeps
// the config valid throughout, and the pre-reload history survives.
func TestModelGroupsProviderDisableAcrossSameNameLayers(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	proj := filepath.Join(work, "proj", ".polytoken")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "config.yaml"), []byte(feasLayeredProjectConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	d := spawnFeasibilityDaemon(t, work, feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": true, "pp": true}))

	// All-enabled catalog: global leaves first, project leaves appended, and
	// the shared gp/g1 duplicate preserved at both concatenated positions.
	s := d.feasState(t)
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"gp/g1", "gp/g3", "gp/g1", "pp/p2"}) {
		t.Fatalf("all-enabled same-name catalog = %v, want duplicate positions preserved [gp/g1 gp/g3 gp/g1 pp/p2]", got)
	}
	if got := groupCandidates(t, s, "polytoken:general-purpose"); !equalStrings(got, []string{"pp/p2"}) {
		t.Fatalf("full-tier reserved group = %v, want the required default pp/p2", got)
	}

	// Routing/turn level, part 1: with everything enabled the pinned group's
	// first available leaf is the global head — the provider request carries
	// the global provider's wire name.
	d.selectModel(t, "mg:failover")
	if s = d.feasState(t); s["active_model"] != "gp/g1" {
		t.Fatalf("after group pin active = %v, want global head gp/g1", s["active_model"])
	}
	d.driveTurn(t, stub, "layered first turn", "stub-reply-model=g1")
	if req := stub.lastRequest(t); req.Model != "g1" {
		t.Fatalf("all-enabled turn executed on %q, want global head leaf g1", req.Model)
	}

	// Disable the provider owning both global leaves. The required full-tier
	// default pp/p2 stays valid, so the idle reload must succeed outright.
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"),
		[]byte(feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": false, "pp": true})), 0o600); err != nil {
		t.Fatal(err)
	}
	d.reloadIdle(t)

	// Catalog after the reload: every gp leaf drops — including the duplicate
	// project-position gp/g1 copy — leaving exactly the project-only leaf.
	s = d.feasState(t)
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"pp/p2"}) {
		t.Fatalf("post-disable same-name catalog = %v, want [pp/p2]", got)
	}
	if got := groupCandidates(t, s, "polytoken:general-purpose"); !equalStrings(got, []string{"pp/p2"}) {
		t.Fatalf("post-disable full-tier reserved group = %v, want the still-valid default pp/p2", got)
	}
	if s["active_model"] != "pp/p2" {
		t.Fatalf("post-reload active = %v, want the only available leaf pp/p2", s["active_model"])
	}
	snap := d.routingSnapshot(t)
	route, _ := snap["route"].(map[string]any)
	target, _ := route["target"].(map[string]any)
	if target["kind"] != "group" || target["group"] != "failover" {
		t.Fatalf("post-reload routing target = %v, want kind=group group=failover", target)
	}
	if reason, _ := snap["transition_reason"].(string); reason != "reload_reconciliation" {
		t.Fatalf("post-reload transition_reason = %v, want reload_reconciliation", snap["transition_reason"])
	}

	// Routing/turn level, part 2: the next turn (no manual reselect) executes
	// on the project-only provider's leaf, and the pre-reload history is
	// preserved alongside the new reply.
	d.driveTurn(t, stub, "layered second turn", "stub-reply-model=p2")
	if req := stub.lastRequest(t); req.Model != "p2" {
		t.Fatalf("post-disable turn executed on %q, want project-only provider leaf p2", req.Model)
	}
	if req := stub.lastRequest(t); req.AuthHeader != "" {
		t.Fatalf("no_auth provider request carried Authorization: %q", req.AuthHeader)
	}
	_, hb := d.do(t, http.MethodGet, "/history", "")
	if !strings.Contains(string(hb), "stub-reply-model=g1") || !strings.Contains(string(hb), "stub-reply-model=p2") {
		t.Fatalf("history after reload+turn lost a reply; want both g1 and p2 replies, got %.1200s", hb)
	}
}

// --- reload-continuity matrix across selection channels -------------------------

// feasMatrixConfig renders the two-provider fixture for the reload-continuity
// matrix. The selected model always lives on provider stub; provider alt owns
// the surviving leaves, and the full-tier default references the failover
// group, so disabling stub keeps every tier default valid.
func feasMatrixConfig(stubURL string, providers map[string]bool) string {
	return fmt.Sprintf(`version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
  alt:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
models:
%s%s%s%smodelgroups:
  failover:
    - stub/m1
    - alt/a1
  polytoken:default_model_full: mg:failover
  polytoken:default_model_mini: alt/a2
`, stubURL, providers["stub"], stubURL, providers["alt"],
		feasModelYAML("stub/m1", "stub", "m1", "full", true),
		feasModelYAML("stub/m2", "stub", "m2", "mini", true),
		feasModelYAML("alt/a1", "alt", "a1", "full", true),
		feasModelYAML("alt/a2", "alt", "a2", "mini", true))
}

// writeMatrixFacet writes one synthetic facet whose polytoken.model pin is ref
// and returns the facets directory for --facets-dir.
func writeMatrixFacet(t *testing.T, work, name, ref string) string {
	t.Helper()
	facets := filepath.Join(work, "facets")
	if err := os.MkdirAll(facets, 0o700); err != nil {
		t.Fatal(err)
	}
	facetYAML := fmt.Sprintf("---\nname: %s\ndescription: synthetic %s facet\npolytoken:\n  model: %s\n---\n\n%s body.\n", name, name, ref, name)
	if err := os.WriteFile(filepath.Join(facets, name+".md"), []byte(facetYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return facets
}

// assertDegradedReminder requires the post-reload model-group degradation
// reminder in session history — the concrete-pin channels' observed history
// evidence of the reload, since they record neither a routing snapshot nor a
// selection switch when the daemon re-resolves the lost model.
func assertDegradedReminder(t *testing.T, d *feasDaemon) {
	t.Helper()
	_, body := d.do(t, http.MethodGet, "/history", "")
	var h struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decode /history: %v", err)
	}
	for _, it := range h.Items {
		if it["type"] == "system_reminder" {
			if reason, _ := it["reason"].(map[string]any); reason["type"] == "model_group_degraded" {
				return
			}
		}
	}
	t.Fatalf("no model_group_degraded system reminder in history after reload; %.1200s", body)
}

// TestReloadContinuityDisablingSelectedProviderMatrix proves, for each
// remaining selection channel, that a successful idle reload after disabling
// the provider owning the selected model (providers.stub.enabled: false)
// continues the session automatically on the next turn: ordinary default route
// with no explicit selection, a group-pinned facet, a concrete facet pin, and
// a manual selection. (The manually selected group case is already pinned by
// TestDisposableDaemonActiveModelTurnsAndReloadContinuity.) Every case drives
// the daemon against the in-process no-auth SSE stub and proves the serving
// model from the recorded provider request and the session history — never
// from /state alone — and requires the pre-reload reply to survive in history.
// Observed history evidence differs by channel: group-shaped routes record a
// model_routing_v1 snapshot with transition reason reload_reconciliation,
// while concrete-pin channels re-resolve silently and surface a
// model_group_degraded reminder instead.
func TestReloadContinuityDisablingSelectedProviderMatrix(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	cases := []struct {
		name                string
		facetModel          string   // non-empty: spawn with a facet pinning this reference
		manualSelection     string   // non-empty: POST /model this reference before the disable
		wantInitialModel    string   // active model the setup must produce
		wantInitialServe    string   // provider wire name the first turn's request must carry
		wantRoutingSnapshot bool     // group-shaped routes record a model_routing_v1 snapshot
		wantAfterOneOf      []string // wire names the post-reload turn may serve
	}{
		{
			name:                "default-route-no-explicit-selection",
			wantInitialModel:    "stub/m1",
			wantInitialServe:    "m1",
			wantRoutingSnapshot: true,
			wantAfterOneOf:      []string{"a1"},
		},
		{
			name:                "group-pinned-facet",
			facetModel:          "mg:failover",
			wantInitialModel:    "stub/m1",
			wantInitialServe:    "m1",
			wantRoutingSnapshot: true,
			wantAfterOneOf:      []string{"a1"},
		},
		{
			name:             "concrete-facet-pin",
			facetModel:       "stub/m2",
			wantInitialModel: "stub/m2",
			wantInitialServe: "m2",
			// Observed re-resolution lands on the full-tier default group's
			// remaining leaf (the lost pin's mini class does not steer the
			// re-route to the mini default).
			wantAfterOneOf: []string{"a1"},
		},
		{
			name:             "manual-concrete-selection",
			manualSelection:  "stub/m2",
			wantInitialModel: "stub/m2",
			wantInitialServe: "m2",
			wantAfterOneOf:   []string{"a1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubProvider(t)
			work := t.TempDir()
			var args []string
			if tc.facetModel != "" {
				facets := writeMatrixFacet(t, work, "pinned", tc.facetModel)
				args = append(args, "--facets-dir", facets, "--facet", "pinned")
			}
			d := spawnFeasibilityDaemon(t, work, feasMatrixConfig(stub.URL, map[string]bool{"stub": true, "alt": true}), args...)
			if tc.manualSelection != "" {
				d.selectModel(t, tc.manualSelection)
			}

			// Setup sanity on /state, then prove the selected model actually
			// serves: the provider request carries its wire name and the reply
			// lands in history.
			if s := d.feasState(t); s["active_model"] != tc.wantInitialModel {
				t.Fatalf("initial active = %v, want %s", s["active_model"], tc.wantInitialModel)
			}
			d.driveTurn(t, stub, "matrix first turn "+tc.name, "stub-reply-model="+tc.wantInitialServe)
			if req := stub.lastRequest(t); req.Model != tc.wantInitialServe {
				t.Fatalf("initial turn served %q, want %q", req.Model, tc.wantInitialServe)
			}

			// Disable the provider owning the selected model; alt stays up and
			// the tier defaults keep the config valid, so the idle reload must
			// succeed outright (reloadIdle requires HTTP 200, not queued, and
			// an empty failed list).
			if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"),
				[]byte(feasMatrixConfig(stub.URL, map[string]bool{"stub": false, "alt": true})), 0o600); err != nil {
				t.Fatal(err)
			}
			d.reloadIdle(t)

			s := d.feasState(t)
			if s["active_model"] == tc.wantInitialModel {
				t.Fatalf("successful reload left the selected model active: %v", s["active_model"])
			}
			if got := groupCandidates(t, s, "failover"); !equalStrings(got, []string{"alt/a1"}) {
				t.Fatalf("post-disable failover catalog = %v, want [alt/a1]", got)
			}
			// Group-shaped routes record a model_routing_v1 snapshot with the
			// reload transition. Concrete-pin channels record neither a
			// snapshot nor a switch on re-resolution; their observed reload
			// evidence in history is the model-group degradation reminder.
			if tc.wantRoutingSnapshot {
				snap := d.routingSnapshot(t)
				if reason, _ := snap["transition_reason"].(string); reason != "reload_reconciliation" {
					t.Fatalf("post-reload transition_reason = %v, want reload_reconciliation", snap["transition_reason"])
				}
			} else {
				assertDegradedReminder(t, d)
			}

			// Continuity: the next turn runs with no manual reselect. The stub
			// request — not /state — proves which model served, the disabled
			// provider's models never serve, and the reply lands in history.
			before := stub.requestCount()
			if code, body := d.do(t, http.MethodPost, "/prompt", fmt.Sprintf(`{"content":"matrix second turn %s"}`, tc.name)); code != http.StatusAccepted {
				t.Fatalf("POST /prompt = %d: %s", code, body)
			}
			var req stubProviderReq
			deadline := time.Now().Add(30 * time.Second)
			for {
				if now := stub.requestCount(); now > before {
					req = stub.lastRequest(t)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no provider request within 30s after reload (initial active was %q)", tc.wantInitialModel)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if req.Model == "m1" || req.Model == "m2" {
				t.Fatalf("disabled provider stub served the post-reload turn: %q", req.Model)
			}
			allowed := false
			for _, a := range tc.wantAfterOneOf {
				if req.Model == a {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Fatalf("post-reload turn served %q, want one of %v", req.Model, tc.wantAfterOneOf)
			}
			if req.AuthHeader != "" {
				t.Fatalf("no_auth provider request carried Authorization: %q", req.AuthHeader)
			}
			driveWaitHistory(t, d, "stub-reply-model="+req.Model)

			// History preservation: the first turn's reply survives alongside
			// the post-reload reply.
			_, hb := d.do(t, http.MethodGet, "/history", "")
			if !strings.Contains(string(hb), "stub-reply-model="+tc.wantInitialServe) {
				t.Fatalf("history after reload lost the first turn's reply, got %.1200s", hb)
			}
		})
	}
}

// TestProviderGateRecoveryPreservesSessionHistory proves the recovery leg of
// AC.5 against the real binary (COMP-2): after a provider-disable, idle
// reload, and turn cycle, quota's normal-mode restore end state (the provider
// re-enabled) reloads idle and the session AUTOMATICALLY CONTINUES — the next
// turn runs with no manual reselect and the full session history survives —
// for the ordinary default route, a group-pinned facet, a concrete facet pin,
// and a manual selection. The observed binary contract is pinned: the reload
// adopts the restored catalog (failover candidates return to
// [stub/m1, alt/a1]) while the active selection stays on the substitute it
// degraded to; the restored provider is not re-adopted automatically. The
// stub request and reply — never /state alone — prove which model served.
func TestProviderGateRecoveryPreservesSessionHistory(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	cases := []struct {
		name             string
		facetModel       string // non-empty: spawn with a facet pinning this reference
		manualSelection  string // non-empty: POST /model this reference before the disable
		wantInitialModel string
		wantInitialServe string // wire name the first turn's request must carry
	}{
		{
			name:             "default-route-no-explicit-selection",
			wantInitialModel: "stub/m1",
			wantInitialServe: "m1",
		},
		{
			name:             "group-pinned-facet",
			facetModel:       "mg:failover",
			wantInitialModel: "stub/m1",
			wantInitialServe: "m1",
		},
		{
			name:             "concrete-facet-pin",
			facetModel:       "stub/m2",
			wantInitialModel: "stub/m2",
			wantInitialServe: "m2",
		},
		{
			name:             "manual-concrete-selection",
			manualSelection:  "stub/m2",
			wantInitialModel: "stub/m2",
			wantInitialServe: "m2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubProvider(t)
			work := t.TempDir()
			var args []string
			if tc.facetModel != "" {
				facets := writeMatrixFacet(t, work, "pinned", tc.facetModel)
				args = append(args, "--facets-dir", facets, "--facet", "pinned")
			}
			allEnabled := feasMatrixConfig(stub.URL, map[string]bool{"stub": true, "alt": true})
			d := spawnFeasibilityDaemon(t, work, allEnabled, args...)
			if tc.manualSelection != "" {
				d.selectModel(t, tc.manualSelection)
			}
			if s := d.feasState(t); s["active_model"] != tc.wantInitialModel {
				t.Fatalf("initial active = %v, want %s", s["active_model"], tc.wantInitialModel)
			}
			firstReply := "stub-reply-model=" + tc.wantInitialServe
			d.driveTurn(t, stub, "recovery first turn "+tc.name, firstReply)
			if req := stub.lastRequest(t); req.Model != tc.wantInitialServe {
				t.Fatalf("initial turn served %q, want %q", req.Model, tc.wantInitialServe)
			}

			// Gate the provider off; the degraded turn serves the surviving
			// provider (pinned by the disable matrix).
			if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"),
				[]byte(feasMatrixConfig(stub.URL, map[string]bool{"stub": false, "alt": true})), 0o600); err != nil {
				t.Fatal(err)
			}
			d.reloadIdle(t)
			secondReply := "stub-reply-model=a1"
			d.driveTurn(t, stub, "recovery second turn "+tc.name, secondReply)

			// Recovery: quota's normal-mode restore end state writes the
			// provider back enabled and the idle reload succeeds (200, empty
			// failed list). OBSERVED BINARY CONTRACT: the reload ADOPTS the
			// re-enabled catalog but does NOT re-adopt the restored provider
			// as the active selection — the session automatically continues
			// on the substitute it degraded to, with history intact. Any
			// product requirement to switch back automatically is a separate
			// decision; what is pinned here is that continuation never fails
			// and the operator sees the restored catalog.
			if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(allEnabled), 0o600); err != nil {
				t.Fatal(err)
			}
			d.reloadIdle(t)
			if got := groupCandidates(t, d.feasState(t), "failover"); !equalStrings(got, []string{"stub/m1", "alt/a1"}) {
				t.Fatalf("post-restore failover catalog = %v, want the restored [stub/m1, alt/a1]", got)
			}
			thirdReply := "stub-reply-model=a1"
			d.driveTurn(t, stub, "recovery third turn "+tc.name, thirdReply)
			if req := stub.lastRequest(t); req.Model != "a1" {
				t.Fatalf("post-restore turn served %q, want the observed continued substitute a1", req.Model)
			}
			if req := stub.lastRequest(t); req.AuthHeader != "" {
				t.Fatalf("no_auth provider request carried Authorization: %q", req.AuthHeader)
			}

			// All three turns survive in session history.
			_, hb := d.do(t, http.MethodGet, "/history", "")
			for _, want := range []string{firstReply, secondReply, thirdReply} {
				if !strings.Contains(string(hb), want) {
					t.Fatalf("history lost %q; got %.1600s", want, hb)
				}
			}
		})
	}
}

// feasGrouplessConfig renders the COMP-1 probe fixture: a version-4 global
// layer with two providers and two class-full models and NO modelgroups — the
// shipped-default/dynamic-catalog shape whose composition no offline observer
// has pinned.
func feasGrouplessConfig(stubURL string, providers map[string]bool) string {
	return fmt.Sprintf(`version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
  alt:
    kind:
      type: custom_open_ai_compatible
    url: %s
    auth:
      type: no_auth
    enabled: %t
models:
%s%s`, stubURL, providers["stub"], stubURL, providers["alt"],
		feasModelYAML("stub/m1", "stub", "m1", "full", true),
		feasModelYAML("alt/a1", "alt", "a1", "full", true))
}

// TestAnalyzerGrouplessShapeMatchesBinary observes the real binary on the
// exact shape the offline analyzer classifies pending-unknown (COMP-1): a
// version-4 global config with providers and models but no modelgroups at
// all. Pinned observations: (a) the analyzer refuses to call a disable of
// this shape Safe — shipped default-route composition was never authorized
// offline; (b) the binary REJECTS the ambiguous two-provider groupless shape
// outright ("unable to infer Full default from 2 enabled models"); (c) the
// unambiguous single-provider groupless shape boots and serves its only
// model, and gating that provider off is REJECTED wholesale by the reload —
// the stale active selection is retained, never a silent re-composition. A
// reload that accepted a config with no usable default, or a turn served by
// the gated provider afterwards, would strand the session and fail the test.
func TestAnalyzerGrouplessShapeMatchesBinary(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	twoModel := feasGrouplessConfig(stub.URL, map[string]bool{"stub": true, "alt": true})

	// (a) The offline analyzer fails closed on this shape.
	report := groupsafety.Analyze(groupsafety.Input{
		Enrolled: []string{"stub", "alt"},
		Global:   groupsafety.Layer{ID: "global", Global: true, Config: []byte(twoModel)},
	}, "stub")
	if report.Verdict != groupsafety.PendingUnknown {
		t.Fatalf("groupless disable verdict = %q (reasons %v), want pending-unknown", report.Verdict, report.Reasons)
	}

	// (b) The binary rejects the ambiguous groupless shape outright instead of
	// composing shipped defaults from two candidates.
	if ok, out := feasValidate(t, work, twoModel); ok {
		t.Fatalf("two-provider groupless v4 config unexpectedly validates; the binary composes shipped defaults after all: %s", out)
	}

	// (c) The unambiguous single-provider groupless shape loads, serves its
	// only model, and refuses the gate's disable wholesale.
	solo := feasGrouplessConfig(stub.URL, map[string]bool{"stub": true, "alt": false})
	if ok, out := feasValidate(t, work, solo); !ok {
		t.Fatalf("single-provider groupless v4 config failed validation: %s", out)
	}
	d := spawnFeasibilityDaemon(t, work, solo)
	d.driveTurn(t, stub, "groupless first turn", "stub-reply-model=m1")
	if req := stub.lastRequest(t); req.Model != "m1" {
		t.Fatalf("single-provider groupless turn served %q, want m1", req.Model)
	}

	gated := feasGrouplessConfig(stub.URL, map[string]bool{"stub": false, "alt": false})
	if ok, out := feasValidate(t, work, gated); ok {
		t.Fatalf("gating the only enabled model off unexpectedly validates: %s", out)
	}
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(gated), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if !r.Queued && len(r.Failed) == 0 {
		t.Fatalf("reload ACCEPTED a config with no usable full default; the session would strand silently: %s", body)
	}
	if s := d.feasState(t); s["active_model"] != "stub/m1" {
		t.Fatalf("rejected reload did not retain the stale active model: %v", s["active_model"])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
