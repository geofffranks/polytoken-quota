// Property suite for the routing policy (policy.go), built on the test-only
// pgregory.net/rapid dependency.
//
// Expected behavior comes from a deliberately simple independent model written
// from the documented contract (policy.go comments, README "Routing uses a
// deterministic lexicographic ranking" rules). Production code under test —
// ComputeSignal, CheckEligibility, EffectiveRemaining, Remaining, and the
// package comparators — is never called to compute an expectation; only domain
// types/constants are shared.
//
// Signals are not modeled numerically in general. Instead, generated providers
// draw from a fixed pool of *known signal classes*: small single-shape snapshot
// constructions whose ordering and cluster relations are provable by hand.
//
// The main construction is a 7-day window whose reset is exactly 4 days away,
// so the elapsed time rounds up to exactly 3 days:
//
//	leftFrac    = 4/7          elapsedFrac = 3/7
//	gap(r)      = (7/4)r - (7/3)(1-r)
//	with r = 4/7 + v:  gap = v * (7/4 + 7/3) = v * 49/12
//
// Class values (v, signal) and adjacent gaps after sorting descending:
//
//	farP +0.25 -> +1.0208   up +0.525   p +0.02 -> +0.0817
//	zero 0     ->  0        m -0.02 -> -0.0817      farM -0.25 -> -1.0208
//
// Sorted adjacent differences are 0.496, 0.443, 0.082, 0.082, 0.939 — every
// value is either far above or far below the 0.20 tie band, so float64 noise
// (≈1e-16) can never flip a cluster decision. Equal-valued classes (two
// providers of the same class, or dbl vs p which are identical windows) compute
// bit-identical signals in production, so they always share a cluster.
//
// Structural and metamorphic checks complement the model: observation-order
// invariance (observations are unique by construction) and policy permutations
// restricted to within-group runs, which preserve each group's first-appearance
// position. Arbitrary group reordering, duplicate observations, or adding a
// missing-signal provider are NOT treated as invariances.
//
// Bounds per generated case: at most 8 provider policies (plus 2 observation
// slots that no policy references, proving unknown observations are ignored),
// a fixed two-element UTC "now" pool, and a fixed identity pool "a".."j" whose
// lexical order equals its index order.

package routing

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Known signal classes

type windowClass int

const (
	wFarP    windowClass = iota // single 7d window, v=+0.25
	wUp                         // single 7d window via usage_percent 30
	wP                          // single 7d window, v=+0.02
	wZero                       // single 7d window, v=0 (exactly on pace)
	wM                          // single 7d window, v=-0.02
	wFarM                       // single 7d window, v=-0.25
	wDbl                        // two identical wP windows (period-weighted mean of equals)
	wNone                       // only a 5h rate-limit window: eligible, no signal
	wZeroRem                    // used == limit: remaining 0 -> ineligible
	wSparse                     // window with period/reset but no usage fields
	wEmpty                      // no windows at all
)

var windowClassName = map[windowClass]string{
	wFarP: "farP", wUp: "up", wP: "p", wZero: "zero", wM: "m", wFarM: "farM",
	wDbl: "dbl", wNone: "none", wZeroRem: "zeroRem", wSparse: "sparse", wEmpty: "empty",
}

func (w windowClass) String() string { return windowClassName[w] }

// sigSlope is the closed-form gap slope for the day-3-of-7 construction: a
// remaining surplus of v over the on-pace fraction yields signal v*sigSlope.
const sigSlope = 49.0 / 12.0

// modelSignal returns the known signal value for a class and whether the
// production policy can compute one at all (ok=false -> no qualifying window).
// Only classes with a qualifying >=24h window return true.
func (w windowClass) modelSignal() (float64, bool) {
	switch w {
	case wFarP:
		return sigSlope * 0.25, true
	case wUp:
		return 0.525, true // (7/4)(0.7) - (7/3)(0.3), r = 0.7 from usage_percent 30
	case wP, wDbl:
		return sigSlope * 0.02, true
	case wZero:
		return 0, true
	case wM:
		return -sigSlope * 0.02, true
	case wFarM:
		return -sigSlope * 0.25, true
	default:
		return 0, false
	}
}

// modelRemaining is the known remaining fraction of the class's window(s),
// computed from construction facts (not from production Remaining helpers).
// ok=false means the window reports no usable remaining value at all.
func (w windowClass) modelRemaining() (float64, bool) {
	switch w {
	case wFarP:
		return 4.0/7.0 + 0.25, true
	case wUp:
		return 0.7, true
	case wP, wDbl:
		return 4.0/7.0 + 0.02, true
	case wZero:
		return 4.0 / 7.0, true
	case wM:
		return 4.0/7.0 - 0.02, true
	case wFarM:
		return 4.0/7.0 - 0.25, true
	case wNone:
		return 0.5, true
	case wZeroRem:
		return 0, true
	default:
		return 0, false
	}
}

const (
	sigWindowPeriod = 7 * 24 * time.Hour
	sigResetIn      = 4 * 24 * time.Hour // reset 4 days out -> elapsed exactly 3 days
)

// sigWindow builds the known-construction window: used fraction 3/7 - v so the
// remaining fraction is 4/7 + v (used/limit is the remaining source).
func sigWindow(now time.Time, v float64) quota.QuotaWindow {
	return quota.QuotaWindow{
		Used:    fptr(3.0/7.0 - v),
		Limit:   fptr(1),
		ResetAt: tptr(now.Add(sigResetIn)),
		Period:  durptr(sigWindowPeriod),
	}
}

// buildWindows materializes a class as quota windows for the fixture snapshot.
func buildWindows(now time.Time, w windowClass) []quota.QuotaWindow {
	switch w {
	case wFarP:
		return []quota.QuotaWindow{sigWindow(now, 0.25)}
	case wUp:
		return []quota.QuotaWindow{{
			UsagePercent: fptr(30),
			ResetAt:      tptr(now.Add(sigResetIn)),
			Period:       durptr(sigWindowPeriod),
		}}
	case wP:
		return []quota.QuotaWindow{sigWindow(now, 0.02)}
	case wZero:
		return []quota.QuotaWindow{sigWindow(now, 0)}
	case wM:
		return []quota.QuotaWindow{sigWindow(now, -0.02)}
	case wFarM:
		return []quota.QuotaWindow{sigWindow(now, -0.25)}
	case wDbl:
		return []quota.QuotaWindow{sigWindow(now, 0.02), sigWindow(now, 0.02)}
	case wNone:
		// A 5-hour session window: usable remaining, but below the one-day
		// quota-cycle floor, so it never yields a signal.
		return []quota.QuotaWindow{{
			Used: fptr(0.5), Limit: fptr(1),
			ResetAt: tptr(now.Add(2 * time.Hour)), Period: durptr(5 * time.Hour),
		}}
	case wZeroRem:
		// Exact exhausted window: used == limit, so the remaining fraction is
		// exactly 0 (built directly rather than via sigWindow arithmetic,
		// whose float sum would leave a ~1e-16 remainder).
		return []quota.QuotaWindow{{
			Used: fptr(1), Limit: fptr(1),
			ResetAt: tptr(now.Add(sigResetIn)), Period: durptr(sigWindowPeriod),
		}}
	case wSparse:
		return []quota.QuotaWindow{{ResetAt: tptr(now.Add(sigResetIn)), Period: durptr(sigWindowPeriod)}}
	case wEmpty:
		return nil
	default:
		panic(fmt.Sprintf("unknown window class %d", int(w)))
	}
}

