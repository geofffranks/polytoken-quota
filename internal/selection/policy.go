// Package selection parses strict version-1 candidate policies and performs
// pure, quota-aware candidate selection for Jev model selection.
//
// ParsePolicy turns an operator-authored YAML document into a validated Policy:
// exact difficulty tiers holding ordered candidate groups of exact model
// references, every reference resolved against the desired policy graph.
// Select then answers a single pure question — given the desired policy, the
// durable observed state, and a reference time, which candidate should serve a
// phase and tier? It never launches work, reserves quota, or touches I/O.
package selection

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// Tier is a difficulty tier name in a candidate policy. The four tiers are
// exact: no aliases, no additional levels, no case variants.
type Tier string

// The difficulty tiers, named exactly as they must appear in a candidate
// policy document.
const (
	TierRoutine       Tier = "routine"
	TierNormal        Tier = "normal"
	TierDifficult     Tier = "difficult"
	TierVeryDifficult Tier = "very_difficult"
)

// Tiers lists the difficulty tiers in increasing order of difficulty.
var Tiers = []Tier{TierRoutine, TierNormal, TierDifficult, TierVeryDifficult}

// TierIndex returns t's position in Tiers, or -1 when t is not a known tier.
func TierIndex(t Tier) int {
	for i, known := range Tiers {
		if t == known {
			return i
		}
	}
	return -1
}

// ValidTier reports whether t is one of the four known difficulty tiers.
func ValidTier(t Tier) bool {
	return TierIndex(t) >= 0
}

// PolicyVersion is the only candidate-policy schema version this build parses.
const PolicyVersion = 1

// MaxPolicyBytes bounds a candidate policy document. A candidate policy is a
// small, hand-authored file; 64 KiB is generous for every phase and tier and
// bounds parse cost on untrusted input.
const MaxPolicyBytes = 64 << 10

// Group is one candidate group: models explicitly considered interchangeable
// for the phase and tier, in policy order. Members are exact model references
// and may carry a reasoning suffix (e.g. "codex/gpt-5.6-sol(medium)"); the
// suffix spelling is preserved verbatim through selection.
type Group []string

// TierPolicy is the ordered candidate groups for one difficulty tier.
type TierPolicy struct {
	Groups []Group
}

// PhasePolicy maps each difficulty tier of one phase to its candidate groups.
// A phase may declare any non-empty subset of the four tiers; selecting an
// absent tier is a Select-time error, never a borrow from another tier.
type PhasePolicy map[Tier]TierPolicy

// Policy is a parsed candidate policy: candidate groups per phase and tier.
type Policy struct {
	// Version is the parsed schema version; always PolicyVersion.
	Version int
	// Phases maps a selection phase name ("execution", "planning", ...) to its
	// per-tier candidate groups. Phase names are arbitrary non-empty keys.
	Phases map[string]PhasePolicy
}

