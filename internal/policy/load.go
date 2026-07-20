package policy

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// supportedVersion is the only desired.yaml schema version this build understands.
const supportedVersion = 1

// defaultOperational is applied when desired.yaml omits the operational section.
var defaultOperational = Operational{
	ValidationTimeout:  30 * time.Second,
	LockWait:           10 * time.Second,
	RecoveredRetention: 7 * 24 * time.Hour,
	BackupCount:        5,
}

// Load reads and validates desired.yaml at path, returning a fully resolved Desired
// graph. It rejects unsupported versions, mappings without concrete model
// enumeration, duplicate/non-concrete model names, models assigned to conflicting
// mappings, ambiguous provider assignments, unresolved desired chains, and invalid
// operational bounds. Resolution is exact; it never guesses using similar names.
func Load(path string) (Desired, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Desired{}, fmt.Errorf("policy: read %s: %w", path, err)
	}
	return loadBytes(data)
}

func loadBytes(data []byte) (Desired, error) {
	var w docWire
	if err := yaml.Unmarshal(data, &w); err != nil {
		return Desired{}, fmt.Errorf("policy: parse: %w", err)
	}
	if w.Version != supportedVersion {
		return Desired{}, fmt.Errorf("policy: unsupported or missing version %d (want %d)", w.Version, supportedVersion)
	}

	d := Desired{Version: w.Version, Providers: map[MappingID]Mapping{}}

	// Build provider mappings. modelOwner maps a base model to the single mapping
	// that owns it; the codexbar/polytoken owners detect ambiguity across mappings.
	// Iterating sorted keys keeps error ordering deterministic.
	modelOwner := map[string]MappingID{}
	codexbarOwner := map[string]MappingID{}
	polytokenOwner := map[string]MappingID{}
	ids := make([]string, 0, len(w.Providers))
	for id := range w.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)

	for _, idStr := range ids {
		id := MappingID(idStr)
		mw := w.Providers[idStr]
		if len(mw.Models) == 0 {
			return Desired{}, fmt.Errorf("policy: mapping %q must enumerate concrete models", id)
		}
		m := Mapping{
			CodexBarProviders:  append([]string(nil), mw.CodexBarProviders...),
			PolytokenProviders: append([]string(nil), mw.PolytokenProviders...),
			Models:             map[string]ModelBaseline{},
		}
		for _, entry := range mw.Models {
			base := entry.name
			if base == "" {
				return Desired{}, fmt.Errorf("policy: mapping %q has an empty model name", id)
			}
			if isGlob(base) {
				return Desired{}, fmt.Errorf("policy: mapping %q has a non-concrete model name %q", id, base)
			}
			if _, dup := m.Models[base]; dup {
				return Desired{}, fmt.Errorf("policy: mapping %q lists duplicate model %q", id, base)
			}
			mb := ModelBaseline{Enabled: true, HadEnabledKey: false}
			if entry.hasEnabled && entry.enabled != nil {
				mb.Enabled = *entry.enabled
				mb.HadEnabledKey = true
			}
			m.Models[base] = mb
		}
		// A model must belong to exactly one mapping.
		for base := range m.Models {
			if other, ok := modelOwner[base]; ok {
				return Desired{}, fmt.Errorf("policy: model %q is assigned to conflicting mappings %q and %q", base, other, id)
			}
			modelOwner[base] = id
		}
		for _, cb := range m.CodexBarProviders {
			if other, ok := codexbarOwner[cb]; ok && other != id {
				return Desired{}, fmt.Errorf("policy: codexbar provider %q is ambiguous across mappings %q and %q", cb, other, id)
			}
			codexbarOwner[cb] = id
		}
		for _, pt := range m.PolytokenProviders {
			if other, ok := polytokenOwner[pt]; ok && other != id {
				return Desired{}, fmt.Errorf("policy: polytoken provider %q is ambiguous across mappings %q and %q", pt, other, id)
			}
			polytokenOwner[pt] = id
		}
		d.Providers[id] = m
	}

	op, err := operationalFromWire(w.Operational)
	if err != nil {
		return Desired{}, err
	}
	d.Operational = op

	if d.Global, err = targetFromWire(w.Global, true, modelOwner); err != nil {
		return Desired{}, fmt.Errorf("policy: global target: %w", err)
	}
	d.Projects = make([]Target, 0, len(w.Projects))
	for i, pw := range w.Projects {
		t, err := targetFromWire(&pw, false, modelOwner)
		if err != nil {
			return Desired{}, fmt.Errorf("policy: project %d: %w", i, err)
		}
		d.Projects = append(d.Projects, t)
	}
	return d, nil
}

