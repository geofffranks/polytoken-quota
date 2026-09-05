package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

var diagnosticAsOf = time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)

type diagnosticDeps struct {
	desired           policy.Desired
	observed          state.State
	targets           []RegisteredTarget
	policyErr         error
	stateErr          error
	targetErr         error
	policyExists      bool
	policyLoads       int
	desiredExistCalls int
	stateLoads        int
	targetLoads       int
	clockReads        int
}

func (d *diagnosticDeps) LoadPolicy() (policy.Desired, error) {
	d.policyLoads++
	return d.desired, d.policyErr
}
func (d *diagnosticDeps) DesiredExists() bool {
	d.desiredExistCalls++
	return d.policyExists
}
func (d *diagnosticDeps) LoadState() (state.State, error) {
	d.stateLoads++
	return d.observed, d.stateErr
}
func (*diagnosticDeps) Save(state.State) error { return nil }
func (d *diagnosticDeps) ResolveTargets(policy.Desired) ([]RegisteredTarget, error) {
	d.targetLoads++
	return d.targets, d.targetErr
}
func (d *diagnosticDeps) Now() time.Time {
	d.clockReads++
	return diagnosticAsOf
}

func diagnosticCoordinator(d *diagnosticDeps) *Coordinator {
	return &Coordinator{Policy: d, State: d, Targets: d, Clock: d}
}

func diagnosticFixture(t *testing.T, routingEnabled bool) (*diagnosticDeps, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private", "CANARY-CANONICAL-ROOT")
	for rel, body := range map[string]string{
		"config.yaml":        "defaults: {}\n",
		"subagents/zeta.md":  "---\nname: Shared\npolytoken:\n  model: live/zeta\n---\n",
		"subagents/alpha.md": "---\nname: Shared\npolytoken:\n  model: live/alpha\n---\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	schedule, err := routing.ParseSchedule("UTC", []routing.OffPeakWindow{{
		Days: []routing.DayOfWeek{routing.Monday}, Start: "00:00", End: "24:00",
	}})
	if err != nil {
		t.Fatal(err)
	}
	past := diagnosticAsOf.Add(-time.Minute)
	equal := diagnosticAsOf
	future := diagnosticAsOf.Add(30 * time.Minute)
	later := diagnosticAsOf.Add(2 * time.Hour)
	used, limit, pct := 80.0, 100.0, 20.0
	hasCredits, unlimited, balance := true, false, "1E+2"
	spendLimit, spendUsed, spendRemaining := 1000.0, 250.0, 750.0
	observedAt := diagnosticAsOf.Add(-10 * time.Minute)
	failedAt := diagnosticAsOf.Add(-5 * time.Minute)
	resetState := quota.ResetCreditState{
		UsageSummary: &quota.CodexUsageSummary{
			ObservedAt:   observedAt,
			Credits:      &quota.UsageCredits{HasCredits: &hasCredits, Unlimited: &unlimited, Balance: &balance},
			SpendControl: &quota.SpendControl{Limit: &spendLimit, Used: &spendUsed, Remaining: &spendRemaining, ResetAt: &later},
		},
		LastSuccess: &quota.ResetCreditInventory{
			ServerAvailableCount: 4,
			UsableCount:          4,
			AvailableExpiries:    []*time.Time{&past, &equal, &future, nil},
			DiscrepancyCount:     1,
			SkippedCount:         2,
			ObservedAt:           observedAt,
		},
		LatestAttempt: &quota.ResetCreditAttempt{Status: quota.CreditAttemptFailed, At: failedAt, Error: "Bearer CANARY-RESET-SECRET"},
	}
	desired := policy.Desired{
		Version: 1,
		Routing: policy.RoutingConfig{Enabled: routingEnabled},
		Providers: map[policy.MappingID]policy.Mapping{
			"alpha": {
				Models: map[string]policy.ModelBaseline{"alpha/full": {Enabled: true}},
				Quota:  &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour, BalanceGroup: "g", Weight: 1, Schedule: &schedule},
			},
			"beta": {
				Models: map[string]policy.ModelBaseline{"beta/full": {Enabled: true}},
				Quota:  &policy.QuotaConfig{Adapter: "anthropic", FreshnessTTL: time.Hour, BalanceGroup: "g", Weight: 1, MonthlyBudgetUSD: 500},
			},
		},
		Global: policy.Target{
			ID: "global", Root: root, Global: true,
			Full: policy.Chain{"beta/full", "alpha/full"}, Mini: policy.Chain{"alpha/full"}, Nano: policy.Chain{"beta/full"},
			Classifier: policy.Chain{"alpha/full", "beta/full"},
			Definitions: []policy.Definition{
				{Path: "subagents/zeta.md", Chain: policy.Chain{"beta/full", "alpha/full"}},
				{Path: "subagents/alpha.md", Chain: policy.Chain{"alpha/full", "beta/full"}},
			},
		},
		Projects: []policy.Target{{ID: "project-b", Root: root, Full: policy.Chain{"alpha/full"}}},
	}
	observed := state.State{Revision: 7, Providers: map[string]state.ProviderState{
		"alpha": {
			Quota: state.QuotaLow, Availability: state.Available,
			QuotaSnapshot: &quota.QuotaSnapshot{
				MappingID: "alpha", CheckedAt: observedAt, Availability: quota.QuotaAvailable, Status: quota.SourceFresh,
				Windows: []quota.QuotaWindow{
					{Name: "past", UsagePercent: &pct, ResetAt: &past},
					{Name: "equal", Used: &used, Limit: &limit, ResetAt: &equal},
					{Name: "future", UsagePercent: &pct, ResetAt: &future},
					{Name: "later", UsagePercent: &pct, ResetAt: &later},
				},
			},
			QuotaAttempt: &quota.QuotaSnapshot{MappingID: "alpha", CheckedAt: failedAt, Status: quota.SourceFailed, Error: "Bearer CANARY-ATTEMPT-SECRET"},
			ResetCredits: resetState,
		},
		"beta": {
			Quota: state.QuotaNormal, Availability: state.Available,
			QuotaSnapshot: &quota.QuotaSnapshot{MappingID: "beta", CheckedAt: observedAt, Availability: quota.QuotaAvailable, Status: quota.SourceFresh, Windows: []quota.QuotaWindow{{Name: "budget", Used: &spendUsed, Limit: &spendLimit, ResetAt: &later}}},
		},
	}}
	resolved, err := NewTargetRegistry().ResolveTargets(desired)
	if err != nil {
		t.Fatal(err)
	}
	return &diagnosticDeps{desired: desired, observed: observed, targets: resolved, policyExists: true}, root
}

