// Package notice renders the tool-neutral reconciliation notice document
// published after a mutating command changes managed Polytoken fields.
//
// The notice is a schema-versioned JSON object of facts — revision, effective
// model chains in the daemon's ModelConfig.name registry-key space, changed
// managed fields, and disabled models. It contains only managed-field facts:
// no credentials, environment, or unrelated configuration can appear.
package notice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// SchemaVersion is the notice document schema version.
const SchemaVersion = 1

// Chain is one named effective chain on a target. Models are desired-chain
// spellings; Render resolves each to a registry key (base model) or null.
type Chain struct {
	Name   string
	Models []string
}

// Target is one notice target: the global target (Kind "global") or one
// managed definition file (Kind "definition").
type Target struct {
	ID            string
	Kind          string
	File          string   // policy-relative definition path (definition targets)
	Chains        []Chain  // global target chains, canonical order
	Chain         []string // definition target effective chain
	ChangedFields [][]string
}

// Input is the pure renderer input assembled by the publication adapter.
// KnownModels is the set of managed base models (the models registry keys);
// entries absent from it render as null.
type Input struct {
	Revision       uint64
	PublishedAt    time.Time
	RoutingEnabled bool
	Targets        []Target
	KnownModels    map[string]bool
	DisabledModels []string
}

type chainDoc struct {
	Name   string    `json:"name"`
	Models []*string `json:"models"`
}

type targetDoc struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	File          string     `json:"file,omitempty"`
	Facet         string     `json:"facet,omitempty"`
	Chains        []chainDoc `json:"chains,omitempty"`
	Chain         []*string  `json:"chain,omitempty"`
	ChangedFields [][]string `json:"changed_fields,omitempty"`
}

type document struct {
	Schema         int         `json:"schema"`
	Revision       uint64      `json:"revision"`
	PublishedAt    string      `json:"published_at"`
	RoutingEnabled bool        `json:"routing_enabled"`
	Targets        []targetDoc `json:"targets"`
	DisabledModels []*string   `json:"disabled_models"`
}

// Render produces the deterministic schema-v1 notice document. Global targets
// sort first, remaining targets by ID; empty chains are omitted; empty-chain
// definition targets are omitted entirely; disabled models are deduped on
// their base model and sorted. Unresolvable entries (not in KnownModels, or
// not parseable by the canonical model-ref grammar) render as JSON null.
func Render(in Input) ([]byte, error) {
	if in.PublishedAt.IsZero() {
		return nil, fmt.Errorf("notice: PublishedAt is required")
	}
	doc := document{
		Schema:         SchemaVersion,
		Revision:       in.Revision,
		PublishedAt:    in.PublishedAt.UTC().Format(time.RFC3339),
		RoutingEnabled: in.RoutingEnabled,
		Targets:        make([]targetDoc, 0, len(in.Targets)),
		DisabledModels: resolveDisabled(in.DisabledModels, in.KnownModels),
	}

	targets := append([]Target(nil), in.Targets...)
	sort.SliceStable(targets, func(i, j int) bool {
		gi, gj := isGlobal(targets[i]), isGlobal(targets[j])
		if gi != gj {
			return gi
		}
		return targets[i].ID < targets[j].ID
	})
	for _, t := range targets {
		td, ok := renderTarget(t, in.KnownModels)
		if !ok {
			continue
		}
		doc.Targets = append(doc.Targets, td)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("notice: encode: %w", err)
	}
	return buf.Bytes(), nil
}

func isGlobal(t Target) bool { return t.Kind == "global" }

func renderTarget(t Target, known map[string]bool) (targetDoc, bool) {
	td := targetDoc{ID: t.ID, Kind: t.Kind, ChangedFields: t.ChangedFields}
	if !isGlobal(t) {
		if len(t.Chain) == 0 {
			return targetDoc{}, false
		}
		td.File = t.File
		td.Facet = facetName(t.File)
		td.Chain = resolveAll(t.Chain, known)
		return td, true
	}
	for _, ch := range t.Chains {
		if len(ch.Models) == 0 {
			continue
		}
		td.Chains = append(td.Chains, chainDoc{Name: ch.Name, Models: resolveAll(ch.Models, known)})
	}
	return td, true
}

// facetName derives the definition registry name from the policy-relative
// file path: the basename without its extension ("subagents/work-api.md" ->
// "work-api"). Empty for an empty path.
func facetName(file string) string {
	if file == "" {
		return ""
	}
	base := path.Base(file)
	return strings.TrimSuffix(base, path.Ext(base))
}

// resolveAll resolves desired-chain spellings to registry keys in order.
func resolveAll(entries []string, known map[string]bool) []*string {
	out := make([]*string, 0, len(entries))
	for _, e := range entries {
		out = append(out, resolveOne(e, known))
	}
	return out
}

// resolveDisabled dedupes entries on their base model, sorts, and resolves
// each to a registry key; unresolvable entries render as null.
func resolveDisabled(entries []string, known map[string]bool) []*string {
	seen := make(map[string]bool, len(entries))
	bases := make([]string, 0, len(entries))
	for _, e := range entries {
		base, _, err := policy.ParseModelRef(e)
		if err != nil {
			continue
		}
		if !seen[base] {
			seen[base] = true
			bases = append(bases, base)
		}
	}
	sort.Strings(bases)
	out := make([]*string, 0, len(bases))
	for _, b := range bases {
		out = append(out, resolveOne(b, known))
	}
	return out
}

func resolveOne(entry string, known map[string]bool) *string {
	base, _, err := policy.ParseModelRef(entry)
	if err != nil || !known[base] {
		return nil
	}
	key := base
	return &key
}
