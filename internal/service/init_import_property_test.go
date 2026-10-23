package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/geofffranks/polytoken-quota/internal/policy"
	"os"
	"path/filepath"
	"testing"
)

func TestInitForcedImportPropertyHistory(t *testing.T) {
	ctx := context.Background()
	for caseIndex := 0; caseIndex < 12; caseIndex++ {
		t.Run(fmt.Sprintf("case-%02d", caseIndex), func(t *testing.T) {
			providerID := fmt.Sprintf("synthetic-provider-%02d", caseIndex)
			model := fmt.Sprintf("%s/model-%02d", providerID, caseIndex)
			definitionCount := 1 + caseIndex%4
			sources := generatedImportSources(providerID, model, definitionCount)
			old := []byte(fmt.Sprintf("version: 1\nproviders:\n  prior-%02d:\n    models: [prior/model]\n", caseIndex))
			coord, desiredPath, writer := newImportPropertyCoordinator(t, old, sources)
			var injected error
			switch caseIndex % 3 {
			case 0:
				injected = errors.New("synthetic global source failure")
				sources.globalErr = injected
			case 1:
				injected = errors.New("synthetic registered projects failure")
				sources.projectsErr = injected
			case 2:
				sources.global.Config.Providers = append(sources.global.Config.Providers, policy.SourceMapping{ID: providerID, Models: map[string]policy.ModelBaseline{model: {Enabled: true}}})
			}
			before, err := os.ReadFile(desiredPath)
			if err != nil {
				t.Fatal(err)
			}
			out := coord.InitWithOptions(ctx, InitOptions{Force: true})
			if out.Accepted || out.Error == nil {
				t.Fatalf("forced init=%+v want rejection", out)
			}
			if injected != nil && !errors.Is(out.Error, injected) {
				t.Fatalf("error %v does not wrap %v", out.Error, injected)
			}
			if writer.replaceCalls != 0 || writer.createCalls != 0 {
				t.Fatalf("writer calls replace/create=%d/%d", writer.replaceCalls, writer.createCalls)
			}
			after, err := os.ReadFile(desiredPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("rejected proposal changed desired bytes")
			}
		})
	}
	t.Run("forced ambiguity publishes once", func(t *testing.T) {
		providerID, model := "synthetic-provider-accept", "synthetic-provider-accept/model"
		sources := generatedImportSources(providerID, model, 3)
		sources.global.Config.Full = []string{"unmapped-provider/model"}
		old := []byte("version: 1\nproviders:\n  prior:\n    models: [prior/model]\n")
		coord, desiredPath, writer := newImportPropertyCoordinator(t, old, sources)
		out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
		if !out.Accepted {
			t.Fatalf("forced import=%+v", out)
		}
		if writer.replaceCalls != 1 || writer.createCalls != 0 {
			t.Fatalf("writer calls=%d/%d", writer.replaceCalls, writer.createCalls)
		}
		got, err := os.ReadFile(desiredPath)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := policy.Load(desiredPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := loaded.Providers[policy.MappingID(providerID)].Models[model]; !ok {
			t.Fatalf("replacement lost %q", model)
		}
		if bytes.Equal(got, old) {
			t.Fatal("forced import left old bytes")
		}
	})
	t.Run("precommit writer error preserves old bytes", func(t *testing.T) {
		sources := generatedImportSources("synthetic-provider-precommit", "synthetic-provider-precommit/model", 2)
		old := []byte("version: 1\nproviders:\n  prior:\n    models: [prior/model]\n")
		coord, path, writer := newImportPropertyCoordinator(t, old, sources)
		writer.replaceErr = errors.New("synthetic replacement failure")
		out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
		if out.Accepted || !errors.Is(out.Error, writer.replaceErr) {
			t.Fatalf("outcome=%+v", out)
		}
		if writer.replaceCalls != 1 || writer.createCalls != 0 {
			t.Fatalf("writer calls=%d/%d", writer.replaceCalls, writer.createCalls)
		}
		assertDesiredBytes(t, path, old)
	})
	t.Run("postcommit warning remains accepted", func(t *testing.T) {
		sources := generatedImportSources("synthetic-provider-warning", "synthetic-provider-warning/model", 2)
		old := []byte("version: 1\nproviders:\n  prior:\n    models: [prior/model]\n")
		coord, path, writer := newImportPropertyCoordinator(t, old, sources)
		warning := errors.New("synthetic postcommit cleanup warning")
		writer.warning = warning
		out := coord.InitWithOptions(context.Background(), InitOptions{Force: true})
		if !out.Accepted || !errors.Is(out.Error, warning) {
			t.Fatalf("outcome=%+v", out)
		}
		if writer.replaceCalls != 1 || writer.createCalls != 0 {
			t.Fatalf("writer calls=%d/%d", writer.replaceCalls, writer.createCalls)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, old) {
			t.Fatal("postcommit warning did not publish")
		}
	})
}

type generatedImportReader struct {
	global                 policy.SourceSet
	projects               []policy.SourceSet
	globalErr, projectsErr error
}

func (r *generatedImportReader) Global(context.Context) (policy.SourceSet, error) {
	return r.global, r.globalErr
}
func (r *generatedImportReader) Projects(context.Context) ([]policy.SourceSet, error) {
	return r.projects, r.projectsErr
}
func generatedImportSources(providerID, model string, definitionCount int) *generatedImportReader {
	definitions := make([]policy.SourceDefinition, definitionCount)
	for i := range definitions {
		definitions[i] = policy.SourceDefinition{Path: fmt.Sprintf("synthetic-definition-%02d.md", i), Model: model}
	}
	return &generatedImportReader{global: policy.SourceSet{ID: "global", Global: true, Root: "/synthetic/global", Config: policy.SourceConfig{Providers: []policy.SourceMapping{{ID: providerID, Models: map[string]policy.ModelBaseline{model: {Enabled: true}}}}}, Definitions: definitions}}
}

type importPropertyWriter struct {
	delegate                  policy.Writer
	replaceCalls, createCalls int
	replaceErr, warning       error
}

func (w *importPropertyWriter) CreateAtomic(ctx context.Context, d policy.Desired) (policy.PublicationResult, error) {
	w.createCalls++
	return w.delegate.CreateAtomic(ctx, d)
}
func (w *importPropertyWriter) ReplaceAtomic(ctx context.Context, d policy.Desired) (policy.PublicationResult, error) {
	w.replaceCalls++
	if w.replaceErr != nil {
		return policy.PublicationResult{}, w.replaceErr
	}
	result, err := w.delegate.ReplaceAtomic(ctx, d)
	if err != nil {
		return result, err
	}
	result.Warning = w.warning
	return result, nil
}
func newImportPropertyCoordinator(t *testing.T, initial []byte, sources *generatedImportReader) (*Coordinator, string, *importPropertyWriter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	spy := newCoordinatorSpy("desired-exists")
	writer := &importPropertyWriter{delegate: policy.NewWriter(path)}
	spy.Coordinator.Policy = FilePolicyLoader{Path: path}
	spy.Coordinator.PolicyWriter = writer
	spy.Coordinator.Sources = sources
	return &spy.Coordinator, path, writer
}
func assertDesiredBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("desired bytes changed: want=%q got=%q", want, got)
	}
}
