package notice

// The in-session hook handler. Polytoken invokes this subcommand (via the
// operator-installed hooks.json entries) at session boundaries; it dispatches
// on POLYTOKEN_HOOK_EVENT:
//
//   - post_model_turn (fire-and-forget): when the published notice carries a
//     revision newer than this session's consumed marker, check /state for
//     explicit idle activity, resolving omitted/null legacy activity only with
//     a matching idle /sync and /sync/summary pair before POST /reload to this
//     session's own daemon (loopback address and bearer token from
//     startup.json/credential.json).
//     Unresolved or active state defers without changing the marker. This check
//     is not atomic with reload. Only a 200 advances the
//     marker: 409 (turn in flight), 422, 401, and transport errors leave it
//     unadvanced so the next event retries naturally. Every failure is a
//     silent no-op — the hook must never disturb a session.
//
// The handler acts solely on its own session's daemon via the documented
// loopback API with that session's own credential (see AGENTS.md: Scoped
// daemon interaction).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookEventTurnEnd and hookEventPrompt are the two Polytoken hook events the
// handler subscribes to (installed by `install-hook`).
const (
	hookEventTurnEnd = "post_model_turn"
	hookEventPrompt  = "pre_user_prompt"
)

// HookDeps carries the handler's explicit inputs. Production fills them from
// command flags and the daemon-provided environment; tests inject fakes.
type HookDeps struct {
	// NoticePath is the notice file to consume; empty resolves the default.
	NoticePath string
	// SessionsDir is the daemon sessions root
	// (~/.local/share/polytoken/sessions by default).
	SessionsDir string
	// HTTPClient performs own-session activity and reload calls; nil uses a
	// client with a 5s timeout per request.
	HTTPClient *http.Client
	// Stdout receives the JSON decision object for blocking events.
	Stdout io.Writer
	// Environ looks up daemon-provided environment variables
	// (POLYTOKEN_HOOK_EVENT, POLYTOKEN_SESSION_ID).
	Environ func(string) string
	// Now stamps state transitions; nil uses time.Now.
	Now func() time.Time
	// diagnostics is invocation-local; no caller can inject external log text.
	diagnostics *hookDiagnostics
}

