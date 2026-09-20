package selection

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// SelectionVersion is the schema version of a selection Result. It is safe to
// evolve Result additively behind this version; consumers must ignore unknown
// fields.
const SelectionVersion = 1

// Kind classifies a selection by the strength of its evidence.
type Kind string

// Selection kinds. A confirmed selection rests on fresh, usable provider
// quota evidence; an uncertain selection is the documented fallback taken
// when no confirmed candidate qualifies. Uncertain never overrides a known
// disabled, unavailable, or exhausted state — those are excluded outright.
const (
	KindConfirmed Kind = "confirmed"
	KindUncertain Kind = "uncertain"
)

// Evidence categories reported on an uncertain selection: why the provider's
// quota evidence did not support confirmation.
const (
	EvidenceMissing = "missing" // no observation was ever recorded
	EvidenceFailed  = "failed"  // a refresh was attempted but produced no usable snapshot
	EvidenceStale   = "stale"   // the last good snapshot is older than the configured TTL
	EvidencePartial = "partial" // the snapshot is fresh but its source reported incomplete data
	EvidenceSparse  = "sparse"  // the snapshot is fresh and complete but reports no usable window
	EvidenceUnknown = "unknown" // provenance is absent, inconsistent, or not explicitly established
)

// ErrNoCandidate is returned when every candidate for the phase and tier is
// excluded and no candidate remains — confirmed or uncertain.
var ErrNoCandidate = errors.New("selection: no eligible candidate")

// Result is one selection outcome. It carries only sanitized, non-secret
// fields and marshals to stable version-1 JSON; Headroom and Evidence are
// omitted when they do not apply.
type Result struct {
	Version   int    `json:"version"` // always SelectionVersion (1)
	Phase     string `json:"phase"`
	Tier      string `json:"tier"`
	Reference string `json:"reference"` // exact candidate reference, suffix preserved
	Base      string `json:"base"`      // reference with any reasoning suffix removed
	Suffix    string `json:"suffix,omitempty"`
	Mapping   string `json:"mapping"` // resolved provider mapping ID
	Kind      Kind   `json:"kind"`
	// Headroom is the provider snapshot's EffectiveRemaining (minimum remaining
	// fraction across usable windows, 0.0–1.0) for confirmed selections.
	Headroom *float64 `json:"headroom,omitempty"`
	// Evidence is the uncertain-evidence category for uncertain selections.
	Evidence string `json:"evidence,omitempty"`
}

// candidate is one reference's classification for the given desired policy,
// observed state, and reference time.
type candidate struct {
	ref      string
	base     string
	suffix   string
	mapping  policy.MappingID
	headroom *float64 // confirmed only
	// status is the classification outcome.
	status candidacy
	// reason holds the exclusion reason or the uncertain evidence category.
	reason string
}

type candidacy int

const (
	candConfirmed candidacy = iota
	candUncertain
	candExcluded
)

