package notice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

const (
	diagnosticFile  = "diagnostics.jsonl"
	diagnosticLimit = 64 * 1024
	reloadBodyLimit = 32 * 1024
)

// diagnosticRecord deliberately has no arbitrary text fields for external input.
// Reasons, categories and events are constants supplied by the handler. Response
// metadata is filtered separately; notice, model, auth and error text never enter it.
type diagnosticRecord struct {
	Schema            int               `json:"schema"`
	Invocation        string            `json:"invocation_id"`
	Timestamp         string            `json:"timestamp"`
	Build             diagnosticBuild   `json:"build"`
	HookEvent         string            `json:"hook_event"`
	Event             string            `json:"event"`
	Reason            string            `json:"reason,omitempty"`
	Revision          uint64            `json:"notice_revision,omitempty"`
	Consumed          uint64            `json:"consumed_revision"`
	Warned            uint64            `json:"last_warned_revision,omitempty"`
	PromptConsumed    uint64            `json:"last_prompt_consumed,omitempty"`
	Artifact          string            `json:"artifact,omitempty"`
	Layout            string            `json:"layout,omitempty"`
	DurationMS        int64             `json:"duration_ms"`
	HTTPStatus        int               `json:"http_status,omitempty"`
	Response          *reloadDiagnostic `json:"response,omitempty"`
	WarningCount      int               `json:"warning_count"`
	WarningCategories []string          `json:"warning_categories,omitempty"`
}

type diagnosticBuild struct {
	GoVersion string `json:"go_version,omitempty"`
	Revision  string `json:"vcs_revision,omitempty"`
	Modified  bool   `json:"vcs_modified,omitempty"`
}

type hookDiagnostics struct {
	root       string
	session    string
	invocation string
	event      string
	build      diagnosticBuild
}

func newHookDiagnostics(d HookDeps, event string) *hookDiagnostics {
	id := d.env("POLYTOKEN_SESSION_ID")
	// A single bounded component, not a path. Do not echo rejected input.
	if !safeSessionID(id) || !filepath.IsAbs(d.sessionsRoot()) {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil
	}
	if event != hookEventTurnEnd && event != hookEventPrompt {
		event = "unknown"
	}
	b := diagnosticBuild{}
	if info, ok := debug.ReadBuildInfo(); ok {
		// Only the toolchain version and a hex VCS revision are retained, never
		// module paths, build flags, environment or arbitrary build settings.
		if strings.HasPrefix(info.GoVersion, "go") && safeBuildToken(info.GoVersion) {
			b.GoVersion = info.GoVersion
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if len(setting.Value) == 40 || len(setting.Value) == 64 {
					if _, err := hex.DecodeString(setting.Value); err == nil {
						b.Revision = setting.Value
					}
				}
			case "vcs.modified":
				b.Modified = setting.Value == "true"
			}
		}
	}
	return &hookDiagnostics{root: d.sessionsRoot(), session: id, invocation: hex.EncodeToString(nonce[:]), event: event, build: b}
}

func safeBuildToken(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func safeSessionID(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (d HookDeps) diagnose(r diagnosticRecord) {
	if d.diagnostics == nil {
		return
	}
	log := d.diagnostics
	r.Schema, r.Invocation, r.Timestamp, r.Build, r.HookEvent = 1, log.invocation, d.now().UTC().Format(time.RFC3339Nano), log.build, log.event
	b, err := json.Marshal(r)
	if err != nil || len(b)+1 > 4096 {
		return
	}
	log.append(append(b, '\n'))
}

// append is best effort and never waits for another invocation's lock. Rooted
// operations confine traversal even if a directory is swapped concurrently.
// Symlinks, hardlinked files and non-private diagnostic files are refused.
// The lock covers rotation and the whole record write, across processes too.
func (l *hookDiagnostics) append(b []byte) {
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil || !ownedDirectory(rootInfo) || rootInfo.Mode().Perm()&0o022 != 0 {
		return
	}
	st, err := root.Lstat(l.session)
	if err != nil || !ownedDirectory(st) || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 {
		return
	}
	session, err := root.OpenRoot(l.session)
	if err != nil {
		return
	}
	defer session.Close()
	opened, err := session.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		return
	}
	if err := session.Mkdir("polytoken-quota", 0o700); err != nil && !os.IsExist(err) {
		return
	}
	st, err = session.Lstat("polytoken-quota")
	if err != nil || !ownedDirectory(st) || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o700 {
		return
	}
	quota, err := session.OpenRoot("polytoken-quota")
	if err != nil {
		return
	}
	defer quota.Close()
	opened, err = quota.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		return
	}
	lock := privateDiagnosticFile(quota, ".diagnostics.lock")
	if lock == nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	f := privateDiagnosticFile(quota, diagnosticFile)
	if f == nil {
		return
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	st, err = f.Stat()
	if err != nil || st.Size() > diagnosticLimit {
		return
	}
	if st.Size()+int64(len(b)) > diagnosticLimit {
		_ = f.Close()
		f = nil
		if quota.Rename(diagnosticFile, diagnosticFile+".1") != nil {
			return
		}
		f = privateDiagnosticFile(quota, diagnosticFile)
		if f == nil {
			return
		}
	}
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return
	}
	if n, err := f.Write(b); err != nil || n != len(b) {
		// Best-effort rollback avoids leaving a partial JSON record after an
		// I/O failure. It is covered by the same lock and never affects hooks.
		_ = f.Truncate(offset)
	}
}