func TestProjectLegacyQuotaIncludesUnobservedMappings(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.desired.Providers["codex"] = policy.Mapping{Models: map[string]policy.ModelBaseline{"codex/model": {Enabled: true}}, Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour}}
	d.desired.Providers["legacy"] = policy.Mapping{Models: map[string]policy.ModelBaseline{"legacy/model": {Enabled: true}}}
	rows, _ := projectLegacyQuota(d.desired, d.observed, diagnosticAsOf)
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.MappingID] = true
	}
	for _, id := range []string{"alpha", "beta", "codex", "legacy"} {
		if !seen[id] {
			t.Fatalf("provider %q missing from legacy projection: %+v", id, rows)
		}
	}
}

func TestProjectProvidersIncludesUnobservedMappings(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.desired.Providers["codex"] = policy.Mapping{Models: map[string]policy.ModelBaseline{"codex/model": {Enabled: true}}, Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: time.Hour}}
	d.desired.Providers["legacy"] = policy.Mapping{Models: map[string]policy.ModelBaseline{"legacy/model": {Enabled: true}}}
	providers, _ := projectProviders(d.desired, d.observed, diagnosticAsOf)
	seen := map[string]bool{}
	for _, provider := range providers {
		seen[provider.MappingID] = true
	}
	for _, id := range []string{"alpha", "beta", "codex", "legacy"} {
		if !seen[id] {
			t.Fatalf("provider %q missing from projection: %+v", id, providers)
		}
	}
}

