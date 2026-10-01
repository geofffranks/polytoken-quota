package groupsafety

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// layeredGlobal mirrors the contract fixture feasLayeredGlobalConfig: provider
// gp owns both global failover leaves, provider pp owns the tier defaults.
func layeredGlobal() Layer {
	return Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    kind:
      type: custom_open_ai_compatible
    url: http://127.0.0.1:9
    auth:
      type: no_auth
    enabled: true
  pp:
    kind:
      type: custom_open_ai_compatible
    url: http://127.0.0.1:9
    auth:
      type: no_auth
    enabled: true
models:
  gp/g1:
    provider: gp
    provider_name: g1
    class: full
    context_window: 200000
    enabled: true
  gp/g3:
    provider: gp
    provider_name: g3
    class: full
    context_window: 200000
    enabled: true
  pp/p2:
    provider: pp
    provider_name: p2
    class: full
    context_window: 200000
    enabled: true
  pp/p4:
    provider: pp
    provider_name: p4
    class: mini
    context_window: 200000
    enabled: true
modelgroups:
  failover:
    - gp/g1
    - gp/g3
  polytoken:default_model_full: pp/p2
  polytoken:default_model_mini: pp/p4
`)}
}

// layeredProject mirrors the contract fixture feasLayeredProjectConfig: the
// first project leaf duplicates the global head leaf and the second is the
// project-only provider's model.
func layeredProject() Layer {
	return Layer{ID: "proj", Config: []byte(`version: 4
modelgroups:
  failover:
    - gp/g1
    - pp/p2
`)}
}

// enrolled covers every provider ID used by the unit fixtures; "absent" is
// deliberately enrolled but appears in no fixture config.
var enrolled = []string{"gp", "pp", "stub", "alt", "on", "off", "solo", "gone", "absent"}

// analyze runs the analyzer over the given layers: the first global layer is
// the global layer, the rest are registered project layers in order.
func analyze(disable string, layers ...Layer) Report {
	in := Input{Enrolled: enrolled}
	for _, l := range layers {
		if l.Global {
			in.Global = l
			continue
		}
		if l.ID == "" {
			l.ID = "proj"
		}
		in.Projects = append(in.Projects, l)
	}
	return Analyze(in, disable)
}

// equal reports got == want for string slices with a useful diff.
func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func wantVerdict(t *testing.T, r Report, v Verdict) {
	t.Helper()
	if r.Verdict != v {
		t.Fatalf("verdict = %q (reasons %v), want %q", r.Verdict, r.Reasons, v)
	}
}

func wantReason(t *testing.T, r Report, substring string) {
	t.Helper()
	for _, reason := range r.Reasons {
		if strings.Contains(reason, substring) {
			return
		}
	}
	t.Fatalf("no reason mentions %q; reasons: %v", substring, r.Reasons)
}

// TestGroupEligibilityGlobalProjectConcatenates proves the analyzer resolves
// same-name groups the way the binary's published catalog composes them:
// global leaves first, project leaves appended, duplicate positions preserved.
// Disabling the provider owning BOTH global leaves via
// providers.<id>.enabled: false keeps the project-only leaf, so the analysis
// is safe and the effective post-disable group is exactly the project-only
// leaf — matching the pinned routing snapshot and local-stub turn (the binary
// comparison itself is the opt-in contract test).
func TestProjectGroupCannotMaskAnotherProjectsEmptyGroup(t *testing.T) {
	global := layeredGlobal()
	empty := Layer{ID: "empty", Config: []byte("version: 4\nmodelgroups:\n  failover: []\n")}
	live := Layer{ID: "live", Config: []byte("version: 4\nmodelgroups:\n  failover:\n    - pp/p2\n")}
	in := Input{Enrolled: enrolled, Global: global, Projects: []Layer{empty, live}}
	r := Analyze(in, "gp")
	wantVerdict(t, r, Unsafe)
	wantReason(t, r, "group \"failover\"")

	// A project facet pin to a group is resolved only against that project's
	// effective root; it cannot borrow a same-name group from another project.
	in.Projects[0].Config = []byte("version: 4\nmodelgroups:\n  outer: []\n")
	in.Projects[0].Definitions = []Definition{{Path: "facets/pinned.md", Model: "mg:outer"}}
	in.Projects[1].Config = []byte("version: 4\nmodelgroups:\n  outer:\n    - pp/p2\n")
	in.Projects[1].Definitions = nil
	r = Analyze(in, "gp")
	wantVerdict(t, r, Unsafe)
	wantReason(t, r, "already unavailable")
}

func TestGroupEligibilityGlobalProjectConcatenates(t *testing.T) {
	r := analyze("gp", layeredGlobal(), layeredProject())
	wantVerdict(t, r, Safe)
	if len(r.Reasons) != 0 {
		t.Fatalf("safe analysis carried evidence: %v", r.Reasons)
	}
	// All-enabled: global [gp/g1 gp/g3] then project [gp/g1 pp/p2], duplicate
	// gp/g1 preserved at both concatenated positions.
	equal(t, "GroupsBefore[failover]", r.GroupsBefore["failover"],
		[]string{"gp/g1", "gp/g3", "gp/g1", "pp/p2"})
	// Post-disable: every gp leaf drops — including the duplicate
	// project-position copy — leaving exactly the project-only leaf.
	equal(t, "GroupsAfter[failover]", r.GroupsAfter["failover"], []string{"pp/p2"})
	// The derived reserved group aliases the full-tier default.
	equal(t, "GroupsBefore[polytoken:general-purpose]", r.GroupsBefore[reservedGroupFull], []string{"pp/p2"})
	equal(t, "GroupsAfter[polytoken:general-purpose]", r.GroupsAfter[reservedGroupFull], []string{"pp/p2"})
}

// TestGroupEligibilityNestedAndCycle proves explicit mg: references resolve
// recursively (a disable empties a referencing group through its nested
// leaves) and that reference cycles fail closed as pending-unknown.
func TestGroupEligibilityNestedAndCycle(t *testing.T) {
	t.Run("nested reference resolves and filters", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  alt:
    url: http://127.0.0.1:9
    enabled: true
  gone:
    url: http://127.0.0.1:9
    enabled: true
models:
  gone/only:
    provider: gone
    enabled: true
  alt/keep:
    provider: alt
    enabled: true
modelgroups:
  inner:
    - alt/keep
  outer:
    - gone/only
    - mg:inner
  polytoken:default_model_full: mg:outer
`)}
		r := analyze("gone", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsBefore[outer]", r.GroupsBefore["outer"], []string{"gone/only", "alt/keep"})
		equal(t, "GroupsAfter[outer]", r.GroupsAfter["outer"], []string{"alt/keep"})
		equal(t, "GroupsAfter[default_full_via_nested_ref]", r.GroupsAfter[tierDefaultFull], []string{"alt/keep"})
	})

	t.Run("two-step cycle is pending-unknown", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
modelgroups:
  g1:
    - mg:g2
  g2:
    - mg:g1
  polytoken:default_model_full: mg:g1
`)}
		wantVerdict(t, analyze("stub", global), PendingUnknown)
	})

	t.Run("self cycle is pending-unknown", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
modelgroups:
  g:
    - stub/m1
    - mg:g
  polytoken:default_model_full: mg:g
`)}
		r := analyze("stub", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "cycle")
	})
}