// Select chooses one candidate for the phase and tier: a pure function of the
// candidate policy, the desired policy graph, the durable observed state, the
// reference time, and the excluded provider families. It never launches work,
// reserves quota, or performs I/O.
//
// Candidates are excluded (fail-closed, ineligible even for the uncertain
// fallback) when their provider family is excluded, their baseline is
// disabled, the provider is manually disabled or known unavailable or
// exhausted, any state or snapshot enum carries an unrecognized non-empty
// value, a quota window reports impossible values, the snapshot reports the
// provider unavailable, or its usable headroom is non-positive. Known-bad
// snapshot evidence excludes before any freshness or completeness demotion: a
// stale or partial snapshot that reports unavailability or zero remaining
// still excludes.
//
// A candidate is confirmed only when every gate holds explicitly: the
// provider's durable quota and availability axes are both present and
// supported (never EffectiveMode's sparse empty-axis defaults), the mapping
// has a configured freshness TTL, the last-good snapshot is fresh within that
// TTL with a non-zero CheckedAt no later than the reference time (future or
// zero timestamps are treated conservatively as unusable provenance), the
// source status is exactly the successful one, the snapshot explicitly
// reports availability, and the usable headroom is positive. Everything else
// is uncertain, categorized by evidence; empty enum values are uncertain
// (sparse), never corrupt.
//
// All confirmed groups are considered before any uncertain fallback: the
// first group (in policy order) holding a confirmed candidate wins, choosing
// the maximum headroom with stable (policy-order) tie-breaking. When no
// candidate is confirmed anywhere, the first group holding an uncertain
// candidate wins, choosing its first member in policy order. The reference is
// returned with its exact spelling preserved. ErrNoCandidate is returned when
// nothing remains; an unknown phase or tier is an error.
func Select(p Policy, desired policy.Desired, st state.State, asOf time.Time, phase string, tier Tier, excludedFamilies []string) (Result, error) {
	if !ValidTier(tier) {
		return Result{}, fmt.Errorf("selection: unknown tier %q", tier)
	}
	pp, ok := p.Phases[phase]
	if !ok {
		return Result{}, fmt.Errorf("selection: unknown phase %q", phase)
	}
	tp, ok := pp[tier]
	if !ok {
		return Result{}, fmt.Errorf("selection: phase %q: no %q tier", phase, tier)
	}
	excludedFamiliesSet := make(map[string]bool, len(excludedFamilies))
	for _, family := range excludedFamilies {
		if family != "" {
			excludedFamiliesSet[family] = true
		}
	}

	// Classify every candidate once, in policy order.
	type scored struct {
		c  candidate
		gi int
	}
	entries := make([]scored, 0)
	for gi, group := range tp.Groups {
		for _, ref := range group {
			entries = append(entries, scored{classify(ref, desired, st, asOf, excludedFamiliesSet), gi})
		}
	}

	// First confirmed group wins, by maximum headroom, stable tie.
	for gi := range tp.Groups {
		var best *candidate
		for _, e := range entries {
			if e.gi != gi || e.c.status != candConfirmed {
				continue
			}
			if best == nil || *e.c.headroom > *best.headroom {
				c := e.c
				best = &c
			}
		}
		if best != nil {
			return buildResult(KindConfirmed, *best, phase, tier), nil
		}
	}
	// No confirmed candidate anywhere: first uncertain candidate in the first
	// group that has one.
	for gi := range tp.Groups {
		for _, e := range entries {
			if e.gi == gi && e.c.status == candUncertain {
				return buildResult(KindUncertain, e.c, phase, tier), nil
			}
		}
	}
	return Result{}, ErrNoCandidate
}

