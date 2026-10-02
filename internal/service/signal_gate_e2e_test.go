package service

// Signal-gate end-to-end tests (approved amendment: gating keys off the
// use-it-or-lose-it routing signal; AC.1/2/3/4/6): the REAL coordinator path —
// real policy loader, target registry, staging, validate runner driven by the
// scripted CommandRunner, publisher with journal/backups, and state store —
// against the synthetic gate fixture, with enrolled providers carrying quota
// configuration and seeded quota snapshots.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// gateClock is a mutable clock so tests can age snapshots past the freshness
// TTL between passes (the shared fixture clock is fixed).
type gateClock struct{ t time.Time }

func (c *gateClock) Now() time.Time { return c.t }

// newSignalFixture is a gate fixture whose enrolled providers carry quota
// configuration so signal verdicts can engage.
func newSignalFixture(t *testing.T, enrolled []string) *gateFixture {
	t.Helper()
	f := newGateFixture(t, enrolled, nil)
	for _, id := range enrolled {
		f.desired.Providers[policy.MappingID(id)] = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex"}}
	}
	return f
}

// signalSeedSnapshot builds a fresh snapshot whose two-day window is exactly
// half elapsed at now, so the use-it-or-lose-it signal is exactly
// 2*remaining - 2*used: used 0.68 -> -0.72, used 0.10 -> +1.60.
func signalSeedSnapshot(now time.Time, usedFrac float64) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		CheckedAt:    now,
		Availability: quota.QuotaAvailable,
		Status:       quota.SourceFresh,
		Windows: []quota.QuotaWindow{{
			Used: fptr64(usedFrac * 100), Limit: fptr64(100),
			ResetAt: tptr64(now.Add(24 * time.Hour)), Period: durptr64(48 * time.Hour),
		}},
	}
}

// updateSnapshots overwrites the named providers' quota snapshots in the
// committed state, simulating a fresh poll between passes.
func (f *gateFixture) updateSnapshots(snapshots map[string]*quota.QuotaSnapshot) {
	f.t.Helper()
	st := f.loadState()
	for id, snap := range snapshots {
		ps := st.Providers[id]
		ps.QuotaSnapshot = snap
		st.Providers[id] = ps
	}
	if err := f.store.Save(st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gateFixture) gateCoordinator(cl *gateClock) *Coordinator {
	f.t.Helper()
	c := f.coordinator()
	c.Clock = cl
	return c
}

// gateRow finds one provider's gate summary row in an outcome.
func gateRow(t *testing.T, outcome TargetOutcome, provider string) ProviderGateSummary {
	t.Helper()
	for _, g := range outcome.ProviderGates {
		if g.Provider == provider {
			return g
		}
	}
	t.Fatalf("provider %s has no gate summary row: %+v", provider, outcome.ProviderGates)
	return ProviderGateSummary{}
}

// TestSignalGateEngagesAndClaims proves AC.1 end to end: a fresh
// projected-exhaustion snapshot gates the provider with an exact-span,
// byte-preserving edit, a signal-attributed ownership claim, a durable
// signal_gated event, outcome gate summaries naming the axis/signal/threshold,
// and a notice carrying the signal reason.
func TestSignalGateEngagesAndClaims(t *testing.T) {
	f := newSignalFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)}, // signal -0.72
		"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)}, // signal +1.60
	}, nil)
	before := f.readGlobalConfig()

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 || out.Revision != 8 {
		t.Fatalf("out=%+v err=%v", out, out.Error)
	}
	want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	if got := f.readGlobalConfig(); got != want {
		t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	st := f.loadState()
	claim, ok := st.ProviderOwnership["gp"]
	if !ok || claim != (state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 8}) {
		t.Fatalf("claim = %+v, want a signal-attributed claim at revision 8", claim)
	}
	if _, held := st.ProviderOwnership["pp"]; held {
		t.Fatalf("pp claimed without a gating signal: %+v", st.ProviderOwnership)
	}
	// The durable timeline records the signal_gated transition with its reason.
	var gated *state.EventRecord
	for i := range st.EventHistory.Events {
		if st.EventHistory.Events[i].Action == "signal_gated" {
			gated = &st.EventHistory.Events[i]
		}
	}
	if gated == nil || gated.MappingID != "gp" || gated.Reason != "signal -0.72 <= threshold +0.00" {
		t.Fatalf("signal_gated event missing or wrong: %+v", st.EventHistory.Events)
	}
	// The outcome channel names the axis, observed signal, and threshold.
	row := gateRow(t, out.Targets[0], "gp")
	if row.Action != GateActionDisabled || row.Axis != state.OwnershipAxisSignal || row.Signal == nil || routing.SignalFormat(*row.Signal) != "-0.72" || row.Threshold == nil || routing.SignalFormat(*row.Threshold) != "+0.00" {
		t.Fatalf("gp gate row = %+v", row)
	}
	// The notice explains the committed state with the signal reason; only
	// providers with fresh committed edits appear.
	doc := readGateNotice(t, f.desired.Operational.NoticePath)
	wantProviders := []notice.ProviderState{
		{ID: "gp", Enabled: false, Reason: "signal-gated (-0.72 <= +0.00)"},
	}
	if len(doc.Providers) != len(wantProviders) {
		t.Fatalf("notice providers = %+v", doc.Providers)
	}
	for i, wantP := range wantProviders {
		if doc.Providers[i] != wantP {
			t.Fatalf("notice provider %d = %+v, want %+v", i, doc.Providers[i], wantP)
		}
	}
}

