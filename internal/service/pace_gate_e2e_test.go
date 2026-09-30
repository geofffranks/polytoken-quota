package service

// Pace-gate end-to-end tests (pace gating AC.1/2/3/4/6): the REAL coordinator
// path — real policy loader, target registry, staging, validate runner driven
// by the scripted CommandRunner, publisher with journal/backups, and state
// store — against the synthetic gate fixture, with enrolled providers carrying
// quota configuration and seeded quota snapshots.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// paceClock is a mutable clock so tests can age snapshots past the freshness
// TTL between passes (the shared fixture clock is fixed).
type paceClock struct{ t time.Time }

func (c *paceClock) Now() time.Time { return c.t }

// newPaceFixture is a gate fixture whose enrolled providers carry quota
// configuration so pace verdicts can engage.
func newPaceFixture(t *testing.T, enrolled []string) *gateFixture {
	t.Helper()
	f := newGateFixture(t, enrolled, nil)
	for _, id := range enrolled {
		f.desired.Providers[policy.MappingID(id)] = policy.Mapping{Quota: &policy.QuotaConfig{Adapter: "codex"}}
	}
	return f
}

// paceSeedSnapshot builds a fresh snapshot whose two-day window is exactly
// half elapsed at now, so the projection pace is exactly twice usedFrac.
func paceSeedSnapshot(now time.Time, usedFrac float64) *quota.QuotaSnapshot {
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

func (f *gateFixture) paceCoordinator(cl *paceClock) *Coordinator {
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

// TestPaceGateEngagesAndClaims proves AC.1 end to end: a fresh over-threshold
// snapshot gates the provider with an exact-span, byte-preserving edit, a
// pace-attributed ownership claim, a durable pace_gated event, outcome gate
// summaries naming the axis/pace/threshold, and a notice carrying the pace
// reason.
func TestPaceGateEngagesAndClaims(t *testing.T) {
	f := newPaceFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)}, // pace 136%
		"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)}, // pace 20%
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
	if !ok || claim != (state.ProviderOwnership{BaselinePresent: true, BaselineValue: true, Owned: true, Axis: state.OwnershipAxisPace, Threshold: 1.0, EngagedRevision: 8}) {
		t.Fatalf("claim = %+v, want a pace-attributed claim at revision 8", claim)
	}
	if _, held := st.ProviderOwnership["pp"]; held {
		t.Fatalf("pp claimed without pacing hot: %+v", st.ProviderOwnership)
	}
	// The durable timeline records the pace_gated transition with its reason.
	var gated *state.EventRecord
	for i := range st.EventHistory.Events {
		if st.EventHistory.Events[i].Action == "pace_gated" {
			gated = &st.EventHistory.Events[i]
		}
	}
	if gated == nil || gated.MappingID != "gp" || gated.Reason != "pace 136% >= threshold 100%" {
		t.Fatalf("pace_gated event missing or wrong: %+v", st.EventHistory.Events)
	}
	// The outcome channel names the axis, observed pace, and threshold.
	row := gateRow(t, out.Targets[0], "gp")
	if row.Action != GateActionDisabled || row.Axis != state.OwnershipAxisPace || row.Pace == nil || routingPct(*row.Pace) != 136 || row.Threshold == nil || routingPct(*row.Threshold) != 100 {
		t.Fatalf("gp gate row = %+v", row)
	}
	// The notice explains the committed state with the pace reason; only
	// providers with fresh committed edits appear.
	doc := readGateNotice(t, f.desired.Operational.NoticePath)
	wantProviders := []notice.ProviderState{
		{ID: "gp", Enabled: false, Reason: "pace-gated (136% >= 100%)"},
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

func routingPct(v float64) int { return int(v*100 + 0.5) }

// TestPacePoolAllHotSkipReleasesPaceClaims proves AC.2 across passes: with gp
// pace-held and pp hot on the next pass, the pool is fully over pace, so no
// pace edits are written, gp's baseline is restored, and a pace_recovered
// event records the pool escape hatch.
func TestPacePoolAllHotSkipReleasesPaceClaims(t *testing.T) {
	f := newPaceFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)},
		"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
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

	// Pass 2: pp goes hot too — the pool is fully over pace.
	f.updateSnapshots(map[string]*quota.QuotaSnapshot{"pp": paceSeedSnapshot(now, 0.68)})
	out = f.coordinator().Reconcile(context.Background(), false, false, false)
	if !out.Accepted || out.PendingCount() != 0 || out.Revision != 9 {
		t.Fatalf("pass2 out=%+v err=%v", out, out.Error)
	}
	if got := f.readGlobalConfig(); got != before {
		t.Fatalf("pass2 must restore gp's baseline and never gate pp:\n--- got ---\n%s\n--- want ---\n%s", got, before)
	}
	st := f.loadState()
	if len(st.ProviderOwnership) != 0 {
		t.Fatalf("claims = %+v, want every pace claim released", st.ProviderOwnership)
	}
	var recovered *state.EventRecord
	for i := range st.EventHistory.Events {
		if st.EventHistory.Events[i].Action == "pace_recovered" {
			recovered = &st.EventHistory.Events[i]
		}
	}
	if recovered == nil || recovered.MappingID != "gp" || !strings.Contains(recovered.Reason, "pool fully over pace") {
		t.Fatalf("pace_recovered event missing or wrong: %+v", st.EventHistory.Events)
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

// TestPaceGateAnalyzerRefusal proves AC.3: a pace-disable the analyzer proves
// unsafe, or cannot prove either way (pending-unknown), refuses the pass with
// no bytes and no persisted claim.
func TestPaceGateAnalyzerRefusal(t *testing.T) {
	t.Run("unsafe pace disable is refused", func(t *testing.T) {
		f := newPaceFixture(t, []string{"gp", "pp"})
		cfg := strings.Replace(globalConfigWith(nil), "polytoken:default_model_full: zz/z1", "polytoken:default_model_full: pp/p1", 1)
		f.writeGlobalConfig(cfg)
		now := f.clock.t
		f.seedState(5, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
			"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)}, // pp hot, owns the tier default
		}, nil)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want one pending refusal", out)
		}
		if !strings.Contains(out.Targets[0].Pending.Summary, "unsafe") {
			t.Fatalf("summary=%q want an unsafe verdict", out.Targets[0].Pending.Summary)
		}
		if got := f.readGlobalConfig(); got != cfg {
			t.Fatal("unsafe pace candidate was published")
		}
		if len(f.runner.calls) != 0 {
			t.Fatalf("staging ran despite the analyzer refusal: %v", f.runner.calls)
		}
		if f.journalExists() {
			t.Fatal("journal written for a refused publication")
		}
		if _, err := os.Stat(f.desired.Operational.NoticePath); !os.IsNotExist(err) {
			t.Fatalf("refused pass emitted a notice: %v", err)
		}
		f.requireNoPendingEdit(t, f.loadState())
	})

	t.Run("pending-unknown pace disable is refused", func(t *testing.T) {
		f := newPaceFixture(t, []string{"gp", "pp"})
		// The failover group carries a ghost leaf: the analyzer cannot prove
		// the group survives gp's disable, so the write is never authorized.
		cfg := strings.Replace(globalConfigWith(nil), "failover: [gp/g1, pp/p1, zz/z1]", "failover: [gp/g1, ghost/undefined, zz/z1]", 1)
		f.writeGlobalConfig(cfg)
		now := f.clock.t
		f.seedState(5, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)}, // gp hot
			"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
		}, nil)

		out := f.coordinator().Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 1 {
			t.Fatalf("out=%+v want one pending refusal", out)
		}
		if !strings.Contains(out.Targets[0].Pending.Summary, "pending-unknown") {
			t.Fatalf("summary=%q want a pending-unknown verdict", out.Targets[0].Pending.Summary)
		}
		if got := f.readGlobalConfig(); got != cfg {
			t.Fatal("pending-unknown pace candidate was published")
		}
		f.requireNoPendingEdit(t, f.loadState())
	})
}

