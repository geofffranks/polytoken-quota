package target

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"pgregory.net/rapid"
)

// Generated roots and synthetic files exercise discovery without touching user workspaces.
func TestPropertyRegisteredTargetOwnershipPreserved(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		targetCount := rapid.IntRange(1, 4).Draw(rt, "targetCount")
		roots := make([]string, targetCount)
		inputs := make([]policy.Target, targetCount)
		desired := policy.Desired{Version: 1, Providers: make(map[policy.MappingID]policy.Mapping)}
		registered := make([]map[string]bool, targetCount)
		fileSnapshots := make([]map[string][]byte, targetCount)
		for i := 0; i < targetCount; i++ {
			root, err := os.MkdirTemp(t.TempDir(), "ownership-target-")
			if err != nil {
				t.Fatal(err)
			}
			roots[i] = root
			if err = os.WriteFile(filepath.Join(root, "config.yaml"), []byte("defaults: {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			global := rapid.Bool().Draw(rt, fmt.Sprintf("target%dGlobal", i))
			if targetCount > 1 && i == 0 {
				global = true
			} else if targetCount > 1 && i == 1 {
				global = false
			}
			targetID := fmt.Sprintf("target-%d", i)
			model := fmt.Sprintf("provider-%d/model-%d", i, i)
			mappingID := policy.MappingID(fmt.Sprintf("provider-%d", i))
			desired.Providers[mappingID] = policy.Mapping{Models: map[string]policy.ModelBaseline{model: {Enabled: true, HadEnabledKey: true}}}
			definitionCount := rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("target%dRegisteredCount", i))
			defs := make([]policy.Definition, 0, definitionCount)
			registered[i] = map[string]bool{"config.yaml": true}
			for j := 0; j < definitionCount; j++ {
				rel := fmt.Sprintf("facets/registered-%d-%d.md", i, j)
				writeOwnershipFixture(t, root, rel, model)
				defs = append(defs, policy.Definition{Path: rel, Chain: policy.Chain{model}})
				registered[i][rel] = true
			}
			extraCount := rapid.IntRange(1, 3).Draw(rt, fmt.Sprintf("target%dExtraCount", i))
			for j := 0; j < extraCount; j++ {
				rel := fmt.Sprintf("facets/unregistered-%d-%d.md", i, j)
				writeOwnershipFixture(t, root, rel, model)
			}
			inputs[i] = policy.Target{ID: targetID, Root: root, Global: global, Definitions: defs}
			fileSnapshots[i] = snapshotOwnershipFiles(t, root)
		}
		for i := range inputs {
			before := cloneOwnershipInputs(desired, inputs[i])
			proposals, err := Discover(roots[i])
			if err != nil {
				t.Fatalf("Discover target %d: %v", i, err)
			}
			proposed := make(map[string]bool)
			for _, path := range proposals {
				proposed[path] = true
			}
			for path := range registered[i] {
				if path != "config.yaml" && !proposed[path] {
					t.Fatalf("Discover omitted registered %q: %v", path, proposals)
				}
			}
			for j := 0; ; j++ {
				path := fmt.Sprintf("facets/unregistered-%d-%d.md", i, j)
				if _, err := os.Stat(filepath.Join(roots[i], filepath.FromSlash(path))); err != nil {
					break
				}
				if !proposed[path] {
					t.Fatalf("Discover omitted model-bearing extra %q", path)
				}
			}
			resolved, err := Resolve(inputs[i])
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			wantPaths := make([]string, 0, len(inputs[i].Definitions))
			for _, def := range inputs[i].Definitions {
				wantPaths = append(wantPaths, filepath.ToSlash(def.Path))
			}
			sort.Strings(wantPaths)
			gotPaths := make([]string, 0, len(resolved.Definitions))
			for _, def := range resolved.Definitions {
				gotPaths = append(gotPaths, def.PolicyPath)
			}
			if !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("resolved=%v want %v", gotPaths, wantPaths)
			}
			plan, err := reconcile.Build(desired, state.State{}, inputs[i], nil)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for _, edit := range plan.Edits {
				if !registered[i][edit.File] {
					t.Fatalf("edit outside registered files: %+v", edit)
				}
				if !inputs[i].Global && len(edit.Path) > 0 && edit.Path[0] == "models" {
					t.Fatalf("project emitted global models edit: %+v", edit)
				}
			}
			if !reflect.DeepEqual(before.desired, desired) || !reflect.DeepEqual(before.target, inputs[i]) {
				t.Fatalf("Resolve/Build mutated target %d", i)
			}
			if after := snapshotOwnershipFiles(t, roots[i]); !reflect.DeepEqual(fileSnapshots[i], after) {
				t.Fatalf("target %d files changed", i)
			}
		}
	})
}

type ownershipInputSnapshot struct {
	desired policy.Desired
	target  policy.Target
}

func cloneOwnershipInputs(desired policy.Desired, target policy.Target) ownershipInputSnapshot {
	copyDesired := policy.Desired{Version: desired.Version, Providers: make(map[policy.MappingID]policy.Mapping, len(desired.Providers))}
	for id, mapping := range desired.Providers {
		models := make(map[string]policy.ModelBaseline, len(mapping.Models))
		for name, baseline := range mapping.Models {
			models[name] = baseline
		}
		mapping.Models = models
		copyDesired.Providers[id] = mapping
	}
	copyTarget := target
	copyTarget.Definitions = make([]policy.Definition, len(target.Definitions))
	copy(copyTarget.Definitions, target.Definitions)
	for i := range copyTarget.Definitions {
		copyTarget.Definitions[i].Chain = append(policy.Chain(nil), target.Definitions[i].Chain...)
	}
	copyTarget.Full = append(policy.Chain(nil), target.Full...)
	copyTarget.Mini = append(policy.Chain(nil), target.Mini...)
	copyTarget.Nano = append(policy.Chain(nil), target.Nano...)
	copyTarget.Classifier = append(policy.Chain(nil), target.Classifier...)
	return ownershipInputSnapshot{desired: copyDesired, target: copyTarget}
}
func writeOwnershipFixture(t *testing.T, root, rel, model string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("---\npolytoken:\n  model: %s\n---\nsynthetic fixture\n", model)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func snapshotOwnershipFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot files: %v", err)
	}
	return out
}
