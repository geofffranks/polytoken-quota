package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// TestPrepareHistoryQualification verifies that BuildPrepareResult correctly
// identifies which managed files have proven byte-level changes by comparing
// staged candidate hashes against live files, and that ChangedEdits and
// Replacements are filtered accordingly.
func TestPrepareHistoryQualification(t *testing.T) {
	liveDir := t.TempDir()
	stagedDir := t.TempDir()

	// file-a: staged content differs from live → changed
	mustWrite(t, filepath.Join(liveDir, "config.yaml"), "old-content-a")
	mustWrite(t, filepath.Join(stagedDir, "config.yaml"), "new-content-a")

	// file-b: staged content identical to live → NOT changed
	mustWrite(t, filepath.Join(liveDir, "definitions/provider.yaml"), "same-content-b")
	mustWrite(t, filepath.Join(stagedDir, "definitions/provider.yaml"), "same-content-b")

	// file-c: no live file (first publish) → changed
	mustWrite(t, filepath.Join(stagedDir, "definitions/project.yaml"), "new-content-c")

	plan := reconcile.Plan{
		Edits: []reconcile.FieldEdit{
			{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("gpt-5.6")},
			{File: "config.yaml", Path: []string{"defaults", "mini"}, Scalar: strPtr("gpt-5.6-mini")},
			{File: "definitions/provider.yaml", Path: []string{"models", "codex/gpt-5.6-sol", "enabled"}, Enabled: boolPtr(true)},
			{File: "definitions/project.yaml", Path: []string{"models", "codex/gpt-5.6-pro", "enabled"}, Enabled: boolPtr(false)},
		},
	}

	result, err := BuildPrepareResult("global", plan, liveDir, stagedDir)
	if err != nil {
		t.Fatalf("BuildPrepareResult: %v", err)
	}

	// ChangedFiles: config.yaml and definitions/project.yaml changed;
	// definitions/provider.yaml did not.
	if !result.ChangedFiles["config.yaml"] {
		t.Error("config.yaml should be in ChangedFiles (content differs)")
	}
	if result.ChangedFiles["definitions/provider.yaml"] {
		t.Error("definitions/provider.yaml should NOT be in ChangedFiles (content identical)")
	}
	if !result.ChangedFiles["definitions/project.yaml"] {
		t.Error("definitions/project.yaml should be in ChangedFiles (no live file)")
	}

	// HasProvenChange: at least one changed file.
	if !result.HasProvenChange() {
		t.Error("HasProvenChange should be true with at least one changed file")
	}

	// ChangedEdits: only edits for changed files.
	if len(result.ChangedEdits) != 3 {
		t.Fatalf("ChangedEdits: got %d, want 3 (2 from config.yaml + 1 from project.yaml)", len(result.ChangedEdits))
	}
	for _, fe := range result.ChangedEdits {
		if fe.File == "definitions/provider.yaml" {
			t.Error("definitions/provider.yaml edits should be excluded from ChangedEdits")
		}
	}

	// Replacements: equal-hash entries dropped.
	// 3 unique files → 3 raw replacements, but provider.yaml has equal hash → dropped.
	if len(result.Replacements) != 2 {
		t.Fatalf("Replacements: got %d, want 2 (provider.yaml dropped, equal-hash)", len(result.Replacements))
	}
}

// TestPrepareNoProvenChange verifies that when all staged files match live
// files, HasProvenChange is false and ChangedEdits is empty.
func TestPrepareNoProvenChange(t *testing.T) {
	liveDir := t.TempDir()
	stagedDir := t.TempDir()

	content := "identical"
	mustWrite(t, filepath.Join(liveDir, "config.yaml"), content)
	mustWrite(t, filepath.Join(stagedDir, "config.yaml"), content)

	plan := reconcile.Plan{
		Edits: []reconcile.FieldEdit{
			{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("gpt-5.6")},
		},
	}

	result, err := BuildPrepareResult("global", plan, liveDir, stagedDir)
	if err != nil {
		t.Fatalf("BuildPrepareResult: %v", err)
	}

	if result.HasProvenChange() {
		t.Error("HasProvenChange should be false when all files match")
	}
	if len(result.ChangedEdits) != 0 {
		t.Errorf("ChangedEdits should be empty, got %d", len(result.ChangedEdits))
	}
	if len(result.Replacements) != 0 {
		t.Errorf("Replacements should be empty (all equal-hash), got %d", len(result.Replacements))
	}
}

// TestPrepareEmptyPlan verifies that an empty plan produces a valid result
// with no changes.
func TestPrepareEmptyPlan(t *testing.T) {
	result, err := BuildPrepareResult("global", reconcile.Plan{}, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("BuildPrepareResult: %v", err)
	}
	if result.HasProvenChange() {
		t.Error("HasProvenChange should be false for empty plan")
	}
	if !result.PlanComputed {
		t.Error("PlanComputed should be true even for an empty plan")
	}
}

