package service

// Per-target preparation result and pure projection helpers that convert
// reconcile plans, ranking decisions, and staging hash comparisons into
// state-owned history DTOs. These are the single implementation shared by both
// verbose reconcile output and durable history templates; verbose rendering is
// a thin view over these projections.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/reconcile"
	"github.com/geofffranks/polytoken-quota/internal/routing"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// PrepareResult captures the per-target preparation outcome after rendering,
// staging, and hash comparison but before publication. It is the pure data that
// history qualification, verbose rendering, and publication all consume.
type PrepareResult struct {
	TargetID     string
	PlanComputed bool
	Plan         reconcile.Plan
	// ChangedFiles maps policy-relative file paths whose staged SHA-256 differs
	// from the live file. An empty map means no proven change.
	ChangedFiles map[string]bool
	// ChangedEdits is the subset of Plan.Edits whose containing file is in
	// ChangedFiles.
	ChangedEdits []reconcile.FieldEdit
	// Replacements is the filtered set of publication replacements with
	// equal-hash entries removed.
	Replacements []publishReplacement
}

// publishReplacement is a minimal view of a publish replacement for
// preparation testing without pulling the publish package into a pure
// preparation test.
type publishReplacement struct {
	LivePath string
	TempPath string
	OldHash  [32]byte
	NewHash  [32]byte
}

// HasProvenChange reports whether at least one managed file has a proven
// old/new byte difference.
func (pr PrepareResult) HasProvenChange() bool {
	for _, changed := range pr.ChangedFiles {
		if changed {
			return true
		}
	}
	return false
}

// ProjectProviders converts the policy/state decision context into state-owned
// ProviderDetail values, using the same logic as verbose trace rendering.
func ProjectProviders(desired policy.Desired, observed state.State) []state.ProviderDetail {
	ids := make([]string, 0, len(desired.Providers))
	for id := range desired.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	out := make([]state.ProviderDetail, 0, len(ids))
	for _, idStr := range ids {
		id := policy.MappingID(idStr)
		m := desired.Providers[id]
		mode := reconcile.MappingMode(desired, observed, id)
		reason := providerModeReason(idStr, m, mode, observed)
		out = append(out, state.ProviderDetail{
			MappingID: idStr,
			Mode:      mode,
			Reason:    reason,
		})
	}
	return out
}

// providerModeReason produces a short, sanitized explanation for a mapping's
// effective mode.
func providerModeReason(id string, m policy.Mapping, mode state.Mode, observed state.State) string {
	switch mode {
	case state.ModeDisabled:
		if ps, ok := observed.Providers[id]; ok {
			if ps.ManualDisabled {
				return "manually disabled"
			}
			if ps.Availability == state.Unavailable {
				return "provider unavailable"
			}
			if ps.Quota == state.QuotaExhausted {
				return "quota exhausted"
			}
		}
		return "disabled"
	case state.ModeReserve:
		return "quota low (reserve)"
	default:
		return "healthy"
	}
}

// ProjectRanks converts routing ranking entries into state-owned RankDetail
// values.
func ProjectRanks(ranking routing.RankingResult) []state.RankDetail {
	out := make([]state.RankDetail, 0, len(ranking.Entries))
	for _, e := range ranking.Entries {
		out = append(out, state.RankDetail{
			MappingID:   e.MappingID,
			Rank:        e.Rank,
			OffPeak:     e.OffPeak,
			Eligible:    e.Eligible,
			Explanation: e.Explanation,
		})
	}
	return out
}

// ProjectChains converts the desired/effective ordering for every managed
// chain on a target into state-owned ChainDetail values.
func ProjectChains(desired policy.Desired, observed state.State, target policy.Target, ranks reconcile.RankLookup) []state.ChainDetail {
	var out []state.ChainDetail
	chains := []struct {
		name  string
		chain policy.Chain
	}{
		{"full", target.Full},
		{"mini", target.Mini},
		{"nano", target.Nano},
		{"classifier", target.Classifier},
	}
	for _, ch := range chains {
		if len(ch.chain) == 0 {
			continue
		}
		out = append(out, projectChain(desired, observed, ch.name, ch.chain, ranks))
	}
	for _, def := range target.Definitions {
		if len(def.Chain) == 0 {
			continue
		}
		out = append(out, projectChain(desired, observed, def.Path, def.Chain, ranks))
	}
	return out
}

func projectChain(desired policy.Desired, observed state.State, name string, chain policy.Chain, ranks reconcile.RankLookup) state.ChainDetail {
	effective, err := reconcile.EffectiveOrder(desired, observed, chain, ranks)
	if err != nil {
		effective = []string{}
	}
	effSet := make(map[string]bool, len(effective))
	for _, e := range effective {
		effSet[e] = true
	}
	var dropped []string
	for _, entry := range chain {
		if !effSet[entry] {
			dropped = append(dropped, entry)
		}
	}
	return state.ChainDetail{
		Name:      name,
		Desired:   append([]string(nil), chain...),
		Effective: effective,
		Dropped:   dropped,
	}
}

