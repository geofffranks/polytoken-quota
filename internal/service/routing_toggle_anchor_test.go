package service

// Integration tests for SetRoutingEnabled against desired.yaml files that use
// YAML anchors. The routing toggle edits desired.yaml with
// document.EditScopedAmbiguity: anchors, aliases, merge keys, and duplicate
// keys that do not involve routing.enabled are tolerated and every byte
// outside the routing.enabled value is preserved; ambiguity on the edited
// path itself still refuses with document.IsAmbiguous and must leave the file
// untouched on disk.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/geofffranks/polytoken-quota/internal/document"
)

// writeDesiredFile writes content to a fresh desired.yaml in a temp dir and
// returns its path.
func writeDesiredFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write desired.yaml: %v", err)
	}
	return path
}

// readDesiredFile reads the file back, failing the test on I/O error.
func readDesiredFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read desired.yaml: %v", err)
	}
	return string(b)
}

// TestRoutingToggleToleratesAnchorsElsewhere proves the toggle succeeds on an
// anchored desired.yaml, changes only the routing.enabled value byte span, is
// idempotent for an already-set value, and that anchors/aliases, merge keys,
// and duplicate keys elsewhere are all tolerated.
func TestRoutingToggleToleratesAnchorsElsewhere(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"distant anchor and alias": {
			in: "version: 1\nproviders:\n  openai: &std\n    models:\n      - gpt-5\n  anthropic: *std\nrouting:\n  enabled: true\n",
			want: "version: 1\nproviders:\n  openai: &std\n    models:\n      - gpt-5\n  anthropic: *std\nrouting:\n  enabled: false\n",
		},
		"distant merge key": {
			in: "version: 1\ndefaults: &d\n  full: x\nother:\n  <<: *d\nrouting:\n  enabled: true\n",
			want: "version: 1\ndefaults: &d\n  full: x\nother:\n  <<: *d\nrouting:\n  enabled: false\n",
		},
		"distant duplicate keys": {
			in: "a: 1\na: 2\nrouting:\n  enabled: true\n",
			want: "a: 1\na: 2\nrouting:\n  enabled: false\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDesiredFile(t, tc.in)
			c := &Coordinator{Policy: FilePolicyLoader{Path: path}}
			if err := c.SetRoutingEnabled(context.Background(), false); err != nil {
				t.Fatalf("SetRoutingEnabled rejected distant ambiguity: %v", err)
			}
			if got := readDesiredFile(t, path); got != tc.want {
				t.Fatalf("toggle was not byte-local:\n got=%q\nwant=%q", got, tc.want)
			}
			// Idempotent no-op: setting the same value again succeeds and
			// leaves every byte identical.
			if err := c.SetRoutingEnabled(context.Background(), false); err != nil {
				t.Fatalf("idempotent SetRoutingEnabled failed: %v", err)
			}
			if got := readDesiredFile(t, path); got != tc.want {
				t.Fatalf("idempotent toggle changed bytes:\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

// TestRoutingToggleRefusesPathInvolvedAmbiguity proves the toggle still fails
// with document.IsAmbiguous when anchors, aliases, duplicates, or merge keys
// involve the routing.enabled edit path, and that the file on disk is left
// untouched.
func TestRoutingToggleRefusesPathInvolvedAmbiguity(t *testing.T) {
	cases := map[string]string{
		"anchor on routing value": "routing: &r\n  enabled: true\n",
		"anchor on enabled value": "routing:\n  enabled: &e true\n",
		"alias value for enabled": "defaults: &d true\nrouting:\n  enabled: *d\n",
		"duplicate routing keys":  "routing:\n  enabled: true\nrouting:\n  enabled: false\n",
		"duplicate enabled keys":  "routing:\n  enabled: true\n  enabled: false\n",
		"merge key in routing":    "defaults: &d {enabled: true}\nrouting:\n  <<: *d\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDesiredFile(t, content)
			c := &Coordinator{Policy: FilePolicyLoader{Path: path}}
			err := c.SetRoutingEnabled(context.Background(), false)
			if err == nil {
				t.Fatal("path-involved ambiguity was accepted")
			}
			if !document.IsAmbiguous(err) {
				t.Fatalf("refusal not classified ambiguous: %v", err)
			}
			if got := readDesiredFile(t, path); got != content {
				t.Fatalf("refused toggle mutated the file:\n got=%q\nwant=%q", got, content)
			}
		})
	}
}
