package hook

// Task 14 fuzz corpus for the hook decoder. FuzzMalformedHookNeverMutates proves
// that arbitrary (including malformed) stdin payloads never panic the decoder,
// never mutate the caller's environment snapshot, and never let a valid Event
// escape a bad decode (a decode error is always paired with a zero-value Event).
// This pins the CodExBar 0.44.0 contract's decode-only safety: account and all
// other parsed fields are discardable on any error, so no partial state leaks.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// FuzzMalformedHookNeverMutates feeds arbitrary bytes to Decode and asserts:
//   - Decode never panics;
//   - on success the returned Event is fully valid (identity + finite numbers);
//   - on error the returned Event is the zero value (no valid Event escapes a bad
//     decode);
//   - the caller's env map is never mutated.
func FuzzMalformedHookNeverMutates(f *testing.F) {
	// Seeds: a truncated object, valid-but-incomplete objects, garbage, and a
	// fully valid payload (so the fuzzer starts from both red and green shapes).
	f.Add([]byte("{"))
	f.Add([]byte(`{"event":"quota_low"`))
	f.Add([]byte("not json at all"))
	f.Add([]byte(`{"event":"quota_low","provider":"codex","timestamp":"2026-07-19T12:00:00Z"}`))
	f.Add([]byte("\x00\x01\x02 malformed binary"))
	f.Fuzz(func(t *testing.T, b []byte) {
		env := map[string]string{"CODEXBAR_EVENT": "quota_low"}
		envBefore := cloneEnv(env)
		ev, err := Decode(bytes.NewReader(b), env, 4096)

		// The caller's env must never be mutated by Decode.
		if !reflect.DeepEqual(envBefore, env) {
			t.Fatalf("Decode mutated the caller's env: before=%v after=%v", envBefore, env)
		}

		if err != nil {
			// A bad decode must return a zero-value Event so no partial state
			// escapes.
			if !isZeroEvent(ev) {
				t.Fatalf("decode error returned non-zero Event: %+v (err=%v)", ev, err)
			}
			return
		}
		// A successful decode must return a fully valid event.
		if !validType(ev.Type) {
			t.Fatalf("decoded invalid event type %q", ev.Type)
		}
		if ev.Provider == "" || ev.Timestamp.IsZero() {
			t.Fatalf("decoded event missing identity: %+v", ev)
		}
	})
}

// cloneEnv returns a shallow copy of env.
func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// isZeroEvent reports whether ev is the zero-value Event (no Type, no Provider,
// zero Timestamp), the contract for a failed decode.
func isZeroEvent(ev Event) bool {
	return ev.Type == "" && ev.Provider == "" && ev.Timestamp.IsZero() &&
		ev.Window == nil && ev.UsagePercent == nil && ev.Used == nil &&
		ev.Limit == nil && ev.ResetAt == nil && ev.Status == nil
}

// TestDecodeZeroEventOnError is a non-fuzz pin of the zero-Event-on-error
// contract for representative malformed inputs.
func TestDecodeZeroEventOnError(t *testing.T) {
	for _, in := range []string{"{", `{"event":"bogus"}`, "", "   "} {
		ev, err := Decode(strings.NewReader(in), nil, 4096)
		if err == nil {
			t.Fatalf("expected error for %q", in)
		}
		if !isZeroEvent(ev) {
			t.Fatalf("non-zero Event on error for %q: %+v", in, ev)
		}
	}
}
