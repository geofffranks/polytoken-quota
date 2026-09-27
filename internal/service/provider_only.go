package service

// Provider-only policy support: the migration preview and the explicit
// unsupported-result guards for chain-dependent commands.
//
// A provider-only policy enrolls Polytoken provider IDs with quota adapter
// configuration and a global target; it carries no model enumeration and no
// chain definitions. Every command that projects chains therefore has exactly
// two honest options here: maintain its provider-only function, or return a
// clear unsupported result. A silent no-op is never acceptable, so the guards
// below reject with a fixed, actionable message before any state is touched.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// ErrProviderOnlyUnsupported is the base of every provider-only unsupported
// result. Commands wrap it with the specific limitation so operators always
// see which capability is missing and what to do instead.
var ErrProviderOnlyUnsupported = errors.New("provider-only policies do not support legacy chain management")

// providerOnlyUnsupported builds the clear rejection for a chain-dependent
// command under a provider-only policy. detail states what is unavailable and
// the honest alternative, if one exists.
func providerOnlyUnsupported(command, detail string) error {
	return fmt.Errorf("%w: %s %s", ErrProviderOnlyUnsupported, command, detail)
}

// MigrationPreview is the read-only report surfaced before (and by) adopting
// provider-only policy mode over an existing installation. It documents that
// previously quota-authored legacy edits — defaults chains, definition chains,
// and model enablement — persist in the Polytoken configuration as
// operator-owned bytes, and it surfaces the backup and journal references an
// operator needs for rollback. Producing a preview never writes anything.
type MigrationPreview struct {
	// Mode is the target policy mode: "provider-only".
	Mode string `json:"mode"`
	// EnrolledProviders lists the Polytoken provider IDs the new policy enrolls.
	EnrolledProviders []string `json:"enrolled_providers"`
	// GlobalRoot is the global Polytoken configuration root the new policy targets.
	GlobalRoot string `json:"global_root"`
	// LegacyOwnedEdits enumerates the legacy quota-authored managed fields that
	// persist as operator-owned after the migration. Empty on a first init.
	LegacyOwnedEdits []LegacyOwnedEdit `json:"legacy_owned_edits,omitempty"`
	// BackupsPath is the bounded backup store holding pre-apply backups of
	// every managed file quota has edited. Empty when unknown.
	BackupsPath string `json:"backups_path,omitempty"`
	// BackupsPresent reports whether any backup exists at BackupsPath yet.
	BackupsPresent bool `json:"backups_present"`
	// JournalPath is the write-ahead apply journal recording committed edits
	// for crash recovery. Empty when unknown.
	JournalPath string `json:"journal_path,omitempty"`
	// JournalPresent reports whether a journal exists at JournalPath yet.
	JournalPresent bool `json:"journal_present"`
	// PolicyBackupPath is where the replaced legacy policy file itself is
	// preserved during a migration. Empty when unknown (first init).
	PolicyBackupPath string `json:"policy_backup_path,omitempty"`
	// PolicyBackupPresent reports whether a preserved legacy policy already
	// exists at PolicyBackupPath (a previous migration or replace).
	PolicyBackupPresent bool `json:"policy_backup_present"`
	// Rollback carries fixed rollback guidance lines.
	Rollback []string `json:"rollback"`
}

// LegacyOwnedEdit is one class of legacy quota-authored managed field that
// persists as operator-owned after migrating to provider-only mode.
type LegacyOwnedEdit struct {
	// TargetID names the registered target ("global" or a project ID).
	TargetID string `json:"target_id"`
	// File names the managed file carrying the edit ("config.yaml" for
	// defaults and model fields, or the definition's relative path).
	File string `json:"file"`
	// Field names the managed field class (defaults.full, definition chain,
	// models).
	Field string `json:"field"`
	// Detail is a bounded, sanitized description of the persisted edit.
	Detail string `json:"detail"`
}

// PreviewProviderOnlyMigration produces the migration preview without writing
// anything: what the provider-only policy would enroll, which legacy
// quota-authored edits persist as operator-owned, where backups and the apply
// journal live, and how to roll back.
func (c *Coordinator) PreviewProviderOnlyMigration(ctx context.Context) (MigrationPreview, error) {
	if c.Sources == nil {
		return MigrationPreview{}, errors.New("service: migration preview requires a source reader")
	}
	proposal, err := policy.InitProviderOnly(ctx, c.Sources)
	if err != nil {
		return MigrationPreview{}, err
	}
	var existing *policy.Desired
	if c.Policy != nil {
		if loaded, err := c.Policy.LoadPolicy(); err == nil {
			existing = &loaded
		} else if !errors.Is(err, os.ErrNotExist) {
			return MigrationPreview{}, fmt.Errorf("service: existing desired.yaml could not be read for the migration preview: %w", err)
		}
	}
	return c.buildMigrationPreview(proposal, existing), nil
}

// buildMigrationPreview assembles the preview from the provider-only proposal
// and the (optional) existing policy. It reads only already-loaded data plus
// the backup/journal path existence; it never writes.
func (c *Coordinator) buildMigrationPreview(proposal policy.Desired, existing *policy.Desired) MigrationPreview {
	p := MigrationPreview{
		Mode:              string(policy.ModeProviderOnly),
		EnrolledProviders: sortedProviderIDs(proposal),
		GlobalRoot:        proposal.Global.Root,
		BackupsPath:       c.BackupsPath,
		JournalPath:       c.JournalPath,
	}
	if c.BackupsPath != "" {
		p.BackupsPresent = pathExists(c.BackupsPath)
	}
	if c.JournalPath != "" {
		p.JournalPresent = pathExists(c.JournalPath)
	}
	if existing != nil && !existing.ProviderOnly() {
		p.LegacyOwnedEdits = legacyOwnedEdits(existing)
	}
	if path, ok := desiredPath(c.Policy); ok {
		p.PolicyBackupPath = path + policy.PolicyBackupSuffix
		p.PolicyBackupPresent = pathExists(p.PolicyBackupPath)
	}
	p.Rollback = migrationRollbackGuidance(p)
	return p
}

// migrationRollbackGuidance returns the fixed rollback lines. Values named
// here are utility-owned paths, never credential or unrelated configuration.
func migrationRollbackGuidance(p MigrationPreview) []string {
	lines := []string{
		"previously quota-authored defaults, definition chains, and model enabled flags stay in your Polytoken configuration as operator-owned edits; provider-only quota never removes or rewrites them",
	}
	if p.PolicyBackupPath != "" {
		lines = append(lines, fmt.Sprintf("the replaced legacy policy is preserved at %s; restore it to that file's original name to undo the migration", p.PolicyBackupPath))
	}
	if p.BackupsPath != "" {
		lines = append(lines, fmt.Sprintf("pre-apply backups of every managed file quota has edited remain under %s; restoring a backup reverts that file's last quota-authored edit", p.BackupsPath))
	}
	if p.JournalPath != "" {
		lines = append(lines, fmt.Sprintf("committed edits are recorded in the apply journal at %s for crash recovery", p.JournalPath))
	}
	lines = append(lines,
		"the migration only replaces the quota policy file; Polytoken configuration bytes are never touched",
		"legacy chain management and provider-only reconciliation are separate paths; rerun init without --provider-only (restoring the preserved policy first) to return to legacy behavior",
	)
	return lines
}

// legacyOwnedEdits enumerates the legacy quota-authored managed fields of an
// existing legacy policy: the defaults and classifier chains, the definition
// chains, and the enumerated model baselines. These persist as operator-owned
// bytes after migration.
func legacyOwnedEdits(existing *policy.Desired) []LegacyOwnedEdit {
	out := make([]LegacyOwnedEdit, 0, len(existing.Projects)+1)
	appendTarget := func(t policy.Target) {
		id := t.ID
		if id == "" {
			id = "global"
		}
		for _, c := range []struct {
			field string
			chain policy.Chain
		}{{"defaults.full", t.Full}, {"defaults.mini", t.Mini}, {"defaults.nano", t.Nano}, {"classifier", t.Classifier}} {
			if len(c.chain) == 0 {
				continue
			}
			out = append(out, LegacyOwnedEdit{
				TargetID: id, File: "config.yaml", Field: c.field,
				Detail: fmt.Sprintf("%d chain entr(ies) stay as configured", len(c.chain)),
			})
		}
		for _, d := range t.Definitions {
			out = append(out, LegacyOwnedEdit{
				TargetID: id, File: d.Path, Field: "definition chain",
				Detail: fmt.Sprintf("%d chain entr(ies) stay as configured", len(d.Chain)),
			})
		}
	}
	if existing.Global.Root != "" || len(existing.Global.Definitions) > 0 || len(existing.Global.Full) > 0 {
		appendTarget(existing.Global)
	}
	for _, p := range existing.Projects {
		appendTarget(p)
	}
	ids := make([]string, 0, len(existing.Providers))
	for id := range existing.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := existing.Providers[policy.MappingID(id)]
		if len(m.Models) == 0 {
			continue
		}
		out = append(out, LegacyOwnedEdit{
			TargetID: "global", File: "config.yaml", Field: "models",
			Detail: fmt.Sprintf("provider %q: %d enumerated model baseline(s) stay as configured", id, len(m.Models)),
		})
	}
	return out
}

// desiredPath extracts the desired.yaml path from a PolicyLoader that exposes
// it (the production FilePolicyLoader does). Test doubles may not.
func desiredPath(loader PolicyLoader) (string, bool) {
	dp, ok := loader.(interface{ DesiredPath() string })
	if !ok {
		return "", false
	}
	path := dp.DesiredPath()
	if path == "" {
		return "", false
	}
	return path, true
}

func sortedProviderIDs(d policy.Desired) []string {
	ids := make([]string, 0, len(d.Providers))
	for id := range d.Providers {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	return ids
}

// pathExists reports whether path exists (any file type). Read errors count
// as absent: the preview only annotates presence.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
