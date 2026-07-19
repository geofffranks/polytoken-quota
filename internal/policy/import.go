package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/geofffranks/codexbar-hooks/internal/state"
	"gopkg.in/yaml.v3"
)

// This file implements the durable desired-policy workflows: Init (a strict
// create-only starter proposal), guarded Import (sync --from-polytoken), drift
// detection, and the Writer that performs exclusive-create and atomic-replace
// file operations. None of these perform CodExBar writes; init/import only
// produce or adopt desired intent and report drift.

// ErrDesiredExists signals that desired.yaml already exists and a strict
// create-only operation refused to overwrite it. The remediation is
// `sync --from-polytoken`.
var ErrDesiredExists = errors.New("desired.yaml already exists; use sync --from-polytoken")

// SourceConfig is the parsed config.yaml layer of one target: the explicit
// provider mappings (each enumerating its concrete managed models with their
// baseline enabled state) and the scalar managed chains (full/mini/nano/
// classifier). It carries exact, concrete model names verbatim from source; no
// implicit runtime model mapping is performed.
type SourceConfig struct {
	Providers  []SourceMapping
	Full       Chain
	Mini       Chain
	Nano       Chain
	Classifier Chain
}

// SourceMapping is one provider mapping read from source config: a set of
// CodExBar provider IDs bound to a set of Polytoken provider IDs, plus the exact
// concrete base models managed by that binding and their baseline enabled state.
type SourceMapping struct {
	ID                 string
	CodexBarProviders  []string
	PolytokenProviders []string
	Models             map[string]ModelBaseline
}

// SourceDefinition is one discovered definition file with its live managed model
// fields. Model is the live polytoken.model ("" when absent); FallbackModels is
// the live polytoken.fallback_models (nil when absent). A definition with neither
// is not model-bearing and is skipped by Init/Import.
type SourceDefinition struct {
	Path           string
	Model          string
	FallbackModels []string
}

// SourceSet is one target's parsed Polytoken source: its config layer and the
// model-bearing definition files discovered within its root.
type SourceSet struct {
	ID          string
	Root        string
	Global      bool
	Config      SourceConfig
	Definitions []SourceDefinition
}

// SourceReader abstracts reading real Polytoken source files (the global config
// plus registered project definitions). The production implementation reads from
// the filesystem; tests supply fixtures. It performs no mutations.
type SourceReader interface {
	Global(context.Context) (SourceSet, error)
	Projects(context.Context) ([]SourceSet, error)
}

// Drift is one managed live difference between desired intent and the observed
// live value. Managed drift is reported by Import, never silently adopted.
type Drift struct {
	TargetID string
	File     string
	Field    string
	Desired  []string
	Live     []string
}

// Reference names a model-bearing definition or a model entry. Uncovered
// references are model-bearing definitions whose chain does not resolve against
// the provider graph — surfaced to doctor.
type Reference struct {
	TargetID string
	File     string
	Model    string
}

// ImportReport summarizes an Init/Import proposal: managed drift detected,
// newly model-bearing unregistered/unresolvable definitions (Uncovered), and
// advisory warnings.
type ImportReport struct {
	Drift     []Drift
	Uncovered []Reference
	Warnings  []string
}

// Writer durably writes desired.yaml. CreateAtomic is a strict exclusive create
// that refuses an existing file; ReplaceAtomic performs a same-filesystem
// atomic replacement of the policy.
type Writer interface {
	CreateAtomic(context.Context, Desired) error
	ReplaceAtomic(context.Context, Desired) error
}

// --- proposal core ----------------------------------------------------------

// offGraphRef records a managed reference whose base model is not enumerated in
// the provider graph. Init surfaces these as Uncovered references; Import treats
// them as ambiguous drift.
type offGraphRef struct {
	TargetID string
	File     string
	Field    string
	Live     []string
}

// Init proposes a starter desired policy from live Polytoken sources without
// writing anything. It discovers global managed references and baseline model
// enablement, proposes only definitions that carry polytoken.model or
// polytoken.fallback_models, materializes exact concrete model enumeration
// verbatim from the source provider mappings (no implicit runtime mapping), and
// reports any references that do not resolve as Uncovered. It performs no
// CodExBar writes and is strict create-only: persistence is the caller's job.
func Init(ctx context.Context, r SourceReader) (Desired, ImportReport, error) {
	d, off, err := propose(ctx, r)
	if err != nil {
		return Desired{}, ImportReport{}, err
	}
	report := ImportReport{}
	for _, og := range off {
		report.Uncovered = append(report.Uncovered, Reference{
			TargetID: og.TargetID,
			File:     og.File,
			Model:    stringsJoin(og.Live, ", "),
		})
	}
	return d, report, nil
}