// classify determines one reference's candidacy. Exclusions are evaluated
// before evidence demotions — known-bad state and known-bad snapshot evidence
// disqualify the candidate outright, whatever the snapshot's freshness or
// completeness — and missing or weak evidence can only demote the candidate
// to the uncertain fallback, never rescue a disabled or exhausted provider.
func classify(ref string, desired policy.Desired, st state.State, asOf time.Time, excludedFamilies map[string]bool) candidate {
	c := candidate{ref: ref}
	base, suffix, err := policy.ParseModelRef(ref)
	if err != nil {
		// Unreachable for policy parsed by ParsePolicy; fail closed regardless.
		c.status, c.reason = candExcluded, "invalid reference"
		return c
	}
	c.base, c.suffix = base, suffix
	if excludedFamilies[FamilyOf(base)] {
		c.status, c.reason = candExcluded, "excluded family"
		return c
	}
	mid, err := desired.ResolveModel(base)
	if err != nil {
		c.status, c.reason = candExcluded, "unresolved model"
		return c
	}
	c.mapping = mid
	m, tracked := desired.Providers[mid]
	if !tracked || !m.Models[base].Enabled {
		c.status, c.reason = candExcluded, "baseline disabled"
		return c
	}

	ps, observed := st.Providers[string(mid)]
	var snap, attempt *quota.QuotaSnapshot
	if observed {
		snap, attempt = ps.QuotaSnapshot, ps.QuotaAttempt
		if ps.ManualDisabled {
			c.status, c.reason = candExcluded, "manually disabled"
			return c
		}
		// Unrecognized non-empty axis values are corrupted observations and
		// never enable a provider. Empty legacy axes add no evidence: polling
		// establishes confirmation through the quota snapshot below.
		if ps.Quota != "" && !validStateQuota(ps.Quota) {
			c.status, c.reason = candExcluded, "corrupt provider state"
			return c
		}
		if ps.Availability != "" && !validStateAvailability(ps.Availability) {
			c.status, c.reason = candExcluded, "corrupt provider state"
			return c
		}
		if ps.Availability == state.Unavailable {
			c.status, c.reason = candExcluded, "provider unavailable"
			return c
		}
		if ps.Quota == state.QuotaExhausted {
			c.status, c.reason = candExcluded, "provider exhausted"
			return c
		}
	}

	if snap == nil {
		// A refresh was attempted but produced no usable snapshot; with no
		// attempt at all, evidence is simply missing.
		if attempt != nil {
			c.status, c.reason = candUncertain, EvidenceFailed
		} else {
			c.status, c.reason = candUncertain, EvidenceMissing
		}
		return c
	}

	// Corrupted snapshot enums and impossible window values exclude outright;
	// empty enums are sparse provenance, demoted later.
	if snap.Status != "" && !validSourceStatus(snap.Status) {
		c.status, c.reason = candExcluded, "corrupt snapshot status"
		return c
	}
	if snap.Availability != "" && !validSnapshotAvailability(snap.Availability) {
		c.status, c.reason = candExcluded, "corrupt snapshot availability"
		return c
	}
	for _, w := range snap.Windows {
		if corruptWindow(w) {
			c.status, c.reason = candExcluded, "corrupt quota window"
			return c
		}
	}

	// Known-bad snapshot evidence excludes before any freshness or
	// completeness demotion: staleness or partiality never rescues a provider
	// that reported itself unavailable or out of quota.
	if snap.Availability == quota.QuotaUnavailable {
		c.status, c.reason = candExcluded, "provider reported unavailable"
		return c
	}
	if rem := snap.EffectiveRemaining(); rem != nil && *rem <= 0 {
		c.status, c.reason = candExcluded, "quota exhausted"
		return c
	}

	// Timestamps are provenance: a snapshot with no stamp, or one stamped
	// after the reference time, is inconsistent evidence and is treated
	// conservatively — never confirmed.
	if snap.CheckedAt.IsZero() {
		c.status, c.reason = candUncertain, EvidenceUnknown
		return c
	}
	if snap.CheckedAt.After(asOf) {
		c.status, c.reason = candUncertain, EvidenceUnknown
		return c
	}

	ttl, confirmable := confirmTTL(&m)
	if !confirmable {
		// The mapping has no quota configuration, so no snapshot can ever be
		// confirmed fresh against a configured TTL.
		c.status, c.reason = candUncertain, EvidenceUnknown
		return c
	}
	if asOf.Sub(snap.CheckedAt) > ttl {
		c.status, c.reason = candUncertain, EvidenceStale
		return c
	}
	if snap.Status == quota.SourceFailed {
		c.status, c.reason = candUncertain, EvidenceFailed
		return c
	}
	if snap.Status == quota.SourcePartial {
		c.status, c.reason = candUncertain, EvidencePartial
		return c
	}
	if snap.Status == "" {
		// No recorded source status: not the exact successful status, so the
		// snapshot can never confirm.
		c.status, c.reason = candUncertain, EvidenceUnknown
		return c
	}
	// Fail closed on anything but an explicit "available": unknown and empty
	// are unusable data — demoted to uncertain, never confirmed.
	if snap.Availability != quota.QuotaAvailable {
		c.status, c.reason = candUncertain, EvidenceUnknown
		return c
	}
	rem := snap.EffectiveRemaining()
	if rem == nil {
		c.status, c.reason = candUncertain, EvidenceSparse
		return c
	}
	c.headroom = rem
	c.status, c.reason = candConfirmed, "available quota"
	return c
}

