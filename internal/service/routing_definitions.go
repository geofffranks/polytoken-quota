package service

import (
	"sort"
	"strings"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// RoutingDefinition is service-ready route metadata. It intentionally exposes
// only target identity and a normalized policy-relative source path; canonical
// filesystem paths remain private to the target resolver.
type RoutingDefinition struct {
	TargetID   string       `json:"target_id"`
	Name       string       `json:"name"`
	SourcePath string       `json:"source_path,omitempty"`
	Desired    policy.Chain `json:"desired"`
}

const coreRoutingSource = "config.yaml"

type coreRoute struct {
	name  string
	chain policy.Chain
}

func coreRoutes(target policy.Target) []coreRoute {
	return []coreRoute{
		{name: "full", chain: target.Full},
		{name: "mini", chain: target.Mini},
		{name: "nano", chain: target.Nano},
		{name: "classifier", chain: target.Classifier},
	}
}

// RoutingDefinitionMetadata returns core and explicitly registered named route
// metadata without computing effective order or rendering a diagnostic view.
// Those projections belong to the diagnostic snapshot task.
func RoutingDefinitionMetadata(targets []RegisteredTarget) ([]RoutingDefinition, error) {
	orderedTargets := append([]RegisteredTarget(nil), targets...)
	sort.SliceStable(orderedTargets, func(i, j int) bool {
		if orderedTargets[i].Policy.Global != orderedTargets[j].Policy.Global {
			return orderedTargets[i].Policy.Global
		}
		return orderedTargets[i].Policy.ID < orderedTargets[j].Policy.ID
	})

	var routes []RoutingDefinition
	for _, registered := range orderedTargets {
		for _, core := range coreRoutes(registered.Policy) {
			if len(core.chain) == 0 {
				continue
			}
			routes = append(routes, RoutingDefinition{
				TargetID:   registered.Policy.ID,
				Name:       core.name,
				SourcePath: coreRoutingSource,
				Desired:    append(policy.Chain(nil), core.chain...),
			})
		}
		named, err := namedRoutingDefinitions(registered.Resolved.Definitions)
		if err != nil {
			return nil, err
		}
		routes = append(routes, named...)
	}
	return routes, nil
}

func namedRoutingDefinitions(definitions []target.ResolvedDefinition) ([]RoutingDefinition, error) {
	type namedDefinition struct {
		definition target.ResolvedDefinition
		name       string
	}
	named := make([]namedDefinition, 0, len(definitions))
	counts := make(map[string]int, len(definitions))
	for _, definition := range definitions {
		metadata, err := target.ReadDefinitionMetadata(definition)
		if err != nil {
			return nil, err
		}
		named = append(named, namedDefinition{definition: definition, name: metadata.Name})
		counts[metadata.Name]++
	}
	routes := make([]RoutingDefinition, 0, len(named))
	for _, item := range named {
		name := item.name
		if counts[name] > 1 {
			name += " (" + item.definition.PolicyPath + ")"
		}
		routes = append(routes, RoutingDefinition{
			TargetID:   item.definition.TargetID,
			Name:       name,
			SourcePath: item.definition.PolicyPath,
			Desired:    append(policy.Chain(nil), item.definition.Chain...),
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		left, right := strings.ToLower(routes[i].Name), strings.ToLower(routes[j].Name)
		if left != right {
			return left < right
		}
		return routes[i].SourcePath < routes[j].SourcePath
	})
	return routes, nil
}
