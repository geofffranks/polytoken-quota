package state

// Property suite for the provider state machine (machine.go), built on the
// test-only pgregory.net/rapid dependency.
//
// Expected behavior comes from a deliberately simple record model written
// from the machine.go contract comments. Production code under test —
// including its normalization, change-detection, and mode helpers — is never
// used to compute an expectation; only domain types and constants are shared.
// Generated histories stay inside the plan's bounded caps (at most 8 provider
// identities and 64 operations per case) and draw timestamps from a small
// fixed pool so identical- and distinct-timestamp repeats both occur.
//
// Deep snapshots are taken with test-owned clone helpers before every
// operation: comparing a cloned snapshot against a fresh clone of the earlier
// state detects an operation that mutated its input in place, which a direct
// comparison against the aliased map would miss.

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Independent model

// modelProvider mirrors exactly the fields the machine.go operations manage,
// plus a preserved copy of every other ProviderState field the operations
// must never touch.
type modelProvider struct {
	quota            Quota
	availability     Availability
	manualDisabled   bool
	manualDisabledAt time.Time
	quotaAt          time.Time
	availabilityAt   time.Time
	preserved        ProviderState
}

// metaSnapshot holds the State-level fields no machine operation may change.
type metaSnapshot struct {
	schema                   int
	revision                 uint64
	onChangeExecutedRevision uint64
	nextEventSequence        uint64
	nextArrivalSequence      uint64
	refreshFailed            []Diagnostic
}

type modelState struct {
	providers map[string]modelProvider
	meta      metaSnapshot
}

func modelValidQuota(q Quota) bool {
	switch q {
	case QuotaNormal, QuotaLow, QuotaExhausted:
		return true
	}
	return false
}

func modelValidAvailability(a Availability) bool {
	switch a {
	case Available, Unavailable:
		return true
	}
	return false
}

// modelMode implements the documented EffectiveMode truth table: sparse empty
// axes normalize to the healthy baseline, manual disable wins outright,
// unrecognized non-empty axes fail closed, unavailable/exhausted disable,
// available+low is reserve, and only available+normal is normal.
func modelMode(quota Quota, availability Availability, manualDisabled bool) Mode {
	q, a := quota, availability
	if q == "" {
		q = QuotaNormal
	}
	if a == "" {
		a = Available
	}
	if manualDisabled {
		return ModeDisabled
	}
	if !modelValidQuota(q) || !modelValidAvailability(a) {
		return ModeDisabled
	}
	if a == Unavailable || q == QuotaExhausted {
		return ModeDisabled
	}
	if q == QuotaLow {
		return ModeReserve
	}
	return ModeNormal
}

// ---------------------------------------------------------------------------
// Operations

type opKind string

const (
	opSet     opKind = "set"
	opClear   opKind = "clear"
	opDisable opKind = "disable"
	opEnable  opKind = "enable"
	opBatch   opKind = "batch-manual"
	opReset   opKind = "reset-manual"
)

type stateOp struct {
	kind          opKind
	provider      string
	all           bool
	hasQuota      bool
	quotaVal      Quota
	hasAvail      bool
	availVal      Availability
	batch         []string
	batchDisabled bool
	at            time.Time
}

func (op stateOp) patch() ProviderPatch {
	patch := ProviderPatch{}
	if op.hasQuota {
		q := op.quotaVal
		patch.Quota = &q
	}
	if op.hasAvail {
		a := op.availVal
		patch.Availability = &a
	}
	return patch
}

func (op stateOp) String() string {
	at := op.at.Format("15:04:05")
	switch op.kind {
	case opSet:
		fields := "empty"
		switch {
		case op.hasQuota && op.hasAvail:
			fields = fmt.Sprintf("quota=%q availability=%q", op.quotaVal, op.availVal)
		case op.hasQuota:
			fields = fmt.Sprintf("quota=%q", op.quotaVal)
		case op.hasAvail:
			fields = fmt.Sprintf("availability=%q", op.availVal)
		}
		return fmt.Sprintf("set provider=%q %s at=%s", op.provider, fields, at)
	case opClear:
		if op.all {
			return fmt.Sprintf("clear --all at=%s", at)
		}
		return fmt.Sprintf("clear provider=%q at=%s", op.provider, at)
	case opDisable:
		return fmt.Sprintf("disable provider=%q at=%s", op.provider, at)
	case opEnable:
		return fmt.Sprintf("enable provider=%q at=%s", op.provider, at)
	case opBatch:
		verb := "disable"
		if !op.batchDisabled {
			verb = "enable"
		}
		return fmt.Sprintf("batch %s %q at=%s", verb, op.batch, at)
	case opReset:
		return fmt.Sprintf("reset manual disables at=%s", at)
	}
	return fmt.Sprintf("unknown op %q", op.kind)
}

// applyProductionOp runs the operation through the production state machine.
func applyProductionOp(s State, op stateOp) (State, error) {
	switch op.kind {
	case opSet:
		return SetProvider(s, op.provider, op.patch(), op.at)
	case opClear:
		return ClearProvider(s, Selector{Provider: op.provider, All: op.all}, op.at)
	case opDisable:
		return DisableProvider(s, op.provider, op.at)
	case opEnable:
		return EnableProvider(s, op.provider, op.at)
	case opBatch:
		return SetManualDisabled(s, op.batch, op.batchDisabled, op.at)
	case opReset:
		return ResetManualDisables(s, op.at)
	}
	panic(fmt.Sprintf("unknown op kind %q", op.kind))
}

// apply mutates the model exactly when the documented contract says the
// production operation succeeds. Rejections are decided before any mutation,
// mirroring the all-or-nothing transition contract.
func (m *modelState) apply(op stateOp) error {
	switch op.kind {
	case opSet:
		return m.applySet(op)
	case opClear:
		return m.applyClear(op)
	case opDisable:
		return m.applyManual([]string{op.provider}, true, op.at)
	case opEnable:
		return m.applyManual([]string{op.provider}, false, op.at)
	case opBatch:
		return m.applyManual(op.batch, op.batchDisabled, op.at)
	case opReset:
		return m.applyReset(op.at)
	}
	panic(fmt.Sprintf("unknown op kind %q", op.kind))
}

