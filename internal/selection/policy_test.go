package selection

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// selDesired builds the desired policy graph shared by the selection tests:
// three provider mappings, one baseline-disabled model, and an unpollable
// anthropic mapping without a quota configuration.
func selDesired() policy.Desired {
	return policy.Desired{
		Version: 1,
		Providers: map[policy.MappingID]policy.Mapping{
			"codex": {
				Models: map[string]policy.ModelBaseline{
					"codex/gpt-5.6-sol":  {Enabled: true},
					"codex/gpt-5.6-mini": {Enabled: true},
				},
				Quota: &policy.QuotaConfig{Adapter: "codex", FreshnessTTL: 30 * time.Minute},
			},
			"zai": {
				Models: map[string]policy.ModelBaseline{
					"zai/glm-5.2": {Enabled: true},
				},
				Quota: &policy.QuotaConfig{Adapter: "zai", FreshnessTTL: time.Hour},
			},
			"anthropic": {
				Models: map[string]policy.ModelBaseline{
					"anthropic/claude-opus":  {Enabled: true},
					"anthropic/claude-haiku": {Enabled: false},
				},
			},
		},
	}
}

// selPolicyDoc is a valid version-1 candidate policy exercising every tier and
// multiple phases, including a reasoning-suffixed reference.
const selPolicyDoc = `version: 1
phases:
  execution:
    routine:
      - - codex/gpt-5.6-sol
        - zai/glm-5.2
      - - anthropic/claude-opus
    normal:
      - - codex/gpt-5.6-sol(medium)
    difficult:
      - - zai/glm-5.2
    very_difficult:
      - - codex/gpt-5.6-sol
  planning:
    routine: [[anthropic/claude-opus]]
    normal: [[anthropic/claude-opus]]
    difficult: [[anthropic/claude-haiku]]
    very_difficult: [[codex/gpt-5.6-sol]]
`

