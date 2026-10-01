package service

// Manual provider enable/disable under a provider-only policy: `routing
// disable <id>` / `routing enable <id>` write the exact
// providers.<id>.enabled managed field on the registered global target through
// the same stage/validate/publish machinery the automatic provider gate uses,
// claim and release provider ownership exactly like a gate disable/restore,
// and survive the next reconcile pass without being reverted. `routing reset`
// keeps its provider-only refusal.
//
// The suites run the REAL coordinator path — real policy loader, target
// registry, staging builder, scripted validate runner, publisher with
// journal/backups, and state store — against the synthetic gate fixture.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/validate"
)

// toggleMutationRunner wraps the scripted gate runner with an optional hook
// that mutates the live global configuration during the validation window,
// simulating an operator edit between planning and publication.
type toggleMutationRunner struct {
	gateCommandRunner
	onValidate func()
}

func (r *toggleMutationRunner) Run(ctx context.Context, name string, args []string, timeout int64, env map[string]string) (stdout, stderr []byte, exit int, truncated bool, err error) {
	out, errOut, code, trunc, runErr := r.gateCommandRunner.Run(ctx, name, args, timeout, env)
	if r.onValidate != nil && code == 0 && strings.Contains(name+" "+strings.Join(args, " "), "config validate") {
		r.onValidate()
	}
	return out, errOut, code, trunc, runErr
}

// toggleClock is a mutable clock for the toggle suites.
type toggleClock struct{ t time.Time }

func (c *toggleClock) Now() time.Time { return c.t }

func TestProviderOnlyManualDisablePublishesByteEditAndClaim(t *testing.T) {
	f := newGateFixture(t, []string{"gp", "pp"}, nil)
	f.seedState(7, nil, nil)
	before := f.readGlobalConfig()
	pub := &gateCountingPublisher{PublisherAdapter: PublisherAdapter{Publisher: f.publisher()}}
	c := f.coordinatorWith(pub)

	out := c.Disable(context.Background(), "gp")
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v err=%v", out, out.Error)
	}
	if out.Revision != 8 {
		t.Fatalf("revision=%d want 8", out.Revision)
	}
	want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	if got := f.readGlobalConfig(); got != want {
		t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	st := f.loadState()
	ps := st.Providers["gp"]
	if !ps.ManualDisabled {
		t.Fatalf("manual disable not recorded in state: %+v", ps)
	}
	claim, ok := st.ProviderOwnership["gp"]
	if !ok || claim != (state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisDisabled, EngagedRevision: 8}) {
		t.Fatalf("claim = %+v, want a disabled-axis claim at revision 8", claim)
	}
	if _, held := st.ProviderOwnership["pp"]; held {
		t.Fatalf("untouched provider claimed: %+v", st.ProviderOwnership)
	}
	// The manual transition is recorded on the durable timeline.
	var manual *state.EventRecord
	for i := range st.EventHistory.Events {
		if st.EventHistory.Events[i].Action == string(state.TriggerRoutingDisable) {
			manual = &st.EventHistory.Events[i]
		}
	}
	if manual == nil || manual.MappingID != "gp" || manual.Category != state.EventManual {
		t.Fatalf("routing_disable event missing or wrong: %+v", st.EventHistory.Events)
	}
	// The edit went through the real staging + validation path.
	var validated bool
	for _, call := range f.runner.calls {
		if strings.Contains(call, "config validate") && strings.Contains(call, "quota-stage-global") {
			validated = true
		}
	}
	if !validated {
		t.Fatalf("staged validation never ran: %v", f.runner.calls)
	}
	if pub.applies != 1 {
		t.Fatalf("ApplyUnderLock calls=%d want exactly one journaled transaction", pub.applies)
	}

	// A second disable is an idempotent handled-without-revision no-op.
	again := f.coordinator().Disable(context.Background(), "gp")
	if !again.Accepted || again.Error != nil || !again.HandledWithoutRevision {
		t.Fatalf("idempotent disable out=%+v err=%v", again, again.Error)
	}
	if got := f.readGlobalConfig(); got != want {
		t.Fatal("idempotent disable changed bytes")
	}
	if st2 := f.loadState(); st2.Revision != 8 {
		t.Fatalf("idempotent disable bumped revision to %d", st2.Revision)
	}
}