// TestSignalPoolAllHotSkipReleasesSignalClaims proves AC.2 across passes: with
// gp signal-held and pp overdrawn on the next pass, the pool is fully
// overdrawn, so no signal edits are written, gp's baseline is restored, and a
// signal_recovered event records the pool escape hatch.
func TestSignalPoolAllHotSkipReleasesSignalClaims(t *testing.T) {
	f := newSignalFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},
		"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
	}, nil)
	before := f.readGlobalConfig()

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("pass1 out=%+v err=%v", out, out.Error)
	}
	gated := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	if got := f.readGlobalConfig(); got != gated {
		t.Fatalf("pass1 byte mismatch:\n%s", got)
	}

	// Pass 2: pp is overdrawn too — the pool is fully overdrawn.
	f.updateSnapshots(map[string]*quota.QuotaSnapshot{"pp": signalSeedSnapshot(now, 0.68)})
	out = f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 || out.Revision != 9 {
		t.Fatalf("pass2 out=%+v err=%v", out, out.Error)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatalf("pass2 must restore gp's baseline and never gate pp:\n--- got ---\n%s\n--- want ---\n%s", got, before)
	}
	st := f.loadState()
	if len(st.ProviderOwnership) != 0 {
		t.Fatalf("claims = %+v, want every signal claim released", st.ProviderOwnership)
	}
	var recovered *state.EventRecord
	for i := range st.EventHistory.Events {
		if st.EventHistory.Events[i].Action == "signal_recovered" {
			recovered = &st.EventHistory.Events[i]
		}
	}
	if recovered == nil || recovered.MappingID != "gp" || !strings.Contains(recovered.Reason, "pool fully overdrawn") {
		t.Fatalf("signal_recovered event missing or wrong: %+v", st.EventHistory.Events)
	}
	row := gateRow(t, out.Targets[0], "gp")
	if row.Action != GateActionRestored {
		t.Fatalf("gp gate row = %+v, want restored", row)
	}
	ppRow := gateRow(t, out.Targets[0], "pp")
	if ppRow.Action != GateActionPoolSkip {
		t.Fatalf("pp gate row = %+v, want pool-skip (the pool rule must not gate pp either)", ppRow)
	}
}

// exhaustedSignalSnapshot builds a signal-seeded snapshot that ALSO crosses
// the fail-closed quota boundary (provider-reported unavailable): signal-hot
// or not, the quota axis observes out-of-quota.
func exhaustedSignalSnapshot(now time.Time, usedFrac float64) *quota.QuotaSnapshot {
	snap := signalSeedSnapshot(now, usedFrac)
	snap.Availability = quota.QuotaUnavailable
	return snap
}