// Import adopts current managed fields as desired intent, subject to guards. It
// refuses while any provider is degraded (effective mode other than normal) or
// when managed drift is ambiguous (a managed definition references a model
// outside the provider graph) unless force is set. When forced it emits a
// warning that the temporary live ordering may become durable intent. Managed
// drift is reported, never silently adopted; unmanaged live content is
// preserved/ignored. It performs no CodExBar writes.
func Import(ctx context.Context, r SourceReader, s state.State, force bool) (Desired, ImportReport, error) {
	d, off, err := propose(ctx, r)
	if err != nil {
		return Desired{}, ImportReport{}, err
	}
	report := ImportReport{}
	for _, og := range off {
		report.Drift = append(report.Drift, Drift{
			TargetID: og.TargetID,
			File:     og.File,
			Field:    og.Field,
			Live:     og.Live,
		})
	}
	degraded := anyDegraded(s)
	ambiguous := len(off) > 0
	if (degraded || ambiguous) && !force {
		return Desired{}, report, errors.New("policy: import refused: provider degraded or managed drift is ambiguous; rerun with --force")
	}
	if force {
		report.Warnings = append(report.Warnings,
			"import forced: temporary ordering may become durable intent")
	}
	return d, report, nil
}

// propose reads sources and builds a valid desired proposal. Definitions whose
// chains resolve against the provider graph are included; model-bearing
// definitions whose entries are off-graph are reported and excluded so the
// returned Desired is valid by construction. It performs no state-dependent
// guarding.
func propose(ctx context.Context, r SourceReader) (Desired, []offGraphRef, error) {
	global, err := r.Global(ctx)
	if err != nil {
		return Desired{}, nil, fmt.Errorf("policy: read global source: %w", err)
	}
	projects, err := r.Projects(ctx)
	if err != nil {
		return Desired{}, nil, fmt.Errorf("policy: read project sources: %w", err)
	}

	d := Desired{Version: supportedVersion, Providers: map[MappingID]Mapping{}, Operational: defaultOperational}
	owner := map[string]MappingID{}
	if err := buildProviders(&d, global.Config.Providers, owner); err != nil {
		return Desired{}, nil, err
	}

	var offGraph []offGraphRef
	gt, goff := buildTarget(global, owner)
	d.Global = gt
	offGraph = append(offGraph, goff...)
	d.Projects = make([]Target, 0, len(projects))
	for _, p := range projects {
		t, off := buildTarget(p, owner)
		offGraph = append(offGraph, off...)
		d.Projects = append(d.Projects, t)
	}
	return d, offGraph, nil
}

// buildProviders copies the source provider mappings verbatim into the proposal,
// enforcing concrete, non-duplicate model enumeration and single ownership of
// each base model. Exact names are preserved; there is no similarity matching.
func buildProviders(d *Desired, providers []SourceMapping, owner map[string]MappingID) error {
	ids := make([]string, 0, len(providers))
	byID := make(map[string]SourceMapping, len(providers))
	for _, sm := range providers {
		if _, dup := byID[sm.ID]; dup {
			return fmt.Errorf("policy: duplicate provider mapping %q", sm.ID)
		}
		ids = append(ids, sm.ID)
		byID[sm.ID] = sm
	}
	sort.Strings(ids)
	for _, id := range ids {
		sm := byID[id]
		if len(sm.Models) == 0 {
			return fmt.Errorf("policy: mapping %q must enumerate concrete models", id)
		}
		m := Mapping{
			CodexBarProviders:  append([]string(nil), sm.CodexBarProviders...),
			PolytokenProviders: append([]string(nil), sm.PolytokenProviders...),
			Models:             map[string]ModelBaseline{},
		}
		bases := make([]string, 0, len(sm.Models))
		for base := range sm.Models {
			bases = append(bases, base)
		}
		sort.Strings(bases)
		for _, base := range bases {
			if base == "" {
				return fmt.Errorf("policy: mapping %q has an empty model name", id)
			}
			if isGlob(base) {
				return fmt.Errorf("policy: mapping %q has a non-concrete model name %q", id, base)
			}
			m.Models[base] = sm.Models[base]
		}
		for base := range m.Models {
			if other, ok := owner[base]; ok {
				return fmt.Errorf("policy: model %q is assigned to conflicting mappings %q and %q", base, other, id)
			}
			owner[base] = MappingID(id)
		}
		d.Providers[MappingID(id)] = m
	}
	return nil
}

