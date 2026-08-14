// Package routing implements the pure, deterministic provider routing policy.
//
// The policy is a pure function: given validated inputs (provider policies, an
// injected clock, observed quota snapshots, modes, and usage history) it returns
// a deterministic global ranking with per-provider explanations. It performs no
// I/O of its own (no HTTP, no state persistence, no time.Now), has no side
// effects, and depends only on the standard library and internal/quota.
//
// Design principles (from the approved spec):
//   - Fail closed: stale/missing/unknown data makes a provider unrankable; it is
//     never demoted or disabled by inference.
//   - No fabricated comparisons: when usage units are incomparable the usage key
//     is skipped for the entire balance group.
//   - Determinism: ranking is a pure function of RankingInput; results are stable
//     across repeated calls with no map-iteration ordering dependence.
package routing

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// DayOfWeek is a canonical weekday abbreviation.
type DayOfWeek string

// Canonical weekday abbreviations used by schedules.
const (
	Monday    DayOfWeek = "mon"
	Tuesday   DayOfWeek = "tue"
	Wednesday DayOfWeek = "wed"
	Thursday  DayOfWeek = "thu"
	Friday    DayOfWeek = "fri"
	Saturday  DayOfWeek = "sat"
	Sunday    DayOfWeek = "sun"
)

// validDay reports whether d is a recognized canonical abbreviation.
var validDay = map[DayOfWeek]bool{
	Monday: true, Tuesday: true, Wednesday: true, Thursday: true,
	Friday: true, Saturday: true, Sunday: true,
}

// dayFromWeekday maps a time.Weekday to its canonical abbreviation.
func dayFromWeekday(w time.Weekday) DayOfWeek {
	switch w {
	case time.Sunday:
		return Sunday
	case time.Monday:
		return Monday
	case time.Tuesday:
		return Tuesday
	case time.Wednesday:
		return Wednesday
	case time.Thursday:
		return Thursday
	case time.Friday:
		return Friday
	default:
		return Saturday
	}
}

// OffPeakWindow is a single off-peak time window on specific days.
type OffPeakWindow struct {
	Days  []DayOfWeek
	Start string // "HH:MM" (00:00–23:59)
	End   string // "HH:MM" (00:00–23:59; "24:00" is an end-of-day sentinel)
}

// Schedule is a provider's off-peak schedule in a timezone.
type Schedule struct {
	Timezone string // IANA name, e.g. "America/Los_Angeles"
	OffPeak  []OffPeakWindow

	// loc is the parsed *time.Location, cached at parse time so the
	// evaluation path (IsOffPeak) is free of filesystem I/O. It is nil for a
	// zero-value Schedule; ParseSchedule is the only constructor that sets it.
	loc *time.Location
}

const (
	minutesPerDay   = 24 * 60
	endOfDayMinute  = minutesPerDay // 1440; represents "24:00" / next-day 00:00
	defaultFreshTTL = 30 * time.Minute
	defaultWeight   = 1
	defaultGroup    = "default"
)