func TestMergedStatusViewConsolidatesSnapshot(t *testing.T) {
	observedAt := diagnosticAsOf.Add(-10 * time.Minute)
	future := diagnosticAsOf.Add(30 * time.Minute)
	d, _ := diagnosticFixture(t, true)
	// Stagger beta's snapshot so the global last-checked is alpha's.
	d.observed.Providers["beta"].QuotaSnapshot.CheckedAt = observedAt.Add(-5 * time.Minute)
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()

	if report.Error != "" {
		t.Fatalf("unexpected fatal error: %q", report.Error)
	}
	if !report.RoutingEnabled {
		t.Fatal("RoutingEnabled = false, want true")
	}
	if !report.LastChecked.Equal(observedAt) {
		t.Fatalf("LastChecked = %s, want max snapshot CheckedAt %s", report.LastChecked, observedAt)
	}
	if len(report.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(report.Providers))
	}
	alpha, beta := report.Providers[0], report.Providers[1]
	if alpha.Provider != "alpha" || beta.Provider != "beta" {
		t.Fatalf("provider order = %s,%s want alpha,beta", alpha.Provider, beta.Provider)
	}
	if alpha.Status != "available" || beta.Status != "available" {
		t.Fatalf("status = %s,%s want available,available", alpha.Status, beta.Status)
	}
	if len(alpha.Windows) != 4 || len(beta.Windows) != 1 {
		t.Fatalf("windows = %d,%d want 4,1", len(alpha.Windows), len(beta.Windows))
	}
	if alpha.NextResetAt == nil || !alpha.NextResetAt.Equal(future) {
		t.Fatalf("alpha NextResetAt = %v, want earliest reset after as-of %s", alpha.NextResetAt, future)
	}
	if len(report.Routes) == 0 {
		t.Fatal("routes empty")
	}
	for _, route := range report.Routes {
		if len(route.Desired) == 0 {
			t.Fatalf("route %q has empty desired chain", route.Name)
		}
	}
	if !report.Problem {
		t.Fatal("Problem = false, want true (failed attempt and missing remaining)")
	}
}

func TestMergedStatusViewStatusPrecedence(t *testing.T) {
	observedAt := diagnosticAsOf.Add(-10 * time.Minute)
	tests := []struct {
		name   string
		mutate func(*diagnosticDeps)
		want   map[string]string
	}{
		{
			name: "manual disable wins over available snapshot",
			mutate: func(d *diagnosticDeps) {
				ps := d.observed.Providers["alpha"]
				ps.ManualDisabled = true
				d.observed.Providers["alpha"] = ps
			},
			want: map[string]string{"alpha": "disabled", "beta": "available"},
		},
		{
			name: "never observed quota-mapped provider shows enabled",
			mutate: func(d *diagnosticDeps) {
				mapping := d.desired.Providers["alpha"]
				d.desired.Providers["gamma"] = mapping
			},
			want: map[string]string{"alpha": "available", "beta": "available", "gamma": "enabled"},
		},
		{
			name: "manual disable wins for a never-observed provider",
			mutate: func(d *diagnosticDeps) {
				d.desired.Providers["gamma"] = d.desired.Providers["alpha"]
				d.observed.Providers["gamma"] = state.ProviderState{ManualDisabled: true}
			},
			want: map[string]string{"alpha": "available", "beta": "available", "gamma": "disabled"},
		},
		{
			name: "unavailable axis wins over snapshot",
			mutate: func(d *diagnosticDeps) {
				ps := d.observed.Providers["beta"]
				ps.Availability = state.Unavailable
				d.observed.Providers["beta"] = ps
			},
			want: map[string]string{"alpha": "available", "beta": "unavailable"},
		},
		{
			name: "failed attempt without snapshot is unavailable",
			mutate: func(d *diagnosticDeps) {
				ps := d.observed.Providers["beta"]
				ps.QuotaSnapshot = nil
				ps.QuotaAttempt = &quota.QuotaSnapshot{MappingID: "beta", CheckedAt: observedAt, Status: quota.SourceFailed}
				ps.Availability = state.Available
				d.observed.Providers["beta"] = ps
			},
			want: map[string]string{"alpha": "available", "beta": "unavailable"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := diagnosticFixture(t, true)
			tc.mutate(d)
			report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
			if report.Error != "" {
				t.Fatalf("unexpected fatal error: %q", report.Error)
			}
			got := map[string]string{}
			for _, provider := range report.Providers {
				got[provider.Provider] = provider.Status
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("status = %v, want %v", got, tc.want)
			}
			for _, provider := range report.Providers {
				if tc.want[provider.Provider] == "enabled" {
					if provider.Windows != nil || provider.NextResetAt != nil {
						t.Fatalf("enabled provider %s carries quota data: %+v", provider.Provider, provider)
					}
				}
			}
		})
	}
}

func TestMergedStatusViewNeverCheckedAndFatal(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	// No provider observations at all: last-checked is zero and every provider is enabled.
	d.observed.Providers = map[string]state.ProviderState{}
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
	if !report.LastChecked.IsZero() {
		t.Fatalf("LastChecked = %s, want zero", report.LastChecked)
	}
	for _, provider := range report.Providers {
		if provider.Status != "enabled" {
			t.Fatalf("provider %s status = %s, want enabled", provider.Provider, provider.Status)
		}
	}

	d, _ = diagnosticFixture(t, true)
	d.policyErr = errors.New("Bearer POLICY-CANARY")
	report = diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
	if report.Error == "" || strings.Contains(report.Error, "CANARY") {
		t.Fatalf("fatal error missing/unsanitized: %q", report.Error)
	}
	if report.Providers != nil || report.Routes != nil || report.RoutingEnabled {
		t.Fatalf("fatal report carries data: providers=%d routes=%d routing=%v", len(report.Providers), len(report.Routes), report.RoutingEnabled)
	}
}