// TestDropEqualHashReplacements verifies that equal-hash entries are dropped
// while differing-hash entries are retained.
func TestDropEqualHashReplacements(t *testing.T) {
	zero := [32]byte{}
	nz := [32]byte{1}
	reps := []publishReplacement{
		{LivePath: "a", OldHash: zero, NewHash: zero}, // equal → dropped
		{LivePath: "b", OldHash: zero, NewHash: nz},   // differ → kept
		{LivePath: "c", OldHash: nz, NewHash: nz},     // equal → dropped
	}
	out := DropEqualHashReplacements(reps)
	if len(out) != 1 {
		t.Fatalf("DropEqualHashReplacements: got %d, want 1", len(out))
	}
	if out[0].LivePath != "b" {
		t.Errorf("expected LivePath b, got %s", out[0].LivePath)
	}
}

// TestHasProvenChangeAcrossTargets verifies the aggregate check across
// multiple preparation results.
func TestHasProvenChangeAcrossTargets(t *testing.T) {
	noChange := PrepareResult{ChangedFiles: map[string]bool{"f": false}}
	hasChange := PrepareResult{ChangedFiles: map[string]bool{"f": true}}

	if HasProvenChangeAcrossTargets([]PrepareResult{noChange}) {
		t.Error("should be false when no target has changes")
	}
	if !HasProvenChangeAcrossTargets([]PrepareResult{noChange, hasChange}) {
		t.Error("should be true when at least one target has changes")
	}
	if HasProvenChangeAcrossTargets(nil) {
		t.Error("should be false for nil input")
	}
}

// TestProjectEdits verifies that ProjectEdits converts FieldEdits to EditDetail
// and filters by changedFiles when provided.
func TestProjectEdits(t *testing.T) {
	edits := []reconcile.FieldEdit{
		{File: "config.yaml", Path: []string{"defaults", "full"}, Scalar: strPtr("gpt-5.6")},
		{File: "definitions/provider.yaml", Path: []string{"models", "codex/gpt-5.6-sol", "enabled"}, Enabled: boolPtr(true)},
		{File: "config.yaml", Path: []string{"models", "codex/gpt-5.6", "enabled"}, Remove: true},
	}

	// nil changedFiles → all edits included
	all := ProjectEdits(edits, nil)
	if len(all) != 3 {
		t.Fatalf("ProjectEdits(nil): got %d, want 3", len(all))
	}
	if all[0].Action != state.EditSetScalar || all[0].Detail != "gpt-5.6" {
		t.Errorf("edit 0: action=%s detail=%q", all[0].Action, all[0].Detail)
	}
	if all[1].Action != state.EditSetBool || all[1].Detail != "true" {
		t.Errorf("edit 1: action=%s detail=%q", all[1].Action, all[1].Detail)
	}
	if all[2].Action != state.EditRemove || all[2].Detail != "removed" {
		t.Errorf("edit 2: action=%s detail=%q", all[2].Action, all[2].Detail)
	}

	// changedFiles filtering: only config.yaml changed
	filtered := ProjectEdits(edits, map[string]bool{"config.yaml": true})
	if len(filtered) != 2 {
		t.Fatalf("ProjectEdits(filtered): got %d, want 2", len(filtered))
	}
	for _, d := range filtered {
		if d.File != "config.yaml" {
			t.Errorf("expected only config.yaml edits, got %s", d.File)
		}
	}
}

// TestProjectEditsSequence verifies sequence edit conversion.
func TestProjectEditsSequence(t *testing.T) {
	edits := []reconcile.FieldEdit{
		{File: "config.yaml", Path: []string{"models"}, Sequence: []string{"a", "b", "c"}},
	}
	details := ProjectEdits(edits, nil)
	if len(details) != 1 {
		t.Fatalf("got %d details, want 1", len(details))
	}
	if details[0].Action != state.EditSetSequence {
		t.Errorf("action=%s, want %s", details[0].Action, state.EditSetSequence)
	}
	if details[0].Detail == "" {
		t.Error("sequence detail should not be empty")
	}
}

// TestBuildTraceDelegatesToProjections verifies that buildTrace produces
// provider mode and edit reports consistent with the pure projections.
func TestBuildTraceDelegatesToProjections(t *testing.T) {
	// buildTrace must produce reports whose content matches the projections
	// converted to report types. We verify that a non-empty edit set round-trips
	// through both paths identically.
	edits := []reconcile.FieldEdit{
		{File: "config.yaml", Path: []string{"x"}, Scalar: strPtr("val")},
	}
	plan := reconcile.Plan{Edits: edits}

	// Via buildTrace
	tr := buildTrace(policy.Desired{}, state.State{}, policy.Target{}, nil, nil, plan)

	// Via projections directly
	editDetails := ProjectEdits(edits, nil)
	for i, d := range editDetails {
		if !reflect.DeepEqual(tr.Edits[i].File, d.File) {
			t.Errorf("edit %d file mismatch: trace=%s projection=%s", i, tr.Edits[i].File, d.File)
		}
		if !reflect.DeepEqual(tr.Edits[i].Path, d.Path) {
			t.Errorf("edit %d path mismatch", i)
		}
		if string(tr.Edits[i].Action) != string(d.Action) {
			t.Errorf("edit %d action mismatch: trace=%s projection=%s", i, tr.Edits[i].Action, d.Action)
		}
		if tr.Edits[i].Detail != d.Detail {
			t.Errorf("edit %d detail mismatch: trace=%q projection=%q", i, tr.Edits[i].Detail, d.Detail)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }
