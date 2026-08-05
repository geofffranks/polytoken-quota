package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/target"
)

// brokenLoader simulates each policy-loader failure mode.
type brokenLoader struct {
	exists bool
	err    error
	des    policy.Desired
}

func (l brokenLoader) LoadPolicy() (policy.Desired, error) { return l.des, l.err }
func (l brokenLoader) DesiredExists() bool                 { return l.exists }

// failingRegistry returns a fixed resolution error.
type failingRegistry struct{ err error }

func (r failingRegistry) ResolveTargets(policy.Desired) ([]RegisteredTarget, error) {
	return nil, r.err
}

func TestPolicyDoctorInspectorFindings(t *testing.T) {
	// Missing desired.yaml is actionable.
	fs := PolicyDoctorInspector{Loader: brokenLoader{exists: false}}.Findings(context.Background())
	if len(fs) != 1 || fs[0].Code != "policy-schema" {
		t.Fatalf("missing-policy findings=%+v", fs)
	}
	// Load error is actionable and sanitized.
	fs = PolicyDoctorInspector{Loader: brokenLoader{exists: true, err: errors.New("bad chain token=SECRET-x")}}.Findings(context.Background())
	if len(fs) != 1 || fs[0].Code != "policy-schema" {
		t.Fatalf("load-error findings=%+v", fs)
	}
	if strings.Contains(fs[0].Message, "SECRET-x") {
		t.Fatalf("unsanitized message: %q", fs[0].Message)
	}
	// A healthy loader contributes nothing.
	if fs := (PolicyDoctorInspector{Loader: brokenLoader{exists: true}}).Findings(context.Background()); len(fs) != 0 {
		t.Fatalf("healthy loader produced findings: %+v", fs)
	}
}

func TestTargetDoctorInspectorFindings(t *testing.T) {
	symlinkErr := errors.New("wrapped: " + target.ErrSymlinkManagedFile.Error())
	// Symlink errors map to the definition-symlink code (via errors.Is).
	fs := TargetDoctorInspector{
		Loader:  brokenLoader{exists: true},
		Targets: failingRegistry{err: target.ErrSymlinkManagedFile},
	}.Findings(context.Background())
	if len(fs) != 1 || fs[0].Code != "definition-symlink" {
		t.Fatalf("symlink findings=%+v (raw err %v)", fs, symlinkErr)
	}
	// Other resolution failures use the generic code.
	fs = TargetDoctorInspector{
		Loader:  brokenLoader{exists: true},
		Targets: failingRegistry{err: errors.New("root missing")},
	}.Findings(context.Background())
	if len(fs) != 1 || fs[0].Code != "target-unresolvable" {
		t.Fatalf("generic findings=%+v", fs)
	}
	// A policy load error is left to the policy inspector.
	fs = TargetDoctorInspector{
		Loader:  brokenLoader{exists: true, err: errors.New("nope")},
		Targets: failingRegistry{err: errors.New("unreached")},
	}.Findings(context.Background())
	if len(fs) != 0 {
		t.Fatalf("load-error should defer to policy inspector: %+v", fs)
	}
}

func TestPublishDoctorInspectorFindings(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "apply.json")
	// No journal → no findings.
	if fs := (PublishDoctorInspector{JournalPath: journal}).Findings(context.Background()); len(fs) != 0 {
		t.Fatalf("missing journal produced findings: %+v", fs)
	}
	if err := os.WriteFile(journal, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := PublishDoctorInspector{JournalPath: journal}.Findings(context.Background())
	if len(fs) != 1 || fs[0].Code != "journal-incomplete" {
		t.Fatalf("journal findings=%+v", fs)
	}
}
