package service

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// RouteProjection is one core or explicitly registered managed route.
type RouteProjection struct {
	TargetID   string   `json:"target_id"`
	Name       string   `json:"name"`
	SourcePath string   `json:"source_path,omitempty"`
	Desired    []string `json:"desired,omitempty"`
	Effective  []string `json:"effective"`
}

// RoutingReport is the bare routing selector: effective routes plus local errors.
type RoutingReport struct {
	AsOf    time.Time         `json:"as_of"`
	Routes  []RouteProjection `json:"routes,omitempty"`
	Errors  []DiagnosticError `json:"errors,omitempty"`
	Partial bool              `json:"partial"`
	Error   string            `json:"error,omitempty"`
}

// RoutingExplainReport adds enablement, full ranks, and desired chains.
type RoutingExplainReport struct {
	AsOf           time.Time         `json:"as_of"`
	RoutingEnabled bool              `json:"routing_enabled"`
	Ranks          []RankEntryReport `json:"ranks,omitempty"`
	Routes         []RouteProjection `json:"routes,omitempty"`
	Errors         []DiagnosticError `json:"errors,omitempty"`
	Partial        bool              `json:"partial"`
	Error          string            `json:"error,omitempty"`
}

func projectRoutes(desired policy.Desired, observed state.State, targets []RegisteredTarget, ranks reconcile.RankLookup) ([]RouteProjection, []DiagnosticError) {
	orderedTargets := append([]RegisteredTarget(nil), targets...)
	sort.SliceStable(orderedTargets, func(i, j int) bool {
		if orderedTargets[i].Policy.Global != orderedTargets[j].Policy.Global {
			return orderedTargets[i].Policy.Global
		}
		return orderedTargets[i].Policy.ID < orderedTargets[j].Policy.ID
	})
	var routes []RouteProjection
	var routeErrors []DiagnosticError
	for _, registered := range orderedTargets {
		for _, core := range []struct {
			name  string
			chain policy.Chain
		}{{"full", registered.Policy.Full}, {"mini", registered.Policy.Mini}, {"nano", registered.Policy.Nano}} {
			if len(core.chain) == 0 {
				continue
			}
			route, projectionError := projectRoute(desired, observed, ranks, registered.Policy.ID, core.name, "", core.chain)
			routes = append(routes, route)
			if projectionError != nil {
				routeErrors = append(routeErrors, *projectionError)
			}
		}
		named, namedErrors := projectNamedRoutes(desired, observed, ranks, registered.Resolved.Definitions)
		routes = append(routes, named...)
		routeErrors = append(routeErrors, namedErrors...)
	}
	return routes, routeErrors
}

func projectNamedRoutes(desired policy.Desired, observed state.State, ranks reconcile.RankLookup, definitions []target.ResolvedDefinition) ([]RouteProjection, []DiagnosticError) {
	type item struct {
		definition target.ResolvedDefinition
		name       string
	}
	items := make([]item, 0, len(definitions))
	counts := make(map[string]int, len(definitions))
	var routeErrors []DiagnosticError
	for _, definition := range definitions {
		metadata, err := target.ReadDefinitionMetadata(definition)
		name := metadata.Name
		if err != nil {
			name = definitionFallbackName(definition.PolicyPath)
			routeErrors = append(routeErrors, DiagnosticError{
				Scope: ErrorScopeRoute, TargetID: definition.TargetID,
				SourcePath: definition.PolicyPath, Summary: "definition metadata unavailable",
			})
		}
		items = append(items, item{definition: definition, name: name})
		counts[name]++
	}
	var routes []RouteProjection
	for _, item := range items {
		name := item.name
		if counts[name] > 1 {
			name += " (" + item.definition.PolicyPath + ")"
		}
		route, projectionError := projectRoute(desired, observed, ranks, item.definition.TargetID, name, item.definition.PolicyPath, item.definition.Chain)
		routes = append(routes, route)
		if projectionError != nil {
			routeErrors = append(routeErrors, *projectionError)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		left, right := strings.ToLower(routes[i].Name), strings.ToLower(routes[j].Name)
		if left != right {
			return left < right
		}
		return routes[i].SourcePath < routes[j].SourcePath
	})
	return routes, routeErrors
}

func projectRoute(desired policy.Desired, observed state.State, ranks reconcile.RankLookup, targetID, name, sourcePath string, chain policy.Chain) (RouteProjection, *DiagnosticError) {
	route := RouteProjection{
		TargetID: targetID, Name: name, SourcePath: sourcePath,
		Desired: append([]string(nil), chain...),
	}
	effective, err := reconcile.EffectiveOrder(desired, observed, chain, ranks)
	if err == nil {
		route.Effective = append([]string(nil), effective...)
		return route, nil
	}
	return route, &DiagnosticError{
		Scope: ErrorScopeRoute, TargetID: targetID, SourcePath: sourcePath,
		Summary: "route projection unavailable",
	}
}

func definitionFallbackName(policyPath string) string {
	base := path.Base(policyPath)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" || name == "." || name == "/" {
		return "definition"
	}
	return name
}

func cloneRoutes(in []RouteProjection, includeDesired bool) []RouteProjection {
	out := make([]RouteProjection, len(in))
	for i := range in {
		out[i] = in[i]
		if includeDesired {
			out[i].Desired = append([]string(nil), in[i].Desired...)
		} else {
			out[i].Desired = nil
		}
		out[i].Effective = append([]string(nil), in[i].Effective...)
	}
	return out
}
