package service

import (
	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
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
	providers := make([]notice.ProviderState, 0, len(edits))
	for _, edit := range edits {
		providers = append(providers, notice.ProviderState{ID: edit.id, Enabled: edit.enabled})
	}
	revision := s.Revision
	fresh := len(edits) > 0
	if !fresh {
		debt := s.PendingProviderNotice
		if debt == nil {
			return false
		}
		// The committed provider states have not changed since the debt's
		// revision (a steady-state pass makes no edits and no ownership
		// movement), so the lost notice is republished verbatim at its own
		// revision. on_change actions are not re-armed: they already had their
		// at-most-once chance at the commit.
		revision = debt.Revision
		providers = make([]notice.ProviderState, 0, len(debt.Providers))
		for _, p := range debt.Providers {
			providers = append(providers, notice.ProviderState{ID: p.ID, Enabled: p.Enabled})
		}
	}
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
	// Confirmed publication clears the republication debt; a stale debt from
	// an older revision is superseded by this pass's fresh document.
	if s.PendingProviderNotice != nil {
		s.PendingProviderNotice = nil
		return true
	}
	if fresh && len(desired.Operational.OnChange) > 0 {
		c.pendingChange = &pendingChange{revision: revision, notice: doc, actions: desired.Operational.OnChange}
	}
	return false
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
