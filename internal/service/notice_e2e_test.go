package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TestHostMutationPathPerformsNoOutboundHTTP guards AC2's host-isolation
// clause. The coordinator's mutation path has no HTTP client by design —
// there is no seam to inject a transport into, because none exists — so the
// invariant is enforced structurally: the service package's production
// sources must not construct or invoke any HTTP machinery. The guard bites
// the moment someone introduces daemon I/O into a mutating command.
func TestHostMutationPathPerformsNoOutboundHTTP(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{
		`"net/http"`,
		"http.Get(", "http.Post(", "http.Head(",
		"http.Client{", "&http.Client", "http.NewRequest(",
		"http.DefaultClient", "http.DefaultTransport",
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatal(rerr)
		}
		src := string(b)
		for _, frag := range banned {
			if strings.Contains(src, frag) {
				t.Fatalf("AC2 violation: %s contains %q — the host mutation path must never perform HTTP I/O (daemon interaction belongs to internal/notice notice-hook only)", name, frag)
			}
		}
	}
}

func buildE2EDesired(noticePath, actionRun string) (desired policy.Desired, targets []RegisteredTarget, outcomes []TargetOutcome) {
	desired = fixtureDesired()
	desired.Operational.NoticePath = noticePath
	if actionRun != "" {
		desired.Operational.OnChange = []policy.OnChangeAction{{Run: actionRun, TimeoutSeconds: 5}}
	}
	rt := RegisteredTarget{Policy: desired.Global}
	outcomes = []TargetOutcome{{
		TargetID: targetID(rt),
		Prepare: &PrepareResult{
			PlanComputed: true,
			ChangedFiles: map[string]bool{"config.yaml": true},
			ChangedEdits: []reconcile.FieldEdit{
				{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("codex/gpt-5.6-luna")},
			},
		},
	}}
	return desired, []RegisteredTarget{rt}, outcomes
}

// e2ePublish runs notifyTargets over a memStore-backed coordinator with the
// minime provider standing-disabled, publishing a real rendered notice.
func e2ePublish(t *testing.T, actionRun string) (*coordinatorSpy, *memStore, string, policy.Desired) {
	t.Helper()
	work := t.TempDir()
	noticePath := filepath.Join(work, "notice", "notice.json")
	desired, targets, outcomes := buildE2EDesired(noticePath, actionRun)

	spy := newCoordinatorSpy()
	store := &memStore{s: state.State{
		Revision:  6,
		Providers: map[string]state.ProviderState{"minime": {Quota: state.QuotaExhausted}},
		Targets:   map[string]state.TargetState{},
	}}
	spy.Coordinator.State = store
	st := store.s
	if c := (&spy.Coordinator); c.notifyTargets(desired, &st, targets, outcomes) {
		t.Fatalf("publication unexpectedly recorded a failure event")
	}
	if _, err := os.Stat(noticePath); err != nil {
		t.Fatalf("notice not published: %v", err)
	}
	return spy, store, noticePath, desired
}

// TestPublishedNoticeDrivesHandlerEndToEnd proves the producer/consumer seam:
// notifyTargets renders and publishes a real notice; the in-session
// notice-hook consumes that exact file, performs exactly one authenticated
// reload against a fake daemon, advances the marker only on 200, and renders
// the actionable drift tier from the real document's field names.
func TestPublishedNoticeDrivesHandlerEndToEnd(t *testing.T) {
	spy, _, noticePath, _ := e2ePublish(t, "")

	var reloads int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reloads++
		if r.Header.Get("Authorization") != "Bearer tok-e2e" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"failed":[]}`))
	}))
	defer daemon.Close()

	sessions := t.TempDir()
	sessDir := filepath.Join(sessions, "sess-e2e")
	if err := os.MkdirAll(filepath.Join(sessDir, "polytoken-quota"), 0o700); err != nil {
		t.Fatal(err)
	}
	port := 0
	if _, err := fmt.Sscanf(daemon.URL[strings.LastIndex(daemon.URL, ":")+1:], "%d", &port); err != nil {
		t.Fatal(err)
	}
	writeE2EJSON(t, filepath.Join(sessDir, "startup.json"), map[string]any{"state": "ready", "pid": 424242, "port": port})
	writeE2EJSON(t, filepath.Join(sessDir, "credential.json"), map[string]any{"token": "tok-e2e"})

	env := map[string]string{
		"POLYTOKEN_HOOK_EVENT": "post_model_turn",
		"POLYTOKEN_SESSION_ID": "sess-e2e",
	}
	run := func() int {
		return notice.RunHook(notice.HookDeps{
			NoticePath:  noticePath,
			SessionsDir: sessions,
			Environ:     func(k string) string { return env[k] },
		})
	}
	if code := run(); code != 0 {
		t.Fatalf("hook exit = %d", code)
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want exactly 1", reloads)
	}
	markerPath := filepath.Join(sessDir, "polytoken-quota", "state.json")
	mb, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	var marker struct {
		ConsumedRevision uint64 `json:"consumed_revision"`
	}
	if err := json.Unmarshal(mb, &marker); err != nil || marker.ConsumedRevision != 6 {
		t.Fatalf("marker = %s, want consumed_revision 6", mb)
	}
	if code := run(); code != 0 || reloads != 1 {
		t.Fatalf("re-fire: code=%d reloads=%d, want no further reload", code, reloads)
	}

	// The drift tier reads the REAL rendered document: a session on the
	// standing-disabled model gets the actionable tier on its next prompt.
	env["POLYTOKEN_HOOK_EVENT"] = "pre_user_prompt"
	env["POLYTOKEN_MODEL_NAME"] = "minime/off-model"
	var out strings.Builder
	if code := notice.RunHook(notice.HookDeps{
		NoticePath:  noticePath,
		SessionsDir: sessions,
		Stdout:      &out,
		Environ:     func(k string) string { return env[k] },
	}); code != 0 {
		t.Fatalf("prompt hook exit = %d", code)
	}
	var decision struct {
		Outcome           string `json:"outcome"`
		AdditionalContext string `json:"additional_context"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &decision); err != nil {
		t.Fatalf("decision parse: %v (%s)", err, out.String())
	}
	if decision.Outcome != "accept" || !strings.Contains(decision.AdditionalContext, "disabled") {
		t.Fatalf("decision = %+v, want accept with actionable disabled tier from the real document", decision)
	}
	_ = spy
}

