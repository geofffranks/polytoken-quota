package service

// Provider-only reconcile and recovery tests (plan tasks 4, AC.2 and AC.7).
//
// The end-to-end suites wire the REAL coordinator path — real policy loader,
// real target registry, real staging builder, real validate runner driven by a
// scripted CommandRunner, real publisher with journal/backups, real state
// store — against complete synthetic private staging roots. The planner tests
// pin the pure decision table.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/staging"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// --- fixture -----------------------------------------------------------------

// gateCommandRunner is a validate.CommandRunner that succeeds for every staged
// candidate except those whose config-validate argv matches failContains (a
// staged-dir substring such as "quota-stage-project-b"). doctor is skipped
// automatically after a config failure, mirroring the real runner.
type gateCommandRunner struct {
	failContains string
	calls        []string
}

func (r *gateCommandRunner) Run(_ context.Context, name string, args []string, _ int64, _ map[string]string) (stdout, stderr []byte, exit int, truncated bool, err error) {
	joined := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, joined)
	if r.failContains != "" && strings.Contains(joined, r.failContains) && strings.Contains(joined, "validate") {
		return nil, []byte("synthetic config validation failure"), 1, false, nil
	}
	return nil, nil, 0, false, nil
}

// gateFixture is one synthetic installation: a global Polytoken root with
// three providers (gp, pp enrolled-able; zz never enrolled, holding the tier
// default and a failover leaf so single-provider disables stay safe), plus
// registered project roots.
type gateFixture struct {
	t                                *testing.T
	base, globalRoot, configPath     string
	statePath, lockPath, journalPath string
	backupRoot, stageTmp             string
	desired                          policy.Desired
	store                            state.Store
	runner                           *gateCommandRunner
	clock                            fixedClock
	projects                         []string
	enrolled                         []string
	// disabled tier default for the unsafe case: enrolling pp makes its
	// disable provably unsafe because pp owns the tier default leaf.
	globalConfig string
}

// globalConfigWith renders the fixture global config with per-provider enabled
// values. The sentinel value "absent" omits the enabled key entirely. Comments
// and blank lines around the managed spans are deliberate: byte preservation is
// an acceptance criterion.
func globalConfigWith(enabled map[string]string) string {
	return "# operator comment above providers\n" +
		"version: 4\n" +
		"providers:\n" +
		"  gp:\n" +
		"    kind: {type: custom_open_ai_compatible}\n" +
		"    url: http://127.0.0.1:9\n" +
		"    auth: {type: no_auth}\n" +
		"    # operator-set value\n" +
		providerEnabledLine("gp", enabled, "    ") +
		"  pp:\n" +
		"    kind: {type: custom_open_ai_compatible}\n" +
		"    url: http://127.0.0.1:9\n" +
		"    auth: {type: no_auth}\n" +
		providerEnabledLine("pp", enabled, "    ") +
		"  zz:\n" +
		"    kind: {type: custom_open_ai_compatible}\n" +
		"    url: http://127.0.0.1:9\n" +
		"    auth: {type: no_auth}\n" +
		providerEnabledLine("zz", enabled, "    ") +
		"# operator comment between sections\n" +
		"models:\n" +
		"  gp/g1: {provider: gp, enabled: true}\n" +
		"  pp/p1: {provider: pp, enabled: true}\n" +
		"  zz/z1: {provider: zz, enabled: true}\n" +
		"modelgroups:\n" +
		"  failover: [gp/g1, pp/p1, zz/z1]\n" +
		"  polytoken:default_model_full: zz/z1\n"
}

func val(id string, enabled map[string]string) string {
	if v, ok := enabled[id]; ok && v != "absent" {
		return v
	}
	return "true"
}

// providerEnabledLine renders the enabled line unless the sentinel omits it.
func providerEnabledLine(id string, enabled map[string]string, indent string) string {
	if v, ok := enabled[id]; ok && v == "absent" {
		return ""
	}
	return indent + "enabled: " + val(id, enabled) + "\n"
}

const projectConfig = "version: 4\nmodelgroups:\n  failover: [gp/g1, pp/p1, zz/z1]\n"

