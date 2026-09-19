package selection

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

var selNow = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func selF(v float64) *float64 { return &v }

func selTime(t time.Time) *time.Time { return &t }

func selWin(name string, used, limit float64) quota.QuotaWindow {
	return quota.QuotaWindow{Name: name, Used: selF(used), Limit: selF(limit)}
}

func selSnap(mid string, checkedAt time.Time, status quota.SourceStatus, avail quota.QuotaAvailability, windows ...quota.QuotaWindow) *quota.QuotaSnapshot {
	return &quota.QuotaSnapshot{
		MappingID:    mid,
		CheckedAt:    checkedAt,
		Status:       status,
		Availability: avail,
		Windows:      windows,
		Error:        "bearer [redacted] account=[redacted]",
	}
}

// selPS builds a provider state with explicit durable axes and optional
// snapshot/attempt observations.
func selPS(q state.Quota, a state.Availability, snap, attempt *quota.QuotaSnapshot) *state.ProviderState {
	return &state.ProviderState{Quota: q, Availability: a, QuotaSnapshot: snap, QuotaAttempt: attempt}
}

// selHealthyPS is a tracked provider in the healthy normal mode.
func selHealthyPS(snap, attempt *quota.QuotaSnapshot) *state.ProviderState {
	return selPS(state.QuotaNormal, state.Available, snap, attempt)
}

func selState(providers map[string]state.ProviderState) state.State {
	return state.State{Providers: providers}
}

func selMustParse(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(doc), selDesired())
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return p
}

func selRun(t *testing.T, p Policy, st state.State, phase string, tier Tier, excluded ...string) (Result, error) {
	t.Helper()
	return Select(p, selDesired(), st, selNow, phase, tier, excluded)
}

// selSingleDoc is a one-candidate-per-tier policy over the healthy codex
// mapping, used by the evidence matrix.
const selSingleDoc = `version: 1
phases:
  execution:
    routine: [[codex/gpt-5.6-sol]]
    normal: [[codex/gpt-5.6-sol]]
    difficult: [[codex/gpt-5.6-sol]]
    very_difficult: [[codex/gpt-5.6-sol]]
`

// selAnthropicDoc targets the unpollable anthropic mapping (no quota config).
const selAnthropicDoc = `version: 1
phases:
  execution:
    routine: [[anthropic/claude-opus]]
    normal: [[anthropic/claude-opus]]
    difficult: [[anthropic/claude-opus]]
    very_difficult: [[anthropic/claude-opus]]
`