func (m *modelState) applySet(op stateOp) error {
	if op.provider == "" {
		return errors.New("model: set requires a provider")
	}
	if !op.hasQuota && !op.hasAvail {
		return errors.New("model: set requires at least one field")
	}
	if op.hasQuota && !modelValidQuota(op.quotaVal) {
		return fmt.Errorf("model: invalid quota %q", op.quotaVal)
	}
	if op.hasAvail && !modelValidAvailability(op.availVal) {
		return fmt.Errorf("model: invalid availability %q", op.availVal)
	}
	rec, tracked := m.providers[op.provider]
	if !tracked {
		// SetProvider seeds an untracked provider with the healthy baseline
		// before applying the patch; the untouched axes keep zero timestamps.
		rec = modelProvider{quota: QuotaNormal, availability: Available}
	}
	if op.hasQuota {
		rec.quota = op.quotaVal
		rec.quotaAt = op.at
	}
	if op.hasAvail {
		rec.availability = op.availVal
		rec.availabilityAt = op.at
	}
	m.providers[op.provider] = rec
	return nil
}

func (m *modelState) applyClear(op stateOp) error {
	if !op.all && op.provider == "" {
		return errors.New("model: clear requires a provider or --all")
	}
	for id, rec := range m.providers {
		if op.all || id == op.provider {
			rec.quota = QuotaNormal
			rec.availability = Available
			rec.quotaAt = op.at
			rec.availabilityAt = op.at
			m.providers[id] = rec
		}
	}
	// A named untracked provider has no entry to clear: the documented no-op.
	return nil
}

func (m *modelState) applyManual(providers []string, disabled bool, at time.Time) error {
	for _, p := range providers {
		if p == "" {
			return errors.New("model: manual control requires non-empty provider IDs")
		}
	}
	for _, p := range providers {
		rec := m.providers[p]
		// Manual controls seed sparse (untracked or empty-string) axes with
		// the healthy baseline, then record the requested value and time.
		if rec.quota == "" {
			rec.quota = QuotaNormal
		}
		if rec.availability == "" {
			rec.availability = Available
		}
		rec.manualDisabled = disabled
		rec.manualDisabledAt = at
		m.providers[p] = rec
	}
	return nil
}