func newGateFixture(t *testing.T, enrolled, projects []string) *gateFixture {
	t.Helper()
	f := &gateFixture{t: t, projects: projects, enrolled: enrolled}
	f.base = t.TempDir()
	f.globalRoot = filepath.Join(f.base, "global")
	if err := os.MkdirAll(f.globalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	f.configPath = filepath.Join(f.globalRoot, "config.yaml")
	f.writeGlobalConfig(globalConfigWith(nil))
	for _, id := range projects {
		root := filepath.Join(f.base, id)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(projectConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.statePath = filepath.Join(f.base, "state.json")
	f.lockPath = filepath.Join(f.base, "lock", "apply.lock")
	f.journalPath = filepath.Join(f.base, "journal", "apply.json")
	f.backupRoot = filepath.Join(f.base, "backups")
	f.stageTmp = filepath.Join(f.base, "stage")
	f.runner = &gateCommandRunner{}
	f.clock = fixedClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	f.store = state.Store{Path: f.statePath, Now: f.clock.Now, RecoveredRetention: 24 * time.Hour}

	f.desired = policy.Desired{
		Version:     1,
		Mode:        policy.ModeProviderOnly,
		Providers:   map[policy.MappingID]policy.Mapping{},
		Global:      policy.Target{ID: "global", Root: f.globalRoot, Global: true},
		Operational: policy.Operational{NoticePath: filepath.Join(f.base, "notice", "notice.json")},
	}
	for _, id := range enrolled {
		f.desired.Providers[policy.MappingID(id)] = policy.Mapping{}
	}
	for _, id := range projects {
		f.desired.Projects = append(f.desired.Projects, policy.Target{ID: id, Root: filepath.Join(f.base, id)})
	}
	return f
}

func (f *gateFixture) writeGlobalConfig(config string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte(config), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gateFixture) readGlobalConfig() string {
	f.t.Helper()
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *gateFixture) seedState(revision uint64, providers map[string]state.ProviderState, ownership map[string]state.ProviderOwnership) {
	f.t.Helper()
	st := state.State{Schema: 1, Revision: revision, Providers: providers, Targets: map[string]state.TargetState{}, ProviderOwnership: ownership}
	if err := f.store.Save(st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gateFixture) loadState() state.State {
	f.t.Helper()
	st, err := f.store.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func (f *gateFixture) publisher() publish.Publisher {
	return publish.Publisher{
		Locker:      publish.NewFileLock(f.lockPath),
		State:       f.store,
		JournalPath: f.journalPath,
		Backups:     publish.BackupStore{Root: f.backupRoot, Limit: 3},
		Clock:       f.clock.Now,
	}
}

func (f *gateFixture) coordinator() *Coordinator {
	return f.coordinatorWith(PublisherAdapter{Publisher: f.publisher()})
}

func (f *gateFixture) coordinatorWith(pub Publisher) *Coordinator {
	return &Coordinator{
		Lock:        publish.NewFileLock(f.lockPath),
		Policy:      fixedPolicyLoader{desired: f.desired},
		State:       StoreState{Store: f.store},
		Targets:     NewTargetRegistry(),
		Stage:       StagingStager{Builder: staging.Builder{TempRoot: f.stageTmp, AuthMode: staging.AuthInert, Sources: staging.FSMaterializer{GlobalDir: f.globalRoot}}},
		Validate:    ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: f.runner}},
		Publish:     pub,
		Clock:       f.clock,
		JournalPath: f.journalPath,
	}
}

func (f *gateFixture) journalExists() bool {
	_, err := os.Stat(f.journalPath)
	return err == nil
}

// gateNoticeDoc is the decoded provider notice document under test.
// TargetsHint stays nil for a provider-only document: a legacy-shaped notice
// carrying a "targets" key would surface here instead.
type gateNoticeDoc struct {
	Schema       int                    `json:"schema"`
	Revision     uint64                 `json:"revision"`
	PublishedAt  string                 `json:"published_at"`
	ProviderOnly bool                   `json:"provider_only"`
	Providers    []notice.ProviderState `json:"providers"`
	TargetsHint  json.RawMessage        `json:"targets"`
}

// readGateNotice decodes the notice document at path.
func readGateNotice(t *testing.T, path string) gateNoticeDoc {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read notice: %v", err)
	}
	var doc gateNoticeDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode notice %s: %v", body, err)
	}
	return doc
}

func (f *gateFixture) requireNoPendingEdit(t *testing.T, st state.State) {
	t.Helper()
	if len(st.ProviderOwnership) > 0 {
		claimed := map[string]state.ProviderOwnership{}
		for id, o := range st.ProviderOwnership {
			if o.Owned {
				claimed[id] = o
			}
		}
		if len(claimed) > 0 {
			t.Fatalf("unexpected ownership claims after refusal: %+v", claimed)
		}
	}
}

// gateCountingPublisher counts ApplyUnderLock calls while delegating to the
// real adapter, so tests can pin the one-transaction publication contract.
type gateCountingPublisher struct {
	PublisherAdapter
	applies int
}

func (p *gateCountingPublisher) ApplyUnderLock(ctx context.Context, tx publish.Transaction) (state.State, error) {
	p.applies++
	return p.PublisherAdapter.ApplyUnderLock(ctx, tx)
}

// --- pure decision table -----------------------------------------------------

func TestPlanProviderGateDecisionTable(t *testing.T) {
	reserve := map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}
	disabled := map[string]state.ProviderState{"gp": {Quota: state.QuotaExhausted, Availability: state.Available}}
	normal := map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}}
	configTrue := []byte("providers:\n  gp:\n    enabled: true\n")
	configFalse := []byte("providers:\n  gp:\n    enabled: false\n")
	configAbsent := []byte("providers:\n  gp:\n    url: x\n")
	desired := policy.Desired{Mode: policy.ModeProviderOnly, Providers: map[policy.MappingID]policy.Mapping{"gp": {}}}

	cases := []struct {
		name string
		ps   map[string]state.ProviderState
		cfg  []byte
		own  map[string]state.ProviderOwnership
		// expectations
		disable, restore, conflict bool
		claim                      *state.ProviderOwnership // expected new/released claim on publish
		claimReleased              bool
	}{
		{"reserve absent key gates off and claims absent baseline", reserve, configAbsent, nil, true, false, false, &state.ProviderOwnership{BaselinePresent: false, Owned: true}, false},
		{"reserve true gates off and claims true baseline", reserve, configTrue, nil, true, false, false, &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true}, false},
		{"reserve explicit false edits nothing and claims nothing", reserve, configFalse, nil, false, false, false, nil, false},
		{"disabled true gates off", disabled, configTrue, nil, true, false, false, &state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true}, false},
		{"normal owned claim restores absent baseline by removing key", normal, configFalse, map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}}, false, true, false, nil, true},
		{"normal owned claim restores explicit false baseline by writing false", normal, configFalse, map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: false, Owned: true}}, false, true, false, nil, true},
		{"normal owned claim restores true baseline", normal, configFalse, map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}}, false, true, false, nil, true},
		{"normal owned claim with operator-restored exact baseline releases without edit", normal, configTrue, map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}}, false, false, false, nil, true},
		{"normal owned claim with changed operator value conflicts", normal, configTrue, map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}}, false, false, true, nil, false},
		{"reserve owned claim with operator re-enable conflicts", reserve, configTrue, map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}}, false, false, true, nil, false},
		{"reserve owned claim intact with stale conflict marker clears it", reserve, configFalse, map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Conflict: true}}, false, false, false, nil, false},
		{"normal no claim never touches the field", normal, configTrue, nil, false, false, false, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observed := state.State{Revision: 3, Providers: tc.ps, ProviderOwnership: tc.own}
			plan, err := planProviderGate(desired, observed, tc.cfg)
			if err != nil {
				t.Fatalf("planProviderGate: %v", err)
			}
			if tc.disable != (len(plan.Disables) == 1) {
				t.Fatalf("disables=%v want disable=%v", plan.Disables, tc.disable)
			}
			if tc.restore != (len(plan.Restores) == 1) {
				t.Fatalf("restores=%v want restore=%v", plan.Restores, tc.restore)
			}
			if tc.conflict != (len(plan.Conflicts) == 1) {
				t.Fatalf("conflicts=%v want conflict=%v", plan.Conflicts, tc.conflict)
			}
			if tc.claim != nil {
				got, ok := plan.PublishedOwnership["gp"]
				if !ok || got != *tc.claim {
					t.Fatalf("published ownership=%+v want %+v", plan.PublishedOwnership, *tc.claim)
				}
				// A new claim must never ride a refusal.
				if _, held := plan.RefusalOwnership["gp"]; held {
					t.Fatalf("refusal ownership carries a new claim: %+v", plan.RefusalOwnership)
				}
			}
			if tc.claimReleased {
				if _, held := plan.PublishedOwnership["gp"]; held {
					t.Fatalf("published ownership still holds gp: %+v", plan.PublishedOwnership)
				}
				if _, held := plan.RefusalOwnership["gp"]; !held {
					t.Fatalf("refusal ownership dropped the held claim: %+v", plan.RefusalOwnership)
				}
			}
			// Every edit addresses exactly the enrolled global provider field.
			for _, e := range plan.Edits {
				if e.File != "config.yaml" || len(e.Path) != 3 || e.Path[0] != "providers" || e.Path[1] != "gp" || e.Path[2] != "enabled" {
					t.Fatalf("edit outside the enrolled global provider field: %+v", e)
				}
			}
		})
	}
}