// TestSelectionPolicyValidation pins the strict version-1 candidate policy
// grammar: exact tiers, ordered non-empty groups of exact resolved references,
// and rejection of every malformed shape — versions, unknown and duplicate
// keys, missing tiers, empty structure, unresolved or malformed references,
// duplicate references within a tier, trailing documents, oversize input, and
// empty documents.
func TestSelectionPolicyValidation(t *testing.T) {
	t.Run("valid multi-phase document preserves order", func(t *testing.T) {
		p, err := ParsePolicy([]byte(selPolicyDoc), selDesired())
		if err != nil {
			t.Fatalf("ParsePolicy(valid) error = %v", err)
		}
		if p.Version != PolicyVersion {
			t.Fatalf("Version = %d, want %d", p.Version, PolicyVersion)
		}
		if len(p.Phases) != 2 {
			t.Fatalf("phases = %d, want 2", len(p.Phases))
		}
		exec := p.Phases["execution"]
		if exec == nil {
			t.Fatal("execution phase missing")
		}
		routine := exec[TierRoutine]
		if len(routine.Groups) != 2 {
			t.Fatalf("routine groups = %d, want 2", len(routine.Groups))
		}
		wantFirst := Group{"codex/gpt-5.6-sol", "zai/glm-5.2"}
		if strings.Join(routine.Groups[0], ",") != strings.Join(wantFirst, ",") {
			t.Fatalf("routine group 0 = %v, want %v", routine.Groups[0], wantFirst)
		}
		if len(routine.Groups[1]) != 1 || routine.Groups[1][0] != "anthropic/claude-opus" {
			t.Fatalf("routine group 1 = %v, want [anthropic/claude-opus]", routine.Groups[1])
		}
		// Reasoning-suffixed references are stored verbatim.
		if got := exec[TierNormal].Groups[0][0]; got != "codex/gpt-5.6-sol(medium)" {
			t.Fatalf("normal candidate = %q, want verbatim suffix", got)
		}
		// Every tier is present for every phase, in order.
		for name, pp := range p.Phases {
			for i, tier := range Tiers {
				if _, ok := pp[tier]; !ok {
					t.Fatalf("phase %q missing tier %q", name, tier)
				}
				if TierIndex(tier) != i {
					t.Fatalf("Tiers[%d] = %q with index %d", i, tier, TierIndex(tier))
				}
			}
		}
	})

	t.Run("baseline-disabled models parse: enabled state is runtime, not parse-time", func(t *testing.T) {
		p, err := ParsePolicy([]byte(selPolicyDoc), selDesired())
		if err != nil {
			t.Fatalf("ParsePolicy error = %v", err)
		}
		if got := p.Phases["planning"][TierDifficult].Groups[0][0]; got != "anthropic/claude-haiku" {
			t.Fatalf("difficult candidate = %q, want anthropic/claude-haiku", got)
		}
	})

	t.Run("missing version", func(t *testing.T) {
		_, err := ParsePolicy([]byte("phases: {execution: {routine: [[codex/gpt-5.6-sol]]}}"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "unsupported or missing version") {
			t.Fatalf("error = %v, want unsupported or missing version", err)
		}
	})
	t.Run("unsupported version", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 2\nphases: {}"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "unsupported or missing version 2") {
			t.Fatalf("error = %v, want version 2 rejected", err)
		}
	})
	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 1\nextra: true\nphases: {}"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "field extra not found") {
			t.Fatalf("error = %v, want unknown field rejected", err)
		}
	})
	t.Run("unknown phase key", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 1\nphases: {execution: {surprise: [[codex/gpt-5.6-sol]]}}"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("error = %v, want unknown phase key rejected", err)
		}
	})
	t.Run("unknown tier key", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol]], impossible: [[codex/gpt-5.6-sol]]}}"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "field impossible not found") {
			t.Fatalf("error = %v, want unknown tier rejected", err)
		}
	})
	t.Run("phase may declare a subset of tiers; requesting an absent tier is a Select error", func(t *testing.T) {
		doc := "version: 1\nphases:\n  plan:\n    very_difficult: [[codex/gpt-5.6-sol]]\n  orchestrate:\n    normal: [[zai/glm-5.2]]\n"
		p, err := ParsePolicy([]byte(doc), selDesired())
		if err != nil {
			t.Fatalf("ParsePolicy(partial tiers) error = %v", err)
		}
		if _, ok := p.Phases["plan"][TierVeryDifficult]; !ok {
			t.Fatal("plan very_difficult missing")
		}
		if _, ok := p.Phases["plan"][TierRoutine]; ok {
			t.Fatal("plan routine should be absent")
		}
		// Selecting an absent tier errors rather than borrowing another tier.
		if _, err := Select(p, selDesired(), state.State{}, selNow, "plan", TierRoutine, nil); err == nil || !strings.Contains(err.Error(), `no "routine" tier`) {
			t.Fatalf("Select on absent tier err = %v, want no \"routine\" tier", err)
		}
	})
	t.Run("phase with no tiers at all", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 1\nphases: {execution: {}}\n"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "defines no tiers") {
			t.Fatalf("error = %v, want empty phase rejected", err)
		}
	})
	t.Run("empty phases", func(t *testing.T) {
		_, err := ParsePolicy([]byte("version: 1\nphases: {}\n"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "defines no phases") {
			t.Fatalf("error = %v, want empty structure rejected", err)
		}
	})
	t.Run("tier with no groups", func(t *testing.T) {
		doc := "version: 1\nphases:\n  execution:\n    routine: []\n    normal: []\n    difficult: []\n    very_difficult: []\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "at least one candidate group") {
			t.Fatalf("error = %v, want empty tier rejected", err)
		}
	})
	t.Run("empty group", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "group 0 is empty") {
			t.Fatalf("error = %v, want empty group rejected", err)
		}
	})
	t.Run("malformed reference grammar", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol(low]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "malformed reasoning suffix") {
			t.Fatalf("error = %v, want malformed reference rejected", err)
		}
	})
	t.Run("empty reference", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[\"\"]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "empty model reference") {
			t.Fatalf("error = %v, want empty reference rejected", err)
		}
	})
	t.Run("empty reference inside a mixed group", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, \"\"]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "empty model reference") {
			t.Fatalf("error = %v, want missing ref entry rejected", err)
		}
	})
	t.Run("unresolved reference", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/nope]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "unresolved model") {
			t.Fatalf("error = %v, want unresolved reference rejected", err)
		}
	})
	t.Run("glob reference is not an exact reference", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/*]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "unresolved model") {
			t.Fatalf("error = %v, want glob rejected as unresolved", err)
		}
	})
	t.Run("duplicate reference within one group", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), `duplicate candidate "codex/gpt-5.6-sol"`) {
			t.Fatalf("error = %v, want duplicate within tier rejected", err)
		}
	})
	t.Run("duplicate reference across groups of one tier", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol], [zai/glm-5.2, codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "groups 0 and 1") {
			t.Fatalf("error = %v, want duplicate across groups rejected", err)
		}
	})
	t.Run("same reference in different tiers is allowed", func(t *testing.T) {
		p, err := ParsePolicy([]byte(selPolicyDoc), selDesired())
		if err != nil {
			t.Fatalf("ParsePolicy error = %v", err)
		}
		if got := p.Phases["execution"][TierVeryDifficult].Groups[0][0]; got != "codex/gpt-5.6-sol" {
			t.Fatalf("very_difficult candidate = %q", got)
		}
	})
	t.Run("duplicate phase key", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol]]}}\nphases: {planning: {routine: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), `key "phases" already defined`) {
			t.Fatalf("error = %v, want duplicate key rejected", err)
		}
	})
	t.Run("duplicate tier key", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol]], routine: [[zai/glm-5.2]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), `key "routine" already defined`) {
			t.Fatalf("error = %v, want duplicate tier key rejected", err)
		}
	})
	t.Run("trailing document", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n---\nversion: 1\nphases: {}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "trailing document") {
			t.Fatalf("error = %v, want trailing document rejected", err)
		}
	})
	t.Run("oversize document", func(t *testing.T) {
		big := bytes.Repeat([]byte("#"), MaxPolicyBytes+1)
		_, err := ParsePolicy(big, selDesired())
		if err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("error = %v, want oversize rejected", err)
		}
	})
	t.Run("document at the byte limit parses", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		padded := doc + strings.Repeat("#", MaxPolicyBytes-len(doc))
		if len(padded) != MaxPolicyBytes {
			t.Fatalf("test setup: padded doc is %d bytes", len(padded))
		}
		if _, err := ParsePolicy([]byte(padded), selDesired()); err != nil {
			t.Fatalf("ParsePolicy(at-limit) error = %v", err)
		}
	})
	t.Run("empty document", func(t *testing.T) {
		_, err := ParsePolicy(nil, selDesired())
		if err == nil || !strings.Contains(err.Error(), "empty policy document") {
			t.Fatalf("error = %v, want empty document rejected", err)
		}
	})
	t.Run("comment-only document", func(t *testing.T) {
		_, err := ParsePolicy([]byte("# just a comment\n"), selDesired())
		if err == nil || !strings.Contains(err.Error(), "empty policy document") {
			t.Fatalf("error = %v, want comment-only document rejected", err)
		}
	})
	t.Run("empty phase name", func(t *testing.T) {
		doc := "version: 1\nphases: {\"\": {routine: [[codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		_, err := ParsePolicy([]byte(doc), selDesired())
		if err == nil || !strings.Contains(err.Error(), "phase name must not be empty") {
			t.Fatalf("error = %v, want empty phase name rejected", err)
		}
	})
	t.Run("tier helpers", func(t *testing.T) {
		for i, tier := range Tiers {
			if !ValidTier(tier) {
				t.Fatalf("ValidTier(%q) = false", tier)
			}
			if TierIndex(tier) != i {
				t.Fatalf("TierIndex(%q) = %d, want %d", tier, TierIndex(tier), i)
			}
		}
		if ValidTier(Tier("Routine")) || ValidTier(Tier("hard")) || ValidTier(Tier("")) {
			t.Fatal("ValidTier accepted an unknown tier")
		}
		if TierIndex(Tier("hard")) != -1 {
			t.Fatal("TierIndex(unknown) != -1")
		}
	})
	t.Run("FamilyOf strips suffix and takes the prefix before the slash", func(t *testing.T) {
		for ref, want := range map[string]string{
			"codex/gpt-5.6-sol(medium)": "codex",
			"codex/gpt-5.6-sol":         "codex",
			"zai/glm-5.2":               "zai",
			"noslash":                   "noslash",
		} {
			if got := FamilyOf(ref); got != want {
				t.Fatalf("FamilyOf(%q) = %q, want %q", ref, got, want)
			}
		}
	})
}