// ---------------------------------------------------------------------------
// Generated case

// propIDPool is the identity pool; lexical order equals index order, which the
// ineligible-tail and display-order checks rely on.
var propIDPool = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

// propNowPool holds two UTC instants: at noon the 00:00–12:00 UTC off-peak
// schedule reads peak (minute 720 is at/after the exclusive end), at 06:00 it
// reads off-peak. Both are Monday 2026-06-15; the generated schedules cover
// all days, so the weekday never matters here (boundary fixtures are curated
// separately).
var propNowPool = []time.Time{
	time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	time.Date(2026, 6, 15, 6, 0, 0, 0, time.UTC),
}

var (
	propGroupChoices  = []string{"", "", "gA", "gB"}
	propModeChoices   = []string{"normal", "normal", "reserve", "disabled", "maintenance", ""}
	propWeightChoices = []int{0, 1, 1, 3, 7, -2}
	propTTLChoices    = []time.Duration{
		0, 0, 30 * time.Minute, 45 * time.Minute, time.Hour, -5 * time.Minute,
	}
	propAgeChoices = []time.Duration{
		0, 10 * time.Minute, 30 * time.Minute, 30*time.Minute + time.Nanosecond,
		45 * time.Minute, 90 * time.Minute,
	}
	propStatusChoices = []quota.SourceStatus{
		quota.SourceFresh, quota.SourceFresh, quota.SourcePartial, quota.SourceFailed,
	}
	propAvailChoices = []quota.QuotaAvailability{
		quota.QuotaAvailable, quota.QuotaAvailable, quota.QuotaAvailable,
		quota.QuotaUnavailable, quota.QuotaUnknown, quota.QuotaAvailability("corrupt"),
	}
	propWindowChoices = []windowClass{
		wP, wP, wZero, wFarP, wM, wFarM, wUp, wDbl, wNone, wZeroRem, wSparse, wEmpty,
	}
	// 0 = nil schedule (never off-peak), 1 = 00:00–12:00 UTC every day.
	propSchedChoices = []int{0, 0, 1}
)

type slotCase struct {
	policy  bool
	obs     bool
	snapNil bool // obs present but carries a nil snapshot (never observed)
	group   string
	mode    string
	weight  int
	ttl     time.Duration
	age     time.Duration
	status  quota.SourceStatus
	avail   quota.QuotaAvailability
	windows windowClass
	sched   int
}

type genCase struct {
	now   time.Time
	slots [10]slotCase
}

func drawGenCase(t *rapid.T) genCase {
	var c genCase
	c.now = rapid.SampledFrom(propNowPool).Draw(t, "now")
	for i := range c.slots {
		s := &c.slots[i]
		pol := fmt.Sprintf("slot%d.", i)
		s.policy = i < 8 && rapid.Bool().Draw(t, pol+"policy")
		s.obs = rapid.Bool().Draw(t, pol+"obs")
		s.snapNil = rapid.Bool().Draw(t, pol+"snapNil")
		s.group = rapid.SampledFrom(propGroupChoices).Draw(t, pol+"group")
		s.mode = rapid.SampledFrom(propModeChoices).Draw(t, pol+"mode")
		s.weight = rapid.SampledFrom(propWeightChoices).Draw(t, pol+"weight")
		s.ttl = rapid.SampledFrom(propTTLChoices).Draw(t, pol+"ttl")
		s.age = rapid.SampledFrom(propAgeChoices).Draw(t, pol+"age")
		s.status = rapid.SampledFrom(propStatusChoices).Draw(t, pol+"status")
		s.avail = rapid.SampledFrom(propAvailChoices).Draw(t, pol+"avail")
		s.windows = rapid.SampledFrom(propWindowChoices).Draw(t, pol+"windows")
		s.sched = rapid.SampledFrom(propSchedChoices).Draw(t, pol+"sched")
	}
	return c
}

// parsedMorningSchedule is the one generated schedule shape: off-peak every
// day outside 00:00–12:00 UTC.
var parsedMorningSchedule = func() *Schedule {
	s, err := ParseSchedule("UTC", []OffPeakWindow{{Days: allDays, Start: "00:00", End: "12:00"}})
	if err != nil {
		panic(err)
	}
	return &s
}()

func (s slotCase) schedule() *Schedule {
	if s.sched == 1 {
		return parsedMorningSchedule
	}
	return nil
}

// schedOffpeak is the model's off-peak truth for a generated schedule class at
// now: the 00:00–12:00 UTC window is the off-peak window (inside it, end
// exclusive), matching the routing package's IsOffPeak contract.
func schedOffpeak(sched int, now time.Time) bool {
	if sched != 1 {
		return false
	}
	return now.Hour()*60+now.Minute() < 720
}

func (c genCase) build() ([]ProviderPolicy, []ProviderObs) {
	var policies []ProviderPolicy
	var obs []ProviderObs
	for i := range c.slots {
		id := propIDPool[i]
		s := c.slots[i]
		if s.policy {
			policies = append(policies, ProviderPolicy{
				MappingID:    id,
				BalanceGroup: s.group,
				Schedule:     s.schedule(),
				FreshnessTTL: s.ttl,
				Weight:       s.weight,
			})
		}
		if s.obs {
			o := ProviderObs{MappingID: id, Mode: s.mode}
			if !s.snapNil {
				o.Snapshot = &quota.QuotaSnapshot{
					MappingID:    id,
					CheckedAt:    c.now.Add(-s.age),
					Windows:      buildWindows(c.now, s.windows),
					Availability: s.avail,
					Status:       s.status,
				}
			}
			obs = append(obs, o)
		}
	}
	return policies, obs
}