// --- AC.2: reserve/disabled gate off, normal restores the owned baseline ----

func TestProviderGateReserveDisabledAndNormalBaseline(t *testing.T) {
	cases := []struct {
		name string
		// start
		startConfig string
		seed        map[string]state.ProviderState
		seedOwn     map[string]state.ProviderOwnership
		// expectation after one reconcile
		wantConfig       func(before string) string
		wantOwnership    map[string]state.ProviderOwnership
		wantClaimGone    bool
		wantHistoryCount int
		// wantProviders is the exact committed provider state list the
		// published notice must carry (nil: no notice at all). It is written
		// by hand per case — never derived from providerEdits — so the notice
		// body is asserted against the actual committed effect (CQ-2).
		wantProviders []notice.ProviderState
	}{
		{
			name:        "reserve gates off an enabled provider and records the true baseline",
			startConfig: globalConfigWith(nil),
			seed:        map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}},
			wantConfig: func(before string) string {
				return strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
			},
			wantOwnership:    map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}},
			wantHistoryCount: 1,
			wantProviders:    []notice.ProviderState{{ID: "gp", Enabled: false}},
		},
		{
			name:        "disabled gates off an absent-key provider and records the absent baseline",
			startConfig: globalConfigWith(map[string]string{"gp": "absent"}),
			seed:        map[string]state.ProviderState{"gp": {Quota: state.QuotaExhausted, Availability: state.Available}},
			wantConfig: func(before string) string {
				// The key is inserted into the gp block; assert the block shape.
				return before
			},
			wantOwnership:    map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}},
			wantHistoryCount: 1,
			wantProviders:    []notice.ProviderState{{ID: "gp", Enabled: false}},
		},
		{
			name:        "normal restores an absent baseline by removing the key",
			startConfig: globalConfigWith(map[string]string{"gp": "false"}),
			seed:        map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			seedOwn:     map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}},
			wantConfig: func(before string) string {
				return strings.Replace(before, "    enabled: false\n", "", 1)
			},
			wantClaimGone:    true,
			wantHistoryCount: 1,
			// Removing the absent-baseline key re-enables the provider; the
			// notice must report the committed effect, not the edit shape.
			wantProviders: []notice.ProviderState{{ID: "gp", Enabled: true}},
		},
		{
			name:        "normal restores an explicit true baseline",
			startConfig: globalConfigWith(map[string]string{"gp": "false"}),
			seed:        map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			seedOwn:     map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}},
			wantConfig: func(before string) string {
				return strings.Replace(before, "    # operator-set value\n    enabled: false", "    # operator-set value\n    enabled: true", 1)
			},
			wantClaimGone:    true,
			wantHistoryCount: 1,
			wantProviders:    []notice.ProviderState{{ID: "gp", Enabled: true}},
		},
		{
			name:        "normal releases a claim without editing when live already equals an explicit false baseline",
			startConfig: globalConfigWith(map[string]string{"gp": "false"}),
			seed:        map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			seedOwn:     map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: false, Owned: true}},
			wantConfig: func(before string) string {
				return before
			},
			wantClaimGone:    true,
			wantHistoryCount: 0,
			wantProviders:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t, []string{"gp"}, nil)
			cfg := tc.startConfig
			f.writeGlobalConfig(cfg)
			f.seedState(7, tc.seed, tc.seedOwn)

			out := f.coordinator().Reconcile(context.Background(), false, false, false)
			if !out.Accepted || out.Error != nil {
				t.Fatalf("out=%+v err=%v", out, out.Error)
			}
			if out.Revision != 8 || out.PendingCount() != 0 {
				t.Fatalf("revision=%d pendings=%d out=%+v", out.Revision, out.PendingCount(), out)
			}
			after := f.readGlobalConfig()
			want := tc.wantConfig(cfg)
			if tc.name == "disabled gates off an absent-key provider and records the absent baseline" {
				// Insertion appends the key inside the gp block: assert the
				// inserted shape and that every unrelated byte survived.
				if !strings.Contains(after, "    auth: {type: no_auth}\n    enabled: false\n") {
					t.Fatalf("absent baseline not gated off:\n%s", after)
				}
				if !strings.Contains(after, "# operator comment above providers") ||
					!strings.Contains(after, "# operator comment between sections") ||
					!strings.Contains(after, "polytoken:default_model_full: zz/z1") {
					t.Fatalf("unrelated bytes lost:\n%s", after)
				}
			} else if after != want {
				t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", after, want)
			}
			st := f.loadState()
			if tc.wantClaimGone {
				if _, held := st.ProviderOwnership["gp"]; held {
					t.Fatalf("claim not released: %+v", st.ProviderOwnership)
				}
			} else {
				for id, wantOwn := range tc.wantOwnership {
					got, ok := st.ProviderOwnership[id]
					if !ok || got != wantOwn {
						t.Fatalf("ownership[%s]=%+v want %+v", id, got, wantOwn)
					}
				}
			}
			if got := len(st.ReconcileHistory.Records); got != tc.wantHistoryCount {
				t.Fatalf("history records=%d want %d", got, tc.wantHistoryCount)
			}
			if f.journalExists() {
				t.Fatal("journal left behind after a committed publish")
			}
			noticePath := f.desired.Operational.NoticePath
			_, noticeErr := os.Stat(noticePath)
			wantNotice := tc.wantProviders != nil
			if wantNotice != (noticeErr == nil) {
				t.Fatalf("provider notice exists=%v want=%v (err=%v)", noticeErr == nil, wantNotice, noticeErr)
			}
			if wantNotice {
				doc := readGateNotice(t, noticePath)
				if !doc.ProviderOnly || doc.Revision != 8 || len(doc.TargetsHint) != 0 {
					t.Fatalf("provider notice has incorrect shape: %+v", doc)
				}
				if !reflect.DeepEqual(doc.Providers, tc.wantProviders) {
					t.Fatalf("notice providers=%+v want %+v", doc.Providers, tc.wantProviders)
				}
			}
		})
	}
}