func (m *modelState) applyReset(at time.Time) error {
	for id, rec := range m.providers {
		if rec.manualDisabled {
			rec.manualDisabled = false
			rec.manualDisabledAt = at
			m.providers[id] = rec
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test-owned deep copies

// cloneQuotaWindow copies the window including its pointer fields.
func cloneQuotaWindow(w quota.QuotaWindow) quota.QuotaWindow {
	if w.Used != nil {
		v := *w.Used
		w.Used = &v
	}
	if w.Limit != nil {
		v := *w.Limit
		w.Limit = &v
	}
	if w.UsagePercent != nil {
		v := *w.UsagePercent
		w.UsagePercent = &v
	}
	if w.ResetAt != nil {
		v := *w.ResetAt
		w.ResetAt = &v
	}
	if w.Period != nil {
		v := *w.Period
		w.Period = &v
	}
	return w
}

// deepCopyProviderState copies a provider record deeply enough that no byte
// reachable through the machine's managed fields is shared with the source.
// Snapshot summaries are copied down to the window/attempt level seeded by
// this suite; deeper optional observations are never set here.
func deepCopyProviderState(ps ProviderState) ProviderState {
	if ps.QuotaSnapshot != nil {
		snap := *ps.QuotaSnapshot
		snap.Windows = make([]quota.QuotaWindow, len(ps.QuotaSnapshot.Windows))
		for i, w := range ps.QuotaSnapshot.Windows {
			snap.Windows[i] = cloneQuotaWindow(w)
		}
		if snap.UsageSummary != nil {
			us := *snap.UsageSummary
			snap.UsageSummary = &us
		}
		if snap.ResetCredits != nil {
			rc := *snap.ResetCredits
			snap.ResetCredits = &rc
		}
		ps.QuotaSnapshot = &snap
	}
	if ps.ResetCredits.LatestAttempt != nil {
		a := *ps.ResetCredits.LatestAttempt
		ps.ResetCredits.LatestAttempt = &a
	}
	if ps.ResetCredits.LastSuccess != nil {
		inv := *ps.ResetCredits.LastSuccess
		ps.ResetCredits.LastSuccess = &inv
	}
	if ps.ResetCredits.UsageSummary != nil {
		us := *ps.ResetCredits.UsageSummary
		ps.ResetCredits.UsageSummary = &us
	}
	if ps.Routing.Decision != nil {
		d := *ps.Routing.Decision
		ps.Routing.Decision = &d
	}
	return ps
}

func cloneProviders(in map[string]ProviderState) map[string]ProviderState {
	out := make(map[string]ProviderState, len(in))
	for k, v := range in {
		out[k] = deepCopyProviderState(v)
	}
	return out
}

func cloneModel(in modelState) modelState {
	out := modelState{providers: make(map[string]modelProvider, len(in.providers)), meta: in.meta}
	out.meta.refreshFailed = append([]Diagnostic(nil), in.meta.refreshFailed...)
	for k, rec := range in.providers {
		rec.preserved = deepCopyProviderState(rec.preserved)
		out.providers[k] = rec
	}
	return out
}

// ---------------------------------------------------------------------------
// Comparison against the model

// propTB is the minimal failure interface shared by *testing.T and *rapid.T.
type propTB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func compareProvider(tb propTB, stage, id string, got ProviderState, want modelProvider) {
	tb.Helper()
	if got.Quota != want.quota {
		tb.Errorf("%s: provider %s quota=%q want %q", stage, id, got.Quota, want.quota)
	}
	if got.Availability != want.availability {
		tb.Errorf("%s: provider %s availability=%q want %q", stage, id, got.Availability, want.availability)
	}
	if got.ManualDisabled != want.manualDisabled {
		tb.Errorf("%s: provider %s manualDisabled=%t want %t", stage, id, got.ManualDisabled, want.manualDisabled)
	}
	if !got.ManualDisabledAt.Equal(want.manualDisabledAt) {
		tb.Errorf("%s: provider %s manualDisabledAt=%v want %v", stage, id, got.ManualDisabledAt, want.manualDisabledAt)
	}
	if !got.QuotaAt.Equal(want.quotaAt) {
		tb.Errorf("%s: provider %s quotaAt=%v want %v", stage, id, got.QuotaAt, want.quotaAt)
	}
	if !got.AvailabilityAt.Equal(want.availabilityAt) {
		tb.Errorf("%s: provider %s availabilityAt=%v want %v", stage, id, got.AvailabilityAt, want.availabilityAt)
	}
	p := want.preserved
	if got.QuotaArrival != p.QuotaArrival || got.AvailabilityArrival != p.AvailabilityArrival {
		tb.Errorf("%s: provider %s arrivals changed: got (%d,%d) want (%d,%d)",
			stage, id, got.QuotaArrival, got.AvailabilityArrival, p.QuotaArrival, p.AvailabilityArrival)
	}
	if !reflect.DeepEqual(got.QuotaSnapshot, p.QuotaSnapshot) {
		tb.Errorf("%s: provider %s quota snapshot changed: got %+v want %+v", stage, id, got.QuotaSnapshot, p.QuotaSnapshot)
	}
	if !reflect.DeepEqual(got.ResetCredits, p.ResetCredits) {
		tb.Errorf("%s: provider %s reset credits changed: got %+v want %+v", stage, id, got.ResetCredits, p.ResetCredits)
	}
	if !reflect.DeepEqual(got.Routing, p.Routing) {
		tb.Errorf("%s: provider %s routing metadata changed: got %+v want %+v", stage, id, got.Routing, p.Routing)
	}
}

func compareProviders(tb propTB, stage string, providers map[string]ProviderState, model map[string]modelProvider) {
	tb.Helper()
	if len(providers) != len(model) {
		tb.Errorf("%s: provider count=%d want %d", stage, len(providers), len(model))
	}
	for id, rec := range model {
		ps, ok := providers[id]
		if !ok {
			tb.Errorf("%s: provider %q missing from state", stage, id)
			continue
		}
		compareProvider(tb, stage, id, ps, rec)
		want := modelMode(rec.quota, rec.availability, rec.manualDisabled)
		if got := EffectiveMode(ps); got != want {
			tb.Errorf("%s: provider %s EffectiveMode=%s want %s", stage, id, got, want)
		}
	}
	for id := range providers {
		if _, ok := model[id]; !ok {
			tb.Errorf("%s: unexpected provider %q in state", stage, id)
		}
	}
}

func compareAll(tb propTB, stage string, s State, model modelState) {
	tb.Helper()
	meta := model.meta
	if s.Schema != meta.schema {
		tb.Errorf("%s: schema=%d want %d", stage, s.Schema, meta.schema)
	}
	if s.Revision != meta.revision {
		tb.Errorf("%s: revision=%d want %d", stage, s.Revision, meta.revision)
	}
	if s.OnChangeExecutedRevision != meta.onChangeExecutedRevision {
		tb.Errorf("%s: onChangeExecutedRevision=%d want %d", stage, s.OnChangeExecutedRevision, meta.onChangeExecutedRevision)
	}
	if s.NextEventSequence != meta.nextEventSequence {
		tb.Errorf("%s: nextEventSequence=%d want %d", stage, s.NextEventSequence, meta.nextEventSequence)
	}
	if s.NextArrivalSequence != meta.nextArrivalSequence {
		tb.Errorf("%s: nextArrivalSequence=%d want %d", stage, s.NextArrivalSequence, meta.nextArrivalSequence)
	}
	if !reflect.DeepEqual(s.RefreshFailed, meta.refreshFailed) {
		tb.Errorf("%s: refresh diagnostics changed: got %+v want %+v", stage, s.RefreshFailed, meta.refreshFailed)
	}
	compareProviders(tb, stage, s.Providers, model.providers)
}

// ---------------------------------------------------------------------------
// Shared replay driver

// replayHistory applies each operation to both production and model, compares
// the result after every step, and verifies the operation never mutated its
// input state. The first divergence fails via tb.
func replayHistory(tb propTB, s State, model modelState, ops []stateOp) {
	tb.Helper()
	modelStart := cloneModel(model)
	initialSnapshot := cloneProviders(s.Providers)

	for i, op := range ops {
		stage := fmt.Sprintf("history op %d/%d: %s", i+1, len(ops), op)
		prev := s
		pre := cloneProviders(prev.Providers)

		next, prodErr := applyProductionOp(prev, op)
		modelErr := model.apply(op)
		if (prodErr == nil) != (modelErr == nil) {
			tb.Fatalf("%s: acceptance mismatch: production error=%v, model error=%v", stage, prodErr, modelErr)
		}
		compareAll(tb, stage+" [result]", next, model)
		if !reflect.DeepEqual(cloneProviders(prev.Providers), pre) {
			tb.Fatalf("%s: operation mutated its input state", stage)
		}
		s = next
	}

	compareAll(tb, "final", s, model)
	// The deep snapshot of the initial state must be untouched by the whole
	// history, whatever the final state looks like.
	compareProviders(tb, "initial snapshot after full history", initialSnapshot, modelStart.providers)
}

// ---------------------------------------------------------------------------
// Generators

var providerPool = []string{"codex", "zai", "vertex", "openrouter"}
var providerPoolWithEmpty = append(append([]string{}, providerPool...), "")

var baseTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// opTimestampPool deliberately repeats a handful of instants so histories
// replay operations at identical timestamps and at distinct ones.
var opTimestampPool = []time.Time{
	baseTime,
	baseTime.Add(time.Minute),
	baseTime.Add(2 * time.Minute),
	baseTime.Add(90 * time.Minute),
	baseTime.Add(24 * time.Hour),
	baseTime.Add(24*time.Hour + time.Minute),
	baseTime.Add(48 * time.Hour),
}

// Seed axes may be sparse (empty), healthy, or corrupt; zero timestamps are
// allowed for axes that were never written after migration.
var seedQuotaChoices = []Quota{"", QuotaNormal, QuotaNormal, QuotaLow, QuotaExhausted, Quota("corrupt-axis")}
var seedAvailabilityChoices = []Availability{"", Available, Available, Unavailable, Availability("corrupt-axis")}
var seedTimestampPool = append([]time.Time{{}}, opTimestampPool...)

// Set patches skew valid values but include non-empty invalid ones so
// rejection paths are exercised alongside accepted writes.
var setQuotaChoices = []Quota{QuotaNormal, QuotaNormal, QuotaLow, QuotaLow, QuotaExhausted, QuotaExhausted, Quota("stale-garbage")}
var setAvailabilityChoices = []Availability{Available, Available, Unavailable, Unavailable, Availability("stale-garbage")}

var opGen = rapid.Custom(func(t *rapid.T) stateOp {
	op := stateOp{
		kind: rapid.SampledFrom([]opKind{
			opSet, opSet, opSet, opSet,
			opClear, opClear,
			opDisable, opEnable, opBatch, opReset,
		}).Draw(t, "kind"),
		at: rapid.SampledFrom(opTimestampPool).Draw(t, "at"),
	}
	switch op.kind {
	case opSet:
		op.provider = rapid.SampledFrom(providerPoolWithEmpty).Draw(t, "provider")
		op.hasQuota = rapid.Bool().Draw(t, "hasQuota")
		op.quotaVal = rapid.SampledFrom(setQuotaChoices).Draw(t, "quota")
		op.hasAvail = rapid.Bool().Draw(t, "hasAvail")
		op.availVal = rapid.SampledFrom(setAvailabilityChoices).Draw(t, "availability")
	case opClear:
		op.provider = rapid.SampledFrom(providerPool).Draw(t, "provider")
		op.all = rapid.Bool().Draw(t, "all")
	case opDisable, opEnable:
		op.provider = rapid.SampledFrom(providerPoolWithEmpty).Draw(t, "provider")
	case opBatch:
		op.batch = rapid.SliceOfN(rapid.SampledFrom(providerPoolWithEmpty), 0, 4).Draw(t, "batch")
		op.batchDisabled = rapid.Bool().Draw(t, "disabled")
	case opReset:
	}
	return op
})

type seedProviderSpec struct {
	id             string
	quota          Quota
	availability   Availability
	manual         bool
	quotaAt        time.Time
	availabilityAt time.Time
	manualAt       time.Time
	rank           int
	arrival        uint64
}

func seedProviderSpecGen(id string) *rapid.Generator[seedProviderSpec] {
	return rapid.Custom(func(t *rapid.T) seedProviderSpec {
		return seedProviderSpec{
			id:             id,
			quota:          rapid.SampledFrom(seedQuotaChoices).Draw(t, "seed["+id+"].quota"),
			availability:   rapid.SampledFrom(seedAvailabilityChoices).Draw(t, "seed["+id+"].availability"),
			manual:         rapid.Bool().Draw(t, "seed["+id+"].manual"),
			quotaAt:        rapid.SampledFrom(seedTimestampPool).Draw(t, "seed["+id+"].quotaAt"),
			availabilityAt: rapid.SampledFrom(seedTimestampPool).Draw(t, "seed["+id+"].availabilityAt"),
			manualAt:       rapid.SampledFrom(seedTimestampPool).Draw(t, "seed["+id+"].manualAt"),
			rank:           rapid.IntRange(0, 7).Draw(t, "seed["+id+"].rank"),
			arrival:        uint64(rapid.IntRange(1, 9).Draw(t, "seed["+id+"].arrival")),
		}
	})
}

// markerSnapshot builds a small quota snapshot used only as preserved
// unrelated metadata: machine operations must carry it through unchanged.
func markerSnapshot(id string, rank int) *quota.QuotaSnapshot {
	used := 0.25 + float64(rank)
	return &quota.QuotaSnapshot{
		MappingID: id,
		CheckedAt: baseTime,
		Windows: []quota.QuotaWindow{
			{Name: "5h", Used: &used},
		},
	}
}

// buildSeed constructs the production state and the independent model from
// the same generated facts, without calling any production helper.
func buildSeed(specs []seedProviderSpec, revision, onChangeExecutedRevision uint64) (State, modelState) {
	s := State{
		Schema:                   CurrentSchema,
		Revision:                 revision,
		Providers:                make(map[string]ProviderState, len(specs)),
		RefreshFailed:            []Diagnostic{{Code: "probe", Provider: providerPool[0], Summary: "synthetic marker", At: baseTime}},
		NextEventSequence:        41,
		NextArrivalSequence:      57,
		OnChangeExecutedRevision: onChangeExecutedRevision,
	}
	model := modelState{
		providers: make(map[string]modelProvider, len(specs)),
		meta: metaSnapshot{
			schema:                   CurrentSchema,
			revision:                 revision,
			onChangeExecutedRevision: onChangeExecutedRevision,
			nextEventSequence:        41,
			nextArrivalSequence:      57,
			refreshFailed:            []Diagnostic{{Code: "probe", Provider: providerPool[0], Summary: "synthetic marker", At: baseTime}},
		},
	}
	for _, sp := range specs {
		ps := ProviderState{
			Quota:               sp.quota,
			Availability:        sp.availability,
			ManualDisabled:      sp.manual,
			ManualDisabledAt:    sp.manualAt,
			QuotaAt:             sp.quotaAt,
			AvailabilityAt:      sp.availabilityAt,
			QuotaArrival:        sp.arrival,
			AvailabilityArrival: sp.arrival + 1,
			QuotaSnapshot:       markerSnapshot(sp.id, sp.rank),
			ResetCredits: quota.ResetCreditState{
				LatestAttempt: &quota.ResetCreditAttempt{Status: quota.CreditAttemptSkipped, At: baseTime},
			},
			Routing: ProviderRouting{
				LastRank:            sp.rank,
				LastDecisionAt:      baseTime,
				LastAppliedRevision: uint64(sp.rank + 2),
				Decision: &RoutingDecision{
					Rank:        sp.rank,
					Eligible:    true,
					OffPeak:     false,
					Explanation: "synthetic",
					EvaluatedAt: baseTime,
				},
			},
		}
		s.Providers[sp.id] = ps
		model.providers[sp.id] = modelProvider{
			quota:            sp.quota,
			availability:     sp.availability,
			manualDisabled:   sp.manual,
			manualDisabledAt: sp.manualAt,
			quotaAt:          sp.quotaAt,
			availabilityAt:   sp.availabilityAt,
			preserved:        deepCopyProviderState(ps),
		}
	}
	return s, model
}

// ---------------------------------------------------------------------------
// Generated properties

// TestPropertyStateHistoryReplay replays bounded operation histories against
// the production machine and the independent record model, comparing the
// result and error behavior after every step and proving no operation ever
// mutates the state it was handed.
func TestPropertyStateHistoryReplay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ids := rapid.SliceOfNDistinct(rapid.SampledFrom(providerPool), 0, len(providerPool), rapid.ID[string]).
			Draw(t, "tracked")
		revision := uint64(rapid.IntRange(1, 5).Draw(t, "revision"))
		onChangeRev := uint64(rapid.IntRange(0, 3).Draw(t, "onChangeRevision"))

		specs := make([]seedProviderSpec, 0, len(ids))
		for _, id := range ids {
			specs = append(specs, seedProviderSpecGen(id).Draw(t, "seed["+id+"]"))
		}
		s, model := buildSeed(specs, revision, onChangeRev)

		// Bounded histories: at most 64 operations per case, from empty to
		// long, with singleton and all-rejected sequences reachable.
		ops := rapid.SliceOfN(opGen, 0, 64).Draw(t, "ops")
		replayHistory(t, s, model, ops)
	})
}