// ResolveModel resolves a base model name to exactly one provider mapping by exact
// match. It returns an error for unknown models and never matches by similarity.
// (Load already rejects conflicting ownership, so at most one mapping owns a base.)
func (d Desired) ResolveModel(base string) (MappingID, error) {
	var found MappingID
	count := 0
	for id, m := range d.Providers {
		if _, ok := m.Models[base]; ok {
			found = id
			count++
		}
	}
	switch count {
	case 0:
		return "", fmt.Errorf("policy: unresolved model %q", base)
	case 1:
		return found, nil
	default:
		return "", fmt.Errorf("policy: model %q is ambiguous across mappings", base)
	}
}

// ResolveCodexBar resolves a CodExBar event provider ID to its mapping. It is the
// event-time counterpart to ResolveModel: an event whose provider is not bound by
// any mapping is rejected. Like ResolveModel it performs exact match only.
func (d Desired) ResolveCodexBar(id string) (MappingID, error) {
	var found MappingID
	count := 0
	for mid, m := range d.Providers {
		for _, cb := range m.CodexBarProviders {
			if cb == id {
				found = mid
				count++
				break
			}
		}
	}
	switch count {
	case 0:
		return "", fmt.Errorf("policy: unknown codexbar provider %q", id)
	case 1:
		return found, nil
	default:
		return "", fmt.Errorf("policy: codexbar provider %q is ambiguous across mappings", id)
	}
}

// baseOf returns the base model portion of a chain entry, stripping any reasoning
// suffix introduced by `(`. Bare entries are returned unchanged.
func baseOf(entry string) string {
	if i := strings.IndexByte(entry, '('); i >= 0 {
		return entry[:i]
	}
	return entry
}

// isGlob reports whether name contains a wildcard character, marking it a
// non-concrete pattern rather than an exact enumerated model.
func isGlob(name string) bool {
	return strings.ContainsAny(name, "*?[")
}

// validateChain ensures every entry in a desired chain resolves to a managed base
// model in the graph, normalizing away reasoning suffixes first.
func validateChain(modelOwner map[string]MappingID, c Chain) error {
	for _, entry := range c {
		base := baseOf(entry)
		if _, ok := modelOwner[base]; !ok {
			return fmt.Errorf("unresolved model %q (entry %q)", base, entry)
		}
	}
	return nil
}

func targetFromWire(w *targetWire, global bool, modelOwner map[string]MappingID) (Target, error) {
	if w == nil {
		return Target{Global: global}, nil
	}
	t := Target{
		ID:         w.ID,
		Root:       w.Root,
		Global:     global,
		Full:       append(Chain(nil), w.Full...),
		Mini:       append(Chain(nil), w.Mini...),
		Nano:       append(Chain(nil), w.Nano...),
		Classifier: append(Chain(nil), w.Classifier...),
	}
	for _, c := range []struct {
		name  string
		chain Chain
	}{
		{"full", t.Full}, {"mini", t.Mini}, {"nano", t.Nano}, {"classifier", t.Classifier},
	} {
		if err := validateChain(modelOwner, c.chain); err != nil {
			return Target{}, fmt.Errorf("chain %q: %w", c.name, err)
		}
	}
	for _, dw := range w.Definitions {
		if err := validateChain(modelOwner, dw.Chain); err != nil {
			return Target{}, fmt.Errorf("definition %q: %w", dw.Path, err)
		}
		t.Definitions = append(t.Definitions, Definition{Path: dw.Path, Chain: append(Chain(nil), dw.Chain...)})
	}
	return t, nil
}