// validStateQuota reports whether q is a supported explicit provider quota
// value. The empty value is handled separately as the legacy sparse state.
func validStateQuota(q state.Quota) bool {
	switch q {
	case state.QuotaNormal, state.QuotaLow, state.QuotaExhausted:
		return true
	}
	return false
}

// validStateAvailability reports whether a is a supported explicit provider
// availability value. The empty value is handled separately as the legacy
// sparse state.
func validStateAvailability(a state.Availability) bool {
	switch a {
	case state.Available, state.Unavailable:
		return true
	}
	return false
}

// validSourceStatus reports whether s is a recognized snapshot source status.
// The empty value is handled separately as absent provenance.
func validSourceStatus(s quota.SourceStatus) bool {
	switch s {
	case quota.SourceFresh, quota.SourcePartial, quota.SourceFailed:
		return true
	}
	return false
}

// validSnapshotAvailability reports whether a is a recognized snapshot
// availability value. The empty value is handled separately as absent
// provenance.
func validSnapshotAvailability(a quota.QuotaAvailability) bool {
	switch a {
	case quota.QuotaAvailable, quota.QuotaUnavailable, quota.QuotaUnknown:
		return true
	}
	return false
}

// corruptWindow reports impossible window observations: a negative used or
// limit value, a usage percentage outside 0..100, or a NaN or ±Inf value in
// any numeric pointer field. NaN defeats every ordered comparison, so it
// would slip past the range checks and flow through Remaining()'s clamp into
// a NaN headroom that no JSON result can represent; infinities distort the
// clamped remaining fraction the same way. Such windows must never enable a
// candidate. (The quota package itself is unchanged: this is a
// selection-side fail-closed classification.)
func corruptWindow(w quota.QuotaWindow) bool {
	if w.Used != nil && (math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) || *w.Used < 0) {
		return true
	}
	if w.Limit != nil && (math.IsNaN(*w.Limit) || math.IsInf(*w.Limit, 0) || *w.Limit < 0) {
		return true
	}
	if w.UsagePercent != nil && (math.IsNaN(*w.UsagePercent) || math.IsInf(*w.UsagePercent, 0) || *w.UsagePercent < 0 || *w.UsagePercent > 100) {
		return true
	}
	return false
}

// confirmTTL returns the freshness TTL a mapping's snapshot must satisfy to
// confirm a selection, and whether the mapping is confirmable at all: a
// mapping without a quota configuration has no configured TTL and is never
// confirmable. A configured non-positive TTL falls back to the policy
// package's DefaultQuotaFreshness (30 minutes, the routing default) — the
// single shared freshness bound, not a selection-local copy.
func confirmTTL(m *policy.Mapping) (time.Duration, bool) {
	if m == nil || m.Quota == nil {
		return 0, false
	}
	ttl := m.Quota.FreshnessTTL
	if ttl <= 0 {
		ttl = policy.DefaultQuotaFreshness
	}
	return ttl, true
}

// buildResult renders one classified candidate as a version-1 Result.
func buildResult(kind Kind, c candidate, phase string, tier Tier) Result {
	r := Result{
		Version:   SelectionVersion,
		Phase:     phase,
		Tier:      string(tier),
		Reference: c.ref,
		Base:      c.base,
		Suffix:    c.suffix,
		Mapping:   string(c.mapping),
		Kind:      kind,
	}
	if kind == KindConfirmed {
		r.Headroom = c.headroom
	} else {
		r.Evidence = c.reason
	}
	return r
}
