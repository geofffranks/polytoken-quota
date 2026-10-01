// Package groupsafety is the read-only version-4 provider-disable safety
// analyzer. Given the explicit, registered configuration layers of a Polytoken
// root (the global layer plus the registered project layers) and one enrolled
// global provider ID, it resolves the effective model-group graph the way the
// supported binary has been proven to compose it — same-name groups
// concatenate with global leaves first and project leaves appended, repeated
// leaves and order preserved — and classifies a proposed
// `providers.<id>.enabled: false` edit as safe, unsafe, or pending-unknown.
//
// The analyzer reads only the config bytes and definition references handed to
// it (ReadLayer reads exactly config.yaml plus the policy allowlisted
// facets/subagents trees of one explicit root). It never scans arbitrary
// workspace roots, never contacts a daemon, never writes anything, and a
// verdict alone never authorizes a write: safe means every provable consumer
// (tier defaults, derived reserved groups, configured groups, facet/subagent
// references in the registered roots) keeps a usable post-disable route;
// unsafe means a breakage is provable from the graph — a group left without
// any available leaf, a required tier default without a model (the observed
// binary rejects such reloads wholesale and retains the stale active
// selection), or a facet/subagent primary reference that would die;
// pending-unknown means the composition's semantics were never pinned against
// the binary (dynamic or unresolved leaves, unknown shipped/reserved names,
// nested-reference cycles, ambiguous layering, schema/version violations) and
// the disable must not be authorized.
//
// Staged `config validate` and `doctor` remain backstops, not a safety proof:
// the disabled-fallback fixture in contract/testdata shows a config that
// passes `config validate` while a definition reference is already dead.
package groupsafety

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// Verdict is the classification of one proposed provider disable.
type Verdict string

const (
	// Safe means every provable consumer keeps a usable post-disable route.
	// It is a necessary condition for authorizing the write, never by itself
	// sufficient: staged binary validation remains a backstop and session
	// continuity is proven separately by the contract suite.
	Safe Verdict = "safe"
	// Unsafe means a breakage is provable from the resolved graph: a group
	// would be left without any available leaf, a required tier default would
	// lose its last model, or a facet/subagent primary reference in a
	// registered root would die. The observed binary rejects such reloads
	// wholesale and keeps the stale active selection, so the write must not be
	// made.
	Unsafe Verdict = "unsafe"
	// PendingUnknown means the composition cannot be proven either way against
	// the supported binary: unknown dynamic/unresolved leaves, unknown shipped
	// or reserved names, nested-reference cycles, ambiguous layering, schema
	// or version violations, or a registered root that is already invalid. The
	// write must not be made.
	PendingUnknown Verdict = "pending-unknown"
)

// Definition is one facet/subagent model reference read from a registered
// root: the primary model reference plus its ordered fallback references.
// Path is the root-relative, slash-separated file path used in report reasons.
type Definition struct {
	Path      string
	Model     string
	Fallbacks []string
}

// Layer is one explicit configuration layer: its raw config.yaml bytes plus
// the managed definition references read under the same registered root.
// Global marks the single global layer; project layers contribute their
// same-name group leaves after the global layer's.
type Layer struct {
	ID          string
	Global      bool
	Config      []byte
	Definitions []Definition
}

// Input is the fully explicit analyzer input. Registered roots enter only as
// the bytes and definitions the caller (or ReadLayer) gathered; the analyzer
// performs no filesystem access of its own.
type Input struct {
	// Enrolled is the explicit set of Polytoken provider IDs the provider-only
	// policy enrolls. A proposed disable outside this set is refused.
	Enrolled []string
	// Global is the registered global layer. Required.
	Global Layer
	// Projects are the registered project layers in registration order.
	// Optional; project layers may carry modelgroups only.
	Projects []Layer
}

// Report is the analyzer's classification of one proposed disable.
type Report struct {
	Verdict Verdict
	// Reasons carries sanitized, deterministic evidence for the verdict:
	// group/model/provider names and reference paths only — never unrelated
	// config values such as URLs or auth material.
	Reasons []string
	// GroupsBefore maps each resolved group name to its effective available
	// leaves under the current provider state, duplicates and order preserved
	// exactly as the binary's published catalog composes them.
	GroupsBefore map[string][]string
	// GroupsAfter is GroupsBefore with the proposed disable applied: every
	// leaf owned by the disabled provider is filtered out, in place.
	GroupsAfter map[string][]string
}