// signalClaimFixture seeds a fixture whose first enrolled provider is gated
// off under a held signal claim (the committed result of a previous pass),
// then refreshes every provider's snapshot to the given ones.
func signalClaimFixture(t *testing.T, enrolled []string, snapshots map[string]*quota.QuotaSnapshot) *gateFixture {
	t.Helper()
	f := newSignalFixture(t, enrolled)
	f.writeGlobalConfig(globalConfigWith(map[string]string{enrolled[0]: "false"}))
	f.seedState(8, nil, map[string]state.ProviderOwnership{
		enrolled[0]: {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 8},
	})
	f.updateSnapshots(snapshots)
	return f
}

// TestQuotaGateExemptSignalComposition is AC.5 end to end through the real
// coordinator, in three coupled parts: (b) a single-member pool of an exempt
// out-of-quota signal-hot provider pool-skips and releases its held signal
// claim, byte-identical to the healthy singleton; (c) when a pool IS skipped,
// the exempt member's own held claim and the non-exempt member's claim both
// release; (a) an exempt out-of-quota member that is NOT signal-hot never
// makes a signal-hot sibling's pool look covered, so the sibling still gates.
func TestQuotaGateExemptSignalComposition(t *testing.T) {
	// markExempt flips provider id's quota_gate to the opt-out.
	markExempt := func(f *gateFixture, id string) {
		t.Helper()
		m := f.desired.Providers[policy.MappingID(id)]
		m.Quota.Gate = policy.QuotaGateConfig{Disabled: true}
		f.desired.Providers[policy.MappingID(id)] = m
	}

	t.Run("exempt singleton is byte-identical to the healthy singleton", func(t *testing.T) {
		now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		healthy := signalClaimFixture(t, []string{"gp"}, map[string]*quota.QuotaSnapshot{
			"gp": signalSeedSnapshot(now, 0.68), // signal-hot
		})
		exempt := signalClaimFixture(t, []string{"gp"}, map[string]*quota.QuotaSnapshot{
			"gp": exhaustedSignalSnapshot(now, 0.68), // signal-hot AND out of quota
		})
		markExempt(exempt, "gp")
		startBytes := healthy.readGlobalConfig() // both fixtures start from the same disabled bytes
		if got := exempt.readGlobalConfig(); got != startBytes {
			t.Fatal("fixture comparison broken: the two fixtures must start identically")
		}

		hOut := healthy.coordinator().Reconcile(context.Background(), false, false, false)
		eOut := exempt.coordinator().Reconcile(context.Background(), false, false, false)
		if !hOut.Accepted || !eOut.Accepted || hOut.PendingCount() != 0 || eOut.PendingCount() != 0 {
			t.Fatalf("healthy=%+v exempt=%+v", hOut, eOut)
		}
		if got := healthy.readGlobalConfig(); got == startBytes {
			t.Fatal("healthy singleton did not restore its baseline")
		}
		healthyBytes := healthy.readGlobalConfig()
		if got := exempt.readGlobalConfig(); got != healthyBytes {
			t.Fatalf("exempt singleton bytes differ from the healthy singleton:\n--- exempt ---\n%s\n--- healthy ---\n%s", got, healthyBytes)
		}
		if got := gateRow(t, eOut.Targets[0], "gp"); got.Action != GateActionRestored {
			t.Fatalf("exempt singleton gate row = %+v, want restored like the healthy singleton", got)
		}
		if st := exempt.loadState(); len(st.ProviderOwnership) != 0 {
			t.Fatalf("exempt singleton claims = %+v, want the held signal claim released", st.ProviderOwnership)
		}
	})

	t.Run("a skipped pool releases the exempt member's own claim and the sibling's claim", func(t *testing.T) {
		now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		f := newSignalFixture(t, []string{"gp", "pp"})
		markExempt(f, "gp")
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "false", "pp": "false"}))
		f.seedState(8, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: exhaustedSignalSnapshot(now, 0.68)}, // out of quota AND signal-hot
			"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},      // signal-hot, healthy quota
		}, map[string]state.ProviderOwnership{
			"gp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 8},
			"pp": {BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisSignal, Threshold: 0.0, EngagedRevision: 8},
		})

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		st := f.loadState()
		if len(st.ProviderOwnership) != 0 {
			t.Fatalf("claims = %+v, want both claims released by the pool skip", st.ProviderOwnership)
		}
		for _, id := range []string{"gp", "pp"} {
			if row := gateRow(t, out.Targets[0], id); row.Action != GateActionRestored {
				t.Fatalf("%s gate row = %+v, want restored", id, row)
			}
		}
	})

	t.Run("exhausted exempt member never masks a signal-hot sibling", func(t *testing.T) {
		now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		f := newSignalFixture(t, []string{"gp", "pp"})
		markExempt(f, "gp")
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: exhaustedSignalSnapshot(now, 0.10)}, // out of quota, signal +1.60 (not hot)
			"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},      // signal-hot
		}, nil)
		before := f.readGlobalConfig()

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		// pp gates off with a signal claim; gp stays enabled and unclaimed
		// (quota gated nothing, and the signal axis did not propose a gate
		// for gp).
		after := f.readGlobalConfig()
		if after == before {
			t.Fatal("pass produced no byte change")
		}
		gpBlock := strings.Split(strings.Split(after, "  gp:\n")[1], "  pp:")[0]
		if !strings.Contains(gpBlock, "enabled: true") {
			t.Fatalf("gp was gated off or altered:\n%s", after)
		}
		ppBlock := strings.Split(strings.Split(after, "  pp:\n")[1], "  zz:")[0]
		if !strings.Contains(ppBlock, "enabled: false") {
			t.Fatalf("pp was not gated off while gp stayed enabled:\n%s", after)
		}
		st := f.loadState()
		if claim, ok := st.ProviderOwnership["pp"]; !ok || !claim.Owned || claim.Axis != state.OwnershipAxisSignal {
			t.Fatalf("pp claim = %+v, want the normal signal claim", claim)
		}
		if _, held := st.ProviderOwnership["gp"]; held {
			t.Fatalf("gp claimed: %+v", st.ProviderOwnership)
		}
		if row := gateRow(t, out.Targets[0], "gp"); row.Action != GateActionUnchanged {
			t.Fatalf("gp gate row = %+v, want unchanged", row)
		}
	})

	t.Run("exempt signal-hot provider in a non-skipped pool is still signal-gated", func(t *testing.T) {
		// quota_gate must not spare the provider from the SIGNAL axis: with a
		// third not-gated enabled member keeping the pool open, the exempt
		// signal-hot provider is disabled by the signal branch with a
		// signal-attributed claim while the quota axis itself plans nothing.
		now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
		f := newSignalFixture(t, []string{"gp", "pp", "zz"})
		markExempt(f, "gp")
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: exhaustedSignalSnapshot(now, 0.68)}, // out of quota AND signal-hot
			"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},      // healthy, not gated
			"zz": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},      // healthy, not gated
		}, nil)
		before := f.readGlobalConfig()

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		after := f.readGlobalConfig()
		if after == before {
			t.Fatal("pass produced no byte change")
		}
		gpBlock := strings.Split(strings.Split(after, "  gp:\n")[1], "  pp:")[0]
		if !strings.Contains(gpBlock, "enabled: false") {
			t.Fatalf("exempt signal-hot provider was spared by quota_gate:\n%s", after)
		}
		ppBlock := strings.Split(strings.Split(after, "  pp:\n")[1], "  zz:")[0]
		if !strings.Contains(ppBlock, "enabled: true") {
			t.Fatalf("pp was gated off:\n%s", after)
		}
		zzBlock := strings.Split(strings.SplitN(after, "  zz:\n", 2)[1], "\nmodels:")[0]
		if !strings.Contains(zzBlock, "enabled: true") {
			t.Fatalf("zz was gated off:\n%s", after)
		}
		st := f.loadState()
		if claim, ok := st.ProviderOwnership["gp"]; !ok || !claim.Owned || claim.Axis != state.OwnershipAxisSignal {
			t.Fatalf("gp claim = %+v, want a signal-attributed claim (the disable came from the signal axis, not quota)", claim)
		}
		if len(st.ProviderOwnership) != 1 {
			t.Fatalf("claims = %+v, want only gp's signal claim", st.ProviderOwnership)
		}
		if row := gateRow(t, out.Targets[0], "gp"); row.Action != GateActionDisabled || row.Axis != state.OwnershipAxisSignal {
			t.Fatalf("gp gate row = %+v, want a signal-axis disable", row)
		}
	})
}