func TestMergedStatusViewPendingTargetsAndErrors(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.observed.Targets = map[string]state.TargetState{
		"project-b": {Pending: &state.ApplyFailure{Stage: "apply"}},
	}
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).MergedStatusView()
	if len(report.PendingTargets) != 1 || report.PendingTargets[0] != "project-b" {
		t.Fatalf("PendingTargets = %v, want [project-b]", report.PendingTargets)
	}
	if report.Error != "" {
		t.Fatalf("unexpected fatal error: %q", report.Error)
	}
}

func TestDiagnosticSnapshotSingleReadSingleClock(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	status := snapshot.StatusView()
	routingView := snapshot.RoutingView()
	explain := snapshot.RoutingExplainView()
	_ = snapshot.StatusView()
	_ = snapshot.RoutingView()
	_ = snapshot.RoutingExplainView()

	if d.policyLoads != 1 || d.stateLoads != 1 || d.targetLoads != 1 || d.clockReads != 1 {
		t.Fatalf("loads policy/state/targets/clock = %d/%d/%d/%d, want 1/1/1/1", d.policyLoads, d.stateLoads, d.targetLoads, d.clockReads)
	}
	if !status.AsOf.Equal(diagnosticAsOf) || !routingView.AsOf.Equal(diagnosticAsOf) || !explain.AsOf.Equal(diagnosticAsOf) {
		t.Fatalf("selectors do not share AsOf: status=%s routing=%s explain=%s", status.AsOf, routingView.AsOf, explain.AsOf)
	}

	for name, invoke := range map[string]func(*Coordinator){
		"legacy status":  func(c *Coordinator) { _ = c.Status(context.Background(), false) },
		"legacy explain": func(c *Coordinator) { _ = c.RankingExplain(context.Background()) },
	} {
		t.Run(name+" adapter", func(t *testing.T) {
			d, _ := diagnosticFixture(t, true)
			invoke(diagnosticCoordinator(d))
			if d.policyLoads != 1 || d.stateLoads != 1 || d.targetLoads != 1 || d.clockReads != 1 {
				t.Fatalf("adapter loads policy/state/targets/clock = %d/%d/%d/%d, want 1/1/1/1", d.policyLoads, d.stateLoads, d.targetLoads, d.clockReads)
			}
		})
	}

	// Every shared-input class fails at selector level and never repeats a read.
	for name, configure := range map[string]func(*diagnosticDeps){
		"policy":  func(d *diagnosticDeps) { d.policyErr = errors.New("Bearer POLICY-CANARY") },
		"state":   func(d *diagnosticDeps) { d.stateErr = errors.New("Bearer STATE-CANARY") },
		"targets": func(d *diagnosticDeps) { d.targetErr = errors.New("/private/CANARY-TARGET") },
	} {
		t.Run(name+" failure", func(t *testing.T) {
			d, _ := diagnosticFixture(t, true)
			configure(d)
			s := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
			for selector, failure := range map[string]string{
				"status": s.StatusView().Error, "routing": s.RoutingView().Error, "explain": s.RoutingExplainView().Error,
			} {
				if failure == "" || strings.Contains(failure, "CANARY") {
					t.Fatalf("%s selector failure not present/sanitized: %q", selector, failure)
				}
			}
			if d.policyLoads > 1 || d.stateLoads > 1 || d.targetLoads > 1 || d.clockReads != 1 {
				t.Fatalf("failure loads policy/state/targets/clock = %d/%d/%d/%d", d.policyLoads, d.stateLoads, d.targetLoads, d.clockReads)
			}
		})
	}
}

