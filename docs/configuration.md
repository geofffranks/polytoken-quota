# Configuration reference

Every `polytoken-quota` setting lives in `~/.polytoken-quota/desired.yaml`.
The README shows the minimal shape; this page documents every key, its
default, and when to set it. All sections except `version`, `providers`, and
a target's `root` are optional.

```yaml
version: 1
providers:
  codex:
    models: [codex/gpt-5]
    quota:
      freshness_ttl: 30m
global:
  root: /home/user/.config/polytoken
  full: [codex/gpt-5]
```

## `version`

Required. Currently `1`. A missing or different version is rejected at load.

## `providers.<id>`

The mapping key `<id>` is the provider identity: state, history, and CLI
output all address the provider by it. When the mapping carries a `quota`
block, the key must also name a built-in quota adapter:

| Key | Adapter |
|-----|---------|
| `codex` | OpenAI Codex allowance polling |
| `zai` | Z.ai allowance polling |
| `anthropic` | Anthropic Admin API spend against `monthly_budget_usd` |
| `neuralwatt` | Neuralwatt Cloud quota endpoint |

A quota block under any other key is rejected at load with the valid names.
Supported non-Anthropic mappings may omit `quota` or use `quota: {}`; both forms
receive the adapter defaults and participate in polling/ranking. Anthropic may
omit `quota` or use `quota: {}` to remain visible but unpollable; it becomes
pollable only with a positive `monthly_budget_usd`. Unknown/manual mappings
without a supported quota adapter may use any key; they keep their configured
chain positions and are visible in diagnostics but are never quota-ranked or
polled.

### `models`

Required. The ownership boundary: only listed concrete models are managed.
Each model must be a concrete name (no `*` wildcards), owned by exactly one
mapping. Entries are either bare names or explicit records:

```yaml
models:
  - codex/gpt-5
  - codex/gpt-5.1: {enabled: false}
```

An explicit `enabled` records the durable baseline state for that model; a
bare name means enabled.

### `quota`

Optional. Enrolls the provider in quota polling and quota-based ranking.
There is no `adapter` field; the mapping key selects the adapter.

| Field | Default | When to set it |
|-------|---------|----------------|
| `monthly_budget_usd` | none | Required and positive for `anthropic`: the monthly spend ceiling treated as that provider's quota. Unused by the other adapters. |
| `freshness_ttl` | `30m` | How long a successful snapshot stays eligible for ranking. Raise it if you check less often than every 30 minutes. |
| `balance_group` | `default` | Providers are only ranked against others in the same group. Use to keep, say, a paid and a free provider from competing. |
| `weight` | `1` | Deterministic tie-break between providers otherwise ranked equal. Higher wins. |
| `schedule` | none (never off-peak) | Off-peak windows for ranking; see below. |

### `schedule`

Optional. Describes when the provider is at peak usage; outside those
windows the provider is treated as off-peak for ranking (off-peak providers
rank ahead of peak ones, all else equal).

```yaml
schedule:
  timezone: Asia/Singapore
  peak:
    - days: [mon, tue, wed, thu, fri]
      start: "14:00"
      end: "18:00"
```

- `timezone`: an IANA zone name; windows are interpreted in the provider's
  local time.
- `peak`: a list of windows, each with lowercase `days` (`mon` through
  `sun`), `start`, and `end` as `HH:MM`. `end` may be `24:00`. Windows must
  not cross midnight and are rejected at load if they do.

The legacy `off_peak` key is rejected with a pointer to `peak`.

## `global` and `projects`

`global` describes the global Polytoken configuration root; each entry in
`projects` registers an additional target. A project root is never
discovered or adopted unless it is listed.

| Field | Set it | Meaning |
|-------|-------|---------|
| `root` | always | Canonical Polytoken configuration root for the target; reconciliation has nothing to apply to without it. |
| `id` | projects | Target identifier used in output. |
| `full`, `mini`, `nano`, `classifier` | when you want default chains | Default chains for the target. Every entry must resolve to a model some mapping owns; reasoning suffixes like `codex/gpt-5(medium)` are allowed. |
| `definitions` | when managing facet/subagent files | Managed files. Each entry has `path` (relative to the target root) and `chain`, validated like the default chains. |

Only the enumerated chains and definition fields are managed; everything
else in those files is preserved byte-for-byte.

## `routing`

Optional. `enabled` defaults to `true`; an omitted section means routing is
on. Set it to opt out:

```yaml
routing:
  enabled: false
```

With routing disabled, reconciliation keeps the authored chain order.
Desired chains remain the baseline either way: disabling routing restores
the authored order.

