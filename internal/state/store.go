package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

// PruneRecovered removes recovered errors whose ResolvedAt is at or before the
// retention cutoff (now - retention). It returns a state with a filtered
// Recovered slice and never mutates the input state.
func PruneRecovered(s State, now time.Time, retention time.Duration) State {
	cutoff := now.Add(-retention)
	kept := make([]ApplyFailure, 0, len(s.Recovered))
	for _, f := range s.Recovered {
		if f.ResolvedAt.After(cutoff) {
			kept = append(kept, f)
		}
	}
	next := s
	next.Recovered = kept
	return next
}

// usageHistoryWeeks is the maximum number of weekly usage samples retained:
// the current week plus four prior weeks.
const usageHistoryWeeks = 5

// weekStart returns the Monday 00:00 UTC boundary of the week containing t. It
// is the deterministic week boundary used for usage-history pruning.
func weekStart(t time.Time) time.Time {
	tt := t.UTC()
	// Weekday: Sunday=0, Monday=1, ..., Saturday=6. Days since Monday:
	// Monday->0, Tuesday->1, ..., Sunday->6.
	daysSinceMonday := (int(tt.Weekday()) + 6) % 7
	return time.Date(tt.Year(), tt.Month(), tt.Day()-daysSinceMonday, 0, 0, 0, 0, time.UTC)
}

// PruneUsageHistory bounds UsageHistory.Weeks to the current week plus four
// prior weeks (at most usageHistoryWeeks entries), using the Monday 00:00 UTC
// week boundary. It never mutates the input state. A nil UsageHistory is
// returned unchanged.
func PruneUsageHistory(s State, now time.Time) State {
	if s.UsageHistory == nil || len(s.UsageHistory.Weeks) == 0 {
		return s
	}
	cutoff := weekStart(now).AddDate(0, 0, -(usageHistoryWeeks-1)*7)
	kept := make([]UsageSample, 0, len(s.UsageHistory.Weeks))
	for _, w := range s.UsageHistory.Weeks {
		if !w.WeekStart.Before(cutoff) {
			kept = append(kept, w)
		}
	}
	next := s
	next.UsageHistory = &UsageHistory{Weeks: kept}
	return next
}

// sanitizeRoutingDecision re-sanitizes the persisted routing decision's free-
// text explanation so a stale or hand-edited state file cannot carry control
// characters or oversized text into the next process lifetime. It never
// mutates its input.
func sanitizeRoutingDecision(d *RoutingDecision) (*RoutingDecision, bool) {
	if d == nil {
		return nil, false
	}
	cleaned := quota.SanitizeText(d.Explanation)
	if hasControl(cleaned) {
		runes := []rune(cleaned)
		kept := make([]rune, 0, len(runes))
		for _, r := range runes {
			if r == 0 || unicode.IsControl(r) {
				continue
			}
			kept = append(kept, r)
		}
		cleaned = string(kept)
	}
	cleaned = truncateUTF8(cleaned, HistoryFreeTextBytes)
	if cleaned == d.Explanation {
		return d, false
	}
	out := *d
	out.Explanation = cleaned
	return &out, true
}

// sanitizeSnapshots runs quota.SanitizeError over every provider snapshot's
// Error field so no raw secret-bearing string can reach the persisted file. It
// never mutates the input state.
func sanitizeSnapshots(s State) State {
	changed := false
	providers := s.Providers
	for k, ps := range providers {
		snap, snapChanged := sanitizeSnap(ps.QuotaSnapshot)
		attempt, attemptChanged := sanitizeSnap(ps.QuotaAttempt)
		credits, creditsChanged := sanitizeResetCredits(ps.ResetCredits)
		decision, decisionChanged := sanitizeRoutingDecision(ps.Routing.Decision)
		if !snapChanged && !attemptChanged && !creditsChanged && !decisionChanged {
			continue
		}
		if !changed {
			// Copy the map lazily on first mutation.
			providers = make(map[string]ProviderState, len(providers))
			for kk, vv := range s.Providers {
				providers[kk] = vv
			}
			changed = true
		}
		ps.QuotaSnapshot = snap
		ps.QuotaAttempt = attempt
		ps.ResetCredits = credits
		ps.Routing.Decision = decision
		providers[k] = ps
	}
	if !changed {
		return s
	}
	next := s
	next.Providers = providers
	return next
}

