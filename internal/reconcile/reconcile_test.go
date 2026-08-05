package reconcile

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// These tests cover the pure desired-chain reconciler. The reconciler transforms a
// desired policy plus observed provider state into an abstract Plan of managed
// FieldEdits. It never touches bytes or files.

// --- shared test helpers -----------------------------------------------------

// fixture builds a Desired whose mappings are keyed by the provider prefix of each
// entry's base model (so "codex/gpt-5.6-sol" -> mapping "codex"), a single target
// with one managed definition "agent.md" carrying the given chain, and an empty
// state at revision 7. Each mapping's CodexBarProviders equals its MappingID so
// setMode keys line up with the state the reconciler inspects.
func fixture(entries ...string) (policy.Desired, state.State, policy.Target) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{}}
	for _, e := range entries {
		ref, err := ParseModelRef(e)
		if err != nil {
			panic(err)
		}
		addMapping(&d, ref.Base)
	}
	chain := append(policy.Chain(nil), entries...)
	target := policy.Target{
		ID:          "t",
		Root:        "/r",
		Definitions: []policy.Definition{{Path: "agent.md", Chain: chain}},
	}
	return d, state.State{Revision: 7}, target
}

// addMapping registers base under a mapping named by its provider prefix, merging
// into an existing mapping when several bases share a provider.
func addMapping(d *policy.Desired, base string) {
	mid := policy.MappingID(providerOf(base))
	m, ok := d.Providers[mid]
	if !ok {
		m = policy.Mapping{
			CodexBarProviders:  []string{string(mid)},
			PolytokenProviders: []string{string(mid)},
			Models:             map[string]policy.ModelBaseline{},
		}
	}
	if _, dup := m.Models[base]; !dup {
		m.Models[base] = policy.ModelBaseline{Enabled: true, HadEnabledKey: false}
	}
	d.Providers[mid] = m
}

// providerOf returns the provider prefix before the first '/'.
func providerOf(base string) string {
	if i := strings.IndexByte(base, '/'); i >= 0 {
		return base[:i]
	}
	return base
}

// setMode writes a ProviderState for key that derives the requested effective mode.
func setMode(s *state.State, key string, mode state.Mode) {
	if s.Providers == nil {
		s.Providers = map[string]state.ProviderState{}
	}
	ps := state.ProviderState{Quota: state.QuotaNormal, Availability: state.Available}
	switch mode {
	case state.ModeDisabled:
		ps.Availability = state.Unavailable
	case state.ModeReserve:
		ps.Quota = state.QuotaLow
	}
	s.Providers[key] = ps
}

// chainEdit reconstructs the ordered managed chain projected for a definition file:
// the polytoken.model scalar followed by the polytoken.fallback_models sequence.
func chainEdit(t *testing.T, p Plan, file string) []string {
	t.Helper()
	var model string
	haveModel := false
	var fallback []string
	for _, e := range p.Edits {
		if e.File != file {
			continue
		}
		if pathEq(e.Path, "polytoken", "model") && e.Scalar != nil {
			model = *e.Scalar
			haveModel = true
		}
		if pathEq(e.Path, "polytoken", "fallback_models") {
			fallback = e.Sequence
		}
	}
	if !haveModel {
		t.Fatalf("no polytoken.model edit for %q in plan %+v", file, p)
	}
	return append([]string{model}, fallback...)
}

// scalarEdit returns the FieldEdit for a two-segment scalar path like ["defaults","full"].
func scalarEdit(t *testing.T, p Plan, group, key string) FieldEdit {
	t.Helper()
	for _, e := range p.Edits {
		if pathEq(e.Path, group, key) {
			return e
		}
	}
	t.Fatalf("no scalar edit for %s.%s in plan %+v", group, key, p)
	return FieldEdit{}
}

// enabledEdit returns the FieldEdit for models.<base>.enabled.
func enabledEdit(t *testing.T, p Plan, base string) FieldEdit {
	t.Helper()
	for _, e := range p.Edits {
		if len(e.Path) == 3 && e.Path[0] == "models" && e.Path[1] == base && e.Path[2] == "enabled" {
			return e
		}
	}
	t.Fatalf("no enabled edit for %q in plan %+v", base, p)
	return FieldEdit{}
}

func pathEq(p []string, parts ...string) bool {
	return slices.Equal(p, parts)
}

// isSubsequence reports whether got appears in full in the same relative order.
func isSubsequence(got, full []string) bool {
	ptr := 0
	for _, g := range got {
		for ptr < len(full) && full[ptr] != g {
			ptr++
		}
		if ptr >= len(full) {
			return false
		}
		ptr++
	}
	return true
}

// --- brief contract tests (verbatim) -----------------------------------------

