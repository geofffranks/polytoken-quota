package contract

// Read-only version-4 provider-disable safety analyzer (internal/groupsafety)
// versus the real binary. Revision 3 of the approved plan requires the
// analyzer's effective-group resolution and safe/unsafe classification to be
// compared against pinned real-binary routing snapshots and actual
// local-stub turns for layered same-name groups — never against the
// documented schema prose.
//
// Isolation mirrors the feasibility suite: fresh temp HOME/XDG, a neutral
// working directory with no .polytoken, an in-process no-auth stub bound to
// 127.0.0.1, and no live configuration, credentials, or external requests.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/groupsafety"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// analyzeLayers runs the analyzer over the same layer bytes the binary loads.
func analyzeLayers(t *testing.T, globalYAML, projectYAML, disable string) groupsafety.Report {
	t.Helper()
	return groupsafety.Analyze(groupsafety.Input{
		Enrolled: []string{"gp", "pp"},
		Global:   groupsafety.Layer{ID: "global", Global: true, Config: []byte(globalYAML)},
		Projects: []groupsafety.Layer{{ID: "proj", Config: []byte(projectYAML)}},
	}, disable)
}

// TestAnalyzerMatchesBinaryProviderDisableRouting proves the analyzer matches
// the binary at every step of the pinned same-name layered disable: the
// all-enabled published catalog equals the analyzer's before-resolution
// (global leaves first, project leaves appended, duplicate gp/g1 positions
// preserved), the safe verdict matches the binary's accepted reload, and the
// post-disable published catalog and the actually served local-stub turn
// equal the analyzer's after-resolution (exactly the project-only leaf).
func TestAnalyzerMatchesBinaryProviderDisableRouting(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	allEnabled := feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": true, "pp": true})
	gpDisabled := feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": false, "pp": true})
	projYAML := feasLayeredProjectConfig()

	// The all-enabled analysis must be safe with the proven concatenation.
	before := analyzeLayers(t, allEnabled, projYAML, "gp")
	if before.Verdict != groupsafety.Safe {
		t.Fatalf("all-enabled analysis verdict = %q (reasons %v), want safe", before.Verdict, before.Reasons)
	}
	if want := []string{"gp/g1", "gp/g3", "gp/g1", "pp/p2"}; !equalStrings(before.GroupsBefore["failover"], want) {
		t.Fatalf("analyzer before-catalog = %v, want %v", before.GroupsBefore["failover"], want)
	}
	if want := []string{"pp/p2"}; !equalStrings(before.GroupsAfter["failover"], want) {
		t.Fatalf("analyzer after-catalog = %v, want %v", before.GroupsAfter["failover"], want)
	}

	// Binary comparison, part 1: the published catalog equals the analyzer's
	// before-resolution, duplicates and order included.
	proj := filepath.Join(work, "proj", ".polytoken")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "config.yaml"), []byte(projYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	d := spawnFeasibilityDaemon(t, work, allEnabled)
	s := d.feasState(t)
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, before.GroupsBefore["failover"]) {
		t.Fatalf("binary catalog = %v, analyzer before = %v", got, before.GroupsBefore["failover"])
	}
	if got := groupCandidates(t, s, "polytoken:general-purpose"); !equalStrings(got, before.GroupsBefore["polytoken:general-purpose"]) {
		t.Fatalf("binary full-tier reserved group = %v, analyzer = %v", got, before.GroupsBefore["polytoken:general-purpose"])
	}

	// The group pin serves the analyzer's head leaf.
	d.selectModel(t, "mg:failover")
	d.driveTurn(t, stub, "analyzer first turn", "stub-reply-model=g1")
	if req := stub.lastRequest(t); req.Model != "g1" {
		t.Fatalf("all-enabled turn served %q, want analyzer head leaf g1", req.Model)
	}

	// The post-disable analysis of the exact candidate bytes stays safe.
	after := analyzeLayers(t, gpDisabled, projYAML, "gp")
	if after.Verdict != groupsafety.Safe {
		t.Fatalf("post-disable analysis verdict = %q (reasons %v), want safe", after.Verdict, after.Reasons)
	}
	if want := []string{"pp/p2"}; !equalStrings(after.GroupsAfter["failover"], want) {
		t.Fatalf("analyzer post-disable catalog = %v, want %v", after.GroupsAfter["failover"], want)
	}

	// Binary comparison, part 2: the idle reload is accepted and the
	// published catalog converges on the analyzer's after-resolution. Note
	// the backstop nuance this pins: `config validate --user` on the global
	// layer alone rejects this candidate (it cannot see the project leaf),
	// while the daemon — which composes global+project like the analyzer —
	// accepts it. Single-layer validation is stricter than the composed
	// runtime; the staged complete-root backstop composes layers before
	// validating (internal/staging), which is why the reload result, not
	// single-layer validate output, is the binary's verdict here.
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(gpDisabled), 0o600); err != nil {
		t.Fatal(err)
	}
	d.reloadIdle(t)
	s = d.feasState(t)
	if got := groupCandidates(t, s, "failover"); !equalStrings(got, after.GroupsAfter["failover"]) {
		t.Fatalf("post-disable binary catalog = %v, analyzer after = %v", got, after.GroupsAfter["failover"])
	}
	snap := d.routingSnapshot(t)
	route, _ := snap["route"].(map[string]any)
	target, _ := route["target"].(map[string]any)
	if target["kind"] != "group" || target["group"] != "failover" {
		t.Fatalf("post-reload routing target = %v, want kind=group group=failover", target)
	}

	// Binary comparison, part 3: the next turn serves the analyzer's
	// after-resolution head leaf, and the pre-reload reply survives. The
	// provider's recorded wire name is the leaf's provider_name portion
	// (p2), so it is compared against the head leaf's name suffix.
	d.driveTurn(t, stub, "analyzer second turn", "stub-reply-model=p2")
	head := after.GroupsAfter["failover"][0]
	wire := head[strings.Index(head, "/")+1:]
	if req := stub.lastRequest(t); req.Model != wire {
		t.Fatalf("post-disable turn served %q, want analyzer head leaf %q (wire %q)", req.Model, head, wire)
	}
	_, hb := d.do(t, http.MethodGet, "/history", "")
	if !strings.Contains(string(hb), "stub-reply-model=g1") || !strings.Contains(string(hb), "stub-reply-model=p2") {
		t.Fatalf("history lost a reply; want g1 and p2, got %.1200s", hb)
	}
}