// TestGroupEligibilityAllLeavesUnavailable proves the binary-proven rule: a
// proposed disable that leaves any configured group without an available leaf
// is unsafe, and a required tier default without a model is unsafe with the
// tier-specific evidence. A group that is already empty before the proposal
// is a currently-invalid root, refused as pending-unknown.
func TestGroupEligibilityAllLeavesUnavailable(t *testing.T) {
	t.Run("tier default emptied by the disable is unsafe", func(t *testing.T) {
		r := analyze("pp", layeredGlobal(), layeredProject())
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, "tier default")
		// The tier default empties (pp/p2 was its only leaf); the failover
		// group keeps the global gp leaves, so it is the tier evidence that
		// makes this unsafe.
		equal(t, "GroupsAfter[failover]", r.GroupsAfter["failover"], []string{"gp/g1", "gp/g3", "gp/g1"})
		equal(t, "GroupsAfter[default_full]", r.GroupsAfter[tierDefaultFull], []string{})
	})

	t.Run("ordinary group emptied by the disable is unsafe", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  solo:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  solo/x1:
    provider: solo
    enabled: true
modelgroups:
  solo-group:
    - solo/x1
  polytoken:default_model_full: stub/m1
`)}
		r := analyze("solo", global)
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, "solo-group")
	})

	t.Run("already-empty group is a pending-unknown invalid root", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
  pp/p4:
    provider: pp
    enabled: false
modelgroups:
  dead:
    - pp/p4
  polytoken:default_model_full: pp/p2
`)}
		// The proposal does not touch the already-empty group; its
		// pre-existing emptiness still refuses the analysis.
		r := analyze("stub", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "already has no available leaf")
	})
}

