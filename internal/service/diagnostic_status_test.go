package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func mergedView(t *testing.T, d *diagnosticDeps) MergedStatusReport {
	t.Helper()
	return diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
}

func mergedRouteByName(t *testing.T, report MergedStatusReport, name string) MergedStatusRoute {
	t.Helper()
	for _, route := range report.Routes {
		if route.Name == name {
			return route
		}
	}
	t.Fatalf("route %q not found in %d routes", name, len(report.Routes))
	return MergedStatusRoute{}
}

func TestMergedStatusSkipsDisabledModelWithRankReason(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["alpha"]
	ps.ManualDisabled = true
	d.observed.Providers["alpha"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "alpha/full", Reason: "manual disable"}}) {
		t.Fatalf("full skipped = %+v, want [alpha/full: manual disable]", full.Skipped)
	}
	// The mini chain is entirely alpha/full: all desired entries are skipped.
	mini := mergedRouteByName(t, report, "mini")
	if !reflect.DeepEqual(mini.Skipped, []SkippedModel{{Model: "alpha/full", Reason: "manual disable"}}) {
		t.Fatalf("mini skipped = %+v, want [alpha/full: manual disable]", mini.Skipped)
	}
	if len(mini.Effective) != 0 {
		t.Fatalf("mini effective = %v, want empty", mini.Effective)
	}
	if mini.ProjectionError {
		t.Fatal("mini flagged as projection error, want successful projection")
	}
}

func TestMergedStatusSkipsQuotaExhaustedModel(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.Quota = state.QuotaExhausted
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	// beta/full precedes alpha/full in the desired chain and is dropped with
	// its disabled-mode condition.
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "quota exhausted"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: quota exhausted]", full.Skipped)
	}
}

func TestMergedStatusSkipsUnavailableModel(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.Availability = state.Unavailable
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "unavailable"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: unavailable]", full.Skipped)
	}
}

func TestMergedStatusSnapshotUnavailableRemovesModelAndReportsReason(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	ps := d.observed.Providers["beta"]
	ps.QuotaSnapshot.Availability = quota.QuotaUnavailable
	d.observed.Providers["beta"] = ps
	report := mergedView(t, d)

	full := mergedRouteByName(t, report, "full")
	if !reflect.DeepEqual(full.Skipped, []SkippedModel{{Model: "beta/full", Reason: "unavailable"}}) {
		t.Fatalf("full skipped = %+v, want [beta/full: unavailable]", full.Skipped)
	}
	if len(full.Effective) != 1 || full.Effective[0] != "alpha/full" {
		t.Fatalf("full effective = %v, want [alpha/full]", full.Effective)
	}
}

func TestMergedStatusNoSkipsWhenChainsUntouched(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	report := mergedView(t, d)
	for _, route := range report.Routes {
		if route.ProjectionError {
			t.Fatalf("route %q flagged projection error", route.Name)
		}
		if len(route.Skipped) != 0 {
			t.Fatalf("route %q skipped = %+v, want none", route.Name, route.Skipped)
		}
	}
}

func TestMergedStatusProjectionErrorRoute(t *testing.T) {
	root := t.TempDir()
	desired := policy.Desired{
		Version: 1,
		Routing: policy.RoutingConfig{Enabled: true},
		Providers: map[policy.MappingID]policy.Mapping{
			"alpha": {
				Models: map[string]policy.ModelBaseline{"alpha/full": {Enabled: true}},
			},
		},
		Global: policy.Target{
			ID: "global", Root: root, Global: true,
			Full: policy.Chain{"alpha/full", "ghost/full"},
		},
	}
	resolved, err := NewTargetRegistry().ResolveTargets(desired)
	if err != nil {
		t.Fatal(err)
	}
	d := &diagnosticDeps{desired: desired, observed: state.State{Revision: 1}, targets: resolved, policyExists: true}
	report := mergedView(t, d)
	if report.Error != "" {
		t.Fatalf("unexpected fatal error: %q", report.Error)
	}

	route := mergedRouteByName(t, report, "full")
	if !route.ProjectionError {
		t.Fatalf("route full projection error = false, want true")
	}
	if len(route.Skipped) != 0 {
		t.Fatalf("projection-error route skipped = %+v, want none", route.Skipped)
	}
	found := false
	for _, e := range report.Errors {
		if e.Scope == ErrorScopeRoute && e.TargetID == "global" {
			found = true
		}
	}
	if !found {
		t.Fatalf("route-scope error for target global missing: %+v", report.Errors)
	}
}
