package service

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// ProviderPreflight carries the source snapshots of one provider-gate
// assessment for the publish-time input recheck.
type ProviderPreflight struct {
	Snapshots []ProviderSourceSnapshot
}

// ProviderSourceSnapshot identifies one exact source file and its observed SHA-256.
type ProviderSourceSnapshot struct {
	TargetID string
	Path     string
	SHA256   [32]byte
}

// RecheckProviderPreflight verifies that every registered source file still has
// the bytes assessed when the gate plan was built. A changed, missing, or newly
// unreadable source refuses the result; callers must retry the gate.
func RecheckProviderPreflight(p ProviderPreflight) error {
	for _, snapshot := range p.Snapshots {
		data, err := os.ReadFile(snapshot.Path)
		if err != nil {
			return fmt.Errorf("service: preflight source changed or became unreadable for target %s", snapshot.TargetID)
		}
		if sha256.Sum256(data) != snapshot.SHA256 {
			return fmt.Errorf("service: preflight source changed for target %s; retry required", snapshot.TargetID)
		}
	}
	return nil
}

func validationTimeout(desired policy.Desired) time.Duration {
	if desired.Operational.ValidationTimeout > 0 {
		return desired.Operational.ValidationTimeout
	}
	return defaultValidationTimeout
}

func appendProviderSnapshots(result *ProviderPreflight, id, root string) error {
	configPath := filepath.Join(root, "config.yaml")
	paths := []string{configPath}
	files, err := policy.DiscoverManagedFiles(root)
	if err != nil {
		return fmt.Errorf("service: discover preflight source files for target %s", id)
	}
	for _, rel := range files {
		paths = append(paths, filepath.Join(root, filepath.FromSlash(rel)))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("service: read preflight source file for target %s", id)
		}
		result.Snapshots = append(result.Snapshots, ProviderSourceSnapshot{TargetID: id, Path: path, SHA256: sha256.Sum256(data)})
	}
	return nil
}
