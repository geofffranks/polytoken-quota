package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
)

func TestAdapterSupportRecognizesAnthropic(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.AnthropicEvidence(now))
	status := adapterSupport("anthropic", now, reg)
	if !status.Supported {
		t.Fatalf("anthropic support=%+v, want supported", status)
	}
}

func TestAdapterSupportRecognizesNeuralwatt(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	reg := quota.NewEvidenceRegistry()
	reg.Register(quota.NeuralwattEvidence(now))
	status := adapterSupport("neuralwatt", now, reg)
	if !status.Supported {
		t.Fatalf("neuralwatt support=%+v, want supported", status)
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