// selClose reports whether two remaining fractions agree to well below the
// precision that matters for selection ordering.
func selClose(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestSelectCandidatePolicy pins the candidate-ordering and exclusion
// semantics of Select: group preference before headroom, maximum headroom with
// stable ties inside a group, all confirmed groups before any uncertain
// fallback, fail-closed exclusions, and argument validation.
func TestSelectCandidatePolicy(t *testing.T) {
	fresh := func(mid string, rem float64) *quota.QuotaSnapshot {
		return selSnap(mid, selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 1-rem, 1))
	}

	t.Run("group preference beats headroom", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol], [zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(fresh("codex", 0.05), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.90), nil),
		})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol" || res.Kind != KindConfirmed {
			t.Fatalf("got %s/%s, want preferred group-0 codex confirmed", res.Kind, res.Reference)
		}
		if res.Headroom == nil || !selClose(*res.Headroom, 0.05) {
			t.Fatalf("headroom = %v, want 0.05", res.Headroom)
		}
	})

	t.Run("maximum headroom wins within the first confirmed group", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(fresh("codex", 0.20), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.90), nil),
		})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "zai/glm-5.2" || res.Headroom == nil || !selClose(*res.Headroom, 0.90) {
			t.Fatalf("got %s headroom %v, want zai 0.90", res.Reference, res.Headroom)
		}
	})

	t.Run("headroom ties resolve stably to policy order", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(fresh("codex", 0.50), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.50), nil),
		})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol" {
			t.Fatalf("tie resolved to %q, want the earlier candidate", res.Reference)
		}
	})

	t.Run("confirmed candidates in later groups beat uncertain earlier groups", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol], [zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"zai": *selHealthyPS(fresh("zai", 0.80), nil),
			// codex absent from state entirely: missing evidence, uncertain.
		})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Kind != KindConfirmed || res.Reference != "zai/glm-5.2" {
			t.Fatalf("got %s/%s, want confirmed zai over uncertain codex", res.Kind, res.Reference)
		}
	})

	t.Run("uncertain fallback takes the first member of the first uncertain group", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		res, err := selRun(t, p, selState(nil), "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Kind != KindUncertain || res.Reference != "codex/gpt-5.6-sol" || res.Evidence != EvidenceMissing {
			t.Fatalf("got %s/%s evidence %q, want uncertain codex missing", res.Kind, res.Reference, res.Evidence)
		}
	})

	t.Run("excluded candidates never win the uncertain fallback", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[anthropic/claude-haiku, codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		res, err := selRun(t, p, selState(nil), "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol" || res.Evidence != EvidenceMissing {
			t.Fatalf("got %s/%q, want baseline-disabled haiku skipped", res.Reference, res.Evidence)
		}
	})

	t.Run("sole baseline-disabled candidate yields ErrNoCandidate", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[anthropic/claude-haiku]], normal: [[anthropic/claude-haiku]], difficult: [[anthropic/claude-haiku]], very_difficult: [[anthropic/claude-haiku]]}}\n"
		p := selMustParse(t, doc)
		_, err := selRun(t, p, selState(nil), "execution", TierRoutine)
		if !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("manual disable excludes even from the uncertain fallback", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
		})
		if _, err := selRun(t, p, st, "execution", TierRoutine); !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("manual disable skips to the next candidate", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol, zai/glm-5.2]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.QuotaNormal, Availability: state.Available, ManualDisabled: true},
			"zai":   *selHealthyPS(fresh("zai", 0.60), nil),
		})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "zai/glm-5.2" || res.Kind != KindConfirmed {
			t.Fatalf("got %s/%s, want confirmed zai", res.Kind, res.Reference)
		}
	})

	t.Run("known unavailable provider state excludes", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.QuotaNormal, Availability: state.Unavailable},
		})
		if _, err := selRun(t, p, st, "execution", TierRoutine); !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("known exhausted provider state excludes", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.QuotaExhausted, Availability: state.Available},
		})
		if _, err := selRun(t, p, st, "execution", TierRoutine); !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("corrupt quota enum never enables a candidate", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.Quota("banana"), Availability: state.Available},
		})
		if _, err := selRun(t, p, st, "execution", TierRoutine); !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("corrupt availability enum never enables a candidate", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		st := selState(map[string]state.ProviderState{
			"codex": {Quota: state.QuotaNormal, Availability: state.Availability("meh")},
		})
		if _, err := selRun(t, p, st, "execution", TierRoutine); !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("family exclusion removes every member of the family", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[zai/glm-5.2, codex/gpt-5.6-sol]], normal: [[codex/gpt-5.6-sol]], difficult: [[codex/gpt-5.6-sol]], very_difficult: [[codex/gpt-5.6-sol]]}}\n"
		p := selMustParse(t, doc)
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(fresh("codex", 0.70), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.95), nil),
		})
		res, err := selRun(t, p, st, "execution", TierRoutine, "zai")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol" {
			t.Fatalf("got %q, want zai family excluded", res.Reference)
		}
	})

	t.Run("family exclusion with sole family member yields ErrNoCandidate", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		_, err := selRun(t, p, selState(nil), "execution", TierRoutine, "codex")
		if !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("err = %v, want ErrNoCandidate", err)
		}
	})

	t.Run("unknown phase is an error", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		_, err := selRun(t, p, selState(nil), "review", TierRoutine)
		if err == nil || !strings.Contains(err.Error(), `unknown phase "review"`) {
			t.Fatalf("err = %v, want unknown phase", err)
		}
	})

	t.Run("unknown tier is an error", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		_, err := selRun(t, p, selState(nil), "execution", Tier("hard"))
		if err == nil || !strings.Contains(err.Error(), `unknown tier "hard"`) {
			t.Fatalf("err = %v, want unknown tier", err)
		}
	})

	t.Run("hand-built policy missing a tier is an error", func(t *testing.T) {
		p := Policy{Version: 1, Phases: map[string]PhasePolicy{"execution": {}}}
		_, err := selRun(t, p, selState(nil), "execution", TierRoutine)
		if err == nil || !strings.Contains(err.Error(), `no "routine" tier`) {
			t.Fatalf("err = %v, want missing tier", err)
		}
	})

	t.Run("empty family names in the exclusion list are ignored", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		res, err := selRun(t, p, selState(nil), "execution", TierRoutine, "")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol" {
			t.Fatalf("got %q, want empty exclusion ignored", res.Reference)
		}
	})
}