// windowMinute parses an "HH:MM" wall-clock time into minutes since midnight.
// When allowEndSentinel is true the literal "24:00" is accepted and returned as
// endOfDayMinute (1440), representing end-of-day. It returns ok=false for
// anything malformed or out of range.
func windowMinute(s string, allowEndSentinel bool) (int, bool) {
	if s == "24:00" {
		if !allowEndSentinel {
			return 0, false
		}
		return endOfDayMinute, true
	}
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	hh, okH := twoDigits(s[0:2])
	mm, okM := twoDigits(s[3:5])
	if !okH || !okM {
		return 0, false
	}
	if hh > 23 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// twoDigits parses a two-character ASCII decimal string.
func twoDigits(s string) (int, bool) {
	if len(s) != 2 {
		return 0, false
	}
	if s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

// ParseSchedule validates and canonicalizes a schedule. It returns an error on
// any invalid timezone, day, or time value. The literal "24:00" is accepted only
// as an end-of-day sentinel. A schedule with no windows means "never off-peak".
func ParseSchedule(tz string, windows []OffPeakWindow) (Schedule, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	parsed := make([]OffPeakWindow, len(windows))
	for i, w := range windows {
		for _, d := range w.Days {
			if !validDay[d] {
				return Schedule{}, fmt.Errorf("window %d: invalid day %q", i, d)
			}
		}
		if _, ok := windowMinute(w.Start, false); !ok {
			return Schedule{}, fmt.Errorf("window %d: invalid start time %q", i, w.Start)
		}
		if _, ok := windowMinute(w.End, true); !ok {
			return Schedule{}, fmt.Errorf("window %d: invalid end time %q", i, w.End)
		}
		days := make([]DayOfWeek, len(w.Days))
		copy(days, w.Days)
		parsed[i] = OffPeakWindow{Days: days, Start: w.Start, End: w.End}
	}
	return Schedule{Timezone: tz, OffPeak: parsed, loc: loc}, nil
}

// IsOffPeak reports whether at (evaluated in the schedule's timezone) falls
// within any off-peak window. It handles midnight-crossing windows and DST
// transitions by comparing wall-clock minutes, which are offset-independent.
// An empty or invalid schedule never reports off-peak.
func (s Schedule) IsOffPeak(at time.Time) bool {
	if len(s.OffPeak) == 0 {
		return false
	}
	// Use the location cached at parse time. A nil location (zero-value or
	// not parsed) is treated as never off-peak (fail closed) without I/O.
	if s.loc == nil {
		return false
	}
	local := at.In(s.loc)
	day := dayFromWeekday(local.Weekday())
	prevDay := dayFromWeekday(local.AddDate(0, 0, -1).Weekday())
	minutes := local.Hour()*60 + local.Minute()
	for _, w := range s.OffPeak {
		start, ok := windowMinute(w.Start, false)
		if !ok {
			continue
		}
		end, ok := windowMinute(w.End, true)
		if !ok {
			continue
		}
		days := make(map[DayOfWeek]bool, len(w.Days))
		for _, d := range w.Days {
			days[d] = true
		}
		if start <= end {
			// Same-day window: [start, end) on a matching day.
			if days[day] && minutes >= start && minutes < end {
				return true
			}
			continue
		}
		// Midnight-crossing window: [start, midnight) starts on a matching day,
		// and [midnight, end) runs into the next day, so it matches when the
		// previous calendar day was a matching day.
		if days[day] && minutes >= start {
			return true
		}
		if days[prevDay] && minutes < end {
			return true
		}
	}
	return false
}

// offPeakAt evaluates off-peak status for a schedule pointer, treating a nil
// schedule as "never off-peak". It avoids dereferencing a nil *Schedule when
// calling the value-receiver IsOffPeak method.
func offPeakAt(s *Schedule, at time.Time) bool {
	if s == nil {
		return false
	}
	return s.IsOffPeak(at)
}

// ProviderPolicy is the routing-relevant configuration for one provider mapping.
type ProviderPolicy struct {
	MappingID    string
	BalanceGroup string        // partitions comparisons; default "default"
	Schedule     *Schedule     // nil = never off-peak
	FreshnessTTL time.Duration // default 30m when zero
	Weight       int           // tie-break/scaling; default 1
}

func (p ProviderPolicy) balanceGroup() string {
	if p.BalanceGroup == "" {
		return defaultGroup
	}
	return p.BalanceGroup
}

func (p ProviderPolicy) freshness() time.Duration {
	if p.FreshnessTTL <= 0 {
		return defaultFreshTTL
	}
	return p.FreshnessTTL
}

func (p ProviderPolicy) weight() int {
	if p.Weight == 0 {
		return defaultWeight
	}
	return p.Weight
}

// ProviderObs is the observed state for one provider mapping at ranking time.
type ProviderObs struct {
	MappingID string
	Mode      string // "normal" | "reserve" | "disabled" (from state.EffectiveMode)
	Snapshot  *quota.QuotaSnapshot
}

// RankingInput is everything the pure policy needs.
type RankingInput struct {
	Now      time.Time
	Policies []ProviderPolicy
	Obs      []ProviderObs
}

// Eligibility reports whether a provider is rankable, with a reason when not.
type Eligibility struct {
	Rankable bool
	Reason   string // sanitized explanation when not rankable
}

// CheckEligibility evaluates whether a provider can participate in ranking.
//
// A provider is unrankable (fail closed) — never demoted or disabled by
// inference — when its mode is not normal/reserve, its snapshot is missing or
// stale, its source failed, or it reports no usable quota data.
func CheckEligibility(policy ProviderPolicy, obs ProviderObs, now time.Time) Eligibility {
	switch obs.Mode {
	case "normal", "reserve":
	default:
		return Eligibility{Reason: "disabled"}
	}
	snap := obs.Snapshot
	if snap == nil || now.Sub(snap.CheckedAt) > policy.freshness() {
		return Eligibility{Reason: "no fresh snapshot"}
	}
	if snap.Status == quota.SourceFailed {
		return Eligibility{Reason: "authentication/configuration failure"}
	}
	if snap.Status == quota.SourcePartial {
		return Eligibility{Reason: "partial quota data"}
	}
	// Fail closed on anything but an explicit "available": unavailable means
	// the provider reported itself exhausted, and any other value (unknown,
	// empty, or a corrupted enum) is unusable data, never rankable.
	if snap.Availability != quota.QuotaAvailable {
		if snap.Availability == quota.QuotaUnavailable {
			return Eligibility{Reason: "quota exhausted/unavailable"}
		}
		return Eligibility{Reason: "no usable quota data"}
	}
	if rem := snap.EffectiveRemaining(); rem == nil || *rem <= 0 {
		return Eligibility{Reason: "no usable quota data"}
	}
	return Eligibility{Rankable: true}
}

// RankEntry is one provider's position in the global ranking.
type RankEntry struct {
	MappingID   string
	Rank        int // 0-based global rank
	OffPeak     bool
	Eligible    bool
	Explanation string // human-readable, sanitized reason for position
}

// RankingResult is the deterministic global ranking.
type RankingResult struct {
	Entries []RankEntry // sorted by rank
}

// rankItem is the per-provider data computed before sorting within a group.
type rankItem struct {
	policy  ProviderPolicy
	offPeak bool
	pace    *float64 // projection pace; nil when not computable
	cluster int      // pace cluster index; -1 when no pace
	weight  int
}

// entry pairs a provider's computed item with its eligibility/reason.
type entry struct {
	item     rankItem
	eligible bool
	reason   string
}

// less compares two items within a balance group by the lexicographic key
// sequence: pace cluster (pairwise) → off-peak → weight → mapping ID.
func (a rankItem) less(b rankItem) bool {
	// Key 1: projection pace cluster (pairwise: both must have a pace).
	if a.cluster >= 0 && b.cluster >= 0 && a.cluster != b.cluster {
		return a.cluster < b.cluster
	}
	// Key 2: off-peak before peak.
	if a.offPeak != b.offPeak {
		return a.offPeak
	}
	// Key 3: higher configured weight first.
	if a.weight != b.weight {
		return a.weight > b.weight
	}
	// Key 4: lexical mapping ID ascending (stable final tie-breaker).
	return a.policy.MappingID < b.policy.MappingID
}

// minProjectionPeriod is the minimum window duration eligible to be a projection
// anchor. Windows shorter than one day (e.g. codex's 5h session window) are rate
// limits, not quota cycles, and are excluded from projection.
const minProjectionPeriod = 24 * time.Hour

// computePace calculates the projection pace for a provider from its anchor
// window — the longest window with Period + ResetAt + a usable remaining that
// clears the minimum-period floor. Returns the pace and true when computable;
// 0 and false when no qualifying window exists.
//
//	usedFrac    = 1 - remainingFraction
//	elapsedFrac = clamp01(1 - (ResetAt - now) / Period)
//	pace        = usedFrac / max(elapsedFrac, eps)
//
// pace < 1.0 → under-utilized (ranks first); pace > 1.0 → over-utilized.
// A just-reset window (used≈0, elapsed≈0) yields pace ≈ 0.
func computePace(snap *quota.QuotaSnapshot, now time.Time) (pace float64, ok bool) {
	if snap == nil {
		return 0, false
	}
	var anchor *quota.QuotaWindow
	for i := range snap.Windows {
		w := &snap.Windows[i]
		if w.Period == nil || *w.Period < minProjectionPeriod {
			continue
		}
		if w.ResetAt == nil {
			continue
		}
		if w.Remaining() == nil {
			continue
		}
		if anchor == nil || *w.Period > *anchor.Period {
			anchor = w
		}
	}
	if anchor == nil {
		return 0, false
	}
	rem := anchor.Remaining()
	usedFrac := 1.0 - *rem
	timeToReset := anchor.ResetAt.Sub(now)
	elapsedFrac := 1.0 - float64(timeToReset)/float64(*anchor.Period)
	if elapsedFrac < 0 {
		elapsedFrac = 0
	} else if elapsedFrac > 1 {
		elapsedFrac = 1
	}
	eps := float64(5*time.Minute) / float64(*anchor.Period)
	if elapsedFrac < eps {
		elapsedFrac = eps
	}
	return usedFrac / elapsedFrac, true
}

// assignPaceClusters assigns cluster indices to entries within a balance group.
// Items without a pace get cluster -1. Among projecting items, connected
// components of the "within 10% absolute pace" relation form clusters,
// indexed in ascending pace order.
func assignPaceClusters(items []entry) {
	type paced struct {
		idx  int
		pace float64
	}
	var ps []paced
	for i := range items {
		if items[i].item.pace != nil {
			ps = append(ps, paced{idx: i, pace: *items[i].item.pace})
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		return ps[i].pace < ps[j].pace
	})
	clusterIdx := 0
	for i, p := range ps {
		if i > 0 && p.pace-ps[i-1].pace >= 0.10 {
			clusterIdx++
		}
		items[p.idx].item.cluster = clusterIdx
	}
}

// Rank computes the deterministic global ranking for the given input.
//
// Eligible providers are grouped by balance group (in first-appearance order;
// groups never interleave) and sorted within each group by the lexicographic
// keys. Ineligible providers are placed after all eligible ones, sorted by
// mapping ID. Within a balance group the pace-projection key is compared
// pairwise: two providers compare on pace only when both can project; if
// either cannot, that pair falls through to off-peak → weight → ID.
func Rank(in RankingInput) RankingResult {
	obsBy := make(map[string]ProviderObs, len(in.Obs))
	for _, o := range in.Obs {
		obsBy[o.MappingID] = o
	}

	entries := make([]entry, 0, len(in.Policies))
	for _, p := range in.Policies {
		obs, ok := obsBy[p.MappingID]
		// The policy (including MappingID) is always set so a missing
		// observation never silently loses the provider's identity.
		item := rankItem{policy: p, cluster: -1}
		var eligible bool
		var reason string
		if ok {
			el := CheckEligibility(p, obs, in.Now)
			eligible = el.Rankable
			reason = el.Reason
			item = rankItem{policy: p, weight: p.weight(), offPeak: offPeakAt(p.Schedule, in.Now), cluster: -1}
			if obs.Snapshot != nil {
				if pace, ok := computePace(obs.Snapshot, in.Now); ok {
					item.pace = &pace
				}
			}
		} else {
			reason = "no fresh snapshot"
		}
		entries = append(entries, entry{item: item, eligible: eligible, reason: reason})
	}

	// Partition into eligible (grouped) and ineligible.
	type group struct {
		name  string
		items []entry
	}
	groupOrder := make([]string, 0)
	groups := make(map[string]*group)
	var ineligible []entry
	for _, e := range entries {
		if !e.eligible {
			ineligible = append(ineligible, e)
			continue
		}
		g := e.item.policy.balanceGroup()
		if _, seen := groups[g]; !seen {
			groups[g] = &group{name: g}
			groupOrder = append(groupOrder, g)
		}
		groups[g].items = append(groups[g].items, e)
	}

	// Assign pace clusters and stable-sort within each group.
	for _, name := range groupOrder {
		g := groups[name]
		assignPaceClusters(g.items)
		sort.SliceStable(g.items, func(i, j int) bool {
			return g.items[i].item.less(g.items[j].item)
		})
	}

	sort.SliceStable(ineligible, func(i, j int) bool {
		return ineligible[i].item.policy.MappingID < ineligible[j].item.policy.MappingID
	})

	result := RankingResult{Entries: make([]RankEntry, 0, len(entries))}
	for _, name := range groupOrder {
		for _, e := range groups[name].items {
			result.Entries = append(result.Entries, e.toRankEntry(len(result.Entries)))
		}
	}
	for _, e := range ineligible {
		result.Entries = append(result.Entries, e.toRankEntry(len(result.Entries)))
	}
	return result
}

// toRankEntry renders an entry as a public RankEntry at the given rank.
func (e entry) toRankEntry(rank int) RankEntry {
	if !e.eligible {
		return RankEntry{
			MappingID:   e.item.policy.MappingID,
			Rank:        rank,
			OffPeak:     e.item.offPeak,
			Eligible:    false,
			Explanation: "ineligible: " + e.reason,
		}
	}
	return RankEntry{
		MappingID:   e.item.policy.MappingID,
		Rank:        rank,
		OffPeak:     e.item.offPeak,
		Eligible:    true,
		Explanation: e.explain(),
	}
}

// explain renders a short, sanitized explanation referencing the decisive
// factors (projection pace and off-peak status) for an eligible provider.
func (e entry) explain() string {
	status := "peak"
	if e.item.offPeak {
		status = "off-peak"
	}
	if e.item.pace != nil {
		return fmt.Sprintf("%s, pace %d%%", status, int(math.Round(*e.item.pace*100)))
	}
	return status
}
