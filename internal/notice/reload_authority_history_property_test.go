package notice

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type authorityHistoryMode uint8

const (
	historyPairedIdle authorityHistoryMode = iota
	historyEpochDisagrees
	historyCursorDisagrees
	historyBusy
)

type authorityHistoryAction uint8

type authorityHistoryChoice struct {
	action authorityHistoryAction
	value  uint64
}

func noticeAuthorityHistoryChoices(seed uint64, count int) []authorityHistoryChoice {
	rng := newDeliveryRNG(seed + 0x517cc1b727220a95)
	choices := make([]authorityHistoryChoice, count)
	for i := range choices {
		action := authorityHistoryAction(rng.next() % 4)
		value := rng.next()
		if action == historyRevision {
			value %= 16
		}
		choices[i] = authorityHistoryChoice{action: action, value: value}
	}
	return choices
}

func noticeAuthorityHistoryTrace(choices []authorityHistoryChoice) [32]byte {
	trace := make([]byte, 0, len(choices)*9)
	for _, choice := range choices {
		trace = append(trace, byte(choice.action))
		for shift := 56; shift >= 0; shift -= 8 {
			trace = append(trace, byte(choice.value>>uint(shift)))
		}
	}
	return sha256.Sum256(trace)
}

const (
	historyFire authorityHistoryAction = iota
	historyRevision
	historyAuthority
	historyReloadStatus
)

type authorityHistoryTransport struct {
	t                        *testing.T
	mode                     authorityHistoryMode
	reloadStatus             int
	failMarker, markerFailed bool
	oldMarker                []byte
	statePath                string
	calls                    []string
	loads                    int
}