// Observed reserved group names. The three polytoken:default_model_* names
// are the configured tier defaults. polytoken:general-purpose and
// polytoken:general-purpose-mini are the derived reserved groups observed to
// publish the full/mini default's effective leaves. The nano tier's derived
// reserved name has never been observed, so a reference to one is
// pending-unknown, as is any other name in the polytoken: namespace.
const (
	tierDefaultFull = "polytoken:default_model_full"
	tierDefaultMini = "polytoken:default_model_mini"
	tierDefaultNano = "polytoken:default_model_nano"

	reservedGroupFull = "polytoken:general-purpose"
	reservedGroupMini = "polytoken:general-purpose-mini"
)

// Analyze classifies disabling disableProvider independently for the global
// root and for each registered project's effective global-plus-project root.
// A verdict is safe only when every root is safe; an unsafe result in any root
// wins over pending-unknown, which wins over safe. It never touches the
// filesystem and fails closed.
func Analyze(in Input, disableProvider string) Report {
	projects := in.Projects
	if len(projects) == 0 {
		projects = []Layer{{ID: "(global)"}}
	}
	combined := Report{Verdict: Safe, Reasons: []string{}, GroupsBefore: map[string][]string{}, GroupsAfter: map[string][]string{}}
	reasonSet := map[string]bool{}
	for _, project := range projects {
		rootInput := in
		rootInput.Projects = nil
		if project.ID != "(global)" {
			rootInput.Projects = []Layer{project}
		}
		root := analyzeRoot(rootInput, disableProvider)
		if root.Verdict == Unsafe || (root.Verdict == PendingUnknown && combined.Verdict == Safe) {
			combined.Verdict = root.Verdict
		}
		for _, reason := range root.Reasons {
			reasonSet[reason] = true
		}
		for name, leaves := range root.GroupsBefore {
			combined.GroupsBefore[name] = append([]string(nil), leaves...)
		}
		for name, leaves := range root.GroupsAfter {
			combined.GroupsAfter[name] = append([]string(nil), leaves...)
		}
	}
	for reason := range reasonSet {
		combined.Reasons = append(combined.Reasons, reason)
	}
	sort.Strings(combined.Reasons)
	return combined
}