// ParsePolicy parses a strict version-1 candidate policy document and resolves
// every candidate reference against the desired policy graph. It rejects
// oversize input, empty documents, unsupported or missing versions, unknown
// keys, duplicate keys, trailing documents, empty phases/tiers/groups, and
// duplicate candidate references within a tier. Every reference must parse
// under the canonical policy.ParseModelRef grammar and resolve to exactly one
// managed model via Desired.ResolveModel — exact matches only, no similarity
// guessing.
func ParsePolicy(data []byte, desired policy.Desired) (Policy, error) {
	if len(data) > MaxPolicyBytes {
		return Policy{}, fmt.Errorf("selection: policy document is %d bytes, over the %d byte limit", len(data), MaxPolicyBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var w docWire
	if err := dec.Decode(&w); err != nil {
		if errors.Is(err, io.EOF) {
			return Policy{}, errors.New("selection: empty policy document")
		}
		return Policy{}, fmt.Errorf("selection: parse: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return Policy{}, errors.New("selection: unexpected trailing document after the policy")
	}
	if w.Version == nil || *w.Version != PolicyVersion {
		v := 0
		if w.Version != nil {
			v = *w.Version
		}
		return Policy{}, fmt.Errorf("selection: unsupported or missing version %d (want %d)", v, PolicyVersion)
	}
	if len(w.Phases) == 0 {
		return Policy{}, errors.New("selection: policy defines no phases")
	}

	p := Policy{Version: PolicyVersion, Phases: make(map[string]PhasePolicy, len(w.Phases))}
	names := make([]string, 0, len(w.Phases))
	for name := range w.Phases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return Policy{}, errors.New("selection: phase name must not be empty")
		}
		pp, err := phaseFromWire(name, w.Phases[name], desired)
		if err != nil {
			return Policy{}, err
		}
		p.Phases[name] = pp
	}
	return p, nil
}

// phaseWire is the on-disk shape of one phase. Pointers distinguish a missing
// tier (rejected) from a present one; unknown tier keys are rejected by
// KnownFields.
type phaseWire struct {
	Routine       *[][]string `yaml:"routine"`
	Normal        *[][]string `yaml:"normal"`
	Difficult     *[][]string `yaml:"difficult"`
	VeryDifficult *[][]string `yaml:"very_difficult"`
}

// docWire is the on-disk shape of a candidate policy document.
type docWire struct {
	Version *int                 `yaml:"version"`
	Phases  map[string]phaseWire `yaml:"phases"`
}

// phaseFromWire validates one phase: at least one tier present, each present
// tier holding ordered non-empty groups of resolvable exact references. A
// phase may omit tiers — selecting an omitted tier is a Select-time error,
// never permission to borrow another tier.
func phaseFromWire(name string, w phaseWire, desired policy.Desired) (PhasePolicy, error) {
	pp := make(PhasePolicy, len(Tiers))
	wired := map[Tier]*[][]string{
		TierRoutine:       w.Routine,
		TierNormal:        w.Normal,
		TierDifficult:     w.Difficult,
		TierVeryDifficult: w.VeryDifficult,
	}
	present := 0
	for _, tier := range Tiers {
		groups := wired[tier]
		if groups == nil {
			continue
		}
		tp, err := tierFromWire(name, tier, *groups, desired)
		if err != nil {
			return nil, err
		}
		pp[tier] = tp
		present++
	}
	if present == 0 {
		return nil, fmt.Errorf("selection: phase %q: defines no tiers", name)
	}
	return pp, nil
}

// tierFromWire validates one tier's ordered groups: at least one group, no
// empty group, no empty or malformed reference, every reference resolved
// against the desired graph, and no duplicate reference within the tier. The
// same reference may appear in multiple tiers — difficulty floors differ.
func tierFromWire(phase string, tier Tier, groups [][]string, desired policy.Desired) (TierPolicy, error) {
	if len(groups) == 0 {
		return TierPolicy{}, fmt.Errorf("selection: phase %q: tier %q: at least one candidate group is required", phase, tier)
	}
	out := make([]Group, 0, len(groups))
	seen := make(map[string]int, len(groups))
	for gi, group := range groups {
		if len(group) == 0 {
			return TierPolicy{}, fmt.Errorf("selection: phase %q: tier %q: group %d is empty", phase, tier, gi)
		}
		g := make(Group, 0, len(group))
		for _, ref := range group {
			base, _, err := policy.ParseModelRef(ref)
			if err != nil {
				return TierPolicy{}, fmt.Errorf("selection: phase %q: tier %q: group %d: %w", phase, tier, gi, err)
			}
			if _, err := desired.ResolveModel(base); err != nil {
				return TierPolicy{}, fmt.Errorf("selection: phase %q: tier %q: group %d: %w", phase, tier, gi, err)
			}
			if prev, dup := seen[ref]; dup {
				return TierPolicy{}, fmt.Errorf("selection: phase %q: tier %q: duplicate candidate %q (groups %d and %d)", phase, tier, ref, prev, gi)
			}
			seen[ref] = gi
			g = append(g, ref)
		}
		out = append(out, g)
	}
	return TierPolicy{Groups: out}, nil
}

// FamilyOf returns the provider family of a model reference: the prefix before
// the first slash. A reference without a slash is its own family. This is the
// identity Select matches against excludedFamilies.
func FamilyOf(modelRef string) string {
	if base, _, err := policy.ParseModelRef(modelRef); err == nil {
		modelRef = base
	}
	if i := strings.IndexByte(modelRef, '/'); i >= 0 {
		return modelRef[:i]
	}
	return modelRef
}