// dump renders a readable witness for failure messages.
func (c genCase) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "now=%s\n", c.now.Format(time.RFC3339))
	for i := range c.slots {
		s := c.slots[i]
		if !s.policy && !s.obs {
			continue
		}
		fmt.Fprintf(&b,
			"  id=%s policy=%t obs=%t snapNil=%t group=%q mode=%q weight=%d ttl=%s age=%s status=%s avail=%q windows=%s sched=%d\n",
			propIDPool[i], s.policy, s.obs, s.snapNil, s.group, s.mode, s.weight,
			s.ttl, s.age, s.status, string(s.avail), s.windows, s.sched)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Independent model

type modelRec struct {
	id      string
	group   string // effective balance group ("" resolved to "default")
	weight  int    // effective (0 resolved to default 1)
	ttl     time.Duration
	age     time.Duration
	mode    string
	status  quota.SourceStatus
	avail   quota.QuotaAvailability
	windows windowClass
	sched   int
	obs     bool
	snapNil bool

	offpeak  bool
	eligible bool
	signal   *float64 // known class value; nil when not computable
	cluster  int
}

// effectiveTTL mirrors the documented freshness default: non-positive TTL
// selects the 30-minute default.
func effectiveTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 30 * time.Minute
	}
	return ttl
}

func (r *modelRec) evaluate(now time.Time) {
	r.offpeak = r.obs && schedOffpeak(r.sched, now)
	// Eligibility truth table (fail closed), from the documented contract:
	// mode must be normal/reserve; a missing observation or nil snapshot,
	// an age strictly beyond the effective TTL, a failed or partial source,
	// any availability other than exactly "available", and missing or
	// non-positive remaining all make the provider ineligible.
	switch r.mode {
	case "normal", "reserve":
	default:
		r.eligible = false
		return
	}
	if !r.obs || r.snapNil {
		r.eligible = false
		return
	}
	if r.age > effectiveTTL(r.ttl) { // snapshot age = now - CheckedAt
		r.eligible = false
		return
	}
	if r.status != quota.SourceFresh {
		r.eligible = false
		return
	}
	if r.avail != quota.QuotaAvailable {
		r.eligible = false
		return
	}
	rem, ok := r.windows.modelRemaining()
	if !ok || rem <= 0 {
		r.eligible = false
		return
	}
	r.eligible = true
	if sig, ok := r.windows.modelSignal(); ok {
		s := sig
		r.signal = &s
	}
	r.cluster = -1
}

// expEntry is the model's expected RankEntry, plus which explanation classes
// the production entry must satisfy.
type expEntry struct {
	id          string
	rank        int
	offpeak     bool
	eligible    bool
	signalShown bool // eligible explanation must reference the signal
}

// modelExpected computes the full expected ranking from the generated case.
func modelExpected(c genCase) []expEntry {
	var recs []*modelRec
	for i := range c.slots {
		s := c.slots[i]
		if !s.policy {
			continue
		}
		g := s.group
		if g == "" {
			g = "default"
		}
		w := s.weight
		if w == 0 {
			w = 1
		}
		rec := &modelRec{
			id: propIDPool[i], group: g, weight: w,
			ttl: s.ttl, age: s.age, mode: s.mode, status: s.status,
			avail: s.avail, windows: s.windows, sched: s.sched,
			obs: s.obs, snapNil: s.snapNil,
		}
		rec.evaluate(c.now)
		recs = append(recs, rec)
	}

	// Group eligible records by balance group in first-appearance order.
	var groupOrder []string
	groups := map[string][]*modelRec{}
	var ineligible []*modelRec
	for _, r := range recs {
		if !r.eligible {
			ineligible = append(ineligible, r)
			continue
		}
		if _, seen := groups[r.group]; !seen {
			groupOrder = append(groupOrder, r.group)
		}
		groups[r.group] = append(groups[r.group], r)
	}

	// Cluster assignment within each group: if any member lacks a computable
	// signal the signal is skipped group-wide; otherwise clusters split where
	// adjacent sorted signals differ by at least the 0.20 tie band.
	for _, g := range groupOrder {
		items := groups[g]
		suppressed := false
		for _, it := range items {
			if it.signal == nil {
				suppressed = true
				break
			}
		}
		if suppressed {
			for _, it := range items {
				it.signal = nil
				it.cluster = -1
			}
		} else {
			sorted := append([]*modelRec(nil), items...)
			sort.SliceStable(sorted, func(i, j int) bool {
				return *sorted[i].signal > *sorted[j].signal
			})
			idx := 0
			for i, it := range sorted {
				if i > 0 && *sorted[i-1].signal-*it.signal >= signalTieBand {
					idx++
				}
				it.cluster = idx
			}
		}
		// Within-group order: cluster, then off-peak, then weight, then ID.
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.signal != nil && b.signal != nil && a.cluster != b.cluster {
				return a.cluster < b.cluster
			}
			if a.offpeak != b.offpeak {
				return a.offpeak
			}
			if a.weight != b.weight {
				return a.weight > b.weight
			}
			return a.id < b.id
		})
	}

	sort.SliceStable(ineligible, func(i, j int) bool {
		return ineligible[i].id < ineligible[j].id
	})

	var out []expEntry
	nextRank := 0
	for _, g := range groupOrder {
		rank := nextRank
		var prev *modelRec
		for _, it := range groups[g] {
			if prev != nil {
				// Semantic rank equality deliberately ignores ID: same
				// cluster (or no signal group-wide), off-peak, and weight
				// share a rank.
				sameCluster := (prev.signal == nil && it.signal == nil) ||
					(prev.signal != nil && it.signal != nil && prev.cluster == it.cluster)
				if !(sameCluster && prev.offpeak == it.offpeak && prev.weight == it.weight) {
					rank++
				}
			}
			out = append(out, expEntry{
				id: it.id, rank: rank, offpeak: it.offpeak, eligible: true,
				signalShown: it.signal != nil,
			})
			prev = it
		}
		nextRank = rank + 1
	}
	for _, it := range ineligible {
		out = append(out, expEntry{id: it.id, rank: nextRank, offpeak: it.offpeak})
		nextRank++
	}
	return out
}

// ---------------------------------------------------------------------------
// Checks

