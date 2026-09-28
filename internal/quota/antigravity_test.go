package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var antigravityNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// fakeAgyRunner records calls and inspects the opener guard during Run. It
// never spawns a process.
type fakeAgyRunner struct {
	lookErr  error
	out      []byte
	runErr   error
	calls    int
	path     string
	args     []string
	guardDir string
	// guardOK reports whether, during Run, PATH's first entry was a 0700
	// directory holding executable open/xdg-open stubs that exit 1.
	guardOK bool
}

func (f *fakeAgyRunner) LookPath(name string) (string, error) {
	if f.lookErr != nil {
		return "", f.lookErr
	}
	return "/synthetic/bin/" + name, nil
}

func (f *fakeAgyRunner) Run(_ context.Context, path string, args, env []string) ([]byte, error) {
	f.calls++
	f.path, f.args = path, args
	var pathVar string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	f.guardDir = strings.Split(pathVar, string(os.PathListSeparator))[0]
	f.guardOK = guardLooksRight(f.guardDir)
	return f.out, f.runErr
}

func guardLooksRight(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	for _, name := range []string{"open", "xdg-open"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm()&0o100 == 0 {
			return false
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(b), "exit 1") {
			return false
		}
	}
	return true
}

func newAntigravityTestSource(t *testing.T, runner AgyRunner) *AntigravitySource {
	t.Helper()
	reg := NewEvidenceRegistry()
	reg.Register(AntigravityEvidence(antigravityNow))
	src := NewAntigravitySource("agy", runner, reg, antigravityNow)
	src.TempDir = t.TempDir()
	return src
}

const antigravityQuotaJSON = `{"command":{"name":"/quota","data":{"response":{"groups":[
	{"displayName":"Gemini","buckets":[
		{"bucketId":"gemini-5h","remainingFraction":0.75,"resetTime":"2026-08-15T15:00:00Z"},
		{"bucketId":"gemini-weekly","remaining":{"case":"remainingFraction","value":0.4},"resetTime":1787443200000}]},
	{"displayName":"Claude and GPT","buckets":[
		{"bucketId":"3p-5h","remainingFraction":0,"resetTime":"2026-08-15T14:00:00Z"},
		{"bucketId":"3p-weekly","remainingFraction":0.1,"resetTime":"2026-08-20T00:00:00Z"}]}]}}}}`

func TestAntigravityParseGeminiOnly(t *testing.T) {
	runner := &fakeAgyRunner{out: []byte(antigravityQuotaJSON)}
	snap, err := newAntigravityTestSource(t, runner).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The exhausted Claude/GPT 5h bucket must not make the snapshot unavailable.
	if snap.Status != SourceFresh || snap.Availability != QuotaAvailable || len(snap.Windows) != 2 {
		t.Fatalf("snap=%+v", snap)
	}
	want := []struct {
		name   string
		used   float64
		period time.Duration
		reset  time.Time
	}{
		{"gemini_5h", 25, 5 * time.Hour, time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC)},
		{"gemini_weekly", 60, 7 * 24 * time.Hour, time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)},
	}
	for i, w := range want {
		got := snap.Windows[i]
		if got.Name != w.name || got.UsagePercent == nil || !floatNear(*got.UsagePercent, w.used) ||
			got.Period == nil || *got.Period != w.period || got.ResetAt == nil || !got.ResetAt.Equal(w.reset) {
			t.Fatalf("window %d=%+v want %+v", i, got, w)
		}
	}
}

