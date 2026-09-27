package service

import (
	"github.com/geofffranks/polytoken-quota/internal/notice"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// notifyProviderGate publishes only after a successful provider-only commit
// with a proven provider enabled-field edit.
func (c *Coordinator) notifyProviderGate(desired policy.Desired, s *state.State, edits []policyProviderEdit) bool {
	if len(edits) == 0 {
		return false
	}
	providers := make([]notice.ProviderState, 0, len(edits))
	for _, edit := range edits {
		providers = append(providers, notice.ProviderState{ID: edit.id, Enabled: edit.enabled})
	}
	revision := s.Revision
	doc, err := notice.RenderProvider(revision, c.now(), providers)
	if err != nil {
		return c.recordNoticeFailure(s, revision, "render", err)
	}
	path, err := notice.ResolvePath(desired.Operational.NoticePath)
	if err != nil {
		return c.recordNoticeFailure(s, revision, "path", err)
	}
	if err := notice.Publish(path, doc); err != nil {
		return c.recordNoticeFailure(s, revision, "publish", err)
	}
	if len(desired.Operational.OnChange) > 0 {
		c.pendingChange = &pendingChange{revision: revision, notice: doc, actions: desired.Operational.OnChange}
	}
	return false
}

type policyProviderEdit struct {
	id      string
	enabled bool
}

// providerEdits extracts only proven provider enabled-field changes from the
// committed global prepare result; removal means the default enabled state.
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
			enabled := !edit.Remove
			if edit.Enabled != nil {
				enabled = *edit.Enabled
			}
			edits = append(edits, policyProviderEdit{id: edit.Path[1], enabled: enabled})
		}
	}
	return edits
}
