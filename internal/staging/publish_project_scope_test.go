package staging

// pq-m4k10: a project candidate's publish dir must be project-scoped. The
// merged global+project config is a validation-only representation; publishing
// it into the project's live config.yaml installs the entire global config
// (providers and their auth, daemon, models, ...) into the project repo and
// produces a file polytoken's project layer rejects. The project publish
// candidate therefore starts from the project's own config bytes, with the
// project plan's edits applied on top.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"gopkg.in/yaml.v3"
)

// TestProjectPublishConfigIsProjectScoped proves the publish dir's config.yaml
// for a registered project derives from the project's own config: no
// global-only keys (providers) and no global auth content reach it, the
// project's own values are preserved, and the project plan's managed scalar
// edit lands on top.
func TestProjectPublishConfigIsProjectScoped(t *testing.T) {
	live := layeredFixture(t)
	plan := reconcile.Plan{
		TargetID: live.Target.ID,
		Edits: []reconcile.FieldEdit{
			{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("codex/gpt(high)")},
		},
	}
	b := builderWith(t, live, AuthTransientSource)
	c, err := b.Build(context.Background(), live.Target, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Cleanup() })

	data, err := os.ReadFile(filepath.Join(c.PublishDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read publish config: %v", err)
	}
	if strings.Contains(string(data), sourceSecret) {
		t.Fatalf("global auth secret leaked into project publish config:\n%s", data)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse publish config: %v\n%s", err, data)
	}
	if _, ok := m["providers"]; ok {
		t.Fatalf("global-only providers key leaked into project publish config:\n%s", data)
	}
	if got := configGet(t, m, "defaults.full"); got != "codex/gpt(high)" {
		t.Fatalf("defaults.full = %v, want the plan's edited value", got)
	}
	if got := configGet(t, m, "models.codex/gpt.enabled"); got != false {
		t.Fatalf("project's own models.codex/gpt.enabled = %v, want false (preserved)", got)
	}
}