func TestStatusProviderProjection(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).StatusView()
	if report.Error != "" || len(report.Providers) != 2 || report.Providers[0].MappingID != "alpha" || report.Providers[1].MappingID != "beta" {
		t.Fatalf("status providers=%+v error=%q", report.Providers, report.Error)
	}
	got := report.Providers[0]
	if got.QuotaClass != quota.ClassLow || got.Availability != state.Available || got.EffectiveMode != state.ModeReserve || got.Reason != "quota_low" {
		t.Fatalf("provider axes=%+v", got)
	}
	if got.Adapter != "codex" || got.Freshness != FreshnessFresh || !got.CheckedAt.Equal(diagnosticAsOf.Add(-10*time.Minute)) {
		t.Fatalf("provider freshness/config=%+v", got)
	}
	if len(got.Windows) != 4 || got.Windows[0].ResetAt == nil || got.Windows[1].Remaining == nil || *got.Windows[1].Remaining != .2 {
		t.Fatalf("windows not complete: %+v", got.Windows)
	}
	if got.LatestAttempt == nil || got.LatestAttempt.Status != quota.SourceFailed || strings.Contains(got.LatestAttempt.Error, "CANARY") {
		t.Fatalf("latest attempt missing or unsafe: %+v", got.LatestAttempt)
	}
	if got.Usage == nil || got.Usage.Credits == nil || got.Usage.Credits.Balance == nil || *got.Usage.Credits.Balance != "1E+2" || got.Usage.SpendControl == nil || *got.Usage.SpendControl.Remaining != 750 {
		t.Fatalf("ordinary usage/spend projection=%+v", got.Usage)
	}
	if got.ResetCredits == nil || got.ResetCredits.LastSuccess == nil || got.ResetCredits.LatestAttempt == nil || got.ResetCredits.Status != quota.CreditAttemptFailed {
		t.Fatalf("reset-credit provenance=%+v", got.ResetCredits)
	}
	if got.ResetCredits.ServerAvailableCount != 4 || got.ResetCredits.UsableCount == nil || *got.ResetCredits.UsableCount != 2 || got.ResetCredits.DiscrepancyCount != 1 || got.ResetCredits.SkippedCount != 2 || len(got.ResetCredits.AvailableExpiries) != 2 {
		t.Fatalf("reset-credit summary=%+v", got.ResetCredits)
	}
	if strings.Contains(got.ResetCredits.LatestAttempt.Error, "CANARY") {
		t.Fatalf("reset-credit error leaked: %q", got.ResetCredits.LatestAttempt.Error)
	}
}

func TestQuotaExemptMappingStatusAndRouteSafety(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.desired.Providers["minime"] = policy.Mapping{
		Models: map[string]policy.ModelBaseline{"minime/qwen": {Enabled: true}},
	}
	d.desired.Global.Classifier = policy.Chain{"alpha/full", "minime/qwen", "beta/full"}
	d.desired.Global.Definitions[0].Chain = policy.Chain{"beta/full", "minime/qwen", "alpha/full"}
	d.observed.Providers["minime"] = state.ProviderState{
		QuotaAttempt: &quota.QuotaSnapshot{MappingID: "minime", CheckedAt: diagnosticAsOf.Add(-time.Hour), Status: quota.SourceFailed, Error: "stale local history"},
	}
	resolved, err := NewTargetRegistry().ResolveTargets(d.desired)
	if err != nil {
		t.Fatal(err)
	}
	d.targets = resolved

	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	status := snapshot.StatusView()
	foundMinime := false
	for _, provider := range status.Providers {
		if provider.MappingID == "minime" {
			foundMinime = true
		}
	}
	if !foundMinime {
		t.Fatalf("configured quota-exempt mapping missing from status: %+v", status.Providers)
	}
	legacy := diagnosticCoordinator(d).Status(context.Background(), false)
	baseline := *d
	baseline.observed = d.observed
	baseline.observed.Providers = maps.Clone(d.observed.Providers)
	delete(baseline.observed.Providers, "minime")
	withoutLocalHistory := diagnosticCoordinator(&baseline).Status(context.Background(), false)
	if legacy.Problem != withoutLocalHistory.Problem {
		t.Fatalf("quota-exempt durable history changed status problem: with=%t without=%t", legacy.Problem, withoutLocalHistory.Problem)
	}
	foundMinime = false
	for _, provider := range legacy.Providers {
		if provider.Provider == "minime" {
			foundMinime = true
		}
	}
	if !foundMinime {
		t.Fatalf("configured quota-exempt mapping missing from merged status: %+v", legacy.Providers)
	}
	for _, route := range snapshot.RoutingView().Routes {
		if route.Name == "classifier" || route.Name == "Shared (subagents/zeta.md)" {
			if !slices.Contains(route.Effective, "minime/qwen") {
				t.Fatalf("quota-exempt model missing from %s route: %+v", route.Name, route)
			}
		}
	}
}