// TestSelectQuotaEvidenceMatrix pins the evidence classification of every
// provider-observation shape: which candidates are confirmed, which are
// uncertain and why, and which are excluded outright — including the rule
// that known-bad snapshot evidence (reported unavailability, non-positive
// usable headroom, corrupted enums or windows) excludes before any stale,
// partial, or no-TTL demotion, the conservative handling of zero and future
// timestamps and sparse state axes, and last-good preservation across failed
// refreshes.
func TestSelectQuotaEvidenceMatrix(t *testing.T) {
	fresh := func(mut func(*quota.QuotaSnapshot)) *quota.QuotaSnapshot {
		s := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1))
		if mut != nil {
			mut(s)
		}
		return s
	}
	stale := func(mut func(*quota.QuotaSnapshot)) *quota.QuotaSnapshot {
		s := selSnap("codex", selNow.Add(-31*time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1))
		if mut != nil {
			mut(s)
		}
		return s
	}
	failedAttempt := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFailed, quota.QuotaUnknown)
	zero := func(s *quota.QuotaSnapshot) { s.Windows = []quota.QuotaWindow{selWin("weekly", 1, 1)} }

	cases := []struct {
		name         string
		doc          string
		mapping      string
		ps           *state.ProviderState
		wantKind     Kind
		wantEvidence string
		wantErr      error
		wantHeadroom float64
	}{
		// -- absent and unobserved providers: missing --
		{
			name:         "provider absent from state: missing",
			ps:           nil,
			wantKind:     KindUncertain,
			wantEvidence: EvidenceMissing,
		},
		{
			name:         "tracked but never observed: missing",
			ps:           &state.ProviderState{},
			wantKind:     KindUncertain,
			wantEvidence: EvidenceMissing,
		},

		// -- failed refreshes --
		{
			name:         "attempt without a usable snapshot: failed",
			ps:           selPS(state.QuotaNormal, state.Available, nil, failedAttempt),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceFailed,
		},
		{
			name:         "fresh failed source: failed",
			ps:           selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Status = quota.SourceFailed; s.Availability = quota.QuotaUnknown }), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceFailed,
		},
		{
			name:         "fresh last good survives a failed refresh: confirmed",
			ps:           selHealthyPS(fresh(nil), failedAttempt),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.75,
		},

		// -- freshness --
		{
			name:         "stale snapshot: stale",
			ps:           selHealthyPS(stale(nil), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceStale,
		},
		{
			name:         "stale snapshot with a newer failed attempt stays stale",
			ps:           selHealthyPS(stale(nil), failedAttempt),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceStale,
		},
		{
			name:         "fresh snapshot at the exact TTL boundary is confirmed",
			ps:           selHealthyPS(selSnap("codex", selNow.Add(-30*time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1)), nil),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.75,
		},
		{
			name:         "snapshot one nanosecond past TTL is stale",
			ps:           selHealthyPS(selSnap("codex", selNow.Add(-30*time.Minute-time.Nanosecond), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1)), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceStale,
		},

		// -- completeness and explicit availability --
		{
			name:         "fresh partial source: partial",
			ps:           selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Status = quota.SourcePartial }), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidencePartial,
		},
		{
			name: "fresh complete snapshot with no usable window: sparse",
			ps: selHealthyPS(fresh(func(s *quota.QuotaSnapshot) {
				s.Windows = []quota.QuotaWindow{{Name: "weekly", ResetAt: selTime(selNow.Add(24 * time.Hour))}}
			}), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceSparse,
		},
		{
			name:         "fresh snapshot with unknown availability: unknown",
			ps:           selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Availability = quota.QuotaUnknown }), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},
		{
			name:         "fresh snapshot with empty availability: unknown",
			ps:           selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Availability = "" }), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},
		{
			name:         "fresh snapshot with empty source status: unknown",
			ps:           selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Status = "" }), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},

		// -- mappings without a configured TTL never confirm, but known-bad
		// snapshot evidence still excludes first --
		{
			name:         "mapping without quota configuration never confirms",
			doc:          selAnthropicDoc,
			mapping:      "anthropic",
			ps:           selHealthyPS(selSnap("anthropic", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("monthly", 0.1, 1)), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},
		{
			name:     "no TTL mapping with reported unavailability excludes",
			doc:      selAnthropicDoc,
			mapping:  "anthropic",
			ps:       selHealthyPS(selSnap("anthropic", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaUnavailable, selWin("monthly", 0.1, 1)), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "no TTL mapping with zero remaining excludes",
			doc:      selAnthropicDoc,
			mapping:  "anthropic",
			ps:       selHealthyPS(selSnap("anthropic", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("monthly", 1, 1)), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},

		// -- known-bad snapshot evidence excludes before stale demotion --
		{
			name:     "fresh reported unavailability excludes outright",
			ps:       selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Availability = quota.QuotaUnavailable }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "stale snapshot with reported unavailability excludes",
			ps:       selHealthyPS(stale(func(s *quota.QuotaSnapshot) { s.Availability = quota.QuotaUnavailable }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "stale snapshot with zero remaining excludes",
			ps:       selHealthyPS(stale(zero), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "stale snapshot with an invalid source status excludes",
			ps:       selHealthyPS(stale(func(s *quota.QuotaSnapshot) { s.Status = quota.SourceStatus("banana") }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "fresh snapshot with an invalid availability enum excludes",
			ps:       selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Availability = quota.QuotaAvailability("meh") }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "fresh snapshot with an invalid source status excludes",
			ps:       selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Status = quota.SourceStatus("banana") }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name: "corrupt window with negative usage percent excludes",
			ps: selHealthyPS(fresh(func(s *quota.QuotaSnapshot) {
				s.Windows = []quota.QuotaWindow{{Name: "weekly", UsagePercent: selF(-10)}}
			}), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name: "corrupt window with negative used excludes",
			ps: selHealthyPS(fresh(func(s *quota.QuotaSnapshot) {
				s.Windows = []quota.QuotaWindow{{Name: "weekly", Used: selF(-1), Limit: selF(1)}}
			}), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},
		{
			name:     "partial source with zero remaining excludes",
			ps:       selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Status = quota.SourcePartial; zero(s) }), nil),
			wantKind: KindUncertain,
			wantErr:  ErrNoCandidate,
		},

		// -- conservative timestamps and sparse durable axes --
		{
			name:         "future snapshot is never confirmed",
			ps:           selHealthyPS(selSnap("codex", selNow.Add(time.Hour), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1)), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},
		{
			name:         "zero CheckedAt is never confirmed",
			ps:           selHealthyPS(selSnap("codex", time.Time{}, quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1)), nil),
			wantKind:     KindUncertain,
			wantEvidence: EvidenceUnknown,
		},
		{
			name:         "sparse provider axes with a fresh complete snapshot confirm",
			ps:           selPS("", "", fresh(nil), nil),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.75,
		},

		// -- headroom arithmetic --
		{
			name:         "non-positive remaining quota excludes outright",
			ps:           selHealthyPS(fresh(zero), nil),
			wantKind:     KindUncertain,
			wantEvidence: "",
			wantErr:      ErrNoCandidate,
		},
		{
			name: "overdrawn quota clamps to zero and excludes",
			ps: func() *state.ProviderState {
				ps := selHealthyPS(fresh(func(s *quota.QuotaSnapshot) { s.Windows = []quota.QuotaWindow{selWin("weekly", 2, 1)} }), nil)
				return ps
			}(),
			wantKind:     KindUncertain,
			wantEvidence: "",
			wantErr:      ErrNoCandidate,
		},
		{
			name: "headroom is the minimum across windows",
			ps: selHealthyPS(fresh(func(s *quota.QuotaSnapshot) {
				s.Windows = []quota.QuotaWindow{selWin("five_hour", 0.05, 1), selWin("weekly", 0.80, 1)}
			}), nil),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.20,
		},
		{
			name: "usage_percent backs the headroom when used/limit is absent",
			ps: selHealthyPS(fresh(func(s *quota.QuotaSnapshot) {
				s.Windows = []quota.QuotaWindow{{Name: "weekly", UsagePercent: selF(60)}}
			}), nil),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.40,
		},
		{
			name:         "reserve mode (low quota, available) confirms",
			ps:           selPS(state.QuotaLow, state.Available, fresh(nil), nil),
			wantKind:     KindConfirmed,
			wantHeadroom: 0.75,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.doc
			if doc == "" {
				doc = selSingleDoc
			}
			mid := tc.mapping
			if mid == "" {
				mid = "codex"
			}
			p := selMustParse(t, doc)
			st := selState(nil)
			if tc.ps != nil {
				st = selState(map[string]state.ProviderState{mid: *tc.ps})
			}
			res, err := selRun(t, p, st, "execution", TierRoutine)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if res.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", res.Kind, tc.wantKind)
			}
			if res.Evidence != tc.wantEvidence {
				t.Fatalf("evidence = %q, want %q", res.Evidence, tc.wantEvidence)
			}
			if tc.wantKind == KindConfirmed {
				if res.Headroom == nil || !selClose(*res.Headroom, tc.wantHeadroom) {
					t.Fatalf("headroom = %v, want %v", res.Headroom, tc.wantHeadroom)
				}
			} else if res.Headroom != nil {
				t.Fatalf("uncertain headroom = %v, want nil", *res.Headroom)
			}
		})
	}
}