// ---------------------------------------------------------------------------
// Exhaustive truth tables

var tableQuotaAxis = []Quota{"", QuotaNormal, QuotaLow, QuotaExhausted, Quota("corrupt-axis")}
var tableAvailabilityAxis = []Availability{"", Available, Unavailable, Availability("corrupt-axis")}

// TestEffectiveModeExhaustiveTable covers every combination of the sparse,
// recognized, and representative corrupt axis values plus both manual-disable
// states, against the model truth table, with literal anchors from the
// machine.go contract comment.
func TestEffectiveModeExhaustiveTable(t *testing.T) {
	for _, q := range tableQuotaAxis {
		for _, a := range tableAvailabilityAxis {
			for _, manual := range []bool{false, true} {
				ps := ProviderState{Quota: q, Availability: a, ManualDisabled: manual}
				want := modelMode(q, a, manual)
				if got := EffectiveMode(ps); got != want {
					t.Errorf("quota=%q availability=%q manual=%t: EffectiveMode=%s want %s", q, a, manual, got, want)
				}
			}
		}
	}

	anchors := []struct {
		quota        Quota
		availability Availability
		manual       bool
		want         Mode
	}{
		{QuotaNormal, Available, false, ModeNormal},
		{QuotaLow, Available, false, ModeReserve},
		{QuotaExhausted, Available, false, ModeDisabled},
		{QuotaNormal, Unavailable, false, ModeDisabled},
		{QuotaLow, Unavailable, false, ModeDisabled},
		{QuotaExhausted, Unavailable, false, ModeDisabled},
		{"", "", false, ModeNormal},                             // legacy sparse baseline is healthy
		{QuotaLow, "", false, ModeReserve},                      // sparse availability normalizes to available
		{"", Unavailable, false, ModeDisabled},                  // sparse quota normalizes to normal
		{Quota("corrupt-axis"), Available, false, ModeDisabled}, // corrupt quota fails closed
		{QuotaNormal, Availability("corrupt-axis"), false, ModeDisabled},
		{QuotaLow, Available, true, ModeDisabled}, // manual disable precedes everything
	}
	for _, tc := range anchors {
		ps := ProviderState{Quota: tc.quota, Availability: tc.availability, ManualDisabled: tc.manual}
		if got := EffectiveMode(ps); got != tc.want {
			t.Errorf("anchor quota=%q availability=%q manual=%t: EffectiveMode=%s want %s",
				tc.quota, tc.availability, tc.manual, got, tc.want)
		}
	}
}