func TestStatusResetAfterAsOfOnly(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).StatusView()
	got := report.Providers[0]
	if got.NextResetAt == nil || !got.NextResetAt.Equal(diagnosticAsOf.Add(30*time.Minute)) {
		t.Fatalf("next reset=%v, want strictly future earliest", got.NextResetAt)
	}
	if len(got.Windows) != 4 || !got.Windows[0].ResetAt.Equal(diagnosticAsOf.Add(-time.Minute)) || !got.Windows[1].ResetAt.Equal(diagnosticAsOf) {
		t.Fatalf("raw reset timestamps were filtered or rewritten: %+v", got.Windows)
	}
	persisted := d.observed.Providers["alpha"].ResetCredits.LastSuccess.AvailableExpiries
	if len(persisted) != 4 || persisted[0] == nil || !persisted[0].Equal(diagnosticAsOf.Add(-time.Minute)) || persisted[1] == nil || !persisted[1].Equal(diagnosticAsOf) {
		t.Fatalf("report-time reset-credit filtering mutated durable state: %+v", persisted)
	}
}

func TestStatusNextResetUsesQuotaCycleAnchor(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	fiveHours := 5 * time.Hour
	week := 7 * 24 * time.Hour
	sparkReset := diagnosticAsOf.Add(1 * time.Hour)
	sessionReset := diagnosticAsOf.Add(2 * time.Hour)
	sparkWeeklyReset := diagnosticAsOf.Add(2 * 24 * time.Hour)
	weeklyReset := diagnosticAsOf.Add(3 * 24 * time.Hour)
	pct := 20.0
	d.observed.Providers["alpha"].QuotaSnapshot.Windows = []quota.QuotaWindow{
		{Name: "session", UsagePercent: &pct, Period: &fiveHours, ResetAt: &sessionReset},
		{Name: "weekly", UsagePercent: &pct, Period: &week, ResetAt: &weeklyReset},
		{Name: "spark", UsagePercent: &pct, Period: &fiveHours, ResetAt: &sparkReset},
		{Name: "spark-weekly", UsagePercent: &pct, Period: &week, ResetAt: &sparkWeeklyReset},
	}
	report := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).StatusView()
	for _, provider := range report.Providers {
		if provider.MappingID != "alpha" {
			continue
		}
		if provider.NextResetAt == nil || !provider.NextResetAt.Equal(weeklyReset) {
			t.Fatalf("alpha next reset=%v want weekly quota-cycle reset %v", provider.NextResetAt, weeklyReset)
		}
		return
	}
	t.Fatal("alpha missing from status view")
}

func TestStatusCommandResponsibility(t *testing.T) {
	d, _ := diagnosticFixture(t, false)
	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	status := snapshot.StatusView()
	if len(status.Providers) != 2 || status.Error != "" {
		t.Fatalf("status=%+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"routes"`, `"ranking"`, `"routing_enabled"`, `"desired"`, `"effective"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("status owns routing field %q: %s", forbidden, encoded)
		}
	}

	// Mutating one selector result must not mutate a later selector or durable input.
	status.Providers[0].Windows[0].Name = "mutated"
	*status.Providers[0].Windows[0].ResetAt = time.Time{}
	*status.Providers[0].Usage.Credits.Balance = "mutated"
	status.Providers[0].ResetCredits.AvailableExpiries[0] = nil
	again := snapshot.StatusView()
	if again.Providers[0].Windows[0].Name == "mutated" || again.Providers[0].Windows[0].ResetAt.IsZero() || *again.Providers[0].Usage.Credits.Balance == "mutated" || again.Providers[0].ResetCredits.AvailableExpiries[0] == nil {
		t.Fatalf("selector DTO mutation reached shared snapshot: %+v", again.Providers[0])
	}
}