// TestSignalGateUnresolvedDefinitionReferencesAreAdvisory mirrors the live
// incident shape: a facet's primary model is a model_group polytoken-ref
// naming a group no layer defines, and its fallback list names an undefined
// model. Definition-reference uncertainty is no signal evidence and gates
// nothing, so the signal-gated disable proceeds and publishes.
func TestSignalGateUnresolvedDefinitionReferencesAreAdvisory(t *testing.T) {
	f := newSignalFixture(t, []string{"gp", "pp"})
	if err := os.MkdirAll(filepath.Join(f.globalRoot, "facets"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: PM\npolytoken:\n" +
		"  model: <polytoken-ref type=\"model_group\" name=\"pm_facet\"/>\n" +
		"  fallback_models:\n    - some/undefined\n---\n"
	if err := os.WriteFile(filepath.Join(f.globalRoot, "facets", "pm.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)}, // gp overdrawn
		"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
	}, nil)
	before := f.readGlobalConfig()

	out := f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 {
		t.Fatalf("out=%+v want the disable published", out)
	}
	want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
	if got := f.readGlobalConfig(); got != want {
		t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	st := f.loadState()
	claim, ok := st.ProviderOwnership["gp"]
	if !ok || !claim.Owned {
		t.Fatalf("gp ownership claim = %+v, want a signal claim", claim)
	}
}

// TestSignalGatePublishesWithUnresolvableGroups mirrors the operator incident
// that removed the offline safety analyzer: configured groups referencing
// models no layer defines, plus an already-empty group. The signal gate
// decides, the candidate is staged and validated through the real runner
// shape, and the disable publishes with an ownership claim and a provider
// notice. The dry-run variant of the same shape evaluates through staging and
// validation but publishes and saves nothing.
func TestSignalGatePublishesWithUnresolvableGroups(t *testing.T) {
	unresolvable := strings.Replace(globalConfigWith(nil),
		"modelgroups:\n",
		"modelgroups:\n  ai_workflow: [ghost/undefined-model, gp/g1]\n  drained: []\n", 1)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	seed := map[string]state.ProviderState{
		"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)}, // gp overdrawn
		"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
	}

	t.Run("signal disable publishes through staged validation", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		f.writeGlobalConfig(unresolvable)
		f.seedState(7, seed, nil)
		before := f.readGlobalConfig()

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v want the disable published", out)
		}
		want := strings.Replace(before, "    # operator-set value\n    enabled: true", "    # operator-set value\n    enabled: false", 1)
		if got := f.readGlobalConfig(); got != want {
			t.Fatalf("byte mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
		}
		st := f.loadState()
		claim, ok := st.ProviderOwnership["gp"]
		if !ok || !claim.Owned {
			t.Fatalf("gp ownership claim = %+v, want a signal claim", claim)
		}
		doc := readGateNotice(t, f.desired.Operational.NoticePath)
		wantProviders := []notice.ProviderState{
			{ID: "gp", Enabled: false, Reason: "signal-gated (-0.72 <= +0.00)"},
		}
		if len(doc.Providers) != len(wantProviders) || doc.Providers[0] != wantProviders[0] {
			t.Fatalf("notice providers = %+v", doc.Providers)
		}
	})

	t.Run("dry run stages and validates but publishes and saves nothing", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		f.writeGlobalConfig(unresolvable)
		f.seedState(7, seed, nil)

		out := f.coordinator().Reconcile(context.Background(), true, false, false)
		if !out.Accepted || out.PendingCount() != 0 || out.Revision != 7 {
			t.Fatalf("out=%+v want a clean evaluated dry run", out)
		}
		if got := f.readGlobalConfig(); got != unresolvable {
			t.Fatal("dry run edited the live config")
		}
		if f.journalExists() {
			t.Fatal("dry run wrote a journal")
		}
		st := f.loadState()
		if st.Revision != 7 || len(st.ProviderOwnership) != 0 || len(st.ReconcileHistory.Records) != 0 {
			t.Fatalf("dry run mutated state: revision=%d ownership=%+v history=%d", st.Revision, st.ProviderOwnership, len(st.ReconcileHistory.Records))
		}
		joined := strings.Join(f.runner.calls, "\n")
		if !strings.Contains(joined, "quota-stage-global") {
			t.Fatalf("dry run did not stage/validate the global root:\n%s", joined)
		}
	})
}

// TestSignalGateRecoveryRestoresBaseline proves AC.4 for both restore shapes:
// an explicit-true baseline is rewritten true, and an absent-key baseline has
// the inserted key removed — no byte-level residue in either shape.
func TestSignalGateRecoveryRestoresBaseline(t *testing.T) {
	t.Run("explicit true baseline is restored", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		now := f.clock.t
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},
			"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
		}, nil)
		before := f.readGlobalConfig()
		if out := f.coordinator().Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": signalSeedSnapshot(now, 0.10)})
		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 || out.Revision != 9 {
			t.Fatalf("recover out=%+v err=%v", out, out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatalf("baseline not restored exactly:\n--- got ---\n%s\n--- want ---\n%s", got, before)
		}
		st := f.loadState()
		if _, held := st.ProviderOwnership["gp"]; held {
			t.Fatalf("claim not released: %+v", st.ProviderOwnership)
		}
	})

	t.Run("absent-key baseline loses the inserted key", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "absent"}))
		now := f.clock.t
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},
			"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
		}, nil)
		original := f.readGlobalConfig()

		if out := f.coordinator().Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		gated := f.readGlobalConfig()
		if !strings.Contains(gated, "    auth: {type: no_auth}\n    enabled: false\n") {
			t.Fatalf("absent baseline not gated off:\n%s", gated)
		}
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": signalSeedSnapshot(now, 0.10)})
		if out := f.coordinator().Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("recover out=%+v err=%v", out, out.Error)
		}
		if got := f.readGlobalConfig(); got != original {
			t.Fatalf("absent-key baseline not restored exactly:\n--- got ---\n%s\n--- want ---\n%s", got, original)
		}
		st := f.loadState()
		if len(st.ProviderOwnership) != 0 {
			t.Fatalf("claims = %+v, want none", st.ProviderOwnership)
		}
	})
}