func operationalFromWire(w *operationalWire) (Operational, error) {
	if w == nil {
		return defaultOperational, nil
	}
	vt, err := parseDur("validation_timeout", w.ValidationTimeout, defaultOperational.ValidationTimeout)
	if err != nil {
		return Operational{}, err
	}
	lw, err := parseDur("lock_wait", w.LockWait, defaultOperational.LockWait)
	if err != nil {
		return Operational{}, err
	}
	rr, err := parseDur("recovered_retention", w.RecoveredRetention, defaultOperational.RecoveredRetention)
	if err != nil {
		return Operational{}, err
	}
	bc := w.BackupCount
	if bc <= 0 {
		return Operational{}, fmt.Errorf("policy: operational backup_count must be >= 1, got %d", bc)
	}
	if vt <= 0 || lw <= 0 || rr <= 0 {
		return Operational{}, errors.New("policy: operational durations must be positive")
	}
	return Operational{ValidationTimeout: vt, LockWait: lw, RecoveredRetention: rr, BackupCount: bc}, nil
}

func parseDur(field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("policy: operational %s %q: %w", field, s, err)
	}
	return d, nil
}

// --- YAML wire types -------------------------------------------------------

// docWire is the on-disk shape of desired.yaml. It differs from the in-memory
// Desired only where custom parsing is needed (model enumeration and durations).
type docWire struct {
	Version     int                    `yaml:"version"`
	Providers   map[string]mappingWire `yaml:"providers"`
	Global      *targetWire            `yaml:"global"`
	Projects    []targetWire           `yaml:"projects"`
	Operational *operationalWire       `yaml:"operational"`
}

type mappingWire struct {
	CodexBarProviders  []string    `yaml:"codexbar_providers"`
	PolytokenProviders []string    `yaml:"polytoken_providers"`
	Models             []modelWire `yaml:"models"`
}

// modelWire is one entry in a mapping's models sequence. It accepts a bare name
// (default enabled, no explicit key) or `name: {enabled: bool}` (records the
// explicit enabled origin).
type modelWire struct {
	name       string
	enabled    *bool
	hasEnabled bool
}

func (m *modelWire) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		m.name = value.Value
		return nil
	case yaml.MappingNode:
		if len(value.Content) != 2 {
			return errors.New("policy: model entry must have a single name key")
		}
		m.name = value.Content[0].Value
		vn := value.Content[1]
		if vn.Kind == yaml.ScalarNode && vn.Tag == "!!null" {
			return nil // name with no explicit enabled state
		}
		if vn.Kind != yaml.MappingNode {
			return fmt.Errorf("policy: model entry %q value must be {enabled: bool}", m.name)
		}
		var v struct {
			Enabled *bool `yaml:"enabled"`
		}
		if err := vn.Decode(&v); err != nil {
			return fmt.Errorf("policy: model entry %q: %w", m.name, err)
		}
		m.enabled = v.Enabled
		m.hasEnabled = v.Enabled != nil
		return nil
	default:
		return errors.New("policy: model entry must be a name or name: {enabled: bool}")
	}
}

type operationalWire struct {
	ValidationTimeout  string `yaml:"validation_timeout"`
	LockWait           string `yaml:"lock_wait"`
	RecoveredRetention string `yaml:"recovered_retention"`
	BackupCount        int    `yaml:"backup_count"`
}

type targetWire struct {
	ID          string           `yaml:"id"`
	Root        string           `yaml:"root"`
	Full        Chain            `yaml:"full"`
	Mini        Chain            `yaml:"mini"`
	Nano        Chain            `yaml:"nano"`
	Classifier  Chain            `yaml:"classifier"`
	Definitions []definitionWire `yaml:"definitions"`
}

type definitionWire struct {
	Path  string `yaml:"path"`
	Chain Chain  `yaml:"chain"`
}
