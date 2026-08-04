# polytoken-quota

`polytoken-quota` polls provider quota directly, records durable sanitized state, and reconciles explicitly managed Polytoken model fields. It supports one global Polytoken configuration and registered project configurations.

Quota polling and runtime routing are opt-in. When enabled, the tool ranks configured providers by fresh quota headroom, availability, balance group, weight, and optional off-peak schedules, then applies the resulting order through the normal validated reconciliation flow. Existing sessions may need a user restart or reload before they see updated choices.

The tool never controls a running Polytoken process, stores provider credentials, or persists raw provider responses, auth headers, or account IDs.

## Minimum versions

| Component | Minimum | Notes |
|-----------|---------|-------|
| Go toolchain | `go1.26.5` | Exact version match (`go env GOVERSION`). |
| Polytoken | `0.5.9` | Supported validation contract (resolved from `PATH`). |

## Install and initial setup

Build from this repository, or install the command into `$GOBIN` / `$GOPATH/bin`:

```sh
go build -o polytoken-quota ./cmd/polytoken-quota
# or:
go install ./cmd/polytoken-quota
```

Make sure the supported `polytoken` binary is installed and that your global Polytoken configuration is valid. Then create the initial policy from the current managed state:

```sh
polytoken-quota init
```

This creates `~/.polytoken-quota/desired.yaml`. `init` is create-only; to refresh an existing policy from live Polytoken fields, use:

```sh
polytoken-quota sync --from-polytoken
```

Review the generated policy before enabling quota polling or routing. Use `polytoken-quota doctor` for actionable configuration, mapping, quota, drift, and validation diagnostics.

## Configuration

All `polytoken-quota` configuration lives in:

```text
~/.polytoken-quota/desired.yaml
```

The file is versioned YAML and is created by `polytoken-quota init`. It has four main sections:

- `providers` maps provider IDs and enumerates the exact concrete models managed by each mapping.
- `global` defines the global Polytoken root, default chains (`full`, `mini`, `nano`, `classifier`), and the definition files whose `polytoken.model` or `polytoken.fallback_models` fields are managed.
- `projects` registers additional targets with the same fields. A project root is not discovered or adopted unless it is listed here.
- `operational` controls validation timeout, lock wait, recovered-error retention, and backup count. If omitted, defaults are 30s, 10s, 168h, and 5 backups.

Quota polling and routing use the same `desired.yaml` file. Add a `quota` block under each participating `providers.<id>` mapping, then enable routing with the top-level `routing.enabled` flag. A mapping without `quota` remains outside quota polling and routing.

A complete minimal shape is:

```yaml
version: 1
providers:
  codex:
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models:
      - codex/gpt-5
    quota:
      adapter: codex
      freshness_ttl: 30m
      balance_group: primary
      weight: 2
      schedule:
        timezone: Asia/Singapore
        peak:
          - days: [mon, tue, wed, thu, fri]
            start: "14:00"
            end: "18:00"
  zai:
    codexbar_providers: [zai]
    polytoken_providers: [zai]
    models:
      - zai/glm-4.5
    quota:
      adapter: zai
      freshness_ttl: 30m
      balance_group: reserve
      weight: 1

global:
  id: global
  root: /home/user/.config/polytoken
  full: [codex/gpt-5, zai/glm-4.5]
  definitions:
    - path: agents/research.md
      chain: [codex/gpt-5, zai/glm-4.5]
projects: []
routing:
  enabled: true
operational:
  validation_timeout: 30s
  lock_wait: 10s
  recovered_retention: 168h
  backup_count: 5
```

The `models` list is the ownership boundary: only listed concrete models and the listed target chains/definition fields are managed. Preserve unmanaged Polytoken settings outside those fields. Model entries may be bare names, as shown above, or explicit mappings such as `codex/gpt-5: {enabled: true}`.

Quota fields are:

- `adapter`: `codex` or `zai`.
- `freshness_ttl`: how long a successful snapshot remains eligible; defaults to `30m`.
- `balance_group`: isolates ranking comparisons between provider groups; defaults to `default`.
- `weight`: deterministic tie-break weight; defaults to `1`.
- `schedule`: optional IANA timezone and `peak` windows. Each window has lowercase `days` (`mon` through `sun`), `start`, and `end`. Peak windows are written in the provider's local timezone; outside them the provider is treated as off-peak for ranking.