func TestProviderOnlyManualEnableRestoresAndReenables(t *testing.T) {
	f := newGateFixture(t, []string{"gp", "pp"}, nil)
	// Seed the committed result of a manual disable: flag set, disabled-axis
	// claim holding an originally-present true baseline, bytes disabled.
	f.seedState(8, map[string]state.ProviderState{
		"gp": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, map[string]state.ProviderOwnership{
		"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisDisabled, EngagedRevision: 8},
	})
	before := f.readGlobalConfig()
	if !strings.Contains(before, "enabled: true") {
		t.Fatal("fixture must start enabled")
	}

	out := f.coordinator().Enable(context.Background(), "gp")
	if !out.Accepted || out.Error != nil {
		t.Fatalf("out=%+v err=%v", out, out.Error)
	}
	if out.Revision != 9 {
		t.Fatalf("revision=%d want 9", out.Revision)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatalf("enable did not restore the baseline bytes:\n--- got ---\n%s\n--- want ---\n%s", got, before)
	}
	st := f.loadState()
	if st.Providers["gp"].ManualDisabled {
		t.Fatalf("manual disable not cleared: %+v", st.Providers["gp"])
	}
	if _, held := st.ProviderOwnership["gp"]; held {
		t.Fatalf("claim not released: %+v", st.ProviderOwnership)
	}

	// An absent-key baseline is restored by REMOVING the key, matching the
	// gate's restore shape.
	f2 := newGateFixture(t, []string{"gp", "pp"}, nil)
	f2.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	f2.seedState(8, map[string]state.ProviderState{
		"gp": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
	}, map[string]state.ProviderOwnership{
		"gp": {BaselinePresent: false, Owned: true, Axis: state.OwnershipAxisDisabled, EngagedRevision: 8},
	})
	out2 := f2.coordinator().Enable(context.Background(), "gp")
	if !out2.Accepted || out2.Error != nil {
		t.Fatalf("absent-baseline enable out=%+v err=%v", out2, out2.Error)
	}
	want := globalConfigWith(map[string]string{"gp": "absent"})
	if got := f2.readGlobalConfig(); got != want {
		t.Fatalf("absent-baseline enable wrote the wrong bytes:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if st2 := f2.loadState(); len(st2.ProviderOwnership) != 0 {
		t.Fatalf("claims not released: %+v", st2.ProviderOwnership)
	}

	// Enabling an operator-held-off provider with no quota claim writes
	// enabled: true explicitly and claims nothing.
	f3 := newGateFixture(t, []string{"gp", "pp"}, nil)
	f3.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	f3.seedState(8, nil, nil)
	out3 := f3.coordinator().Enable(context.Background(), "gp")
	if !out3.Accepted || out3.Error != nil {
		t.Fatalf("operator-held-off enable out=%+v err=%v", out3, out3.Error)
	}
	want3 := globalConfigWith(nil)
	if got := f3.readGlobalConfig(); got != want3 {
		t.Fatalf("enable wrote the wrong bytes:\n--- got ---\n%s\n--- want ---\n%s", got, want3)
	}
	if st3 := f3.loadState(); len(st3.ProviderOwnership) != 0 {
		t.Fatalf("enable invented a claim: %+v", st3.ProviderOwnership)
	}

	// A signal-axis claim held by the automatic gate is released by a manual
	// enable: the recorded baseline is restored, the claim is deleted, and
	// neither the ownership map nor the notice debt is corrupted.
	f4 := newGateFixture(t, []string{"gp", "pp"}, nil)
	f4.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	f4.seedState(6, map[string]state.ProviderState{
		"gp": {Quota: state.QuotaNormal, Availability: state.Available},
	}, map[string]state.ProviderOwnership{
		"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.5, EngagedRevision: 6},
	})
	out4 := f4.coordinator().Enable(context.Background(), "gp")
	if !out4.Accepted || out4.Error != nil {
		t.Fatalf("signal-claim enable out=%+v err=%v", out4, out4.Error)
	}
	want4 := globalConfigWith(nil)
	if got := f4.readGlobalConfig(); got != want4 {
		t.Fatalf("signal-claim enable wrote the wrong bytes:\n--- got ---\n%s\n--- want ---\n%s", got, want4)
	}
	st4 := f4.loadState()
	if _, held := st4.ProviderOwnership["gp"]; held {
		t.Fatalf("signal claim not released: %+v", st4.ProviderOwnership)
	}
	if st4.PendingProviderNotice != nil {
		t.Fatalf("signal-claim enable left notice debt: %+v", st4.PendingProviderNotice)
	}
	doc4 := readGateNotice(t, f4.desired.Operational.NoticePath)
	if len(doc4.Providers) != 1 || doc4.Providers[0].ID != "gp" || !doc4.Providers[0].Enabled {
		t.Fatalf("signal-claim enable notice = %+v want gp enabled=true", doc4.Providers)
	}
}

// TestProviderOnlyManualToggleOverOperatorHeldOffField pins the notice truth
// for the gate's own restore semantics: when quota's disable claimed an
// operator-held-off (enabled: false) baseline, a later manual enable restores
// that baseline — the provider stays disabled — and both the disable and the
// enable must record enabled=false in the published notice, never the toggle
// direction.
func TestProviderOnlyManualToggleOverOperatorHeldOffField(t *testing.T) {
	f := newGateFixture(t, []string{"gp", "pp"}, nil)
	f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false"}))
	f.seedState(7, nil, nil)

	out := f.coordinator().Disable(context.Background(), "gp")
	if !out.Accepted || out.Error != nil {
		t.Fatalf("disable out=%+v err=%v", out, out.Error)
	}
	if got := f.readGlobalConfig(); got != globalConfigWith(map[string]string{"gp": "false"}) {
		t.Fatal("disable changed the operator-held-off field")
	}
	doc := readGateNotice(t, f.desired.Operational.NoticePath)
	if len(doc.Providers) != 1 || doc.Providers[0].ID != "gp" || doc.Providers[0].Enabled {
		t.Fatalf("disable notice = %+v want gp enabled=false", doc.Providers)
	}
	if st := f.loadState(); st.PendingProviderNotice != nil {
		t.Fatalf("disable notice debt not cleared after publish: %+v", st.PendingProviderNotice)
	}

	ena := f.coordinator().Enable(context.Background(), "gp")
	if !ena.Accepted || ena.Error != nil {
		t.Fatalf("enable out=%+v err=%v", ena, ena.Error)
	}
	if got := f.readGlobalConfig(); got != globalConfigWith(map[string]string{"gp": "false"}) {
		t.Fatal("enable did not restore the held-off baseline")
	}
	doc2 := readGateNotice(t, f.desired.Operational.NoticePath)
	if len(doc2.Providers) != 1 || doc2.Providers[0].ID != "gp" || doc2.Providers[0].Enabled {
		t.Fatalf("enable notice = %+v want gp enabled=false (the enable restored a held-off baseline)", doc2.Providers)
	}
	st2 := f.loadState()
	if st2.PendingProviderNotice != nil {
		t.Fatalf("enable notice debt not cleared after publish: %+v", st2.PendingProviderNotice)
	}
	if _, held := st2.ProviderOwnership["gp"]; held {
		t.Fatalf("claim not released: %+v", st2.ProviderOwnership)
	}
}

func TestProviderOnlyManualToggleRefusals(t *testing.T) {
	t.Run("unknown provider refused without staging", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.seedState(7, nil, nil)
		before := f.readGlobalConfig()
		out := f.coordinator().Disable(context.Background(), "zz")
		if out.Accepted || out.Error == nil {
			t.Fatalf("out=%+v want refusal", out)
		}
		if !strings.Contains(out.Error.Error(), "not configured") {
			t.Fatalf("error=%v want a not-configured refusal", out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatal("refused toggle mutated bytes")
		}
		if st := f.loadState(); st.Revision != 7 || len(st.ProviderOwnership) != 0 {
			t.Fatalf("refused toggle mutated state: rev=%d ownership=%+v", st.Revision, st.ProviderOwnership)
		}
		if len(f.runner.calls) != 0 {
			t.Fatalf("refusal staged or validated: %v", f.runner.calls)
		}
	})

	t.Run("unregistered target refused", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.seedState(7, nil, nil)
		if err := os.RemoveAll(f.globalRoot); err != nil {
			t.Fatal(err)
		}
		out := f.coordinator().Disable(context.Background(), "gp")
		if out.Accepted || out.Error == nil {
			t.Fatalf("out=%+v want refusal", out)
		}
		if st := f.loadState(); st.Revision != 7 {
			t.Fatalf("refused toggle bumped revision to %d", st.Revision)
		}
	})

	t.Run("staged validation failure refused", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.seedState(7, nil, nil)
		f.runner.failContains = "quota-stage-global"
		before := f.readGlobalConfig()
		out := f.coordinator().Disable(context.Background(), "gp")
		if out.Accepted || out.Error == nil {
			t.Fatalf("out=%+v want refusal", out)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatal("refused toggle mutated bytes")
		}
		if st := f.loadState(); st.Revision != 7 || len(st.ProviderOwnership) != 0 || st.Providers["gp"].ManualDisabled {
			t.Fatalf("refused toggle mutated state: %+v", st)
		}
	})

	t.Run("live edit during validation window refused as stale", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.seedState(7, nil, nil)
		mutating := &toggleMutationRunner{gateCommandRunner: gateCommandRunner{}}
		mutating.onValidate = func() {
			// An operator edit that differs from anything quota would publish:
			// both providers flipped off during the validation window.
			cfg := globalConfigWith(map[string]string{"gp": "false", "pp": "false"})
			if err := os.WriteFile(f.configPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c := f.coordinator()
		c.Validate = ValidateRunner{Runner: validate.Runner{Binary: "polytoken", Commands: mutating}}
		out := c.Disable(context.Background(), "gp")
		if out.Accepted || out.Error == nil {
			t.Fatalf("out=%+v want refusal", out)
		}
		if !strings.Contains(out.Error.Error(), "changed") {
			t.Fatalf("error=%v want a stale-input refusal", out.Error)
		}
		if want := globalConfigWith(map[string]string{"gp": "false", "pp": "false"}); f.readGlobalConfig() != want {
			t.Fatalf("stale refusal published or altered bytes:\n%s", f.readGlobalConfig())
		}
		if st := f.loadState(); st.Revision != 7 || len(st.ProviderOwnership) != 0 {
			t.Fatalf("stale refusal mutated state: rev=%d ownership=%+v", st.Revision, st.ProviderOwnership)
		}
	})

	t.Run("reset still refused under provider-only", func(t *testing.T) {
		f := newGateFixture(t, []string{"gp", "pp"}, nil)
		f.seedState(7, nil, nil)
		before := f.readGlobalConfig()
		out := f.coordinator().Reset(context.Background())
		if out.Accepted || out.Error == nil {
			t.Fatalf("out=%+v want refusal", out)
		}
		if !strings.Contains(out.Error.Error(), "provider-only policies do not support legacy chain management") {
			t.Fatalf("error=%v want the provider-only unsupported refusal", out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatal("refused reset mutated bytes")
		}
		if st := f.loadState(); st.Revision != 7 {
			t.Fatalf("refused reset bumped revision to %d", st.Revision)
		}
		if len(f.runner.calls) != 0 {
			t.Fatalf("refused reset staged: %v", f.runner.calls)
		}
	})
}

func TestProviderOnlyManualDisableSurvivesReconcile(t *testing.T) {
	f := newGateFixture(t, []string{"gp", "pp"}, nil)
	f.seedState(7, nil, nil)

	out := f.coordinator().Disable(context.Background(), "gp")
	if !out.Accepted || out.Error != nil {
		t.Fatalf("disable out=%+v err=%v", out, out.Error)
	}
	disabledBytes := f.readGlobalConfig()
	if !strings.Contains(disabledBytes, "enabled: false") {
		t.Fatal("disable did not commit")
	}

	rec := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !rec.Accepted || rec.Error != nil {
		t.Fatalf("reconcile out=%+v err=%v", rec, rec.Error)
	}
	if got := f.readGlobalConfig(); got != disabledBytes {
		t.Fatalf("reconcile reverted the manual disable:\n--- got ---\n%s\n--- want ---\n%s", got, disabledBytes)
	}
	// The gate treats the manual toggle as its own held claim: the summary
	// row reports held, not a restore or a conflict.
	row := gateRow(t, rec.Targets[0], "gp")
	if row.Action != GateActionHeld {
		t.Fatalf("gate row after reconcile = %+v, want held", row)
	}
	st := f.loadState()
	claim, ok := st.ProviderOwnership["gp"]
	if !ok || !claim.Owned || claim.Axis != state.OwnershipAxisDisabled {
		t.Fatalf("claim after reconcile = %+v, want the disabled-axis claim intact", claim)
	}

	// And a manual enable survives the next reconcile too.
	ena := f.coordinator().Enable(context.Background(), "gp")
	if !ena.Accepted || ena.Error != nil {
		t.Fatalf("enable out=%+v err=%v", ena, ena.Error)
	}
	enabledBytes := f.readGlobalConfig()
	rec2 := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !rec2.Accepted || rec2.Error != nil {
		t.Fatalf("reconcile out=%+v err=%v", rec2, rec2.Error)
	}
	if got := f.readGlobalConfig(); got != enabledBytes {
		t.Fatalf("reconcile reverted the manual enable:\n--- got ---\n%s\n--- want ---\n%s", got, enabledBytes)
	}
	if st2 := f.loadState(); len(st2.ProviderOwnership) != 0 {
		t.Fatalf("reconcile reinvented claims: %+v", st2.ProviderOwnership)
	}
}

func TestProviderToggleInputCurrentRejectsChangedField(t *testing.T) {
	config := []byte(globalConfigWith(nil))
	if err := providerToggleInputCurrent(config, "gp", true, true); err != nil {
		t.Fatalf("unchanged input refused: %v", err)
	}
	changed := []byte(globalConfigWith(map[string]string{"gp": "false"}))
	if err := providerToggleInputCurrent(changed, "gp", true, true); err == nil {
		t.Fatal("changed input accepted")
	}
	removed := []byte(globalConfigWith(map[string]string{"gp": "absent"}))
	if err := providerToggleInputCurrent(removed, "gp", true, true); err == nil {
		t.Fatal("removed-key input accepted")
	}
	if err := providerToggleInputCurrent(removed, "gp", false, false); err != nil {
		t.Fatalf("absent-key match refused: %v", err)
	}
}