## `operational`

Optional; every field also defaults individually, so a partial section is
valid.

| Field | Default | Meaning |
|-------|---------|---------|
| `validation_timeout` | `30s` | Budget for validating a staged reconcile candidate. |
| `lock_wait` | `10s` | How long to wait for the state mutation lock. |
| `recovered_retention` | `168h` | How long recovered-error history is kept. |
| `backup_count` | `5` | State backups retained. Must be at least 1. |
| `notice_path` | `~/.local/polytoken-quota/notice.json` | Where the reconciliation notice is published. The path must be visible inside agent containers for the in-session hook to converge (bind-mount it at the same path, or point it at an already-shared location). |
| `on_change` | none | Opt-in host-side actions run after a committed revision changed managed fields (see below). |

Durations are Go duration strings (`30s`, `10m`, `168h`) and must be
positive; a negative or zero `backup_count` is rejected at load.

### `on_change`

An optional list of actions executed by the host binary after a reconcile
commits a revision that changed at least one managed field. Each action is an
absolute executable invoked directly (no shell) with the notice JSON on
stdin, literal arguments, and a minimal sanitized environment (only `PATH`
and `HOME` plus the configured `env`) — provider credentials in the
environment never reach an action. Actions run after the state commit,
outside the mutation lock, at most once per revision, inside a 120s
aggregate budget; unstarted actions past the budget are skipped. Failures
(non-zero exit, timeout, spawn error, budget skip) are recorded as
`notice`-category `on-change-failed` events visible in
`polytoken-quota history` (notice publication failures are recorded as
`notice`-category `notice-publish`/`notice-render`/`notice-path` events) and
never change the reconcile's exit code. At most 16 actions, each with a
1–60s timeout (default 10s).

```yaml
operational:
  on_change:
    - run: /usr/local/bin/reconfigure-other-cli
      args: ["--scope", "global"]
      env: { CLI_CONFIG: /etc/cli.conf }
      timeout_seconds: 10
```

`run` must be an absolute path; `args` and `env` values are literal strings
with no interpolation of notice content. With no `on_change` configured,
nothing executes.

### Notice payload

The same JSON document is used for the notice file and as the `stdin` payload
for every `on_change` action. There is no additional envelope. A representative
payload is:

```json
{
  "schema": 1,
  "revision": 43,
  "published_at": "2026-08-16T02:00:05Z",
  "routing_enabled": true,
  "targets": [
    {
      "id": "global",
      "kind": "global",
      "chains": [
        {"name": "full", "models": ["codex/gpt-5.6-luna", null]},
        {"name": "mini", "models": ["minime/gemma-3-27b"]}
      ],
      "changed_fields": [["defaults", "full"]]
    },
    {
      "id": "work-api",
      "kind": "definition",
      "file": "subagents/work-api.md",
      "facet": "work-api",
      "chain": ["codex/gpt-5.6-luna", "zai/glm-4.6"],
      "changed_fields": [["polytoken", "model"]]
    }
  ],
  "disabled_models": ["zai/glm-5.2"]
}
```

Payload fields:

| Field | Meaning |
|---|---|
| `schema` | Notice schema version. Current value: `1`. |
| `revision` | The committed ptq state revision that caused the notice. |
| `published_at` | UTC publication timestamp in RFC 3339 format. |
| `routing_enabled` | Whether quota-based routing was enabled for the revision. |
| `targets` | Changed/effective model facts for the global target and managed definition targets. |
| `targets[].chains` | Global named chains such as `full`, `mini`, and `nano`. Each model is a Polytoken registry key; `null` means ptq could not resolve the model to a registry key. |
| `targets[].chain` | Effective chain for a managed definition target. |
| `targets[].changed_fields` | Managed key paths changed in the revision. Values are never included. |
| `targets[].facet` | Definition facet name when ptq can derive it from the managed definition path. |
| `disabled_models` | The standing set of models whose mapped provider is currently disabled, not merely models disabled by this particular revision. |

The notice is written only after a committed reconciliation changes at least
one managed file. It is written atomically with restrictive permissions. It
never contains provider credentials, auth values, raw command output, or
unmanaged configuration. If notice publication fails, ptq records a sanitized
notice event and does not change the reconciliation result.

For `on_change`, ptq invokes each configured executable directly with the
complete notice document above on standard input. The process environment is
limited to `PATH`, `HOME`, and the configured `env` additions. Actions run
after state commit, outside the mutation lock, and failures do not alter ptq's
exit code; inspect `polytoken-quota history` for failure events.