// TestOnChangeStdinCarriesNoSourceSecrets (AC10): a canary-shaped value is
// threaded through an input the publication pipeline genuinely consumes —
// a ChangedEdit Scalar — and asserted absent from the published notice and
// the action's stdin payload. This is a live tripwire for the schema
// contract that changed_fields carries key paths only, never edit values:
// if a future renderer starts emitting edit Scalar/Sequence values (or any
// richer source data) into the notice, this test fails. A failing action's
// recorded event must not carry the canary either.
func TestOnChangeStdinCarriesNoSourceSecrets(t *testing.T) {
	work := t.TempDir()
	const canary = "sk-ant-canary-DO-NOT-USE"

	capture := filepath.Join(work, "stdin-captured.txt")
	action := filepath.Join(work, "capture.sh")
	if err := os.WriteFile(action, []byte("#!/bin/sh\ncat > "+capture+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	noticePath := filepath.Join(work, "notice", "notice.json")
	desired, targets, outcomes := buildE2EDesired(noticePath, action)
	// Thread the canary through the consumed input: the changed edit's value
	// (a Scalar the pipeline reads but must not render — changed_fields are
	// key paths only).
	outcomes[0].Prepare.ChangedEdits = []reconcile.FieldEdit{
		{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr(canary)},
	}

	spy := newCoordinatorSpy()
	store := &memStore{s: state.State{
		Revision:  7,
		Providers: map[string]state.ProviderState{},
		Targets:   map[string]state.TargetState{},
	}}
	spy.Coordinator.State = store
	st := store.s
	c := &spy.Coordinator
	if c.notifyTargets(desired, &st, targets, outcomes) {
		t.Fatalf("publication failure")
	}
	c.runPendingOnChange(context.Background())

	for name, path := range map[string]string{
		"published notice": noticePath,
		"action stdin":     capture,
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), canary) {
			t.Fatalf("AC10 violation: canary present in %s", name)
		}
	}

	// A failing action's recorded event must not carry the canary either
	// (its stderr could echo environment material).
	failAction := filepath.Join(work, "fail.sh")
	if err := os.WriteFile(failAction, []byte("#!/bin/sh\necho "+canary+" >&2\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	noticePath2 := filepath.Join(work, "notice2", "notice.json")
	desired2, targets2, outcomes2 := buildE2EDesired(noticePath2, failAction)
	spy2 := newCoordinatorSpy()
	store2 := &memStore{s: state.State{Revision: 8, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
	spy2.Coordinator.State = store2
	st2 := store2.s
	c2 := &spy2.Coordinator
	c2.notifyTargets(desired2, &st2, targets2, outcomes2)
	c2.runPendingOnChange(context.Background())
	loaded, _ := store2.LoadState()
	for _, e := range loaded.EventHistory.Events {
		if strings.Contains(e.Reason, canary) || strings.Contains(e.Action, canary) {
			t.Fatalf("AC10 violation: canary present in event %+v", e)
		}
	}
}

func writeE2EJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
