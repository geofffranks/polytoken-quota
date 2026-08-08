package policy

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/routing"
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

// defaultQuotaFreshness is the freshness TTL applied when a quota section omits
// freshness_ttl, matching the routing package's default.
const defaultQuotaFreshness = 30 * time.Minute

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
		// Optional per-provider quota/routing section. When absent it stays nil
		// (routing treats the mapping as unrankable). When present its schedule
		// (if any) is validated via routing.ParseSchedule so a bad schedule
		// rejects policy loading rather than being silently accepted.
		if mw.Quota != nil {
			qc, err := quotaFromWire(string(id), mw.Quota)
			if err != nil {
				return Desired{}, err
			}
			m.Quota = qc
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

	d.Routing = routingFromWire(w.Routing)

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

// ParseModelRef splits a chain entry into its base model and optional
// reasoning suffix, validating the spelling strictly: a suffixed entry must be
// exactly "base(suffix)" with a non-empty base, a non-empty suffix, and the
// closing parenthesis as the final character. This is the single canonical
// model-reference grammar — policy loading and reconciliation must agree, or a
// policy described as validated could later be rejected (or silently
// preserved malformed) at reconcile time.
func ParseModelRef(entry string) (base, suffix string, err error) {
	if entry == "" {
		return "", "", errors.New("policy: empty model reference")
	}
	open := strings.IndexByte(entry, '(')
	if open < 0 {
		if strings.ContainsAny(entry, ")") {
			return "", "", fmt.Errorf("policy: unbalanced suffix in %q", entry)
		}
		return entry, "", nil
	}
	if open == 0 {
		return "", "", fmt.Errorf("policy: empty base model in %q", entry)
	}
	if entry[len(entry)-1] != ')' {
		return "", "", fmt.Errorf("policy: malformed reasoning suffix in %q (must end with ')')", entry)
	}
	inner := entry[open+1 : len(entry)-1]
	if inner == "" || strings.ContainsAny(inner, "()") {
		return "", "", fmt.Errorf("policy: malformed reasoning suffix in %q", entry)
	}
	return entry[:open], inner, nil
}

// baseOf returns the base model portion of a chain entry, stripping any reasoning
// suffix introduced by `(`. Bare entries are returned unchanged. Callers that
// must reject malformed spellings use ParseModelRef instead.
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
		base, _, err := ParseModelRef(entry)
		if err != nil {
			return err
		}
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
	Routing     *routingWire           `yaml:"routing"`
}

type mappingWire struct {
	CodexBarProviders  []string    `yaml:"codexbar_providers"`
	PolytokenProviders []string    `yaml:"polytoken_providers"`
	Models             []modelWire `yaml:"models"`
	Quota              *quotaWire  `yaml:"quota"`
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

// routingWire is the on-disk shape of the top-level `routing` section.
type routingWire struct {
	Enabled bool `yaml:"enabled"`
}

// quotaWire is the on-disk shape of a mapping's `quota` section.
type quotaWire struct {
	Adapter          string        `yaml:"adapter"`
	FreshnessTTL     string        `yaml:"freshness_ttl"`
	BalanceGroup     string        `yaml:"balance_group"`
	Weight           int           `yaml:"weight"`
	MonthlyBudgetUSD float64       `yaml:"monthly_budget_usd"`
	Schedule         *scheduleWire `yaml:"schedule"`
}

// scheduleWire is the on-disk shape of a peak schedule. Peak windows are
// complemented into the internal off-peak representation used by ranking.
type scheduleWire struct {
	Timezone string           `yaml:"timezone"`
	Peak     []peakWindowWire `yaml:"peak"`
	OffPeak  []peakWindowWire `yaml:"off_peak"` // legacy key detected for migration errors
	peakSet  bool
	offSet   bool
}

func (s *scheduleWire) UnmarshalYAML(value *yaml.Node) error {
	type plain scheduleWire
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*s = scheduleWire(decoded)
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "peak":
			s.peakSet = true
		case "off_peak":
			s.offSet = true
		}
	}
	return nil
}

// peakWindowWire is one peak time window.
type peakWindowWire struct {
	Days  []string `yaml:"days"`
	Start string   `yaml:"start"`
	End   string   `yaml:"end"`
}

