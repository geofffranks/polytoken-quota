package cli

// CLI-level provider-only routing toggle tests: `routing disable/enable <id>`
// and `routing reset` through Run against a REAL service.Coordinator — real
// lock, state store, target registry, staging builder, scripted validate
// runner, and publisher with journal — over a synthetic private global root.
// No live configuration, daemon, or credentials are involved.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/service"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// toggleCommandRunner records every validate invocation and succeeds.
type toggleCommandRunner struct{ calls []string }

func (r *toggleCommandRunner) Run(_ context.Context, name string, args []string, _ int64, _ map[string]string) (stdout, stderr []byte, exit int, truncated bool, err error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil, 0, false, nil
}

type cliToggleClock struct{}

func (cliToggleClock) Now() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }

type cliTogglePolicy struct{ d policy.Desired }

func (l cliTogglePolicy) LoadPolicy() (policy.Desired, error) { return l.d, nil }
func (l cliTogglePolicy) DesiredExists() bool                 { return true }

const cliToggleConfig = "# operator comment above providers\n" +
	"version: 4\n" +
	"providers:\n" +
	"  gp:\n" +
	"    kind: {type: custom_open_ai_compatible}\n" +
	"    url: http://127.0.0.1:9\n" +
	"    auth: {type: no_auth}\n" +
	"    enabled: true\n" +
	"  pp:\n" +
	"    kind: {type: custom_open_ai_compatible}\n" +
	"    url: http://127.0.0.1:9\n" +
	"    auth: {type: no_auth}\n" +
	"    enabled: true\n" +
	"models:\n" +
	"  gp/g1: {provider: gp, enabled: true}\n" +
	"  pp/p1: {provider: pp, enabled: true}\n" +
	"modelgroups:\n" +
	"  polytoken:default_model_full: gp/g1\n"

// newCliToggleFixture builds one synthetic provider-only installation and the
// real Dependencies carrying a fully wired service.Coordinator.
func newCliToggleFixture(t *testing.T) (Dependencies, string, *toggleCommandRunner) {
	t.Helper()
	base := t.TempDir()
	globalRoot := filepath.Join(base, "global")
	if err := os.MkdirAll(globalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(globalRoot, "config.yaml")
	if err := os.WriteFile(configPath, []byte(cliToggleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	desired := policy.Desired{
		Version:     1,
		Mode:        policy.ModeProviderOnly,
		Providers:   map[policy.MappingID]policy.Mapping{"gp": {}, "pp": {}},
		Global:      policy.Target{ID: "global", Root: globalRoot, Global: true},
		Operational: policy.Operational{NoticePath: filepath.Join(base, "notice", "notice.json")},
	}
	runner := &toggleCommandRunner{}
	coord := &service.Coordinator{
		Lock:    publish.NewFileLock(filepath.Join(base, "lock", "apply.lock")),
		Policy:  cliTogglePolicy{d: desired},
		State:   service.StoreState{Store: state.Store{Path: filepath.Join(base, "state.json"), Now: cliToggleClock{}.Now, RecoveredRetention: 24 * time.Hour}},
		Targets: service.NewTargetRegistry(),
		Stage: service.StagingStager{Builder: staging.Builder{
			TempRoot: filepath.Join(base, "stage"), AuthMode: staging.AuthInert,
			Sources: staging.FSMaterializer{GlobalDir: globalRoot},
		}},
		Validate:    service.ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: runner}},
		Publish:     service.PublisherAdapter{Publisher: publish.Publisher{Locker: publish.NewFileLock(filepath.Join(base, "lock", "apply.lock")), State: state.Store{Path: filepath.Join(base, "state.json"), Now: cliToggleClock{}.Now, RecoveredRetention: 24 * time.Hour}, JournalPath: filepath.Join(base, "journal", "apply.json"), Backups: publish.BackupStore{Root: filepath.Join(base, "backups"), Limit: 3}, Clock: cliToggleClock{}.Now}},
		Clock:       cliToggleClock{},
		JournalPath: filepath.Join(base, "journal", "apply.json"),
	}
	return Dependencies{Mutator: coord}, configPath, runner
}

func TestRoutingDisableProviderOnlyEndToEnd(t *testing.T) {
	deps, configPath, runner := newCliToggleFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"routing", "disable", "gp"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != ExitOK {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "routing disabled") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(cliToggleConfig, "    auth: {type: no_auth}\n    enabled: true\n  pp:", "    auth: {type: no_auth}\n    enabled: false\n  pp:", 1)
	if string(b) != want {
		t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", b, want)
	}
	// Staging and validation actually ran against the real polytoken command
	// surface (scripted here), driven by the routing command.
	var validated bool
	for _, call := range runner.calls {
		if strings.Contains(call, "config validate") && strings.Contains(call, "quota-stage-global") {
			validated = true
		}
	}
	if !validated {
		t.Fatalf("staged validation never ran: %v", runner.calls)
	}
}

func TestRoutingEnableProviderOnlyEndToEnd(t *testing.T) {
	deps, configPath, _ := newCliToggleFixture(t)
	// Seed the committed result of a manual disable, then re-enable it.
	disableOut := deps.Mutator.Disable(context.Background(), "gp")
	if !disableOut.Accepted || disableOut.Error != nil {
		t.Fatalf("disable out=%+v err=%v", disableOut, disableOut.Error)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"routing", "enable", "gp"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != ExitOK {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "routing enabled") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != cliToggleConfig {
		t.Fatalf("enable did not restore the baseline bytes:\n%s", b)
	}
}

func TestRoutingResetProviderOnlyStillRefused(t *testing.T) {
	deps, configPath, runner := newCliToggleFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"routing", "reset"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != ExitRejected {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "provider-only policies do not support legacy chain management") {
		t.Fatalf("stderr=%q want the provider-only refusal", stderr.String())
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != cliToggleConfig {
		t.Fatal("refused reset mutated bytes")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refused reset staged: %v", runner.calls)
	}
}