// sanitizeDiagnostics re-sanitizes every persisted free-text diagnostic field
// (refresh-failed summaries and per-target apply-failure text) immediately
// before marshaling. Ingestion paths already sanitize, but a state file is
// long-lived and hand-editable, so the store never trusts that the fields it
// re-persists are still clean. It never mutates the input state.
func sanitizeDiagnostics(s State) State {
	next := s
	if len(s.RefreshFailed) > 0 {
		refresh := make([]Diagnostic, len(s.RefreshFailed))
		for i, d := range s.RefreshFailed {
			d.Summary = quota.SanitizeText(d.Summary)
			refresh[i] = d
		}
		next.RefreshFailed = refresh
	}
	if len(s.Targets) > 0 {
		targets := make(map[string]TargetState, len(s.Targets))
		for id, ts := range s.Targets {
			if ts.Pending != nil {
				pending := *ts.Pending
				pending.Summary = quota.SanitizeText(pending.Summary)
				pending.Remediation = quota.SanitizeText(pending.Remediation)
				ts.Pending = &pending
			}
			targets[id] = ts
		}
		next.Targets = targets
	}
	return next
}

func sanitizeResetCredits(credits quota.ResetCreditState) (quota.ResetCreditState, bool) {
	if credits.LatestAttempt == nil || credits.LatestAttempt.Error == "" {
		return credits, false
	}
	cleaned := quota.SanitizeText(credits.LatestAttempt.Error)
	if cleaned == credits.LatestAttempt.Error {
		return credits, false
	}
	attempt := *credits.LatestAttempt
	attempt.Error = cleaned
	credits.LatestAttempt = &attempt
	return credits, true
}

func sanitizeSnap(snap *quota.QuotaSnapshot) (*quota.QuotaSnapshot, bool) {
	if snap == nil {
		return snap, false
	}
	out := *snap
	changed := false
	if snap.Error != "" {
		cleaned := quota.SanitizeError(strErr(snap.Error))
		if cleaned != snap.Error {
			out.Error = cleaned
			changed = true
		}
	}
	if snap.ResetCredits != nil && snap.ResetCredits.Error != "" {
		cleaned := quota.SanitizeText(snap.ResetCredits.Error)
		if cleaned != snap.ResetCredits.Error {
			attempt := *snap.ResetCredits
			attempt.Error = cleaned
			out.ResetCredits = &attempt
			changed = true
		}
	}
	if !changed {
		return snap, false
	}
	return &out, true
}

// strErr adapts a plain string to an error so it can flow through
// quota.SanitizeError.
type strErr string

func (e strErr) Error() string { return string(e) }

// Load reads and decodes the state file. A missing file returns a fresh, empty
// schema-5 state with initialized maps and counters. Legacy schemas migrate
// operational fields in memory and discard legacy reconcile history before event
// pruneObsoleteTriggers drops history records whose trigger kind no longer
// exists (e.g. legacy "hook" records after hook ingestion was removed), so a
// schema-4 file loads cleanly under schema 5. Dropped records are accounted in
// OmittedHistoryRecords.
func pruneObsoleteTriggers(h ReconcileHistory) ReconcileHistory {
	kept := make([]ReconcileRecord, 0, len(h.Records))
	for _, r := range h.Records {
		switch r.Trigger.Kind {
		case TriggerInit, TriggerReconcile, TriggerRoutingDisable, TriggerRoutingEnable,
			TriggerRoutingReset, TriggerQuotaCheck, TriggerSet, TriggerClear:
			kept = append(kept, r)
		default:
			h.OmittedHistoryRecords++
		}
	}
	h.Records = kept
	return h
}