// TestSelectCorruptWindowNonFiniteExcludes proves corruptWindow rejects NaN
// and ±Inf in every numeric pointer field — Used, Limit and UsagePercent —
// not just out-of-range finite values. NaN defeats ordered comparisons, so a
// range-only check lets it slip through Remaining()'s clamp into a NaN
// headroom; rem <= 0 is false for NaN, so the candidate would "confirm" with
// a headroom that no JSON document can represent (encoding/json refuses
// NaN/Inf). Every field/value pair must exclude outright, and a finite
// control must still confirm with JSON-safe headroom.
func TestSelectCorruptWindowNonFiniteExcludes(t *testing.T) {
	fields := []struct {
		name string
		set  func(w *quota.QuotaWindow, v float64)
	}{
		{"Used", func(w *quota.QuotaWindow, v float64) {
			w.Used = selF(v)
			if w.Limit == nil {
				w.Limit = selF(1)
			}
		}},
		{"Limit", func(w *quota.QuotaWindow, v float64) {
			w.Limit = selF(v)
			if w.Used == nil {
				w.Used = selF(0.25)
			}
		}},
		{"UsagePercent", func(w *quota.QuotaWindow, v float64) { w.UsagePercent = selF(v) }},
	}
	values := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(+1)},
		{"-Inf", math.Inf(-1)},
	}
	p := selMustParse(t, selSingleDoc)
	for _, field := range fields {
		for _, val := range values {
			t.Run(field.name+"/"+val.name, func(t *testing.T) {
				w := quota.QuotaWindow{Name: "weekly"}
				field.set(&w, val.value)
				snap := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, w)
				st := selState(map[string]state.ProviderState{"codex": *selHealthyPS(snap, nil)})
				_, err := selRun(t, p, st, "execution", TierRoutine)
				if !errors.Is(err, ErrNoCandidate) {
					t.Fatalf("window with %s=%v must exclude the candidate outright, got err=%v", field.name, val.value, err)
				}
			})
		}
	}

	// JSON safety control: a fully finite window still confirms and its
	// Result marshals cleanly. Exclusion above is the only guard needed,
	// because a non-finite window can never become a headroom.
	t.Run("finite control confirms with JSON-safe headroom", func(t *testing.T) {
		snap := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1))
		st := selState(map[string]state.ProviderState{"codex": *selHealthyPS(snap, nil)})
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Kind != KindConfirmed || res.Headroom == nil || !selClose(*res.Headroom, 0.75) {
			t.Fatalf("control result = %+v, want confirmed with 0.75 headroom", res)
		}
		if _, err := json.Marshal(res); err != nil {
			t.Fatalf("json.Marshal(confirmed result): %v", err)
		}
	})
}