// TestSignalClaimReleaseOnStaleness proves a held signal gate is released on
// degraded evidence: an aged snapshot past the freshness TTL, or a snapshot
// with no qualifying window, releases the claim through the normal branch and
// records the reason.
func TestSignalClaimReleaseOnStaleness(t *testing.T) {
	engage := func(t *testing.T, f *gateFixture, cl *gateClock) string {
		t.Helper()
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: signalSeedSnapshot(cl.t, 0.68)},
			"pp": {QuotaSnapshot: signalSeedSnapshot(cl.t, 0.10)},
		}, nil)
		before := f.readGlobalConfig()
		if out := f.gateCoordinator(cl).Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		return before
	}

	t.Run("stale snapshot releases the claim", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		cl := &gateClock{t: f.clock.t}
		before := engage(t, f, cl)
		cl.t = cl.t.Add(31 * time.Minute) // past the 30m freshness TTL
		out := f.gateCoordinator(cl).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatalf("stale evidence must restore the baseline:\n--- got ---\n%s\n--- want ---\n%s", got, before)
		}
		st := f.loadState()
		if _, held := st.ProviderOwnership["gp"]; held {
			t.Fatalf("claim not released on staleness: %+v", st.ProviderOwnership)
		}
		var recovered *state.EventRecord
		for i := range st.EventHistory.Events {
			if st.EventHistory.Events[i].Action == "signal_recovered" {
				recovered = &st.EventHistory.Events[i]
			}
		}
		if recovered == nil || recovered.Reason != "stale quota snapshot" {
			t.Fatalf("signal_recovered event missing or wrong: %+v", st.EventHistory.Events)
		}
	})

	t.Run("uncomputable signal releases the claim", func(t *testing.T) {
		f := newSignalFixture(t, []string{"gp", "pp"})
		cl := &gateClock{t: f.clock.t}
		before := engage(t, f, cl)
		// A fresh snapshot with only sub-day windows: signal is uncomputable.
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": {
			CheckedAt: cl.t, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{
				Used: fptr64(99), Limit: fptr64(100),
				ResetAt: tptr64(cl.t.Add(2 * time.Hour)), Period: durptr64(5 * time.Hour),
			}},
		}})
		out := f.gateCoordinator(cl).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatalf("uncomputable signal must restore the baseline:\n--- got ---\n%s\n--- want ---\n%s", got, before)
		}
		if st := f.loadState(); len(st.ProviderOwnership) != 0 {
			t.Fatalf("claims = %+v, want none", st.ProviderOwnership)
		}
	})
}