func analyzeRoot(in Input, disableProvider string) Report {
	r := &resolver{
		reasons:      map[string]bool{},
		before:       map[string][]string{},
		after:        map[string][]string{},
		disable:      disableProvider,
		enrolled:     map[string]bool{},
		groupSources: map[string][]parsedGroup{},
		definitions:  []Definition{},
		unknownRefs:  map[string]bool{},
		memo:         map[groupResolutionKey]groupResolution{},
		budget:       maxGroupExpansions,
	}
	for _, id := range in.Enrolled {
		r.enrolled[id] = true
	}

	switch {
	case disableProvider == "":
		r.pending("proposed disable names no provider")
	case !r.enrolled[disableProvider]:
		r.pending(fmt.Sprintf("provider %q is not enrolled in the provider-only policy", disableProvider))
	}
	if len(in.Global.Config) == 0 {
		r.pending("no global layer supplied")
	}

	// Parse every layer strictly. Global-plus-one-project same-name
	// concatenation is the proven composition; anything else in or across
	// project layers (providers/models/defaults sections, a second project
	// layer defining the same group, reserved-namespace groups outside the
	// global layer) has no pinned cross-layer semantics and fails closed.
	var global *parsedLayer
	layers := append([]Layer{in.Global}, in.Projects...)
	for i, l := range layers {
		p, perr := parseLayer(l)
		if perr != "" {
			r.pending(perr)
			continue
		}
		if l.Global {
			if global != nil {
				r.pending("more than one global layer supplied")
				continue
			}
			global = p
			for _, name := range p.groupOrder {
				// Index 0 keeps global leaves first regardless of project
				// registration order.
				r.groupSources[name] = append([]parsedGroup{{layerIndex: 0, leaves: p.groups[name]}}, r.groupSources[name]...)
			}
			if p.hasDefaults {
				r.pending("global layer sets defaults; the legacy defaults key has no observed version-4 semantics")
			}
			if len(p.models) == 0 && len(p.groupOrder) > 0 {
				r.pending("global layer defines groups but no models; leaf resolution is unobservable")
			}
			for _, name := range p.groupOrder {
				if isReserved(name) && !isTierDefault(name) {
					r.pending(fmt.Sprintf("global group %q uses the reserved polytoken: namespace with unobserved semantics", name))
				}
			}
		} else {
			if p.hasProviders {
				r.pending(fmt.Sprintf("project layer %q sets providers; project-layer provider composition is unobserved", l.ID))
			}
			if p.hasModels {
				r.pending(fmt.Sprintf("project layer %q sets models; project-layer model composition is unobserved", l.ID))
			}
			if p.hasDefaults {
				r.pending(fmt.Sprintf("project layer %q sets defaults; project-layer default composition is unobserved", l.ID))
			}
			for _, name := range p.groupOrder {
				if isReserved(name) {
					r.pending(fmt.Sprintf("project layer %q defines reserved-namespace group %q; its layering is unobserved", l.ID, name))
				}
				r.groupSources[name] = append(r.groupSources[name], parsedGroup{layerIndex: i, leaves: p.groups[name]})
			}
		}
		r.definitions = append(r.definitions, l.Definitions...)
	}

	// Ambiguity: only global-plus-one-project same-name concatenation is
	// proven; two or more project layers defining one group has no pinned
	// order.
	for name, srcs := range r.groupSources {
		count := 0
		for _, s := range srcs {
			if s.layerIndex != 0 {
				count++
			}
		}
		if count > 1 {
			r.pending(fmt.Sprintf("group %q is defined by %d project layers; their concatenation order is unobserved", name, count))
		}
	}

	// Fail closed on the groupless shape: with zero configured groups there is
	// nothing to resolve, so the only remaining routes are the binary's
	// shipped defaults and the dynamic catalog, whose composition this
	// offline analyzer has never observed. Classifying that shape Safe would
	// authorize a disable on unproven evidence (rev3: pin shipped composition
	// in a binary fixture or classify the affected routes pending-unknown —
	// never generalize).
	if global != nil && len(r.groupSources) == 0 && (global.hasProviders || global.hasModels) {
		r.pending("no modelgroups are configured in any registered layer; shipped default-route composition is unproven")
	}

	r.resolveGroups(global)
	r.checkDefinitions(global)

	report := Report{Verdict: Safe, Reasons: []string{}, GroupsBefore: r.before, GroupsAfter: r.after}
	for reason := range r.reasons {
		report.Reasons = append(report.Reasons, reason)
	}
	sort.Strings(report.Reasons)
	for _, reason := range report.Reasons {
		switch {
		case strings.HasPrefix(reason, unsafePrefix):
			report.Verdict = Unsafe
		case strings.HasPrefix(reason, pendingPrefix):
			if report.Verdict != Unsafe {
				report.Verdict = PendingUnknown
			}
		}
	}
	// Evidence prefixes keep the verdict auditable in code; strip them so
	// callers render plain evidence.
	for i, reason := range report.Reasons {
		if _, rest, ok := strings.Cut(reason, ": "); ok {
			report.Reasons[i] = rest
		}
	}
	return report
}

const (
	unsafePrefix  = "unsafe"
	pendingPrefix = "pending"
)

type parsedGroup struct {
	layerIndex int
	leaves     []leaf
}

type leaf struct {
	isRef bool
	value string
}

type modelDef struct {
	provider string
	enabled  bool
}

type parsedLayer struct {
	providers    map[string]bool
	hasProviders bool
	models       map[string]modelDef
	hasModels    bool
	hasDefaults  bool
	groupOrder   []string
	groups       map[string][]leaf
}

type resolver struct {
	reasons      map[string]bool
	before       map[string][]string
	after        map[string][]string
	disable      string
	enrolled     map[string]bool
	definitions  []Definition
	groupSources map[string][]parsedGroup
	unknownRefs  map[string]bool
	// memo caches one group source's resolved leaves per (group, layer,
	// disabled) so shared references resolve once instead of exponentially:
	// a chain of groups each referencing the next twice would otherwise
	// expand 2^n leaf walks under the coordinator's mutation lock.
	memo map[groupResolutionKey]groupResolution
	// budget bounds the total resolveLeaves work per root analysis — both
	// expansions and appended leaves, so a hostile graph is bounded in time
	// AND output size (memoized sub-lists re-appended many times would
	// otherwise build a 2^n-element result even with cached expansions). A
	// hostile or merely oversized graph exhausts it and fails closed
	// pending-unknown instead of hanging the reconcile.
	budget int
}

// groupResolutionKey identifies one memoized leaf resolution: the group name,
// the layer supplying its leaves, and the provider state it was resolved
// under.
type groupResolutionKey struct {
	group      string
	layerIndex int
	disabled   bool
}

// groupResolution is one memoized resolution: the resolved leaf list (duplicates
// and order preserved) and whether it was tainted by unresolved references.
type groupResolution struct {
	leaves  []string
	tainted bool
}

// maxGroupExpansions bounds resolveLeaves work per analyzed root — expansions
// plus appended leaves. A memoized sane config stays orders of magnitude
// below it; the bound exists so a hostile reference graph fails closed
// quickly rather than spinning under the mutation lock.
const maxGroupExpansions = 50000

// chargeLeaves debits n units of analysis budget (one expansion or n appended
// leaves). It reports whether budget remains; on exhaustion it records the
// pending reason once and the caller fails its subtree closed.
func (r *resolver) chargeLeaves(n int) bool {
	r.budget -= n
	if r.budget > 0 {
		return true
	}
	r.pending("modelgroups resolution exceeded the analysis budget; the composition is left unproven")
	return false
}

func (r *resolver) pending(s string) { r.reasons[pendingPrefix+": "+s] = true }
func (r *resolver) unsafe(s string)  { r.reasons[unsafePrefix+": "+s] = true }

// isReserved reports whether name lives in the polytoken: namespace whose
// composition semantics come from the binary rather than the operator's
// config.
func isReserved(name string) bool { return strings.HasPrefix(name, "polytoken:") }

func isTierDefault(name string) bool {
	return name == tierDefaultFull || name == tierDefaultMini || name == tierDefaultNano
}

// available reports whether model is usable under the current provider state
// (disabled=false) or with the proposed disable applied (disabled=true).
// known reports whether the model exists in the parsed global catalog; an
// unknown model is statically skipped by the binary, but for the analyzer it
// is pending-unknown evidence because a dynamically discovered model cannot
// be distinguished from a typo offline.
func (r *resolver) available(global *parsedLayer, name string, disabled bool) (usable, known bool) {
	if global == nil {
		return false, false
	}
	def, ok := global.models[name]
	if !ok {
		return false, false
	}
	if !def.enabled {
		return false, true
	}
	enabledNow, hasProvider := global.providers[def.provider]
	if !hasProvider {
		r.pending(fmt.Sprintf("model %q attributes to provider %q which has no providers entry", name, def.provider))
		return false, true
	}
	if disabled && def.provider == r.disable {
		return false, true
	}
	return enabledNow, true
}

// modelProviderFromName infers a model's owning provider from its catalog
// name, which the observed Polytoken config shapes as
// <provider>/<model>(<effort>): everything before the first slash names the
// provider. It fails closed when the name has no slash, an empty provider, or
// an empty model part. The effort suffix follows the model name and never
// changes the split.
func modelProviderFromName(name string) (string, bool) {
	provider, model, ok := strings.Cut(name, "/")
	if !ok || provider == "" || model == "" {
		return "", false
	}
	return provider, true
}

// reservedAliasTarget maps the observed derived reserved group names to their
// tier default. Unknown reserved names stay unaliased and surface as unknown
// references.
func reservedAliasTarget(name string) (string, bool) {
	switch name {
	case reservedGroupFull:
		return tierDefaultFull, true
	case reservedGroupMini:
		return tierDefaultMini, true
	}
	return "", false
}

