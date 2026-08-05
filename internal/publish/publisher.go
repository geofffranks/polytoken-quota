package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// Publisher applies validated candidate files to live Polytoken targets through
// a write-ahead journal and recovers any interrupted transaction on the next
// invocation. State is published through State (a state.Store) and is always the
// last durable step (the commit record) before the journal is removed.
type Publisher struct {
	Locker      Locker
	State       state.Store
	FS          DurableFS
	JournalPath string
	Backups     BackupStore
	// ManagedRoot is the fallback directory managed live files must stay
	// within when a Transaction does not carry its own ManagedRoot. When a
	// containment root is known (either source), Apply rejects symlinks or
	// path escapes (TOCTOU). Transactions built by the Coordinator always
	// carry the per-target canonical root, which takes precedence: a single
	// Publisher-level root cannot express multiple registered targets.
	ManagedRoot string
	// Clock returns the current time used for recovery timestamps. Defaults to
	// time.Now when nil.
	Clock func() time.Time
	// FaultHook is consulted at each named durable step when non-nil; it is a
	// test-only seam for injecting failures at every recoverable point.
	Fault FaultHook
}

func (p Publisher) fs() DurableFS {
	if p.FS != nil {
		return p.FS
	}
	return OSFS{}
}

func (p Publisher) hook(step string) error {
	if p.Fault == nil {
		return nil
	}
	return p.Fault(step)
}

func (p Publisher) now() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now()
}

// saveState publishes the committed state through the store while propagating
// the fault hook into the store's durability boundary (stepStateFsync). The
// store copy is local, so the propagation does not leak into the recovery paths,
// which call p.State.Save directly and must always complete the commit.
func (p Publisher) saveState(s state.State) error {
	st := p.State
	st.Fault = func() error { return p.hook(stepStateFsync) }
	return st.Save(s)
}

// Apply writes tx.Replacements to their live paths through a crash-consistent
// journal, then publishes tx.Next as the committed observed state. It first
// recovers any prior unfinished journal for the same target so a crash mid-apply
// never overlaps. It returns the committed state.
//
// Apply order: lock → recover prior journal → create backups → write journal →
// per file: write temp, fsync temp, atomic rename, fsync parent dir, update
// journal progress → publish state.json last (commit) → remove journal → unlock.
//
// Apply is the self-locking entry point and owns recovery for standalone use.
// Callers that already hold the lock (e.g. the Coordinator, which locks once
// for a whole multi-target transaction) MUST recover once and then call
// ApplyUnderLock per target instead: flock(2) LOCK_EX is not re-entrant, so a
// second exclusive acquire on the same path from the same process deadlocks.
func (p Publisher) Apply(ctx context.Context, tx Transaction) (state.State, error) {
	if p.Locker == nil {
		return state.State{}, errors.New("publish: no locker configured")
	}
	unlock, err := p.Locker.Lock(ctx)
	if err != nil {
		return state.State{}, err
	}
	defer func() { _ = unlock() }()
	// Recover any prior unfinished transaction before starting a new one, so a
	// crash mid-apply converges to a coherent committed state. ApplyUnderLock
	// assumes this has already happened.
	if _, _, err := p.Recover(ctx, tx.Prior); err != nil {
		return state.State{}, fmt.Errorf("publish: pre-apply recover: %w", err)
	}
	return p.ApplyUnderLock(ctx, tx)
}

