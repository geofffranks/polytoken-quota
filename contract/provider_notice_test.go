package contract

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
)

func TestProviderNoticeReloadsSyntheticSession(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)
	stub := newStubProvider(t)
	work := t.TempDir()
	d := spawnFeasibilityDaemon(t, work, feasGlobalConfig(stub.URL, map[string]bool{"stub/m1": true, "stub/m2": true, "stub/m3": true}))
	d.driveTurn(t, stub, "before provider notice", "stub-reply-model=m3")

	noticePath := filepath.Join(work, "shared", "notice.json")
	doc, err := notice.RenderProvider(5, time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), []notice.ProviderState{{ID: "stub", Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(noticePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noticePath, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(work, "sessions")
	env := map[string]string{"POLYTOKEN_HOOK_EVENT": "post_model_turn", "POLYTOKEN_SESSION_ID": d.sessionID}
	if code := notice.RunHook(notice.HookDeps{NoticePath: noticePath, SessionsDir: sessions, Environ: func(k string) string { return env[k] }}); code != 0 {
		t.Fatalf("notice-hook exit=%d", code)
	}
	markerPath := filepath.Join(sessions, d.sessionID, "polytoken-quota", "state.json")
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("consumed marker missing: %v", err)
	}
	var marker struct {
		ConsumedRevision uint64 `json:"consumed_revision"`
	}
	if err := json.Unmarshal(markerBytes, &marker); err != nil || marker.ConsumedRevision != 5 {
		t.Fatalf("consumed marker=%s want revision 5", markerBytes)
	}

	// The provider-only notice makes no model or fallback assertion. The hook
	// has reloaded only this daemon at the post-turn boundary; the next local
	// synthetic turn must still complete successfully.
	if code, _ := d.do(t, http.MethodGet, "/health", ""); code != http.StatusOK {
		t.Fatalf("daemon health=%d after hook reload", code)
	}
	d.driveTurn(t, stub, "after provider notice", "stub-reply-model=m3")
	if req := stub.lastRequest(t); req.AuthHeader != "" || !strings.HasPrefix(req.Path, "/v1/") {
		t.Fatalf("unexpected local stub request: %+v", req)
	}
}