// TestPaceGateRecoveryRestoresBaseline proves AC.4 for both restore shapes:
// an explicit-true baseline is rewritten true, and an absent-key baseline has
// the inserted key removed — no byte-level residue in either shape.
func TestPaceGateRecoveryRestoresBaseline(t *testing.T) {
	t.Run("explicit true baseline is restored", func(t *testing.T) {
		f := newPaceFixture(t, []string{"gp", "pp"})
		now := f.clock.t
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)},
			"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
		}, nil)
		before := f.readGlobalConfig()
		if out := f.coordinator().Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": paceSeedSnapshot(now, 0.10)})
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
		f := newPaceFixture(t, []string{"gp", "pp"})
		f.writeGlobalConfig(globalConfigWith(map[string]string{"gp": "absent"}))
		now := f.clock.t
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)},
			"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
		}, nil)
		original := f.readGlobalConfig()

		if out := f.coordinator().Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		gated := f.readGlobalConfig()
		if !strings.Contains(gated, "    auth: {type: no_auth}\n    enabled: false\n") {
			t.Fatalf("absent baseline not gated off:\n%s", gated)
		}
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": paceSeedSnapshot(now, 0.10)})
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

// TestPaceClaimReleaseOnStaleness proves a held pace gate is released on
// degraded evidence: an aged snapshot past the freshness TTL, or a snapshot
// with no qualifying window, releases the claim through the normal branch and
// records the reason.
func TestPaceClaimReleaseOnStaleness(t *testing.T) {
	engage := func(t *testing.T, f *gateFixture, cl *paceClock) string {
		t.Helper()
		f.seedState(7, map[string]state.ProviderState{
			"gp": {QuotaSnapshot: paceSeedSnapshot(cl.t, 0.68)},
			"pp": {QuotaSnapshot: paceSeedSnapshot(cl.t, 0.10)},
		}, nil)
		before := f.readGlobalConfig()
		if out := f.paceCoordinator(cl).Reconcile(context.Background(), false, false, false); !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("engage out=%+v err=%v", out, out.Error)
		}
		return before
	}

	t.Run("stale snapshot releases the claim", func(t *testing.T) {
		f := newPaceFixture(t, []string{"gp", "pp"})
		cl := &paceClock{t: f.clock.t}
		before := engage(t, f, cl)
		cl.t = cl.t.Add(31 * time.Minute) // past the 30m freshness TTL
		out := f.paceCoordinator(cl).Reconcile(context.Background(), false, false, false)
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
			if st.EventHistory.Events[i].Action == "pace_recovered" {
				recovered = &st.EventHistory.Events[i]
			}
		}
		if recovered == nil || recovered.Reason != "stale quota snapshot" {
			t.Fatalf("pace_recovered event missing or wrong: %+v", st.EventHistory.Events)
		}
	})

	t.Run("uncomputable pace releases the claim", func(t *testing.T) {
		f := newPaceFixture(t, []string{"gp", "pp"})
		cl := &paceClock{t: f.clock.t}
		before := engage(t, f, cl)
		// A fresh snapshot with only sub-day windows: pace is uncomputable.
		f.updateSnapshots(map[string]*quota.QuotaSnapshot{"gp": {
			CheckedAt: cl.t, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
			Windows: []quota.QuotaWindow{{
				Used: fptr64(99), Limit: fptr64(100),
				ResetAt: tptr64(cl.t.Add(2 * time.Hour)), Period: durptr64(5 * time.Hour),
			}},
		}})
		out := f.paceCoordinator(cl).Reconcile(context.Background(), false, false, false)
		if !out.Accepted || out.PendingCount() != 0 {
			t.Fatalf("out=%+v err=%v", out, out.Error)
		}
		if got := f.readGlobalConfig(); got != before {
			t.Fatalf("uncomputable pace must restore the baseline:\n--- got ---\n%s\n--- want ---\n%s", got, before)
		}
		if st := f.loadState(); len(st.ProviderOwnership) != 0 {
			t.Fatalf("claims = %+v, want none", st.ProviderOwnership)
		}
	})
}

