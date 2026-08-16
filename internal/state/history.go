package state

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

const (
	HistoryRecordLimit        = 100
	HistoryTargetsPerRecord   = 64
	HistoryProvidersPerRecord = 64
	HistoryRanksPerRecord     = 64
	HistoryChainsPerTarget    = 128
	HistoryEntriesPerChain    = 64
	HistoryEditsPerTarget     = 256
	HistoryFieldPathDepth     = 8
	HistoryPathComponentBytes = 128
	HistoryPolicyPathBytes    = 512
	HistoryIdentifierBytes    = 128
	HistoryFreeTextBytes      = 512
	HistoryRecordEncodedBytes = 256 * 1024
	HistoryEncodedBytes       = 4 * 1024 * 1024
)

type TriggerKind string

const (
	TriggerInit           TriggerKind = "init"
	TriggerHook           TriggerKind = "hook"
	TriggerReconcile      TriggerKind = "reconcile"
	TriggerRoutingDisable TriggerKind = "routing-disable"
	TriggerRoutingEnable  TriggerKind = "routing-enable"
	TriggerRoutingReset   TriggerKind = "routing-reset"
	TriggerQuotaCheck     TriggerKind = "quota-check"
	TriggerSet            TriggerKind = "set"
	TriggerClear          TriggerKind = "clear"
)

type HookEventKind string

const (
	HookQuotaLow            HookEventKind = "quota_low"
	HookQuotaReached        HookEventKind = "quota_reached"
	HookQuotaReset          HookEventKind = "quota_reset"
	HookProviderUnavailable HookEventKind = "provider_unavailable"
	HookProviderRecovered   HookEventKind = "provider_recovered"
	HookRefreshFailed       HookEventKind = "refresh_failed"
)

type TargetOutcomeKind string

const (
	OutcomeApplied TargetOutcomeKind = "applied"
	OutcomePending TargetOutcomeKind = "pending"
)

type PendingStage string

const (
	PendingRender         PendingStage = "render"
	PendingStageBuild     PendingStage = "stage"
	PendingConfigValidate PendingStage = "config_validate"
	PendingDoctor         PendingStage = "doctor"
	PendingPublishPrepare PendingStage = "publish-prepare"
	PendingPublish        PendingStage = "publish"
	PendingResolveTargets PendingStage = "resolve-targets"
)

type EditAction string

const (
	EditSetScalar   EditAction = "set-scalar"
	EditSetSequence EditAction = "set-sequence"
	EditSetBool     EditAction = "set-bool"
	EditRemove      EditAction = "remove"
)

type HistoryTier string

const (
	TierFull      HistoryTier = "full"
	TierAggregate HistoryTier = "aggregate"
)

type HookEvidence struct {
	Event        HookEventKind
	Provider     string
	Timestamp    time.Time
	Window       *string    `json:",omitempty"`
	UsagePercent *float64   `json:",omitempty"`
	Used         *float64   `json:",omitempty"`
	Limit        *float64   `json:",omitempty"`
	ResetAt      *time.Time `json:",omitempty"`
	Status       *string    `json:",omitempty"`
}

type SetEvidence struct {
	Provider     string
	Quota        *Quota        `json:",omitempty"`
	Availability *Availability `json:",omitempty"`
}
type ClearEvidence struct {
	Provider string `json:",omitempty"`
	All      bool
}
type Trigger struct {
	Kind      TriggerKind
	Hook      *HookEvidence  `json:",omitempty"`
	MappingID string         `json:",omitempty"`
	Set       *SetEvidence   `json:",omitempty"`
	Clear     *ClearEvidence `json:",omitempty"`
}

type ProviderDetail struct {
	MappingID string
	Mode      Mode
	Reason    string
}
type RankDetail struct {
	MappingID   string
	Rank        int
	OffPeak     bool
	Eligible    bool
	Explanation string
}
type ChainOmittedCounts struct {
	Desired   int
	Effective int
	Dropped   int
}
type ChainDetail struct {
	Name      string
	Desired   []string `json:",omitempty"`
	Effective []string `json:",omitempty"`
	Dropped   []string `json:",omitempty"`
	Omitted   ChainOmittedCounts
}
type EditDetail struct {
	File   string
	Path   []string
	Action EditAction
	Detail string
}
type PendingDetail struct {
	Stage       PendingStage
	Summary     string
	Remediation string
}
type TargetOmittedCounts struct {
	Chains int
	Edits  int
}

// TargetDetail carries planned detail plus runtime outcome when finalized.
// Outcome and Pending are excluded from RecordTemplate JSON so templates have no
// runtime publication outcome on the wire.
type TargetDetail struct {
	ID           string
	PlanComputed bool              `json:"-"`
	Outcome      TargetOutcomeKind `json:",omitempty"`
	Pending      *PendingDetail    `json:",omitempty"`
	Chains       []ChainDetail     `json:",omitempty"`
	Edits        []EditDetail      `json:",omitempty"`
	Omitted      TargetOmittedCounts
}

