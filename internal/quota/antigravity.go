// antigravity.go implements the Antigravity quota adapter. Antigravity exposes
// no HTTP quota API with a portable credential, so the adapter runs the vendor
// `agy` CLI's local `/quota` slash command in print mode
// (`agy -p /quota --output-format json`) and parses its JSON summary.
//
// Only the Gemini quota group is reported; Claude/GPT and other groups are
// ignored. The payload shape mirrors quota-axi's agy normalizer and was not
// captured live.
//
// The subprocess is shell-free (direct exec), bounded by a timeout and a 1 MiB
// stdout cap, and never surfaces its stderr. So that a logged-out CLI cannot
// open a browser login, PATH is prefixed with a private temporary directory
// holding `open` and `xdg-open` stubs that exit 1; the directory is always
// removed afterwards. The adapter never inspects other processes.
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	antigravityProviderName = "antigravity"
	antigravityTimeout      = 15 * time.Second
	antigravityMaxStdout    = 1 << 20 // 1 MiB
)

// antigravityArgs are the fixed agy arguments; no caller data is interpolated.
var antigravityArgs = []string{"-p", "/quota", "--output-format", "json"}

// AgyRunner is the injectable process seam. Tests pass a fake so no process is
// ever spawned.
type AgyRunner interface {
	// LookPath resolves the agy executable, as exec.LookPath does.
	LookPath(name string) (string, error)
	// Run executes path with args and env, returning bounded stdout.
	Run(ctx context.Context, path string, args, env []string) ([]byte, error)
}

// execAgyRunner is the production runner backed by os/exec.
type execAgyRunner struct{}

func (execAgyRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (execAgyRunner) Run(ctx context.Context, path string, args, env []string) ([]byte, error) {
	var stdout cappedBuffer
	stdout.max = antigravityMaxStdout
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	// Bound the wait for stdout to close after the context kills agy, in case
	// a descendant process inherited the pipe.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = &stdout
	// Stderr is discarded: it can carry account details and is never surfaced.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if stdout.overflow {
		return nil, errors.New("output exceeds size limit")
	}
	return stdout.buf.Bytes(), nil
}

// cappedBuffer keeps at most max bytes and records whether more arrived. It
// keeps accepting writes so the child is not killed by a broken pipe.
type cappedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.overflow = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

// AntigravitySource reports the Antigravity Gemini quota via the agy CLI. It
// satisfies QuotaSource.
type AntigravitySource struct {
	mappingID string
	Runner    AgyRunner
	Evidence  *EvidenceRegistry
	Now       func() time.Time
	// TempDir is the parent for the opener-guard directory; empty uses the
	// system default.
	TempDir string
}

// AntigravityEvidence returns the sanitized contract evidence for the
// Antigravity adapter. The dates are release-owned and do not renew.
func AntigravityEvidence(_ time.Time) Evidence {
	recorded := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	return Evidence{
		Provider:    antigravityProviderName,
		Endpoint:    "exec:agy -p /quota --output-format json",
		Method:      "EXEC",
		AuthType:    "vendor-cli-session",
		SchemaNote:  "command {name:/quota, data:{groups[{displayName, buckets[{bucketId, remainingFraction, resetTime, disabled}]}]}}; Gemini group only; derived from quota-axi's adapter, not captured live",
		FixturePath: "contract/testdata/quota/antigravity/quota.json",
		RecordedAt:  recorded,
		ReviewBy:    recorded.AddDate(0, 3, 0), // quarterly review per evidence policy
	}
}

// NewAntigravitySource constructs an AntigravitySource. A nil runner uses the
// real os/exec runner. If reg is nil the source is unsupported.
func NewAntigravitySource(mappingID string, runner AgyRunner, reg *EvidenceRegistry, now time.Time) *AntigravitySource {
	if reg == nil {
		reg = NewEvidenceRegistry()
	}
	if runner == nil {
		runner = execAgyRunner{}
	}
	return &AntigravitySource{
		mappingID: mappingID,
		Runner:    runner,
		Evidence:  reg,
		Now:       func() time.Time { return now },
	}
}

// MappingID returns the provider mapping this source serves.
func (a *AntigravitySource) MappingID() string { return a.mappingID }