// resolveLeaves walks one layer's leaves of a group, resolving mg: references
// recursively with cycle detection. Duplicates and order are preserved;
// unavailable leaves are filtered in place, exactly as the binary's published
// catalog composes them. The second return value reports taint: the group's
// leaves did not fully resolve (a reference cycle or an unknown model), so
// emptiness conclusions about the group are unprovable and must stay
// pending-unknown instead of unsafe.
func (r *resolver) resolveLeaves(global *parsedLayer, group string, src parsedGroup, disabled bool, stack []string) ([]string, bool) {
	for _, g := range stack {
		if g == group {
			r.pending(fmt.Sprintf("group %q takes part in a modelgroups reference cycle", group))
			return nil, true
		}
	}
	key := groupResolutionKey{group: group, layerIndex: src.layerIndex, disabled: disabled}
	if hit, ok := r.memo[key]; ok {
		// Re-appending a cached sub-list still costs output budget: many
		// references to one healthy group would otherwise build an
		// exponential result from cheap cache hits.
		if !r.chargeLeaves(len(hit.leaves)) {
			return nil, true
		}
		// Return a copy so callers appending to their own leaf lists can never
		// alias the cached slice.
		return append([]string(nil), hit.leaves...), hit.tainted
	}
	if !r.chargeLeaves(1) {
		return nil, true
	}
	out := []string{}
	tainted := false
	for _, l := range src.leaves {
		if l.isRef {
			ref := strings.TrimPrefix(l.value, "mg:")
			if ref == "" {
				r.pending(fmt.Sprintf("group %q has an empty modelgroups reference", group))
				tainted = true
				continue
			}
			if target, ok := reservedAliasTarget(ref); ok {
				ref = target
			}
			sources, defined := r.groupSources[ref]
			if !defined {
				if !r.unknownRefs[ref] {
					r.unknownRefs[ref] = true
					r.pending(fmt.Sprintf("group %q references %q which no registered layer defines and no observed reserved group name matches", group, l.value))
				}
				tainted = true
				continue
			}
			next := append(append([]string{}, stack...), group)
			for _, s := range sources {
				leaves, t := r.resolveLeaves(global, ref, s, disabled, next)
				if len(leaves) > 0 && !r.chargeLeaves(len(leaves)) {
					return nil, true
				}
				out = append(out, leaves...)
				tainted = tainted || t
			}
			continue
		}
		usable, known := r.available(global, l.value, disabled)
		if !known {
			r.pending(fmt.Sprintf("group %q references model %q which no registered layer defines; dynamic catalog composition is unproven", group, l.value))
			tainted = true
			continue
		}
		if usable {
			if !r.chargeLeaves(1) {
				return nil, true
			}
			out = append(out, l.value)
		}
	}
	r.memo[key] = groupResolution{leaves: append([]string(nil), out...), tainted: tainted}
	return out, tainted
}

// resolveGroups computes GroupsBefore/GroupsAfter for every configured group
// plus the observed derived reserved groups, and records the emptiness
// evidence that drives the verdict. Emptiness proven on a fully resolved
// graph is unsafe; emptiness on a tainted graph (cycles, unknown leaves) is
// only pending-unknown, because the unresolvable part cannot be proven
// available or unavailable.
func (r *resolver) resolveGroups(global *parsedLayer) {
	names := make([]string, 0, len(r.groupSources))
	for name := range r.groupSources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var before, after []string
		tainted := false
		for _, src := range r.groupSources[name] {
			leaves, t := r.resolveLeaves(global, name, src, false, nil)
			before = append(before, leaves...)
			tainted = tainted || t
			leaves, t = r.resolveLeaves(global, name, src, true, nil)
			after = append(after, leaves...)
			tainted = tainted || t
		}
		r.before[name], r.after[name] = before, after
		switch {
		case len(after) == 0 && len(before) > 0 && tainted:
			r.pending(fmt.Sprintf("group %q resolves to no available leaves, but unresolved references leave the post-disable graph unproven", name))
		case len(after) == 0 && len(before) > 0:
			if isTierDefault(name) {
				r.unsafe(fmt.Sprintf("disabling %q leaves tier default %q without any available model; the binary rejects such reloads wholesale", r.disable, name))
			} else {
				r.unsafe(fmt.Sprintf("disabling %q leaves group %q without any available leaf; the binary rejects all-unavailable groups", r.disable, name))
			}
		case len(after) == 0:
			// Already unusable before the proposal: the registered root is not
			// currently valid, so no disable can be proven safe through it.
			if isTierDefault(name) {
				r.pending(fmt.Sprintf("tier default %q already has no available model; the registered root is not currently valid", name))
			} else {
				r.pending(fmt.Sprintf("group %q already has no available leaf; the registered root is not currently valid", name))
			}
		}
	}
	// Publish the derived reserved groups as aliases of their tier defaults,
	// matching the observed catalog.
	for _, alias := range []string{reservedGroupFull, reservedGroupMini} {
		target, _ := reservedAliasTarget(alias)
		if _, defined := r.groupSources[target]; defined {
			r.before[alias] = r.before[target]
			r.after[alias] = r.after[target]
		}
	}
}