`routing.enabled` defaults to `false`. Enabling it changes only the effective managed order; the desired chains in `desired.yaml` remain the user-authored baseline and are restored when routing is disabled. `sync --from-polytoken` replaces the policy with current managed values, while `--force` overrides its degraded/ambiguous-drift guard.

## Commands

| Command | Description |
|---------|-------------|
| `init` | Create the initial `desired.yaml` from the current managed state. |
| `sync --from-polytoken [--force]` | Sync desired state from the live Polytoken config. |
| `reconcile [--dry-run]` | Reconcile managed Polytoken fields toward desired state. |
| `quota check [--provider ID] [--json] [--reconcile]` | Poll quota once; optionally filter a mapping, emit JSON, and reconcile after saving observations. |
| `quota status [--json]` | Show quota snapshots, windows, attempts, and routing metadata. |
| `routing explain [--json]` | Show provider ranking and explanations. |
| `routing enable` | Set `routing.enabled: true` in `desired.yaml`. |
| `routing disable` | Set `routing.enabled: false` in `desired.yaml`. |
| `status [--json]` | Show current provider availability, drift, pending targets, and routing state. |
| `doctor [--json]` | Run configuration, quota, drift, pending-journal, and validation diagnostics. |
| `hook` | Read a legacy hook event from standard input and apply it. |
| `state set <provider> [--quota X] [--availability Y]` | Set a provider's managed automatic state. |
| `state clear <provider> \| --all` | Clear a provider's, or all providers', automatic state. |
| `disable <provider>` | Manually disable a configured provider. |
| `enable <provider>` | Clear a manual disable and resume automatic state. |
| `reset` | Clear all manual disables while preserving automatic observations. |

Exit codes are `0` for an accepted clean result, `1` for a rejected request or diagnostic failure, and `2` for an accepted operation with a pending provider, quota, target, or validation problem. `quota check --json` emits one sanitized structured envelope for accepted and rejected requests.

## Quota polling and routing

Quota polling and routing are disabled by default. Run a one-shot check manually or schedule it with an external scheduler:

```sh
polytoken-quota quota check
polytoken-quota quota status
polytoken-quota routing explain
```

When routing is enabled, a successful fresh snapshot can make a provider eligible; stale, unavailable, unknown, partial-without-usable-data, and missing alias observations fail closed. Peak windows are expressed once, for example Monday–Friday 14:00–18:00 in `Asia/Singapore`; all other times are off-peak for ranking. Provider failures preserve the last good snapshot and are reported by `status`, `quota status`, and `doctor`.

The utility does not install, start, stop, or control timers. Set up scheduling manually and choose a cadence permitted by each provider. If desired, add jitter in the external scheduler or wrapper so multiple machines do not poll at once.

Example launchd user agent (adjust the binary and utility-home paths):

```xml
<!-- ~/Library/LaunchAgents/com.example.polytoken-quota.plist -->
<plist version="1.0"><dict>
  <key>Label</key><string>com.example.polytoken-quota</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/polytoken-quota</string><string>quota</string><string>check</string>
  </array>
  <key>StartInterval</key><integer>1800</integer>
</dict></plist>
```

Example systemd user timer:

```ini
# ~/.config/systemd/user/polytoken-quota.service
[Service]
Type=oneshot
ExecStart=%h/go/bin/polytoken-quota quota check

# ~/.config/systemd/user/polytoken-quota.timer
[Timer]
OnBootSec=5m
OnUnitActiveSec=30m
Persistent=true
```

Example cron entry (run `crontab -e`):

```cron
*/30 * * * * /usr/local/bin/polytoken-quota quota check
```

Changing quota policy or enabling routing may change the choices seen by existing Polytoken sessions. The utility never controls those sessions; a user restart or reload may be required. Quota polling never persists credentials, raw provider responses, auth headers, or account IDs. Only bounded, sanitized quota observations and error summaries are stored in the utility state.