func (a *AntigravitySource) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Status reports whether this source is supported, gated on evidence.
func (a *AntigravitySource) Status() SupportStatus {
	if a.Evidence == nil {
		return SupportFromEvidence(EvidenceStatus{
			State:  EvidenceAbsent,
			Reason: "provider " + antigravityProviderName + " has no recorded contract evidence; record evidence before enabling",
		})
	}
	return SupportFromEvidence(a.Evidence.Status(antigravityProviderName, a.now()))
}

// Fetch runs `agy -p /quota --output-format json` and parses the Gemini
// buckets. It fails closed, running nothing, when the evidence gate is
// unsupported or agy is not installed.
func (a *AntigravitySource) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	st := a.Status()
	if !st.Supported {
		return a.fail(st.Reason), errors.New(st.Reason)
	}
	path, err := a.Runner.LookPath("agy")
	if err != nil || path == "" {
		msg := "antigravity: agy CLI is not installed"
		return a.fail(msg), errors.New(msg)
	}

	out, err := a.run(ctx, path)
	if err != nil {
		msg := SanitizeError(err)
		return a.fail(msg), errors.New(msg)
	}

	windows, partial, perr := parseAntigravityQuota(out)
	if perr != nil {
		msg := SanitizeError(perr)
		return a.fail(msg), errors.New(msg)
	}
	status := SourceFresh
	if partial {
		status = SourcePartial
	}
	return QuotaSnapshot{
		MappingID:    a.mappingID,
		CheckedAt:    a.now(),
		Windows:      windows,
		Availability: determineAvailability(windows),
		Status:       status,
	}, nil
}

func (a *AntigravitySource) fail(reason string) QuotaSnapshot {
	return QuotaSnapshot{
		MappingID:    a.mappingID,
		Availability: QuotaUnknown,
		Status:       SourceFailed,
		Error:        reason,
	}
}

// run executes agy behind the opener guard and a timeout. Errors are fixed
// diagnostics; the child's output is never echoed.
func (a *AntigravitySource) run(ctx context.Context, path string) ([]byte, error) {
	guardDir, err := newOpenerGuard(a.TempDir)
	if err != nil {
		return nil, errors.New("antigravity: could not prepare browser-open guard")
	}
	defer os.RemoveAll(guardDir)

	cctx, cancel := context.WithTimeout(ctx, antigravityTimeout)
	defer cancel()
	env := withPathPrefix(os.Environ(), guardDir)
	out, err := a.Runner.Run(cctx, path, append([]string(nil), antigravityArgs...), env)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("antigravity: agy /quota timed out")
		}
		return nil, errors.New("antigravity: agy /quota failed; check that agy is installed and logged in")
	}
	return out, nil
}

// openerStub is the content of the `open`/`xdg-open` stubs: refuse to open.
const openerStub = "#!/bin/sh\nexit 1\n"

// newOpenerGuard creates a private (0700) directory holding `open` and
// `xdg-open` stubs that exit 1. The caller must remove it.
func newOpenerGuard(parent string) (string, error) {
	dir, err := os.MkdirTemp(parent, "polytoken-quota-agy-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(openerStub), 0o700); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// withPathPrefix returns a copy of env with dir prepended to PATH. All other
// entries are unchanged.
func withPathPrefix(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if found {
				continue
			}
			found = true
			if v == "" {
				kv = "PATH=" + dir
			} else {
				kv = "PATH=" + dir + string(os.PathListSeparator) + v
			}
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

// --- Output parsing -------------------------------------------------------

var antigravityCommandNames = map[string]bool{"quota": true, "/quota": true, "usage": true, "/usage": true}

// parseAntigravityQuota parses agy's JSON print output into Gemini windows.
// A Gemini bucket without a usable remaining fraction sets partial. Wrong
// command names, invalid JSON, and output with no Gemini bucket are errors.
func parseAntigravityQuota(body []byte) (windows []QuotaWindow, partial bool, err error) {
	var root map[string]any
	if json.Unmarshal(bytes.TrimSpace(body), &root) != nil {
		return nil, false, errors.New("antigravity: agy /quota returned invalid JSON")
	}
	command, _ := root["command"].(map[string]any)
	name, _ := command["name"].(string)
	if !antigravityCommandNames[name] {
		return nil, false, errors.New("antigravity: agy output is not a /quota result")
	}
	summary := antigravitySummary(command["data"])
	if summary == nil {
		return nil, false, errors.New("antigravity: agy /quota summary malformed")
	}
	groups, _ := summary["groups"].([]any)
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		groupName := firstString(group, "displayName", "name")
		buckets, _ := group["buckets"].([]any)
		for _, rawBucket := range buckets {
			bucket, ok := rawBucket.(map[string]any)
			if !ok {
				continue
			}
			if disabled, _ := bucket["disabled"].(bool); disabled {
				continue
			}
			bucketID := firstString(bucket, "bucketId", "bucket_id", "id")
			if bucketID == "" {
				continue
			}
			if !strings.Contains(strings.ToLower(groupName+" "+bucketID), "gemini") {
				continue
			}
			w, ok := antigravityWindow(bucket)
			if !ok {
				partial = true
				continue
			}
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, false, errors.New("antigravity: no Gemini quota buckets reported")
	}
	return windows, partial, nil
}

// antigravitySummary selects the quota summary object from command.data: its
// response, its summary, or data itself when it carries groups.
func antigravitySummary(raw any) map[string]any {
	data, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if m, ok := data["response"].(map[string]any); ok {
		return m
	}
	if m, ok := data["summary"].(map[string]any); ok {
		return m
	}
	if _, ok := data["groups"].([]any); ok {
		return data
	}
	return nil
}

// antigravityWindow converts one Gemini bucket. It returns ok=false when the
// bucket has no usable remaining fraction.
func antigravityWindow(bucket map[string]any) (QuotaWindow, bool) {
	frac, ok := antigravityRemainingFraction(bucket)
	if !ok {
		return QuotaWindow{}, false
	}
	used := (1 - clampRange(frac, 0, 1)) * 100
	w := QuotaWindow{UsagePercent: &used}

	kind := strings.ToLower(strings.Join([]string{
		firstString(bucket, "window"), firstString(bucket, "bucketId"), firstString(bucket, "bucket_id"),
		firstString(bucket, "displayName"), firstString(bucket, "name"),
	}, " "))
	switch {
	case strings.Contains(kind, "5h") || strings.Contains(kind, "five"):
		w.Name = "gemini_5h"
		d := 5 * time.Hour
		w.Period = &d
	case strings.Contains(kind, "week"):
		w.Name = "gemini_weekly"
		d := 7 * 24 * time.Hour
		w.Period = &d
	default:
		w.Name = "gemini_quota"
	}
	if t, ok := firstTime(bucket, "resetTime", "reset_time"); ok {
		w.ResetAt = &t
	}
	return w, true
}

// antigravityRemainingFraction reads remainingFraction / remaining_fraction, or
// remaining as a number, an object with remainingFraction, or a protobuf oneof
// {case: "remainingFraction", value}.
func antigravityRemainingFraction(bucket map[string]any) (float64, bool) {
	if f, ok := firstNumber(bucket, "remainingFraction", "remaining_fraction", "remaining"); ok {
		return f, true
	}
	rem, ok := bucket["remaining"].(map[string]any)
	if !ok {
		return 0, false
	}
	if f, ok := firstNumber(rem, "remainingFraction", "remaining_fraction"); ok {
		return f, true
	}
	if c, _ := rem["case"].(string); c == "remainingFraction" || c == "remaining_fraction" {
		return firstNumber(rem, "value")
	}
	return 0, false
}

// firstNumber returns the first finite numeric value (number or numeric
// string) among keys.
func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if finite(v) {
				return v, true
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && finite(f) {
				return f, true
			}
		}
	}
	return 0, false
}

// firstTime returns the first parseable time among keys: an RFC 3339 string,
// or epoch seconds/milliseconds as a number or numeric string.
func firstTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if t, ok := parseFlexibleTime(m[k]); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseFlexibleTime accepts RFC 3339 strings and epoch seconds or milliseconds
// (values above 1e12 are treated as milliseconds).
func parseFlexibleTime(v any) (time.Time, bool) {
	var epoch float64
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, false
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC(), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, false
		}
		epoch = f
	case float64:
		epoch = x
	default:
		return time.Time{}, false
	}
	if epoch <= 0 || !finite(epoch) {
		return time.Time{}, false
	}
	if epoch > 1e12 {
		return millisToTime(int64(epoch)), true
	}
	return time.Unix(int64(epoch), 0).UTC(), true
}

// firstString returns the first non-empty trimmed string among keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

var _ QuotaSource = (*AntigravitySource)(nil)