// ApplyUnderLock performs the full journal + file replacement + state commit +
// journal removal WITHOUT acquiring or releasing the lock. The caller holds the
// advisory lock for the whole transaction. ApplyUnderLock must NOT acquire the
// lock: flock(2) LOCK_EX is not re-entrant, and re-locking the same path from
// the same process returns EWOULDBLOCK, deadlocking.
//
// ApplyUnderLock assumes the journal was already recovered by the caller. The
// Coordinator recovers exactly once at the top of its transaction (before any
// per-target apply), so recovery happens a single time; ApplyUnderLock must not
// re-run Recover. The public Apply wrapper recovers then calls ApplyUnderLock
// for standalone use.
//
// Each invocation commits state.json (the commit record) before removing its
// journal, so the terminal invariant (state published before journal removed)
// holds per target. When the Coordinator publishes several targets under one
// held lock, these per-target state publishes are cumulative, atomic
// intermediate commits; the Coordinator's final saveState is the authoritative
// commit that supersedes them.
func (p Publisher) ApplyUnderLock(ctx context.Context, tx Transaction) (state.State, error) {
	if err := p.validateTransaction(tx); err != nil {
		return state.State{}, err
	}

	// 1. Create bounded mode-preserving backups of every live file about to be
	// replaced. Failures here abort before the journal is written.
	for i := range tx.Replacements {
		r := &tx.Replacements[i]
		if err := p.hook(stepBackup); err != nil {
			return state.State{}, err
		}
		bp, err := p.Backups.Snapshot(r.LivePath)
		if err != nil {
			return state.State{}, err
		}
		r.BackupPath = bp
	}

	// 2. Write the durable journal BEFORE any live-file rename. The journal
	// stores only hashes and paths.
	j := Journal{
		Schema:        JournalSchema,
		PriorRevision: tx.Prior.Revision,
		NextRevision:  tx.Next.Revision,
		TargetID:      tx.TargetID,
		ManagedRoot:   p.managedRootFor(tx),
		Replacements:  cloneReplacements(tx.Replacements),
		Intended:      intendedOutcome(tx),
	}
	if err := writeJournal(p.fs(), p.JournalPath, j, p.Fault); err != nil {
		return state.State{}, err
	}

	// 3. Apply each replacement: temp write, fsync temp, atomic rename, fsync
	// parent dir, then record progress in the journal.
	for i := range tx.Replacements {
		r := &tx.Replacements[i]
		if err := p.applyOne(p.managedRootFor(tx), r); err != nil {
			// Leave the journal in place; Recover will restore from backup.
			return state.State{}, err
		}
		j.Replacements[i].Applied = true
		if err := p.hook(stepProgress); err != nil {
			return state.State{}, err
		}
		if err := writeJournal(p.fs(), p.JournalPath, j, p.Fault); err != nil {
			return state.State{}, err
		}
	}

	// 4. Publish state.json last — the commit record. The durability boundary
	// lives inside Save (stepStateFsync): the fault hook fires after the temp
	// write but before the fsync, so if a crash lands there state.json stays at
	// the prior revision and the journal survives for Recover to repair. The
	// journal is removed only once the commit is durable.
	if err := p.hook(stepStatePublish); err != nil {
		return state.State{}, err
	}
	if err := p.saveState(tx.Next); err != nil {
		return state.State{}, err
	}

	// 5. Remove the journal only after the committed state is durable.
	if err := p.hook(stepJournalRemove); err != nil {
		return state.State{}, err
	}
	if err := removeJournal(p.fs(), p.JournalPath); err != nil {
		return state.State{}, err
	}
	return tx.Next, nil
}

// validateTransaction checks the TOCTOU defenses (paths within ManagedRoot, no
// symlinks) and required fields.
func (p Publisher) validateTransaction(tx Transaction) error {
	if tx.TargetID == "" {
		return errors.New("publish: empty target id")
	}
	if tx.Next.Revision == 0 {
		return errors.New("publish: next state has zero revision")
	}
	root := p.managedRootFor(tx)
	for i := range tx.Replacements {
		r := &tx.Replacements[i]
		if r.LivePath == "" {
			return fmt.Errorf("publish: replacement %d empty live path", i)
		}
		if root != "" {
			if err := ensureNoSymlink(root, r.LivePath); err != nil {
				return fmt.Errorf("publish: live path %s: %w", r.LivePath, err)
			}
		}
	}
	return nil
}

