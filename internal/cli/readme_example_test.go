package cli

// The README's Configuration section advertises a minimal desired.yaml. This
// test extracts that fenced example and proves it is exactly what it claims:
// loadable by policy.Load with the documented defaults and nothing extra.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
)

// readmeExampleYAML returns the first ```yaml fenced block in README.md.
func readmeExampleYAML(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	const fence = "```yaml"
	start := strings.Index(string(raw), fence)
	if start < 0 {
		t.Fatal("README.md has no ```yaml fenced block")
	}
	rest := string(raw)[start+len(fence):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("README.md yaml block is unterminated")
	}
	return rest[:end]
}

// TestReadmeMinimalExampleLoads proves the README minimal example loads with
// the simplified defaults: routing enabled without a routing section, adapter
// derived from the mapping key, and operational defaults without a section.
func TestReadmeMinimalExampleLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.yaml")
	if err := os.WriteFile(path, []byte(readmeExampleYAML(t)), 0o600); err != nil {
		t.Fatalf("write example: %v", err)
	}
	d, err := policy.Load(path)
	if err != nil {
		t.Fatalf("README example failed to load: %v", err)
	}
	if !d.Routing.Enabled {
		t.Error("README example should load with routing enabled by default")
	}
	if d.Operational.BackupCount != 5 || d.Operational.ValidationTimeout != 30*time.Second {
		t.Errorf("operational defaults not applied: %+v", d.Operational)
	}
	if q := d.Providers["codex"].Quota; q == nil || q.Adapter != "codex" {
		t.Errorf("codex quota=%+v want adapter from mapping key", q)
	}
	if q := d.Providers["anthropic"].Quota; q == nil || q.Adapter != "anthropic" || q.MonthlyBudgetUSD != 250 {
		t.Errorf("anthropic quota=%+v want mapping-key adapter and budget", q)
	}
	if q := d.Providers["codex"].Quota; q.FreshnessTTL != 30*time.Minute || q.BalanceGroup != "default" || q.Weight != 1 {
		t.Errorf("quota defaults not applied: %+v", q)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "docs", "configuration.md")); err != nil {
		t.Errorf("docs/configuration.md does not exist: %v", err)
	}
	if !strings.Contains(readmeRaw(t), "docs/configuration.md") {
		t.Error("README does not link to docs/configuration.md")
	}
}

// readmeRaw returns README.md's full text for link assertions.
func readmeRaw(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(raw)
}