func TestStatusIgnoresRouteLocalErrors(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	path := filepath.Join(d.desired.Global.Root, "subagents", "zeta.md")
	if err := os.WriteFile(path, []byte("---\nname: [broken\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	status := snapshot.StatusView()
	if status.Error != "" || len(status.Providers) != 2 {
		t.Fatalf("route-local failure poisoned status: %+v", status)
	}
	if routingView := snapshot.RoutingView(); !routingView.Partial || len(routingView.Errors) != 1 {
		t.Fatalf("routing selector did not retain local failure: %+v", routingView)
	}
}

func TestStatusPreservesManualDisabled(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	disabled := d.observed.Providers["alpha"]
	disabled.ManualDisabled = true
	d.observed.Providers["alpha"] = disabled
	report := diagnosticCoordinator(d).Status(context.Background(), false)
	if report.Error != "" {
		t.Fatalf("status error=%q", report.Error)
	}
	var got *MergedStatusProvider
	for i := range report.Providers {
		if report.Providers[i].Provider == "alpha" {
			got = &report.Providers[i]
		}
	}
	if got == nil {
		t.Fatalf("alpha provider missing from status: %+v", report.Providers)
	}
	if got.Status != StatusDisabled {
		t.Fatalf("status=%q want %q (manual disable)", got.Status, StatusDisabled)
	}
}

func TestRoutingViewIncludesClassifierAndConcreteSources(t *testing.T) {
	d, canonicalRoot := diagnosticFixture(t, true)
	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	bare := snapshot.RoutingView()
	explain := snapshot.RoutingExplainView()
	if bare.Error != "" || bare.Partial || len(bare.Errors) != 0 || len(bare.Routes) != 7 {
		t.Fatalf("bare routing=%+v", bare)
	}
	wantNames := []string{"full", "mini", "nano", "classifier", "Shared (subagents/alpha.md)", "Shared (subagents/zeta.md)", "full"}
	wantTargets := []string{"global", "global", "global", "global", "global", "global", "project-b"}
	wantSources := []string{"config.yaml", "config.yaml", "config.yaml", "config.yaml", "subagents/alpha.md", "subagents/zeta.md", "config.yaml"}
	for i, want := range wantNames {
		if bare.Routes[i].Name != want || bare.Routes[i].TargetID != wantTargets[i] || bare.Routes[i].SourcePath != wantSources[i] {
			t.Fatalf("route[%d]=%+v want name=%q target=%q source=%q; routes=%+v", i, bare.Routes[i], want, wantTargets[i], wantSources[i], bare.Routes)
		}
		if bare.Routes[i].Desired != nil {
			t.Fatalf("bare routing leaked desired chain: %+v", bare.Routes[i])
		}
	}
	if len(explain.Routes) != len(bare.Routes) || !explain.RoutingEnabled || len(explain.Ranks) != 2 {
		t.Fatalf("explain selector incomplete: %+v", explain)
	}
	for i := range bare.Routes {
		if len(bare.Routes[i].Effective) == 0 {
			t.Fatalf("bare route lost effective chain at %d: %+v", i, bare.Routes[i])
		}
		if explain.Routes[i].Desired == "" || explain.Routes[i].Effective == "" {
			t.Fatalf("explain route lost scalar values at %d: %+v", i, explain.Routes[i])
		}
	}
	encoded, err := json.Marshal(struct {
		Bare    RoutingReport
		Explain RoutingExplainReport
	}{bare, explain})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{canonicalRoot, "CANARY-CANONICAL-ROOT"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("routing output leaked canonical path %q: %s", forbidden, encoded)
		}
	}

	bare.Routes[0].Effective[0] = "mutated"
	explain.Routes[0].Desired = "mutated"
	again := snapshot.RoutingExplainView()
	if again.Routes[0].Effective == "mutated" || again.Routes[0].Desired == "mutated" {
		t.Fatal("routing selector mutation reached shared snapshot")
	}
}

func TestRoutingExplainComplete(t *testing.T) {
	d, _ := diagnosticFixture(t, false)
	explain := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).RoutingExplainView()
	if explain.Error != "" || explain.RoutingEnabled || len(explain.Ranks) != 2 {
		t.Fatalf("disabled-routing explain lost ranks: %+v", explain)
	}
	for _, rank := range explain.Ranks {
		if rank.MappingID == "" || rank.Rank < 0 || rank.Explanation == "" {
			t.Fatalf("incomplete rank: %+v", rank)
		}
	}
	alpha := explain.Ranks[0]
	if alpha.MappingID != "alpha" || !alpha.Eligible || !alpha.OffPeak || !strings.Contains(alpha.Explanation, "off-peak") {
		t.Fatalf("alpha rank explanation=%+v", alpha)
	}
	if len(explain.Routes) == 0 || !reflect.DeepEqual(explain.Routes[0].Desired, explain.Routes[0].Effective) {
		t.Fatalf("disabled routing effective chain is not baseline: %+v", explain.Routes)
	}
}

