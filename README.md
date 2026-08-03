# polytoken-quota

`polytoken-quota` consumes CodexBar quota and provider-availability events, records durable state, and
reconciles explicitly managed Polytoken model fields. It supports one global Polytoken configuration
and registered project configurations.

## How it works

1. `hook` reads one CodexBar event from stdin and maps its provider to a configured provider mapping.
2. The reconciler updates durable quota/availability state and computes each target's desired model
   chains.
3. Candidate changes are rendered in an isolated staging root, then checked with `polytoken config
   validate` and `polytoken doctor`.
4. Only validated changes to managed fields are published. Apply journals and backups support recovery;
   `status` and `doctor` expose drift, pending targets, and failures.

The tool never controls a running Polytoken process. Existing sessions may need a user restart or
reload before they see reconciled choices. It never stores provider credentials or unrelated config.

## Minimum versions

| Component | Minimum | Notes |
|-----------|---------|-------|
| Go toolchain | `go1.26.5` | Exact version match (`go env GOVERSION`). |
| CodexBar | `0.44.0` | Hook contract. |
| Polytoken | `0.5.9` | Supported validation contract (resolved from `PATH`). |

## Install and initial setup

Build from this repository, or install the command into `$GOBIN` / `$GOPATH/bin`:

```sh
go build -o polytoken-quota ./cmd/polytoken-quota
# or:
go install ./cmd/polytoken-quota
```

Make sure the supported `polytoken` binary is installed and that your global Polytoken configuration
is valid. Then create the initial policy from the current managed state:

```sh
polytoken-quota init
```

This creates `~/.polytoken-quota/desired.yaml` and prints the CodexBar setup reminder. `init` is
create-only; to refresh an existing policy from live Polytoken fields, use:

```sh
polytoken-quota sync --from-polytoken
```

Review the generated policy before enabling hooks. Use `polytoken-quota doctor` for actionable
configuration, mapping, drift, and validation diagnostics.

## Configure CodexBar

In CodexBar's hook settings, register the same direct command for all six events below:

- `quota_low`
- `quota_reached`
- `quota_reset`
- `provider_unavailable`
- `provider_recovered`
- `refresh_failed`

Configure the hook as an executable plus the `hook` argument, not as a shell command. For example:

```text
/usr/local/bin/polytoken-quota hook
```

Use the absolute path to the installed binary. CodexBar supplies the event JSON on standard input;
`polytoken-quota hook` does not need provider credentials or event arguments. The tool does not edit
CodexBar settings automatically. With CodexBar 0.44.0 or later, `codexbar hooks test` can exercise
the configured delivery with a synthetic event.

## Configuration file

The policy file is `~/.polytoken-quota/desired.yaml` (mode `0600`). It is versioned YAML with four
main sections:

- `providers` maps CodexBar provider IDs to Polytoken provider IDs and explicitly enumerates the
  concrete models managed by each mapping. Model names must be exact; wildcards are rejected.
- `global` defines the global Polytoken root, default chains (`full`, `mini`, `nano`, `classifier`),
  and the definition files whose `polytoken.model` or `polytoken.fallback_models` fields are managed.
- `projects` registers additional targets with the same fields. A project root is not discovered or
  adopted unless it is listed here.
- `operational` controls validation timeout, lock wait, recovered-error retention, and backup count.
  If omitted, defaults are 30s, 10s, 168h, and 5 backups.

A minimal shape is:

```yaml
version: 1
providers:
  codex:
    codexbar_providers: [codex]
    polytoken_providers: [codex]
    models: [codex/gpt-5]
global:
  id: global
  root: /home/user/.config/polytoken
  full: [codex/gpt-5]
  definitions:
    - path: agents/research.md
      chain: [codex/gpt-5]
projects: []
operational:
  validation_timeout: 30s
  lock_wait: 10s
  recovered_retention: 168h
  backup_count: 5
```

The `models` list is the ownership boundary: only listed concrete models and the listed target
chains/definition fields are managed. Preserve unmanaged Polytoken settings outside those fields.
The `root` and definition paths identify the files to manage; use absolute paths when configuring
projects. `sync --from-polytoken` replaces the policy with current managed values, while `--force`
overrides its degraded/ambiguous-drift guard.

## Manual provider controls

When CodexBar does not reliably deliver a provider event, use the top-level manual controls:

```sh
polytoken-quota disable codex
polytoken-quota enable codex
polytoken-quota reset
```

`disable` and `enable` accept only providers configured in `desired.yaml`. A manual disable is
durable, takes precedence over automatic quota/availability state, and is not cleared by later hook
events. `enable` clears only that override and resumes the latest automatic state; the provider may
still be unavailable or out of quota. `reset` clears all manual overrides without erasing automatic
observations or their timestamps.

These commands reconcile all registered targets through the normal validation and publication path.
If a target cannot apply, the command prints its sanitized stage, summary, and remediation
immediately and exits `2` after persisting the pending failure for later `status`/`doctor` output.
Manual disables themselves are informational in `doctor`, and `status` reports
`reason=manual_disabled`. Existing Polytoken sessions may still need a user reload or restart.

## Commands

| Command | Description |
|---------|-------------|
| `init` | Create the initial `desired.yaml` from the current managed state. |
| `hook` | Read a CodexBar hook event from stdin and apply it. |
| `status [--json]` | Show current quota/availability state and drift. |
| `reconcile [--dry-run]` | Reconcile managed Polytoken fields toward desired state. |
| `sync --from-polytoken [--force]` | Sync desired state from the live Polytoken config. |
| `state set <provider> [--quota X] [--availability Y]` | Set a provider's managed automatic state. |
| `state clear <provider> \| --all` | Clear a provider's (or all) automatic state. |
| `disable <provider>` | Manually disable a configured provider. |
| `enable <provider>` | Clear a manual disable and resume automatic state. |
| `reset` | Clear all manual disables while preserving automatic observations. |
| `doctor [--json]` | Run health and drift diagnostics. |