// TestNormalizeAndValidityTables exhaustively checks sparse-axis
// normalization and the recognized-value predicates over the same domains.
func TestNormalizeAndValidityTables(t *testing.T) {
	for _, q := range tableQuotaAxis {
		wantQ := q
		if q == "" {
			wantQ = QuotaNormal
		}
		gotQ, _ := NormalizeProviderAxes(ProviderState{Quota: q})
		if gotQ != wantQ {
			t.Errorf("NormalizeProviderAxes quota=%q: got %q want %q", q, gotQ, wantQ)
		}
		wantValid := q == QuotaNormal || q == QuotaLow || q == QuotaExhausted
		if got := ValidQuotaAxis(q); got != wantValid {
			t.Errorf("ValidQuotaAxis(%q)=%t want %t", q, got, wantValid)
		}
	}
	for _, a := range tableAvailabilityAxis {
		wantA := a
		if a == "" {
			wantA = Available
		}
		_, gotA := NormalizeProviderAxes(ProviderState{Availability: a})
		if gotA != wantA {
			t.Errorf("NormalizeProviderAxes availability=%q: got %q want %q", a, gotA, wantA)
		}
		wantValid := a == Available || a == Unavailable
		if got := ValidAvailabilityAxis(a); got != wantValid {
			t.Errorf("ValidAvailabilityAxis(%q)=%t want %t", a, got, wantValid)
		}
	}
}