// TestGroupEligibilityDynamicPending proves fail-closed handling of dynamic
// and unresolved composition: leaves referencing models no registered layer
// defines, models without attributable providers, unknown group references,
// and undefined facet pins are all pending-unknown and never authorize a
// write — even when the rest of the graph looks usable.
func TestGroupEligibilityDynamicPending(t *testing.T) {
	t.Run("ghost leaf in the affected group is pending-unknown", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  pp/ok:
    provider: pp
    enabled: true
modelgroups:
  mix:
    - gp/ghost
    - pp/ok
  polytoken:default_model_full: pp/ok
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "gp/ghost")
	})

	t.Run("ghost leaf in an unaffected group still fails closed", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  affected:
    - gp/g1
    - pp/p2
  unrelated:
    - ghost/nope
    - pp/p2
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "ghost/nope")
	})

	t.Run("name-only model that is the sole leaf is unsafe to disable", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    enabled: true
modelgroups:
  g:
    - gp/g1
  polytoken:default_model_full: gp/g1
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, "leaves group \"g\" without")
	})

	t.Run("model attributing to an unknown provider is refused", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: missing-provider
    enabled: true
modelgroups:
  g:
    - gp/g1
  polytoken:default_model_full: gp/g1
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "has no providers entry")
	})

	t.Run("mg reference to an undefined group is pending-unknown", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  g:
    - mg:nowhere
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "mg:nowhere")
	})
}

// TestModelProviderInference proves the user's contract: a valid model entry
// needs no provider field — the provider is the text before the first slash of
// the model name, in the observed <provider>/<model>(<effort>) shape. An
// explicit provider field keeps precedence over the name prefix, malformed
// names fail closed, and the effort suffix never affects ownership.
func TestModelProviderInference(t *testing.T) {
	t.Run("name-only mapping attributes to the name's provider", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    enabled: true
  pp/p2:
    enabled: true
modelgroups:
  failover:
    - gp/g1
    - pp/p2
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsAfter[failover]", r.GroupsAfter["failover"], []string{"pp/p2"})
	})

	t.Run("effort suffix does not affect provider inference", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1(high):
    enabled: true
  pp/p2:
    enabled: true
modelgroups:
  failover:
    - gp/g1(high)
    - pp/p2
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsAfter[failover]", r.GroupsAfter["failover"], []string{"pp/p2"})
	})

	t.Run("explicit provider field takes precedence over the name prefix", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: pp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  g:
    - gp/g1
  polytoken:default_model_full: pp/p2
`)}
		// gp/g1 names a gp prefix but explicitly attributes to pp, so
		// disabling gp leaves every consumer with a usable route.
		r := analyze("gp", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsAfter[g]", r.GroupsAfter["g"], []string{"gp/g1"})
	})

	t.Run("inferred provider without a providers entry fails closed", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    enabled: true
models:
  ghost/x:
    enabled: true
modelgroups:
  g:
    - ghost/x
  polytoken:default_model_full: ghost/x
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "has no providers entry")
	})

	t.Run("malformed names fail closed", func(t *testing.T) {
		for _, name := range []string{"noslash", "/g1", "gp/"} {
			global := Layer{ID: "global", Global: true, Config: []byte(fmt.Sprintf(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
models:
  %s:
    enabled: true
modelgroups:
  g:
    - %s
  polytoken:default_model_full: g
`, name, name))}
			r := analyze("gp", global)
			wantVerdict(t, r, PendingUnknown)
			wantReason(t, r, "models."+name)
		}
	})
}