// TestSelectPreservesReference pins that the selected reference keeps its
// exact spelling — reasoning suffix included — through the Result and its
// version-1 JSON form, and that the JSON carries only sanitized fields.
func TestSelectPreservesReference(t *testing.T) {
	snap := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1))
	st := selState(map[string]state.ProviderState{"codex": *selHealthyPS(snap, nil)})

	t.Run("suffixed reference is preserved verbatim", func(t *testing.T) {
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol(medium)]], normal: [[codex/gpt-5.6-sol(medium)]], difficult: [[codex/gpt-5.6-sol(medium)]], very_difficult: [[codex/gpt-5.6-sol(medium)]]}}\n"
		p := selMustParse(t, doc)
		res, err := selRun(t, p, st, "execution", TierNormal)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Reference != "codex/gpt-5.6-sol(medium)" {
			t.Fatalf("reference = %q, want verbatim suffix preserved", res.Reference)
		}
		if res.Base != "codex/gpt-5.6-sol" || res.Suffix != "medium" {
			t.Fatalf("base/suffix = %q/%q, want codex/gpt-5.6-sol/medium", res.Base, res.Suffix)
		}
		if res.Mapping != "codex" {
			t.Fatalf("mapping = %q, want codex", res.Mapping)
		}
	})

	t.Run("bare reference has no suffix", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Suffix != "" || res.Base != res.Reference {
			t.Fatalf("suffix = %q base = %q, want empty suffix equal to reference", res.Suffix, res.Base)
		}
	})

	t.Run("confirmed result marshals to stable version-1 JSON", func(t *testing.T) {
		p := selMustParse(t, selSingleDoc)
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Version != SelectionVersion || SelectionVersion != 1 {
			t.Fatalf("version = %d, want 1", res.Version)
		}
		want := `{"version":1,"phase":"execution","tier":"routine","reference":"codex/gpt-5.6-sol","base":"codex/gpt-5.6-sol","mapping":"codex","kind":"confirmed","headroom":0.75}`
		got, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != want {
			t.Fatalf("json =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("uncertain result marshals with evidence and omits headroom", func(t *testing.T) {
		staleState := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(selSnap("codex", selNow.Add(-time.Hour), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1)), nil),
		})
		doc := "version: 1\nphases: {execution: {routine: [[codex/gpt-5.6-sol(medium)]], normal: [[codex/gpt-5.6-sol(medium)]], difficult: [[codex/gpt-5.6-sol(medium)]], very_difficult: [[codex/gpt-5.6-sol(medium)]]}}\n"
		p := selMustParse(t, doc)
		res, err := selRun(t, p, staleState, "execution", TierNormal)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		want := `{"version":1,"phase":"execution","tier":"normal","reference":"codex/gpt-5.6-sol(medium)","base":"codex/gpt-5.6-sol","suffix":"medium","mapping":"codex","kind":"uncertain","evidence":"stale"}`
		got, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != want {
			t.Fatalf("json =\n%s\nwant\n%s", got, want)
		}
		var back Result
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(res, back) {
			t.Fatalf("round trip = %+v, want %+v", back, res)
		}
	})

	t.Run("result JSON never carries provider observation internals", func(t *testing.T) {
		tainted := selSnap("codex", selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.25, 1))
		tainted.Error = "SECRET-MARKER bearer abc123 account=7"
		st := selState(map[string]state.ProviderState{"codex": *selHealthyPS(tainted, nil)})
		p := selMustParse(t, selSingleDoc)
		res, err := selRun(t, p, st, "execution", TierRoutine)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		got, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(got), "SECRET-MARKER") {
			t.Fatalf("json leaked observation internals: %s", got)
		}
	})
}