// --- AC.2: operator edits and stale evidence --------------------------------

func TestProviderGateOperatorEditConflict(t *testing.T) {
	f := newGateFixture(t, []string{"gp"}, nil)
	f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	// Quota holds an expected-off claim; the operator re-enables the provider.
	f.seedState(4, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}},
		map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}})
	f.writeGlobalConfig(globalConfigWith(nil)) // operator edit: back to true

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 1 {
		t.Fatalf("out=%+v want one pending conflict", out)
	}
	if got := f.readGlobalConfig(); got != globalConfigWith(nil) {
		t.Fatal("operator edit was overwritten")
	}
	st := f.loadState()
	own := st.ProviderOwnership["gp"]
	if !own.Owned || !own.Conflict || !own.BaselinePresent || !own.BaselineValue {
		t.Fatalf("ownership=%+v want held claim with conflict marker", own)
	}
	if f.journalExists() {
		t.Fatal("journal written for a refused publication")
	}
	if got := len(st.ReconcileHistory.Records); got != 0 {
		t.Fatalf("history records=%d want 0 on refusal", got)
	}

	// The operator restores the owned expectation: the conflict clears, the
	// claim holds, and no edit is made (the field already matches).
	f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	out = f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("out=%+v want clean reconcile after operator resolves", out)
	}
	st = f.loadState()
	if own := st.ProviderOwnership["gp"]; !own.Owned || own.Conflict {
		t.Fatalf("ownership=%+v want intact claim without conflict", own)
	}
}

func TestProviderGateAbsentBaselineAndStaleEvidence(t *testing.T) {
	t.Run("absent baseline is restored by removing the key", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
		f.seedState(11, map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}})

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v want a clean restore", out)
		}
		want := strings.Replace(globalConfigWith(map[string]string{"gp": "false"}), "    enabled: false\n", "", 1)
		if got := f.readGlobalConfig(); got != want {
			t.Fatalf("absent baseline not restored by key removal:\n%s", got)
		}
		if _, held := f.loadState().ProviderOwnership["gp"]; held {
			t.Fatal("claim not released after the restore")
		}
	})

	t.Run("a stale claim whose operator added an enabled key conflicts without a forced restore", func(t *testing.T) {
		// The claim from an earlier episode records an ABSENT baseline, so the
		// field should be off-by-removal while the claim is held. The operator
		// has since added `enabled: true` explicitly — a divergence the gate
		// can see: report pending, never write, never force.
		f := newGateFixture(t, []string{"gp"}, nil)
		f.writeGlobalConfig(globalConfigWith(nil))
		f.seedState(11, map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			map[string]state.ProviderOwnership{"gp": {BaselinePresent: false, Owned: true}})

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want pending conflict on stale evidence", out)
		}
		if got := f.readGlobalConfig(); got != globalConfigWith(nil) {
			t.Fatal("stale-evidence conflict rewrote the operator's added key")
		}
		own := f.loadState().ProviderOwnership["gp"]
		if !own.Owned || !own.Conflict {
			t.Fatalf("ownership=%+v want conflict recorded", own)
		}
		if f.journalExists() {
			t.Fatal("journal written for a refused publication")
		}
	})
}

// --- AC.7: combined multi-provider publication, registered roots, refusals --

func TestProviderGateCombinedMultiProviderSingleTransaction(t *testing.T) {
	f := newGateFixture(t, []string{"gp", "pp"}, []string{"project-a"})
	f.seedState(2, map[string]state.ProviderState{
		"gp": {Quota: state.QuotaExhausted, Availability: state.Available},
		"pp": {Quota: state.QuotaLow, Availability: state.Available},
	}, nil)
	counting := &gateCountingPublisher{PublisherAdapter: PublisherAdapter{Publisher: f.publisher()}}
	before := f.readGlobalConfig()

	out := f.coordinatorWith(counting).Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("out=%+v", out)
	}
	if counting.applies != 1 {
		t.Fatalf("publishes=%d want exactly one global journal transaction", counting.applies)
	}
	want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	want = strings.Replace(want, "    enabled: true\n  zz:", "    enabled: false\n  zz:", 1)
	if got := f.readGlobalConfig(); got != want {
		t.Fatalf("combined edits wrong:\n%s", got)
	}
	st := f.loadState()
	for id, wantOwn := range map[string]state.ProviderOwnership{
		"gp": {BaselinePresent: true, BaselineValue: true, Owned: true},
		"pp": {BaselinePresent: true, BaselineValue: true, Owned: true},
	} {
		if got := st.ProviderOwnership[id]; got != wantOwn {
			t.Fatalf("ownership[%s]=%+v want %+v", id, got, wantOwn)
		}
	}
	// Project roots were staged and validated with the composed global layer.
	joined := strings.Join(f.runner.calls, "\n")
	if !strings.Contains(joined, "quota-stage-project-a") {
		t.Fatalf("project root not staged/validated: %v", f.runner.calls)
	}
}

