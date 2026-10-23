package service

import (
	"context"
	"fmt"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"testing"
)

type retryHistoryStore struct {
	value state.State
	saves int
}

func (s *retryHistoryStore) LoadState() (state.State, error) { return s.value, nil }
func (s *retryHistoryStore) Save(v state.State) error        { s.value = v; s.saves++; return nil }

type retryHistoryPublisher struct {
	store    *retryHistoryStore
	failures map[string]map[uint64]bool
	applies  map[string]int
}

func (p *retryHistoryPublisher) Recover(_ context.Context, _ state.State) (state.State, error) {
	return p.store.value, nil
}
func (p *retryHistoryPublisher) ApplyUnderLock(_ context.Context, tx publish.Transaction) (state.State, error) {
	p.applies[tx.TargetID]++
	if p.failures[tx.TargetID][tx.Next.Revision] {
		return state.State{}, fmt.Errorf("synthetic publish fault for %s at revision %d", tx.TargetID, tx.Next.Revision)
	}
	return tx.Next, nil
}

type retryHistoryModel struct {
	revision uint64
	applied  map[string]uint64
}

func (m *retryHistoryModel) attempt(targets []string, failed map[string]bool) {
	m.revision++
	for _, id := range targets {
		if !failed[id] {
			m.applied[id] = m.revision
		}
	}
}

// Finite fake-publisher schedules model coordinator retry isolation, not durable filesystem journaling.
func TestCoordinatorBoundedRetryHistoriesPersistRevisionAndIsolateTargets(t *testing.T) {
	targets := []string{"global", "project-a"}
	const initialRevision uint64 = 20
	for schedule := 0; schedule < 1<<(len(targets)*3); schedule++ {
		t.Run(fmt.Sprintf("schedule-%02x", schedule), func(t *testing.T) {
			store := &retryHistoryStore{value: state.State{Schema: 1, Revision: initialRevision, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
			faults := map[string]map[uint64]bool{}
			for i, id := range targets {
				faults[id] = map[uint64]bool{}
				for round := 0; round < 3; round++ {
					bit := round*len(targets) + i
					faults[id][initialRevision+uint64(round)+1] = schedule&(1<<bit) != 0
				}
			}
			publisher := &retryHistoryPublisher{store: store, failures: faults, applies: map[string]int{}}
			base := newCoordinatorSpy().withTargets("global", validTargetKey, "project-a", validTargetKey)
			base.Coordinator.State = store
			base.Coordinator.Publish = publisher
			model := retryHistoryModel{revision: initialRevision, applied: map[string]uint64{}}
			for round := 0; round < 3; round++ {
				failed := map[string]bool{}
				for _, id := range targets {
					failed[id] = faults[id][model.revision+1]
				}
				model.attempt(targets, failed)
				out := base.Coordinator.Reconcile(context.Background(), false, false, false)
				if !out.Accepted || out.Error != nil {
					t.Fatalf("round %d outcome=%+v", round, out)
				}
				if out.Revision != model.revision || store.value.Revision != model.revision {
					t.Fatalf("round %d returned/durable=%d/%d model=%d", round, out.Revision, store.value.Revision, model.revision)
				}
				if len(out.Targets) != len(targets) {
					t.Fatalf("round %d outcomes=%d want %d", round, len(out.Targets), len(targets))
				}
				for _, id := range targets {
					got, ok := store.value.Targets[id]
					if !ok {
						t.Fatalf("missing target %q", id)
					}
					if got.AttemptedRevision != model.revision {
						t.Errorf("target %s attempted=%d want %d", id, got.AttemptedRevision, model.revision)
					}
					wantApplied := model.applied[id]
					if got.AppliedRevision != wantApplied {
						t.Errorf("target %s applied=%d want %d", id, got.AppliedRevision, wantApplied)
					}
					if failed[id] {
						if got.Pending == nil {
							t.Errorf("target %s missing pending failure", id)
							continue
						}
						if got.Pending.LastSuccessfulRevision != wantApplied || got.Pending.AttemptedRevision != model.revision {
							t.Errorf("target %s pending=%+v", id, got.Pending)
						}
					} else if got.Pending != nil {
						t.Errorf("target %s retained pending %+v", id, got.Pending)
					}
				}
			}
			if store.saves != 3 {
				t.Errorf("saves=%d want 3", store.saves)
			}
			for _, id := range targets {
				if publisher.applies[id] != 3 {
					t.Errorf("target %s applies=%d want 3", id, publisher.applies[id])
				}
			}
		})
	}
}

var _ StateStore = (*retryHistoryStore)(nil)
var _ Publisher = (*retryHistoryPublisher)(nil)