// TestSignalGateIgnoresSubDayWindowsAndStaleSnapshots proves degraded or
// ineligible evidence never engages the signal axis in the first place.
func TestSignalGateIgnoresSubDayWindowsAndStaleSnapshots(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		snap  *quota.QuotaSnapshot
		clock *gateClock
	}{
		{
			name: "sub-day windows never gate",
			snap: func() *quota.QuotaSnapshot {
				return &quota.QuotaSnapshot{
					CheckedAt: base, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
					Windows: []quota.QuotaWindow{{
						Used: fptr64(99), Limit: fptr64(100),
						ResetAt: tptr64(base.Add(2 * time.Hour)), Period: durptr64(5 * time.Hour),
					}},
				}
			}(),
		},
		{
			name:  "stale snapshot never gates",
			snap:  signalSeedSnapshot(base.Add(-31*time.Minute), 0.68),
			clock: &gateClock{t: base},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignalFixture(t, []string{"gp", "pp"})
			before := f.readGlobalConfig()
			f.seedState(7, map[string]state.ProviderState{
				"gp": {QuotaSnapshot: tc.snap},
				"pp": {QuotaSnapshot: signalSeedSnapshot(base, 0.10)},
			}, nil)
			cl := tc.clock
			if cl == nil {
				cl = &gateClock{t: base}
			}
			out := f.gateCoordinator(cl).Reconcile(context.Background(), false, false, false)
			if !out.Accepted || out.PendingCount() != 0 {
				t.Fatalf("out=%+v err=%v", out, out.Error)
			}
			if got := f.readGlobalConfig(); got != before {
				t.Fatalf("ineligible evidence must not edit bytes:\n--- got ---\n%s", got)
			}
			if st := f.loadState(); len(st.ProviderOwnership) != 0 {
				t.Fatalf("claims = %+v, want none", st.ProviderOwnership)
			}
			row := gateRow(t, out.Targets[0], "gp")
			if row.Action != GateActionUnchanged {
				t.Fatalf("gp gate row = %+v, want unchanged", row)
			}
		})
	}
}