// TemplateTarget is the plan-only target representation. Outcome and Pending are
// retained only for compatibility with callers that build a template before
// finalization; they are runtime fields and are never serialized as template data.
type TemplateTarget struct {
	ID           string
	PlanComputed bool              `json:"-"`
	Outcome      TargetOutcomeKind `json:"-"`
	Pending      *PendingDetail    `json:"-"`
	Chains       []ChainDetail     `json:",omitempty"`
	Edits        []EditDetail      `json:",omitempty"`
	Omitted      TargetOmittedCounts
}
type CompactTarget struct {
	ID      string
	Outcome TargetOutcomeKind
	Pending *PendingDetail `json:",omitempty"`
}
type RecordOmittedCounts struct {
	Providers    int
	Ranks        int
	Chains       int
	ChainEntries int
	Edits        int
}
type AuthoritativeTargetCounts struct {
	Total   int
	Applied int
	Pending int
	Omitted int
}
type RecordTemplate struct {
	Revision  uint64
	Trigger   Trigger
	Providers []ProviderDetail `json:",omitempty"`
	Ranks     []RankDetail     `json:",omitempty"`
	Targets   []TemplateTarget `json:",omitempty"`
	Omitted   RecordOmittedCounts
}
type ReconcileRecord struct {
	Revision        uint64
	CompletedAt     time.Time
	Trigger         Trigger
	Tier            HistoryTier
	Counts          AuthoritativeTargetCounts
	DetailTruncated bool             `json:",omitempty"`
	Providers       []ProviderDetail `json:",omitempty"`
	Ranks           []RankDetail     `json:",omitempty"`
	Targets         []TargetDetail   `json:",omitempty"`
	CompactTargets  []CompactTarget  `json:",omitempty"`
	Omitted         RecordOmittedCounts
}

// EventCategory identifies the source of a meaningful timeline event.
type EventCategory string

const (
	EventHook          EventCategory = "hook"
	EventManual        EventCategory = "manual"
	EventQuotaFailure  EventCategory = "quota-failure"
	EventRoutingChange EventCategory = "routing-change"
	EventNotice        EventCategory = "notice"
)

// EventResult describes whether an event changed state or was intentionally
// ignored/no-op. It is bounded and safe for human/JSON projections.
type EventResult string

const (
	EventChanged  EventResult = "changed"
	EventIgnored  EventResult = "ignored"
	EventFailed   EventResult = "failed"
	EventNoChange EventResult = "no-change"
)

// EventRecord is the durable, sanitized representation of one meaningful
// provider or routing event. Reconcile evidence is intentionally bounded and
// represented as summaries rather than raw source content.
type EventRecord struct {
	Sequence        uint64
	ArrivalSequence uint64 `json:",omitempty"`
	Revision        uint64
	Ordinal         int
	At              time.Time
	RecordedAt      time.Time
	Category        EventCategory
	Action          string
	Provider        string `json:",omitempty"`
	MappingID       string `json:",omitempty"`
	Result          EventResult
	Reason          string       `json:",omitempty"`
	BeforeQuota     Quota        `json:",omitempty"`
	AfterQuota      Quota        `json:",omitempty"`
	BeforeAvail     Availability `json:",omitempty"`
	AfterAvail      Availability `json:",omitempty"`
	BeforeMode      Mode         `json:",omitempty"`
	AfterMode       Mode         `json:",omitempty"`
	UsagePercent    *float64     `json:",omitempty"`
	Used            *float64     `json:",omitempty"`
	Limit           *float64     `json:",omitempty"`
	ResetAt         *time.Time   `json:",omitempty"`
	Status          string       `json:",omitempty"`
	OldRank         *int         `json:",omitempty"`
	NewRank         *int         `json:",omitempty"`
	OldEligible     *bool        `json:",omitempty"`
	NewEligible     *bool        `json:",omitempty"`
	OldOffPeak      *bool        `json:",omitempty"`
	NewOffPeak      *bool        `json:",omitempty"`
	Explanation     string       `json:",omitempty"`
	Applied         int          `json:",omitempty"`
	Pending         int          `json:",omitempty"`
	Changes         []string     `json:",omitempty"`
}

// EventHistory is the bounded newest-first meaningful event timeline.
type EventHistory struct {
	Events        []EventRecord
	OmittedEvents int
}

// ReconcileHistory remains an in-memory compatibility type for callers being
// migrated to EventHistory. It is not persisted by the schema-5 state store.
type ReconcileHistory struct {
	Records               []ReconcileRecord
	OmittedHistoryRecords int
}

// FinalizeHistoryRecord attaches runtime outcomes to a plan template and then
// applies the deterministic full/aggregate projection and record ceiling.
func FinalizeHistoryRecord(input RecordTemplate, outcomes []CompactTarget, completedAt time.Time) (ReconcileRecord, error) {
	if len(outcomes) != len(input.Targets) {
		return ReconcileRecord{}, fmt.Errorf("state: history outcome count does not match template targets")
	}
	byID := make(map[string]CompactTarget, len(outcomes))
	for _, outcome := range outcomes {
		outcome.ID = sanitizeIdentifier(outcome.ID)
		outcome.Pending = sanitizePending(outcome.Pending)
		if _, exists := byID[outcome.ID]; exists {
			return ReconcileRecord{}, fmt.Errorf("state: duplicate history target outcome")
		}
		byID[outcome.ID] = outcome
	}
	input = SanitizeRecordTemplate(input)
	final := make([]TargetDetail, 0, len(input.Targets))
	for _, target := range input.Targets {
		outcome, ok := byID[target.ID]
		if !ok {
			return ReconcileRecord{}, fmt.Errorf("state: missing history target outcome")
		}
		final = append(final, TargetDetail{ID: target.ID, Outcome: outcome.Outcome, Pending: copyPending(outcome.Pending), Chains: target.Chains, Edits: target.Edits, Omitted: target.Omitted})
	}
	return projectHistoryRecord(input, final, completedAt)
}