// propTB is the minimal failure interface shared by *testing.T and *rapid.T.
type propTB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// checkAgainstModel compares a production ranking entry-by-entry against the
// model expectation, including explanation classes: ineligible explanations
// carry the "ineligible: " prefix with a reason; eligible explanations name
// peak/off-peak consistently and reference the signal exactly when the group's
// signal comparison was not suppressed.
func checkAgainstModel(tb propTB, stage, witness string, res RankingResult, exp []expEntry) {
	tb.Helper()
	if len(res.Entries) != len(exp) {
		tb.Fatalf("%s: entry count %d, model expects %d\n%s", stage, len(res.Entries), len(exp), witness)
	}
	for i, want := range exp {
		got := res.Entries[i]
		if got.MappingID != want.id {
			tb.Fatalf("%s: position %d id %q, model expects %q\n%s", stage, i, got.MappingID, want.id, witness)
		}
		if got.Rank != want.rank {
			tb.Fatalf("%s: %s rank %d, model expects %d\n%s", stage, got.MappingID, got.Rank, want.rank, witness)
		}
		if got.OffPeak != want.offpeak {
			tb.Fatalf("%s: %s offPeak %t, model expects %t\n%s", stage, got.MappingID, got.OffPeak, want.offpeak, witness)
		}
		if got.Eligible != want.eligible {
			tb.Fatalf("%s: %s eligible %t, model expects %t\n%s", stage, got.MappingID, got.Eligible, want.eligible, witness)
		}
		if !want.eligible {
			if !strings.HasPrefix(got.Explanation, "ineligible: ") ||
				strings.TrimSpace(strings.TrimPrefix(got.Explanation, "ineligible: ")) == "" {
				tb.Fatalf("%s: %s explanation %q lacks an ineligible reason\n%s", stage, got.MappingID, got.Explanation, witness)
			}
			continue
		}
		e := got.Explanation
		if e == "" {
			tb.Fatalf("%s: %s empty eligible explanation\n%s", stage, got.MappingID, witness)
		}
		if want.offpeak {
			if !strings.Contains(e, "off-peak") {
				tb.Fatalf("%s: %s explanation %q should say off-peak\n%s", stage, got.MappingID, e, witness)
			}
		} else if !strings.Contains(e, "peak") || strings.Contains(e, "off-peak") {
			tb.Fatalf("%s: %s explanation %q should say peak\n%s", stage, got.MappingID, e, witness)
		}
		if want.signalShown != strings.Contains(e, "signal") {
			tb.Fatalf("%s: %s explanation %q signal mention=%t, model expects %t\n%s",
				stage, got.MappingID, e, strings.Contains(e, "signal"), want.signalShown, witness)
		}
	}
}

// checkStructuralInvariants asserts properties that must hold for every input,
// independent of the model: each policy represented exactly once, monotone
// ranks, all eligible entries before all ineligible ones, ineligible entries
// sorted by ID, eligible groups contiguous in first-appearance order, and
// equal-rank runs within a group displayed in ID order.
func checkStructuralInvariants(tb propTB, stage string, in RankingInput, res RankingResult) {
	tb.Helper()
	entries := res.Entries

	counts := map[string]int{}
	for _, p := range in.Policies {
		counts[p.MappingID]++
	}
	gotCounts := map[string]int{}
	for _, e := range entries {
		gotCounts[e.MappingID]++
	}
	if !reflect.DeepEqual(counts, gotCounts) {
		tb.Fatalf("%s: result identities %v do not match input policies %v", stage, gotCounts, counts)
	}

	policyByID := map[string]ProviderPolicy{}
	for _, p := range in.Policies {
		policyByID[p.MappingID] = p
	}

	for i := 1; i < len(entries); i++ {
		if entries[i-1].Rank > entries[i].Rank {
			tb.Fatalf("%s: rank %d precedes %d at positions %d,%d", stage,
				entries[i-1].Rank, entries[i].Rank, i-1, i)
		}
	}
	firstIneligible := len(entries)
	for i, e := range entries {
		if !e.Eligible {
			firstIneligible = i
			break
		}
	}
	for i := firstIneligible; i < len(entries); i++ {
		if entries[i].Eligible {
			tb.Fatalf("%s: eligible %s appears after ineligible entries", stage, entries[i].MappingID)
		}
	}
	for i := firstIneligible + 1; i < len(entries); i++ {
		if entries[i].MappingID <= entries[i-1].MappingID {
			tb.Fatalf("%s: ineligible entries not strictly ID-sorted at %d: %q then %q",
				stage, i, entries[i-1].MappingID, entries[i].MappingID)
		}
	}

	// Group runs over eligible entries: contiguous, in the groups'
	// first-appearance order over the input policies (restricted to policies
	// that turned out eligible), with equal-rank runs displayed in ID order.
	var runSequence []string
	lastIndex := map[string]int{}
	for i := 0; i < firstIneligible; i++ {
		g := policyByID[entries[i].MappingID].balanceGroup()
		if i == 0 || policyByID[entries[i-1].MappingID].balanceGroup() != g {
			runSequence = append(runSequence, g)
		}
		if prev, ok := lastIndex[g]; ok && prev != i-1 {
			tb.Fatalf("%s: group %q entries not contiguous at position %d", stage, g, i)
		}
		lastIndex[g] = i
	}
	var wantRuns []string
	eligible := eligibleIDsFrom(entries)
	for _, p := range in.Policies {
		if !eligible[p.MappingID] {
			continue
		}
		g := p.balanceGroup()
		if len(wantRuns) == 0 || wantRuns[len(wantRuns)-1] != g {
			wantRuns = append(wantRuns, g)
		}
	}
	if len(wantRuns) != len(runSequence) {
		tb.Fatalf("%s: group run sequence %v, first-appearance order %v", stage, runSequence, wantRuns)
	}
	for i := range wantRuns {
		if runSequence[i] != wantRuns[i] {
			tb.Fatalf("%s: group run sequence %v, first-appearance order %v", stage, runSequence, wantRuns)
		}
	}

	// Equal-rank runs within a group are displayed in ascending ID order
	// (ID stabilizes presentation only).
	for i := 1; i < firstIneligible; i++ {
		a, b := entries[i-1], entries[i]
		if a.Rank == b.Rank && b.MappingID <= a.MappingID {
			tb.Fatalf("%s: equal-rank run not ID-ordered: %q then %q", stage, a.MappingID, b.MappingID)
		}
	}
}

func eligibleIDsFrom(entries []RankEntry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		if e.Eligible {
			m[e.MappingID] = true
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Deep input snapshots (mutation detection)

func clonePolicy(p ProviderPolicy) ProviderPolicy {
	if p.Schedule != nil {
		sched := *p.Schedule
		sched.OffPeak = make([]OffPeakWindow, len(p.Schedule.OffPeak))
		for i, w := range p.Schedule.OffPeak {
			w.Days = append([]DayOfWeek(nil), w.Days...)
			sched.OffPeak[i] = w
		}
		p.Schedule = &sched
	}
	return p
}

func cloneSnapshot(s *quota.QuotaSnapshot) *quota.QuotaSnapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.Windows = make([]quota.QuotaWindow, len(s.Windows))
	for i, w := range s.Windows {
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
		out.Windows[i] = w
	}
	return &out
}

func cloneRankingInput(policies []ProviderPolicy, obs []ProviderObs) ([]ProviderPolicy, []ProviderObs) {
	cp := make([]ProviderPolicy, len(policies))
	for i, p := range policies {
		cp[i] = clonePolicy(p)
	}
	co := make([]ProviderObs, len(obs))
	for i, o := range obs {
		o.Snapshot = cloneSnapshot(o.Snapshot)
		co[i] = o
	}
	return cp, co
}

// ---------------------------------------------------------------------------
// Permutation helpers

// fisherYatesPerm draws a uniformly random permutation of [0,n); every drawn
// value is a valid shuffle step, so shrinking stays precondition-safe.
func fisherYatesPerm(t *rapid.T, n int, name string) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, name)
		perm[i], perm[j] = perm[j], perm[i]
	}
	return perm
}