// TestLegacyReconcileUnchangedBySignal proves AC.6 isolation: legacy Build
// plan bytes are identical whether the mappings carry signal_gate
// configuration or not, and hot quota snapshots never change that.
func TestLegacyReconcileUnchangedBySignal(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	hot := map[string]state.ProviderState{
		"codex": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},
		"zai":   {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
	}
	mkDesired := func(withGate bool) policy.Desired {
		gate := policy.SignalGateConfig{}
		if withGate {
			gate = policy.SignalGateConfig{Disabled: true, Threshold: -0.5}
		}
		return policy.Desired{
			Version: 1,
			Providers: map[policy.MappingID]policy.Mapping{
				"codex": {Models: map[string]policy.ModelBaseline{"codex/g1": {Enabled: true, HadEnabledKey: true}}, Quota: &policy.QuotaConfig{Adapter: "codex", SignalGate: gate}},
				"zai":   {Models: map[string]policy.ModelBaseline{"zai/g1": {Enabled: true, HadEnabledKey: true}}, Quota: &policy.QuotaConfig{Adapter: "zai"}},
			},
			Global: policy.Target{
				ID: "global", Global: true,
				Full: policy.Chain{"codex/g1", "zai/g1"},
			},
		}
	}
	target := policy.Target{ID: "global", Global: true}
	observed := state.State{Revision: 3, Providers: hot}

	planWith, err := reconcile.Build(mkDesired(true), observed, target, nil)
	if err != nil {
		t.Fatalf("Build with signal config: %v", err)
	}
	planWithout, err := reconcile.Build(mkDesired(false), observed, target, nil)
	if err != nil {
		t.Fatalf("Build without signal config: %v", err)
	}
	a, _ := json.Marshal(planWith)
	b, _ := json.Marshal(planWithout)
	if string(a) != string(b) {
		t.Fatalf("legacy plans diverge with signal config present:\n%s\nvs\n%s", a, b)
	}
	if len(planWith.Edits) == 0 {
		t.Fatalf("legacy plan unexpectedly empty")
	}
}

