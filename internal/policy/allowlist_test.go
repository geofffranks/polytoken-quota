package policy

// Direct table test pinning the documented path-shape guarantees of the
// staging read-allowlist predicates (review round nit #1): the predicate's
// edge cases are guaranteed by its doc comment, so they are pinned here
// explicitly rather than only through walk behavior.

import "testing"

func TestInStagingAllowlistPathShapes(t *testing.T) {
	admitted := []string{
		"config.yaml",            // the root config document
		"facets/x.md",            // top-level tree file
		"facets/a/b.md",          // nested tree file
		"subagents/y.md",         // the other tree
		"subagents/p/q/r.md",     // deep nesting
		"subagents/polytoken.md", // benign polytoken-named definition
		"facets/.md",             // literal *.md suffix; polytoken's stem rule rejects it at load, not the allowlist
	}
	for _, rel := range admitted {
		if !InStagingAllowlist(rel) {
			t.Errorf("InStagingAllowlist(%q) = false; want true", rel)
		}
	}
	rejected := []string{
		"",                           // empty
		"config.yaml.bak",            // backup of the config document
		"nested/config.yaml",         // config.yaml is allowlisted at the root only
		"facets",                     // the tree itself is not a file path
		"facets/",                    // empty leaf
		"facets/x.txt",               // not *.md
		"facets/x.md.bak",            // backup inside the tree
		"facets/x.MD",                // case-sensitive *.md (matches polytoken discovery)
		"subagents/credentials.json", // non-definition payload
		"facets/../x.md",             // escape attempt
		"./facets/x.md",              // dot segment
		"facets//x.md",               // empty segment
		"skills/x.md",                // foreign tree
		"agents/research.md",         // legacy foreign tree
		"subagentsfoo/x.md",          // prefix confusion
		"watchdog.env",               // the real incident file
	}
	for _, rel := range rejected {
		if InStagingAllowlist(rel) {
			t.Errorf("InStagingAllowlist(%q) = true; want false", rel)
		}
	}
}

func TestInStagingAllowlistTreePathShapes(t *testing.T) {
	inside := []string{"facets", "subagents", "facets/a", "subagents/a/b"}
	for _, rel := range inside {
		if !InStagingAllowlistTree(rel) {
			t.Errorf("InStagingAllowlistTree(%q) = false; want true", rel)
		}
	}
	outside := []string{"", ".", "skills", "config.yaml", "facets-extra", "subagentsfoo", "read-once"}
	for _, rel := range outside {
		if InStagingAllowlistTree(rel) {
			t.Errorf("InStagingAllowlistTree(%q) = true; want false", rel)
		}
	}
}
