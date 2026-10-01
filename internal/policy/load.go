package policy

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
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
	BackupCount:        1,
}

// quota freshness is carried by DefaultQuotaFreshness (types.go).

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

	mode, err := modeFromWire(w.Mode)
	if err != nil {
		return Desired{}, err
	}
	if mode == ModeProviderOnly {
		return loadProviderOnly(w)
	}

	d := Desired{Version: w.Version, Mode: ModeLegacy, Providers: map[MappingID]Mapping{}}

	// Build provider mappings. modelOwner maps a base model to the single mapping
	// that owns it. Iterating sorted keys keeps error ordering deterministic.
	modelOwner := map[string]MappingID{}
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
			Models:     map[string]ModelBaseline{},
			KeepEnabled: mw.KeepEnabled,
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
		// Supported non-Anthropic mappings default into quota routing even when
		// the quota section is omitted. Anthropic remains visible but unpollable
		// without an explicit user budget, because no safe budget default exists.
		if quota.KnownAdapter(string(id)) {
			if id == "anthropic" && mw.Quota == nil {
				// Leave Anthropic unpollable until the user supplies a budget.
			} else if id == "anthropic" && mw.Quota != nil && !mw.Quota.hasAnyField() {
				// An explicit empty quota block has the same safe meaning.
			} else {
				qc, err := quotaFromWire(string(id), "", mw.Quota)
				if err != nil {
					return Desired{}, err
				}
				m.Quota = qc
			}
		} else if mw.Quota != nil {
			qc, err := quotaFromWire(string(id), "", mw.Quota)
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
		d.Providers[id] = m
	}

	op, err := operationalFromWire(w.Operational)
	if err != nil {
		return Desired{}, err
	}
	d.Operational = op

	d.Routing = routingFromWire(w.Routing)

	sel, err := selectionFromWire(w.Selection)
	if err != nil {
		return Desired{}, err
	}
	d.Selection = sel

	if d.Global, err = targetFromWire(w.Global, true, modelOwner); err != nil {
		return Desired{}, fmt.Errorf("policy: global target: %w", err)
	}
	d.Projects = make([]Target, 0, len(w.Projects))
	for i := range w.Projects {
		t, err := targetFromWire(&w.Projects[i].targetWire, false, modelOwner)
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
	bc := defaultOperational.BackupCount
	if w.BackupCount != nil {
		bc = *w.BackupCount
	}
	if bc <= 0 {
		return Operational{}, fmt.Errorf("policy: operational backup_count must be >= 1, got %d", bc)
	}
	if vt <= 0 || lw <= 0 || rr <= 0 {
		return Operational{}, errors.New("policy: operational durations must be positive")
	}
	actions, err := onChangeFromWire(w.OnChange)
	if err != nil {
		return Operational{}, err
	}
	return Operational{ValidationTimeout: vt, LockWait: lw, RecoveredRetention: rr, BackupCount: bc, NoticePath: w.NoticePath, OnChange: actions}, nil
}

// onChangeFromWire validates and resolves the optional on_change action list:
// absolute run paths only, bounded per-action timeouts, at most
// MaxOnChangeActions entries. An empty list resolves to nil (nothing executes).
func onChangeFromWire(entries []onChangeWire) ([]OnChangeAction, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxOnChangeActions {
		return nil, fmt.Errorf("policy: operational on_change allows at most %d actions, got %d", MaxOnChangeActions, len(entries))
	}
	out := make([]OnChangeAction, 0, len(entries))
	for i, e := range entries {
		if !filepath.IsAbs(e.Run) {
			return nil, fmt.Errorf("policy: operational on_change[%d] run must be an absolute path, got %q", i, e.Run)
		}
		timeout := DefaultOnChangeTimeoutSeconds
		if e.TimeoutSeconds != nil {
			timeout = *e.TimeoutSeconds
		}
		if timeout < 1 || timeout > MaxOnChangeTimeoutSeconds {
			return nil, fmt.Errorf("policy: operational on_change[%d] timeout_seconds must be 1..%d, got %d", i, MaxOnChangeTimeoutSeconds, timeout)
		}
		out = append(out, OnChangeAction{Run: e.Run, Args: e.Args, Env: e.Env, TimeoutSeconds: timeout})
	}
	return out, nil
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
	Mode        string                 `yaml:"mode"`
	Providers   map[string]mappingWire `yaml:"providers"`
	Global      *targetWire            `yaml:"global"`
	Projects    []projectWire          `yaml:"projects"`
	Operational *operationalWire       `yaml:"operational"`
	Routing     *routingWire           `yaml:"routing"`
	Selection   *selectionWire         `yaml:"selection"`
}