func TestRoutingExplainUsesDedicatedScalarProjection(t *testing.T) {
	snapshot := DiagnosticSnapshot{
		routingEnabled: true,
		ranks:          []RankEntryReport{{MappingID: "codex", Rank: 0, Eligible: true, Explanation: "peak"}},
		routes: []RouteProjection{{
			TargetID: "global", Name: "full", SourcePath: "config.yaml",
			Desired:   []string{"codex/gpt-5.6-luna(high)", "zai/glm-5.2"},
			Effective: []string{"zai/glm-5.2", "codex/gpt-5.6-luna(high)"},
		}},
	}
	snapshot.explainRanks = projectExplainRanks(snapshot.ranks)
	snapshot.explainRoutes = projectExplainRoutes(snapshot.routes)

	report := snapshot.RoutingExplainView()
	if got := report.Routes[0].Desired; got != "codex/gpt-5.6-luna(high)" {
		t.Fatalf("explain desired=%q, want top model", got)
	}
	if got := report.Routes[0].Effective; got != "zai/glm-5.2" {
		t.Fatalf("explain effective=%q, want selected model", got)
	}
	if got := report.Ranks[0].Status; got != "ready" {
		t.Fatalf("explain status=%q, want ready", got)
	}
	bare := snapshot.RoutingView()
	if got := bare.Routes[0].SourcePath; got != "config.yaml" {
		t.Fatalf("bare routing source=%q, want config.yaml", got)
	}
	if got := bare.Routes[0].TargetID; got != "global" {
		t.Fatalf("bare routing target=%q, want global", got)
	}
	if got := bare.Routes[0].Desired; len(got) != 0 {
		t.Fatalf("bare routing should not expose desired chain, got %v", got)
	}
	if got := bare.Routes[0].Effective; len(got) != 2 {
		t.Fatalf("shared full-chain effective projection changed: %v", got)
	}
}

func TestRoutingExplainEmptyModelsUseNone(t *testing.T) {
	snapshot := DiagnosticSnapshot{routes: []RouteProjection{{Name: "empty"}}}
	snapshot.explainRoutes = projectExplainRoutes(snapshot.routes)
	report := snapshot.RoutingExplainView()
	if got := report.Routes[0].Desired; got != "none" {
		t.Fatalf("empty desired=%q, want none", got)
	}
	if got := report.Routes[0].Effective; got != "none" {
		t.Fatalf("empty effective=%q, want none", got)
	}
}

func TestRoutingExplainPendingTargetsSanitizedAndCopied(t *testing.T) {
	d, _ := diagnosticFixture(t, true)
	d.observed.Targets = map[string]state.TargetState{
		"z-project": {Pending: &state.ApplyFailure{}},
		"a-project": {Pending: &state.ApplyFailure{}},
		"bad:key":   {Pending: &state.ApplyFailure{}},
		"bad\nkey":  {Pending: &state.ApplyFailure{}},
	}
	snapshot := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background())
	report := snapshot.RoutingExplainView()
	want := []string{"<invalid>", "a-project", "z-project"}
	if !reflect.DeepEqual(report.PendingTargets, want) {
		t.Fatalf("pending targets=%v, want %v", report.PendingTargets, want)
	}
	if d.stateLoads != 1 {
		t.Fatalf("state loads=%d, want 1", d.stateLoads)
	}
	report.PendingTargets[0] = "mutated"
	again := snapshot.RoutingExplainView()
	if !reflect.DeepEqual(again.PendingTargets, want) {
		t.Fatalf("pending targets were not copied: %v", again.PendingTargets)
	}
}

func TestRoutingPartialDefinitionError(t *testing.T) {
	d, canonicalRoot := diagnosticFixture(t, true)
	broken := filepath.Join(d.desired.Global.Root, "subagents", "zeta.md")
	if err := os.WriteFile(broken, []byte("---\nname: [broken\nsecret: CANARY-FRONTMATTER\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	routingView := diagnosticCoordinator(d).BuildDiagnosticSnapshot(context.Background()).RoutingView()
	if routingView.Error != "" || !routingView.Partial || len(routingView.Errors) != 1 {
		t.Fatalf("partial routing classification=%+v", routingView)
	}
	if len(routingView.Routes) != 7 {
		t.Fatalf("trustworthy routes were not preserved: %+v", routingView.Routes)
	}
	err := routingView.Errors[0]
	if err.Scope != ErrorScopeRoute || err.TargetID != "global" || err.SourcePath != "subagents/zeta.md" || err.Summary == "" {
		t.Fatalf("route-local error lacks safe identity: %+v", err)
	}
	encoded, marshalErr := json.Marshal(routingView)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, forbidden := range []string{canonicalRoot, "CANARY-CANONICAL-ROOT", "CANARY-FRONTMATTER", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("partial route output leaked %q: %s", forbidden, encoded)
		}
	}
}