// hookState is the per-session marker file persisted under
// <sessions-dir>/<session-id>/polytoken-quota/state.json.
type hookState struct {
	ConsumedRevision uint64 `json:"consumed_revision,omitempty"`
	// LastSeenModel is the active model observed at the previous
	// pre_user_prompt; empty until the first prompt.
	LastSeenModel string `json:"last_seen_model,omitempty"`
	// LastWarnedRevision suppresses repeat drift warnings for one consumed
	// revision (all tiers compose into a single message).
	LastWarnedRevision uint64 `json:"last_warned_revision,omitempty"`
	// LastPromptConsumed is the consumed revision observed at the previous
	// prompt; a model change paired with a newer consumed revision is the
	// reload-forced-switch signature.
	LastPromptConsumed uint64 `json:"last_prompt_consumed,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
}

// RunHook executes the handler for one hook event and returns the process
// exit code. It always returns 0: hook failures are silent no-ops and
// blocking-event errors fail open.
func RunHook(d HookDeps) int {
	event := d.env("POLYTOKEN_HOOK_EVENT")
	d.diagnostics = newHookDiagnostics(d, event)
	started := time.Now()
	d.diagnose(diagnosticRecord{Event: "hook_started"})
	defer func() {
		d.diagnose(diagnosticRecord{Event: "hook_finished", DurationMS: time.Since(started).Milliseconds()})
	}()
	switch event {
	case hookEventTurnEnd:
		d.reloadIfStale()
		return 0
	case hookEventPrompt:
		d.handlePrompt()
		return 0
	default:
		d.diagnose(diagnosticRecord{Event: "noop", Reason: "unsupported_event"})
		return 0
	}
}

// noticeDoc is the subset of the notice document the warning tiers consume.
type noticeDoc struct {
	Schema         int            `json:"schema"`
	Revision       uint64         `json:"revision"`
	ProviderOnly   bool           `json:"provider_only"`
	Targets        []noticeTarget `json:"targets"`
	DisabledModels []*string      `json:"disabled_models"`
}

type noticeTarget struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Facet  string `json:"facet"`
	Chains []struct {
		Name   string    `json:"name"`
		Models []*string `json:"models"`
	} `json:"chains"`
	Chain []*string `json:"chain"`
}

// handlePrompt evaluates the drift tiers and the forced-switch note for one
// pre_user_prompt, printing the fail-open accept decision (with
// additional_context when a warning fires). The active model and
// last-prompt-consumed markers update every prompt regardless of warnings.
func (d HookDeps) handlePrompt() {
	current := d.env("POLYTOKEN_MODEL_NAME")
	sessDir, sessionID := d.sessionDir()
	if current == "" || sessionID == "" {
		d.diagnose(diagnosticRecord{Event: "noop", Reason: "missing_prompt_inputs"})
		d.printAccept()
		return
	}
	statePath := d.statePath(sessDir)
	state := d.readState(statePath)

	var lines []string
	var categories []string
	if doc, ok := d.readNotice(); ok && !doc.ProviderOnly {
		chain, head := applicableChain(doc, d.env("POLYTOKEN_FACET_NAME"))
		inChain := chainContains(chain, current)
		// Drift warnings fire at most once per published notice revision (the
		// revision that caused the drift), so a fresh marker (0) never
		// collides with a real revision (>= 1).
		if !inChain && state.LastWarnedRevision != doc.Revision {
			if chainContains(doc.DisabledModels, current) {
				lines = append(lines, actionableWarning(doc.Revision, current, head))
				categories = append(categories, "disabled_model")
			} else {
				lines = append(lines, informationalWarning(doc.Revision, current, head))
				categories = append(categories, "outside_chain")
			}
			state.LastWarnedRevision = doc.Revision
		}
		if state.LastSeenModel != "" && current != state.LastSeenModel && state.ConsumedRevision > state.LastPromptConsumed {
			lines = append(lines, forcedSwitchNote(state.ConsumedRevision, state.LastSeenModel, current))
			categories = append(categories, "forced_switch")
		}
		if len(lines) == 0 {
			reason := "in_chain"
			if !inChain {
				reason = "warning_already_reported"
			}
			d.diagnose(diagnosticRecord{Event: "noop", Reason: reason, Revision: doc.Revision, Consumed: state.ConsumedRevision})
		}
	}

	d.diagnose(diagnosticRecord{Event: "prompt_warnings", WarningCount: len(categories), WarningCategories: categories})
	state.LastSeenModel = current
	state.LastPromptConsumed = state.ConsumedRevision
	state.UpdatedAt = d.now().UTC().Format(time.RFC3339)
	d.markerResult(writeHookState(statePath, state), state)

	if len(lines) == 0 {
		d.printAccept()
		return
	}
	type decision struct {
		Outcome           string `json:"outcome"`
		AdditionalContext string `json:"additional_context"`
	}
	b, err := json.Marshal(decision{Outcome: "accept", AdditionalContext: strings.Join(lines, "\n")})
	if err != nil {
		d.printAccept()
		return
	}
	fmt.Fprintln(d.out(), string(b))
}

func (d HookDeps) printAccept() {
	fmt.Fprintln(d.out(), `{"outcome":"accept"}`)
}

// readNotice parses the full notice document; missing or corrupt files are
// "no news" and suppress warnings without failing the prompt.
func (d HookDeps) readNotice() (noticeDoc, bool) {
	b, reason := d.noticeBytes()
	var doc noticeDoc
	if reason == "available" {
		if json.Unmarshal(b, &doc) != nil {
			reason = "invalid_json"
		} else if doc.Schema != SchemaVersion {
			reason = "unsupported_schema"
		}
	}
	d.noticeCheck(reason, doc.Revision, doc.ProviderOnly)
	if reason != "available" {
		return noticeDoc{}, false
	}
	return doc, true
}

// applicableChain selects the chain the session is held to: the definition
// target whose facet matches the active facet name, else the global target's
// full chain. Absent, empty, or non-matching facet variables fall back to the
// global full chain. The returned head is the first non-null chain entry.
func applicableChain(doc noticeDoc, facet string) (chain []*string, head string) {
	if facet != "" {
		for _, t := range doc.Targets {
			if t.Kind == "definition" && t.Facet == facet {
				return t.Chain, chainHead(t.Chain)
			}
		}
	}
	for _, t := range doc.Targets {
		if t.Kind != "global" {
			continue
		}
		for _, ch := range t.Chains {
			if ch.Name == "full" {
				return ch.Models, chainHead(ch.Models)
			}
		}
	}
	return nil, ""
}

func chainHead(chain []*string) string {
	for _, m := range chain {
		if m != nil {
			return *m
		}
	}
	return ""
}

func chainContains(chain []*string, model string) bool {
	for _, m := range chain {
		if m != nil && *m == model {
			return true
		}
	}
	return false
}

func actionableWarning(rev uint64, model, head string) string {
	msg := fmt.Sprintf("[polytoken-quota] revision %d: this session is running %s, which is disabled (its provider can no longer serve it)", rev, model)
	if head != "" {
		msg += fmt.Sprintf("; the configured chain head is %s and convergence falls back to it", head)
	}
	msg += ". Switch at a convenient boundary — note a provider switch starts with uncached context."
	return msg
}

func informationalWarning(rev uint64, model, head string) string {
	msg := fmt.Sprintf("[polytoken-quota] revision %d: this session is running %s, which is enabled but outside the configured chain", rev, model)
	if head != "" {
		msg += fmt.Sprintf(" (head: %s)", head)
	}
	msg += ". Switching is optional; a provider switch starts with uncached context."
	return msg
}

func forcedSwitchNote(rev uint64, from, to string) string {
	return fmt.Sprintf("[polytoken-quota] revision %d: this session's model changed from %s to %s (the prior model was removed by the reconciliation); context on the new provider started uncached.", rev, from, to)
}

// reloadIfStale performs the post-turn reload convergence. Silent on every
// failure path by design.
func (d HookDeps) reloadIfStale() {
	revision, ok := d.noticeRevision()
	if !ok {
		return
	}
	sessDir, sessionID := d.sessionDir()
	if sessionID == "" {
		return
	}
	state := d.readState(d.statePath(sessDir))
	if state.ConsumedRevision >= revision {
		d.diagnose(diagnosticRecord{Event: "noop", Reason: "revision_consumed", Revision: revision, Consumed: state.ConsumedRevision})
		return
	}
	runtimeDir := filepath.Join(filepath.Dir(d.sessionsRoot()), "sessions-v1", sessionID)
	layout := "runtime"
	port, ok := d.readPort(runtimeDir, layout)
	if !ok {
		layout = "session"
		port, ok = d.readPort(sessDir, layout) // compatibility with session layouts that keep artifacts together
		if !ok {
			return
		}
		runtimeDir = sessDir
	}
	token, ok := d.readToken(runtimeDir, layout)
	if !ok {
		return
	}
	if !d.daemonIdle(port, token) || !d.postReload(port, token) {
		return
	}
	state.ConsumedRevision = revision
	state.UpdatedAt = d.now().UTC().Format(time.RFC3339)
	d.markerResult(writeHookState(d.statePath(sessDir), state), state)
}

// noticeRevision reads just the revision from the notice document. Missing or
// corrupt files are "no news".
func (d HookDeps) noticeRevision() (uint64, bool) {
	b, reason := d.noticeBytes()
	var doc struct {
		Schema   int    `json:"schema"`
		Revision uint64 `json:"revision"`
	}
	if reason == "available" {
		if json.Unmarshal(b, &doc) != nil {
			reason = "invalid_json"
		} else if doc.Schema != SchemaVersion {
			reason = "unsupported_schema"
		}
	}
	d.noticeCheck(reason, doc.Revision, false)
	if reason != "available" {
		return 0, false
	}
	return doc.Revision, true
}

func (d HookDeps) noticeBytes() ([]byte, string) {
	path := d.NoticePath
	if path == "" {
		var err error
		path, err = ResolvePath("")
		if err != nil {
			return nil, "path_unavailable"
		}
	}
	return artifactBytes(path)
}

func (d HookDeps) noticeCheck(reason string, revision uint64, providerOnly bool) {
	if reason == "available" && providerOnly {
		reason = "provider_only"
	}
	d.diagnose(diagnosticRecord{Event: "notice_check", Reason: reason, Revision: revision})
}

func (d HookDeps) sessionDir() (dir, sessionID string) {
	sessionID = d.env("POLYTOKEN_SESSION_ID")
	if sessionID == "" {
		return "", ""
	}
	return filepath.Join(d.sessionsRoot(), sessionID), sessionID
}

func (d HookDeps) sessionsRoot() string {
	if d.SessionsDir != "" {
		return d.SessionsDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "polytoken", "sessions")
}

func (d HookDeps) statePath(sessDir string) string {
	return filepath.Join(sessDir, "polytoken-quota", "state.json")
}

// postReload issues the authenticated reload call. True only on HTTP 200.
func (d HookDeps) postReload(port int, token string) bool {
	started := time.Now()
	result := diagnosticRecord{Event: "reload_result", Reason: "transport_failed"}
	d.diagnose(diagnosticRecord{Event: "reload_request"})
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
		d.diagnose(result)
	}()
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/reload", port)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body boundedReloadBody
	_, readErr := io.Copy(&body, resp.Body)
	result.HTTPStatus = resp.StatusCode
	result.Reason = "http_non_200"
	if resp.StatusCode == http.StatusOK {
		result.Reason = "http_200"
	}
	result.Response = parseReloadDiagnostic(body, readErr)
	if resp.StatusCode == http.StatusOK && result.Response.Queued != nil && *result.Response.Queued {
		result.Reason = "http_200_queued_unexpected"
	}
	return resp.StatusCode == http.StatusOK
}

func (d HookDeps) readPort(sessDir, layout string) (int, bool) {
	b, reason := artifactBytes(filepath.Join(sessDir, "startup.json"))
	var doc struct {
		Port int `json:"port"`
	}
	if reason == "available" {
		if json.Unmarshal(b, &doc) != nil {
			reason = "invalid_json"
		} else if doc.Port <= 0 {
			reason = "invalid_port"
		}
	}
	d.diagnose(diagnosticRecord{Event: "artifact_check", Artifact: "startup", Layout: layout, Reason: reason})
	return doc.Port, reason == "available"
}

func (d HookDeps) readToken(sessDir, layout string) (string, bool) {
	b, reason := artifactBytes(filepath.Join(sessDir, "credential.json"))
	var doc struct {
		Token string `json:"token"`
	}
	if reason == "available" {
		if json.Unmarshal(b, &doc) != nil {
			reason = "invalid_json"
		} else if doc.Token == "" {
			reason = "empty_token"
		}
	}
	d.diagnose(diagnosticRecord{Event: "artifact_check", Artifact: "credential", Layout: layout, Reason: reason})
	return doc.Token, reason == "available"
}

func readHookState(path string) hookState {
	var st hookState
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

func writeHookState(path string, st hookState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (d HookDeps) env(key string) string {
	if d.Environ == nil {
		return os.Getenv(key)
	}
	return d.Environ(key)
}

func (d HookDeps) out() io.Writer {
	if d.Stdout == nil {
		return os.Stdout
	}
	return d.Stdout
}

func (d HookDeps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}