// projectWire is one entry of the top-level `projects` sequence. It records the
// observed top-level keys so provider-only load can reject unsupported project
// fields exactly; legacy load decodes the same wire shape and ignores unknown
// keys as before.
type projectWire struct {
	targetWire
	keys []string
}

func (p *projectWire) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			p.keys = append(p.keys, value.Content[i].Value)
		}
	}
	return value.Decode(&p.targetWire)
}

// adapterNames lists the known quota adapter names for rejection diagnostics.
func adapterNames() []string {
	names := make([]string, 0, 4)
	for _, def := range quota.AdapterDefinitions() {
		names = append(names, def.Name)
	}
	return names
}

// modeFromWire resolves the optional mode key. An omitted key and the explicit
// "legacy" spelling both load as ModeLegacy; "provider-only" is the strictly
// opt-in provider-only mode. Every other value is rejected so a typo can never
// silently select a mode the operator did not intend.
func modeFromWire(s string) (PolicyMode, error) {
	switch PolicyMode(s) {
	case "", ModeLegacy:
		return ModeLegacy, nil
	case ModeProviderOnly:
		return ModeProviderOnly, nil
	default:
		return "", fmt.Errorf("policy: unknown mode %q (want legacy or provider-only)", s)
	}
}

// loadProviderOnly parses the strictly opt-in provider-only policy: enrolled
// Polytoken provider IDs with optional quota adapter configuration, a global
// target, and registered project roots for read-only global+project safety
// assessment. Legacy target/model fields — model enumeration, chain definitions,
// and the routing/selection sections — conflict with the mode and are rejected
// without mutating anything, so a mixed file can never half-convert an
// installation. Project entries carry exactly id and root: any other field
// (legacy chains, definitions, or unsupported keys) is rejected.
func loadProviderOnly(w docWire) (Desired, error) {
	if len(w.Providers) == 0 {
		return Desired{}, errors.New("policy: provider-only policy must enroll at least one provider")
	}
	d := Desired{
		Version:     w.Version,
		Mode:        ModeProviderOnly,
		Providers:   map[MappingID]Mapping{},
		Operational: defaultOperational,
	}
	ids := make([]string, 0, len(w.Providers))
	for id := range w.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, idStr := range ids {
		mw := w.Providers[idStr]
		if len(mw.Models) > 0 {
			return Desired{}, fmt.Errorf("policy: provider-only mapping %q must not enumerate models (remove the legacy models field)", idStr)
		}
		m := Mapping{KeepEnabled: mw.KeepEnabled}
		if mw.Quota != nil {
			if !mw.Quota.adapterSet || strings.TrimSpace(mw.Quota.Adapter) == "" {
				names := adapterNames()
				return Desired{}, fmt.Errorf("policy: provider %q: provider-only quota requires an explicit adapter (one of: %s)", idStr, strings.Join(names, ", "))
			}
			qc, err := quotaFromWire(idStr, mw.Quota.Adapter, mw.Quota)
			if err != nil {
				return Desired{}, err
			}
			m.Quota = qc
		}
		d.Providers[MappingID(idStr)] = m
	}
	if w.Routing != nil {
		return Desired{}, errors.New("policy: provider-only policy must not set routing (chain reordering is a legacy behavior)")
	}
	if w.Selection != nil {
		return Desired{}, errors.New("policy: provider-only policy must not set selection (model selection is a legacy behavior)")
	}
	projects, err := providerOnlyProjectRoots(w.Projects)
	if err != nil {
		return Desired{}, err
	}
	d.Projects = projects
	if w.Global == nil {
		return Desired{}, errors.New("policy: provider-only policy requires a global target with a root")
	}
	if err := providerOnlyTargetFromWire(w.Global); err != nil {
		return Desired{}, fmt.Errorf("policy: global target: %w", err)
	}
	if w.Global.Root == "" {
		return Desired{}, errors.New("policy: provider-only policy requires a global target root")
	}
	d.Global = Target{ID: w.Global.ID, Root: w.Global.Root, Global: true}

	op, err := operationalFromWire(w.Operational)
	if err != nil {
		return Desired{}, err
	}
	d.Operational = op
	// Resolve the selection defaults exactly like legacy Load does for an
	// omitted section. The selection section itself is rejected above; the
	// resolved default matters only so the in-memory policy is fully resolved
	// and serialization matches an omitted section.
	d.Selection = defaultSelection()
	return d, nil
}