// routingFromWire translates the optional top-level routing section. A nil
// section yields routing disabled (the backward-compatible default).
func routingFromWire(w *routingWire) RoutingConfig {
	if w == nil {
		return RoutingConfig{}
	}
	return RoutingConfig{Enabled: w.Enabled}
}

// quotaFromWire translates a mapping's quota section into a QuotaConfig. The
// schedule, when present, is validated via routing.ParseSchedule so an invalid
// timezone/day/time rejects policy loading. FreshnessTTL defaults to 30m when
// omitted (matching the routing package's default), like the operational
// durations.
func quotaFromWire(mappingID string, w *quotaWire) (*QuotaConfig, error) {
	qc := &QuotaConfig{
		Adapter:          w.Adapter,
		FreshnessTTL:     defaultQuotaFreshness,
		BalanceGroup:     w.BalanceGroup,
		Weight:           w.Weight,
		MonthlyBudgetUSD: w.MonthlyBudgetUSD,
	}
	if math.IsNaN(w.MonthlyBudgetUSD) || math.IsInf(w.MonthlyBudgetUSD, 0) || w.MonthlyBudgetUSD < 0 {
		return nil, fmt.Errorf("policy: mapping %q: monthly_budget_usd must be finite and positive", mappingID)
	}
	// The anthropic adapter measures month-to-date spend against a
	// user-defined budget; without one there is nothing to measure against.
	if w.Adapter == "anthropic" && w.MonthlyBudgetUSD == 0 {
		return nil, fmt.Errorf("policy: mapping %q: the anthropic adapter requires monthly_budget_usd (the spend ceiling to treat as this provider's quota)", mappingID)
	}
	ttl, err := parseDur("freshness_ttl", w.FreshnessTTL, defaultQuotaFreshness)
	if err != nil {
		return nil, fmt.Errorf("policy: mapping %q: %w", mappingID, err)
	}
	qc.FreshnessTTL = ttl
	// Apply documented defaults for omitted quota fields at load time so the
	// resolved config matches its struct comments ("default" and 1).
	if qc.BalanceGroup == "" {
		qc.BalanceGroup = "default"
	}
	if qc.Weight == 0 {
		qc.Weight = 1
	}
	if w.Schedule != nil {
		s, err := scheduleFromWire(mappingID, w.Schedule)
		if err != nil {
			return nil, err
		}
		qc.Schedule = s
	}
	return qc, nil
}

// scheduleFromWire validates and builds a routing.Schedule from its wire form,
// delegating timezone/day/time validation to routing.ParseSchedule.
func scheduleFromWire(mappingID string, w *scheduleWire) (*routing.Schedule, error) {
	if w.offSet {
		if w.peakSet {
			return nil, fmt.Errorf("policy: mapping %q defines both schedule.peak and legacy schedule.off_peak; remove schedule.off_peak", mappingID)
		}
		return nil, fmt.Errorf("policy: mapping %q uses legacy schedule.off_peak; replace it with schedule.peak", mappingID)
	}
	windows := make([]routing.OffPeakWindow, 0, len(w.Peak))
	for _, pw := range w.Peak {
		days := make([]routing.DayOfWeek, len(pw.Days))
		for j, d := range pw.Days {
			days[j] = routing.DayOfWeek(d)
		}
		if pw.Start == "00:00" && pw.End == "24:00" {
			continue
		}
		// A peak window is converted to off-peak windows before and after it.
		// The complement is represented per day; cross-midnight peak windows are
		// rejected until the config can express their complement unambiguously.
		if pw.Start >= pw.End {
			return nil, fmt.Errorf("policy: mapping %q peak window %q-%q must not cross midnight", mappingID, pw.Start, pw.End)
		}
		if pw.Start != "00:00" {
			windows = append(windows, routing.OffPeakWindow{Days: days, Start: "00:00", End: pw.Start})
		}
		if pw.End != "24:00" {
			windows = append(windows, routing.OffPeakWindow{Days: days, Start: pw.End, End: "24:00"})
		}
	}
	s, err := routing.ParseSchedule(w.Timezone, windows)
	if err != nil {
		return nil, fmt.Errorf("policy: mapping %q schedule: %w", mappingID, err)
	}
	return &s, nil
}