// buildTarget assembles one target from its source set, resolving scalar and
// definition chains against the provider graph. Off-graph entries are reported
// and dropped so the target's chains are valid.
func buildTarget(ss SourceSet, owner map[string]MappingID) (Target, []offGraphRef) {
	t := Target{ID: ss.ID, Root: ss.Root, Global: ss.Global}
	var off []offGraphRef
	t.Full, off = appendChain(off, ss.ID, "defaults.full", "config.yaml", ss.Config.Full, owner)
	t.Mini, off = appendChain(off, ss.ID, "defaults.mini", "config.yaml", ss.Config.Mini, owner)
	t.Nano, off = appendChain(off, ss.ID, "defaults.nano", "config.yaml", ss.Config.Nano, owner)
	t.Classifier, off = appendChain(off, ss.ID, "autonomous_permission_matcher.classifier_model", "config.yaml", ss.Config.Classifier, owner)

	for _, def := range ss.Definitions {
		chain := defChain(def)
		if len(chain) == 0 {
			continue // polytoken block but no model field — not model-bearing
		}
		kept, dropped := filterChain(ss.ID, chainField(def), def.Path, chain, owner)
		off = append(off, dropped...)
		if len(kept) == 0 {
			continue // entirely off-graph — uncovered, do not propose
		}
		t.Definitions = append(t.Definitions, Definition{Path: def.Path, Chain: kept})
	}
	return t, off
}

// defChain materializes a definition's live managed chain: polytoken.model first,
// then polytoken.fallback_models.
func defChain(def SourceDefinition) Chain {
	var c Chain
	if def.Model != "" {
		c = append(c, def.Model)
	}
	c = append(c, def.FallbackModels...)
	return c
}

// chainField names the managed field that contributed a definition's chain.
func chainField(def SourceDefinition) string {
	if def.Model != "" {
		return "polytoken.model"
	}
	return "polytoken.fallback_models"
}

// filterChain keeps entries that resolve to a managed base model and reports the
// rest as off-graph references. appendChain is the scalar-chain convenience.
func filterChain(targetID, field, file string, chain Chain, owner map[string]MappingID) (Chain, []offGraphRef) {
	var kept Chain
	var off []offGraphRef
	for _, entry := range chain {
		if _, ok := owner[baseOf(entry)]; ok {
			kept = append(kept, entry)
		} else {
			off = append(off, offGraphRef{TargetID: targetID, File: file, Field: field, Live: []string{entry}})
		}
	}
	return kept, off
}

func appendChain(off []offGraphRef, targetID, field, file string, chain Chain, owner map[string]MappingID) (Chain, []offGraphRef) {
	kept, dropped := filterChain(targetID, field, file, chain, owner)
	return kept, append(off, dropped...)
}

// anyDegraded reports whether any provider in observed state has an effective
// mode other than normal. Import refuses while any provider is degraded.
func anyDegraded(s state.State) bool {
	for _, ps := range s.Providers {
		if state.EffectiveMode(ps) != state.ModeNormal {
			return true
		}
	}
	return false
}