// providerOnlyTargetFromWire rejects the legacy chain-bearing target fields in
// a provider-only global target. Full/mini/nano defaults, the classifier, and
// definition chains are exactly the fields quota authored in legacy mode, so
// their presence marks a mixed legacy/provider-only document.
func providerOnlyTargetFromWire(w *targetWire) error {
	for _, c := range []struct {
		name  string
		chain Chain
	}{{"full", w.Full}, {"mini", w.Mini}, {"nano", w.Nano}, {"classifier", w.Classifier}} {
		if len(c.chain) > 0 {
			return fmt.Errorf("chain %q is a legacy field and conflicts with provider-only mode (remove it)", c.name)
		}
	}
	if len(w.Definitions) > 0 {
		return errors.New("definitions are legacy fields and conflict with provider-only mode (remove them)")
	}
	return nil
}

// providerOnlyProjectRoots converts strictly-shaped project entries into
// id/root-only targets. Provider-only projects register reconciliation roots so
// read-only safety assessment can cover every registered target; they carry no
// legacy chains, definitions, or any other field. Non-empty unique ids and
// roots are required, and the document order is preserved.
func providerOnlyProjectRoots(wire []projectWire) ([]Target, error) {
	projects := make([]Target, 0, len(wire))
	seen := make(map[string]bool, len(wire))
	for i, pw := range wire {
		for _, key := range pw.keys {
			if key != "id" && key != "root" {
				return nil, fmt.Errorf("policy: project %d: field %q is not supported in provider-only mode (projects register id and root only)", i, key)
			}
		}
		id := strings.TrimSpace(pw.ID)
		if id == "" {
			return nil, fmt.Errorf("policy: project %d: provider-only projects require a non-empty id", i)
		}
		if strings.TrimSpace(pw.Root) == "" {
			return nil, fmt.Errorf("policy: project %q: provider-only projects require a non-empty root", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("policy: project %q is registered more than once", id)
		}
		seen[id] = true
		projects = append(projects, Target{ID: id, Root: pw.Root})
	}
	return projects, nil
}

type mappingWire struct {
	Models      []modelWire `yaml:"models"`
	Quota       *quotaWire  `yaml:"quota"`
	KeepEnabled bool        `yaml:"keep_enabled"`
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
	// BackupCount is a pointer so an omitted key (nil) can default to 1 while
	// an explicit zero or negative value is still rejected. One pre-apply
	// backup per managed file is the minimum viable rollback point; the
	// runtime publisher prunes each file's backups oldest-first to this count
	// as the file changes.
	BackupCount *int `yaml:"backup_count"`
	// NoticePath is optional; empty means the default notice location.
	NoticePath string `yaml:"notice_path"`
	// OnChange is the optional list of post-commit host actions.
	OnChange []onChangeWire `yaml:"on_change"`
}

type onChangeWire struct {
	Run            string            `yaml:"run"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	TimeoutSeconds *int              `yaml:"timeout_seconds"`
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

// quotaWire is the on-disk shape of a mapping's `quota` section. In legacy mode
// the mapping key itself selects the quota adapter, so there is no adapter
// field; a quota block under a key that is not a known adapter name rejects
// policy load, and an explicit quota.adapter key is rejected as conflicting.
// In provider-only mode the provider key is a Polytoken provider ID, so the
// quota section must name its adapter explicitly via quota.adapter.
type quotaWire struct {
	Adapter          string        `yaml:"adapter"`
	FreshnessTTL     string        `yaml:"freshness_ttl"`
	BalanceGroup     string        `yaml:"balance_group"`
	Weight           int           `yaml:"weight"`
	MonthlyBudgetUSD float64       `yaml:"monthly_budget_usd"`
	Mode             string        `yaml:"mode"`
	Schedule         *scheduleWire `yaml:"schedule"`
	// SignalGate is the strict sub-decode of the optional provider-only
	// signal_gate block; nil when the key is absent.
	SignalGate       *signalGateWire `yaml:"signal_gate"`
	hasFields        bool
	monthlyBudgetSet bool
	adapterSet       bool
}

func (q *quotaWire) UnmarshalYAML(value *yaml.Node) error {
	type plain quotaWire
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*q = quotaWire(decoded)
	q.hasFields = len(value.Content) > 0
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "monthly_budget_usd":
			q.monthlyBudgetSet = true
		case "adapter":
			q.adapterSet = true
		}
	}
	return nil
}

func (q *quotaWire) hasAnyField() bool {
	return q != nil && q.hasFields
}

// signalGateWire is the on-disk shape of a mapping's `quota.signal_gate`
// section. Unlike the surrounding lenient quota block — plain decoding so
// existing files never tighten — signal_gate is decoded strictly: unknown
// keys, duplicate keys, non-mapping shapes, and explicit null values are all
// rejected, mirroring the selection section's grammar. Decoding errors are
// fixed strings that name the allowed grammar but never echo document-derived
// values back into diagnostics.
type signalGateWire struct {
	Enabled   *bool
	Threshold float64
	// enabledSet/thresholdSet distinguish an explicit key (including an
	// explicit empty value, which must be rejected) from an omitted one,
	// which defaults.
	enabledSet   bool
	thresholdSet bool
}

func (p *signalGateWire) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!null" {
		return nil // `signal_gate:` with no value: every key defaults
	}
	if value.Kind != yaml.MappingNode {
		return errors.New("policy: signal_gate must be a mapping")
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "enabled":
			if p.enabledSet {
				return errors.New("policy: signal_gate: duplicate enabled key")
			}
			p.enabledSet = true
			if value.Content[i+1].ShortTag() == "!!null" {
				// An explicit `enabled: null` (also ~ and aliases to null) is
				// not a boolean: yaml would silently decode it as the zero
				// value. Reject it with the same fixed, non-echoing error as
				// any other non-boolean value.
				return errors.New("policy: signal_gate: enabled must be a boolean")
			}
			var b bool
			if err := value.Content[i+1].Decode(&b); err != nil {
				return errors.New("policy: signal_gate: enabled must be a boolean")
			}
			p.Enabled = &b
		case "threshold":
			if p.thresholdSet {
				return errors.New("policy: signal_gate: duplicate threshold key")
			}
			p.thresholdSet = true
			if value.Content[i+1].ShortTag() == "!!null" {
				return errors.New("policy: signal_gate: threshold must be a non-positive number")
			}
			if err := value.Content[i+1].Decode(&p.Threshold); err != nil {
				return errors.New("policy: signal_gate: threshold must be a non-positive number")
			}
		default:
			return errors.New("policy: signal_gate: unknown key (want enabled or threshold)")
		}
	}
	return nil
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
// section yields routing enabled: quota-based routing is the tool's primary
// purpose, so desired.yaml files without a routing section route by default.
// An explicit `routing: {enabled: false}` opts out.
func routingFromWire(w *routingWire) RoutingConfig {
	if w == nil {
		return RoutingConfig{Enabled: true}
	}
	return RoutingConfig{Enabled: w.Enabled}
}

// selectionWire is the on-disk shape of the optional top-level `selection`
// section. Unlike the legacy sections — which plain yaml decoding leaves
// lenient so existing files never tighten — the selection section is decoded
// strictly: unknown keys, duplicate keys, and non-mapping shapes are all
// rejected. Decoding errors are fixed strings that name the allowed grammar
// but never echo document-derived values back into diagnostics.
type selectionWire struct {
	Jev *jevWire `yaml:"jev"`
}

func (s *selectionWire) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!null" {
		return nil // `selection:` with no value: every key defaults
	}
	if value.Kind != yaml.MappingNode {
		return errors.New("policy: selection must be a mapping")
	}
	seenJev := false
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "jev":
			if seenJev {
				return errors.New("policy: selection: duplicate jev key")
			}
			seenJev = true
			var jw jevWire
			if err := value.Content[i+1].Decode(&jw); err != nil {
				return fmt.Errorf("policy: selection: %w", err)
			}
			s.Jev = &jw
		default:
			return errors.New("policy: selection: unknown key (want jev)")
		}
	}
	return nil
}

// jevWire is the on-disk shape of selection.jev. `enabled`, `model`, and
// `timeout` are the only keys: model pins the versioned assessment model
// (validated as jev-X.Y.Z; DocumentedJevModel when the key is omitted).
// Duplicate keys are rejected instead of silently last-wins, and explicit
// empty values are rejected rather than interpreted as omitted.
type jevWire struct {
	Enabled *bool
	Model   string
	Timeout string
	// modelSet/timeoutSet distinguish an explicit key (including an explicit
	// empty value, which must be rejected) from an omitted one, which defaults.
	modelSet   bool
	timeoutSet bool
}

func (j *jevWire) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!null" {
		return nil // `jev:` with no value: every key defaults
	}
	if value.Kind != yaml.MappingNode {
		return errors.New("policy: selection jev must be a mapping")
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "enabled":
			if j.Enabled != nil {
				return errors.New("policy: selection jev: duplicate enabled key")
			}
			if value.Content[i+1].ShortTag() == "!!null" {
				// An explicit `enabled: null` (also ~ and aliases to null) is
				// not a boolean: yaml would silently decode it as the zero
				// value. Reject it with the same fixed, non-echoing error as
				// any other non-boolean value.
				return errors.New("policy: selection jev: enabled must be a boolean")
			}
			var b bool
			if err := value.Content[i+1].Decode(&b); err != nil {
				return errors.New("policy: selection jev: enabled must be a boolean")
			}
			j.Enabled = &b
		case "model":
			if j.modelSet {
				return errors.New("policy: selection jev: duplicate model key")
			}
			j.modelSet = true
			if err := value.Content[i+1].Decode(&j.Model); err != nil {
				return errors.New("policy: selection jev: model must be a versioned pin like jev-1.13.0")
			}
		case "timeout":
			if j.timeoutSet {
				return errors.New("policy: selection jev: duplicate timeout key")
			}
			j.timeoutSet = true
			if err := value.Content[i+1].Decode(&j.Timeout); err != nil {
				return errors.New("policy: selection jev: timeout must be a positive duration (e.g. 10s)")
			}
		default:
			return errors.New("policy: selection jev: unknown key (want enabled, model, or timeout)")
		}
	}
	return nil
}

// ValidJevPin reports whether model has the documented versioned pin shape:
// exactly `jev-X.Y.Z` with three non-empty numeric components. Like the quota
// adapter names, the value is validated against a fixed grammar rather than
// accepted as any non-empty string, so pins stay comparable across releases.
// This is the single jev pin grammar for the whole tree: policy load uses it
// for the desired configuration, and the selection package's ValidModelPin
// delegates to it, so the two layers cannot drift.
func ValidJevPin(model string) bool {
	rest, ok := strings.CutPrefix(model, "jev-")
	if !ok {
		return false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// defaultSelection is applied when desired.yaml omits the selection section
// (or any key within it): JEV assessment disabled, pinned to
// DocumentedJevModel, bounded by DefaultJevTimeout.
func defaultSelection() SelectionConfig {
	return SelectionConfig{Jev: JevSelectionConfig{Enabled: false, Model: DocumentedJevModel, Timeout: DefaultJevTimeout}}
}

// selectionFromWire translates the optional selection section into its
// resolved form. An omitted section or key yields the documented default
// (JEV disabled, DocumentedJevModel pin, DefaultJevTimeout). Explicit keys are
// validated, never partially interpreted: the model pin must match jev-X.Y.Z,
// the timeout must parse and be positive, and an explicit empty model or
// timeout is an error rather than a silent default. Validation errors are
// fixed strings that do not echo the offending config values.
func selectionFromWire(w *selectionWire) (SelectionConfig, error) {
	sel := defaultSelection()
	if w == nil || w.Jev == nil {
		return sel, nil
	}
	if w.Jev.Enabled != nil {
		sel.Jev.Enabled = *w.Jev.Enabled
	}
	if w.Jev.modelSet {
		if !ValidJevPin(w.Jev.Model) {
			return SelectionConfig{}, errors.New("policy: selection jev model must be a versioned pin like jev-1.13.0")
		}
		sel.Jev.Model = w.Jev.Model
	}
	if w.Jev.timeoutSet {
		d, err := time.ParseDuration(w.Jev.Timeout)
		if err != nil || d <= 0 {
			return SelectionConfig{}, errors.New("policy: selection jev timeout must be a positive duration (e.g. 10s)")
		}
		sel.Jev.Timeout = d
	}
	return sel, nil
}

// quotaFromWire translates a mapping's quota section into a QuotaConfig. In
// legacy mode explicitAdapter is empty and the mapping key is the adapter name,
// validated here so an unknown key rejects policy load; an explicit
// quota.adapter key conflicts with the key-selected adapter and is rejected.
// In provider-only mode the provider key is a Polytoken provider ID, so
// explicitAdapter is the required quota.adapter value and must be a known
// adapter. The schedule, when present, is validated via routing.ParseSchedule
// so an invalid timezone/day/time rejects policy loading. FreshnessTTL
// defaults to 30m when omitted (matching the routing package's default), like
// the operational durations.
func quotaFromWire(mappingID, explicitAdapter string, w *quotaWire) (*QuotaConfig, error) {
	adapter := explicitAdapter
	if explicitAdapter == "" {
		// Legacy tolerance: a leftover quota.adapter key is ignored — it can
		// neither select nor override the adapter derived from the mapping key
		// (existing files never tighten). In provider-only mode the adapter is
		// required and validated below.
		adapter = mappingID
		if !quota.KnownAdapter(adapter) {
			return nil, fmt.Errorf("policy: mapping %q: the provider key selects the quota adapter and must be one of: %s", mappingID, strings.Join(adapterNames(), ", "))
		}
	} else if !quota.KnownAdapter(explicitAdapter) {
		return nil, fmt.Errorf("policy: provider %q: unknown quota adapter %q (want one of: %s)", mappingID, explicitAdapter, strings.Join(adapterNames(), ", "))
	}
	if w == nil {
		w = &quotaWire{}
	}
	// signal_gate is a provider-only gating concept. Legacy mode keeps
	// rank-demotion-only semantics (approved plan non-goal), and a legacy
	// file cannot carry the key today (it was never a recognized quota field),
	// so accepting it there could only ever mean silently ignoring operator
	// intent: reject it instead.
	if explicitAdapter == "" && w.SignalGate != nil {
		return nil, fmt.Errorf("policy: mapping %q: signal_gate is only valid in provider-only mode", mappingID)
	}
	// quota.mode selects the Anthropic source: "api" (default, the Admin
	// cost-report adapter) or "subscription" (the experimental OAuth usage
	// adapter). It is only meaningful for the anthropic adapters.
	mode := strings.TrimSpace(w.Mode)
	if adapter != "anthropic" && adapter != "anthropic-subscription" && mode != "" {
		return nil, fmt.Errorf("policy: mapping %q: quota mode is only valid for the anthropic provider", mappingID)
	}
	if adapter == "anthropic-subscription" && mode != "" {
		return nil, fmt.Errorf("policy: mapping %q: adapter anthropic-subscription is always subscription; remove quota.mode", mappingID)
	}
	if mode == "" {
		mode = "api"
	}
	if adapter == "anthropic-subscription" {
		mode = "subscription"
	}
	if mode != "api" && mode != "subscription" {
		return nil, fmt.Errorf("policy: mapping %q: unknown quota mode %q (want api or subscription)", mappingID, mode)
	}
	if mode == "subscription" {
		if w.monthlyBudgetSet {
			return nil, fmt.Errorf("policy: mapping %q: quota mode subscription does not use monthly_budget_usd (the subscription's own session/weekly caps are the quota); remove it", mappingID)
		}
	} else if adapter == "anthropic" && w.hasAnyField() && !w.monthlyBudgetSet {
		return nil, fmt.Errorf("policy: mapping %q: the anthropic adapter requires monthly_budget_usd (the spend ceiling to treat as this provider's quota)", mappingID)
	}
	resolved := adapter
	if mode == "subscription" {
		resolved = "anthropic-subscription"
	}
	qc := &QuotaConfig{
		Adapter:          resolved,
		FreshnessTTL:     DefaultQuotaFreshness,
		BalanceGroup:     w.BalanceGroup,
		Weight:           w.Weight,
		MonthlyBudgetUSD: w.MonthlyBudgetUSD,
	}
	if math.IsNaN(w.MonthlyBudgetUSD) || math.IsInf(w.MonthlyBudgetUSD, 0) || w.MonthlyBudgetUSD < 0 || (adapter == "anthropic" && mode == "api" && w.MonthlyBudgetUSD == 0) {
		return nil, fmt.Errorf("policy: mapping %q: monthly_budget_usd must be finite and positive", mappingID)
	}
	// The anthropic api-mode adapter measures month-to-date spend against a
	// user-defined budget; without one there is nothing to measure against.
	if adapter == "anthropic" && mode == "api" && !w.monthlyBudgetSet {
		return nil, fmt.Errorf("policy: mapping %q: the anthropic adapter requires monthly_budget_usd (the spend ceiling to treat as this provider's quota)", mappingID)
	}
	ttl, err := parseDur("freshness_ttl", w.FreshnessTTL, DefaultQuotaFreshness)
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
	// Resolve signal gating so the in-memory config matches its struct docs:
	// an absent block (or any omitted key within it) means gating on at the
	// documented DefaultSignalGateThreshold (the projected-exhaustion line).
	qc.SignalGate = DefaultSignalGate()
	if w.SignalGate != nil {
		if w.SignalGate.Enabled != nil && !*w.SignalGate.Enabled {
			qc.SignalGate.Disabled = true
		}
		if w.SignalGate.thresholdSet {
			if math.IsNaN(w.SignalGate.Threshold) || math.IsInf(w.SignalGate.Threshold, 0) || w.SignalGate.Threshold > 0 {
				return nil, fmt.Errorf("policy: mapping %q: signal_gate threshold must be finite and non-positive", mappingID)
			}
			qc.SignalGate.Threshold = w.SignalGate.Threshold
		}
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