// checkDefinitions verifies every facet/subagent reference from the
// registered roots stays usable after the disable. A primary reference that
// dies is provable breakage (unsafe); a reference that was already dead, or
// whose fallback list would lose entries, involves semantics that were never
// pinned against the binary (pending-unknown).
func (r *resolver) checkDefinitions(global *parsedLayer) {
	for _, def := range r.definitions {
		where := def.Path
		if where == "" {
			where = "(definition)"
		}
		if def.Model != "" {
			r.checkReference(global, def.Model, where, "primary")
		}
		alive, deadAfter, deadBefore, unknown := 0, 0, 0, 0
		for _, fb := range def.Fallbacks {
			switch r.referenceState(global, fb) {
			case refUsable:
				alive++
			case refDeadAfter:
				deadAfter++
			case refDeadBefore:
				deadBefore++
			case refUnknown:
				unknown++
			}
		}
		switch {
		case len(def.Fallbacks) == 0:
		case unknown > 0:
			r.pending(fmt.Sprintf("definition %s lists a fallback that no registered layer defines; fallback composition is unproven", where))
		case alive == 0:
			r.pending(fmt.Sprintf("definition %s would keep no usable fallback; fallback semantics are unproven", where))
		case deadAfter > 0:
			r.pending(fmt.Sprintf("disabling %q would drop %d of %d fallback references of definition %s; fallback semantics are unproven", r.disable, deadAfter, len(def.Fallbacks), where))
		case deadBefore > 0:
			r.pending(fmt.Sprintf("definition %s lists %d already-unavailable fallback references; the registered root is not currently valid", where, deadBefore))
		}
	}
}

type refState int

const (
	refUsable refState = iota
	refDeadAfter
	refDeadBefore
	refUnknown
)

// referenceState classifies one reference before and after the proposed
// disable.
func (r *resolver) referenceState(global *parsedLayer, ref string) refState {
	name := ref
	if strings.HasPrefix(ref, "mg:") {
		name = strings.TrimPrefix(ref, "mg:")
		if name == "" {
			return refUnknown
		}
	}
	if target, ok := reservedAliasTarget(name); ok {
		name = target
	}
	if _, defined := r.groupSources[name]; defined && (strings.HasPrefix(ref, "mg:") || isReserved(name)) {
		before, after := r.before[name], r.after[name]
		switch {
		case len(after) > 0:
			return refUsable
		case len(before) > 0:
			return refDeadAfter
		default:
			return refDeadBefore
		}
	}
	if strings.HasPrefix(ref, "mg:") {
		// A group-shaped reference to an undefined, non-reserved name.
		return refUnknown
	}
	beforeUsable, knownBefore := r.available(global, name, false)
	if !knownBefore {
		return refUnknown
	}
	afterUsable, _ := r.available(global, name, true)
	switch {
	case afterUsable:
		return refUsable
	case beforeUsable:
		return refDeadAfter
	default:
		return refDeadBefore
	}
}