func (f *authorityHistoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Helper()
	if req.Header.Get("Authorization") != "Bearer tok-1" {
		return nil, fmt.Errorf("unexpected synthetic credential")
	}
	key := req.Method + " " + req.URL.Path
	f.calls = append(f.calls, key)
	status, body := http.StatusOK, "{}"
	switch key {
	case "GET /state":
		body = `{"turn_in_flight":null}`
	case "GET /sync":
		body = `{"version":1,"daemon_epoch":"test-epoch","cursor":"c1","turn":null,"queue":{"revision":"q1","items":[]},"interrogatives":[]}`
	case "GET /sync/summary":
		epoch, cursor, busy := "test-epoch", "c1", false
		switch f.mode {
		case historyEpochDisagrees:
			epoch = "other-epoch"
		case historyCursorDisagrees:
			cursor = "c2"
		case historyBusy:
			busy = true
		}
		body = fmt.Sprintf(`{"version":1,"daemon_epoch":%q,"cursor":%q,"busy":%t,"attention":false,"history_checkpoint":"cp","history_total_items":0}`, epoch, cursor, busy)
	case "POST /reload":
		f.loads++
		status = f.reloadStatus
		if status == http.StatusOK && f.failMarker {
			var err error
			f.oldMarker, err = os.ReadFile(f.statePath)
			if err != nil {
				return nil, fmt.Errorf("read marker fixture: %w", err)
			}
			if err = os.Remove(f.statePath); err != nil {
				return nil, fmt.Errorf("remove marker fixture: %w", err)
			}
			if err = os.Mkdir(f.statePath, 0o700); err != nil {
				return nil, fmt.Errorf("obstruct marker fixture: %w", err)
			}
			f.markerFailed = true
			f.failMarker = false
		}
	default:
		return nil, fmt.Errorf("unexpected synthetic request %s", key)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

type authorityHistoryModel struct {
	revision, consumed uint64
	mode               authorityHistoryMode
	reload             int
	failMarker         bool
	loads              int
	wantRequest        []string
}

func (m *authorityHistoryModel) observe() {
	if m.revision <= m.consumed {
		return
	}
	m.wantRequest = append(m.wantRequest, "GET /state", "GET /sync", "GET /sync/summary")
	if m.mode != historyPairedIdle {
		return
	}
	m.wantRequest = append(m.wantRequest, "POST /reload")
	m.loads++
	if m.reload != http.StatusOK || m.failMarker {
		m.failMarker = false
		return
	}
	m.consumed = m.revision
}

// Fake transport histories; no live daemon or session is contacted.
func TestReloadAuthorityGeneratedHistory(t *testing.T) {
	cases, suffix := 16, 28
	mode := os.Getenv("NOTICE_HISTORY_MODE")
	if mode == "" {
		mode = "fast"
	}
	if mode != "fast" && mode != "full" {
		t.Fatalf("invalid NOTICE_HISTORY_MODE %q (want fast or full)", mode)
	}
	seedBase := uint64(0)
	if raw := os.Getenv("NOTICE_HISTORY_SEED"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 {
			t.Fatalf("invalid NOTICE_HISTORY_SEED %q", raw)
		}
		seedBase = parsed
	}
	if mode == "full" {
		cases = 48
	}
	if raw := os.Getenv("NOTICE_AUTHORITY_CASES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			t.Fatalf("invalid NOTICE_AUTHORITY_CASES %q", raw)
		}
		cases = parsed
	}
	for offset := uint64(0); offset < uint64(cases); offset++ {
		seed := seedBase + offset
		t.Run(fmt.Sprintf("seed-%02d", seed), func(t *testing.T) {
			h := newHookHarness(t)
			t.Cleanup(h.server.Close)
			if err := os.Remove(h.statePath()); err != nil {
				t.Fatal(err)
			}
			base := uint64(4)
			writeJSON(t, h.noticePath, map[string]any{"schema": SchemaVersion, "revision": base})
			writeJSON(t, h.statePath(), hookState{BaselineRevision: &base, ConsumedRevision: base})
			tr := &authorityHistoryTransport{t: t, reloadStatus: http.StatusOK, statePath: h.statePath()}
			deps := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, HTTPClient: &http.Client{Transport: tr}, Stdout: io.Discard, Environ: func(k string) string { return h.env[k] }, Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }}
			m := authorityHistoryModel{revision: 4, consumed: 4, reload: http.StatusOK}
			apply := func(a authorityHistoryAction, v uint64) {
				t.Helper()
				switch a {
				case historyRevision:
					m.revision = v
					writeJSON(t, h.noticePath, map[string]any{"schema": SchemaVersion, "revision": m.revision})
				case historyAuthority:
					m.mode, tr.mode = authorityHistoryMode(v%4), authorityHistoryMode(v%4)
				case historyReloadStatus:
					statuses := [...]int{http.StatusOK, http.StatusConflict, http.StatusUnauthorized, http.StatusServiceUnavailable}
					m.reload, tr.reloadStatus = statuses[v%uint64(len(statuses))], statuses[v%uint64(len(statuses))]
				case historyFire:
					m.wantRequest = nil
					m.observe()
					tr.calls = nil
					if RunHook(deps) != 0 {
						t.Fatal("hook did not fail open")
					}
					if tr.markerFailed {
						if err := os.RemoveAll(h.statePath()); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(h.statePath(), tr.oldMarker, 0o600); err != nil {
							t.Fatal(err)
						}
						tr.markerFailed = false
					}
					if strings.Join(tr.calls, ",") != strings.Join(m.wantRequest, ",") {
						t.Fatalf("request order %v want %v", tr.calls, m.wantRequest)
					}
					st := readHookState(h.statePath())
					if st.ConsumedRevision != m.consumed || st.BaselineRevision == nil || *st.BaselineRevision != base {
						t.Fatalf("marker=%+v model consumed=%d", st, m.consumed)
					}
				}
			}
			apply(historyFire, 0)
			apply(historyRevision, 9)
			apply(historyAuthority, uint64(historyPairedIdle))
			apply(historyReloadStatus, 0)
			apply(historyFire, 0)
			apply(historyAuthority, uint64(historyEpochDisagrees))
			apply(historyFire, 0)
			apply(historyAuthority, uint64(historyPairedIdle))
			apply(historyReloadStatus, 1)
			apply(historyFire, 0)
			apply(historyReloadStatus, 0)
			tr.failMarker, m.failMarker = true, true
			apply(historyFire, 0)
			if err := os.RemoveAll(h.statePath()); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, h.statePath(), hookState{BaselineRevision: &base, ConsumedRevision: m.consumed})
			apply(historyFire, 0)
			choices := noticeAuthorityHistoryChoices(seed, suffix)
			for _, choice := range choices {
				apply(choice.action, choice.value)
			}
			t.Logf("semantic_trace=%x", noticeAuthorityHistoryTrace(choices))
		})
	}
}
func TestReloadAuthorityStateLockContentionSkipsThenRetries(t *testing.T) {
	h := newHookHarness(t)
	t.Cleanup(h.server.Close)
	tr := &authorityHistoryTransport{t: t, reloadStatus: http.StatusOK, statePath: h.statePath()}
	deps := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, HTTPClient: &http.Client{Transport: tr}, Stdout: io.Discard, Environ: func(k string) string { return h.env[k] }}
	base := uint64(4)
	writeJSON(t, h.statePath(), hookState{BaselineRevision: &base, ConsumedRevision: base})
	locked, ok := deps.lockState()
	if !ok {
		t.Fatal("could not acquire fixture state lock")
	}
	if RunHook(deps) != 0 || len(tr.calls) != 0 || readHookState(h.statePath()).ConsumedRevision != base {
		t.Fatal("contended hook issued requests or changed marker")
	}
	locked()
	if RunHook(deps) != 0 || tr.loads != 1 || readHookState(h.statePath()).ConsumedRevision != 9 {
		t.Fatal("later event did not retry after lock release")
	}
}