// TestAnalyzerRefusesTierBreakingDisable proves the analyzer's unsafe verdict
// matches the binary for the composition the plan pins: disabling the provider
// that owns a required tier default's only model must be classified unsafe,
// must fail the `config validate` backstop outright, and must be rejected
// wholesale by the daemon reload, which retains the stale active selection.
func TestStagedLayeredGroupValidationUsesComposedRoute(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	global := `version: 4
providers:
  stub:
    kind:
      type: custom_open_ai_compatible
    url: http://127.0.0.1:9
    auth:
      type: no_auth
    enabled: true
models:
  stub/m1:
    provider: stub
    provider_name: m1
    enabled: true
  stub/m2:
    provider: stub
    provider_name: m2
    enabled: true
modelgroups:
  failover:
    - stub/m1
    - stub/m2
  polytoken:default_model_full: stub/m1
`
	project := `version: 4
modelgroups:
  failover:
    - stub/m1
    - stub/m2
`
	if ok, out := feasValidate(t, t.TempDir(), project); ok {
		t.Fatalf("project-only candidate unexpectedly validates without global models: %s", out)
	}
	work := t.TempDir()
	globalDir := filepath.Join(work, "global")
	projectDir := filepath.Join(work, "project", ".polytoken")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	res := target.Resolved{ID: "layered", CanonicalRoot: projectDir}
	b := staging.Builder{TempRoot: t.TempDir(), AuthMode: staging.AuthInert, Sources: staging.FSMaterializer{GlobalDir: globalDir}}
	candidate, err := b.Build(context.Background(), res, reconcile.Plan{TargetID: res.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = candidate.Cleanup() })
	env := isolateEnv(t, work)
	if code := runCommand(t, bin, env, candidate, work, "config", "validate"); code != 0 {
		t.Fatalf("composed staged candidate failed validation (exit %d)", code)
	}
	data, err := os.ReadFile(filepath.Join(candidate.ConfigDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "stub/m1") || !strings.Contains(string(data), "stub/m2") {
		t.Fatalf("composed candidate omitted route leaves: %s", data)
	}
}

func TestAnalyzerRefusesTierBreakingDisable(t *testing.T) {
	bin := polytokenBin(t)
	if bin == "" {
		t.Skip("POLYTOKEN_CONTRACT_BIN / POLYTOKEN_BIN not set; opt-in suite")
	}
	requireDaemonCapabilities(t, bin)

	stub := newStubProvider(t)
	work := t.TempDir()
	allEnabled := feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": true, "pp": true})
	ppDisabled := feasLayeredGlobalConfig(stub.URL, map[string]bool{"gp": true, "pp": false})

	report := analyzeLayers(t, allEnabled, feasLayeredProjectConfig(), "pp")
	if report.Verdict != groupsafety.Unsafe {
		t.Fatalf("tier-breaking analysis verdict = %q (reasons %v), want unsafe", report.Verdict, report.Reasons)
	}
	tier := false
	for _, reason := range report.Reasons {
		if strings.Contains(reason, "polytoken:default_model_full") {
			tier = true
		}
	}
	if !tier {
		t.Fatalf("unsafe reasons do not name the emptied tier default: %v", report.Reasons)
	}
	if got := report.GroupsAfter["polytoken:default_model_full"]; len(got) != 0 {
		t.Fatalf("analyzer post-disable tier default = %v, want empty", got)
	}

	// Backstop: `config validate` rejects the candidate outright.
	if ok, out := feasValidate(t, work, ppDisabled); ok {
		t.Fatalf("tier-breaking candidate passed config validate; the backstop and analyzer disagree")
	} else if !strings.Contains(out, "unavailable") {
		t.Fatalf("expected an unavailable-leaf diagnostic, output:\n%s", out)
	}

	// Binary: the reload is rejected wholesale — the stale active selection
	// and the unchanged (all-enabled) catalog are retained, including the
	// still-available pp/p2 leaf — the observed behavior the unsafe verdict
	// predicts.
	proj := filepath.Join(work, "proj", ".polytoken")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "config.yaml"), []byte(feasLayeredProjectConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	d := spawnFeasibilityDaemon(t, work, allEnabled)
	d.selectModel(t, "mg:failover")
	if s := d.feasState(t); s["active_model"] != "gp/g1" {
		t.Fatalf("setup active = %v, want gp/g1", s["active_model"])
	}
	if err := os.WriteFile(filepath.Join(work, "isohome", ".config", "polytoken", "config.yaml"), []byte(ppDisabled), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := d.do(t, http.MethodPost, "/reload", "")
	if code != http.StatusOK {
		t.Fatalf("POST /reload = %d: %s", code, body)
	}
	if s := d.feasState(t); s["active_model"] != "gp/g1" {
		t.Fatalf("rejected reload did not retain the stale active model: %v", s["active_model"])
	}
	if got := groupCandidates(t, d.feasState(t), "failover"); !equalStrings(got, []string{"gp/g1", "gp/g3", "gp/g1", "pp/p2"}) {
		t.Fatalf("rejected reload changed the catalog: %v", got)
	}
}