func (r *resolver) checkReference(global *parsedLayer, ref, where, kind string) {
	switch r.referenceState(global, ref) {
	case refUsable:
	case refUnknown:
		r.pending(fmt.Sprintf("definition %s %s reference %q cannot be resolved; dynamic catalog composition is unproven", where, kind, ref))
	case refDeadAfter:
		r.unsafe(fmt.Sprintf("disabling %q kills the %s reference %q of definition %s; the reference would no longer resolve", r.disable, kind, ref, where))
	case refDeadBefore:
		r.pending(fmt.Sprintf("definition %s %s reference %q is already unavailable; the registered root is not currently valid", where, kind, ref))
	}
}

// parseLayer strictly parses the relevant subset of one layer's config.yaml:
// providers enabled flags, the models catalog, and modelgroups (scalar or
// sequence values). The version key is not read: configs of any declared
// version are analyzed with this same grammar. Duplicate keys anywhere in the
// document are schema violations. Unrelated keys and provider entry fields
// (url, kind, auth, ...) are never read. The returned string is a sanitized
// reason for the pending-unknown verdict, empty on success.
func parseLayer(l Layer) (*parsedLayer, string) {
	out := &parsedLayer{providers: map[string]bool{}, models: map[string]modelDef{}, groups: map[string][]leaf{}}
	what := "global layer"
	if !l.Global {
		what = fmt.Sprintf("project layer %q", l.ID)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(l.Config, &doc); err != nil {
		return out, fmt.Sprintf("%s config does not parse: %v", what, err)
	}
	if len(doc.Content) == 0 {
		return out, fmt.Sprintf("%s config is empty", what)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return out, fmt.Sprintf("%s config is not a mapping", what)
	}
	if reason := checkDuplicateKeys(root, what); reason != "" {
		return out, reason
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		switch key.Value {
		case "providers":
			out.hasProviders = true
			if val.Kind != yaml.MappingNode {
				return out, fmt.Sprintf("%s providers must be a mapping", what)
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				key := val.Content[j]
				if key.Tag != "!!str" || key.Value == "" {
					return out, fmt.Sprintf("%s providers names must be non-empty strings", what)
				}
				id := key.Value
				entry := val.Content[j+1]
				if id == "" || entry.Kind != yaml.MappingNode {
					return out, fmt.Sprintf("%s providers entries must be non-empty mappings", what)
				}
				enabled := true
				for k := 0; k+1 < len(entry.Content); k += 2 {
					if entry.Content[k].Value == "enabled" {
						if entry.Content[k+1].Tag != "!!bool" {
							return out, fmt.Sprintf("%s providers.%s.enabled must be a boolean", what, id)
						}
						enabled = entry.Content[k+1].Value == "true"
					}
				}
				out.providers[id] = enabled
			}
		case "models":
			out.hasModels = true
			if val.Kind != yaml.MappingNode {
				return out, fmt.Sprintf("%s models must be a mapping", what)
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				key := val.Content[j]
				if key.Tag != "!!str" || key.Value == "" {
					return out, fmt.Sprintf("%s models names must be non-empty strings", what)
				}
				name := key.Value
				entry := val.Content[j+1]
				if name == "" || entry.Kind != yaml.MappingNode {
					return out, fmt.Sprintf("%s models entries must be non-empty mappings", what)
				}
				def := modelDef{enabled: true}
				haveProvider := false
				for k := 0; k+1 < len(entry.Content); k += 2 {
					switch entry.Content[k].Value {
					case "provider":
						if entry.Content[k+1].Tag != "!!str" || entry.Content[k+1].Value == "" {
							return out, fmt.Sprintf("%s models.%s.provider must be a non-empty string", what, name)
						}
						def.provider = entry.Content[k+1].Value
						haveProvider = true
					case "enabled":
						if entry.Content[k+1].Tag != "!!bool" {
							return out, fmt.Sprintf("%s models.%s.enabled must be a boolean", what, name)
						}
						def.enabled = entry.Content[k+1].Value == "true"
					}
				}
				if !haveProvider {
					// The provider field is optional: the observed model-name
					// shape <provider>/<model>(<effort>) names its owner
					// before the first slash, and the effort suffix never
					// affects it.
					provider, ok := modelProviderFromName(name)
					if !ok {
						return out, fmt.Sprintf("%s models.%s has no provider field and its name is not <provider>/<model>; ownership is unprovable", what, name)
					}
					def.provider = provider
				}
				out.models[name] = def
			}
		case "modelgroups":
			if val.Kind != yaml.MappingNode {
				return out, fmt.Sprintf("%s modelgroups must be a mapping", what)
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				key := val.Content[j]
				gval := val.Content[j+1]
				if key.Tag != "!!str" || key.Value == "" {
					return out, fmt.Sprintf("%s modelgroups names must be non-empty strings", what)
				}
				name := key.Value
				entries := []*yaml.Node{gval}
				if gval.Kind == yaml.SequenceNode {
					entries = gval.Content
				} else if gval.Kind != yaml.ScalarNode {
					return out, fmt.Sprintf("%s modelgroups group %q must be a leaf sequence", what, name)
				}
				leaves := []leaf{}
				for _, item := range entries {
					if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || item.Value == "" {
						return out, fmt.Sprintf("%s modelgroups group %q leaves must be non-empty strings", what, name)
					}
					if strings.HasPrefix(item.Value, "mg:") {
						leaves = append(leaves, leaf{isRef: true, value: item.Value})
						continue
					}
					// A bare name is a concrete model reference, never a group
					// reference (observed: the binary rejects bare group-name
					// leaves as no-available-leaf).
					leaves = append(leaves, leaf{value: item.Value})
				}
				out.groups[name] = leaves
				out.groupOrder = append(out.groupOrder, name)
			}
		case "defaults":
			out.hasDefaults = true
		}
	}
	return out, ""
}

// checkDuplicateKeys walks the whole document and reports the first duplicate
// mapping key as a schema violation. Only key names and line numbers are
// included — never values.
func checkDuplicateKeys(n *yaml.Node, what string) string {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if reason := checkDuplicateKeys(c, what); reason != "" {
				return reason
			}
		}
		return ""
	case yaml.MappingNode:
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if prev, dup := seen[key.Value]; dup {
				return fmt.Sprintf("%s config has duplicate key %q at line %d (first defined at line %d); ambiguous keys are refused", what, key.Value, key.Line, prev)
			}
			seen[key.Value] = key.Line
			if reason := checkDuplicateKeys(n.Content[i+1], what); reason != "" {
				return reason
			}
		}
		return ""
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if reason := checkDuplicateKeys(c, what); reason != "" {
				return reason
			}
		}
		return ""
	default:
		// Scalars and alias nodes: alias content was already checked at the
		// anchor.
		return ""
	}
}

