package selection

import (
	"errors"
	"fmt"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
	"testing"
	"time"
)

func TestSelectDeterministicGeneratedCases(t *testing.T) {
	refs := []string{"codex/gpt-5.6-sol(medium)", "zai/glm-5.2", "anthropic/claude-opus"}
	types := []struct {
		name string
		make func(string) *state.ProviderState
	}{{"missing", func(string) *state.ProviderState { return nil }}, {"fresh", func(mid string) *state.ProviderState {
		return selHealthyPS(selSnap(mid, selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("synthetic", 0.2, 1)), nil)
	}}, {"stale", func(mid string) *state.ProviderState {
		return selHealthyPS(selSnap(mid, selNow.Add(-2*time.Hour), quota.SourceFresh, quota.QuotaAvailable, selWin("synthetic", 0.2, 1)), nil)
	}}, {"stale-unavailable", func(mid string) *state.ProviderState {
		return selHealthyPS(selSnap(mid, selNow.Add(-2*time.Hour), quota.SourceFresh, quota.QuotaUnavailable, selWin("synthetic", 0.2, 1)), nil)
	}}, {"stale-zero", func(mid string) *state.ProviderState {
		return selHealthyPS(selSnap(mid, selNow.Add(-2*time.Hour), quota.SourcePartial, quota.QuotaAvailable, selWin("synthetic", 1, 1)), nil)
	}}, {"disabled", func(string) *state.ProviderState {
		return &state.ProviderState{Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true}
	}}}
	for rotation := range refs {
		for reverse := 0; reverse < 2; reverse++ {
			ordered := []string{refs[rotation], refs[(rotation+1)%len(refs)], refs[(rotation+2)%len(refs)]}
			if reverse == 1 {
				ordered[0], ordered[2] = ordered[2], ordered[0]
			}
			for a := range types {
				for b := range types {
					for c := range types {
						name := fmt.Sprintf("rot%d/rev%d/evidence%d-%d-%d", rotation, reverse, a, b, c)
						t.Run(name, func(t *testing.T) {
							groups := fmt.Sprintf("[[%s], [%s], [%s]]", ordered[0], ordered[1], ordered[2])
							p := selMustParse(t, fmt.Sprintf("version: 1\nphases: {generated: {routine: %s}}\n", groups))
							classes := []int{a, b, c}
							providers := map[string]state.ProviderState{}
							for i, ref := range ordered {
								base, _, err := policy.ParseModelRef(ref)
								if err != nil {
									t.Fatal(err)
								}
								mid := FamilyOf(base)
								if ps := types[classes[i]].make(mid); ps != nil {
									providers[mid] = *ps
								}
							}
							res, err := Select(p, selDesired(), selState(providers), selNow, "generated", TierRoutine, nil)
							want := generatedExpected(ordered, classes)
							if want == "" {
								if !errors.Is(err, ErrNoCandidate) {
									t.Fatalf("Select=%+v,%v want no candidate", res, err)
								}
								return
							}
							if err != nil {
								t.Fatalf("Select: %v", err)
							}
							if res.Reference != want {
								t.Fatalf("reference=%q want %q", res.Reference, want)
							}
							wantKind := KindUncertain
							if generatedConfirmed(want, classes[indexOfRef(ordered, want)]) {
								wantKind = KindConfirmed
							}
							if res.Kind != wantKind {
								t.Fatalf("kind=%q want %q", res.Kind, wantKind)
							}
							base, suffix, err := policy.ParseModelRef(want)
							if err != nil || res.Base != base || res.Suffix != suffix || res.Reference != base+"("+suffix+")" {
								if suffix == "" && err == nil && res.Reference == base {
								} else {
									t.Fatalf("decomposition=%q/%q/%q want %q", res.Reference, res.Base, res.Suffix, want)
								}
							}
						})
					}
				}
			}
		}
	}
}
func generatedConfirmed(ref string, class int) bool {
	return class == 1 && ref != "anthropic/claude-opus"
}
func generatedExpected(ordered []string, classes []int) string {
	for i, class := range classes {
		if generatedConfirmed(ordered[i], class) {
			return ordered[i]
		}
	}
	for i, class := range classes {
		if class == 0 || class == 1 || class == 2 {
			return ordered[i]
		}
	}
	return ""
}
func indexOfRef(refs []string, ref string) int {
	for i, candidate := range refs {
		if candidate == ref {
			return i
		}
	}
	return -1
}
func TestSelectGeneratedFamilyExclusions(t *testing.T) {
	refs := []string{"codex/gpt-5.6-sol(medium)", "zai/glm-5.2", "anthropic/claude-opus"}
	for excludedIndex, excluded := range []string{"codex", "zai", "anthropic"} {
		t.Run(excluded, func(t *testing.T) {
			groups := fmt.Sprintf("[[%s], [%s], [%s]]", refs[0], refs[1], refs[2])
			p := selMustParse(t, fmt.Sprintf("version: 1\nphases: {generated: {routine: %s}}\n", groups))
			providers := map[string]state.ProviderState{}
			for _, ref := range refs {
				mid := FamilyOf(ref)
				providers[mid] = *selHealthyPS(selSnap(mid, selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("synthetic", 0.1, 1)), nil)
			}
			res, err := Select(p, selDesired(), selState(providers), selNow, "generated", TierRoutine, []string{excluded})
			if err != nil {
				t.Fatal(err)
			}
			want := refs[0]
			if excludedIndex == 0 {
				want = refs[1]
			} else if excludedIndex == 1 {
				want = refs[0]
			}
			if res.Reference != want || res.Kind != KindConfirmed {
				t.Fatalf("excluding %q got %+v want %q", excluded, res, want)
			}
		})
	}
}
func TestSelectGeneratedGroupCompetition(t *testing.T) {
	refs := []string{"codex/gpt-5.6-sol(medium)", "zai/glm-5.2", "anthropic/claude-opus"}
	type evidence struct {
		name     string
		kind     Kind
		headroom float64
		excluded bool
	}
	classes := []evidence{{name: "missing", kind: KindUncertain}, {name: "low", kind: KindConfirmed, headroom: 0.2}, {name: "high", kind: KindConfirmed, headroom: 0.8}, {name: "disabled", excluded: true}, {name: "exhausted", excluded: true}}
	for reverse := 0; reverse < 2; reverse++ {
		ordered := append([]string(nil), refs...)
		if reverse == 1 {
			ordered[0], ordered[1] = ordered[1], ordered[0]
		}
		p := selMustParse(t, fmt.Sprintf("version: 1\nphases: {generated: {routine: [[%s, %s], [%s]]}}\n", ordered[0], ordered[1], ordered[2]))
		for _, first := range classes {
			for _, second := range classes {
				t.Run(fmt.Sprintf("reverse%d/%s/%s", reverse, first.name, second.name), func(t *testing.T) {
					labels := []evidence{first, second}
					providers := map[string]state.ProviderState{}
					for i, label := range labels {
						mid := FamilyOf(ordered[i])
						if label.name == "missing" {
							continue
						}
						ps := selHealthyPS(selSnap(mid, selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("synthetic", 1-label.headroom, 1)), nil)
						if label.name == "disabled" {
							ps.ManualDisabled = true
						}
						if label.name == "exhausted" {
							ps.Quota = state.QuotaExhausted
						}
						providers[mid] = *ps
					}
					res, err := Select(p, selDesired(), selState(providers), selNow, "generated", TierRoutine, nil)
					winner := -1
					for i, label := range labels {
						if label.kind == KindConfirmed && (winner < 0 || label.headroom > labels[winner].headroom) {
							winner = i
						}
					}
					if winner < 0 {
						for i, label := range labels {
							if !label.excluded {
								winner = i
								break
							}
						}
					}
					if winner < 0 {
						winner = 2
					}
					if err != nil || res.Reference != ordered[winner] {
						t.Fatalf("result=%+v err=%v want %q", res, err, ordered[winner])
					}
					if winner < 2 && labels[winner].kind == KindConfirmed {
						if res.Kind != KindConfirmed || res.Headroom == nil || !selClose(*res.Headroom, labels[winner].headroom) {
							t.Fatalf("result=%+v want headroom %v", res, labels[winner].headroom)
						}
					} else if res.Kind != KindUncertain || res.Headroom != nil {
						t.Fatalf("result=%+v want uncertain", res)
					}
					base, suffix, parseErr := policy.ParseModelRef(ordered[winner])
					if parseErr != nil || res.Base != base || res.Suffix != suffix {
						t.Fatalf("decomposition=%q/%q want %q/%q: %v", res.Base, res.Suffix, base, suffix, parseErr)
					}
				})
			}
		}
	}
}
func TestSelectGeneratedFloorNeverRescuesAbstention(t *testing.T) {
	for _, floor := range Tiers {
		t.Run(string(floor), func(t *testing.T) {
			env := newRunnerEnv(t, func(d *policy.Desired) { d.Selection.Jev.Enabled = true })
			env.assessor.abstain = true
			runner := &SelectRunner{Snapshot: env.snap, NewAssessor: env.newAssessor(), ReadPolicyFile: func(string) ([]byte, error) { return []byte(selPolicyDoc), nil }}
			out, err := runner.Run(t.Context(), SelectRequest{PolicyPath: "synthetic-policy", Phase: "execution", MinTier: floor, Prompt: "synthetic task"})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != SelectAssessmentUnavailable || !out.Abstained || out.Tier != "" || out.Result.Reference != "" {
				t.Fatalf("floor %q invented selection: %+v", floor, out)
			}
		})
	}
}
func TestSelectGeneratedPhaseTierIsolation(t *testing.T) {
	for i, tier := range Tiers {
		ref := "codex/gpt-5.6-sol"
		doc := fmt.Sprintf("version: 1\nphases: {p%d: {%s: [[%s]]}}\n", i, tier, ref)
		p := selMustParse(t, doc)
		res, err := Select(p, selDesired(), selState(nil), selNow, fmt.Sprintf("p%d", i), tier, nil)
		if err != nil || res.Reference != ref || res.Tier != string(tier) {
			t.Fatalf("tier %q result=%+v err=%v", tier, res, err)
		}
		for _, other := range Tiers {
			if other == tier {
				continue
			}
			if _, err := Select(p, selDesired(), selState(nil), selNow, fmt.Sprintf("p%d", i), other, nil); err == nil {
				t.Fatalf("tier %q borrowed policy for %q", other, tier)
			}
		}
	}
}