// ---------------------------------------------------------------------------
// Curated deterministic interaction and regression cases

// TestManualControlSeedsSparseAndUntracked verifies the documented seeding:
// manual controls give untracked providers healthy baseline axes, and seed a
// tracked provider's sparse availability axis while leaving its quota alone.
func TestManualControlSeedsSparseAndUntracked(t *testing.T) {
	at := baseTime.Add(time.Hour)
	s := State{
		Schema:    CurrentSchema,
		Revision:  3,
		Providers: map[string]ProviderState{"codex": {Quota: QuotaLow}},
	}
	next, err := SetManualDisabled(s, []string{"zai", "codex"}, true, at)
	if err != nil {
		t.Fatal(err)
	}
	if next.Providers["codex"].Quota != QuotaLow {
		t.Fatalf("codex quota changed: %+v", next.Providers["codex"])
	}
	if next.Providers["codex"].Availability != Available {
		t.Fatalf("codex sparse availability not seeded: %+v", next.Providers["codex"])
	}
	zai := next.Providers["zai"]
	if zai.Quota != QuotaNormal || zai.Availability != Available {
		t.Fatalf("untracked provider not seeded healthy: %+v", zai)
	}
	for _, id := range []string{"codex", "zai"} {
		if !next.Providers[id].ManualDisabled || !next.Providers[id].ManualDisabledAt.Equal(at) {
			t.Fatalf("%s manual control missing: %+v", id, next.Providers[id])
		}
	}
	if EffectiveMode(next.Providers["codex"]) != ModeDisabled {
		t.Fatalf("codex mode=%s want disabled", EffectiveMode(next.Providers["codex"]))
	}
	// State-level fields must survive untouched.
	if next.Schema != CurrentSchema || next.Revision != 3 {
		t.Fatalf("state metadata changed: %+v", next)
	}
}

// TestBatchWithEmptyIDIsFullyRejected proves a batch containing an empty
// provider ID is rejected whole: no entry is created or updated, and the
// input state is left byte-identical.
func TestBatchWithEmptyIDIsFullyRejected(t *testing.T) {
	at := baseTime.Add(time.Minute)
	s := State{
		Schema:    CurrentSchema,
		Revision:  9,
		Providers: map[string]ProviderState{"codex": {Quota: QuotaLow, Availability: Available, QuotaAt: at}},
	}
	pre := cloneProviders(s.Providers)
	next, err := SetManualDisabled(s, []string{"codex", ""}, true, at.Add(time.Minute))
	if err == nil {
		t.Fatal("batch with empty provider ID accepted")
	}
	compareProviders(t, "after rejected batch", next.Providers, map[string]modelProvider{
		"codex": {
			quota:        QuotaLow,
			availability: Available,
			quotaAt:      at,
			preserved:    deepCopyProviderState(ProviderState{Quota: QuotaLow, Availability: Available, QuotaAt: at}),
		},
	})
	if !reflect.DeepEqual(cloneProviders(s.Providers), pre) {
		t.Fatal("rejected batch mutated its input state")
	}
}

// TestClearAllPreservesManualControls verifies clear --all refreshes only the
// automatic axes (and their timestamps), carrying manual-disable state,
// arrivals, and unrelated metadata through unchanged.
func TestClearAllPreservesManualControls(t *testing.T) {
	at1 := baseTime
	at2 := baseTime.Add(2 * time.Hour)
	s := State{
		Schema: CurrentSchema,
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaExhausted, Availability: Unavailable, ManualDisabled: true,
				ManualDisabledAt: at1, QuotaArrival: 4, AvailabilityArrival: 5},
			"zai": {Quota: QuotaNormal, Availability: Available, ManualDisabled: false},
		},
	}
	next, err := ClearProvider(s, Selector{All: true}, at2)
	if err != nil {
		t.Fatal(err)
	}
	for id, ps := range next.Providers {
		if ps.Quota != QuotaNormal || ps.Availability != Available {
			t.Fatalf("%s not reset: %+v", id, ps)
		}
		if !ps.QuotaAt.Equal(at2) || !ps.AvailabilityAt.Equal(at2) {
			t.Fatalf("%s axis timestamps not refreshed: %+v", id, ps)
		}
	}
	codex := next.Providers["codex"]
	if !codex.ManualDisabled || !codex.ManualDisabledAt.Equal(at1) {
		t.Fatalf("clear changed manual controls: %+v", codex)
	}
	if codex.QuotaArrival != 4 || codex.AvailabilityArrival != 5 {
		t.Fatalf("clear changed arrivals: %+v", codex)
	}
	if got := EffectiveMode(codex); got != ModeDisabled {
		t.Fatalf("codex mode=%s want disabled (manual disable survives clear)", got)
	}
	if next.Providers["zai"].ManualDisabledAt.Equal(at2) {
		t.Fatalf("clear refreshed manual timestamp of never-disabled provider: %+v", next.Providers["zai"])
	}
}

