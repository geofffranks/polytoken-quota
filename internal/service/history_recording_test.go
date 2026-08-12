package service

import (
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TestRecordHistoryIfQualifiedWithProvenChange verifies that a transaction
// with at least one target having a proven file change produces a history
// record in state.ReconcileHistory.
func TestRecordHistoryIfQualifiedWithProvenChange(t *testing.T) {
	c := &Coordinator{}
	s := &state.State{Revision: 5}
	outcomes := []TargetOutcome{
		{
			TargetID:        "global",
			AppliedRevision: 5,
			Prepare: &PrepareResult{
				TargetID:     "global",
				PlanComputed: true,
				ChangedFiles: map[string]bool{"config.yaml": true},
				ChangedEdits: []reconcile.FieldEdit{
					{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("gpt-5.6")},
				},
			},
		},
	}

	c.recordHistoryIfQualified(s, txReconcile, transactionInput{}, outcomes, nil, policy.Desired{})

	if len(s.ReconcileHistory.Records) != 1 {
		t.Fatalf("expected 1 history record, got %d", len(s.ReconcileHistory.Records))
	}
	rec := s.ReconcileHistory.Records[0]
	if rec.Revision != 5 {
		t.Errorf("record revision: got %d, want 5", rec.Revision)
	}
	if rec.Trigger.Kind != state.TriggerReconcile {
		t.Errorf("trigger kind: got %s, want %s", rec.Trigger.Kind, state.TriggerReconcile)
	}
	if rec.Counts.Total != 1 {
		t.Errorf("counts total: got %d, want 1", rec.Counts.Total)
	}
	if rec.Counts.Applied != 1 {
		t.Errorf("counts applied: got %d, want 1", rec.Counts.Applied)
	}
}

// TestRecordHistorySkipsDryRun verifies that dry-run transactions produce no
// history record even when there are proven changes.
func TestRecordHistorySkipsDryRun(t *testing.T) {
	c := &Coordinator{}
	s := &state.State{Revision: 5}
	outcomes := []TargetOutcome{
		{
			TargetID: "global",
			Prepare: &PrepareResult{
				ChangedFiles: map[string]bool{"config.yaml": true},
			},
		},
	}

	c.recordHistoryIfQualified(s, txReconcile, transactionInput{DryRun: true}, outcomes, nil, policy.Desired{})

	if len(s.ReconcileHistory.Records) != 0 {
		t.Errorf("dry run should produce no history, got %d records", len(s.ReconcileHistory.Records))
	}
}

// TestRecordHistorySkipsNoProvenChange verifies that transactions with no
// proven file change produce no history record.
func TestRecordHistorySkipsNoProvenChange(t *testing.T) {
	c := &Coordinator{}
	s := &state.State{Revision: 5}
	outcomes := []TargetOutcome{
		{
			TargetID: "global",
			Prepare: &PrepareResult{
				ChangedFiles: map[string]bool{}, // no proven change
			},
		},
	}

	c.recordHistoryIfQualified(s, txReconcile, transactionInput{}, outcomes, nil, policy.Desired{})

	if len(s.ReconcileHistory.Records) != 0 {
		t.Errorf("no proven change should produce no history, got %d records", len(s.ReconcileHistory.Records))
	}
}

// TestRecordHistoryQuotaCheckAllProviders verifies that a quota check run with
// no provider filter — the `check --reconcile` all-providers case — records a
// history record when there is a proven file change. Regression: a
// TriggerQuotaCheck with an empty MappingID ("all") must validate.
func TestRecordHistoryQuotaCheckAllProviders(t *testing.T) {
	c := &Coordinator{}
	s := &state.State{Revision: 5}
	outcomes := []TargetOutcome{
		{
			TargetID:        "global",
			AppliedRevision: 5,
			Prepare: &PrepareResult{
				TargetID:     "global",
				PlanComputed: true,
				ChangedFiles: map[string]bool{"config.yaml": true},
				ChangedEdits: []reconcile.FieldEdit{
					{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("gpt-5.6")},
				},
			},
		},
	}

	c.recordHistoryIfQualified(s, txQuotaCheck, transactionInput{}, outcomes, nil, policy.Desired{})

	if len(s.ReconcileHistory.Records) != 1 {
		t.Fatalf("quota check (all providers) with proven change should produce 1 history record, got %d", len(s.ReconcileHistory.Records))
	}
	rec := s.ReconcileHistory.Records[0]
	if rec.Trigger.Kind != state.TriggerQuotaCheck {
		t.Errorf("trigger kind: got %s, want %s", rec.Trigger.Kind, state.TriggerQuotaCheck)
	}
	if rec.Counts.Applied != 1 {
		t.Errorf("counts applied: got %d, want 1", rec.Counts.Applied)
	}
}

// TestRecordHistoryDeduplicatesRevision verifies that recording the same
// revision twice replaces rather than duplicates.
func TestRecordHistoryDeduplicatesRevision(t *testing.T) {
	c := &Coordinator{}
	s := &state.State{Revision: 5}
	outcomes := []TargetOutcome{
		{
			TargetID:        "global",
			AppliedRevision: 5,
			Prepare: &PrepareResult{
				ChangedFiles: map[string]bool{"config.yaml": true},
				ChangedEdits: []reconcile.FieldEdit{
					{File: "config.yaml", Path: []string{"x"}, Scalar: strPtr("v")},
				},
			},
		},
	}

	// Record twice at same revision
	c.recordHistoryIfQualified(s, txReconcile, transactionInput{}, outcomes, nil, policy.Desired{})
	c.recordHistoryIfQualified(s, txReconcile, transactionInput{}, outcomes, nil, policy.Desired{})

	if len(s.ReconcileHistory.Records) != 1 {
		t.Errorf("dedup: expected 1 record, got %d", len(s.ReconcileHistory.Records))
	}
}