func TestProviderGateNoEditOnUnsafeCandidate(t *testing.T) {
	// Enrolling pp makes its disable provably unsafe: pp owns the tier default
	// leaf, and the observed binary rejects such reloads wholesale.
	f := newGateFixture(t, []string{"pp"}, nil)
	cfg := strings.Replace(globalConfigWith(nil), "polytoken:default_model_full: zz/z1", "polytoken:default_model_full: pp/p1", 1)
	f.writeGlobalConfig(cfg)
	f.seedState(5, map[string]state.ProviderState{"pp": {Quota: state.QuotaExhausted, Availability: state.Available}}, nil)
	before := cfg

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 1 {
		t.Fatalf("out=%+v want one pending refusal", out)
	}
	summary := out.Targets[0].Pending.Summary
	if !strings.Contains(summary, "unsafe") {
		t.Fatalf("summary=%q want an unsafe safety verdict", summary)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatal("unsafe candidate was published")
	}
	if len(f.runner.calls) != 0 {
		t.Fatalf("staging ran despite the analyzer refusal: %v", f.runner.calls)
	}
	if f.journalExists() {
		t.Fatal("journal written for a refused publication")
	}
	if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
		t.Fatalf("unsafe candidate emitted a provider notice: %v", err)
	}
	f.requireNoPendingEdit(t, f.loadState())
}

func TestProviderGateRegisteredProjectSafety(t *testing.T) {
	t.Run("project roots are evaluated with the composed global layer", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, []string{"project-a", "project-b"})
		f.seedState(1, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)
		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v", out)
		}
		joined := strings.Join(f.runner.calls, "\n")
		for _, want := range []string{"quota-stage-global", "quota-stage-project-a", "quota-stage-project-b"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("staged validation missing %s:\n%s", want, joined)
			}
		}
	})

	t.Run("a failing later project refuses the whole publication", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, []string{"project-a", "project-b"})
		f.runner.failContains = "quota-stage-project-b"
		f.seedState(3, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)
		before := f.readGlobalConfig()

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 3 {
			t.Fatalf("out=%+v want every target pending", out)
		}
		var globalPending, projectBPending bool
		for _, o := range out.Targets {
			if o.Pending == nil {
				t.Fatalf("target %s reported applied during a refused publication", o.TargetID)
			}
			switch o.TargetID {
			case "global":
				globalPending = true
				if !strings.Contains(o.Pending.Summary, "project-b") {
					t.Fatalf("global pending=%q want the publication-withheld reason naming project-b", o.Pending.Summary)
				}
			case "project-b":
				projectBPending = true
				if o.Pending.Stage != "validate" {
					t.Fatalf("project-b stage=%q want validate", o.Pending.Stage)
				}
			}
		}
		if !globalPending || !projectBPending {
			t.Fatalf("out=%+v want global and project-b pending", out)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatal("global config edited despite a failing registered project")
		}
		if f.journalExists() {
			t.Fatal("journal written for a refused publication")
		}
		st := f.loadState()
		f.requireNoPendingEdit(t, st)
		if len(st.ReconcileHistory.Records) != 0 {
			t.Fatalf("history records=%d want 0 (quota/history stay independent of the failed edit)", len(st.ReconcileHistory.Records))
		}
	})
}

// --- AC.7: byte preservation, dry-run, journal recovery ----------------------

func TestProviderGateBytePreservation(t *testing.T) {
	f := newGateFixture(t, []string{"gp"}, nil)
	before := f.readGlobalConfig()
	f.seedState(1, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("out=%+v", out)
	}
	after := f.readGlobalConfig()
	want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	if after != want {
		t.Fatalf("edit was not the exact span:\n--- got ---\n%s\n--- want ---\n%s", after, want)
	}
}

