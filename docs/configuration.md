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

Durations are Go duration strings (`30s`, `10m`, `168h`) and must be
positive; a negative or zero `backup_count` is rejected at load.