// TestGroupEligibilityShippedDefaults proves tier-default handling: observed
// configured defaults are enforced, the observed derived reserved group names
// resolve as aliases, the unobserved nano reserved name and unknown shipped
// references fail closed, and an absent tier default imposes no invented
// shipped-default requirement (the contract fixtures omit the nano default
// and reload successfully).
func TestGroupEligibilityShippedDefaults(t *testing.T) {
	t.Run("mini tier default enforced", func(t *testing.T) {
		r := analyze("pp", layeredGlobal(), layeredProject())
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, tierDefaultMini)
	})

	t.Run("absent nano default imposes no requirement", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  alt:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  alt/a1:
    provider: alt
    enabled: true
modelgroups:
  failover:
    - stub/m1
    - alt/a1
  polytoken:default_model_full: alt/a1
`)}
		wantVerdict(t, analyze("stub", global), Safe)
	})

	t.Run("facet pin to the full reserved group resolves as alias", func(t *testing.T) {
		global := layeredGlobal()
		global.Definitions = []Definition{{Path: "subagents/pinned.md", Model: reservedGroupFull}}
		wantVerdict(t, analyze("gp", global, layeredProject()), Safe)
	})

	t.Run("unobserved nano reserved name is pending-unknown", func(t *testing.T) {
		global := layeredGlobal()
		global.Definitions = []Definition{{Path: "subagents/nano.md", Model: "polytoken:general-purpose-nano"}}
		r := analyze("gp", global, layeredProject())
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "polytoken:general-purpose-nano")
	})

	t.Run("operator-defined reserved-namespace group is pending-unknown", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  pp:
    url: http://127.0.0.1:9
    enabled: true
  alt:
    url: http://127.0.0.1:9
    enabled: true
models:
  pp/p2:
    provider: pp
    enabled: true
  alt/a1:
    provider: alt
    enabled: true
modelgroups:
  polytoken:default_model_full: alt/a1
  polytoken:custom:
    - pp/p2
    - alt/a1
`)}
		r := analyze("pp", global)
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "reserved polytoken: namespace")
	})
}