func TestProviderGateDryRunNoPublish(t *testing.T) {
	f := newGateFixture(t, []string{"gp"}, []string{"project-a"})
	before := f.readGlobalConfig()
	f.seedState(9, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

	out := f.coordinator().Reconcile(context.Background(), true, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("out=%+v want a clean evaluated dry-run", out)
	}
	if out.Revision != 9 {
		t.Fatalf("dry-run advanced the revision to %d", out.Revision)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatal("dry-run edited the live config")
	}
	if f.journalExists() {
		t.Fatal("dry-run wrote a journal")
	}
	st := f.loadState()
	if st.Revision != 9 || len(st.ReconcileHistory.Records) != 0 {
		t.Fatalf("dry-run mutated state: revision=%d history=%d", st.Revision, len(st.ReconcileHistory.Records))
	}
	if len(st.ProviderOwnership) != 0 {
		t.Fatalf("dry-run recorded ownership claims: %+v", st.ProviderOwnership)
	}
	// The evaluation itself is real: every registered root was staged.
	joined := strings.Join(f.runner.calls, "\n")
	if !strings.Contains(joined, "quota-stage-global") || !strings.Contains(joined, "quota-stage-project-a") {
		t.Fatalf("dry-run did not stage/validate the registered roots:\n%s", joined)
	}
}

func TestProviderGateAtomicPublishConflictAndRecover(t *testing.T) {
	t.Run("rename fault leaves no edit and no claim, retry converges", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		before := f.readGlobalConfig()
		f.seedState(1, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)
		faulted := f.publisher()
		faulted.Fault = func(step string) error {
			if step == "rename" {
				return publish.ErrInjected
			}
			return nil
		}
		out := f.coordinatorWith(PublisherAdapter{Publisher: faulted}).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want a pending publish failure", out)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatal("publish fault edited live bytes")
		}
		f.requireNoPendingEdit(t, f.loadState())
		if !f.journalExists() {
			t.Fatal("journal not retained after the interrupted apply")
		}

		// Retry without the fault: recovery first, then the gate converges.
		out = f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("retry out=%+v", out)
		}
		want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		if got := f.readGlobalConfig(); got != want {
			t.Fatal("retry did not converge to the gated-off bytes")
		}
		own := f.loadState().ProviderOwnership["gp"]
		if !own.Owned || !own.BaselinePresent || !own.BaselineValue {
			t.Fatalf("ownership after retry=%+v want the recorded claim", own)
		}
		if f.journalExists() {
			t.Fatal("journal left behind after recovery")
		}
	})

	t.Run("crash after renames rolls the ownership snapshot forward", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		before := f.readGlobalConfig()
		offBytes := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		// Simulated crash: the renames landed (live bytes are off) and the
		// journal carrying the intended ownership snapshot is durable, but the
		// state commit never happened.
		f.writeGlobalConfig(offBytes)
		newHash := sha256.Sum256([]byte(offBytes))
		oldHash := sha256.Sum256([]byte(before))
		journal := map[string]any{
			"schema":         1,
			"prior_revision": 7,
			"next_revision":  8,
			"target_id":      "global",
			"managed_root":   f.globalRoot,
			"replacements": []map[string]any{{
				"live_path": f.configPath,
				"temp_path": f.configPath,
				"old_hash":  fmtHash(oldHash),
				"new_hash":  fmtHash(newHash),
				"mode":      0o600,
				"applied":   true,
			}},
			"intended":      map[string]any{"attempted_revision": 8, "applied_revision": 8, "attempted_at_unix": f.clock.t.Unix(), "applied_at_unix": f.clock.t.Unix()},
			"ownership_set": true,
			"ownership":     map[string]any{"gp": map[string]any{"baseline_present": true, "baseline_value": true, "owned": true}},
		}
		data, err := json.Marshal(journal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(f.journalPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.journalPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v want recovery then a clean no-edit reconcile", out)
		}
		if got := f.readGlobalConfig(); got != offBytes {
			t.Fatal("recovery did not keep the rolled-forward bytes")
		}
		st := f.loadState()
		own := st.ProviderOwnership["gp"]
		if !own.Owned || !own.BaselinePresent || !own.BaselineValue {
			t.Fatalf("ownership=%+v want the rolled-forward claim", own)
		}
		if f.journalExists() {
			t.Fatal("journal not removed after recovery")
		}
	})
}

// --- check --reconcile wiring -------------------------------------------------

func TestProviderGateCheckReconcileQuotaHistoryIndependent(t *testing.T) {
	t.Run("check --reconcile gates off and records quota, events and history", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		f.desired.Providers[policy.MappingID("gp")] = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: 30 * time.Minute, BalanceGroup: "default", Weight: 1}}
		before := f.readGlobalConfig()
		f.seedState(2, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

		coord := f.coordinator()
		coord.QuotaPoller = &fakePoller{results: map[string]quota.QuotaSnapshot{
			"gp": {MappingID: "gp", Status: quota.SourceFresh, CheckedAt: f.clock.t},
		}}
		out := coord.QuotaCheck(context.Background(), "", true)
		if !out.Accepted || out.Error != nil || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		if got := f.readGlobalConfig(); got != want {
			t.Fatal("check --reconcile did not gate the provider off")
		}
		st := f.loadState()
		if st.Providers["gp"].QuotaSnapshot == nil || st.Providers["gp"].QuotaAttempt == nil {
			t.Fatalf("quota observations not persisted: %+v", st.Providers["gp"])
		}
		if len(st.ReconcileHistory.Records) != 1 {
			t.Fatalf("history records=%d want 1 for the proven provider change", len(st.ReconcileHistory.Records))
		}
		if own := st.ProviderOwnership["gp"]; !own.Owned || !own.BaselinePresent {
			t.Fatalf("ownership=%+v want the recorded claim", own)
		}
		// The check --reconcile path publishes the PROVIDER notice, never the
		// legacy chain document (CQ-1): a legacy shape would carry no provider
		// status and trigger false chain-drift warnings in hooked sessions.
		doc := readGateNotice(t, f.desired.Operational.NoticePath)
		if !doc.ProviderOnly || doc.Revision != 3 || doc.TargetsHint != nil {
			t.Fatalf("check --reconcile notice shape: %+v", doc)
		}
		if want := []notice.ProviderState{{ID: "gp", Enabled: false}}; !reflect.DeepEqual(doc.Providers, want) {
			t.Fatalf("check --reconcile notice providers=%+v want %+v", doc.Providers, want)
		}
		// The republication debt is recorded with the commit, so a crash
		// between the state save and the notice publication still converges
		// once publication succeeds here.
		if st.PendingProviderNotice != nil {
			t.Fatalf("notice debt not cleared after confirmed publication: %+v", st.PendingProviderNotice)
		}
	})

	t.Run("check --reconcile keeps the poll independent when the gate refuses", func(t *testing.T) {
		f := newGateFixture(t, []string{"pp"}, nil)
		f.desired.Providers[policy.MappingID("pp")] = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: 30 * time.Minute, BalanceGroup: "default", Weight: 1}}
		// Make the pp disable provably unsafe so the gate refuses.
		cfg := strings.Replace(globalConfigWith(nil), "polytoken:default_model_full: zz/z1", "polytoken:default_model_full: pp/p1", 1)
		f.writeGlobalConfig(cfg)
		f.seedState(6, map[string]state.ProviderState{"pp": {Quota: state.QuotaExhausted, Availability: state.Available}}, nil)

		coord := f.coordinator()
		coord.QuotaPoller = &fakePoller{results: map[string]quota.QuotaSnapshot{
			"pp": {MappingID: "pp", Status: quota.SourceFresh, CheckedAt: f.clock.t},
		}}
		out := coord.QuotaCheck(context.Background(), "", true)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want accepted check with a pending gate refusal", out)
		}
		if got := f.readGlobalConfig(); got != cfg {
			t.Fatal("refused gate edited the global config")
		}
		st := f.loadState()
		// The quota observation is independent of the failed edit: it persists.
		if st.Providers["pp"].QuotaSnapshot == nil {
			t.Fatalf("quota observation lost on refusal: %+v", st.Providers["pp"])
		}
		if len(st.ReconcileHistory.Records) != 0 {
			t.Fatalf("history records=%d want 0 on refusal", len(st.ReconcileHistory.Records))
		}
		// A refused gate publishes no notice of any shape.
		if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
			t.Fatalf("refused gate published a notice (stat err=%v)", err)
		}
		f.requireNoPendingEdit(t, st)
	})
}

