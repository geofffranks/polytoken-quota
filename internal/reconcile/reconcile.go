// Package reconcile is the pure desired-chain reconciler for the polytoken-quota
// utility. It transforms a desired policy plus observed provider state into an
// abstract Plan of managed FieldEdits. It is a pure function of its inputs: it
// never touches bytes, files, or the filesystem. The Plan is consumed by staging
// (which materializes candidate files) and the coordinator.
//
// For each desired chain the reconciler normalizes entries into a base model plus an
// optional reasoning suffix, resolves the base to exactly one provider mapping,
// drops entries whose provider is disabled, stable-partitions the survivors
// (normal first, then reserve), and projects them onto managed fields. A disabled
// provider forces its models to enabled:false; a healthy provider restores each
// model's desired baseline. A required chain with no survivor is a typed render
// failure that produces no edits.
package reconcile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/geofffranks/codexbar-hooks/internal/policy"
	"github.com/geofffranks/codexbar-hooks/internal/state"
)

// configFile names the managed config.yaml document (relative to a target root) that
// holds the global scalar fields and per-model enable flags.
const configFile = "config.yaml"

// ModelRef is a normalized desired chain entry. Spelling is the exact desired
// preference (including any reasoning suffix); Base is the portion used for
// provider-mode matching; Suffix is the reasoning level without its parentheses
// (empty when none).
type ModelRef struct {
	Spelling string
	Base     string
	Suffix   string
}

// FieldEdit is one abstract managed-field change addressed at the key level. File is
// the facet/subagent definition path for list edits, or configFile for global
// config.yaml edits. Path addresses a key sequence (e.g. ["defaults","full"] or
// ["models","codex/gpt-5.6-sol","enabled"]). Exactly one of Scalar, Sequence, or
// Enabled carries the value; Remove signals that the key should be removed.
type FieldEdit struct {
	File     string
	Path     []string
	Scalar   *string
	Sequence []string
	Enabled  *bool
	Remove   bool
}

// Plan is the reconciled set of managed edits for one target, stamped with the
// observed state revision it was computed from.
type Plan struct {
	TargetID string
	Revision uint64
	Edits    []FieldEdit
}

// EmptyChainError is the typed render failure returned when a required chain has no
// survivor after disabled filtering. No live edit is produced for the target.
type EmptyChainError struct {
	TargetID string
	File     string
	Field    string
}

func (e EmptyChainError) Error() string {
	return "required chain has no survivor: " + e.TargetID + ":" + e.Field
}

// ParseModelRef splits a desired chain entry into its base model and an optional
// reasoning suffix. "codex/gpt-5.6-sol(medium)" yields base "codex/gpt-5.6-sol" and
// suffix "medium"; a bare entry yields Base == Spelling with an empty suffix. An
// empty reference or an unbalanced suffix is an error.
func ParseModelRef(entry string) (ModelRef, error) {
	if entry == "" {
		return ModelRef{}, errors.New("reconcile: empty model reference")
	}
	open := strings.IndexByte(entry, '(')
	if open < 0 {
		return ModelRef{Spelling: entry, Base: entry}, nil
	}
	closeIdx := strings.LastIndexByte(entry, ')')
	if closeIdx <= open {
		return ModelRef{}, fmt.Errorf("reconcile: unbalanced suffix in %q", entry)
	}
	return ModelRef{
		Spelling: entry,
		Base:     entry[:open],
		Suffix:   entry[open+1 : closeIdx],
	}, nil
}

// survivor is a desired chain entry that survived disabled filtering, with its
// effective mode for partitioning.
type survivor struct {
	ref  ModelRef
	mode state.Mode
}

// modeRank orders effective modes from least to most degraded: normal < reserve <
// disabled. Unknown values rank as normal.
func modeRank(m state.Mode) int {
	switch m {
	case state.ModeDisabled:
		return 2
	case state.ModeReserve:
		return 1
	default:
		return 0
	}
}

// mappingMode derives a mapping's effective mode from the observed state of its
// CodExBar providers (the state machine keys providers by their CodExBar ID). A
// provider absent from the observed state is healthy. When a mapping covers several
// providers, the most-degraded mode wins, so a degraded provider never leaves its
// models over-promoted.
func mappingMode(d policy.Desired, s state.State, id policy.MappingID) state.Mode {
	m, ok := d.Providers[id]
	if !ok {
		return state.ModeNormal
	}
	worst := state.ModeNormal
	for _, cb := range m.CodexBarProviders {
		ps, seen := s.Providers[cb]
		if !seen {
			continue
		}
		mode := state.EffectiveMode(ps)
		if modeRank(mode) > modeRank(worst) {
			worst = mode
		}
	}
	return worst
}