// validation. Loading never writes the file.
func (st Store) Load() (State, error) {
	data, err := os.ReadFile(st.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return newState(), nil
		}
		return State{}, fmt.Errorf("state: read %s: %w", st.Path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("state: parse %s: %w", st.Path, err)
	}
	if s.Schema < 0 || s.Schema > CurrentSchema {
		return State{}, fmt.Errorf("state: unsupported schema %d in %s (current %d)", s.Schema, st.Path, CurrentSchema)
	}
	legacySchema := s.Schema < CurrentSchema
	if legacySchema {
		s.ReconcileHistory = pruneObsoleteTriggers(s.ReconcileHistory)
		s.EventHistory = EventHistory{}
	}
	if s.Providers == nil {
		s.Providers = map[string]ProviderState{}
	}
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	if s.NextArrivalSequence == 0 {
		maxArrival := uint64(0)
		for _, ps := range s.Providers {
			if ps.QuotaArrival > maxArrival {
				maxArrival = ps.QuotaArrival
			}
			if ps.AvailabilityArrival > maxArrival {
				maxArrival = ps.AvailabilityArrival
			}
		}
		if maxArrival == ^uint64(0) {
			return State{}, fmt.Errorf("state: arrival sequence overflow in %s", st.Path)
		}
		s.NextArrivalSequence = maxArrival + 1
	}
	if s.Schema < CurrentSchema {
		s.Schema = CurrentSchema
	}
	if err := ValidateEventHistory(s.EventHistory); err != nil {
		return State{}, fmt.Errorf("state: validate event history in %s: %w", st.Path, err)
	}
	return s, nil
}

// Save prunes recovered errors older than RecoveredRetention (by Now) and
// atomically writes the state with mode 0600. The write is crash-consistent via
// a same-directory temporary file: the temp file is fsync'd BEFORE the rename
// and the parent directory is fsync'd AFTER, so the committed state.json is
// durably atomic — a crash can never leave a torn or stale commit record while a
// journal that depends on it has already been removed (the terminal invariant).
// A reader never observes a partial state file. The persisted JSON contains only
// sanitized state fields — never provider credentials, account names, auth
// blocks, or unmanaged source content.
func (st Store) Save(s State) error {
	s = PruneRecovered(s, st.now(), st.RecoveredRetention)
	s = PruneUsageHistory(s, st.now())
	s = sanitizeSnapshots(s)
	s = sanitizeDiagnostics(s)
	var err error
	s.EventHistory, err = BoundEventHistory(SanitizeEventHistory(s.EventHistory))
	if err != nil {
		return fmt.Errorf("state: bound event history: %w", err)
	}
	if err := ValidateEventHistory(s.EventHistory); err != nil {
		return fmt.Errorf("state: validate event history: %w", err)
	}
	if s.NextArrivalSequence == 0 {
		maxArrival := uint64(0)
		for _, ps := range s.Providers {
			if ps.QuotaArrival > maxArrival {
				maxArrival = ps.QuotaArrival
			}
			if ps.AvailabilityArrival > maxArrival {
				maxArrival = ps.AvailabilityArrival
			}
		}
		if maxArrival == ^uint64(0) {
			return fmt.Errorf("state: arrival sequence overflow")
		}
		s.NextArrivalSequence = maxArrival + 1
	}
	s.Schema = CurrentSchema
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}
	dir := filepath.Dir(st.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: create dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	// Durability fault boundary (test-only seam): the temp file has been written
	// but is not yet durable. A non-nil Fault simulates a crash between the write
	// and the fsync so callers can prove the commit is not durable here.
	if err := st.fault(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	// C1: fsync the temp file's bytes BEFORE the rename so the commit record is
	// stable the instant it becomes visible via the rename.
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("state: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmp, st.Path); err != nil {
		cleanup()
		return fmt.Errorf("state: rename temp: %w", err)
	}
	// C1: fsync the parent directory AFTER the rename so the directory entry
	// pointing at the new state.json is stable. Without this a crash after the
	// rename could lose the new entry while the journal is already gone.
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("state: fsync dir: %w", err)
	}
	return nil
}

// fault consults the optional test-only durability seam. It returns nil when
// unset (production).
func (st Store) fault() error {
	if st.Fault == nil {
		return nil
	}
	return st.Fault()
}

// fsyncDir flushes directory-entry changes (the rename) to stable storage by
// fsync'ing the parent directory's open file descriptor. All supported targets
// (Linux, macOS, and other Unix) accept fsync on a directory fd; should a future
// platform lack it, returning nil is an acceptable degradation at the cost of a
// wider rename crash window.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return syscall.Fsync(int(d.Fd()))
}

func (st Store) now() time.Time {
	if st.Now != nil {
		return st.Now()
	}
	return time.Now()
}

func newState() State {
	return State{
		Schema:              CurrentSchema,
		Providers:           map[string]ProviderState{},
		Targets:             map[string]TargetState{},
		NextEventSequence:   1,
		NextArrivalSequence: 1,
	}
}