// TestProviderGateRepublishesLostNotice proves the notice channel converges
// after a lost publication (CQ-3): a publish failure at the provider-changing
// revision records a republication debt, and the next no-edit pass republishes
// the same provider states at that revision without waiting for another
// byte-changing edit.
func TestProviderGateRepublishesLostNotice(t *testing.T) {
	t.Run("publish failure retries on the next steady pass", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		// Point the notice path under a regular FILE: Publish's directory
		// creation fails deterministically until the file is removed.
		noticeParent := filepath.Dir(f.desired.Operational.NoticePath)
		if err := os.WriteFile(noticeParent, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil || out.Revision != 8 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if _, err := os.Stat(f.desired.Operational.NoticePath); err == nil {
			t.Fatal("notice published despite the fault")
		}
		st := f.loadState()
		wantDebt := &state.PendingProviderNotice{Revision: 8, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: false}}}
		if !reflect.DeepEqual(st.PendingProviderNotice, wantDebt) {
			t.Fatalf("notice debt=%+v want %+v", st.PendingProviderNotice, wantDebt)
		}
		found := false
		for _, e := range st.EventHistory.Events {
			if e.Category == state.EventNotice && e.Result == state.EventFailed && e.Action == "notice-publish" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no notice-publish failure event: %+v", st.EventHistory.Events)
		}

		// Heal the fault and run a steady pass: the claim holds, the gate
		// makes no edits, and the lost notice is republished verbatim.
		if err := os.Remove(noticeParent); err != nil {
			t.Fatal(err)
		}
		out2 := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out2.Accepted || out2.Error != nil || out2.Revision != 9 || out2.PendingCount() != 0 {
			t.Fatalf("out2=%+v err=%v", out2, out2.Error)
		}
		doc := readGateNotice(t, f.desired.Operational.NoticePath)
		if !doc.ProviderOnly || doc.Revision != 8 || doc.TargetsHint != nil {
			t.Fatalf("republished notice shape: %+v", doc)
		}
		if want := []notice.ProviderState{{ID: "gp", Enabled: false}}; !reflect.DeepEqual(doc.Providers, want) {
			t.Fatalf("republished notice providers=%+v want %+v", doc.Providers, want)
		}
		if st := f.loadState(); st.PendingProviderNotice != nil {
			t.Fatalf("notice debt not cleared after republication: %+v", st.PendingProviderNotice)
		}
	})

	t.Run("fresh provider publication runs on_change once, retry does not", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		logPath := filepath.Join(f.base, "action.log")
		action := filepath.Join(f.base, "action.sh")
		if err := os.WriteFile(action, []byte("#!/bin/sh\nprintf 'run\\n' >> "+logPath+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		f.desired.Operational.OnChange = []policy.OnChangeAction{{Run: action, TimeoutSeconds: 5}}
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)
		coord := f.coordinator()
		out := coord.Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("first reconcile out=%+v err=%v", out, out.Error)
		}
		if got := strings.Count(readTestFile(t, logPath), "run"); got != 1 {
			t.Fatalf("on_change runs after fresh commit = %d, want 1", got)
		}
		st := f.loadState()
		st.PendingProviderNotice = &state.PendingProviderNotice{Revision: out.Revision, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: false}}}
		if err := f.store.Save(st); err != nil {
			t.Fatal(err)
		}
		if out = coord.Reconcile(context.Background(), false, false, false); !out.Accepted || out.Error != nil {
			t.Fatalf("retry reconcile out=%+v err=%v", out, out.Error)
		}
		if got := strings.Count(readTestFile(t, logPath), "run"); got != 1 {
			t.Fatalf("debt-only retry ran on_change again: %d", got)
		}
	})

	t.Run("debt recorded before a crash republishes without edits", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		// Live field already false; quota holds the intact claim. Simulate the
		// crash window: the revision-7 commit recorded its notice debt but the
		// process died before publishing.
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}},
			map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}})
		st := f.loadState()
		st.PendingProviderNotice = &state.PendingProviderNotice{Revision: 7, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: false}}}
		if err := f.store.Save(st); err != nil {
			t.Fatal(err)
		}

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil || out.Revision != 8 || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		doc := readGateNotice(t, f.desired.Operational.NoticePath)
		if !doc.ProviderOnly || doc.Revision != 7 {
			t.Fatalf("republished notice shape: %+v", doc)
		}
		if want := []notice.ProviderState{{ID: "gp", Enabled: false}}; !reflect.DeepEqual(doc.Providers, want) {
			t.Fatalf("republished notice providers=%+v want %+v", doc.Providers, want)
		}
		if st := f.loadState(); st.PendingProviderNotice != nil {
			t.Fatalf("notice debt not cleared: %+v", st.PendingProviderNotice)
		}
	})

	t.Run("conflict marker movement preserves matching notice debt", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "true"}))
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}},
			map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}})
		st := f.loadState()
		st.PendingProviderNotice = &state.PendingProviderNotice{Revision: 6, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: true}}}
		if err := f.store.Save(st); err != nil {
			t.Fatal(err)
		}
		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want conflict-only pass", out)
		}
		if got := f.loadState().PendingProviderNotice; !reflect.DeepEqual(got, st.PendingProviderNotice) {
			t.Fatalf("conflict-only pass changed matching debt: %+v", got)
		}
	})

	t.Run("fresh provider edit merges with unrelated unpublished debt", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.writeGlobalConfig(globalConfigWith(map[string]string{"pp": "false"}))
		f.seedState(7, map[string]state.ProviderState{
			"gp": {Quota: state.QuotaLow, Availability: state.Available},
			"pp": {Quota: state.QuotaNormal, Availability: state.Available},
		}, map[string]state.ProviderOwnership{"pp": {BaselinePresent: true, BaselineValue: true, Owned: true}})
		st := f.loadState()
		st.PendingProviderNotice = &state.PendingProviderNotice{Revision: 6, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: false}}}
		if err := f.store.Save(st); err != nil {
			t.Fatal(err)
		}
		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		doc := readGateNotice(t, f.desired.Operational.NoticePath)
		want := []notice.ProviderState{{ID: "gp", Enabled: false}, {ID: "pp", Enabled: true}}
		if !reflect.DeepEqual(doc.Providers, want) {
			t.Fatalf("merged notice providers=%+v want %+v", doc.Providers, want)
		}
		if doc.Revision != out.Revision {
			t.Fatalf("merged notice revision=%d want fresh revision %d", doc.Revision, out.Revision)
		}
	})

	t.Run("ownership movement invalidates debt only when committed bytes differ", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		// The operator restored the exact baseline themselves: the release
		// moves ownership, so a debt describing older states must never be
		// republished.
		f.seedState(7, map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}},
			map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}})
		st := f.loadState()
		st.PendingProviderNotice = &state.PendingProviderNotice{Revision: 5, Providers: []state.ProviderNoticeState{{ID: "gp", Enabled: false}}}
		if err := f.store.Save(st); err != nil {
			t.Fatal(err)
		}

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.Error != nil || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
			t.Fatalf("stale debt republished a notice (err=%v)", err)
		}
		if st := f.loadState(); st.PendingProviderNotice != nil {
			t.Fatalf("stale notice debt not invalidated: %+v", st.PendingProviderNotice)
		}
	})
}