func floatNear(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestAntigravityUnknownBucketKind(t *testing.T) {
	body := `{"command":{"name":"quota","data":{"groups":[{"name":"Gemini","buckets":[{"id":"daily-thing","remaining":0.5}]}]}}}`
	windows, partial, err := parseAntigravityQuota([]byte(body))
	if err != nil || partial || len(windows) != 1 || windows[0].Name != "gemini_quota" || windows[0].Period != nil {
		t.Fatalf("windows=%+v partial=%v err=%v", windows, partial, err)
	}
}

func TestAntigravityGeminiDetectedFromBucketID(t *testing.T) {
	body := `{"command":{"name":"quota","data":{"groups":[{"displayName":"Models","buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.5}]}]}}}`
	windows, _, err := parseAntigravityQuota([]byte(body))
	if err != nil || len(windows) != 1 || windows[0].Name != "gemini_weekly" {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
}

func TestAntigravityIgnoresDisabledBucket(t *testing.T) {
	body := `{"command":{"name":"/usage","data":{"summary":{"groups":[{"name":"Gemini","buckets":[
		{"bucketId":"gemini-5h","disabled":true,"remainingFraction":0},
		{"bucket_id":"gemini-weekly","remaining_fraction":0.9,"reset_time":"2026-08-20T00:00:00Z"}]}]}}}}`
	windows, _, err := parseAntigravityQuota([]byte(body))
	if err != nil || len(windows) != 1 || windows[0].Name != "gemini_weekly" {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
	if !floatNear(*windows[0].UsagePercent, 10) {
		t.Fatalf("usage=%v want 10", *windows[0].UsagePercent)
	}
}

func TestAntigravityBucketWithoutFractionIsPartial(t *testing.T) {
	body := `{"command":{"name":"quota","data":{"groups":[{"name":"Gemini","buckets":[
		{"bucketId":"gemini-5h","resetTime":"2026-08-15T15:00:00Z"},
		{"bucketId":"gemini-weekly","remainingFraction":0.5}]}]}}}`
	windows, partial, err := parseAntigravityQuota([]byte(body))
	if err != nil || !partial || len(windows) != 1 {
		t.Fatalf("windows=%+v partial=%v err=%v", windows, partial, err)
	}
}

func TestAntigravityRejectsWrongCommand(t *testing.T) {
	for _, body := range []string{
		`{"command":{"name":"/help","data":{"groups":[{"name":"Gemini","buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.5}]}]}}}`,
		`{"data":{"groups":[]}}`,
	} {
		if _, _, err := parseAntigravityQuota([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestAntigravityInvalidJSON(t *testing.T) {
	runner := &fakeAgyRunner{out: []byte("Please log in at https://example.invalid\n")}
	snap, err := newAntigravityTestSource(t, runner).Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || !strings.Contains(snap.Error, "invalid JSON") {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if strings.Contains(snap.Error, "log in") {
		t.Fatal("raw CLI output leaked into the error")
	}
}

func TestAntigravityNoGeminiBuckets(t *testing.T) {
	body := `{"command":{"name":"quota","data":{"groups":[{"displayName":"Claude and GPT","buckets":[{"bucketId":"3p-weekly","remainingFraction":0.5}]}]}}}`
	_, _, err := parseAntigravityQuota([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "no Gemini quota buckets") {
		t.Fatalf("err=%v", err)
	}
}

func TestAntigravityNotInstalledExecutesNothing(t *testing.T) {
	runner := &fakeAgyRunner{lookErr: errors.New(`exec: "agy": executable file not found in $PATH`)}
	snap, err := newAntigravityTestSource(t, runner).Fetch(context.Background())
	if err == nil || snap.Status != SourceFailed || snap.Error != "antigravity: agy CLI is not installed" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner called %d times", runner.calls)
	}
}

func TestAntigravityOpenerGuard(t *testing.T) {
	for name, runner := range map[string]*fakeAgyRunner{
		"success": {out: []byte(antigravityQuotaJSON)},
		"error":   {runErr: errors.New("exit status 1: token=synthetic-secret")},
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := newAntigravityTestSource(t, runner).Fetch(context.Background())
			if runner.calls != 1 || runner.path != "/synthetic/bin/agy" {
				t.Fatalf("calls=%d path=%q", runner.calls, runner.path)
			}
			if !reflect.DeepEqual(runner.args, []string{"-p", "/quota", "--output-format", "json"}) {
				t.Fatalf("args=%v", runner.args)
			}
			if !runner.guardOK {
				t.Fatalf("opener guard %q was not a private dir with blocking stubs", runner.guardDir)
			}
			if _, statErr := os.Stat(runner.guardDir); !os.IsNotExist(statErr) {
				t.Fatalf("guard dir %q not removed: %v", runner.guardDir, statErr)
			}
			if name == "error" {
				if err == nil || snap.Status != SourceFailed || strings.Contains(snap.Error, "synthetic-secret") {
					t.Fatalf("snap=%+v err=%v", snap, err)
				}
			} else if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
		})
	}
}

func TestWithPathPrefix(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := withPathPrefix([]string{"HOME=/h", "PATH=/usr/bin" + sep + "/bin", "LANG=C"}, "/guard")
	want := []string{"HOME=/h", "PATH=/guard" + sep + "/usr/bin" + sep + "/bin", "LANG=C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := withPathPrefix([]string{"HOME=/h"}, "/guard"); !reflect.DeepEqual(got, []string{"HOME=/h", "PATH=/guard"}) {
		t.Fatalf("missing PATH: got %v", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := cappedBuffer{max: 4}
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, err := c.Write([]byte("def")); n != 3 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c.buf.String() != "abcd" || !c.overflow {
		t.Fatalf("buf=%q overflow=%v", c.buf.String(), c.overflow)
	}
}

func TestAntigravityUnsupportedWithoutEvidence(t *testing.T) {
	runner := &fakeAgyRunner{out: []byte(antigravityQuotaJSON)}
	src := NewAntigravitySource("agy", runner, nil, antigravityNow)
	if src.Status().Supported {
		t.Fatal("source without evidence reported supported")
	}
	if _, err := src.Fetch(context.Background()); err == nil || runner.calls != 0 {
		t.Fatalf("err=%v calls=%d; want fail closed", err, runner.calls)
	}
}

func TestAntigravityEvidenceFresh(t *testing.T) {
	e := AntigravityEvidence(time.Time{})
	if st := EvaluateEvidence(&e, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)); st.State != EvidenceFresh {
		t.Fatalf("evidence state=%s reason=%q", st.State, st.Reason)
	}
	if e.Method != "EXEC" || e.AuthType != "vendor-cli-session" {
		t.Fatalf("evidence=%+v", e)
	}
}
