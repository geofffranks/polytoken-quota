package notice

import (
	"os"
	"syscall"
	"time"
)

// lockState serializes the complete read/modify/write transition, including
// reload consumption. Contention skips this event rather than waiting; a later
// event can retry. The stable lock file is separate from the renamed state file.
func (d *HookDeps) lockState() (func(), bool) {
	failed := func(reason string) (func(), bool) {
		d.diagnose(diagnosticRecord{Event: "state_lock", Reason: reason})
		return nil, false
	}
	quota := privateSessionQuota(d.sessionsRoot(), d.env("POLYTOKEN_SESSION_ID"))
	if quota == nil {
		return failed("unsafe_directory")
	}
	f := privateDiagnosticFile(quota, "state.lock")
	if f == nil {
		quota.Close()
		return failed("unsafe_lock")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = quota.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return failed("busy")
		}
		return failed("unavailable")
	}
	// Verify the opened lock is still the directory's named lock before using
	// it. The private parent excludes other users replacing it afterward.
	st, err := quota.Lstat("state.lock")
	opened, statErr := f.Stat()
	if err != nil || statErr != nil || !privateStateInfo(st) || !os.SameFile(st, opened) {
		f.Close()
		quota.Close()
		return failed("unsafe_lock")
	}
	d.stateRoot = quota
	return func() {
		d.stateRoot = nil
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		_ = quota.Close()
	}, true
}

// ensureBaseline requires the state lock and a valid notice. A legacy consumed
// revision is retained as the baseline so a newer notice remains eligible.
// The first observed notice otherwise establishes the baseline without reload.
// No in-memory marker is advanced unless its atomic write succeeds.
func (d HookDeps) ensureBaseline(state *hookState, revision uint64) (firstObserved, ok bool) {
	if state.BaselineRevision != nil {
		return false, true
	}
	baseline := revision
	reason := "first_observed"
	firstObserved = true
	if state.ConsumedRevision > 0 {
		baseline = state.ConsumedRevision
		reason = "legacy_consumed"
		firstObserved = false
	}
	next := *state
	next.BaselineRevision = &baseline
	next.UpdatedAt = d.now().UTC().Format(time.RFC3339)
	err := d.writeState(next)
	d.markerResult(err, next)
	if err != nil {
		d.diagnose(diagnosticRecord{Event: "baseline_init", Reason: "write_failed", Revision: baseline, Consumed: state.ConsumedRevision})
		return false, false
	}
	*state = next
	d.diagnose(diagnosticRecord{Event: "baseline_init", Reason: reason, Revision: baseline, Consumed: state.ConsumedRevision})
	return firstObserved, true
}

func (d HookDeps) initializeSession() {
	sessDir, sessionID := d.sessionDir()
	if sessionID == "" {
		d.diagnose(diagnosticRecord{Event: "noop", Reason: "missing_session"})
		return
	}
	path := d.statePath(sessDir)
	unlock, ok := d.lockState()
	if !ok {
		return
	}
	defer unlock()
	state, ok := d.readState(path)
	if !ok {
		return
	}
	if revision, valid := d.noticeRevision(); valid {
		d.ensureBaseline(&state, revision)
	}
}
