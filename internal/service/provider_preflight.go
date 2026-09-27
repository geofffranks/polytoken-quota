package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/document"
	"github.com/geofffranks/polytoken-quota/internal/groupsafety"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
)

// ProviderPreflight is a read-only, all-registered-target assessment. Snapshots
// records the source bytes read for each registered config and managed definition.
// Ready is true only when every proposed disable is analyzer-safe and every full
// staged candidate passes config validation and doctor.
type ProviderPreflight struct {
	Ready     bool
	Reports   map[string]groupsafety.Report
	Snapshots []ProviderSourceSnapshot
}

// ProviderSourceSnapshot identifies one exact source file and its observed SHA-256.
type ProviderSourceSnapshot struct {
	TargetID string
	Path     string
	SHA256   [32]byte
}

// RecheckProviderPreflight verifies that every registered source file still has
// the bytes assessed by ProviderPreflight. A changed, missing, or newly
// unreadable source refuses the result; callers must retry the preflight.
func RecheckProviderPreflight(p ProviderPreflight) error {
	for _, snapshot := range p.Snapshots {
		data, err := os.ReadFile(snapshot.Path)
		if err != nil {
			return fmt.Errorf("service: preflight source changed or became unreadable for target %s", snapshot.TargetID)
		}
		if sha256.Sum256(data) != snapshot.SHA256 {
			return fmt.Errorf("service: preflight source changed for target %s; retry required", snapshot.TargetID)
		}
	}
	return nil
}

// PreflightProviderDisables assesses a proposed set of enrolled provider disables
// under the coordinator lock. It never recovers or publishes a journal, saves
// state, or modifies source files. Staging candidates are removed before return.
// Reconcile remains unsupported for provider-only policy in this slice.
func (c *Coordinator) PreflightProviderDisables(ctx context.Context, disable []string) (result ProviderPreflight, err error) {
	if c.Lock == nil || c.Policy == nil || c.State == nil || c.Targets == nil || c.Stage == nil || c.Validate == nil {
		return result, errors.New("service: provider preflight dependencies are incomplete")
	}
	unlock, err := c.Lock.Lock(ctx)
	if err != nil {
		return result, fmt.Errorf("service: acquire preflight lock: %w", err)
	}
	defer func() { _ = unlock() }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	desired, err := c.Policy.LoadPolicy()
	if err != nil {
		return result, err
	}
	if !desired.ProviderOnly() {
		return result, errors.New("service: provider preflight requires provider-only policy")
	}
	observed, err := c.State.LoadState()
	if err != nil {
		return result, fmt.Errorf("service: load preflight state: %w", err)
	}
	registered, err := c.Targets.ResolveTargets(desired)
	if err != nil {
		return result, fmt.Errorf("service: resolve preflight targets: %w", err)
	}
	if len(registered) == 0 || !registered[0].Resolved.Global {
		return result, errors.New("service: provider preflight requires a registered global target")
	}
	ids := append([]string(nil), disable...)
	sort.Strings(ids)
	if len(ids) == 0 {
		return result, errors.New("service: provider preflight requires at least one proposed disable")
	}
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] == id) {
			return result, errors.New("service: provider preflight disable set is empty or contains duplicates")
		}
		if _, ok := desired.Providers[policy.MappingID(id)]; !ok {
			return result, fmt.Errorf("service: provider %q is not enrolled", id)
		}
	}

	layers := make([]groupsafety.Layer, len(registered))
	for i, target := range registered {
		layer, readErr := groupsafety.ReadLayer(target.Resolved.ID, target.Resolved.CanonicalRoot, target.Resolved.Global)
		if readErr != nil {
			return result, readErr
		}
		layers[i] = layer
		if snapshotErr := appendProviderSnapshots(&result, target.Resolved.ID, target.Resolved.CanonicalRoot); snapshotErr != nil {
			return result, snapshotErr
		}
	}
	global := layers[0]
	if !global.Global {
		return result, errors.New("service: first preflight target is not global")
	}
	for _, layer := range layers[1:] {
		if layer.Global {
			return result, errors.New("service: more than one global preflight target")
		}
	}

	// Apply providers in deterministic order to an in-memory candidate graph so
	// later decisions see the combined proposal, not each provider in isolation.
	candidateLayers := append([]groupsafety.Layer(nil), layers...)
	result.Reports = make(map[string]groupsafety.Report, len(ids))
	for _, id := range ids {
		report := groupsafety.Report{Verdict: groupsafety.Safe, Reasons: []string{}}
		input := groupsafety.Input{Global: candidateLayers[0]}
		for enrolled := range desired.Providers {
			input.Enrolled = append(input.Enrolled, string(enrolled))
		}
		sort.Strings(input.Enrolled)
		globalReport := groupsafety.Analyze(input, id)
		report = mergeProviderReport(report, globalReport)
		for _, project := range candidateLayers[1:] {
			input := groupsafety.Input{Global: candidateLayers[0], Projects: []groupsafety.Layer{project}}
			for enrolled := range desired.Providers {
				input.Enrolled = append(input.Enrolled, string(enrolled))
			}
			sort.Strings(input.Enrolled)
			rootReport := groupsafety.Analyze(input, id)
			report = mergeProviderReport(report, rootReport)
		}
		result.Reports[id] = report
		if report.Verdict != groupsafety.Safe {
			return result, fmt.Errorf("service: provider %q preflight is %s", id, report.Verdict)
		}
		candidateLayers[0].Config, err = setProviderEnabled(candidateLayers[0].Config, id, false)
		if err != nil {
			return result, fmt.Errorf("service: construct combined provider edit plan: %w", err)
		}
	}

	edits := make([]reconcile.FieldEdit, 0, len(ids))
	for _, id := range ids {
		value := false
		edits = append(edits, reconcile.FieldEdit{File: "config.yaml", Path: []string{"providers", id, "enabled"}, Enabled: &value})
	}
	globalPlan := reconcile.Plan{TargetID: registered[0].Policy.ID, Revision: observed.Revision, Edits: edits}
	for i, target := range registered {
		plan := reconcile.Plan{TargetID: target.Policy.ID, Revision: observed.Revision}
		if i == 0 {
			plan = globalPlan
		}
		var projectGlobal *reconcile.Plan
		if i > 0 {
			projectGlobal = &globalPlan
		}
		candidate, stageErr := c.Stage.Stage(ctx, target.Resolved, plan, projectGlobal)
		if stageErr != nil {
			return result, fmt.Errorf("service: stage preflight target %s: %w", target.Resolved.ID, stageErr)
		}
		validation := c.Validate.Validate(ctx, candidate.WithoutCleanup(), validationTimeout(desired))
		cleanupErr := candidate.Cleanup()
		if cleanupErr != nil {
			return result, fmt.Errorf("service: clean preflight staging for target %s: %w", target.Resolved.ID, cleanupErr)
		}
		if !validation.StartupValid {
			if validation.Error != nil {
				return result, fmt.Errorf("service: preflight validation failed for target %s at %s", target.Resolved.ID, validation.Error.Stage)
			}
			return result, fmt.Errorf("service: preflight validation failed for target %s", target.Resolved.ID)
		}
	}
	if err := RecheckProviderPreflight(result); err != nil {
		return result, err
	}
	result.Ready = true
	return result, nil
}