// managedRootFor returns the containment root for tx: the transaction's own
// per-target root when set, otherwise the Publisher-level fallback.
func (p Publisher) managedRootFor(tx Transaction) string {
	if tx.ManagedRoot != "" {
		return tx.ManagedRoot
	}
	return p.ManagedRoot
}

// applyOne performs one durable replacement: verify the staged temp file's hash,
// fsync it, atomically rename it over the live path, then fsync the parent dir.
// root is the containment root the live path must stay within (empty disables
// the check). Each step consults the fault hook so failures are recoverable.
func (p Publisher) applyOne(root string, r *Replacement) error {
	fs := p.fs()
	if r.TempPath == "" {
		return fmt.Errorf("publish: replacement %s has no temp path", r.LivePath)
	}
	// TOCTOU: re-check immediately before write.
	if root != "" {
		if err := ensureNoSymlink(root, r.LivePath); err != nil {
			return err
		}
	}
	// Verify the staged candidate temp content matches NewHash before renaming.
	data, err := fs.ReadFile(r.TempPath)
	if err != nil {
		return errStep(stepTempWrite, err)
	}
	if got := sha256Bytes(data); got != r.NewHash {
		return fmt.Errorf("publish: temp %s hash mismatch", r.TempPath)
	}
	// Apply the source mode to the temp file so the renamed live file preserves
	// it. Temp write fault targets this fsync-less write step.
	if err := p.hook(stepTempWrite); err != nil {
		return err
	}
	if err := fs.WriteFile(r.TempPath, data, r.Mode.Perm()); err != nil {
		return errStep(stepTempWrite, err)
	}
	// fsync the temp file via an open handle.
	if err := p.fsyncPath(r.TempPath, stepTempFsync); err != nil {
		return err
	}
	// Atomic rename temp → live.
	if err := p.hook(stepRename); err != nil {
		return err
	}
	if err := fs.Rename(r.TempPath, r.LivePath); err != nil {
		return errStep(stepRename, err)
	}
	// fsync parent dir so the rename entry is durable.
	if err := p.hook(stepDirFsync); err != nil {
		return err
	}
	if err := fs.SyncDir(parentDir(r.LivePath)); err != nil {
		return errStep(stepDirFsync, err)
	}
	r.Applied = true
	return nil
}

// fsyncPath opens path through the FS's Open, fsyncs it, and closes it. Used for
// the temp-file fsync step in applyOne.
func (p Publisher) fsyncPath(path, step string) error {
	fs := p.fs()
	f, err := fs.Open(path)
	if err != nil {
		return errStep(step, err)
	}
	if err := p.hook(step); err != nil {
		_ = f.Close()
		return err
	}
	if err := fs.Fsync(f); err != nil {
		_ = f.Close()
		return errStep(step, err)
	}
	return f.Close()
}