// TestClearNamedUntrackedIsNoOp verifies clearing an untracked provider
// neither creates an entry nor touches anything else.
func TestClearNamedUntrackedIsNoOp(t *testing.T) {
	at := baseTime.Add(time.Minute)
	s := State{
		Schema:    CurrentSchema,
		Providers: map[string]ProviderState{"codex": {Quota: QuotaLow, QuotaAt: at}},
	}
	next, err := ClearProvider(s, Selector{Provider: "zai"}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Providers["zai"]; ok {
		t.Fatalf("clear created untracked entry: %+v", next.Providers["zai"])
	}
	compareProviders(t, "after no-op clear", next.Providers, map[string]modelProvider{
		"codex": {
			quota:     QuotaLow,
			quotaAt:   at,
			preserved: deepCopyProviderState(ProviderState{Quota: QuotaLow, QuotaAt: at}),
		},
	})
}

// TestResetTouchesOnlyClearedManualTimestamps verifies reset clears manual
// disables (updating only those timestamps), leaves never-disabled providers'
// manual timestamps alone, does not seed sparse axes, and preserves automatic
// observations and their timestamps.
func TestResetTouchesOnlyClearedManualTimestamps(t *testing.T) {
	at1 := baseTime
	at3 := baseTime.Add(3 * time.Hour)
	s := State{
		Schema: CurrentSchema,
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaLow, Availability: "", ManualDisabled: true, ManualDisabledAt: at1, QuotaAt: at1},
			"zai":   {Quota: QuotaExhausted, Availability: Unavailable},
		},
	}
	next, err := ResetManualDisables(s, at3)
	if err != nil {
		t.Fatal(err)
	}
	codex := next.Providers["codex"]
	if codex.ManualDisabled || !codex.ManualDisabledAt.Equal(at3) {
		t.Fatalf("codex manual state after reset: %+v", codex)
	}
	if codex.Quota != QuotaLow || codex.Availability != "" || !codex.QuotaAt.Equal(at1) {
		t.Fatalf("reset changed automatic observations: %+v", codex)
	}
	if got := EffectiveMode(codex); got != ModeReserve {
		t.Fatalf("codex mode=%s want reserve (low quota, sparse availability)", got)
	}
	zai := next.Providers["zai"]
	if zai.ManualDisabled || !zai.ManualDisabledAt.IsZero() {
		t.Fatalf("reset touched inactive manual record: %+v", zai)
	}
	if zai.Quota != QuotaExhausted || zai.Availability != Unavailable {
		t.Fatalf("reset changed automatic observations: %+v", zai)
	}
}

