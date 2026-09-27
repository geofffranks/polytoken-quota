package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/doctor"
	"github.com/geofffranks/polytoken-quota/internal/service"
)

// initSpy extends depsSpy with provider-only init observation: the exact
// InitOptions passed through, an attachable migration preview, and a
// MigrationPreviewer implementation for the --preview path.
type initSpy struct {
	depsSpy
	gotOpts      service.InitOptions
	migration    *service.MigrationPreview
	previewCalls int
	preview      service.MigrationPreview
	previewErr   error
}

func (s *initSpy) Dependencies() Dependencies {
	return Dependencies{
		Mutator:         s,
		Diagnoser:       s,
		SnapshotBuilder: s,
		Previewer:       s,
	}
}

func (s *initSpy) InitWithOptions(_ context.Context, opts service.InitOptions) service.Outcome {
	s.Mutations++
	s.InitCalled = true
	s.gotOpts = opts
	return service.Outcome{Accepted: true, Migration: s.migration}
}

func (s *initSpy) PreviewProviderOnlyMigration(context.Context) (service.MigrationPreview, error) {
	s.previewCalls++
	return s.preview, s.previewErr
}

var _ MigrationPreviewer = (*initSpy)(nil)

func sampleMigrationPreview() *service.MigrationPreview {
	return &service.MigrationPreview{
		Mode:              "provider-only",
		EnrolledProviders: []string{"codex", "team-llm"},
		GlobalRoot:        "/home/user/.config/polytoken",
		LegacyOwnedEdits: []service.LegacyOwnedEdit{
			{TargetID: "global", File: "config.yaml", Field: "defaults.full", Detail: "1 chain entr(ies) stay as configured"},
			{TargetID: "global", File: "config.yaml", Field: "models", Detail: "provider \"codex\": 3 enumerated model baseline(s) stay as configured"},
		},
		BackupsPath:      "/home/user/.polytoken-quota/backups",
		BackupsPresent:   true,
		JournalPath:      "/home/user/.polytoken-quota/journal/apply.json",
		PolicyBackupPath: "/home/user/.polytoken-quota/desired.yaml.before-provider-only",
		Rollback: []string{
			"previously quota-authored defaults, definition chains, and model enabled flags stay in your Polytoken configuration as operator-owned edits",
		},
	}
}

// TestInitProviderOnlyFlagPassthrough proves init --provider-only forwards the
// opt-in mode to the mutator and renders the provider-only completion text.
func TestInitProviderOnlyFlagPassthrough(t *testing.T) {
	spy := &initSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--provider-only"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !spy.gotOpts.ProviderOnly || spy.gotOpts.Force {
		t.Fatalf("opts=%+v want provider-only without force", spy.gotOpts)
	}
	if !strings.Contains(stdout.String(), "provider-only desired.yaml created.") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// TestInitProviderOnlyMigrationSurfacesPreview proves a provider-only
// replacement surfaces the migration preview: the persisted legacy edits,
// backup/journal references, and rollback guidance.
func TestInitProviderOnlyMigrationSurfacesPreview(t *testing.T) {
	spy := &initSpy{migration: sampleMigrationPreview()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--provider-only", "--force"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !spy.gotOpts.ProviderOnly || !spy.gotOpts.Force {
		t.Fatalf("opts=%+v", spy.gotOpts)
	}
	out := stdout.String()
	for _, want := range []string{
		"provider-only migration preview:",
		"codex, team-llm",
		"legacy quota-authored edits that persist as operator-owned:",
		"defaults.full",
		"models",
		"backups",
		"apply journal",
		"before-provider-only",
		"rollback guidance:",
		"operator-owned",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview output missing %q:\n%s", want, out)
		}
	}
}

// TestInitPreviewIsReadOnlyPath proves init --preview invokes only the
// previewer (never the mutator), exits 0, and renders the preview.
func TestInitPreviewIsReadOnlyPath(t *testing.T) {
	spy := &initSpy{preview: *sampleMigrationPreview()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--provider-only", "--preview"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if spy.Mutations != 0 {
		t.Fatalf("preview mutated via the mutator %d time(s)", spy.Mutations)
	}
	if spy.previewCalls != 1 {
		t.Fatalf("previewer calls=%d want 1", spy.previewCalls)
	}
	if !strings.Contains(stdout.String(), "provider-only migration preview:") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// TestInitPreviewUnavailableRejected proves a missing previewer dependency is
// a clear rejection, not a silent success.
func TestInitPreviewUnavailableRejected(t *testing.T) {
	spy := &initSpy{}
	deps := spy.Dependencies()
	deps.Previewer = nil
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--preview"}, strings.NewReader(""), &stdout, &stderr, deps)
	if code != ExitRejected {
		t.Fatalf("exit=%d want rejection", code)
	}
	if !strings.Contains(stderr.String(), "migration preview unavailable") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if spy.Mutations != 0 {
		t.Fatal("unavailable preview fell through to the mutator")
	}
}

// TestInitPreviewErrorRejected proves a previewer error is surfaced.
func TestInitPreviewErrorRejected(t *testing.T) {
	spy := &initSpy{previewErr: errPreviewFailed{}}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--preview"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitRejected {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "preview failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

type errPreviewFailed struct{}

func (errPreviewFailed) Error() string { return "preview failed" }

// TestStatusTextNotesProviderOnlyMode proves the status text view states the
// provider-only mode explicitly instead of rendering chain sections as
// silently empty.
func TestStatusTextNotesProviderOnlyMode(t *testing.T) {
	spy := newDepsSpy()
	spy.StatusReportValue = service.MergedStatusReport{ProviderOnly: true, RoutingEnabled: false}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"status"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "provider-only") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "routes are empty by design") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// TestStatusJSONCarriesProviderOnly proves the status JSON envelope carries
// the provider-only flag for machine consumers.
func TestStatusJSONCarriesProviderOnly(t *testing.T) {
	spy := newDepsSpy()
	spy.StatusReportValue = service.MergedStatusReport{ProviderOnly: true}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"provider_only":true`) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// reconcileUnsupportedSpy returns the provider-only unsupported outcome from
// Reconcile.
type reconcileUnsupportedSpy struct {
	depsSpy
}

func (s *reconcileUnsupportedSpy) Dependencies() Dependencies {
	return Dependencies{Mutator: s, Diagnoser: s, SnapshotBuilder: s}
}

func (s *reconcileUnsupportedSpy) Reconcile(_ context.Context, _, _, _ bool) service.Outcome {
	s.Mutations++
	return service.Outcome{Accepted: false, Error: fmt.Errorf("wrapped: %w: %s",
		service.ErrProviderOnlyUnsupported, "reconcile provider gating is not implemented")}
}

// TestReconcileProviderOnlyMessageRendered proves the quiet reconcile renders
// the provider-only unsupported message on stderr (the quiet exit-code-only
// contract stays untouched for every other error).
func TestReconcileProviderOnlyMessageRendered(t *testing.T) {
	spy := &reconcileUnsupportedSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"reconcile"}, strings.NewReader(""), &stdout, &stderr, spy.Dependencies())
	if code != ExitRejected {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "provider-only policies do not support legacy chain management") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// Compile-time proof the spy satisfies the diagnoser surfaces it inherits.
var (
	_ service.Diagnoser       = (*initSpy)(nil)
	_ service.SnapshotBuilder = (*initSpy)(nil)
)

var _ = doctor.Report{} // keep the doctor import tied to the depsSpy contract
