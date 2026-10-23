package notice

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type deliveryAction uint8

const (
	deliveryFire deliveryAction = iota
	deliverySetRevision
	deliverySetActive
	deliverySetReloadStatus
)

type deliveryFakeTransport struct {
	active                  bool
	reloadStatus            int
	stateCalls, reloadCalls int
}

func (f *deliveryFakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer tok-1" {
		return nil, fmt.Errorf("request did not use synthetic session credential")
	}
	status := http.StatusOK
	body := `{"turn_in_flight":false,"main_provider_wait":null}`
	switch req.Method + " " + req.URL.Path {
	case "GET /state":
		f.stateCalls++
		if f.active {
			body = `{"turn_in_flight":true}`
		}
	case "POST /reload":
		f.reloadCalls++
		status = f.reloadStatus
		body = `{"queued":false}`
	default:
		return nil, fmt.Errorf("unexpected synthetic request %s %s", req.Method, req.URL.Path)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

type deliveryModel struct {
	revision, consumed                    uint64
	active                                bool
	reloadStatus, stateCalls, reloadCalls int
}

func (m *deliveryModel) fire() {
	if m.revision <= m.consumed {
		return
	}
	m.stateCalls++
	if m.active {
		return
	}
	m.reloadCalls++
	if m.reloadStatus == http.StatusOK {
		m.consumed = m.revision
	}
}

// Synthetic fake-transport histories; no live daemon or account is contacted.
func TestNoticeDeliveryHistoryProperty(t *testing.T) {
	config := readNoticeHistoryConfig(t, "NOTICE_HISTORY_MODE", "NOTICE_HISTORY_SEED", "NOTICE_HISTORY_CASES", 20, 80, 500)
	logNoticeHistoryRun(t, config)
	suffixLength := 32
	if config.Mode == "full" {
		suffixLength = 96
	}
	for _, seed := range noticeHistorySeeds(config) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			h := newHookHarness(t)
			h.server.Close()
			transport := &deliveryFakeTransport{reloadStatus: http.StatusOK}
			deps := HookDeps{NoticePath: h.noticePath, SessionsDir: h.sessionsDir, HTTPClient: &http.Client{Transport: transport}, Stdout: h.stdout, Environ: func(k string) string { return h.env[k] }, Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }}
			model := deliveryModel{revision: 4, reloadStatus: http.StatusOK}
			if err := os.Remove(h.statePath()); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, h.noticePath, map[string]any{"schema": SchemaVersion, "revision": model.revision})
			if code := RunHook(deps); code != 0 {
				t.Fatalf("baseline RunHook exit=%d", code)
			}
			baseline := readHookState(h.statePath())
			if baseline.BaselineRevision == nil || *baseline.BaselineRevision != 4 || transport.stateCalls != 0 || transport.reloadCalls != 0 {
				t.Fatal("first revision was not baselined without transport")
			}
			model.revision = 9
			writeJSON(t, h.noticePath, map[string]any{"schema": SchemaVersion, "revision": model.revision})
			actions := []deliveryAction{deliverySetActive, deliveryFire, deliverySetActive, deliveryFire, deliverySetRevision, deliverySetReloadStatus, deliveryFire, deliverySetReloadStatus, deliveryFire, deliveryFire, deliverySetRevision, deliveryFire}
			values := []int{1, 0, 0, 0, 10, http.StatusServiceUnavailable, 0, http.StatusOK, 0, 0, 17, 0}
			for i, a := range actions {
				v := 0
				if i < len(values) {
					v = values[i]
				}
				applyDeliveryAction(t, deps, h, transport, &model, a, v)
			}
			rng := newDeliveryRNG(uint64(seed) + 0x9e3779b97f4a7c15)
			for i := 0; i < suffixLength; i++ {
				a := deliveryAction(rng.next() % 4)
				v := 0
				switch a {
				case deliverySetRevision:
					v = int(rng.next() % 20)
				case deliverySetActive:
					v = int(rng.next() % 2)
				case deliverySetReloadStatus:
					statuses := [...]int{http.StatusOK, http.StatusConflict, http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusServiceUnavailable}
					v = statuses[rng.next()%uint64(len(statuses))]
				}
				applyDeliveryAction(t, deps, h, transport, &model, a, v)
			}
		})
	}
}
func applyDeliveryAction(t *testing.T, deps HookDeps, h *hookHarness, transport *deliveryFakeTransport, model *deliveryModel, action deliveryAction, value int) {
	t.Helper()
	switch action {
	case deliverySetRevision:
		model.revision = uint64(value)
		writeJSON(t, h.noticePath, map[string]any{"schema": SchemaVersion, "revision": model.revision})
	case deliverySetActive:
		model.active = value != 0
		transport.active = model.active
	case deliverySetReloadStatus:
		model.reloadStatus = value
		transport.reloadStatus = value
	case deliveryFire:
		model.fire()
		if code := RunHook(deps); code != 0 {
			t.Fatalf("RunHook exit=%d", code)
		}
	}
	if action != deliveryFire {
		return
	}
	state := readHookState(h.statePath())
	if state.BaselineRevision == nil || *state.BaselineRevision != 4 {
		t.Fatalf("baseline=%v", state.BaselineRevision)
	}
	if state.ConsumedRevision != model.consumed {
		t.Fatalf("consumed=%d model=%d", state.ConsumedRevision, model.consumed)
	}
	if transport.stateCalls != model.stateCalls || transport.reloadCalls != model.reloadCalls {
		t.Fatalf("requests state/reload=%d/%d model=%d/%d", transport.stateCalls, transport.reloadCalls, model.stateCalls, model.reloadCalls)
	}
}

type deliveryRNG struct{ state uint64 }

func newDeliveryRNG(seed uint64) *deliveryRNG { return &deliveryRNG{state: seed} }
func (r *deliveryRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