// TestSameTimestampRepeatIdempotentDifferentTimestampAdvances pins the
// timestamp contract for repeated authoritative sets: repeating the same
// patch at the same instant is byte-identical, while repeating it at a later
// instant advances only that axis's timestamp.
func TestSameTimestampRepeatIdempotentDifferentTimestampAdvances(t *testing.T) {
	t1 := baseTime
	t2 := baseTime.Add(time.Hour)
	low := QuotaLow

	s1, err := SetProvider(State{}, "codex", ProviderPatch{Quota: &low}, t1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := SetProvider(s1, "codex", ProviderPatch{Quota: &low}, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s1.Providers, s2.Providers) {
		t.Fatalf("same-timestamp repeat changed state: %+v vs %+v", s1.Providers, s2.Providers)
	}
	s3, err := SetProvider(s2, "codex", ProviderPatch{Quota: &low}, t2)
	if err != nil {
		t.Fatal(err)
	}
	if !s3.Providers["codex"].QuotaAt.Equal(t2) {
		t.Fatalf("later repeat did not advance quotaAt: %+v", s3.Providers["codex"])
	}
	// The later repeat must not have touched the availability axis.
	if !s3.Providers["codex"].AvailabilityAt.IsZero() {
		t.Fatalf("later repeat touched availability timestamp: %+v", s3.Providers["codex"])
	}
}

// TestRejectedOperationsThenContinuedUse walks a fixed sequence mixing
// accepted and rejected operations and proves every rejection leaves the
// state unchanged, after which later accepted operations behave exactly as
// if the rejected ones had never happened.
func TestRejectedOperationsThenContinuedUse(t *testing.T) {
	t1 := baseTime
	t2 := baseTime.Add(time.Minute)
	t3 := baseTime.Add(2 * time.Minute)
	low := QuotaLow
	unavailable := Unavailable

	s := State{Schema: CurrentSchema, Providers: map[string]ProviderState{
		"codex": {Quota: QuotaNormal, Availability: Available},
	}}
	model := modelState{
		providers: map[string]modelProvider{},
		meta:      metaSnapshot{schema: CurrentSchema},
	}
	s, err := SetProvider(s, "codex", ProviderPatch{Quota: &low}, t1)
	if err != nil {
		t.Fatal(err)
	}
	model.providers["codex"] = modelProvider{
		quota:        QuotaLow,
		availability: Available,
		quotaAt:      t1,
		preserved:    deepCopyProviderState(ProviderState{Quota: QuotaNormal, Availability: Available, QuotaAt: t1}),
	}

	rejected := []stateOp{
		{kind: opSet, provider: "codex", at: t2}, // empty patch
		{kind: opSet, provider: "codex", hasQuota: true, quotaVal: Quota("stale-garbage"), at: t2},
		{kind: opSet, provider: "", hasAvail: true, availVal: Unavailable, at: t2},
		{kind: opClear, provider: "", at: t2}, // neither provider nor --all
		{kind: opDisable, provider: "", at: t2},
		{kind: opBatch, batch: []string{"codex", ""}, batchDisabled: true, at: t2},
	}
	for i, op := range rejected {
		before := cloneProviders(s.Providers)
		next, err := applyProductionOp(s, op)
		if err == nil {
			t.Fatalf("op %d (%s) unexpectedly accepted", i, op)
		}
		compareAll(t, fmt.Sprintf("rejected op %d (%s)", i, op), next, model)
		if !reflect.DeepEqual(cloneProviders(s.Providers), before) {
			t.Fatalf("rejected op %d (%s) mutated its input state", i, op)
		}
	}

	s, err = SetProvider(s, "codex", ProviderPatch{Availability: &unavailable}, t3)
	if err != nil {
		t.Fatal(err)
	}
	model.providers["codex"] = modelProvider{
		quota:          QuotaLow,
		availability:   Unavailable,
		quotaAt:        t1,
		availabilityAt: t3,
		preserved:      deepCopyProviderState(ProviderState{Quota: QuotaNormal, Availability: Available, QuotaAt: t1}),
	}
	compareAll(t, "final", s, model)
	codex := s.Providers["codex"]
	if codex.Quota != QuotaLow || codex.Availability != Unavailable ||
		!codex.QuotaAt.Equal(t1) || !codex.AvailabilityAt.Equal(t3) {
		t.Fatalf("continued use after rejections diverged: %+v", codex)
	}
	if got := EffectiveMode(codex); got != ModeDisabled {
		t.Fatalf("codex mode=%s want disabled", got)
	}
}

// TestMixedHistoryDeepSnapshot replays a fixed longer history — duplicates,
// batch controls, clears, resets, and rejected operations interleaved — with
// per-step input-immutability checks and a final deep-snapshot recheck of the
// initial state.
func TestMixedHistoryDeepSnapshot(t *testing.T) {
	t1 := baseTime
	t2 := baseTime.Add(time.Minute)
	t3 := baseTime.Add(2 * time.Minute)
	exhausted := QuotaExhausted
	unavailable := Unavailable

	s := State{
		Schema:                   CurrentSchema,
		Revision:                 2,
		NextEventSequence:        41,
		NextArrivalSequence:      57,
		OnChangeExecutedRevision: 1,
		RefreshFailed:            []Diagnostic{{Code: "probe", Provider: "codex", Summary: "synthetic marker", At: baseTime}},
		Providers: map[string]ProviderState{
			"codex": {Quota: QuotaLow, Availability: Available, QuotaAt: t1, QuotaArrival: 2},
			"zai":   {Quota: "", Availability: Unavailable, ManualDisabled: true, ManualDisabledAt: t1},
		},
	}
	model := modelState{
		providers: map[string]modelProvider{
			"codex": {
				quota:        QuotaLow,
				availability: Available,
				quotaAt:      t1,
				preserved:    deepCopyProviderState(ProviderState{Quota: QuotaLow, Availability: Available, QuotaAt: t1, QuotaArrival: 2}),
			},
			"zai": {
				quota:            "",
				availability:     Unavailable,
				manualDisabled:   true,
				manualDisabledAt: t1,
				preserved:        deepCopyProviderState(ProviderState{Quota: "", Availability: Unavailable, ManualDisabled: true, ManualDisabledAt: t1}),
			},
		},
		meta: metaSnapshot{
			schema:                   CurrentSchema,
			revision:                 2,
			onChangeExecutedRevision: 1,
			nextEventSequence:        41,
			nextArrivalSequence:      57,
			refreshFailed:            []Diagnostic{{Code: "probe", Provider: "codex", Summary: "synthetic marker", At: baseTime}},
		},
	}

	ops := []stateOp{
		{kind: opSet, provider: "codex", hasQuota: true, quotaVal: exhausted, at: t2},
		{kind: opSet, provider: "zai", hasQuota: true, quotaVal: Quota("stale-garbage"), at: t2}, // rejected
		{kind: opBatch, batch: []string{"codex", "zai", "codex"}, batchDisabled: true, at: t2},   // duplicates
		{kind: opEnable, provider: "vertex", at: t2},                                             // creates untracked entry
		{kind: opClear, all: true, at: t3},
		{kind: opSet, provider: "zai", hasAvail: true, availVal: unavailable, at: t3},
		{kind: opReset, at: t3},
		{kind: opClear, provider: "", at: t3}, // rejected
	}
	replayHistory(t, s, model, ops)
}

// TestChangedHelpersMatchModel pins the change-detection helpers over a mixed
// state: they compare against the raw stored axis values, so sparse (empty)
// values count as different from any requested write, untracked providers
// compare against zero values, and equal requested values are unchanged.
func TestChangedHelpersMatchModel(t *testing.T) {
	low := QuotaLow
	normal := QuotaNormal
	available := Available
	s := State{Providers: map[string]ProviderState{
		"codex": {Quota: QuotaLow, Availability: ""},
		"zai":   {Quota: QuotaNormal, Availability: Available, ManualDisabled: true},
	}}

	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"set equal quota", SetChanged(s, "codex", ProviderPatch{Quota: &low}), false},
		{"set different quota", SetChanged(s, "codex", ProviderPatch{Quota: &normal}), true},
		{"set availability over sparse", SetChanged(s, "codex", ProviderPatch{Availability: &available}), true},
		{"empty patch", SetChanged(s, "codex", ProviderPatch{}), false},
		{"set on untracked provider", SetChanged(s, "vertex", ProviderPatch{Quota: &normal}), true},
		{"clear dirty provider", ClearChanged(s, Selector{Provider: "codex"}), true},
		{"clear clean provider", ClearChanged(s, Selector{Provider: "zai"}), false},
		{"clear all with dirty entry", ClearChanged(s, Selector{All: true}), true},
		{"clear untracked provider", ClearChanged(s, Selector{Provider: "vertex"}), false},
		{"disable already-disabled", ManualDisableChanged(s, []string{"zai"}, true), false},
		{"enable disabled", ManualDisableChanged(s, []string{"zai"}, false), true},
		{"disable untracked", ManualDisableChanged(s, []string{"vertex"}, true), true},
		{"disable clean", ManualDisableChanged(s, []string{"codex"}, true), true},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %t want %t", tc.name, tc.got, tc.want)
		}
	}
}
