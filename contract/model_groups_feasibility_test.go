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
