package service

import (
	"errors"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

func TestRecordTargetOutcomesRetainsLastAppliedOnRepeatedFailure(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	c := &Coordinator{}
	first := state.State{Targets: map[string]state.TargetState{
		"global": {AttemptedRevision: 10, AppliedRevision: 10, AppliedAt: now},
		"project": {AttemptedRevision: 10, AppliedRevision: 10, AppliedAt: now},
	}}
	for _, revision := range []uint64{11, 12} {
		first = c.recordTargetOutcomes(first, []TargetOutcome{
			pendingOutcome("global", revision, "publish", errors.New("synthetic failure")),
			appliedOutcome("project", revision),
		})
		got := first.Targets["global"]
		if got.AttemptedRevision != revision || got.AppliedRevision != 10 || !got.AppliedAt.Equal(now) {
			t.Fatalf("revision %d: global history = %+v, want attempted %d and last applied 10 at %s", revision, got, revision, now)
		}
		if got.Pending == nil || got.Pending.LastSuccessfulRevision != 10 || !got.Pending.LastSuccessfulAt.Equal(now) {
			t.Fatalf("revision %d: pending last success = %+v, want revision 10 at %s", revision, got.Pending, now)
		}
		if other := first.Targets["project"]; other.AppliedRevision != revision || other.Pending != nil {
			t.Fatalf("revision %d: independent project outcome = %+v", revision, other)
		}
	}
}
