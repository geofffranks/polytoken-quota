package service

// Detection tests for the legacy modelgroups guard. The coordinator scans the
// resolved targets' live layer configs — the global root's config.yaml and every
// registered project root's config.yaml — because the staged merge composes
// those layers, so a modelgroups key on either layer makes every legacy
// tier-default write an invalid candidate. A layer whose config.yaml is absent
// or unreadable contributes no detection: staging and validation reject such
// layers downstream exactly as before, so detection never widens its own
// failure surface.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

func guardTargets(globalRoot, projectRoot string) []RegisteredTarget {
	targets := []RegisteredTarget{{
		Policy:   policy.Target{ID: "global", Global: true},
		Resolved: target.Resolved{ID: "global", CanonicalRoot: globalRoot, Global: true},
	}}
	if projectRoot != "" {
		targets = append(targets, RegisteredTarget{
			Policy:   policy.Target{ID: "proj", Root: projectRoot},
			Resolved: target.Resolved{ID: "proj", CanonicalRoot: projectRoot},
		})
	}
	return targets
}

func TestDetectModelGroupsScansEveryRegisteredLayer(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	projDir := filepath.Join(root, "proj")
	for _, dir := range []string{globalDir, projDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(globalDir, "config.yaml"),
		"version: 4\nproviders:\n  gp:\n    api_key: inert\nmodelgroups:\n  failover:\n    - gp/g1\n")
	writeFile(t, filepath.Join(projDir, "config.yaml"),
		"version: 4\nproviders:\n  gp:\n    api_key: inert\n")

	cases := []struct {
		name        string
		globalWith  bool // rewrite the global config with a modelgroups layer
		projectWith bool // rewrite the project config with a modelgroups layer
		want        bool
	}{
		{name: "global layer only", globalWith: true, want: true},
		{name: "project layer only", projectWith: true, want: true},
		{name: "no layer", want: false},
		{name: "both layers", globalWith: true, projectWith: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globalConfig := "version: 4\nproviders:\n  gp:\n    api_key: inert\n"
			projectConfig := globalConfig
			if tc.globalWith {
				globalConfig += "modelgroups:\n  failover:\n    - gp/g1\n"
			}
			if tc.projectWith {
				projectConfig += "modelgroups:\n  failover:\n    - gp/g1\n"
			}
			writeFile(t, filepath.Join(globalDir, "config.yaml"), globalConfig)
			writeFile(t, filepath.Join(projDir, "config.yaml"), projectConfig)
			if got := detectModelGroups(guardTargets(globalDir, projDir)); got != tc.want {
				t.Fatalf("detectModelGroups = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectModelGroupsFailsOpenOnUnreadableLayers(t *testing.T) {
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No config.yaml anywhere: nothing to detect, no failure.
	if detectModelGroups(guardTargets(globalDir, "")) {
		t.Fatal("absent configs must not be detected as modelgroups")
	}
	// An unparseable config is downstream staging/validation's failure, not the
	// guard's: detection stays silent.
	writeFile(t, filepath.Join(globalDir, "config.yaml"), "version: 4\n  bad: [")
	if detectModelGroups(guardTargets(globalDir, "")) {
		t.Fatal("unparseable config must not be detected as modelgroups")
	}
}

func TestDetectModelGroupsIgnoresNestedKeys(t *testing.T) {
	// Only a TOP-LEVEL modelgroups key means the config uses modelgroups; a
	// nested key of the same name elsewhere must not trip the guard.
	root := t.TempDir()
	globalDir := filepath.Join(root, "global")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, "config.yaml"),
		"version: 4\nproviders:\n  gp:\n    api_key: inert\n    modelgroups: reference-only\n")
	if detectModelGroups(guardTargets(globalDir, "")) {
		t.Fatal("nested modelgroups key must not be detected")
	}
}