// TestAnalyzerStrictSchemaAndVersion proves the strict layer grammar: the
// version key is not read and any config version is accepted, duplicate keys
// anywhere are ambiguous and refused, project layers may carry modelgroups
// only, and malformed read fields fail closed as pending-unknown.
func TestAnalyzerStrictSchemaAndVersion(t *testing.T) {
	valid := `version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  g:
    - gp/g1
    - pp/p2
  polytoken:default_model_full: pp/p2
`
	cases := []struct {
		name   string
		global string
		proj   string
		twoPro bool
		reason string
	}{
		{
			name: "duplicate group key",
			global: strings.Replace(valid, "modelgroups:\n  g:\n    - gp/g1\n",
				"modelgroups:\n  g:\n    - gp/g1\n  g:\n    - gp/g1\n", 1),
			reason: "duplicate key",
		},
		{
			name:   "duplicate top-level key",
			global: valid + "providers: {}\n",
			reason: "duplicate key",
		},
		{
			name:   "project layer sets providers",
			global: valid,
			proj:   "version: 4\nproviders:\n  x: {}\n",
			reason: "sets providers",
		},
		{
			name:   "project layer sets models",
			global: valid,
			proj:   "version: 4\nmodels: {}\n",
			reason: "sets models",
		},
		{
			name:   "project layer sets defaults",
			global: valid,
			proj:   "version: 4\ndefaults:\n  full: gp/g1\n",
			reason: "sets defaults",
		},
		{name: "model without provider and unparseable name",
			global: strings.Replace(strings.Replace(valid, "  gp/g1:\n    provider: gp\n    enabled: true", "  noslash:\n    enabled: true", 1), "    - gp/g1\n", "    - noslash\n", 1),
			reason: "not <provider>/<model>"},
		{name: "group leaf not a string", global: strings.Replace(valid, "    - gp/g1\n", "    - 7\n", 1), reason: "non-empty strings"},
		{name: "group valued as mapping", global: strings.Replace(valid, "  g:\n    - gp/g1\n    - pp/p2\n", "  g:\n    x: 1\n", 1), reason: "leaf sequence"},
		{name: "not a mapping", global: "- a\n- b\n", reason: "not a mapping"},
		{name: "empty config", global: "", reason: "empty"},
		{name: "unparseable", global: "version: 4\n  bad: [", reason: "does not parse"},
		{name: "defaults key in v4", global: valid + "defaults:\n  full: gp/g1\n", reason: "defaults key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Enrolled: []string{"gp"}, Global: Layer{ID: "global", Global: true, Config: []byte(tc.global)}}
			if tc.proj != "" {
				if tc.twoPro {
					in.Projects = []Layer{{ID: "p1", Config: []byte(tc.proj)}, {ID: "p2", Config: []byte(tc.proj)}}
				} else {
					in.Projects = []Layer{{ID: "p1", Config: []byte(tc.proj)}}
				}
			}
			r := Analyze(in, "gp")
			wantVerdict(t, r, PendingUnknown)
			wantReason(t, r, tc.reason)
		})
	}

	t.Run("any config version is accepted", func(t *testing.T) {
		baseline := Analyze(Input{Enrolled: []string{"gp"}, Global: Layer{ID: "global", Global: true, Config: []byte(valid)}}, "gp")
		for _, variant := range []string{
			strings.Replace(valid, "version: 4", "version: 5", 1),
			strings.Replace(valid, "version: 4", `version: "4"`, 1),
			strings.Replace(valid, "version: 4\n", "", 1),
		} {
			r := Analyze(Input{Enrolled: []string{"gp"}, Global: Layer{ID: "global", Global: true, Config: []byte(variant)}}, "gp")
			wantVerdict(t, r, baseline.Verdict)
			if len(r.GroupsAfter) != len(baseline.GroupsAfter) {
				t.Fatalf("GroupsAfter=%v want %v", r.GroupsAfter, baseline.GroupsAfter)
			}
			for name, leaves := range baseline.GroupsAfter {
				equal(t, "GroupsAfter["+name+"]", r.GroupsAfter[name], leaves)
			}
		}
	})

	t.Run("non-bool provider enabled flag is refused", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: "yes"
models:
  gp/g1:
    provider: gp
    enabled: true
modelgroups:
  g:
    - gp/g1
  polytoken:default_model_full: gp/g1
`)}
		r := Analyze(Input{Enrolled: []string{"gp"}, Global: global}, "gp")
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "must be a boolean")
	})

	t.Run("proposal outside the enrolled set is refused", func(t *testing.T) {
		r := Analyze(Input{Enrolled: []string{"gp"}, Global: Layer{ID: "global", Global: true, Config: []byte(valid)}}, "unenrolled")
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "not enrolled")
	})

	t.Run("empty proposal is refused", func(t *testing.T) {
		r := Analyze(Input{Enrolled: []string{"gp"}, Global: Layer{ID: "global", Global: true, Config: []byte(valid)}}, "")
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "names no provider")
	})

	t.Run("missing global layer is refused", func(t *testing.T) {
		r := Analyze(Input{Enrolled: []string{"gp"}}, "gp")
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "no global layer")
	})
}

// TestAnalyzerDefinitionReferences proves facet/subagent reference checking in
// registered roots: a primary reference that would die is unsafe, an
// already-dead or unresolvable reference is pending-unknown, group-shaped
// pins resolve through the effective graph, and fallback loss is
// pending-unknown because fallback semantics were never pinned.
func TestAnalyzerDefinitionReferences(t *testing.T) {
	base := `version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
  pp/p4:
    provider: pp
    enabled: false
modelgroups:
  failover:
    - gp/g1
    - pp/p2
  solo:
    - pp/p2
    - pp/p4
  polytoken:default_model_full: pp/p2
`
	defs := func(d ...Definition) Layer {
		return Layer{ID: "global", Global: true, Config: []byte(base), Definitions: d}
	}

	t.Run("concrete pin on the disabled provider is unsafe", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "facets/pinned.md", Model: "gp/g1"}))
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, "kills the primary reference")
	})

	t.Run("concrete pin on a surviving provider is safe", func(t *testing.T) {
		wantVerdict(t, analyze("gp", defs(Definition{Path: "facets/pinned.md", Model: "pp/p2"})), Safe)
	})

	t.Run("group pin survives while the group keeps a leaf", func(t *testing.T) {
		wantVerdict(t, analyze("gp", defs(Definition{Path: "facets/pinned.md", Model: "mg:failover"})), Safe)
	})

	t.Run("group pin dies when the group empties", func(t *testing.T) {
		r := analyze("pp", defs(Definition{Path: "facets/pinned.md", Model: "mg:solo"}))
		wantVerdict(t, r, Unsafe)
		wantReason(t, r, "kills the primary reference")
		wantReason(t, r, "mg:solo")
	})

	t.Run("undefined pin model is pending-unknown", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "facets/pinned.md", Model: "ghost/nope"}))
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "ghost/nope")
	})

	t.Run("already-dead pin model is pending-unknown", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "subagents/dead.md", Model: "pp/p4"}))
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "already unavailable")
	})

	t.Run("partial fallback loss is pending-unknown", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "facets/fb.md", Model: "pp/p2", Fallbacks: []string{"gp/g1", "pp/p2"}}))
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "drop 1 of 2 fallback")
	})

	t.Run("all fallbacks dead is pending-unknown", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "facets/fb.md", Model: "pp/p2", Fallbacks: []string{"gp/g1"}}))
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "no usable fallback")
	})

	t.Run("undefined fallback is pending-unknown", func(t *testing.T) {
		r := analyze("gp", defs(Definition{Path: "facets/fb.md", Model: "pp/p2", Fallbacks: []string{"ghost/nope"}}))
		wantVerdict(t, r, PendingUnknown)
		wantReason(t, r, "fallback")
	})
}

// TestAnalyzerModelingSemantics pins the remaining graph-modeling rules:
// currently-disabled providers are filtered from the effective graph before
// any proposal, duplicate positions are routing behavior preserved verbatim,
// a no-change proposal stays safe, reasons are sanitized, and repeated
// analysis is deterministic.
func TestAnalyzerModelingSemantics(t *testing.T) {
	t.Run("currently disabled provider is filtered from the graph", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  on:
    url: http://127.0.0.1:9
    enabled: true
  off:
    url: http://127.0.0.1:9
    enabled: false
models:
  on/m1:
    provider: on
    enabled: true
  off/x1:
    provider: off
    enabled: true
modelgroups:
  g:
    - on/m1
    - off/x1
  polytoken:default_model_full: on/m1
`)}
		// Disabling the already-disabled provider changes nothing.
		r := analyze("off", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsBefore[g]", r.GroupsBefore["g"], []string{"on/m1"})
		equal(t, "GroupsAfter[g]", r.GroupsAfter["g"], []string{"on/m1"})
	})

	t.Run("duplicate positions are preserved routing behavior", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  dup:
    - gp/g1
    - gp/g1
    - pp/p2
  polytoken:default_model_full: gp/g1
`)}
		r := analyze("pp", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsBefore[dup]", r.GroupsBefore["dup"], []string{"gp/g1", "gp/g1", "pp/p2"})
		equal(t, "GroupsAfter[dup]", r.GroupsAfter["dup"], []string{"gp/g1", "gp/g1"})
	})

	t.Run("enrolled provider absent from the config has no modeled effect", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("absent", global)
		wantVerdict(t, r, Safe)
		equal(t, "GroupsAfter[default_full]", r.GroupsAfter[tierDefaultFull], []string{"pp/p2"})
	})

	t.Run("reasons never carry unrelated config values and are deterministic", func(t *testing.T) {
		global := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  gp:
    url: http://127.0.0.1:9/endpoint
    auth:
      type: static_key
      key: synthetic-secret-do-not-leak
    enabled: true
  pp:
    url: http://127.0.0.1:9
    enabled: true
models:
  gp/g1:
    provider: gp
    enabled: true
  pp/p2:
    provider: pp
    enabled: true
modelgroups:
  g:
    - gp/g1
    - pp/p2
    - ghost/nope
  polytoken:default_model_full: pp/p2
`)}
		r := analyze("gp", global)
		wantVerdict(t, r, PendingUnknown)
		for _, reason := range r.Reasons {
			if strings.Contains(reason, "http") || strings.Contains(reason, "secret") || strings.Contains(reason, "auth") {
				t.Fatalf("reason leaks config values: %q", reason)
			}
		}
		again := analyze("gp", global)
		if !reflect.DeepEqual(r.Reasons, again.Reasons) || r.Verdict != again.Verdict {
			t.Fatalf("analysis not deterministic: %v vs %v", r.Reasons, again.Reasons)
		}
	})
}