// TestProviderEnabledFieldStrictDuplicateKeys proves the ownership read never
// derives bookkeeping from ambiguous bytes (ADV-3): duplicated keys under the
// providers section are a refusal, not a last-key-wins fact.
func TestProviderEnabledFieldStrictDuplicateKeys(t *testing.T) {
	cases := []struct {
		name        string
		config      string
		wantErr     bool
		wantPresent bool
		wantValue   bool
		wantKnown   bool
	}{
		{"unique true", "providers:\n  gp:\n    enabled: true\n", false, true, true, true},
		{"unique false", "providers:\n  gp:\n    enabled: false\n", false, true, false, true},
		{"absent key in present block", "providers:\n  gp:\n    url: http://x\n", false, false, false, true},
		{"missing provider block", "providers:\n  other:\n    enabled: true\n", false, false, false, false},
		{"missing providers section", "version: 4\n", false, false, false, false},
		{"duplicate provider id", "providers:\n  gp:\n    enabled: true\n  gp:\n    enabled: false\n", true, false, false, false},
		{"duplicate enabled key", "providers:\n  gp:\n    enabled: true\n    enabled: false\n", true, false, false, false},
		{"duplicate nested key under a provider block", "providers:\n  gp:\n    url: http://x\n    url: http://y\n    enabled: true\n", true, false, false, false},
		// The ownership read is scoped to the providers section: a duplicate
		// elsewhere is the write gates' concern, not bookkeeping evidence.
		{"duplicate key outside providers", "version: 4\nversion: 4\nproviders:\n  gp:\n    enabled: true\n", false, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, value, known, err := providerEnabledField([]byte(tc.config), "gp")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if present != tc.wantPresent || value != tc.wantValue || known != tc.wantKnown {
				t.Fatalf("present=%v value=%v known=%v want %v/%v/%v", present, value, known, tc.wantPresent, tc.wantValue, tc.wantKnown)
			}
		})
	}
}

// TestProviderGateRefusesDuplicateProviderConfig proves the gate end-to-end:
// a duplicated providers key refuses the whole pass, persists no ownership
// movement, and never releases an existing claim based on last-wins bytes.
func TestProviderGateRefusesDuplicateProviderConfig(t *testing.T) {
	dup := "# operator comment above providers\nversion: 4\nproviders:\n  gp:\n    kind: {type: custom_open_ai_compatible}\n    url: http://127.0.0.1:9\n    auth: {type: no_auth}\n    enabled: true\n  gp:\n    kind: {type: custom_open_ai_compatible}\n    url: http://127.0.0.1:9\n    auth: {type: no_auth}\n    enabled: false\n  zz:\n    kind: {type: custom_open_ai_compatible}\n    url: http://127.0.0.1:9\n    auth: {type: no_auth}\n    enabled: true\nmodels:\n  gp/g1: {provider: gp, enabled: true}\n  zz/z1: {provider: zz, enabled: true}\nmodelgroups:\n  polytoken:default_model_full: zz/z1\n"

	t.Run("reserve pass refuses and records no claim", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		f.writeGlobalConfig(dup)
		f.seedState(4, map[string]state.ProviderState{"gp": {Quota: state.QuotaLow, Availability: state.Available}}, nil)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want one pending refusal", out)
		}
		if got := out.Targets[0].Pending.Summary; !strings.Contains(got, "duplicate key") {
			t.Fatalf("pending summary=%q want a duplicate-key refusal", got)
		}
		if got := f.readGlobalConfig(); got != dup {
			t.Fatal("refused pass edited the global config")
		}
		st := f.loadState()
		f.requireNoPendingEdit(t, st)
		if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
			t.Fatal("refused pass published a notice")
		}
	})

	t.Run("normal mode keeps a held claim instead of releasing on ambiguous bytes", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp"}, nil)
		f.writeGlobalConfig(dup)
		own := map[string]state.ProviderOwnership{"gp": {BaselinePresent: true, BaselineValue: true, Owned: true}}
		f.seedState(4, map[string]state.ProviderState{"gp": {Quota: state.QuotaNormal, Availability: state.Available}}, own)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want one pending refusal", out)
		}
		st := f.loadState()
		got, ok := st.ProviderOwnership["gp"]
		if !ok || got != own["gp"] {
			t.Fatalf("ownership=%+v want the claim retained untouched (%+v)", st.ProviderOwnership, own["gp"])
		}
	})
}

// fmtHash renders a sha256 digest as the journal's hex form.
func fmtHash(d [32]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range d {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}
