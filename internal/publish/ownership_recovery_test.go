package publish

// Provider-ownership metadata recovery tests: the journal's intended
// ownership snapshot and its atomic preservation across roll-forward and
// restore, plus the non-clobber refusal when live bytes match neither the
// journal's old nor new hash after an interrupted apply. All fixtures are
// synthetic.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/state"
)

// ownershipVariants covers the recorded baseline shapes the schema must round
// trip through the journal: key absent, explicit false, explicit true, and a
// held conflict marker.
var ownershipVariants = map[string]state.ProviderOwnership{
	"baseline-absent":   {BaselinePresent: false, Owned: true},
	"baseline-false":    {BaselinePresent: true, BaselineValue: false, Owned: true},
	"baseline-true":     {BaselinePresent: true, BaselineValue: true, Owned: true},
	"baseline-conflict": {BaselinePresent: true, BaselineValue: true, Owned: true, Conflict: true},
}

// TestProviderOwnershipJournalIntendedStateAndRollForward proves the journal
// durably records the transaction's intended ownership snapshot and that
// roll-forward after a crash at the state commit adopts it atomically — for
// every baseline variant.
func TestProviderOwnershipJournalIntendedStateAndRollForward(t *testing.T) {
	for name, want := range ownershipVariants {
		t.Run(name, func(t *testing.T) {
			env := faultEnv(t, "state-fsync")
			env.Tx.Next.ProviderOwnership = map[string]state.ProviderOwnership{"prov-a": want}
			if _, err := env.Publisher.Apply(context.Background(), env.Tx); err == nil {
				t.Fatal("expected Apply to fail at the injected state-fsync step")
			}

			// The durable journal carries the intended snapshot verbatim.
			j, ok, err := readJournal(OSFS{}, env.Publisher.JournalPath)
			if err != nil || !ok {
				t.Fatalf("read journal: ok=%v err=%v", ok, err)
			}
			if !j.OwnershipSet || j.Ownership["prov-a"] != want {
				t.Fatalf("journal ownership = set=%v %+v, want %+v", j.OwnershipSet, j.Ownership["prov-a"], want)
			}

			env.rewiredForRecover()
			final, report, err := env.Publisher.Recover(context.Background(), env.Prior)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if report.Action != ActionRollForward {
				t.Fatalf("action=%s want roll-forward", report.Action)
			}
			if got := final.ProviderOwnership["prov-a"]; got != want {
				t.Fatalf("rolled-forward ownership = %+v, want %+v", got, want)
			}
			committed := env.committedState(t)
			if got := committed.ProviderOwnership["prov-a"]; got != want {
				t.Fatalf("committed ownership = %+v, want %+v", got, want)
			}
			if _, err := os.Stat(env.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal not removed after roll-forward: %v", err)
			}
		})
	}
}

// TestProviderOwnershipRestoreKeepsPriorMetadata proves restore keeps the
// prior state's ownership metadata: the restored bytes are the
// pre-transaction ones, so the journal's intended snapshot must NOT be
// adopted.
func TestProviderOwnershipRestoreKeepsPriorMetadata(t *testing.T) {
	env := faultEnv(t, "rename")
	priorOwnership := map[string]state.ProviderOwnership{
		"prov-a": {BaselinePresent: true, BaselineValue: true},
	}
	env.Prior.ProviderOwnership = priorOwnership
	env.Tx.Prior = env.Prior
	env.Tx.Next.ProviderOwnership = map[string]state.ProviderOwnership{
		"prov-a": {BaselinePresent: true, BaselineValue: true, Owned: true},
	}
	if _, err := env.Publisher.Apply(context.Background(), env.Tx); err == nil {
		t.Fatal("expected Apply to fail at the injected rename step")
	}

	env.rewiredForRecover()
	final, report, err := env.Publisher.Recover(context.Background(), env.Prior)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Action != ActionRestore {
		t.Fatalf("action=%s want restore", report.Action)
	}
	if got := final.ProviderOwnership["prov-a"]; got != priorOwnership["prov-a"] {
		t.Fatalf("restored ownership = %+v, want prior %+v", got, priorOwnership["prov-a"])
	}
	if committed := env.committedState(t); committed.ProviderOwnership["prov-a"] != priorOwnership["prov-a"] {
		t.Fatalf("committed ownership = %+v, want prior %+v", committed.ProviderOwnership["prov-a"], priorOwnership["prov-a"])
	}
}