// ProjectHistoryRecord is retained as a convenience for callers that already
// have runtime-bearing target details; new callers should use FinalizeHistoryRecord.
func ProjectHistoryRecord(input RecordTemplate, completedAt time.Time) (ReconcileRecord, error) {
	bounded := SanitizeRecordTemplate(input)
	return projectHistoryRecord(bounded, targetDetailsFromTemplate(bounded.Targets), completedAt)
}

func projectHistoryRecord(input RecordTemplate, targets []TargetDetail, completedAt time.Time) (ReconcileRecord, error) {
	if input.Revision == 0 || completedAt.IsZero() {
		return ReconcileRecord{}, fmt.Errorf("state: history revision and completion time are required")
	}
	input = SanitizeRecordTemplate(input)
	completedAt = completedAt.UTC()
	if err := ValidateRecordTemplate(input); err != nil {
		return ReconcileRecord{}, err
	}
	counts := countTargets(targets)
	full := ReconcileRecord{Revision: input.Revision, CompletedAt: completedAt, Trigger: input.Trigger, Tier: TierFull, Counts: counts, Providers: input.Providers, Ranks: input.Ranks, Targets: targets, Omitted: input.Omitted}
	b, err := json.Marshal(full)
	if err != nil {
		return ReconcileRecord{}, err
	}
	if len(targets) <= HistoryTargetsPerRecord && len(b) <= HistoryRecordEncodedBytes {
		if err := ValidateHistoryRecord(full); err != nil {
			return ReconcileRecord{}, err
		}
		return full, nil
	}
	agg := aggregateRecord(full)
	if err := ValidateHistoryRecord(agg); err != nil {
		return ReconcileRecord{}, err
	}
	return agg, nil
}

func targetDetailsFromTemplate(targets []TemplateTarget) []TargetDetail {
	out := make([]TargetDetail, len(targets))
	for i, target := range targets {
		outcome := target.Outcome
		if outcome == "" {
			outcome = OutcomeApplied
		}
		out[i] = TargetDetail{ID: target.ID, Chains: target.Chains, Edits: target.Edits, Omitted: target.Omitted, Outcome: outcome, Pending: copyPending(target.Pending)}
	}
	return out
}

func countTargets(targets []TargetDetail) AuthoritativeTargetCounts {
	c := AuthoritativeTargetCounts{Total: len(targets)}
	for _, t := range targets {
		if t.Outcome == OutcomeApplied {
			c.Applied++
		} else if t.Outcome == OutcomePending {
			c.Pending++
		}
	}
	return c
}

func aggregateRecord(r ReconcileRecord) ReconcileRecord {
	all := make([]CompactTarget, 0, len(r.Targets))
	for _, t := range r.Targets {
		all = append(all, CompactTarget{ID: t.ID, Outcome: t.Outcome, Pending: copyPending(t.Pending)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	retained := all
	if len(retained) > HistoryTargetsPerRecord {
		retained = retained[:HistoryTargetsPerRecord]
	}
	counts := r.Counts
	counts.Omitted = counts.Total - len(retained)
	return ReconcileRecord{Revision: r.Revision, CompletedAt: r.CompletedAt, Trigger: r.Trigger, Tier: TierAggregate, Counts: counts, DetailTruncated: true, CompactTargets: retained, Omitted: allOmitted(r)}
}

func allOmitted(r ReconcileRecord) RecordOmittedCounts {
	o := r.Omitted
	o.Providers += len(r.Providers)
	o.Ranks += len(r.Ranks)
	for _, t := range r.Targets {
		o.Chains += len(t.Chains) + t.Omitted.Chains
		o.Edits += len(t.Edits) + t.Omitted.Edits
		for _, c := range t.Chains {
			o.ChainEntries += len(c.Desired) + len(c.Effective) + len(c.Dropped) + c.Omitted.Desired + c.Omitted.Effective + c.Omitted.Dropped
		}
	}
	return o
}

func SanitizeRecordTemplate(in RecordTemplate) RecordTemplate {
	out := RecordTemplate{Revision: in.Revision, Trigger: sanitizeTrigger(in.Trigger), Omitted: in.Omitted}
	providers := append([]ProviderDetail(nil), in.Providers...)
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].MappingID < providers[j].MappingID })
	if len(providers) > HistoryProvidersPerRecord {
		out.Omitted.Providers += len(providers) - HistoryProvidersPerRecord
		providers = providers[:HistoryProvidersPerRecord]
	}
	for _, p := range providers {
		p.MappingID = sanitizeIdentifier(p.MappingID)
		p.Reason = sanitizeText(p.Reason)
		out.Providers = append(out.Providers, p)
	}
	ranks := append([]RankDetail(nil), in.Ranks...)
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].Rank != ranks[j].Rank {
			return ranks[i].Rank < ranks[j].Rank
		}
		return ranks[i].MappingID < ranks[j].MappingID
	})
	if len(ranks) > HistoryRanksPerRecord {
		out.Omitted.Ranks += len(ranks) - HistoryRanksPerRecord
		ranks = ranks[:HistoryRanksPerRecord]
	}
	for _, r := range ranks {
		r.MappingID = sanitizeIdentifier(r.MappingID)
		r.Explanation = sanitizeText(r.Explanation)
		out.Ranks = append(out.Ranks, r)
	}
	targets := append([]TemplateTarget(nil), in.Targets...)
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	for _, t := range targets {
		out.Targets = append(out.Targets, sanitizeTemplateTarget(t))
	}
	return out
}