// ReadLayer gathers one registered root's layer: exactly config.yaml plus the
// policy allowlisted facet/subagent definition files under root. It never
// enumerates any other path. Project roots may be registered at the project
// directory; the configuration directory is resolved with the shared
// canonical-root rule.
func ReadLayer(id, root string, global bool) (Layer, error) {
	cfgDir := root
	if !global {
		canonical, err := policy.CanonicalProjectRoot(root)
		if err != nil {
			return Layer{}, fmt.Errorf("groupsafety: project %s: %w", id, err)
		}
		cfgDir = canonical
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		return Layer{}, fmt.Errorf("groupsafety: read %s config: %w", layerKind(global), err)
	}
	layer := Layer{ID: id, Global: global, Config: data}
	paths, err := policy.DiscoverManagedFiles(cfgDir)
	if err != nil {
		return Layer{}, fmt.Errorf("groupsafety: discover %s definitions: %w", layerKind(global), err)
	}
	for _, rel := range paths {
		raw, err := os.ReadFile(filepath.Join(cfgDir, filepath.FromSlash(rel)))
		if err != nil {
			return Layer{}, fmt.Errorf("groupsafety: read definition %q: %w", rel, err)
		}
		src, ok, err := policy.ReadManagedDefinition(raw)
		if err != nil {
			return Layer{}, fmt.Errorf("groupsafety: parse definition %q: %w", rel, err)
		}
		if !ok {
			continue
		}
		layer.Definitions = append(layer.Definitions, Definition{Path: rel, Model: src.Model, Fallbacks: src.FallbackModels})
	}
	return layer, nil
}

func layerKind(global bool) string {
	if global {
		return "global"
	}
	return "project"
}
