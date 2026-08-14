package state

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var historyTestTime = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func validHistoryTemplate() RecordTemplate {
	return RecordTemplate{
		Revision:  7,
		Trigger:   Trigger{Kind: TriggerReconcile},
		Providers: []ProviderDetail{{MappingID: "codex", Mode: ModeNormal, Reason: "healthy"}},
		Ranks:     []RankDetail{{MappingID: "codex", Rank: 0, Eligible: true, Explanation: "healthy"}},
		Targets: []TemplateTarget{{
			ID: "global", Outcome: OutcomeApplied,
			Chains: []ChainDetail{{Name: "full", Desired: []string{"codex/gpt"}, Effective: []string{"codex/gpt"}}},
			Edits:  []EditDetail{{File: "config.yaml", Path: []string{"defaults", "full"}, Action: EditSetScalar, Detail: "codex/gpt"}},
		}},
	}
}

func validHistoryRecord(t *testing.T) ReconcileRecord {
	t.Helper()
	r, err := ProjectHistoryRecord(validHistoryTemplate(), historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordTemplateHasNoCompletionOrRuntimeOutcome(t *testing.T) {
	pending := &PendingDetail{Stage: PendingPublish, Summary: "pending", Remediation: "retry"}
	tpl := RecordTemplate{Revision: 9, Trigger: Trigger{Kind: TriggerReconcile}, Targets: []TemplateTarget{{ID: "global", PlanComputed: true, Outcome: OutcomePending, Pending: pending}}}
	b, err := json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CompletedAt", "Outcome", "outcome", "Applied", "pending", "retry"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("template contains runtime field %q: %s", forbidden, b)
		}
	}
	r, err := FinalizeHistoryRecord(tpl, []CompactTarget{{ID: "global", Outcome: OutcomeApplied}}, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counts.Applied != 1 || r.Targets[0].Outcome != OutcomeApplied {
		t.Fatalf("record=%+v", r)
	}
}

func TestValidateFullHistoryEnforcesSharedAndOutcomeCounts(t *testing.T) {
	base := validHistoryRecord(t)
	cases := []struct {
		name   string
		mutate func(*ReconcileRecord)
	}{
		{"providers_over_limit", func(r *ReconcileRecord) { r.Providers = make([]ProviderDetail, HistoryProvidersPerRecord+1) }},
		{"ranks_over_limit", func(r *ReconcileRecord) { r.Ranks = make([]RankDetail, HistoryRanksPerRecord+1) }},
		{"applied_count_mismatch", func(r *ReconcileRecord) { r.Counts.Applied = 0; r.Counts.Pending = 1 }},
		{"pending_count_mismatch", func(r *ReconcileRecord) { r.Counts.Applied = 2; r.Counts.Pending = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := DeepCopyHistoryRecord(base)
			tc.mutate(&r)
			if err := ValidateHistoryRecord(r); err == nil {
				t.Fatal("expected persisted full record rejection")
			}
		})
	}
}

func TestEventHistorySizingMarshalErrorsFailClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	st := Store{Path: p, Now: func() time.Time { return historyTestTime }}
	bad := math.NaN()
	if err := st.Save(State{Providers: map[string]ProviderState{}, Targets: map[string]TargetState{}, EventHistory: EventHistory{Events: []EventRecord{{Sequence: 1, Revision: 1, Ordinal: 0, At: historyTestTime, RecordedAt: historyTestTime, Category: EventHook, Action: "quota_low", Provider: "codex", Result: EventChanged, UsagePercent: &bad}}}}); err == nil {
		t.Fatal("Save accepted event history containing unsupported JSON value")
	}
}

