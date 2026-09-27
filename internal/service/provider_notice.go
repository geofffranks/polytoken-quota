package service

import (
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// notifyProviderGate publishes only after a successful provider-only commit.
// With fresh provider edits it publishes their committed states; with no fresh
// edits it republishes a lost notice recorded in PendingProviderNotice, so a
// publish failure or a crash between the state commit and the publication
// converges on the next provider-only pass instead of waiting for another
// byte-changing edit. It returns true when the caller must persist s: a
// notice-failure event was appended, or the republication debt changed.
func (c *Coordinator) notifyProviderGate(desired policy.Desired, s *state.State, edits []policyProviderEdit) bool {
	fresh := len(edits) > 0
	debt := s.PendingProviderNotice
	if debt == nil && !fresh {
		return false
	}
	revision := s.Revision
	if !fresh && debt != nil {
		revision = debt.Revision
	}
	providers := providerNoticeStates(debt, edits, nil)
	doc, err := notice.RenderProvider(revision, c.now(), providers)
	if err != nil {
		return c.recordProviderNoticeFailure(s, revision, providers, "render", err)
	}
	path, err := notice.ResolvePath(desired.Operational.NoticePath)
	if err != nil {
		return c.recordProviderNoticeFailure(s, revision, providers, "path", err)
	}
	if err := notice.Publish(path, doc); err != nil {
		return c.recordProviderNoticeFailure(s, revision, providers, "publish", err)
	}
	// Confirmed publication clears the republication debt. Fresh provider
	// edits still arm their own post-commit action even when older debt was
	// included in the published document.
	changed := s.PendingProviderNotice != nil
	if changed {
		s.PendingProviderNotice = nil
	}
	if fresh && len(desired.Operational.OnChange) > 0 {
		c.pendingChange = &pendingChange{revision: revision, notice: doc, actions: desired.Operational.OnChange}
	}
	return changed
}

// recordProviderNoticeFailure appends a sanitized notice failure event and
// records the republication debt carrying exactly the provider states that
// failed to publish, so a later provider-only pass retries the lost notice
// even though the pass that committed it is over. It returns true when the
// caller must persist s.
func (c *Coordinator) recordProviderNoticeFailure(s *state.State, revision uint64, providers []notice.ProviderState, stage string, err error) bool {
	s.PendingProviderNotice = pendingNoticeDebt(revision, providerStatesToEdits(providers))
	return c.recordNoticeFailure(s, revision, stage, err)
}

// reconcileProviderNoticeDebt keeps pending provider states that still match
// the registered global config, then overlays states committed by fresh edits.
func reconcileProviderNoticeDebt(previous *state.PendingProviderNotice, enabled map[string]bool, revision uint64, edits []policyProviderEdit) *state.PendingProviderNotice {
	states := make(map[string]bool)
	if previous != nil {
		for _, p := range previous.Providers {
			// A nil map means evaluation could not establish the enrolled
			// providers (for example, an early refusal); retain debt in that
			// case. A non-nil map is authoritative, so de-enrolled IDs are
			// unverifiable and must not survive a successful evaluation.
			if enabled == nil {
				states[p.ID] = p.Enabled
				continue
			}
			if current, ok := enabled[p.ID]; ok && current == p.Enabled {
				states[p.ID] = p.Enabled
			}
		}
	}
	for _, edit := range edits {
		states[edit.id] = edit.enabled
	}
	if len(states) == 0 {
		return nil
	}
	if len(edits) == 0 && previous != nil {
		revision = previous.Revision
	}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	debt := &state.PendingProviderNotice{Revision: revision, Providers: make([]state.ProviderNoticeState, 0, len(ids))}
	for _, id := range ids {
		debt.Providers = append(debt.Providers, state.ProviderNoticeState{ID: id, Enabled: states[id]})
	}
	return debt
}

// providerNoticeStates merges any debt with fresh edits, where a fresh state
// for a provider takes precedence. When enabled is non-nil, stale debt is
// omitted unless it still matches the committed value.
func providerNoticeStates(debt *state.PendingProviderNotice, edits []policyProviderEdit, enabled map[string]bool) []notice.ProviderState {
	states := make(map[string]bool)
	if debt != nil {
		for _, p := range debt.Providers {
			if enabled != nil {
				current, ok := enabled[p.ID]
				if !ok || current != p.Enabled {
					continue
				}
			}
			states[p.ID] = p.Enabled
		}
	}
	for _, edit := range edits {
		states[edit.id] = edit.enabled
	}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	providers := make([]notice.ProviderState, 0, len(ids))
	for _, id := range ids {
		providers = append(providers, notice.ProviderState{ID: id, Enabled: states[id]})
	}
	return providers
}

// providerPlanNoticeEdits extracts only provider enabled-field edits from a
// gate plan, in the same sanitized provider-state shape used for notices.
func providerPlanNoticeEdits(edits []reconcile.FieldEdit) []policyProviderEdit {
	out := make([]policyProviderEdit, 0, len(edits))
	for _, edit := range edits {
		if edit.File != "config.yaml" || len(edit.Path) != 3 || edit.Path[0] != "providers" || edit.Path[2] != "enabled" {
			continue
		}
		enabled := edit.Remove
		if edit.Enabled != nil {
			enabled = *edit.Enabled
		}
		out = append(out, policyProviderEdit{id: edit.Path[1], enabled: enabled})
	}
	return out
}

// pendingNoticeDebt builds the republication debt for one committed pass.
func pendingNoticeDebt(revision uint64, edits []policyProviderEdit) *state.PendingProviderNotice {
	debt := &state.PendingProviderNotice{Revision: revision, Providers: make([]state.ProviderNoticeState, 0, len(edits))}
	for _, e := range edits {
		debt.Providers = append(debt.Providers, state.ProviderNoticeState{ID: e.id, Enabled: e.enabled})
	}
	return debt
}

// providerStatesToEdits adapts notice provider states back into the edit
// shape the debt helper consumes.
func providerStatesToEdits(providers []notice.ProviderState) []policyProviderEdit {
	out := make([]policyProviderEdit, 0, len(providers))
	for _, p := range providers {
		out = append(out, policyProviderEdit{id: p.ID, enabled: p.Enabled})
	}
	return out
}

// ownershipMapsEqual compares two ownership maps, treating nil and empty as
// equal so fresh states and cleared maps compare cleanly.
func ownershipMapsEqual(a, b map[string]state.ProviderOwnership) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

type policyProviderEdit struct {
	id      string
	enabled bool
}

// providerEdits extracts only proven provider enabled-field changes from the
// committed global prepare result. Removal restores the operator's absent-key
// baseline, and an absent `providers.<id>.enabled` key means the provider is
// default-enabled — so a remove edit reports enabled=true, matching the
// committed effect.
func providerEdits(outcomes []TargetOutcome) []policyProviderEdit {
	var edits []policyProviderEdit
	for _, outcome := range outcomes {
		if outcome.Prepare == nil {
			continue
		}
		for _, edit := range outcome.Prepare.ChangedEdits {
			if edit.File != "config.yaml" || len(edit.Path) != 3 || edit.Path[0] != "providers" || edit.Path[2] != "enabled" {
				continue
			}
			enabled := edit.Remove
			if edit.Enabled != nil {
				enabled = *edit.Enabled
			}
			edits = append(edits, policyProviderEdit{id: edit.Path[1], enabled: enabled})
		}
	}
	return edits
}