func sanitizeTemplateTarget(t TemplateTarget) TemplateTarget {
	bounded := sanitizeTarget(TargetDetail{ID: t.ID, Outcome: t.Outcome, Pending: t.Pending, Chains: t.Chains, Edits: t.Edits, Omitted: t.Omitted})
	return TemplateTarget{ID: bounded.ID, PlanComputed: t.PlanComputed, Outcome: bounded.Outcome, Pending: bounded.Pending, Chains: bounded.Chains, Edits: bounded.Edits, Omitted: bounded.Omitted}
}

func sanitizeTarget(t TargetDetail) TargetDetail {
	out := TargetDetail{ID: sanitizeIdentifier(t.ID), Outcome: t.Outcome, Pending: sanitizePending(t.Pending), Omitted: t.Omitted}
	chains := append([]ChainDetail(nil), t.Chains...)
	sort.SliceStable(chains, func(i, j int) bool { return chains[i].Name < chains[j].Name })
	if len(chains) > HistoryChainsPerTarget {
		out.Omitted.Chains += len(chains) - HistoryChainsPerTarget
		chains = chains[:HistoryChainsPerTarget]
	}
	for _, c := range chains {
		c.Name = sanitizeIdentifier(c.Name)
		c.Desired = boundIDs(c.Desired, &c.Omitted.Desired)
		c.Effective = boundIDs(c.Effective, &c.Omitted.Effective)
		c.Dropped = boundIDs(c.Dropped, &c.Omitted.Dropped)
		out.Chains = append(out.Chains, c)
	}
	edits := append([]EditDetail(nil), t.Edits...)
	sort.SliceStable(edits, func(i, j int) bool {
		a, b := edits[i].File+"\x00"+strings.Join(edits[i].Path, "\x00"), edits[j].File+"\x00"+strings.Join(edits[j].Path, "\x00")
		return a < b
	})
	if len(edits) > HistoryEditsPerTarget {
		out.Omitted.Edits += len(edits) - HistoryEditsPerTarget
		edits = edits[:HistoryEditsPerTarget]
	}
	for _, e := range edits {
		e.Path = append([]string(nil), e.Path...)
		e.Detail = sanitizeText(e.Detail)
		out.Edits = append(out.Edits, e)
	}
	return out
}
func boundIDs(in []string, omitted *int) []string {
	if len(in) > HistoryEntriesPerChain {
		*omitted += len(in) - HistoryEntriesPerChain
		in = in[:HistoryEntriesPerChain]
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = sanitizeIdentifier(s)
	}
	return out
}
func sanitizePending(p *PendingDetail) *PendingDetail {
	if p == nil {
		return nil
	}
	return &PendingDetail{Stage: p.Stage, Summary: sanitizeText(p.Summary), Remediation: sanitizeText(p.Remediation)}
}
func sanitizeTrigger(t Trigger) Trigger {
	out := t
	if t.Hook != nil {
		h := *t.Hook
		h.Provider = sanitizeIdentifier(h.Provider)
		h.Timestamp = h.Timestamp.UTC()
		if h.ResetAt != nil {
			x := h.ResetAt.UTC()
			h.ResetAt = &x
		}
		if h.Window != nil {
			x := sanitizeText(*h.Window)
			h.Window = &x
		}
		if h.Status != nil {
			x := sanitizeText(*h.Status)
			h.Status = &x
		}
		out.Hook = &h
	}
	out.MappingID = sanitizeIdentifierOptional(t.MappingID)
	if t.Set != nil {
		x := *t.Set
		x.Provider = sanitizeIdentifier(x.Provider)
		out.Set = &x
	}
	if t.Clear != nil {
		x := *t.Clear
		x.Provider = sanitizeIdentifierOptional(x.Provider)
		out.Clear = &x
	}
	return out
}
func sanitizeText(s string) string { return truncateUTF8(quota.SanitizeText(s), HistoryFreeTextBytes) }
func sanitizeIdentifierOptional(s string) string {
	if s == "" {
		return ""
	}
	return sanitizeIdentifier(s)
}
// unsafeIdentifier is the redaction sentinel emitted by sanitizeIdentifier when
// a required identifier is empty, invalid UTF-8, or contains control characters.
// It is never a real provider or mapping identifier.
const unsafeIdentifier = "unsafe-id"

func sanitizeIdentifier(s string) string {
	s = quota.SanitizeText(s)
	if !utf8.ValidString(s) || s == "" || hasControl(s) {
		return unsafeIdentifier
	}
	return truncateUTF8(s, HistoryIdentifierBytes)
}