// Recover compares the journal, live-file hashes, backups, and committed state
// revision to converge a possibly-interrupted transaction to a coherent
// committed state. Every recovery path — roll-forward, restore-from-backup, or
// no-op — ends by atomically publishing state.json with the accepted event
// state and final per-target outcomes BEFORE the journal is removed. Recovery
// never loses an accepted event.
//
// Recover is called by Apply at the start of every mutation and by the
// coordinator at startup. prior is the currently committed observed state.
func (p Publisher) Recover(ctx context.Context, prior state.State) (state.State, RecoveryReport, error) {
	j, ok, err := readJournal(p.fs(), p.JournalPath)
	if err != nil {
		// Corrupt or incomplete journal: distinguish it from a genuine no-op so
		// the coordinator and status reporting can surface the failure. The
		// journal is left in place for an operator to inspect.
		return prior, RecoveryReport{Action: ActionCorrupt}, err
	}
	if !ok {
		// No journal → no-op coherent commit.
		return prior, RecoveryReport{Action: ActionNoop, TargetID: ""}, nil
	}

	// If the next state revision is already committed, the transaction is done;
	// just remove the stale journal (idempotent no-op). A removal failure is
	// reported but never fails the recovery: the commit is already durable.
	if prior.Revision == j.NextRevision {
		report := RecoveryReport{Action: ActionNoop, TargetID: j.TargetID}
		if err := removeJournal(p.fs(), p.JournalPath); err != nil {
			report.CleanupError = err.Error()
		}
		return prior, report, nil
	}

	// I3.1: the journal was recorded against a specific base revision. If that
	// no longer matches the committed prior, the journal is stale — written for
	// a different base state — and must not be applied. Treat it as corrupt and
	// leave it for inspection rather than rolling forward/restoring the wrong
	// base.
	if j.PriorRevision != prior.Revision {
		return prior, RecoveryReport{Action: ActionCorrupt, TargetID: j.TargetID},
			fmt.Errorf("publish: journal prior revision %d != committed %d", j.PriorRevision, prior.Revision)
	}

	// The journal's path-bearing fields are persisted input: re-validate every
	// path (containment + no-symlink for live paths, backup-root containment
	// for backups) before any read or restore, exactly as Apply validates a
	// fresh transaction. A tampered or corrupt-but-parseable journal must not
	// direct recovery reads or writes to arbitrary paths.
	if err := p.validateJournalPaths(j); err != nil {
		return prior, RecoveryReport{Action: ActionCorrupt, TargetID: j.TargetID}, err
	}

	// Decide roll-forward vs restore by comparing live-file hashes.
	if p.allIntendedPresent(j) {
		return p.rollForward(prior, j)
	}
	return p.restoreFromBackup(prior, j)
}

// validateJournalPaths re-applies the transaction path defenses to a decoded
// journal before recovery touches the filesystem. Live paths must be
// non-empty and — when a containment root is known (the journal's own
// ManagedRoot, else the Publisher fallback) — contained with no symlink
// components. Backup paths, when present, must sit directly under the
// configured backup root.
func (p Publisher) validateJournalPaths(j Journal) error {
	root := j.ManagedRoot
	if root == "" {
		root = p.ManagedRoot
	}
	for i := range j.Replacements {
		r := &j.Replacements[i]
		if r.LivePath == "" {
			return fmt.Errorf("publish: journal replacement %d empty live path", i)
		}
		if root != "" {
			if err := ensureNoSymlink(root, r.LivePath); err != nil {
				return fmt.Errorf("publish: journal live path %s: %w", r.LivePath, err)
			}
		}
		if r.BackupPath != "" && p.Backups.Root != "" {
			backupRoot, err := filepathAbs(p.Backups.Root)
			if err != nil {
				return err
			}
			backup, err := filepathAbs(r.BackupPath)
			if err != nil {
				return err
			}
			if !isWithin(backupRoot, backup) {
				return fmt.Errorf("publish: journal backup path %s: %w", r.BackupPath, ErrPathEscape)
			}
		}
	}
	return nil
}

// allIntendedPresent reports whether every replacement's live file exists and
// matches its NewHash.
func (p Publisher) allIntendedPresent(j Journal) bool {
	for _, r := range j.Replacements {
		got, err := sha256OfFile(p.fs(), r.LivePath)
		if err != nil || got != r.NewHash {
			return false
		}
	}
	return true
}

// rollForward publishes the accepted next state with the target applied, then
// removes the journal.
func (p Publisher) rollForward(prior state.State, j Journal) (state.State, RecoveryReport, error) {
	next := advanceState(prior, j, true, p.now())
	if err := p.State.Save(next); err != nil {
		return prior, RecoveryReport{Action: ActionRollForward, TargetID: j.TargetID}, err
	}
	report := RecoveryReport{Action: ActionRollForward, TargetID: j.TargetID}
	if err := removeJournal(p.fs(), p.JournalPath); err != nil {
		report.CleanupError = err.Error()
	}
	return next, report, nil
}