func stringsJoin(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// --- Writer -----------------------------------------------------------------

// fileWriter writes desired.yaml at path using exclusive-create and
// same-filesystem atomic rename.
type fileWriter struct {
	path string
}

// NewWriter returns a Writer that manages desired.yaml at path.
func NewWriter(path string) Writer { return &fileWriter{path: path} }

// CreateAtomic writes desired.yaml with an exclusive create (O_CREATE|O_EXCL),
// mode 0600, fsyncing the file and its directory for durability. If the file
// already exists it returns ErrDesiredExists and leaves the existing bytes
// untouched.
func (w *fileWriter) CreateAtomic(_ context.Context, d Desired) error {
	data, err := marshalDesired(d)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrDesiredExists, w.path)
		}
		return fmt.Errorf("policy: create %s: %w", w.path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("policy: write %s: %w", w.path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("policy: sync %s: %w", w.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("policy: close %s: %w", w.path, err)
	}
	return syncDir(filepath.Dir(w.path))
}

// ReplaceAtomic replaces the entire desired.yaml file with the new policy
// content using an atomic rename. This is a whole-file replacement, not
// field-level targeted editing — existing comments, key ordering, and
// unmanaged policy content are NOT preserved. The sync use case replaces the
// entire desired policy, so full reserialization is correct. For
// byte-preserving edits to live Polytoken files, use the document package.
//
// The serialized policy is written to a temp file in the target's directory
// (mode 0600), fsynced, renamed over the target, and the directory is fsynced,
// so a crash cannot leave a partially written file.
func (w *fileWriter) ReplaceAtomic(_ context.Context, d Desired) error {
	data, err := marshalDesired(d)
	if err != nil {
		return err
	}
	dir := filepath.Dir(w.path)
	tmp, err := os.CreateTemp(dir, ".desired.yaml.*")
	if err != nil {
		return fmt.Errorf("policy: create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("policy: write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("policy: chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("policy: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("policy: close temp: %w", err)
	}
	if err := os.Rename(tmpName, w.path); err != nil {
		cleanup()
		return fmt.Errorf("policy: rename %s: %w", w.path, err)
	}
	return syncDir(dir)
}

// syncDir fsyncs the directory holding a newly created/renamed file so the
// directory entry change is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("policy: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("policy: sync dir %s: %w", dir, err)
	}
	return nil
}

// --- desired serialization --------------------------------------------------

// marshalDesired renders a Desired to the on-disk desired.yaml shape. Provider
// models are emitted verbatim: a bare name for the default enabled baseline, or
// `name: {enabled: bool}` when an explicit enabled key was captured.
func marshalDesired(d Desired) ([]byte, error) {
	doc := outDoc{Version: d.Version, Providers: map[string]outMapping{}}
	for id, m := range d.Providers {
		om := outMapping{
			CodexBarProviders:  m.CodexBarProviders,
			PolytokenProviders: m.PolytokenProviders,
		}
		bases := make([]string, 0, len(m.Models))
		for base := range m.Models {
			bases = append(bases, base)
		}
		sort.Strings(bases)
		for _, base := range bases {
			om.Models = append(om.Models, modelOut{Name: base, MB: m.Models[base]})
		}
		doc.Providers[string(id)] = om
	}
	if ot := targetOut(d.Global); ot != nil {
		doc.Global = ot
	}
	for _, p := range d.Projects {
		if ot := targetOut(p); ot != nil {
			doc.Projects = append(doc.Projects, *ot)
		}
	}
	if d.Operational != (Operational{}) {
		doc.Operational = &outOperational{
			ValidationTimeout:  d.Operational.ValidationTimeout.String(),
			LockWait:           d.Operational.LockWait.String(),
			RecoveredRetention: d.Operational.RecoveredRetention.String(),
			BackupCount:        d.Operational.BackupCount,
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("policy: marshal desired: %w", err)
	}
	return buf.Bytes(), nil
}

func targetOut(t Target) *outTarget {
	empty := t.Root == "" && t.ID == "" && len(t.Definitions) == 0 &&
		len(t.Full) == 0 && len(t.Mini) == 0 && len(t.Nano) == 0 && len(t.Classifier) == 0
	if empty {
		return nil
	}
	ot := &outTarget{
		ID: t.ID, Root: t.Root,
		Full: t.Full, Mini: t.Mini, Nano: t.Nano, Classifier: t.Classifier,
	}
	for _, def := range t.Definitions {
		ot.Definitions = append(ot.Definitions, outDefinition{Path: def.Path, Chain: def.Chain})
	}
	return ot
}

// modelOut renders a managed model as a bare name (default enabled baseline) or
// `name: {enabled: bool}` (explicit enabled key).
type modelOut struct {
	Name string
	MB   ModelBaseline
}

func (m modelOut) MarshalYAML() (interface{}, error) {
	if m.MB.Enabled && !m.MB.HadEnabledKey {
		return m.Name, nil
	}
	return map[string]map[string]bool{m.Name: {"enabled": m.MB.Enabled}}, nil
}

type outDoc struct {
	Version     int                    `yaml:"version"`
	Providers   map[string]outMapping  `yaml:"providers,omitempty"`
	Global      *outTarget             `yaml:"global,omitempty"`
	Projects    []outTarget            `yaml:"projects,omitempty"`
	Operational *outOperational        `yaml:"operational,omitempty"`
}

type outMapping struct {
	CodexBarProviders  []string   `yaml:"codexbar_providers,omitempty"`
	PolytokenProviders []string   `yaml:"polytoken_providers,omitempty"`
	Models             []modelOut `yaml:"models"`
}

type outTarget struct {
	ID          string          `yaml:"id,omitempty"`
	Root        string          `yaml:"root,omitempty"`
	Full        Chain           `yaml:"full,omitempty"`
	Mini        Chain           `yaml:"mini,omitempty"`
	Nano        Chain           `yaml:"nano,omitempty"`
	Classifier  Chain           `yaml:"classifier,omitempty"`
	Definitions []outDefinition `yaml:"definitions,omitempty"`
}

type outDefinition struct {
	Path  string `yaml:"path"`
	Chain Chain  `yaml:"chain"`
}

type outOperational struct {
	ValidationTimeout  string `yaml:"validation_timeout"`
	LockWait           string `yaml:"lock_wait"`
	RecoveredRetention string `yaml:"recovered_retention"`
	BackupCount        int    `yaml:"backup_count"`
}