func TestDesiredChainOnlyStablePartition(t *testing.T) {
	d, s, target := fixture("zai/glm-5.2", "codex/gpt-5.6-sol(medium)", "minime/gemma")
	setMode(&s, "zai", state.ModeDisabled)
	setMode(&s, "codex", state.ModeReserve)
	setMode(&s, "minime", state.ModeNormal)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	want := []string{"minime/gemma", "codex/gpt-5.6-sol(medium)"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for _, emitted := range got {
		if !slices.Contains([]string{"zai/glm-5.2", "codex/gpt-5.6-sol(medium)", "minime/gemma"}, emitted) {
			t.Fatalf("injected %q", emitted)
		}
	}
}

func TestScalarUsesFirstSurvivorOnly(t *testing.T) {
	p := buildScalarFixture(t)
	e := scalarEdit(t, p, "defaults", "full")
	if e.Scalar == nil || *e.Scalar != "healthy/a" || len(e.Sequence) != 0 {
		t.Fatalf("edit=%+v", e)
	}
}

func TestEmptyChainFailsWithoutEdits(t *testing.T) {
	d, s, target := allDisabledFixture()
	p, err := Build(d, s, target, nil)
	var empty EmptyChainError
	if !errors.As(err, &empty) || len(p.Edits) != 0 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

// buildScalarFixture reconciles a target whose defaults.full chain is
// [healthy/a, reserve/c, gone/b]; healthy is normal (first survivor), reserve is
// reserve, gone is disabled.
func buildScalarFixture(t *testing.T) Plan {
	t.Helper()
	d, s, target := fixture("healthy/a", "reserve/c", "gone/b")
	setMode(&s, "healthy", state.ModeNormal)
	setMode(&s, "reserve", state.ModeReserve)
	setMode(&s, "gone", state.ModeDisabled)
	target.Full = policy.Chain{"healthy/a", "reserve/c", "gone/b"}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return p
}

// allDisabledFixture reconciles a target whose every provider is disabled, so every
// non-empty chain has no survivor.
func allDisabledFixture() (policy.Desired, state.State, policy.Target) {
	d, s, target := fixture("zai/glm-5.2", "codex/gpt-5.6-sol")
	setMode(&s, "zai", state.ModeDisabled)
	setMode(&s, "codex", state.ModeDisabled)
	target.Full = policy.Chain{"zai/glm-5.2", "codex/gpt-5.6-sol"}
	return d, s, target
}

// --- rule tests --------------------------------------------------------------

func TestParseModelRef(t *testing.T) {
	cases := []struct {
		in                string
		wantBase, wantSuf string
	}{
		{"codex/gpt-5.6-sol", "codex/gpt-5.6-sol", ""},
		{"codex/gpt-5.6-sol(medium)", "codex/gpt-5.6-sol", "medium"},
		{"zai/glm-5.2(low)", "zai/glm-5.2", "low"},
	}
	for _, tc := range cases {
		ref, err := ParseModelRef(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if ref.Base != tc.wantBase || ref.Suffix != tc.wantSuf || ref.Spelling != tc.in {
			t.Fatalf("%s: got=%+v", tc.in, ref)
		}
	}
	if _, err := ParseModelRef("codex/gpt(low"); err == nil {
		t.Fatal("expected error on unbalanced suffix")
	}
	if _, err := ParseModelRef(""); err == nil {
		t.Fatal("expected error on empty reference")
	}
}

// Rule 1 + 7: a suffixed entry matches its base model for mode filtering but keeps
// its exact desired spelling on output.
func TestSuffixNormalizationPreservesSpelling(t *testing.T) {
	d, s, target := fixture("codex/gpt-5.6-sol(medium)", "minime/gemma")
	setMode(&s, "codex", state.ModeNormal)
	setMode(&s, "minime", state.ModeNormal)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	want := []string{"codex/gpt-5.6-sol(medium)", "minime/gemma"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}

	// Disabling the codex provider removes the suffixed entry via base match.
	setMode(&s, "codex", state.ModeDisabled)
	p2, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := chainEdit(t, p2, "agent.md"); !slices.Equal(got, []string{"minime/gemma"}) {
		t.Fatalf("after disable got=%v", got)
	}
}

// Rule 2: entries whose provider is disabled are removed.
func TestDisabledRemoved(t *testing.T) {
	d, s, target := fixture("a/x", "b/x", "c/x")
	setMode(&s, "a", state.ModeNormal)
	setMode(&s, "b", state.ModeDisabled)
	setMode(&s, "c", state.ModeNormal)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := chainEdit(t, p, "agent.md")
	want := []string{"a/x", "c/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// Rule 3: survivors are stable-partitioned normal-then-reserve, preserving desired
// relative order within each partition.
func TestStablePartitionPreservesRelativeOrder(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		modes map[string]state.Mode
		want  []string
	}{
		{
			name:  "already partitioned",
			in:    []string{"n1/x", "n2/x", "r1/x", "r2/x"},
			modes: map[string]state.Mode{"n1": state.ModeNormal, "n2": state.ModeNormal, "r1": state.ModeReserve, "r2": state.ModeReserve},
			want:  []string{"n1/x", "n2/x", "r1/x", "r2/x"},
		},
		{
			name:  "interleaved",
			in:    []string{"n1/x", "r1/x", "n2/x", "r2/x"},
			modes: map[string]state.Mode{"n1": state.ModeNormal, "n2": state.ModeNormal, "r1": state.ModeReserve, "r2": state.ModeReserve},
			want:  []string{"n1/x", "n2/x", "r1/x", "r2/x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s, target := fixture(tc.in...)
			for k, m := range tc.modes {
				setMode(&s, k, m)
			}
			p, err := Build(d, s, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := chainEdit(t, p, "agent.md")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// Rule 8: a provider that is disabled forces enabled=false on all its models; a
// healthy provider restores each model's desired baseline, so an intentionally
// disabled baseline stays disabled.
func TestBaselineIntentionalDisable(t *testing.T) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex": {
			CodexBarProviders:  []string{"codex"},
			PolytokenProviders: []string{"codex"},
			Models: map[string]policy.ModelBaseline{
				"codex/intentional": {Enabled: false, HadEnabledKey: true},
				"codex/healthy":     {Enabled: true, HadEnabledKey: false},
			},
		},
	}}
	target := policy.Target{ID: "t", Root: "/r"}

	// Healthy provider: baselines restored.
	s := state.State{Revision: 3}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := enabledEdit(t, p, "codex/intentional"); e.Enabled == nil || *e.Enabled {
		t.Fatalf("intentional disable not preserved: %+v", e)
	}
	if e := enabledEdit(t, p, "codex/healthy"); e.Enabled == nil || !*e.Enabled {
		t.Fatalf("healthy baseline not enabled: %+v", e)
	}

	// Disabled provider: every model disabled regardless of baseline.
	setMode(&s, "codex", state.ModeDisabled)
	p2, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"codex/intentional", "codex/healthy"} {
		if e := enabledEdit(t, p2, base); e.Enabled == nil || *e.Enabled {
			t.Fatalf("%s should be disabled when provider disabled: %+v", base, e)
		}
	}
}

// Rule 4: facet/subagent projection promotes the first survivor to polytoken.model
// and emits the rest as polytoken.fallback_models.
func TestFacetPrimaryFallbackProjection(t *testing.T) {
	d, s, target := fixture("a/x", "b/x", "c/x")
	setMode(&s, "a", state.ModeNormal)
	setMode(&s, "b", state.ModeNormal)
	setMode(&s, "c", state.ModeNormal)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	var model string
	var fallback []string
	haveFallback := false
	for _, e := range p.Edits {
		if e.File != "agent.md" {
			continue
		}
		if pathEq(e.Path, "polytoken", "model") && e.Scalar != nil {
			model = *e.Scalar
		}
		if pathEq(e.Path, "polytoken", "fallback_models") {
			fallback = e.Sequence
			haveFallback = true
		}
	}
	if model != "a/x" {
		t.Fatalf("model=%q", model)
	}
	if !haveFallback || !slices.Equal(fallback, []string{"b/x", "c/x"}) {
		t.Fatalf("fallback=%v haveFallback=%v", fallback, haveFallback)
	}
}

// Rule 4 cont.: with a single survivor, fallback_models is cleared (Remove) so a
// previously-present-now-disabled fallback does not linger.
func TestFacetSingleSurvivorClearsFallback(t *testing.T) {
	d, s, target := fixture("a/x", "b/x")
	setMode(&s, "a", state.ModeNormal)
	setMode(&s, "b", state.ModeDisabled)
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	var model string
	var fb *FieldEdit
	for i := range p.Edits {
		e := &p.Edits[i]
		if e.File != "agent.md" {
			continue
		}
		if pathEq(e.Path, "polytoken", "model") && e.Scalar != nil {
			model = *e.Scalar
		}
		if pathEq(e.Path, "polytoken", "fallback_models") {
			fb = e
		}
	}
	if model != "a/x" {
		t.Fatalf("model=%q", model)
	}
	if fb == nil {
		t.Fatal("no fallback_models edit")
	}
	if len(fb.Sequence) != 0 || !fb.Remove {
		t.Fatalf("expected Remove clear of fallback_models, got=%+v", fb)
	}
}

// Rule 5: scalar fields write only the first survivor and never invent a fallback
// sequence. An unfiltered scalar is a clean scalar (no-op-viable) edit.
func TestScalarFieldsFirstSurvivorNoFallback(t *testing.T) {
	d, s, target := fixture("a/x", "b/x", "c/x")
	setMode(&s, "a", state.ModeNormal)
	setMode(&s, "b", state.ModeReserve)
	setMode(&s, "c", state.ModeDisabled)
	target.Full = policy.Chain{"a/x", "b/x", "c/x"}
	target.Classifier = policy.Chain{"a/x", "b/x", "c/x"}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ group, key string }{
		{"defaults", "full"},
		{"autonomous_permission_matcher", "classifier_model"},
	} {
		e := scalarEdit(t, p, tc.group, tc.key)
		if e.Scalar == nil || *e.Scalar != "a/x" {
			t.Fatalf("%s.%s scalar=%v", tc.group, tc.key, e.Scalar)
		}
		if len(e.Sequence) != 0 {
			t.Fatalf("%s.%s invented fallback %v", tc.group, tc.key, e.Sequence)
		}
		if e.Remove {
			t.Fatalf("%s.%s unexpectedly removed", tc.group, tc.key)
		}
	}
}

// Rule 6: the result for each chain is an ordered subsequence of desired survivors
// only; a healthy model belonging to another chain is never injected.
func TestNoModelFromAnotherChain(t *testing.T) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{}}
	for _, base := range []string{"a/1", "a/2", "b/1", "b/2"} {
		addMapping(&d, base)
	}
	s := state.State{Revision: 1}
	target := policy.Target{
		ID:   "t",
		Root: "/r",
		Definitions: []policy.Definition{
			{Path: "one.md", Chain: policy.Chain{"a/1", "a/2"}},
			{Path: "two.md", Chain: policy.Chain{"b/1", "b/2"}},
		},
	}
	p, err := Build(d, s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	one := chainEdit(t, p, "one.md")
	two := chainEdit(t, p, "two.md")
	allowOne := map[string]bool{"a/1": true, "a/2": true}
	allowTwo := map[string]bool{"b/1": true, "b/2": true}
	for _, m := range one {
		if !allowOne[m] {
			t.Fatalf("one.md injected %q", m)
		}
	}
	for _, m := range two {
		if !allowTwo[m] {
			t.Fatalf("two.md injected %q", m)
		}
	}
	if !isSubsequence(one, []string{"a/1", "a/2"}) {
		t.Fatalf("one.md not an ordered subsequence: %v", one)
	}
	if !isSubsequence(two, []string{"b/1", "b/2"}) {
		t.Fatalf("two.md not an ordered subsequence: %v", two)
	}
}

// Rule 9: an empty required chain is a typed render failure naming target/field/file
// and produces no live edit.
func TestEmptyChainNamesTargetFieldFile(t *testing.T) {
	d, s, target := allDisabledFixture()
	p, err := Build(d, s, target, nil)
	var empty EmptyChainError
	if !errors.As(err, &empty) {
		t.Fatalf("not EmptyChainError: %v", err)
	}
	if empty.TargetID != "t" {
		t.Fatalf("TargetID=%q", empty.TargetID)
	}
	if empty.Field != "defaults.full" {
		t.Fatalf("Field=%q", empty.Field)
	}
	if empty.File != configFile {
		t.Fatalf("File=%q want %q", empty.File, configFile)
	}
	if err.Error() == "" {
		t.Fatal("empty error message")
	}
	if len(p.Edits) != 0 {
		t.Fatalf("expected no edits, got %+v", p.Edits)
	}
}

// An empty definition chain also fails, naming the definition file.
func TestEmptyDefinitionChainFailsNamingFile(t *testing.T) {
	d, s, target := fixture("a/x")
	setMode(&s, "a", state.ModeDisabled)
	// Only the definition chain is populated; scalar chains are unmanaged (empty).
	p, err := Build(d, s, target, nil)
	var empty EmptyChainError
	if !errors.As(err, &empty) {
		t.Fatalf("not EmptyChainError: %v", err)
	}
	if empty.File != "agent.md" {
		t.Fatalf("File=%q want agent.md", empty.File)
	}
	if len(p.Edits) != 0 {
		t.Fatalf("expected no edits, got %+v", p.Edits)
	}
}

// A target with no managed chains at all still yields baseline enabled edits.
func TestEnabledEditsWithoutChains(t *testing.T) {
	d := policy.Desired{Version: 1, Providers: map[policy.MappingID]policy.Mapping{
		"codex": {
			CodexBarProviders:  []string{"codex"},
			PolytokenProviders: []string{"codex"},
			Models: map[string]policy.ModelBaseline{
				"codex/m": {Enabled: true, HadEnabledKey: false},
			},
		},
	}}
	target := policy.Target{ID: "t", Root: "/r"}
	p, err := Build(d, state.State{Revision: 1}, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetID != "t" || p.Revision != 1 {
		t.Fatalf("plan meta=%+v", p)
	}
	e := enabledEdit(t, p, "codex/m")
	if e.Enabled == nil || !*e.Enabled {
		t.Fatalf("baseline enabled not restored: %+v", e)
	}
}
