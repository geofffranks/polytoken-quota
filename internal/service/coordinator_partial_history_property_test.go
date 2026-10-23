package service

import (
	"context"
	"fmt"
	"github.com/geofffranks/polytoken-quota/internal/publish"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"testing"
)

// This fake publisher models intermediate target bytes, not the real journal, OS rename or crash durability.
type partialHistoryPublisher struct {
	store         *retryHistoryStore
	bytes, before map[string]map[string]string
	failAfter     map[string]int
	writeOrder    []string
}

func newPartialHistoryPublisher(store *retryHistoryStore, targets []string) *partialHistoryPublisher {
	p := &partialHistoryPublisher{store: store, bytes: map[string]map[string]string{}, before: map[string]map[string]string{}, failAfter: map[string]int{}}
	for _, target := range targets {
		p.bytes[target] = map[string]string{"a": "initial", "b": "initial"}
	}
	return p
}
func (p *partialHistoryPublisher) Recover(_ context.Context, _ state.State) (state.State, error) {
	for target, snapshot := range p.before {
		if snapshot != nil {
			p.bytes[target] = cloneFileBytes(snapshot)
			p.before[target] = nil
		}
	}
	return p.store.value, nil
}
func (p *partialHistoryPublisher) ApplyUnderLock(_ context.Context, tx publish.Transaction) (state.State, error) {
	target := tx.TargetID
	p.before[target] = cloneFileBytes(p.bytes[target])
	for i, file := range []string{"a", "b"} {
		p.bytes[target][file] = fmt.Sprintf("%s-r%d", file, tx.Next.Revision)
		p.writeOrder = append(p.writeOrder, fmt.Sprintf("%s/%s/r%d", target, file, tx.Next.Revision))
		if stop, fail := p.failAfter[target]; fail && stop == i+1 {
			return state.State{}, fmt.Errorf("synthetic partial write for %s after file %s", target, file)
		}
	}
	p.before[target] = nil
	return tx.Next, nil
}
func cloneFileBytes(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, contents := range in {
		out[name] = contents
	}
	return out
}
func TestCoordinatorPartialHistoryRecoveryAndIsolation(t *testing.T) {
	targets := []string{"global", "project-a"}
	const initialRevision uint64 = 30
	for schedule := 0; schedule < 81; schedule++ {
		t.Run(fmt.Sprintf("schedule-%02d", schedule), func(t *testing.T) {
			store := &retryHistoryStore{value: state.State{Schema: 1, Revision: initialRevision, Providers: map[string]state.ProviderState{}, Targets: map[string]state.TargetState{}}}
			publisher := newPartialHistoryPublisher(store, targets)
			base := newCoordinatorSpy().withTargets("global", validTargetKey, "project-a", validTargetKey)
			base.Coordinator.State = store
			base.Coordinator.Publish = publisher
			model := retryHistoryModel{revision: initialRevision, applied: map[string]uint64{}}
			modelBytes := map[string]map[string]string{}
			pendingSnapshots := map[string]map[string]string{}
			for _, target := range targets {
				modelBytes[target] = map[string]string{"a": "initial", "b": "initial"}
			}
			remaining := schedule
			for round := 0; round < 2; round++ {
				outcome := map[string]int{}
				failed := map[string]bool{}
				for _, target := range targets {
					outcome[target] = remaining % 3
					remaining /= 3
					if outcome[target] != 0 {
						failed[target] = true
						publisher.failAfter[target] = outcome[target]
					} else {
						delete(publisher.failAfter, target)
					}
				}
				for _, target := range targets {
					modelBytes[target] = cloneFileBytes(modelBytes[target])
					if pendingSnapshots[target] != nil {
						modelBytes[target] = cloneFileBytes(pendingSnapshots[target])
						pendingSnapshots[target] = nil
					}
				}
				model.attempt(targets, failed)
				for _, target := range targets {
					prior := cloneFileBytes(modelBytes[target])
					if failed[target] {
						partial := cloneFileBytes(modelBytes[target])
						for i, file := range []string{"a", "b"} {
							partial[file] = fmt.Sprintf("%s-r%d", file, model.revision)
							if i+1 == outcome[target] {
								break
							}
						}
						pendingSnapshots[target] = prior
						modelBytes[target] = partial
					} else {
						modelBytes[target] = map[string]string{"a": fmt.Sprintf("a-r%d", model.revision), "b": fmt.Sprintf("b-r%d", model.revision)}
						pendingSnapshots[target] = nil
					}
				}
				out := base.Coordinator.Reconcile(context.Background(), false, false, false)
				if !out.Accepted || out.Error != nil {
					t.Fatalf("round %d outcome=%+v", round, out)
				}
				if out.Revision != model.revision || store.value.Revision != model.revision {
					t.Fatalf("round %d revisions=%d/%d model=%d", round, out.Revision, store.value.Revision, model.revision)
				}
				if store.saves != round+1 {
					t.Errorf("saves=%d want %d", store.saves, round+1)
				}
				for _, target := range targets {
					got, ok := store.value.Targets[target]
					if !ok {
						t.Fatalf("missing target %q", target)
					}
					if got.AttemptedRevision != model.revision || got.AppliedRevision != model.applied[target] {
						t.Errorf("target %s revisions=%d/%d want %d/%d", target, got.AttemptedRevision, got.AppliedRevision, model.revision, model.applied[target])
					}
					if failed[target] {
						if got.Pending == nil || got.Pending.LastSuccessfulRevision != model.applied[target] {
							t.Errorf("pending=%+v", got.Pending)
						}
					} else if got.Pending != nil {
						t.Errorf("successful target has pending %+v", got.Pending)
					}
					for name, want := range modelBytes[target] {
						if actual := publisher.bytes[target][name]; actual != want {
							t.Errorf("target %s file %s=%q want %q", target, name, actual, want)
						}
					}
				}
			}
		})
	}
}