// sanitizeEventProvider sanitizes the optional Provider field of an event
// record. Unlike a required identifier, an empty provider is valid:
// routing_changed events carry their identity in MappingID, and the display
// falls back from an empty Provider to MappingID. Because the redaction
// sentinel is never a real provider name, any occurrence — including pre-fix
// records that baked it into this optional field — is healed to empty so the
// MappingID fallback works.
func sanitizeEventProvider(s string) string {
	out := sanitizeIdentifierOptional(s)
	if out == unsafeIdentifier {
		return ""
	}
	return out
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func hasControl(s string) bool {
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func ValidateRecordTemplate(t RecordTemplate) error {
	if t.Revision == 0 {
		return fmt.Errorf("state: history template revision is required")
	}
	if err := validateTrigger(t.Trigger); err != nil {
		return err
	}
	if len(t.Providers) > HistoryProvidersPerRecord || len(t.Ranks) > HistoryRanksPerRecord {
		return fmt.Errorf("state: history template shared detail over limit")
	}
	for _, p := range t.Providers {
		if err := validID(p.MappingID); err != nil {
			return err
		}
		if !validMode(p.Mode) {
			return fmt.Errorf("state: history invalid provider mode")
		}
		if err := validText(p.Reason); err != nil {
			return err
		}
	}
	for _, r := range t.Ranks {
		if err := validID(r.MappingID); err != nil {
			return err
		}
		if r.Rank < 0 {
			return fmt.Errorf("state: history invalid rank")
		}
		if err := validText(r.Explanation); err != nil {
			return err
		}
	}
	for _, x := range t.Targets {
		if err := validateTemplateTarget(x); err != nil {
			return err
		}
	}
	return validOmitted(t.Omitted)
}

func ValidateHistoryRecord(r ReconcileRecord) error {
	if r.Revision == 0 {
		return fmt.Errorf("state: history revision is required")
	}
	if err := validRequiredUTC(r.CompletedAt); err != nil {
		return err
	}
	if err := validateTrigger(r.Trigger); err != nil {
		return err
	}
	if r.Tier != TierFull && r.Tier != TierAggregate {
		return fmt.Errorf("state: history invalid tier %q", r.Tier)
	}
	if err := validateCounts(r.Counts); err != nil {
		return err
	}
	if err := validOmitted(r.Omitted); err != nil {
		return err
	}
	if r.Tier == TierFull {
		if r.DetailTruncated || len(r.CompactTargets) != 0 || len(r.Targets) > HistoryTargetsPerRecord || len(r.Providers) > HistoryProvidersPerRecord || len(r.Ranks) > HistoryRanksPerRecord || r.Counts.Total != len(r.Targets) || r.Counts.Omitted != 0 {
			return fmt.Errorf("state: invalid full history structure")
		}
		actual := countTargets(r.Targets)
		if actual.Applied != r.Counts.Applied || actual.Pending != r.Counts.Pending {
			return fmt.Errorf("state: full history target counts do not match outcomes")
		}
		if err := validateRecordDetails(r.Targets); err != nil {
			return err
		}
		for _, p := range r.Providers {
			if err := validateProviderDetail(p); err != nil {
				return err
			}
		}
		for _, rank := range r.Ranks {
			if err := validateRankDetail(rank); err != nil {
				return err
			}
		}
	} else {
		if !r.DetailTruncated || len(r.Providers) != 0 || len(r.Ranks) != 0 || len(r.Targets) != 0 || len(r.CompactTargets) > HistoryTargetsPerRecord || r.Counts.Omitted != r.Counts.Total-len(r.CompactTargets) {
			return fmt.Errorf("state: invalid aggregate history structure")
		}
		compactCounts := AuthoritativeTargetCounts{Total: len(r.CompactTargets)}
		for _, t := range r.CompactTargets {
			if err := validID(t.ID); err != nil {
				return err
			}
			if err := validateOutcome(t.Outcome, t.Pending); err != nil {
				return err
			}
			if t.Outcome == OutcomeApplied {
				compactCounts.Applied++
			} else {
				compactCounts.Pending++
			}
		}
		if compactCounts.Applied > r.Counts.Applied || compactCounts.Pending > r.Counts.Pending {
			return fmt.Errorf("state: aggregate target counts exceed authoritative counts")
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > HistoryRecordEncodedBytes {
		return fmt.Errorf("state: history record exceeds encoded limit")
	}
	return nil
}

func ValidateEventHistory(h EventHistory) error {
	if h.OmittedEvents < 0 || len(h.Events) > EventHistoryLimit {
		return fmt.Errorf("state: invalid event history count")
	}
	seen := make(map[uint64]bool, len(h.Events))
	for i, e := range h.Events {
		if e.Sequence == 0 || seen[e.Sequence] {
			return fmt.Errorf("state: invalid or duplicate event sequence at %d", i)
		}
		seen[e.Sequence] = true
		if e.Revision == 0 || e.Ordinal < 0 || e.At.IsZero() || e.RecordedAt.IsZero() || e.At.Location() != time.UTC || e.RecordedAt.Location() != time.UTC {
			return fmt.Errorf("state: invalid event metadata at %d", i)
		}
		for name, value := range map[string]*float64{"usage_percent": e.UsagePercent, "used": e.Used, "limit": e.Limit} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
				return fmt.Errorf("state: invalid event %s at %d", name, i)
			}
		}
		if e.Result != EventChanged && e.Result != EventIgnored && e.Result != EventFailed && e.Result != EventNoChange {
			return fmt.Errorf("state: invalid event result %q", e.Result)
		}
		if e.Category != EventHook && e.Category != EventManual && e.Category != EventQuotaFailure && e.Category != EventRoutingChange && e.Category != EventNotice {
			return fmt.Errorf("state: invalid event category %q", e.Category)
		}
	}
	return nil
}

func SanitizeEventHistory(h EventHistory) EventHistory {
	out := EventHistory{OmittedEvents: h.OmittedEvents, Events: make([]EventRecord, len(h.Events))}
	for i, e := range h.Events {
		e.Provider = sanitizeEventProvider(e.Provider)
		e.MappingID = sanitizeIdentifier(e.MappingID)
		e.Action = sanitizeText(e.Action)
		e.Reason = sanitizeText(e.Reason)
		e.Status = sanitizeText(e.Status)
		e.Explanation = sanitizeText(e.Explanation)
		if len(e.Changes) > HistoryEditsPerTarget {
			e.Changes = e.Changes[:HistoryEditsPerTarget]
		}
		if e.Changes != nil {
			changes := make([]string, len(e.Changes))
			for j, change := range e.Changes {
				changes[j] = sanitizeText(change)
			}
			e.Changes = changes
		}
		out.Events[i] = e
	}
	return out
}

func BoundEventHistory(h EventHistory) (EventHistory, error) {
	if len(h.Events) > EventHistoryLimit {
		h.OmittedEvents += len(h.Events) - EventHistoryLimit
		h.Events = h.Events[:EventHistoryLimit]
	}
	for {
		b, err := json.Marshal(h)
		if err != nil {
			return EventHistory{}, err
		}
		if len(b) <= EventHistoryEncodedBytes {
			return h, nil
		}
		if len(h.Events) == 0 {
			return h, nil
		}
		h.Events = h.Events[:len(h.Events)-1]
		h.OmittedEvents++
	}
}

func AppendEvent(h EventHistory, e EventRecord) (EventHistory, error) {
	out := EventHistory{OmittedEvents: h.OmittedEvents, Events: make([]EventRecord, 0, len(h.Events)+1)}
	out.Events = append(out.Events, e)
	out.Events = append(out.Events, h.Events...)
	return BoundEventHistory(SanitizeEventHistory(out))
}

func ValidateReconcileHistory(h ReconcileHistory) error {
	if h.OmittedHistoryRecords < 0 || len(h.Records) > HistoryRecordLimit {
		return fmt.Errorf("state: invalid history count")
	}
	var prior uint64 = ^uint64(0)
	for _, r := range h.Records {
		if err := ValidateHistoryRecord(r); err != nil {
			return err
		}
		if r.Revision >= prior {
			return fmt.Errorf("state: history is not newest-first")
		}
		prior = r.Revision
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(b) > HistoryEncodedBytes {
		return fmt.Errorf("state: history exceeds encoded limit")
	}
	return nil
}

func validateTrigger(t Trigger) error {
	valid := map[TriggerKind]bool{TriggerInit: true, TriggerHook: true, TriggerReconcile: true, TriggerRoutingDisable: true, TriggerRoutingEnable: true, TriggerRoutingReset: true, TriggerQuotaCheck: true, TriggerSet: true, TriggerClear: true}
	if !valid[t.Kind] {
		return fmt.Errorf("state: invalid history trigger")
	}
	switch t.Kind {
	case TriggerHook:
		if t.Hook == nil || t.MappingID != "" || t.Set != nil || t.Clear != nil {
			return fmt.Errorf("state: invalid hook trigger")
		}
		h := t.Hook
		events := map[HookEventKind]bool{HookQuotaLow: true, HookQuotaReached: true, HookQuotaReset: true, HookProviderUnavailable: true, HookProviderRecovered: true, HookRefreshFailed: true}
		if !events[h.Event] {
			return fmt.Errorf("state: invalid hook event")
		}
		if err := validID(h.Provider); err != nil {
			return err
		}
		if err := validRequiredUTC(h.Timestamp); err != nil {
			return err
		}
		if h.ResetAt != nil {
			if err := validRequiredUTC(*h.ResetAt); err != nil {
				return err
			}
		}
		if h.UsagePercent != nil && (!finite(*h.UsagePercent) || *h.UsagePercent < 0 || *h.UsagePercent > 1) {
			return fmt.Errorf("state: invalid hook usage")
		}
		for _, v := range []*float64{h.Used, h.Limit} {
			if v != nil && (!finite(*v) || *v < 0) {
				return fmt.Errorf("state: invalid hook numeric")
			}
		}
		for _, v := range []*string{h.Window, h.Status} {
			if v != nil {
				if err := validText(*v); err != nil {
					return err
				}
			}
		}
	case TriggerRoutingDisable, TriggerRoutingEnable:
		if t.MappingID == "" || t.Hook != nil || t.Set != nil || t.Clear != nil {
			return fmt.Errorf("state: invalid mapping trigger")
		}
		if t.MappingID != "all" {
			return validID(t.MappingID)
		}
	case TriggerQuotaCheck:
		// A quota check targets all providers when no filter is supplied (empty
		// MappingID, the common `check --reconcile` case) or a single mapping
		// when --provider is given. It carries no hook/set/clear evidence.
		if t.Hook != nil || t.Set != nil || t.Clear != nil {
			return fmt.Errorf("state: invalid quota-check trigger")
		}
		if t.MappingID != "" {
			return validID(t.MappingID)
		}
	case TriggerSet:
		if t.Set == nil || t.Hook != nil || t.MappingID != "" || t.Clear != nil {
			return fmt.Errorf("state: invalid set trigger")
		}
		if err := validID(t.Set.Provider); err != nil {
			return err
		}
		if t.Set.Quota == nil && t.Set.Availability == nil {
			return fmt.Errorf("state: empty set trigger")
		}
		if t.Set.Quota != nil && *t.Set.Quota != QuotaNormal && *t.Set.Quota != QuotaLow && *t.Set.Quota != QuotaExhausted {
			return fmt.Errorf("state: invalid quota")
		}
		if t.Set.Availability != nil && *t.Set.Availability != Available && *t.Set.Availability != Unavailable {
			return fmt.Errorf("state: invalid availability")
		}
	case TriggerClear:
		if t.Clear == nil || t.Hook != nil || t.MappingID != "" || t.Set != nil || t.Clear.All == (t.Clear.Provider != "") {
			return fmt.Errorf("state: invalid clear trigger")
		}
		if !t.Clear.All {
			return validID(t.Clear.Provider)
		}
	default:
		if t.Hook != nil || t.MappingID != "" || t.Set != nil || t.Clear != nil {
			return fmt.Errorf("state: trigger has irrelevant evidence")
		}
	}
	return nil
}
func validateTemplateTarget(t TemplateTarget) error {
	return validateTargetDetails(t.ID, t.Chains, t.Edits, t.Omitted)
}

func validateRecordDetails(targets []TargetDetail) error {
	for _, t := range targets {
		if err := validateTarget(t); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderDetail(p ProviderDetail) error {
	if err := validID(p.MappingID); err != nil {
		return err
	}
	if !validMode(p.Mode) {
		return fmt.Errorf("state: history invalid provider mode")
	}
	return validText(p.Reason)
}

func validateRankDetail(r RankDetail) error {
	if err := validID(r.MappingID); err != nil {
		return err
	}
	if r.Rank < 0 {
		return fmt.Errorf("state: history invalid rank")
	}
	return validText(r.Explanation)
}

func validateTarget(t TargetDetail) error {
	if err := validateTargetDetails(t.ID, t.Chains, t.Edits, t.Omitted); err != nil {
		return err
	}
	return validateOutcome(t.Outcome, t.Pending)
}

func validateTargetDetails(id string, chains []ChainDetail, edits []EditDetail, omitted TargetOmittedCounts) error {
	if err := validID(id); err != nil {
		return err
	}
	if len(chains) > HistoryChainsPerTarget || len(edits) > HistoryEditsPerTarget {
		return fmt.Errorf("state: target detail over limit")
	}
	if err := validTargetOmitted(omitted); err != nil {
		return err
	}
	for _, c := range chains {
		if err := validChain(c); err != nil {
			return err
		}
	}
	for _, e := range edits {
		if err := validEdit(e); err != nil {
			return err
		}
	}
	return nil
}

func validChain(c ChainDetail) error {
	if err := validID(c.Name); err != nil {
		return err
	}
	if len(c.Desired) > HistoryEntriesPerChain || len(c.Effective) > HistoryEntriesPerChain || len(c.Dropped) > HistoryEntriesPerChain {
		return fmt.Errorf("state: chain entries over limit")
	}
	if c.Omitted.Desired < 0 || c.Omitted.Effective < 0 || c.Omitted.Dropped < 0 {
		return fmt.Errorf("state: invalid chain omitted counts")
	}
	for _, xs := range [][]string{c.Desired, c.Effective, c.Dropped} {
		for _, x := range xs {
			if err := validID(x); err != nil {
				return err
			}
		}
	}
	return nil
}

func validEdit(e EditDetail) error {
	if err := validPolicyPath(e.File); err != nil {
		return err
	}
	if err := validFieldPath(e.Path); err != nil {
		return err
	}
	if e.Action != EditSetScalar && e.Action != EditSetSequence && e.Action != EditSetBool && e.Action != EditRemove {
		return fmt.Errorf("state: invalid edit action")
	}
	return validText(e.Detail)
}
func validateOutcome(o TargetOutcomeKind, p *PendingDetail) error {
	if o == OutcomeApplied {
		if p != nil {
			return fmt.Errorf("state: applied target has pending detail")
		}
		return nil
	}
	if o != OutcomePending || p == nil {
		return fmt.Errorf("state: invalid target outcome")
	}
	valid := map[PendingStage]bool{PendingRender: true, PendingStageBuild: true, PendingConfigValidate: true, PendingDoctor: true, PendingPublishPrepare: true, PendingPublish: true, PendingResolveTargets: true}
	if !valid[p.Stage] {
		return fmt.Errorf("state: invalid pending stage")
	}
	if err := validText(p.Summary); err != nil {
		return err
	}
	return validText(p.Remediation)
}
func validateCounts(c AuthoritativeTargetCounts) error {
	if c.Total < 0 || c.Applied < 0 || c.Pending < 0 || c.Omitted < 0 || c.Total != c.Applied+c.Pending || c.Omitted > c.Total {
		return fmt.Errorf("state: invalid target counts")
	}
	return nil
}
func validOmitted(o RecordOmittedCounts) error {
	if o.Providers < 0 || o.Ranks < 0 || o.Chains < 0 || o.ChainEntries < 0 || o.Edits < 0 {
		return fmt.Errorf("state: invalid omitted counts")
	}
	return nil
}
func validTargetOmitted(o TargetOmittedCounts) error {
	if o.Chains < 0 || o.Edits < 0 {
		return fmt.Errorf("state: invalid target omitted counts")
	}
	return nil
}
func validMode(m Mode) bool { return m == ModeNormal || m == ModeReserve || m == ModeDisabled }
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func validRequiredUTC(t time.Time) error {
	if t.IsZero() || t.Location() != time.UTC {
		return fmt.Errorf("state: history time must be non-zero UTC")
	}
	return nil
}
func validID(s string) error {
	if !utf8.ValidString(s) || len(s) < 1 || len(s) > HistoryIdentifierBytes || hasControl(s) {
		return fmt.Errorf("state: invalid history identifier")
	}
	return nil
}
func validText(s string) error {
	if !utf8.ValidString(s) || len(s) > HistoryFreeTextBytes || hasControl(s) {
		return fmt.Errorf("state: invalid history text")
	}
	return nil
}
func validPolicyPath(s string) error {
	if !utf8.ValidString(s) || len(s) < 1 || len(s) > HistoryPolicyPathBytes || hasControl(s) || strings.Contains(s, "\\") || strings.HasPrefix(s, "/") || path.Clean(s) != s {
		return fmt.Errorf("state: invalid history policy path")
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || c == "." || c == ".." || len(c) > HistoryPathComponentBytes {
			return fmt.Errorf("state: invalid history policy path component")
		}
	}
	return nil
}
func validFieldPath(p []string) error {
	if len(p) < 1 || len(p) > HistoryFieldPathDepth {
		return fmt.Errorf("state: invalid history field path")
	}
	for _, c := range p {
		if err := validID(c); err != nil {
			return err
		}
	}
	return nil
}

func AppendHistory(h ReconcileHistory, r ReconcileRecord) (ReconcileHistory, error) {
	out := DeepCopyReconcileHistory(h)
	records := make([]ReconcileRecord, 0, len(out.Records)+1)
	records = append(records, DeepCopyHistoryRecord(r))
	for _, old := range out.Records {
		if old.Revision != r.Revision {
			records = append(records, old)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Revision > records[j].Revision })
	if len(records) > HistoryRecordLimit {
		records = records[:HistoryRecordLimit]
	}
	out.Records = records
	return boundHistory(out)
}
func boundHistory(h ReconcileHistory) (ReconcileHistory, error) {
	for {
		size, err := historySize(h)
		if err != nil {
			return ReconcileHistory{}, err
		}
		if size <= HistoryEncodedBytes {
			return h, nil
		}
		changed := false
		for i := len(h.Records) - 1; i >= 0; i-- {
			if h.Records[i].Tier == TierFull {
				h.Records[i] = aggregateRecord(h.Records[i])
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		if len(h.Records) == 0 {
			return h, nil
		}
		h.Records = h.Records[:len(h.Records)-1]
		h.OmittedHistoryRecords++
	}
}
func historySize(h ReconcileHistory) (int, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func DeepCopyReconcileHistory(h ReconcileHistory) ReconcileHistory {
	out := ReconcileHistory{OmittedHistoryRecords: h.OmittedHistoryRecords, Records: make([]ReconcileRecord, len(h.Records))}
	for i, r := range h.Records {
		out.Records[i] = DeepCopyHistoryRecord(r)
	}
	return out
}
func DeepCopyHistoryRecord(r ReconcileRecord) ReconcileRecord {
	out := r
	out.Trigger = copyTrigger(r.Trigger)
	out.Providers = append([]ProviderDetail(nil), r.Providers...)
	out.Ranks = append([]RankDetail(nil), r.Ranks...)
	out.Targets = make([]TargetDetail, len(r.Targets))
	for i, t := range r.Targets {
		out.Targets[i] = copyTarget(t)
	}
	out.CompactTargets = make([]CompactTarget, len(r.CompactTargets))
	for i, t := range r.CompactTargets {
		out.CompactTargets[i] = t
		out.CompactTargets[i].Pending = copyPending(t.Pending)
	}
	return out
}
func copyTarget(t TargetDetail) TargetDetail {
	out := t
	out.Pending = copyPending(t.Pending)
	out.Chains = make([]ChainDetail, len(t.Chains))
	for i, c := range t.Chains {
		out.Chains[i] = c
		out.Chains[i].Desired = append([]string(nil), c.Desired...)
		out.Chains[i].Effective = append([]string(nil), c.Effective...)
		out.Chains[i].Dropped = append([]string(nil), c.Dropped...)
	}
	out.Edits = make([]EditDetail, len(t.Edits))
	for i, e := range t.Edits {
		out.Edits[i] = e
		out.Edits[i].Path = append([]string(nil), e.Path...)
	}
	return out
}
func copyPending(p *PendingDetail) *PendingDetail {
	if p == nil {
		return nil
	}
	x := *p
	return &x
}
func copyTrigger(t Trigger) Trigger {
	out := t
	if t.Hook != nil {
		x := *t.Hook
		if x.Window != nil {
			v := *x.Window
			x.Window = &v
		}
		if x.Status != nil {
			v := *x.Status
			x.Status = &v
		}
		if x.UsagePercent != nil {
			v := *x.UsagePercent
			x.UsagePercent = &v
		}
		if x.Used != nil {
			v := *x.Used
			x.Used = &v
		}
		if x.Limit != nil {
			v := *x.Limit
			x.Limit = &v
		}
		if x.ResetAt != nil {
			v := *x.ResetAt
			x.ResetAt = &v
		}
		out.Hook = &x
	}
	if t.Set != nil {
		x := *t.Set
		if x.Quota != nil {
			v := *x.Quota
			x.Quota = &v
		}
		if x.Availability != nil {
			v := *x.Availability
			x.Availability = &v
		}
		out.Set = &x
	}
	if t.Clear != nil {
		x := *t.Clear
		out.Clear = &x
	}
	return out
}

func sanitizeHistory(h ReconcileHistory) ReconcileHistory {
	out := DeepCopyReconcileHistory(h)
	for i := range out.Records {
		r := &out.Records[i]
		r.Trigger = sanitizeTrigger(r.Trigger)
		for j := range r.Providers {
			r.Providers[j].Reason = sanitizeText(r.Providers[j].Reason)
		}
		for j := range r.Ranks {
			r.Ranks[j].Explanation = sanitizeText(r.Ranks[j].Explanation)
		}
		for j := range r.Targets {
			sanitizeTargetText(&r.Targets[j])
		}
		for j := range r.CompactTargets {
			r.CompactTargets[j].Pending = sanitizePending(r.CompactTargets[j].Pending)
		}
	}
	return out
}
func sanitizeTargetText(t *TargetDetail) {
	t.Pending = sanitizePending(t.Pending)
	for i := range t.Edits {
		t.Edits[i].Detail = sanitizeText(t.Edits[i].Detail)
	}
}