// TestSignalGateDryRunReportsVerdicts proves a dry run reports the same
// sanitized per-provider gate verdicts as an applied pass — without editing
// bytes, persisting claims, or recording history.
func TestSignalGateDryRunReportsVerdicts(t *testing.T) {
	f := newSignalFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: signalSeedSnapshot(now, 0.68)},
		"pp": {QuotaSnapshot: signalSeedSnapshot(now, 0.10)},
	}, nil)
	before := f.readGlobalConfig()

	out := f.coordinator().Reconcile(context.Background(), true, false, false)
	if !out.Accepted || out.PendingCount() != 0 || out.Revision != 7 {
		t.Fatalf("out=%+v err=%v", out, out.Error)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatalf("dry run edited bytes:\n--- got ---\n%s", got)
	}
	st := f.loadState()
	if len(st.ProviderOwnership) != 0 {
		t.Fatalf("dry run persisted claims: %+v", st.ProviderOwnership)
	}
	if len(st.EventHistory.Events) != 0 {
		t.Fatalf("dry run recorded history: %+v", st.EventHistory.Events)
	}
	if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
		t.Fatalf("dry run published a notice: %v", err)
	}
	row := gateRow(t, out.Targets[0], "gp")
	if row.Action != GateActionDisabled || row.Axis != state.OwnershipAxisSignal || row.Signal == nil || routing.SignalFormat(*row.Signal) != "-0.72" {
		t.Fatalf("dry-run gp gate row = %+v, want the same verdict as an applied pass", row)
	}

	// The verbose trace carries the same verdicts for rendering.
	verbose := f.coordinator().Reconcile(context.Background(), true, false, true)
	if !verbose.Accepted {
		t.Fatalf("verbose dry run out=%+v err=%v", verbose, verbose.Error)
	}
	trace := verbose.Targets[0].Trace
	if trace == nil || len(trace.ProviderGates) == 0 {
		t.Fatalf("verbose dry run trace missing gate summaries: %+v", trace)
	}
	if trace.ProviderGates[0].Provider != "gp" || trace.ProviderGates[0].Action != GateActionDisabled {
		t.Fatalf("verbose trace rows = %+v", trace.ProviderGates)
	}
}