// TestPaceGateIgnoresSubDayWindowsAndStaleSnapshots proves degraded or
// ineligible evidence never engages the pace axis in the first place.
func TestPaceGateIgnoresSubDayWindowsAndStaleSnapshots(t *testing.T) {
	cases := []struct {
		name  string
		snap  *quota.QuotaSnapshot
		clock *paceClock
	}{
		{
			name: "sub-day windows never gate",
			snap: func() *quota.QuotaSnapshot {
				s := paceSeedSnapshot(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), 0.99)
				s.Windows = []quota.QuotaWindow{{
					Used: fptr64(99), Limit: fptr64(100),
					ResetAt: tptr64(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC).Add(2 * time.Hour)),
					Period:  durptr64(5 * time.Hour),
				}}
				return s
			}(),
		},
		{
			name:  "stale snapshot never gates",
			snap:  paceSeedSnapshot(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC).Add(-31*time.Minute), 0.68),
			clock: &paceClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPaceFixture(t, []string{"gp", "pp"})
			before := f.readGlobalConfig()
			f.seedState(7, map[string]state.ProviderState{
				"gp": {QuotaSnapshot: tc.snap},
				"pp": {QuotaSnapshot: paceSeedSnapshot(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), 0.10)},
			}, nil)
			cl := tc.clock
			if cl == nil {
				cl = &paceClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
			}
			out := f.paceCoordinator(cl).Reconcile(context.Background(), false, false, false)
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