func ownedDirectory(st os.FileInfo) bool {
	if !st.IsDir() {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Getuid())
}

func privateDiagnosticFile(root *os.Root, name string) *os.File {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Nlink != 1 || s.Uid != uint32(os.Getuid()) {
		_ = f.Close()
		return nil
	}
	return f
}

// boundedReloadBody retains only a bounded prefix while preserving the existing
// response drain and timeout. Oversized or unreadable bodies are not parsed.
type boundedReloadBody struct {
	bytes     []byte
	oversized bool
}

func (b *boundedReloadBody) Write(p []byte) (int, error) {
	n := len(p)
	remaining := reloadBodyLimit - len(b.bytes)
	if len(p) > remaining {
		b.oversized = true
		p = p[:remaining]
	}
	b.bytes = append(b.bytes, p...)
	return n, nil
}

type reloadDiagnostic struct {
	Parse              string          `json:"parse"`
	Queued             *bool           `json:"queued,omitempty"`
	Failed             []string        `json:"failed,omitempty"`
	FailedCount        int             `json:"failed_count"`
	UnknownFailedCount int             `json:"unknown_failed_count,omitempty"`
	Subsystems         map[string]bool `json:"subsystems,omitempty"`
}

func parseReloadDiagnostic(b boundedReloadBody, readErr error) *reloadDiagnostic {
	r := &reloadDiagnostic{Parse: "invalid_json"}
	if readErr != nil {
		r.Parse = "read_failed"
		return r
	}
	if b.oversized {
		r.Parse = "too_large"
		return r
	}
	if len(b.bytes) == 0 {
		r.Parse = "empty"
		return r
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b.bytes, &fields) != nil || fields == nil {
		return r
	}
	r.Parse = "parsed"
	if v, ok := fields["queued"]; ok {
		var queued bool
		if string(v) != "null" && json.Unmarshal(v, &queued) == nil {
			r.Queued = &queued
		}
	}
	for _, key := range []string{"facets_changed", "hooks_changed", "permissions_changed", "context_files_reloaded", "extensions_changed", "subagents_changed", "skills_changed", "mcp_servers_changed", "permission_monitor_changed"} {
		if v, ok := fields[key]; ok {
			var changed bool
			if string(v) != "null" && json.Unmarshal(v, &changed) == nil {
				if r.Subsystems == nil {
					r.Subsystems = make(map[string]bool)
				}
				r.Subsystems[key] = changed
			}
		}
	}
	var failed []string
	if json.Unmarshal(fields["failed"], &failed) == nil {
		r.FailedCount = len(failed)
		seen := make(map[string]bool)
		for _, subsystem := range failed {
			switch subsystem {
			case "facets", "hooks", "permissions", "context_files", "extensions", "subagents", "skills", "providers", "active_model", "mcp", "permission_monitor":
				if !seen[subsystem] {
					r.Failed = append(r.Failed, subsystem)
					seen[subsystem] = true
				}
			default:
				r.UnknownFailedCount++
			}
		}
	}
	return r
}

func artifactBytes(path string) ([]byte, string) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "missing"
	}
	if err != nil {
		return nil, "read_failed"
	}
	return b, "available"
}

func (d HookDeps) readState(path string) hookState {
	b, reason := artifactBytes(path)
	var st hookState
	if reason == "available" && json.Unmarshal(b, &st) != nil {
		reason = "invalid_json"
	}
	d.diagnose(diagnosticRecord{Event: "state_check", Reason: reason, Consumed: st.ConsumedRevision, Warned: st.LastWarnedRevision, PromptConsumed: st.LastPromptConsumed})
	return st
}

func (d HookDeps) markerResult(err error, st hookState) {
	reason := "written"
	if err != nil {
		reason = "write_failed"
	}
	d.diagnose(diagnosticRecord{Event: "marker_write", Reason: reason, Consumed: st.ConsumedRevision, Warned: st.LastWarnedRevision, PromptConsumed: st.LastPromptConsumed})
}