func TestValidateHistoryFieldMatrix(t *testing.T) {
	at := strings.Repeat("é", HistoryIdentifierBytes/2)
	base := validHistoryRecord(t)
	cases := []struct {
		name   string
		mutate func(*ReconcileRecord)
	}{
		{"at_limit", func(r *ReconcileRecord) { r.Targets[0].ID = at }},
		{"bad_tier", func(r *ReconcileRecord) { r.Tier = HistoryTier("future") }},
		{"zero_revision", func(r *ReconcileRecord) { r.Revision = 0 }},
		{"zero_time", func(r *ReconcileRecord) { r.CompletedAt = time.Time{} }},
		{"non_utc_time", func(r *ReconcileRecord) { r.CompletedAt = r.CompletedAt.In(time.FixedZone("x", 3600)) }},
		{"invalid_utf8", func(r *ReconcileRecord) { r.Targets[0].ID = string([]byte{0xff}) }},
		{"control", func(r *ReconcileRecord) { r.Targets[0].ID = "bad\n" }},
		{"absolute_path", func(r *ReconcileRecord) { r.Targets[0].Edits[0].File = "/tmp/config.yaml" }},
		{"dot_path", func(r *ReconcileRecord) { r.Targets[0].Edits[0].File = "a/../config.yaml" }},
		{"backslash_path", func(r *ReconcileRecord) { r.Targets[0].Edits[0].File = `a\config.yaml` }},
		{"empty_field_path", func(r *ReconcileRecord) { r.Targets[0].Edits[0].Path = nil }},
		{"deep_field_path", func(r *ReconcileRecord) { r.Targets[0].Edits[0].Path = make([]string, HistoryFieldPathDepth+1) }},
		{"over_targets", func(r *ReconcileRecord) { r.Targets = make([]TargetDetail, HistoryTargetsPerRecord+1) }},
		{"invalid_numeric", func(r *ReconcileRecord) {
			v := math.NaN()
			r.Trigger = Trigger{Kind: TriggerHook, Hook: &HookEvidence{Event: HookQuotaLow, Provider: "codex", Timestamp: historyTestTime, UsagePercent: &v}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := DeepCopyHistoryRecord(base)
			tc.mutate(&r)
			err := ValidateHistoryRecord(r)
			if tc.name == "at_limit" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestHistoryProjectionTruncatesWithOmittedCounts(t *testing.T) {
	tpl := validHistoryTemplate()
	for i := 0; i < HistoryProvidersPerRecord+5; i++ {
		tpl.Providers = append(tpl.Providers, ProviderDetail{MappingID: "p" + strings.Repeat("x", i+1), Mode: ModeNormal, Reason: strings.Repeat("r", 900)})
	}
	tpl.Targets[0].Chains = make([]ChainDetail, HistoryChainsPerTarget+2)
	for i := range tpl.Targets[0].Chains {
		tpl.Targets[0].Chains[i] = ChainDetail{Name: "c" + strings.Repeat("x", i+1), Desired: make([]string, HistoryEntriesPerChain+3)}
		for j := range tpl.Targets[0].Chains[i].Desired {
			tpl.Targets[0].Chains[i].Desired[j] = "m"
		}
	}
	r, err := ProjectHistoryRecord(tpl, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Providers) != HistoryProvidersPerRecord || r.Omitted.Providers == 0 {
		t.Fatalf("providers=%d omitted=%+v", len(r.Providers), r.Omitted)
	}
	if len(r.Targets[0].Chains) != HistoryChainsPerTarget || r.Targets[0].Omitted.Chains != 2 {
		t.Fatalf("target=%+v", r.Targets[0].Omitted)
	}
	if r.Targets[0].Chains[0].Omitted.Desired != 3 {
		t.Fatalf("chain omitted=%+v", r.Targets[0].Chains[0].Omitted)
	}
}

func TestHistoryTierBoundary64Vs65Targets(t *testing.T) {
	for _, n := range []int{64, 65} {
		t.Run(string(rune(n)), func(t *testing.T) {
			tpl := validHistoryTemplate()
			tpl.Targets = make([]TemplateTarget, n)
			for i := range tpl.Targets {
				tpl.Targets[i] = TemplateTarget{ID: "t" + strings.Repeat("x", i+1), Outcome: OutcomeApplied}
			}
			r, err := ProjectHistoryRecord(tpl, historyTestTime)
			if err != nil {
				t.Fatal(err)
			}
			want := TierFull
			if n == 65 {
				want = TierAggregate
			}
			if r.Tier != want {
				t.Fatalf("tier=%s want=%s", r.Tier, want)
			}
			if n == 65 && (len(r.CompactTargets) != 64 || r.Counts.Omitted != 1 || !r.DetailTruncated) {
				t.Fatalf("aggregate=%+v", r)
			}
		})
	}
}

func TestHistoryRecordBudgetBoundaryAt256KiBAndOneByteOver(t *testing.T) {
	base := validHistoryTemplate()
	base.Targets[0].Chains = nil
	entry := strings.Repeat("x", HistoryIdentifierBytes)
	chain := func(n int) ChainDetail {
		values := make([]string, n)
		for i := range values {
			values[i] = entry
		}
		return ChainDetail{Name: "chain", Desired: values, Effective: append([]string(nil), values...), Dropped: append([]string(nil), values...)}
	}
	chains := make([]ChainDetail, HistoryChainsPerTarget)
	lastFull := -1
	for n := 0; n < HistoryChainsPerTarget; n++ {
		chains[n] = chain(HistoryEntriesPerChain)
		chains[n].Name = "chain-" + strings.Repeat("x", n+1)
		base.Targets[0].Chains = chains[:n+1]
		bounded := SanitizeRecordTemplate(base)
		raw := ReconcileRecord{Revision: bounded.Revision, CompletedAt: historyTestTime, Trigger: bounded.Trigger, Tier: TierFull, Counts: AuthoritativeTargetCounts{Total: 1, Applied: 1}, Providers: bounded.Providers, Ranks: bounded.Ranks, Targets: targetDetailsFromTemplate(bounded.Targets), Omitted: bounded.Omitted}
		b, _ := json.Marshal(raw)
		if len(b) <= HistoryRecordEncodedBytes {
			lastFull = n + 1
		} else {
			break
		}
	}
	if lastFull < 1 || lastFull == HistoryChainsPerTarget {
		t.Fatalf("fixture did not cross ceiling: lastFull=%d", lastFull)
	}
	base.Targets[0].Chains = chains[:lastFull]
	r, err := ProjectHistoryRecord(base, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tier != TierFull {
		t.Fatalf("boundary full projected %s", r.Tier)
	}
	base.Targets[0].Chains = chains[:lastFull+1]
	r, err = ProjectHistoryRecord(base, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tier != TierAggregate {
		t.Fatalf("over budget projected %s", r.Tier)
	}
}

func TestAggregateRecordBoundsCompactPendingTextToRecordCeiling(t *testing.T) {
	tpl := validHistoryTemplate()
	tpl.Targets = make([]TemplateTarget, 65)
	for i := range tpl.Targets {
		tpl.Targets[i] = TemplateTarget{ID: "target-" + strings.Repeat("x", i+1), Outcome: OutcomePending, Pending: &PendingDetail{Stage: PendingPublish, Summary: strings.Repeat("s", HistoryFreeTextBytes), Remediation: strings.Repeat("r", HistoryFreeTextBytes)}}
	}
	r, err := ProjectHistoryRecord(tpl, historyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if r.Tier != TierAggregate || len(b) > HistoryRecordEncodedBytes {
		t.Fatalf("tier=%s bytes=%d", r.Tier, len(b))
	}
	if len(r.CompactTargets) != HistoryTargetsPerRecord {
		t.Fatalf("compact targets=%d", len(r.CompactTargets))
	}
	if r.Counts.Omitted != r.Counts.Total-len(r.CompactTargets) {
		t.Fatalf("counts=%+v retained=%d", r.Counts, len(r.CompactTargets))
	}
}

func TestEventHistoryAppendIsNewestFirstAndDeepCopied(t *testing.T) {
	at := historyTestTime
	e := EventRecord{Sequence: 2, Revision: 2, Ordinal: 0, At: at, RecordedAt: at, Category: EventHook, Action: "quota_reached", Provider: "codex", Result: EventChanged, Reason: "Bearer SECRET account=alice", Changes: []string{"defaults.full"}}
	h, err := AppendEvent(EventHistory{}, e)
	if err != nil {
		t.Fatal(err)
	}
	e.Sequence = 1
	e.Changes[0] = "changed"
	if h.Events[0].Sequence != 2 || h.Events[0].Changes[0] != "defaults.full" {
		t.Fatalf("append aliases input: %+v", h.Events[0])
	}
	second := EventRecord{Sequence: 3, Revision: 3, Ordinal: 0, At: at.Add(time.Second), RecordedAt: at.Add(time.Second), Category: EventRoutingChange, Action: "routing_changed", Provider: "codex", Result: EventChanged}
	h, err = AppendEvent(h, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Events) != 2 || h.Events[0].Sequence != 3 || h.Events[1].Sequence != 2 {
		t.Fatalf("events not newest first: %+v", h.Events)
	}
	if h.Events[1].Reason == "Bearer SECRET account=alice" {
		t.Fatal("event reason was not sanitized")
	}
}

func TestSanitizeEventHistoryPreservesEmptyProviderForRoutingEvents(t *testing.T) {
	at := historyTestTime
	// routing_changed events are keyed by MappingID and intentionally carry no
	// Provider; the display falls back from an empty Provider to MappingID.
	routing := EventRecord{Sequence: 1, Revision: 1, Ordinal: 0, At: at, RecordedAt: at, Category: EventRoutingChange, Action: "routing_changed", MappingID: "codex", Result: EventChanged, Reason: "peak, pace 98%"}
	// A hook event carries a real provider that must survive sanitization.
	hook := EventRecord{Sequence: 2, Revision: 1, Ordinal: 1, At: at, RecordedAt: at, Category: EventHook, Action: "quota_low", Provider: "zai", Result: EventChanged}
	// A pre-fix record that baked the redaction sentinel into the optional Provider.
	legacy := EventRecord{Sequence: 3, Revision: 1, Ordinal: 2, At: at, RecordedAt: at, Category: EventRoutingChange, Action: "routing_changed", Provider: "unsafe-id", MappingID: "neuralwatt", Result: EventChanged}

	out := SanitizeEventHistory(EventHistory{Events: []EventRecord{routing, hook, legacy}})

	if out.Events[0].Provider != "" {
		t.Fatalf("routing event provider = %q, want empty so display falls back to MappingID %q", out.Events[0].Provider, out.Events[0].MappingID)
	}
	if out.Events[0].MappingID != "codex" {
		t.Fatalf("routing event mappingID = %q, want codex", out.Events[0].MappingID)
	}
	if out.Events[1].Provider != "zai" {
		t.Fatalf("hook event provider = %q, want zai", out.Events[1].Provider)
	}
	if out.Events[2].Provider != "" {
		t.Fatalf("legacy routing event provider = %q, want empty (healed from sentinel)", out.Events[2].Provider)
	}
	if out.Events[2].MappingID != "neuralwatt" {
		t.Fatalf("legacy routing event mappingID = %q, want neuralwatt", out.Events[2].MappingID)
	}
}

func TestValidateEventHistoryRejectsNonFiniteAndNonUTCFields(t *testing.T) {
	bad := math.NaN()
	e := EventRecord{Sequence: 1, Revision: 1, Ordinal: 0, At: historyTestTime.In(time.FixedZone("offset", 3600)), RecordedAt: historyTestTime, Category: EventHook, Action: "quota_low", Result: EventChanged, UsagePercent: &bad}
	if err := ValidateEventHistory(EventHistory{Events: []EventRecord{e}}); err == nil {
		t.Fatal("expected invalid event field rejection")
	}
}

func TestValidateEventHistoryRejectsDuplicateSequence(t *testing.T) {
	e := EventRecord{Sequence: 1, Revision: 1, Ordinal: 0, At: historyTestTime, RecordedAt: historyTestTime, Category: EventHook, Action: "quota_low", Result: EventChanged}
	if err := ValidateEventHistory(EventHistory{Events: []EventRecord{e, e}}); err == nil {
		t.Fatal("expected duplicate event sequence rejection")
	}
}

func TestValidateHistoryRejectsDuplicateRevision(t *testing.T) {
	r := validHistoryRecord(t)
	if err := ValidateReconcileHistory(ReconcileHistory{Records: []ReconcileRecord{r, r}}); err == nil {
		t.Fatal("expected duplicate revision rejection")
	}
}

// TestValidateQuotaCheckTriggerMatrix locks in the trigger contract for
// TriggerQuotaCheck: a quota check targets all providers (empty MappingID, the
// common `check --reconcile` case) or a single mapping, and must never carry
// hook/set/clear evidence. Regression for the silent history-drop bug.
func TestValidateQuotaCheckTriggerMatrix(t *testing.T) {
	tpl := validHistoryTemplate()
	accept := func(t *testing.T, trig Trigger) {
		t.Helper()
		b := tpl
		b.Trigger = trig
		if _, err := ProjectHistoryRecord(b, historyTestTime); err != nil {
			t.Fatalf("expected accept for %+v, got %v", trig, err)
		}
	}
	reject := func(t *testing.T, trig Trigger) {
		t.Helper()
		b := tpl
		b.Trigger = trig
		if _, err := ProjectHistoryRecord(b, historyTestTime); err == nil {
			t.Fatalf("expected reject for %+v", trig)
		}
	}

	t.Run("all_providers_empty_mapping", func(t *testing.T) { accept(t, Trigger{Kind: TriggerQuotaCheck}) })
	t.Run("single_mapping", func(t *testing.T) { accept(t, Trigger{Kind: TriggerQuotaCheck, MappingID: "openai"}) })
	t.Run("hook_evidence", func(t *testing.T) { reject(t, Trigger{Kind: TriggerQuotaCheck, Hook: &HookEvidence{}}) })
	t.Run("set_evidence", func(t *testing.T) { reject(t, Trigger{Kind: TriggerQuotaCheck, Set: &SetEvidence{}}) })
	t.Run("clear_evidence", func(t *testing.T) { reject(t, Trigger{Kind: TriggerQuotaCheck, Clear: &ClearEvidence{}}) })
}

func TestHistoryEncodedAggregateCeilingDegradesThenValidates(t *testing.T) {
	h := ReconcileHistory{}
	for i := 1; i <= HistoryRecordLimit; i++ {
		tpl := validHistoryTemplate()
		tpl.Revision = uint64(i)
		tpl.Targets = make([]TemplateTarget, 65)
		for j := range tpl.Targets {
			tpl.Targets[j] = TemplateTarget{ID: "t" + strings.Repeat("x", j+1), Outcome: OutcomePending, Pending: &PendingDetail{Stage: PendingPublish, Summary: strings.Repeat("s", HistoryFreeTextBytes), Remediation: strings.Repeat("r", HistoryFreeTextBytes)}}
		}
		r, err := ProjectHistoryRecord(tpl, historyTestTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		h, err = AppendHistory(h, r)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateReconcileHistory(h); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(h)
	if len(b) > HistoryEncodedBytes {
		t.Fatalf("history=%d", len(b))
	}
	if h.OmittedHistoryRecords == 0 {
		t.Fatal("expected byte pruning")
	}
}

func TestHistoryBudgetDegradesOldestThenPrunesOldest(t *testing.T) {
	h := ReconcileHistory{}
	for i := 1; i <= 100; i++ {
		tpl := validHistoryTemplate()
		tpl.Revision = uint64(i)
		tpl.Targets[0].Edits = make([]EditDetail, 150)
		for j := range tpl.Targets[0].Edits {
			tpl.Targets[0].Edits[j] = EditDetail{File: "config.yaml", Path: []string{"models", "m"}, Action: EditSetScalar, Detail: strings.Repeat("x", 512)}
		}
		r, err := ProjectHistoryRecord(tpl, historyTestTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		h, err = AppendHistory(h, r)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(h.Records) == 0 || h.Records[0].Revision != 100 {
		t.Fatalf("newest lost: %+v", h)
	}
	seenAggregate := false
	for _, r := range h.Records {
		if r.Tier == TierAggregate {
			seenAggregate = true
		} else if seenAggregate {
			t.Fatal("newer record degraded before older")
		}
	}
	if err := ValidateReconcileHistory(h); err != nil {
		t.Fatal(err)
	}
}

func FuzzValidateHistoryRecord(f *testing.F) {
	b, _ := json.Marshal(ReconcileRecord{Revision: 1, CompletedAt: historyTestTime, Trigger: Trigger{Kind: TriggerReconcile}, Tier: TierFull, Counts: AuthoritativeTargetCounts{}})
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		var r ReconcileRecord
		if json.Unmarshal(b, &r) == nil {
			_ = ValidateHistoryRecord(r)
		}
	})
}
