package service

import (
	"context"
	"path/filepath"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// onChangeAggregateBudget bounds the total wall-clock time spent on
// operator-configured on_change actions after one committed revision.
const onChangeAggregateBudget = 120 * time.Second

// pendingChange is the post-commit work stashed by notifyTargets when a
// revision with proven managed-file changes was published and on_change
// actions are configured. It is executed outside the mutation lock by
// runPendingOnChange.
type pendingChange struct {
	revision uint64
	notice   []byte
	actions  []policy.OnChangeAction
}

// globalChainNames are the four global scalar chain names projected by
// ProjectChains; every other projected chain name is a definition file path.
var globalChainNames = map[string]bool{
	"full": true, "mini": true, "nano": true, "classifier": true,
}

// buildNoticeInput projects the applied reconciliation into the notice
// renderer's pure input. It mirrors recordHistoryIfQualified's decision data
// (proven per-target changes, ranking, chain projection) so the published
// notice always reflects what was actually applied. It is pure: no filesystem
// or network access.
func buildNoticeInput(desired policy.Desired, s state.State, targets []RegisteredTarget, outcomes []TargetOutcome, ranks reconcile.RankLookup, publishedAt time.Time) notice.Input {
	in := notice.Input{
		Revision:       s.Revision,
		PublishedAt:    publishedAt,
		RoutingEnabled: desired.Routing.Enabled,
		KnownModels:    managedModelBases(desired),
	}

	targetByID := make(map[string]policy.Target, len(targets))
	for _, rt := range targets {
		targetByID[targetID(rt)] = rt.Policy
	}
	changedByFile := make(map[string][][]string)
	for _, o := range outcomes {
		if o.Prepare == nil {
			continue
		}
		for _, fe := range o.Prepare.ChangedEdits {
			path := append([]string(nil), fe.Path...)
			changedByFile[fe.File] = append(changedByFile[fe.File], path)
		}
	}

	// DisabledModels is the STANDING disabled set — every model of every
	// mapping whose effective mode is disabled — not just the models disabled
	// by this revision. A session may consume a notice many revisions after
	// the disabling one; the actionable tier must still apply (AC5).
	for id, m := range desired.Providers {
		if reconcile.MappingMode(desired, s, id) != state.ModeDisabled {
			continue
		}
		for base := range m.Models {
			in.DisabledModels = append(in.DisabledModels, base)
		}
	}

	for _, o := range outcomes {
		pol, ok := targetByID[o.TargetID]
		if !ok {
			continue
		}
		chains := ProjectChains(desired, s, pol, ranks)

		global := notice.Target{
			ID:            targetIDLabel(pol, o.TargetID),
			Kind:          "global",
			ChangedFields: changedByFile["config.yaml"],
		}
		for _, ch := range chains {
			if globalChainNames[ch.Name] {
				global.Chains = append(global.Chains, notice.Chain{Name: ch.Name, Models: ch.Effective})
				continue
			}
			if len(ch.Effective) == 0 {
				continue
			}
			in.Targets = append(in.Targets, notice.Target{
				ID:            ch.Name,
				Kind:          "definition",
				File:          ch.Name,
				Chain:         ch.Effective,
				ChangedFields: changedByFile[ch.Name],
			})
		}
		in.Targets = append(in.Targets, global)
	}
	return in
}

// targetIDLabel names a target's global-style chain block: "global" for the
// global target, otherwise the target's stable ID.
func targetIDLabel(pol policy.Target, fallback string) string {
	if pol.Global {
		return "global"
	}
	return fallback
}

// managedModelBases collects every concrete base model managed by any mapping,
// expressed as the daemon's ModelConfig.name registry key.
func managedModelBases(desired policy.Desired) map[string]bool {
	out := make(map[string]bool)
	for _, m := range desired.Providers {
		for base := range m.Models {
			out[base] = true
		}
	}
	return out
}

// notifyTargets publishes the reconciliation notice after a successful commit
// when at least one managed file changed. It is best-effort: a publication or
// render failure is recorded as a sanitized notice event in state and never
// affects the transaction's own outcome. It returns true when a failure event
// was appended (so the caller can persist it).
func (c *Coordinator) notifyTargets(desired policy.Desired, s *state.State, targets []RegisteredTarget, outcomes []TargetOutcome) bool {
	prepResults := make([]PrepareResult, 0, len(outcomes))
	for i := range outcomes {
		if outcomes[i].Prepare != nil {
			prepResults = append(prepResults, *outcomes[i].Prepare)
		}
	}
	if !HasProvenChangeAcrossTargets(prepResults) {
		return false
	}
	ranks, _ := ComputeRanking(desired, *s, c.now())
	in := buildNoticeInput(desired, *s, targets, outcomes, ranks, c.now())
	doc, err := notice.Render(in)
	if err != nil {
		return c.recordNoticeFailure(s, in.Revision, "render", err)
	}
	path, err := notice.ResolvePath(desired.Operational.NoticePath)
	if err != nil {
		return c.recordNoticeFailure(s, in.Revision, "path", err)
	}
	if err := notice.Publish(path, doc); err != nil {
		return c.recordNoticeFailure(s, in.Revision, "publish", err)
	}
	if len(desired.Operational.OnChange) > 0 {
		c.pendingChange = &pendingChange{revision: in.Revision, notice: doc, actions: desired.Operational.OnChange}
	}
	return false
}

// runPendingOnChange executes the stashed post-commit on_change actions
// outside the mutation lock: it first reserves the revision durably
// (at-most-once across processes), executes the actions with bounded
// timeouts and an aggregate budget, then records any failures as sanitized
// notice events. It never affects the already-committed transaction's
// outcome.
func (c *Coordinator) runPendingOnChange(ctx context.Context) {
	pc := c.pendingChange
	c.pendingChange = nil
	if pc == nil || len(pc.actions) == 0 {
		return
	}
	if !c.reserveOnChange(ctx, pc.revision) {
		return
	}
	specs := make([]notice.OnChangeSpec, 0, len(pc.actions))
	for _, a := range pc.actions {
		specs = append(specs, notice.OnChangeSpec{
			Run:     a.Run,
			Args:    a.Args,
			Env:     a.Env,
			Timeout: time.Duration(a.TimeoutSeconds) * time.Second,
		})
	}
	results := notice.ExecuteOnChange(ctx, specs, pc.notice, onChangeAggregateBudget)
	hasFailure := false
	for _, r := range results {
		if r.Err != nil || r.Skipped {
			hasFailure = true
			break
		}
	}
	if hasFailure {
		c.recordOnChangeFailures(ctx, pc.revision, results)
	}
}

// reserveOnChange durably marks the revision as executed before running any
// action, so overlapping invocations skip it (at-most-once semantics: a
// crashed action is never retried for the same revision). The lock is held
// only for the brief reserve, never across action execution.
func (c *Coordinator) reserveOnChange(ctx context.Context, revision uint64) bool {
	c.step("on-change-reserve")
	unlock, err := c.Lock.Lock(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = unlock() }()
	s, err := c.State.LoadState()
	if err != nil {
		return false
	}
	if s.OnChangeExecutedRevision >= revision {
		return false
	}
	s.OnChangeExecutedRevision = revision
	return c.State.Save(s) == nil
}

// recordOnChangeFailures appends one sanitized on-change-failed event per
// failed or budget-skipped action. Best-effort: persistence errors are
// swallowed (the committed transaction must not be disturbed).
func (c *Coordinator) recordOnChangeFailures(ctx context.Context, revision uint64, results []notice.OnChangeResult) {
	c.step("on-change-record")
	unlock, err := c.Lock.Lock(ctx)
	if err != nil {
		return
	}
	defer func() { _ = unlock() }()
	s, err := c.State.LoadState()
	if err != nil {
		return
	}
	appended := false
	for _, r := range results {
		if r.Err == nil && !r.Skipped {
			continue
		}
		reason := "skipped: aggregate budget exhausted"
		if r.Err != nil {
			reason = r.Err.Error()
		}
		// Identify the action by basename: the history sanitizer redacts
		// absolute paths from Reason, and operator action paths carry no
		// secret material beyond their location.
		h, aerr := state.AppendEvent(s.EventHistory, state.EventRecord{
			Sequence:   nextEventSequence(&s),
			Revision:   revision,
			Ordinal:    0,
			At:         c.now(),
			RecordedAt: c.now(),
			Category:   state.EventNotice,
			Action:     "on-change-failed",
			Result:     state.EventFailed,
			Reason:     filepath.Base(r.Run) + ": " + reason,
		})
		if aerr != nil {
			continue
		}
		s.EventHistory = h
		appended = true
	}
	if appended {
		_ = c.State.Save(s)
	}
}

// recordNoticeFailure appends a sanitized notice failure event to the state
// history and returns true (caller persists it). The event is keyed by
// revision with an empty provider mapping.
func (c *Coordinator) recordNoticeFailure(s *state.State, revision uint64, stage string, err error) bool {
	if s.EventHistory.Events == nil {
		s.EventHistory.Events = []state.EventRecord{}
	}
	h, aerr := state.AppendEvent(s.EventHistory, state.EventRecord{
		Sequence: nextEventSequence(s),
		Revision: revision,
		Ordinal:  0,
		At:       c.now(),
		RecordedAt: c.now(),
		Category:   state.EventNotice,
		Action:     "notice-" + stage,
		Result:     state.EventFailed,
		Reason:     err.Error(), // sanitized downstream by AppendEvent's SanitizeEventHistory
	})
	if aerr != nil {
		return false
	}
	s.EventHistory = h
	return true
}