// restoreFromBackup restores every file in the target transaction from its
// pre-apply backup, then publishes the accepted state with the target marked
// pending (last-known-good files). The accepted event is never lost.
func (p Publisher) restoreFromBackup(prior state.State, j Journal) (state.State, RecoveryReport, error) {
	for _, r := range j.Replacements {
		if r.BackupPath == "" {
			return prior, RecoveryReport{Action: ActionRestore, TargetID: j.TargetID},
				fmt.Errorf("publish: recover: no backup for %s", r.LivePath)
		}
		if err := p.Backups.Restore(r.BackupPath, r.LivePath); err != nil {
			return prior, RecoveryReport{Action: ActionRestore, TargetID: j.TargetID}, err
		}
	}
	next := advanceState(prior, j, false, p.now())
	if err := p.State.Save(next); err != nil {
		return prior, RecoveryReport{Action: ActionRestore, TargetID: j.TargetID}, err
	}
	report := RecoveryReport{Action: ActionRestore, TargetID: j.TargetID}
	if err := removeJournal(p.fs(), p.JournalPath); err != nil {
		report.CleanupError = err.Error()
	}
	return next, report, nil
}

// advanceState rebuilds the recovered next state from prior and the journal:
// advance the revision and providers to the journal's next revision, then record
// the final per-target outcome (applied when ok, pending/last-known-good
// otherwise). The accepted event is preserved because prior's providers are
// carried forward.
func advanceState(prior state.State, j Journal, applied bool, now time.Time) state.State {
	next := cloneState(prior)
	next.Revision = j.NextRevision
	if next.Schema == 0 {
		next.Schema = 1
	}
	if next.Targets == nil {
		next.Targets = map[string]state.TargetState{}
	}
	t := j.Intended
	if applied {
		t.AppliedRevision = j.NextRevision
		t.AttemptedRevision = j.NextRevision
		t.AppliedAt = now
		t.AttemptedAt = now
		t.Pending = nil
	} else {
		t.AttemptedRevision = j.NextRevision
		t.AttemptedAt = now
		t.Pending = &state.ApplyFailure{
			TargetID:          j.TargetID,
			Stage:             "publish",
			Summary:           "restored to last-known-good after interrupted apply",
			AttemptedRevision: j.NextRevision,
			AttemptedAt:       now,
			LiveStatus:        "last-known-good",
		}
	}
	next.Targets[j.TargetID] = t
	return next
}

// cloneState returns a deep copy of s so recovered-state edits never mutate the
// caller's prior state.
func cloneState(s state.State) state.State {
	out := s
	if s.Providers != nil {
		out.Providers = make(map[string]state.ProviderState, len(s.Providers))
		for k, v := range s.Providers {
			out.Providers[k] = v
		}
	}
	if s.Targets != nil {
		out.Targets = make(map[string]state.TargetState, len(s.Targets))
		for k, v := range s.Targets {
			tp := v
			if v.Pending != nil {
				pp := *v.Pending
				tp.Pending = &pp
			}
			out.Targets[k] = tp
		}
	}
	if s.RefreshFailed != nil {
		out.RefreshFailed = append([]state.Diagnostic(nil), s.RefreshFailed...)
	}
	if s.Recovered != nil {
		out.Recovered = append([]state.ApplyFailure(nil), s.Recovered...)
	}
	return out
}

// intendedOutcome projects the target's intended post-apply outcome from the
// transaction (applied at the next revision, no pending error).
func intendedOutcome(tx Transaction) state.TargetState {
	return state.TargetState{
		AttemptedRevision: tx.Next.Revision,
		AppliedRevision:   tx.Next.Revision,
	}
}

// cloneReplacements returns a deep copy of rs.
func cloneReplacements(rs []Replacement) []Replacement {
	out := make([]Replacement, len(rs))
	copy(out, rs)
	return out
}
