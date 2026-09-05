package policy

import (
	"testing"
	"time"
)

const baseOnChangeYAML = "version: 1\nproviders: {a: {models: [codex/m]}}\n"

// TestLoadOnChangeActions: valid on_change entries parse with literal
// args/env and a defaulted per-action timeout; an omitted list stays empty.
func TestLoadOnChangeActions(t *testing.T) {
	t.Run("valid entries with defaults", func(t *testing.T) {
		yaml := baseOnChangeYAML + `operational:
  on_change:
    - run: /usr/local/bin/reconfigure
      args: ["--scope", "global"]
      env: {CLI_CONFIG: /etc/cli.conf}
    - run: /usr/local/bin/other
      timeout_seconds: 30
`
		d, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(d.Operational.OnChange) != 2 {
			t.Fatalf("on_change entries = %d, want 2", len(d.Operational.OnChange))
		}
		first := d.Operational.OnChange[0]
		if first.Run != "/usr/local/bin/reconfigure" || len(first.Args) != 2 || first.Args[0] != "--scope" {
			t.Fatalf("first = %+v", first)
		}
		if first.Env["CLI_CONFIG"] != "/etc/cli.conf" {
			t.Fatalf("first.Env = %v", first.Env)
		}
		if first.TimeoutSeconds != DefaultOnChangeTimeoutSeconds {
			t.Fatalf("default timeout = %d, want %d", first.TimeoutSeconds, DefaultOnChangeTimeoutSeconds)
		}
		if second := d.Operational.OnChange[1]; second.TimeoutSeconds != 30 {
			t.Fatalf("explicit timeout = %d, want 30", second.TimeoutSeconds)
		}
	})
	t.Run("omitted list stays empty", func(t *testing.T) {
		d, err := Load(writeTemp(t, baseOnChangeYAML+"operational: {backup_count: 2}\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(d.Operational.OnChange) != 0 {
			t.Fatalf("on_change = %v, want empty", d.Operational.OnChange)
		}
		if d.Operational.LockWait != 10*time.Second {
			t.Fatalf("other defaults intact: %+v", d.Operational)
		}
	})
}

// TestLoadOnChangeValidation: relative run paths, out-of-range timeouts, and
// more than 16 actions are policy errors at load time.
func TestLoadOnChangeValidation(t *testing.T) {
	cases := map[string]string{
		"relative run":     baseOnChangeYAML + "operational:\n  on_change:\n    - run: bin/reconfigure\n",
		"zero timeout":     baseOnChangeYAML + "operational:\n  on_change:\n    - run: /abs/bin/x\n      timeout_seconds: 0\n",
		"oversize timeout": baseOnChangeYAML + "operational:\n  on_change:\n    - run: /abs/bin/x\n      timeout_seconds: 61\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, yaml)); err == nil {
				t.Fatalf("accepted invalid on_change (%s)", name)
			}
		})
	}
	t.Run("more than sixteen actions", func(t *testing.T) {
		yaml := baseOnChangeYAML + "operational:\n  on_change:\n"
		for i := 0; i < MaxOnChangeActions+1; i++ {
			yaml += "    - run: /abs/bin/x" + string(rune('a'+i)) + "\n"
		}
		if _, err := Load(writeTemp(t, yaml)); err == nil {
			t.Fatal("accepted seventeen on_change actions")
		}
	})
	t.Run("exactly sixteen actions accepted", func(t *testing.T) {
		yaml := baseOnChangeYAML + "operational:\n  on_change:\n"
		for i := 0; i < MaxOnChangeActions; i++ {
			yaml += "    - run: /abs/bin/x" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "\n"
		}
		if _, err := Load(writeTemp(t, yaml)); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
}
