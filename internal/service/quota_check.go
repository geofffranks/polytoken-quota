package service

// QuotaCheck transaction: the per-kind handler that polls provider adapters,
// folds the observations into the next state (preserving last-good snapshots on
// failure), and optionally runs the full stage/validate/publish reconcile
// pipeline against the freshly observed state. It reuses the common
// Coordinator.transact path for lock/recover/save, so crash recovery and the
// single locked cycle are identical to every other mutator.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// transactQuotaCheck implements the quota check transaction. It loads the
// desired policy, polls each configured provider independently (provider
// failures are isolated), folds the attempts into the next state — always
// updating QuotaAttempt and updating QuotaSnapshot only on a successful attempt
// — bumps the revision, and atomically persists the observations. When
// in.Reconcile is set it then resolves targets and runs the existing
// render→stage→validate→publish pipeline against the observed state.
//
// Exit semantics are carried on the returned Outcome: Accepted is false on
// rejection (no mutation), and Problem is true when any polled provider's
// attempt failed (or, in reconcile mode, a target is pending).
func (c *Coordinator) transactQuotaCheck(ctx context.Context, recovered state.State, in transactionInput) Outcome {
	if c.QuotaPoller == nil {
		return Outcome{Accepted: false, Error: errors.New("service: quota check requires a quota poller")}
	}
	c.step("load-policy")
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	c.step("load-state")
	observed := recovered

	c.step("poll")
	attempts, err := c.QuotaPoller.Poll(ctx, desired, in.Provider, c.now())
	if err != nil {
		return Outcome{Accepted: false, Error: err}
	}
	attemptReports := quotaAttemptDiagnostics(attempts)

	next := observed
	next.Revision = observed.Revision + 1
	next = applyQuotaObservations(next, desired, attempts)
	if desired.Routing.Enabled {
		next = applyRoutingMetadata(next, desired, c.now())
	}
	problem := anyAttemptFailed(attempts)

	var outcomes []TargetOutcome
	if in.Reconcile {
		c.step("load-sources")
		targets, terr := c.Targets.ResolveTargets(desired)
		if terr != nil {
			// Target resolution failed, but the observations are still accepted:
			// record the resolution as a pending target outcome and persist the
			// observations (mirrors the transactManual resolution-failure path).
			pending := pendingOutcome("quota-reconcile", next.Revision, "resolve_targets", terr)
			outcomes = []TargetOutcome{pending}
		} else {
			outcomes = c.processTargets(ctx, desired, observed, next, targets, true)
		}
		next = c.recordTargetOutcomes(next, outcomes)
	}

	c.step("save-state")
	if serr := c.State.Save(next); serr != nil {
		return Outcome{Accepted: false, Revision: next.Revision, Problem: problem, Targets: outcomes, ProviderAttempts: attemptReports, Error: fmt.Errorf("service: persist quota observations: %w", serr)}
	}
	return Outcome{Accepted: true, Revision: next.Revision, Problem: problem, Targets: outcomes, ProviderAttempts: attemptReports}
}

// applyQuotaObservations folds the polled attempts into the next state. For each
// polled mapping, every CodExBar provider it backs receives the attempt as its
// QuotaAttempt; a successful (non-failed) attempt also replaces the
// last-good QuotaSnapshot. A failed attempt NEVER overwrites the existing
// QuotaSnapshot, preserving last-good. Each provider gets its own snapshot copy
// so the pointers never alias.
func applyQuotaObservations(next state.State, desired policy.Desired, attempts map[string]quota.QuotaSnapshot) state.State {
	// Allocate a fresh Providers map (mirroring state.SetProvider) so the prior
	// state's map is never aliased: the caller passes the recovered observed
	// state, and mutating next.Providers must not mutate observed.Providers.
	fresh := make(map[string]state.ProviderState, len(next.Providers)+1)
	for k, v := range next.Providers {
		fresh[k] = v
	}
	for id, m := range desired.Providers {
		if m.Quota == nil {
			continue
		}
		snap, ok := attempts[string(id)]
		if !ok {
			continue
		}
		for _, cb := range m.CodexBarProviders {
			ps := fresh[cb]
			attempt := snap
			ps.QuotaAttempt = &attempt
			if snap.Status != quota.SourceFailed {
				good := snap
				ps.QuotaSnapshot = &good
			}
			fresh[cb] = ps
		}
	}
	next.Providers = fresh
	return next
}

// anyAttemptFailed reports whether any polled attempt has a failed status,
// which the CLI maps to exit code 2 (pending provider problem).
type QuotaAttemptDiagnostic struct {
	MappingID string    `json:"mapping_id"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

func quotaAttemptDiagnostics(attempts map[string]quota.QuotaSnapshot) []QuotaAttemptDiagnostic {
	ids := make([]string, 0, len(attempts))
	for id := range attempts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]QuotaAttemptDiagnostic, 0, len(ids))
	for _, id := range ids {
		s := attempts[id]
		out = append(out, QuotaAttemptDiagnostic{MappingID: id, Status: string(s.Status), Error: quota.SanitizeText(s.Error), CheckedAt: s.CheckedAt})
	}
	return out
}

func anyAttemptFailed(attempts map[string]quota.QuotaSnapshot) bool {
	for _, snap := range attempts {
		if snap.Status == quota.SourceFailed {
			return true
		}
	}
	return false
}

// applyRoutingMetadata durably records the accepted ranking decision alongside
// quota observations. It only runs when routing is enabled.
func applyRoutingMetadata(next state.State, desired policy.Desired, now time.Time) state.State {
	_, ranking := ComputeRanking(desired, next, now)
	providers := make(map[string]state.ProviderState, len(next.Providers))
	for k, v := range next.Providers {
		providers[k] = v
	}
	order := make([]string, 0, len(ranking.Entries))
	for _, entry := range ranking.Entries {
		if !entry.Eligible {
			continue
		}
		order = append(order, entry.MappingID)
		m := desired.Providers[policy.MappingID(entry.MappingID)]
		for _, cb := range m.CodexBarProviders {
			ps := providers[cb]
			ps.Routing.LastRank = entry.Rank
			ps.Routing.LastDecisionAt = now
			ps.Routing.LastAppliedRevision = next.Revision
			providers[cb] = ps
		}
	}
	next.Providers = providers
	next.RoutingHistory = &state.RoutingHistory{LastGoodGlobalRank: order, ComputedAt: now}
	return next
}

// sortedMappingIDs returns the desired provider mapping IDs in sorted order for
// deterministic polling.
func sortedMappingIDs(desired policy.Desired) []string {
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	return ids
}