// TestLegacyReconcileUnchangedByPace proves AC.6 isolation: legacy Build plan
// bytes are identical whether the mappings carry pace_gate configuration or
// not, and hot quota snapshots never change that.
func TestLegacyReconcileUnchangedByPace(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	hot := map[string]state.ProviderState{
		"codex": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)},
		"zai":   {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
	}
	mkDesired := func(withPace bool) policy.Desired {
		pace := policy.PaceGateConfig{}
		if withPace {
			pace = policy.PaceGateConfig{Enabled: false, Threshold: 1.5}
		}
		return policy.Desired{
			Version: 1,
			Providers: map[policy.MappingID]policy.Mapping{
				"codex": {Models: map[string]policy.ModelBaseline{"codex/g1": {Enabled: true, HadEnabledKey: true}}, Quota: &policy.QuotaConfig{Adapter: "codex", PaceGate: pace}},
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
		t.Fatalf("Build with pace config: %v", err)
	}
	planWithout, err := reconcile.Build(mkDesired(false), observed, target, nil)
	if err != nil {
		t.Fatalf("Build without pace config: %v", err)
	}
	a, _ := json.Marshal(planWith)
	b, _ := json.Marshal(planWithout)
	if string(a) != string(b) {
		t.Fatalf("legacy plans diverge with pace config present:\n%s\nvs\n%s", a, b)
	}
	if len(planWith.Edits) == 0 {
		t.Fatalf("legacy plan unexpectedly empty")
	}
}

// TestPaceGateDryRunReportsVerdicts proves a dry run reports the same
// sanitized per-provider gate verdicts as an applied pass — without editing
// bytes, persisting claims, or recording history.
func TestPaceGateDryRunReportsVerdicts(t *testing.T) {
	f := newPaceFixture(t, []string{"gp", "pp"})
	now := f.clock.t
	f.seedState(7, map[string]state.ProviderState{
		"gp": {QuotaSnapshot: paceSeedSnapshot(now, 0.68)},
		"pp": {QuotaSnapshot: paceSeedSnapshot(now, 0.10)},
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
	if row.Action != GateActionDisabled || row.Axis != state.OwnershipAxisPace || row.Pace == nil || routingPct(*row.Pace) != 136 {
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