// ProjectEdits converts reconcile field edits into state-owned EditDetail
// values. Only edits whose file is in changedFiles are included; this is the
// change-qualified projection used by both verbose rendering and history.
func ProjectEdits(edits []reconcile.FieldEdit, changedFiles map[string]bool) []state.EditDetail {
	out := make([]state.EditDetail, 0, len(edits))
	for _, fe := range edits {
		if changedFiles != nil && !changedFiles[fe.File] {
			continue
		}
		out = append(out, fieldEditToDetail(fe))
	}
	return out
}

func fieldEditToDetail(fe reconcile.FieldEdit) state.EditDetail {
	d := state.EditDetail{File: fe.File, Path: fe.Path}
	switch {
	case fe.Remove:
		d.Action = state.EditRemove
		d.Detail = "removed"
	case fe.Scalar != nil:
		d.Action = state.EditSetScalar
		d.Detail = *fe.Scalar
	case len(fe.Sequence) > 0:
		d.Action = state.EditSetSequence
		d.Detail = fmt.Sprintf("%v", fe.Sequence)
	case fe.Enabled != nil:
		d.Action = state.EditSetBool
		if *fe.Enabled {
			d.Detail = "true"
		} else {
			d.Detail = "false"
		}
	}
	return d
}

// HasProvenChangeAcrossTargets reports whether any of the given preparation
// results has at least one proven managed-file byte difference.
func HasProvenChangeAcrossTargets(results []PrepareResult) bool {
	for _, pr := range results {
		if pr.HasProvenChange() {
			return true
		}
	}
	return false
}

// BuildPrepareResult computes a PrepareResult from a successfully rendered plan
// by comparing staged candidate file hashes against corresponding live files.
// ChangedFiles maps each policy-relative file path whose staged SHA-256 differs
// from the live file (or whose live file does not exist). ChangedEdits is the
// subset of Plan.Edits whose containing file is in ChangedFiles. Replacements
// are built from the same hashes with equal-hash entries already dropped.
func BuildPrepareResult(targetID string, plan reconcile.Plan, liveRoot, stagedDir string) (PrepareResult, error) {
	changedFiles := make(map[string]bool)
	var replacements []publishReplacement
	seen := map[string]bool{}
	for _, fe := range plan.Edits {
		if seen[fe.File] {
			continue
		}
		seen[fe.File] = true
		livePath := filepath.Join(liveRoot, filepath.FromSlash(fe.File))
		tempPath := filepath.Join(stagedDir, filepath.FromSlash(fe.File))
		old, new, err := fileSHA256Pair(livePath, tempPath)
		if err != nil {
			return PrepareResult{}, err
		}
		if old != new {
			changedFiles[fe.File] = true
		}
		replacements = append(replacements, publishReplacement{
			LivePath: livePath,
			TempPath: tempPath,
			OldHash:  old,
			NewHash:  new,
		})
	}
	return PrepareResult{
		TargetID:     targetID,
		PlanComputed: true,
		Plan:         plan,
		ChangedFiles: changedFiles,
		ChangedEdits: filterEditsByChangedFiles(plan.Edits, changedFiles),
		Replacements: DropEqualHashReplacements(replacements),
	}, nil
}

// fileSHA256Pair returns the SHA-256 of the live file (or zero digest when it
// does not exist) and the SHA-256 of the staged temp file.
func fileSHA256Pair(livePath, tempPath string) (old, new [32]byte, err error) {
	tempData, err := os.ReadFile(tempPath)
	if err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("read staged file: %w", err)
	}
	new = sha256.Sum256(tempData)
	liveData, err := os.ReadFile(livePath)
	if err != nil {
		if os.IsNotExist(err) {
			return [32]byte{}, new, nil // first publish: zero old hash
		}
		return [32]byte{}, [32]byte{}, fmt.Errorf("read live file: %w", err)
	}
	old = sha256.Sum256(liveData)
	return old, new, nil
}

// DropEqualHashReplacements removes replacements whose OldHash equals NewHash.
func DropEqualHashReplacements(replacements []publishReplacement) []publishReplacement {
	out := make([]publishReplacement, 0, len(replacements))
	for _, r := range replacements {
		if r.OldHash != r.NewHash {
			out = append(out, r)
		}
	}
	return out
}

func filterEditsByChangedFiles(edits []reconcile.FieldEdit, changedFiles map[string]bool) []reconcile.FieldEdit {
	out := make([]reconcile.FieldEdit, 0, len(edits))
	for _, fe := range edits {
		if changedFiles[fe.File] {
			out = append(out, fe)
		}
	}
	return out
}