func mergeProviderReport(total, next groupsafety.Report) groupsafety.Report {
	if next.Verdict == groupsafety.Unsafe || (next.Verdict == groupsafety.PendingUnknown && total.Verdict != groupsafety.Unsafe) {
		total.Verdict = next.Verdict
	}
	total.Reasons = append(total.Reasons, next.Reasons...)
	sort.Strings(total.Reasons)
	total.Reasons = compactPreflightStrings(total.Reasons)
	if total.GroupsBefore == nil {
		total.GroupsBefore = make(map[string][]string)
	}
	if total.GroupsAfter == nil {
		total.GroupsAfter = make(map[string][]string)
	}
	for name, leaves := range next.GroupsBefore {
		total.GroupsBefore[name] = append([]string(nil), leaves...)
	}
	for name, leaves := range next.GroupsAfter {
		total.GroupsAfter[name] = append([]string(nil), leaves...)
	}
	return total
}

func compactPreflightStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func validationTimeout(desired policy.Desired) time.Duration {
	if desired.Operational.ValidationTimeout > 0 {
		return desired.Operational.ValidationTimeout
	}
	return defaultValidationTimeout
}

func appendProviderSnapshots(result *ProviderPreflight, id, root string) error {
	configPath := filepath.Join(root, "config.yaml")
	paths := []string{configPath}
	files, err := policy.DiscoverManagedFiles(root)
	if err != nil {
		return fmt.Errorf("service: discover preflight source files for target %s", id)
	}
	for _, rel := range files {
		paths = append(paths, filepath.Join(root, filepath.FromSlash(rel)))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("service: read preflight source file for target %s", id)
		}
		result.Snapshots = append(result.Snapshots, ProviderSourceSnapshot{TargetID: id, Path: path, SHA256: sha256.Sum256(data)})
	}
	return nil
}

func setProviderEnabled(config []byte, id string, enabled bool) ([]byte, error) {
	value := enabled
	return document.EditYAML(config, []document.Edit{{
		Path: []string{"providers", id, "enabled"}, Kind: document.Boolean, Bool: &value,
	}})
}