// Build reconciles one target's desired chains against the observed provider state
// and returns the abstract managed edits. It is deterministic and never mutates its
// inputs. A non-empty chain whose survivors are all disabled yields an
// EmptyChainError and a plan with no edits; an empty desired chain is treated as
// unmanaged and skipped.
func Build(desired policy.Desired, observed state.State, target policy.Target) (Plan, error) {
	plan := Plan{TargetID: target.ID, Revision: observed.Revision}

	// survivors resolves, filters, and stable-partitions a desired chain: disabled
	// entries are dropped, normal survivors precede reserve survivors, and desired
	// relative order is preserved within each partition.
	survivors := func(c policy.Chain) ([]survivor, error) {
		var reserve []survivor
		normal := make([]survivor, 0, len(c))
		for _, entry := range c {
			ref, err := ParseModelRef(entry)
			if err != nil {
				return nil, err
			}
			mid, err := desired.ResolveModel(ref.Base)
			if err != nil {
				return nil, err
			}
			switch mode := mappingMode(desired, observed, mid); mode {
			case state.ModeDisabled:
				continue
			case state.ModeReserve:
				reserve = append(reserve, survivor{ref: ref, mode: mode})
			default:
				normal = append(normal, survivor{ref: ref, mode: mode})
			}
		}
		return append(normal, reserve...), nil
	}

	// Scalar config fields: write only the first survivor, never a fallback.
	for _, sp := range []struct {
		field string
		path  []string
		chain policy.Chain
	}{
		{"defaults.full", []string{"defaults", "full"}, target.Full},
		{"defaults.mini", []string{"defaults", "mini"}, target.Mini},
		{"defaults.nano", []string{"defaults", "nano"}, target.Nano},
		{"autonomous_permission_matcher.classifier_model", []string{"autonomous_permission_matcher", "classifier_model"}, target.Classifier},
	} {
		if len(sp.chain) == 0 {
			continue // unmanaged for this target
		}
		sv, err := survivors(sp.chain)
		if err != nil {
			return emptyPlan(target, observed), err
		}
		if len(sv) == 0 {
			return emptyPlan(target, observed), EmptyChainError{TargetID: target.ID, File: configFile, Field: sp.field}
		}
		v := sv[0].ref.Spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: configFile, Path: sp.path, Scalar: &v})
	}

	// Facet/subagent definitions: first survivor -> polytoken.model, rest ->
	// polytoken.fallback_models; with no remaining survivor the fallback list is
	// cleared so a disabled fallback cannot linger.
	for _, def := range target.Definitions {
		if len(def.Chain) == 0 {
			continue
		}
		sv, err := survivors(def.Chain)
		if err != nil {
			return emptyPlan(target, observed), err
		}
		if len(sv) == 0 {
			return emptyPlan(target, observed), EmptyChainError{TargetID: target.ID, File: def.Path, Field: "polytoken.model"}
		}
		primary := sv[0].ref.Spelling
		plan.Edits = append(plan.Edits, FieldEdit{File: def.Path, Path: []string{"polytoken", "model"}, Scalar: &primary})
		rest := make([]string, 0, len(sv)-1)
		for _, s2 := range sv[1:] {
			rest = append(rest, s2.ref.Spelling)
		}
		fb := FieldEdit{File: def.Path, Path: []string{"polytoken", "fallback_models"}}
		if len(rest) > 0 {
			fb.Sequence = rest
		} else {
			fb.Remove = true
		}
		plan.Edits = append(plan.Edits, fb)
	}

	// Baseline enable state for every managed model, deterministically ordered.
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, idStr := range ids {
		id := policy.MappingID(idStr)
		mapping := desired.Providers[id]
		mode := mappingMode(desired, observed, id)
		bases := make([]string, 0, len(mapping.Models))
		for base := range mapping.Models {
			bases = append(bases, base)
		}
		sort.Strings(bases)
		for _, base := range bases {
			enabled := false
			if mode != state.ModeDisabled {
				enabled = mapping.Models[base].Enabled
			}
			plan.Edits = append(plan.Edits, FieldEdit{
				File:    configFile,
				Path:    []string{"models", base, "enabled"},
				Enabled: &enabled,
			})
		}
	}

	return plan, nil
}

// emptyPlan returns a no-edit plan carrying only the target identity and observed
// revision, used when a chain fails to render.
func emptyPlan(target policy.Target, observed state.State) Plan {
	return Plan{TargetID: target.ID, Revision: observed.Revision}
}