// TestProviderOwnershipLegacyJournalKeepsPriorMetadata proves a legacy journal
// (no ownership_set field) never disturbs the prior state's ownership
// metadata on roll-forward.
func TestProviderOwnershipLegacyJournalKeepsPriorMetadata(t *testing.T) {
	env := newStagedEnv(t, "")
	priorOwnership := map[string]state.ProviderOwnership{
		"prov-a": {BaselinePresent: false, Owned: true},
	}
	env.Prior.ProviderOwnership = priorOwnership

	// Hand-written journal without the ownership fields: exactly what a
	// previous binary persisted.
	j := Journal{
		Schema:        JournalSchema,
		PriorRevision: env.Prior.Revision,
		NextRevision:  env.Tx.Next.Revision,
		TargetID:      env.Tx.TargetID,
		Intended:      intendedOutcome(env.Tx),
		Replacements:  cloneReplacements(env.Tx.Replacements),
	}
	if err := writeJournal(env.Publisher.fs(), env.Publisher.JournalPath, j, nil); err != nil {
		t.Fatal(err)
	}
	// Live file already holds the candidate content, so recovery takes the
	// roll-forward path.
	if err := os.WriteFile(env.LivePath, []byte(constCandidate), env.Tx.Replacements[0].Mode.Perm()); err != nil {
		t.Fatal(err)
	}

	final, report, err := env.Publisher.Recover(context.Background(), env.Prior)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Action != ActionRollForward {
		t.Fatalf("action=%s want roll-forward", report.Action)
	}
	if got := final.ProviderOwnership["prov-a"]; got != priorOwnership["prov-a"] {
		t.Fatalf("ownership = %+v, want prior %+v preserved", got, priorOwnership["prov-a"])
	}
}

// TestProviderOwnershipRecoveryRefusesExternalLiveBytes is the non-clobber
// safeguard: after an interrupted apply, live bytes matching neither the
// journal's old nor new hash must never be overwritten. Recovery refuses,
// keeps the journal, and records a pending recover-stage failure — from both
// interruption points (rename → restore path; state-fsync → applied but
// uncommitted). Once the operator puts the recorded bytes back, recovery
// converges normally.
func TestProviderOwnershipRecoveryRefusesExternalLiveBytes(t *testing.T) {
	const external = "model: operator/edited\n"
	for _, step := range []string{"rename", "state-fsync"} {
		t.Run(step, func(t *testing.T) {
			env := faultEnv(t, step)
			env.Tx.Next.ProviderOwnership = map[string]state.ProviderOwnership{
				"prov-a": {BaselinePresent: true, BaselineValue: true, Owned: true},
			}
			if _, err := env.Publisher.Apply(context.Background(), env.Tx); err == nil {
				t.Fatalf("expected Apply to fail at the injected %s step", step)
			}

			// External writer lands while the transaction is interrupted.
			if err := os.WriteFile(env.LivePath, []byte(external), 0o600); err != nil {
				t.Fatal(err)
			}

			env.rewiredForRecover()
			_, report, err := env.Publisher.Recover(context.Background(), env.Prior)
			if err == nil {
				t.Fatalf("recovery must refuse external live bytes; report=%+v", report)
			}
			if report.Action != ActionRefuseExternal {
				t.Fatalf("action=%s want refuse-external", report.Action)
			}
			got, err := os.ReadFile(env.LivePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != external {
				t.Fatalf("external live bytes were overwritten:\n%s", got)
			}
			if _, err := os.Stat(env.Publisher.JournalPath); err != nil {
				t.Fatalf("journal not retained after refusal: %v", err)
			}
			committed := env.committedState(t)
			ts := committed.Targets["global"]
			if ts.Pending == nil || ts.Pending.Stage != "recover" || ts.Pending.LiveStatus != "journal-retained" {
				t.Fatalf("pending conflict not retained: %+v", ts)
			}
			if got := committed.ProviderOwnership["prov-a"]; got.Owned {
				t.Fatalf("refusal must keep prior ownership, got %+v", got)
			}

			// Operator resolution: the recorded baseline bytes return, and
			// recovery converges (restore path), consuming the journal.
			if err := os.WriteFile(env.LivePath, []byte(constLive), 0o600); err != nil {
				t.Fatal(err)
			}
			_, report, err = env.Publisher.Recover(context.Background(), env.Prior)
			if err != nil {
				t.Fatalf("recover after resolution: %v", err)
			}
			if report.Action != ActionRestore {
				t.Fatalf("action=%s want restore after resolution", report.Action)
			}
			if _, err := os.Stat(env.Publisher.JournalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal not removed after resolved recovery: %v", err)
			}
		})
	}
}