// groupRuns returns the maximal contiguous index runs of equal balance group.
func groupRuns(policies []ProviderPolicy) [][]int {
	var runs [][]int
	for i, p := range policies {
		if len(runs) > 0 {
			last := runs[len(runs)-1]
			if policies[last[0]].balanceGroup() == p.balanceGroup() {
				runs[len(runs)-1] = append(last, i)
				continue
			}
		}
		runs = append(runs, []int{i})
	}
	return runs
}

// permuteWithinGroupRuns shuffles policies inside each same-group run, which
// preserves the group first-appearance sequence by construction.
func permuteWithinGroupRuns(t *rapid.T, policies []ProviderPolicy) []ProviderPolicy {
	out := append([]ProviderPolicy(nil), policies...)
	for _, run := range groupRuns(policies) {
		if len(run) < 2 {
			continue
		}
		perm := fisherYatesPerm(t, len(run), "groupRunPerm")
		vals := make([]ProviderPolicy, len(run))
		for k, idx := range run {
			vals[k] = policies[idx]
		}
		for k, idx := range run {
			out[idx] = vals[perm[k]]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Generated property

func TestRankPropertyGenerated(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gc := drawGenCase(t)
		policies, obs := gc.build()
		in := RankingInput{Now: gc.now, Policies: policies, Obs: obs}
		witness := gc.dump()

		prePolicies, preObs := cloneRankingInput(policies, obs)

		res := Rank(in)
		checkAgainstModel(t, "generated model comparison", witness, res, modelExpected(gc))
		checkStructuralInvariants(t, "structural invariants", in, res)

		// Determinism: a repeated call returns an identical result.
		if again := Rank(in); !reflect.DeepEqual(res, again) {
			t.Fatalf("Rank is not deterministic across repeated calls\n%s", witness)
		}
		// Neither call mutated its input.
		if postPolicies, postObs := cloneRankingInput(policies, obs); !reflect.DeepEqual(prePolicies, postPolicies) || !reflect.DeepEqual(preObs, postObs) {
			t.Fatalf("Rank mutated its input policies or observations\n%s", witness)
		}

		// Observation-order invariance: observations are unique by
		// construction, so shuffling their slice order must not matter.
		perm := fisherYatesPerm(t, len(obs), "obsPerm")
		obsShuffled := make([]ProviderObs, len(obs))
		for i, j := range perm {
			obsShuffled[i] = obs[j]
		}
		if res2 := Rank(RankingInput{Now: gc.now, Policies: policies, Obs: obsShuffled}); !reflect.DeepEqual(res, res2) {
			t.Fatalf("observation order changed the ranking\n%s", witness)
		}

		// Policy permutation restricted to within-group runs preserves every
		// group's first-appearance position, so the result must be identical.
		polPermuted := permuteWithinGroupRuns(t, policies)
		if res3 := Rank(RankingInput{Now: gc.now, Policies: polPermuted, Obs: obs}); !reflect.DeepEqual(res, res3) {
			t.Fatalf("within-group policy permutation changed the ranking\n%s", witness)
		}
	})
}

// ---------------------------------------------------------------------------
// Curated deterministic boundary cases
//
// These pin exact boundary behavior by hand — independently of the generated
// model — so the model and these cases cross-check each other.

// curPolicy builds a ProviderPolicy with the given fields (weight 0 and TTL 0
// exercise the documented defaults).
func curPolicy(id, group string, weight int, sched *Schedule, ttl time.Duration) ProviderPolicy {
	return ProviderPolicy{
		MappingID: id, BalanceGroup: group, Schedule: sched,
		FreshnessTTL: ttl, Weight: weight,
	}
}

// curHealthyObs builds a fresh, available, normal-mode observation whose
// snapshot carries the given known signal class.
func curHealthyObs(id, mode string, now time.Time, age time.Duration, w windowClass) ProviderObs {
	return ProviderObs{
		MappingID: id,
		Mode:      mode,
		Snapshot: &quota.QuotaSnapshot{
			MappingID:    id,
			CheckedAt:    now.Add(-age),
			Windows:      buildWindows(now, w),
			Availability: quota.QuotaAvailable,
			Status:       quota.SourceFresh,
		},
	}
}

func curRank(t *testing.T, stage string, in RankingInput) RankingResult {
	t.Helper()
	return Rank(in)
}

// curWant is one hand-computed expected entry.
type curWant struct {
	id       string
	rank     int
	offpeak  bool
	eligible bool
}

func curCheck(t *testing.T, stage string, res RankingResult, want []curWant) {
	t.Helper()
	if len(res.Entries) != len(want) {
		t.Fatalf("%s: got %d entries %v, want %d", stage, len(res.Entries), res.Entries, len(want))
	}
	for i, w := range want {
		got := res.Entries[i]
		if got.MappingID != w.id || got.Rank != w.rank || got.OffPeak != w.offpeak || got.Eligible != w.eligible {
			t.Fatalf("%s: position %d got {id=%s rank=%d offPeak=%t eligible=%t}, want {id=%s rank=%d offPeak=%t eligible=%t}",
				stage, i, got.MappingID, got.Rank, got.OffPeak, got.Eligible,
				w.id, w.rank, w.offpeak, w.eligible)
		}
	}
}

func TestRankPropertyCuratedBoundaries(t *testing.T) {
	now := rankNow // 2026-06-15 12:00 UTC

	t.Run("freshness TTL boundary", func(t *testing.T) {
		// Strictly-beyond-TTL staleness: age == TTL is fresh, one nanosecond
		// more is stale. TTL 0 selects the 30-minute default, and a negative
		// TTL selects the same default.
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("a", "", 0, nil, 0),              // age 30m: exactly at TTL -> fresh
			curPolicy("b", "", 0, nil, 0),              // age 30m+1ns: stale
			curPolicy("c", "", 0, nil, 0),              // age 29m: fresh
			curPolicy("d", "", 0, nil, -5*time.Minute), // negative TTL -> 30m default; age 31m -> stale
			curPolicy("e", "", 0, nil, time.Hour),      // explicit 1h TTL; age 45m -> fresh
		}}
		obs := []ProviderObs{
			curHealthyObs("a", "normal", now, 30*time.Minute, wZero),
			curHealthyObs("b", "normal", now, 30*time.Minute+time.Nanosecond, wZero),
			curHealthyObs("c", "normal", now, 29*time.Minute, wZero),
			curHealthyObs("d", "normal", now, 31*time.Minute, wZero),
			curHealthyObs("e", "normal", now, 45*time.Minute, wZero),
		}
		res := curRank(t, "ttl", RankingInput{Now: now, Policies: in.Policies, Obs: obs})
		// Eligible a,c,e share one cluster/rank; ineligible b,d trail by ID.
		curCheck(t, "ttl", res, []curWant{
			{"a", 0, false, true}, {"c", 0, false, true}, {"e", 0, false, true},
			{"b", 1, false, false}, {"d", 2, false, false},
		})
	})

	t.Run("mode eligibility", func(t *testing.T) {
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("n", "", 0, nil, 0), curPolicy("r", "", 0, nil, 0),
			curPolicy("d", "", 0, nil, 0), curPolicy("maint", "", 0, nil, 0),
			curPolicy("blank", "", 0, nil, 0), curPolicy("zmiss", "", 0, nil, 0),
		}}
		obs := []ProviderObs{
			curHealthyObs("n", "normal", now, 0, wZero),
			curHealthyObs("r", "reserve", now, 0, wZero),
			curHealthyObs("d", "disabled", now, 0, wZero),
			curHealthyObs("maint", "maintenance", now, 0, wZero),
			curHealthyObs("blank", "", now, 0, wZero),
			// zmiss has no observation at all.
		}
		in.Obs = obs
		res := curRank(t, "modes", in)
		curCheck(t, "modes", res, []curWant{
			{"n", 0, false, true}, {"r", 0, false, true},
			{"blank", 1, false, false}, {"d", 2, false, false},
			{"maint", 3, false, false}, {"zmiss", 4, false, false},
		})
	})

	t.Run("degraded and sparse evidence", func(t *testing.T) {
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("ok", "", 0, nil, 0),
			curPolicy("corrupt", "", 0, nil, 0), curPolicy("emptywin", "", 0, nil, 0),
			curPolicy("failed", "", 0, nil, 0), curPolicy("negrem", "", 0, nil, 0),
			curPolicy("nilsnap", "", 0, nil, 0), curPolicy("partial", "", 0, nil, 0),
			curPolicy("sparse", "", 0, nil, 0), curPolicy("unknown", "", 0, nil, 0),
			curPolicy("unavail", "", 0, nil, 0), curPolicy("zerorem", "", 0, nil, 0),
		}}
		obs := []ProviderObs{
			curHealthyObs("ok", "normal", now, 0, wZero),
			curHealthyObs("corrupt", "normal", now, 0, wZero),
			curHealthyObs("emptywin", "normal", now, 0, wEmpty),
			curHealthyObs("failed", "normal", now, 0, wZero),
			curHealthyObs("negrem", "normal", now, 0, wZero),
			{MappingID: "nilsnap", Mode: "normal"},
			curHealthyObs("partial", "normal", now, 0, wZero),
			curHealthyObs("sparse", "normal", now, 0, wSparse),
			curHealthyObs("unknown", "normal", now, 0, wZero),
			curHealthyObs("unavail", "normal", now, 0, wZero),
			curHealthyObs("zerorem", "normal", now, 0, wZeroRem),
		}
		// Degrade the copies that need a non-fresh status, a bad enum, or a
		// negative remaining.
		obs[1].Snapshot.Availability = quota.QuotaAvailability("corrupt")
		obs[3].Snapshot.Status = quota.SourceFailed  // failed
		obs[6].Snapshot.Status = quota.SourcePartial // partial
		obs[8].Snapshot.Availability = quota.QuotaUnknown
		obs[9].Snapshot.Availability = quota.QuotaUnavailable
		obs[4].Snapshot.Windows[0].Used = fptr(1.2) // used beyond limit -> clamped to 0

		in.Obs = obs
		res := curRank(t, "degraded", in)
		want := []curWant{{"ok", 0, false, true}}
		for rank, id := range []string{
			"corrupt", "emptywin", "failed", "negrem", "nilsnap",
			"partial", "sparse", "unavail", "unknown", "zerorem",
		} {
			want = append(want, curWant{id, rank + 1, false, false})
		}
		curCheck(t, "degraded", res, want)
	})

	t.Run("remaining source preference", func(t *testing.T) {
		// used/limit wins over usage_percent: used==limit forces remaining 0
		// (ineligible) even though usage_percent would read a healthy 50.
		w := quota.QuotaWindow{
			Used: fptr(1), Limit: fptr(1), UsagePercent: fptr(50),
			ResetAt: tptr(now.Add(sigResetIn)), Period: durptr(sigWindowPeriod),
		}
		in := RankingInput{Now: now, Policies: []ProviderPolicy{curPolicy("p", "", 0, nil, 0)}}
		obs := []ProviderObs{{MappingID: "p", Mode: "normal", Snapshot: &quota.QuotaSnapshot{
			MappingID: "p", CheckedAt: now, Windows: []quota.QuotaWindow{w},
			Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
		}}}
		in.Obs = obs
		res := curRank(t, "preference", in)
		curCheck(t, "preference", res, []curWant{{"p", 0, false, false}})
	})

	t.Run("off-peak UTC boundaries", func(t *testing.T) {
		week := ParseScheduleUTC(t, []OffPeakWindow{{Days: []DayOfWeek{Monday, Tuesday, Wednesday, Thursday, Friday}, Start: "09:00", End: "17:00"}})
		mon := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
		if mon.Weekday() != time.Monday {
			t.Fatalf("anchor date is not a Monday: %s", mon)
		}
		cases := []struct {
			at    time.Time
			offpk bool
		}{
			{mon.Add(8*time.Hour + 59*time.Minute), false}, // 08:59 outside -> peak
			{mon.Add(9 * time.Hour), true},                 // 09:00 start inclusive -> off-peak
			{mon.Add(16*time.Hour + 59*time.Minute), true}, // 16:59 inside -> off-peak
			{mon.Add(17 * time.Hour), false},               // 17:00 end exclusive -> peak
		}
		for _, tc := range cases {
			if got := week.IsOffPeak(tc.at); got != tc.offpk {
				t.Fatalf("IsOffPeak(%s)=%t want %t", tc.at, got, tc.offpk)
			}
		}

		// Midnight-crossing: Saturday 22:00 through Sunday 04:00.
		wkend := ParseScheduleUTC(t, []OffPeakWindow{{Days: []DayOfWeek{Saturday}, Start: "22:00", End: "04:00"}})
		sat := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
		sun := sat.Add(24 * time.Hour)
		wkCases := []struct {
			at    time.Time
			offpk bool
		}{
			{sat.Add(21*time.Hour + 59*time.Minute), false}, // 21:59 before start -> peak
			{sat.Add(22 * time.Hour), true},                 // 22:00 start -> off-peak
			{sun.Add(3*time.Hour + 59*time.Minute), true},   // Sunday 03:59 still in window
			{sun.Add(4 * time.Hour), false},                 // Sunday 04:00 end exclusive -> peak
		}
		for _, tc := range wkCases {
			if got := wkend.IsOffPeak(tc.at); got != tc.offpk {
				t.Fatalf("IsOffPeak(%s)=%t want %t", tc.at, got, tc.offpk)
			}
		}

		// "24:00" end-of-day sentinel: the window covers the whole day, so
		// every instant is off-peak.
		allDay := ParseScheduleUTC(t, []OffPeakWindow{{Days: allDays, Start: "00:00", End: "24:00"}})
		for _, at := range []time.Time{mon, mon.Add(23*time.Hour + 59*time.Minute)} {
			if !allDay.IsOffPeak(at) {
				t.Fatalf("all-day window not off-peak at %s", at)
			}
		}

		// A zero-value (unparsed) schedule is never off-peak (fail closed on
		// the nil cached location). A nil *Schedule never reaches the value
		// receiver: Rank's offPeakAt helper treats it as never off-peak, which
		// the nil-schedule cases above already exercise.
		if (Schedule{}).IsOffPeak(now) {
			t.Fatal("zero-value schedule reports off-peak")
		}
	})

	t.Run("off-peak then weight decide ties", func(t *testing.T) {
		// A 12:00–18:00 UTC off-peak window contains noon but not 06:00.
		afternoon := ParseScheduleUTC(t, []OffPeakWindow{{Days: allDays, Start: "12:00", End: "18:00"}})
		// At noon the afternoon schedule is off-peak; the nil schedule is peak.
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("y", "", 0, nil, 0),       // peak
			curPolicy("x", "", 0, afternoon, 0), // off-peak
		}}
		obs := []ProviderObs{curHealthyObs("x", "normal", now, 0, wZero), curHealthyObs("y", "normal", now, 0, wZero)}
		in.Obs = obs
		curCheck(t, "offpeak first", curRank(t, "op", in), []curWant{
			{"x", 0, true, true}, {"y", 1, false, true},
		})

		// Same case at 06:00: both peak, equal signals -> shared rank, ID order.
		six := now.Add(-6 * time.Hour)
		obs6 := []ProviderObs{curHealthyObs("x", "normal", six, 0, wZero), curHealthyObs("y", "normal", six, 0, wZero)}
		curCheck(t, "offpeak tie at 06:00", curRank(t, "op6", RankingInput{Now: six, Policies: in.Policies, Obs: obs6}), []curWant{
			{"x", 0, false, true}, {"y", 0, false, true},
		})

		// Weight: 0 resolves to the default 1 and ties with an explicit 1;
		// higher weight ranks first; negative weight ranks last.
		inW := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("b0", "", 0, nil, 0), curPolicy("a7", "", 7, nil, 0),
			curPolicy("dm2", "", -2, nil, 0), curPolicy("c1", "", 1, nil, 0),
		}}
		obsW := []ProviderObs{
			curHealthyObs("a7", "normal", now, 0, wZero), curHealthyObs("b0", "normal", now, 0, wZero),
			curHealthyObs("c1", "normal", now, 0, wZero), curHealthyObs("dm2", "normal", now, 0, wZero),
		}
		curCheck(t, "weights", curRank(t, "w", RankingInput{Now: now, Policies: inW.Policies, Obs: obsW}), []curWant{
			{"a7", 0, false, true},
			{"b0", 1, false, true}, {"c1", 1, false, true},
			{"dm2", 2, false, true},
		})

		// Off-peak beats weight: off-peak weight 1 outranks peak weight 7.
		inB := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("pk", "", 7, nil, 0), curPolicy("op", "", 1, afternoon, 0),
		}}
		obsB := []ProviderObs{curHealthyObs("op", "normal", now, 0, wZero), curHealthyObs("pk", "normal", now, 0, wZero)}
		inB.Obs = obsB
		curCheck(t, "offpeak beats weight", curRank(t, "b", inB), []curWant{
			{"op", 0, true, true}, {"pk", 1, false, true},
		})
	})

	t.Run("adjacent gap clusters and transitive chains", func(t *testing.T) {
		rank := func(policies []ProviderPolicy, obs []ProviderObs, want []curWant, stage string) {
			t.Helper()
			curCheck(t, stage, curRank(t, stage, RankingInput{Now: now, Policies: policies, Obs: obs}), want)
		}
		mk := func(id string, w windowClass) (ProviderPolicy, ProviderObs) {
			return curPolicy(id, "", 0, nil, 0), curHealthyObs(id, "normal", now, 0, w)
		}

		// farP vs p: adjacent gap ~0.94 >= 0.20 -> separate clusters.
		p1, o1 := mk("f", wFarP)
		p2, o2 := mk("n", wP)
		rank([]ProviderPolicy{p1, p2}, []ProviderObs{o1, o2}, []curWant{
			{"f", 0, false, true}, {"n", 1, false, true},
		}, "distinct clusters")

		// p, zero, m: adjacent gaps ~0.082 < 0.20 -> one transitive cluster,
		// all share a rank despite a total spread of ~0.16.
		p3, o3 := mk("n", wP)
		p4, o4 := mk("z", wZero)
		p5, o5 := mk("m", wM)
		rank([]ProviderPolicy{p3, p4, p5}, []ProviderObs{o3, o4, o5}, []curWant{
			{"m", 0, false, true}, {"n", 0, false, true}, {"z", 0, false, true},
		}, "transitive chain one cluster")

		// Mixed: farP alone, then p+zero tied.
		p6, o6 := mk("a", wFarP)
		p7, o7 := mk("b", wP)
		p8, o8 := mk("c", wZero)
		rank([]ProviderPolicy{p6, p7, p8}, []ProviderObs{o6, o7, o8}, []curWant{
			{"a", 0, false, true}, {"b", 1, false, true}, {"c", 1, false, true},
		}, "two clusters with tie")

		// Two identical windows produce the same signal as one: dbl ties with p.
		p9, o9 := mk("d", wDbl)
		p10, o10 := mk("n", wP)
		rank([]ProviderPolicy{p9, p10}, []ProviderObs{o9, o10}, []curWant{
			{"d", 0, false, true}, {"n", 0, false, true},
		}, "identical-window mean ties")

		// usage_percent-sourced remaining gives the known 0.525 class: its own
		// cluster between farP and p.
		p11, o11 := mk("u", wUp)
		p12, o12 := mk("n", wP)
		p13, o13 := mk("f", wFarP)
		rank([]ProviderPolicy{p11, p12, p13}, []ProviderObs{o11, o12, o13}, []curWant{
			{"f", 0, false, true}, {"u", 1, false, true}, {"n", 2, false, true},
		}, "usage-percent class separates")
	})

	t.Run("group missing signal suppresses signal comparison", func(t *testing.T) {
		// x has only a 5h window: eligible but signal-less. The whole group
		// loses the signal key, so y (farP) and z (p) no longer outrank it.
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("x", "gA", 0, nil, 0), curPolicy("y", "gA", 0, nil, 0), curPolicy("z", "gA", 0, nil, 0),
		}}
		obs := []ProviderObs{
			curHealthyObs("x", "normal", now, 0, wNone),
			curHealthyObs("y", "normal", now, 0, wFarP),
			curHealthyObs("z", "normal", now, 0, wP),
		}
		in.Obs = obs
		res := curRank(t, "suppress", in)
		curCheck(t, "suppress", res, []curWant{
			{"x", 0, false, true}, {"y", 0, false, true}, {"z", 0, false, true},
		})
		for _, e := range res.Entries {
			if strings.Contains(e.Explanation, "signal") {
				t.Fatalf("suppressed group explanation mentions signal: %q", e.Explanation)
			}
		}

		// Contrast: the same group without x keeps the signal; farP outranks
		// the p/m tie and explanations mention the signal.
		in2 := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("w", "gA", 0, nil, 0), curPolicy("y", "gA", 0, nil, 0), curPolicy("z", "gA", 0, nil, 0),
		}}
		obs2 := []ProviderObs{
			curHealthyObs("w", "normal", now, 0, wM),
			curHealthyObs("y", "normal", now, 0, wFarP),
			curHealthyObs("z", "normal", now, 0, wP),
		}
		in2.Obs = obs2
		res2 := curRank(t, "nonsuppressed", in2)
		curCheck(t, "nonsuppressed", res2, []curWant{
			{"y", 0, false, true}, {"w", 1, false, true}, {"z", 1, false, true},
		})
		for _, e := range res2.Entries {
			if !strings.Contains(e.Explanation, "signal") {
				t.Fatalf("expected signal in explanation: %q", e.Explanation)
			}
		}
	})

	t.Run("groups contiguous, first appearance, ineligible tail", func(t *testing.T) {
		// Policy order interleaves gA/gB and mixes eligibility; eligible
		// groups appear contiguously in first-eligible-appearance order and
		// ineligible entries trail sorted by ID.
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("a1", "gA", 0, nil, 0),
			curPolicy("b1", "gB", 0, nil, 0),
			curPolicy("a2", "gA", 0, nil, 0), // becomes ineligible (disabled)
			curPolicy("b2", "gB", 0, nil, 0),
			curPolicy("a3", "gA", 0, nil, 0),
			curPolicy("z9", "", 0, nil, 0), // becomes ineligible (stale)
		}}
		obs := []ProviderObs{
			curHealthyObs("a1", "normal", now, 0, wFarP),
			curHealthyObs("b1", "normal", now, 0, wFarP),
			curHealthyObs("a2", "disabled", now, 0, wFarP),
			curHealthyObs("b2", "normal", now, 0, wFarP),
			curHealthyObs("a3", "normal", now, 0, wFarP),
			curHealthyObs("z9", "normal", now, 2*time.Hour, wFarP),
		}
		in.Obs = obs
		curCheck(t, "layout", curRank(t, "layout", in), []curWant{
			{"a1", 0, false, true}, {"a3", 0, false, true},
			{"b1", 1, false, true}, {"b2", 1, false, true},
			{"a2", 2, false, false}, {"z9", 3, false, false},
		})
	})

	t.Run("rank continuation across groups", func(t *testing.T) {
		in := RankingInput{Now: now, Policies: []ProviderPolicy{
			curPolicy("p", "g1", 0, nil, 0), curPolicy("q", "g1", 0, nil, 0),
			curPolicy("s", "g2", 0, nil, 0), curPolicy("t", "g2", 0, nil, 0),
		}}
		obs := []ProviderObs{
			curHealthyObs("p", "normal", now, 0, wFarP),
			curHealthyObs("q", "normal", now, 0, wP),
			curHealthyObs("s", "normal", now, 0, wM),
			curHealthyObs("t", "disabled", now, 0, wP),
		}
		in.Obs = obs
		curCheck(t, "continuation", curRank(t, "cont", in), []curWant{
			{"p", 0, false, true}, {"q", 1, false, true},
			{"s", 2, false, true}, {"t", 3, false, false},
		})
	})

	t.Run("input fidelity", func(t *testing.T) {
		// Observations for unknown IDs are ignored; every policy appears
		// exactly once; empty input yields an empty result; repeated calls
		// agree and inputs are untouched.
		in := RankingInput{Now: now, Policies: []ProviderPolicy{curPolicy("a", "", 0, nil, 0)}}
		obs := []ProviderObs{curHealthyObs("a", "normal", now, 0, wZero), curHealthyObs("ghost", "normal", now, 0, wFarP)}
		in.Obs = obs
		res := curRank(t, "fidelity", in)
		curCheck(t, "fidelity", res, []curWant{{"a", 0, false, true}})

		empty := curRank(t, "empty", RankingInput{Now: now})
		if len(empty.Entries) != 0 {
			t.Fatalf("empty input produced %d entries", len(empty.Entries))
		}

		preP, preO := cloneRankingInput(in.Policies, obs)
		r1 := Rank(in)
		r2 := Rank(in)
		r3 := Rank(in)
		if !reflect.DeepEqual(r1, r2) || !reflect.DeepEqual(r2, r3) {
			t.Fatal("repeated Rank calls disagree")
		}
		postP, postO := cloneRankingInput(in.Policies, obs)
		if !reflect.DeepEqual(preP, postP) || !reflect.DeepEqual(preO, postO) {
			t.Fatal("Rank mutated its input")
		}
	})
}

// ParseScheduleUTC parses a schedule in UTC or fails the test.
func ParseScheduleUTC(t *testing.T, windows []OffPeakWindow) *Schedule {
	t.Helper()
	s, err := ParseSchedule("UTC", windows)
	if err != nil {
		t.Fatalf("ParseSchedule UTC: %v", err)
	}
	return &s
}
