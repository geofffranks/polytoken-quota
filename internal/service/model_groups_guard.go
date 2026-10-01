package service

// The legacy modelgroups guard. Polytoken rejects a version-4 config that
// combines a legacy tier default with an explicit model-group definition, and
// the staged merge composes the global and project layers, so a modelgroups
// key on ANY registered layer makes every legacy tier-default write an invalid
// candidate — including the shared global plan that project staging applies to
// its merged validation copies. Before each legacy transaction the coordinator
// detects that key on the registered roots' live config.yaml files and stamps
// every target, so the reconciler leaves the tier-default fields
// operator-owned and reports the skipped fields instead.
//
// Provider-only reconcile never runs this: its gate path derives edits from
// the raw global layer and never writes tier defaults, so whether the composed
// candidate is valid is decided by staged validation through the real
// Polytoken binary and the pre-publication snapshot recheck.

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// layerConfigFile names the per-layer Polytoken config document the detection
// reads; it matches the staging layer reader's root config.yaml.
const layerConfigFile = "config.yaml"

// applyModelGroupsGuard detects modelgroups across the resolved targets' live
// layer configs and stamps every target accordingly. It mutates the slice
// elements in place; callers pass the freshly resolved slice they thread
// through the rest of the transaction. Provider-only transactions never call
// this.
func applyModelGroupsGuard(targets []RegisteredTarget) {
	if !detectModelGroups(targets) {
		return
	}
	for i := range targets {
		targets[i].Policy.UsesModelGroups = true
	}
}

// detectModelGroups reports whether any registered layer's live config.yaml —
// the global root's or any registered project root's — defines a top-level
// modelgroups key.
//
// A layer whose config.yaml is absent, unreadable, or unparseable contributes
// no detection (fail-open): staging and validation reject such layers
// downstream exactly as before, so detection never widens its own failure
// surface and legacy configs without modelgroups keep today's byte-for-byte
// behavior.
func detectModelGroups(targets []RegisteredTarget) bool {
	for _, rt := range targets {
		raw, err := os.ReadFile(filepath.Join(rt.Resolved.CanonicalRoot, layerConfigFile))
		if err != nil {
			continue
		}
		if hasTopLevelModelGroups(raw) {
			return true
		}
	}
	return false
}

// hasTopLevelModelGroups reports whether the document's top-level mapping
// carries a modelgroups key. Anything unparseable reads as no.
func hasTopLevelModelGroups(raw []byte) bool {
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false
	}
	_, ok := doc["modelgroups"]
	return ok
}