// TestGroupEligibilityGrouplessConfigIsPendingUnknown proves the fail-closed
// classification for registered roots that configure no modelgroups at all:
// with zero configured groups there is nothing to resolve, so the only
// remaining routes are the binary's shipped defaults and the dynamic catalog,
// whose composition the offline analyzer has never observed. Such a shape
// must never classify a disable Safe (COMP-1).
func TestGroupEligibilityGrouplessConfigIsPendingUnknown(t *testing.T) {
	groupless := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  alt:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  alt/a1:
    provider: alt
    enabled: true
`)}
	r := analyze("stub", groupless)
	wantVerdict(t, r, PendingUnknown)
	wantReason(t, r, "no modelgroups are configured")

	// The guard keys on configured groups, not on tier-default presence: the
	// same config with one configured group stays on the proven Safe path
	// (mirrors the absent-nano grouped fixture, which the binary reloads).
	grouped := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  alt:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  alt/a1:
    provider: alt
    enabled: true
modelgroups:
  polytoken:default_model_full: alt/a1
`)}
	wantVerdict(t, analyze("stub", grouped), Safe)

	// A project layer's groups also satisfy the guard for the combined-root
	// analysis: the composed root has configured groups to reason about.
	projected := Layer{ID: "proj", Config: []byte("version: 4\nmodelgroups:\n  failover: [alt/a1]\n")}
	wantVerdict(t, analyze("stub", groupless, projected), Safe)
}

// TestGroupEligibilityHostileReferenceGraphBounded proves the analyzer bounds
// reference resolution (ADV-1): a chain of groups each referencing the next
// twice would expand 2^n leaf walks and 2^n-element lists without the
// memoized, budgeted resolver. The analysis must return promptly with a
// pending-unknown verdict instead of hanging the reconcile under the mutation
// lock.
func TestGroupEligibilityHostileReferenceGraphBounded(t *testing.T) {
	const depth = 40
	var b strings.Builder
	b.WriteString("version: 4\nproviders:\n  stub:\n    url: http://127.0.0.1:9\n    enabled: true\nmodels:\n  stub/m1:\n    provider: stub\n    enabled: true\nmodelgroups:\n")
	for i := depth - 1; i >= 0; i-- {
		if i == depth-1 {
			b.WriteString(fmt.Sprintf("  g%02d: [stub/m1]\n", i))
			continue
		}
		b.WriteString(fmt.Sprintf("  g%02d: [mg:g%02d, mg:g%02d]\n", i, i+1, i+1))
	}
	global := Layer{ID: "global", Global: true, Config: []byte(b.String())}
	r := analyze("stub", global)
	wantVerdict(t, r, PendingUnknown)
	wantReason(t, r, "analysis budget")

	// A healthy deeply-referencing graph stays fully resolvable within the
	// budget: duplicates and order are preserved through memoized
	// sub-resolutions, and the disable verdict matches the resolved graph.
	healthy := Layer{ID: "global", Global: true, Config: []byte(`version: 4
providers:
  stub:
    url: http://127.0.0.1:9
    enabled: true
  alt:
    url: http://127.0.0.1:9
    enabled: true
models:
  stub/m1:
    provider: stub
    enabled: true
  alt/a1:
    provider: alt
    enabled: true
modelgroups:
  base: [stub/m1, alt/a1]
  mid: [mg:base, mg:base]
  top: [mg:mid, alt/a1]
`)}
	hr := analyze("stub", healthy)
	wantVerdict(t, hr, Safe)
	want := []string{"stub/m1", "alt/a1", "stub/m1", "alt/a1", "alt/a1"}
	if !reflect.DeepEqual(hr.GroupsBefore["top"], want) {
		t.Fatalf("healthy deep graph leaves = %v, want %v", hr.GroupsBefore["top"], want)
	}
	if wantAfter := []string{"alt/a1", "alt/a1", "alt/a1"}; !reflect.DeepEqual(hr.GroupsAfter["top"], wantAfter) {
		t.Fatalf("healthy deep graph post-disable leaves = %v, want %v", hr.GroupsAfter["top"], wantAfter)
	}
}
