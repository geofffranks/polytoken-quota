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

## Policy modes: legacy and provider-only

The `mode` key selects the policy grammar. It is optional:

- **Legacy** (no `mode` key, or `mode: legacy`): the full grammar documented on
  this page — provider mappings enumerate concrete `models`, targets carry
  desired chains (`full`/`mini`/`nano`/`classifier`) and `definitions`, and
  quota may reorder chains when `routing` is enabled.

  One legacy exception: when the target's configuration uses `modelgroups` — a
  top-level `modelgroups` key in the global `config.yaml` or any registered
  project layer's `config.yaml` — Polytoken rejects any candidate that combines
  a legacy tier default with an explicit model-group definition. Reconcile then
  leaves the tier defaults (`defaults.full`/`mini`/`nano` and the classifier
  pin, including routing-driven reorders of them) unwritten and operator-owned,
  reporting each skipped field under `reconcile --verbose`;
  `models.*.enabled` and facet/subagent fields are still managed. A
  configuration without `modelgroups` anywhere is written exactly as before.
- **`mode: provider-only`** (strictly opt-in): quota tracks enrolled Polytoken
  provider IDs and never edits models, chains, or definitions. Every other key
  on this page keeps its legacy meaning.

```yaml
version: 1
mode: provider-only
providers:
  codex:
    quota:
      adapter: codex
  team-llm: {}
global:
  root: /home/user/.config/polytoken
operational:
  notice_path: /shared/polytoken-quota/notice.json
```

In this mode, provider IDs are the unit of quota gating. During reserve or
exhausted/disabled quota, reconciliation disables the enrolled provider; when
quota returns to normal it restores only the exact baseline it previously
recorded and owned. It does not edit `modelgroups`, tier defaults, model enable
flags, or facet/subagent assignments, and it cannot promise which remaining
model Polytoken will serve. Group composition follows Polytoken's observed
behavior: same-name global leaves precede project leaves, with duplicate leaves
retained in their authored positions.

### Provider-only grammar

- `providers.<id>` enrolls the Polytoken provider ID `<id>`. IDs are enrolled
  verbatim — they do not need to name a quota adapter. A mapping may be empty
  (`id: {}`): the provider is visible in diagnostics but never polled.
- `providers.<id>.keep_enabled` optionally spares the provider from the
  automatic signal gate: with `keep_enabled: true` the gate never plans a
  disable for that provider, whatever its quota signal shows (default false
  when the key is absent). It constrains disabling only — a keep_enabled
  provider that is currently gated off is re-enabled on the next reconcile
  pass, regardless of signal recovery, and an explicit operator
  `routing disable`/`enable` overrides it.
- `providers.<id>.quota` optionally attaches quota polling. Unlike legacy
  mode, the quota block must name its adapter explicitly with `quota.adapter`
  (one of the built-in adapter names). The Anthropic rules carry over:
  `adapter: anthropic` requires `monthly_budget_usd` unless
  `mode: subscription` is set, and `adapter: anthropic-subscription` is always
  subscription mode (no budget, no `mode` key).
- `providers.<id>.quota.quota_gate: {enabled: false}` suspends automatic
  **quota** gating for that provider: an exhausted/unavailable observation
  never plans a disable, a quota claim holding the field off is restored on
  the next pass, and only an explicit operator disable turns the provider off.
  The signal gate is a separate axis and still applies. See
  [`quota_gate`](#quota_gate) for the full contract and the ranking/polling
  side effects of carrying a quota section.
- `global.root` is required: it is the single configuration root the
  provider-only policy targets.
- `projects` entries register additional roots with exactly `id` and `root` —
  the same registration grammar as legacy mode, and a first-class part of the
  provider-only gate: every registered root's composed candidate is staged and
  validated (config validate + doctor) before a disable publishes. Legacy
  project FIELDS are
  rejected in provider-only mode: a project entry carrying anything beyond
  `id` and `root` (for example `full`/`mini`/`nano`/`classifier` chains or
  `definitions`) fails to load.
- Legacy fields are **rejected** in provider-only mode, and the file fails to
  load: `models` under a provider, `full`/`mini`/`nano`/`classifier` chains and
  `definitions` on a target, and the top-level `routing` and `selection`
  sections. A mixed legacy/provider-only file never half-converts
  an installation.
- `operational` behaves exactly as in legacy mode.

### Provider-only command support

| Command | Provider-only behavior |
|---|---|
| `init --provider-only [--force]` | Create (or, with `--force`, replace) the policy enrolling the live global provider IDs. Never adopts model groups, models, chains, or definitions. |
| `init --provider-only --preview` | Read-only migration preview (see below). |
| `status` | Provider-level status with `provider_only: true`; route/chain sections are empty by design and the text view says so. |
| `check` | Polls enrolled, adapter-configured providers as usual. |
| `check --reconcile` | Poll and apply provider-only gating to enrolled global providers after the signal gate and staged validation pass. |
| `reconcile` | Apply provider-only gating without polling; provider state comes from the latest saved evidence. Supports `--dry-run`, `--keep-staging` (dry-run only), and `--verbose`. |
| `routing enable/disable <mapping-id>` | Maintained: writes the enrolled provider's `providers.<id>.enabled` field on the registered global target (staged, validated, and journaled like the automatic gate) and records/releases the durable manual-disable claim. A manual toggle survives later reconcile passes. Manual toggles override `keep_enabled` and `quota_gate`: an explicit operator action may disable a provider the automatic gate spares. |
| `routing reset` | Unsupported: clearing every manual disable has no provider-only meaning; enable providers individually with `routing enable <provider>`. |
| `doctor` | Maintained: policy schema, state, publication/journal, and quota findings work; no chain findings exist. |
| `history` | Maintained: state history is independent of the policy mode. |
| `select`, `select-eval` | Unsupported: model selection projects managed chains, which a provider-only policy does not define. |
| `install-hook` / `notice-hook` | Maintained: hook installation and the notice path are mode-independent. |

### Migrating a legacy policy to provider-only mode

`init --provider-only --force` over an existing legacy policy is a migration:

- The operator-authored `operational` section (timeouts, notice path,
  `on_change` actions, backup retention) is carried into the new policy.
- The replaced legacy policy file is preserved at
  `desired.yaml.before-provider-only` next to the original.
- The migration preview — printed by the command, and available in advance
  via `init --provider-only --preview` — reports:
  - the provider IDs the new policy enrolls and the global root it targets;
  - every legacy quota-authored edit that **persists as operator-owned**:
    previously managed `defaults.full`/`mini`/`nano`/`classifier` chains,
    definition chains, and `models.*` enablement stay in your Polytoken
    configuration exactly as quota left them. Provider-only quota never
    removes or rewrites those edits; they are yours to change or revert.
  - the managed-file backup root and apply journal paths (with whether files
    exist there yet) and the preserved policy path;
  - rollback guidance (restore the preserved policy file, or restore a
    managed-file backup, and rerun `init` without `--provider-only`).
- The migration only replaces the quota policy file. Polytoken configuration
  bytes are never touched.

A plain legacy `init --force` over a provider-only policy is rejected: leaving
provider-only mode is a deliberate migration, never a side effect.

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
| `opencode-go` | OpenCode Go usage-window polling (`OPENCODE_GO_API_KEY`, else OpenCode's `auth.json`) |

A quota block under any other key is rejected at load with the valid names.
Supported non-Anthropic mappings may omit `quota` or use `quota: {}`; both forms
receive the adapter defaults and participate in polling/ranking. Anthropic may
omit `quota` or use `quota: {}` to remain visible but unpollable; it becomes
pollable only with a positive `monthly_budget_usd`. Unknown/manual mappings
without a supported quota adapter may use any key; they keep their configured
chain positions and are visible in diagnostics but are never quota-ranked or
polled.

Provider note: `opencode-go` reports percentages rather than dollars, so it needs
no `monthly_budget_usd` and may omit `quota` or use `quota: {}`. Its credential is
the transient `OPENCODE_GO_API_KEY` when set, otherwise the key OpenCode itself
stored in its `auth.json` (the `opencode-go` entry, then `opencode`; the `key`
field only, read from an absolute path). Only when neither resolves does the
adapter fail closed and make no HTTP request. It polls three windows (`rolling`,
`weekly`, `monthly`) and is subject to the same fail-closed evidence gate as every
other adapter. See the
[OpenCode Go adapter](../README.md#opencode-go-adapter) section of the README.

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
| `mode` | `api` | Anthropic only. `api` (default) polls the Admin cost report against `monthly_budget_usd`. `subscription` is the experimental Claude-subscription source: it reads the Claude Code OAuth session (`.credentials.json` in `$CLAUDE_CONFIG_DIR` or `~/.claude`, or the macOS Keychain item `Claude Code-credentials`, read-only) and reports the plan's five-hour session and seven-day weekly utilization as `session` (300m) and `weekly` (10080m) windows. `subscription` forbids `monthly_budget_usd`. |
| `monthly_budget_usd` | none | Required and positive for `anthropic` in `api` mode: the monthly spend ceiling treated as that provider's quota. Unused by the other adapters and forbidden in `subscription` mode. |
| `freshness_ttl` | `30m` | How long a successful snapshot stays eligible for ranking. Raise it if you check less often than every 30 minutes. |
| `balance_group` | `default` | Providers are only ranked against others in the same group. Use to keep, say, a paid and a free provider from competing. |
| `weight` | `1` | Global tie-break between providers otherwise ranked equal. Higher wins. When signal cluster, schedule, and weight are all equal, providers share a routing rank and each route keeps its authored chain order. The signal is compared only when every eligible provider in the balance group can compute it. |
| `schedule` | none (never off-peak) | Off-peak windows for ranking; see below. |
| `quota_gate` | gating on | `{enabled: false}` suspends automatic quota gating for that provider: an exhausted/unavailable observation — durable axes or the latest poll — never plans a disable, and a provider quota had gated off is re-enabled on the next pass. The signal axis is separate (`signal_gate`/`keep_enabled` still apply); diagnostics keep reporting the raw out-of-quota state; only an explicit `routing disable` turns the provider off. See below for the prerequisites and side effects. |

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

#### `quota_gate`

Optional. `{enabled: false}` is the only non-default shape; it declares that
quota observations must not turn the provider off — for example during a
provider-side free-energy promo that the quota API cannot observe. The key is
accepted in both policy modes wherever a `quota` section is valid: under a
provider-only enrollment, and under a legacy mapping whose key names a known
quota adapter.

What it changes, and what it deliberately does not:

- **Gating only.** Exhausted/unavailable durable axes and the fail-closed poll
  boundary no longer plan a disable for that provider; a quota-axis claim
  already holding its enabled field off is restored and released on the next
  pass. Manual disables (`routing disable`) still apply, and a corrupted state
  axis still fails closed to disabled.
- **Observations stay truthful.** `status` and `doctor` keep reporting the raw
  out-of-quota axes — the exemption never launders the provider to healthy —
  and name the suspension as a separate policy fact: the status provider
  projection carries `quota_gate_suspended: true` (the merged status REASON
  ends with `; quota gating suspended`), and `doctor` emits a
  `quota-gate-suspended` info finding. `check` names the provider out of quota
  with gating suspended.
- **The signal axis is separate.** `signal_gate` and `keep_enabled` are
  unaffected in both directions: an exempt provider can still be signal-gated,
  and the all-hot pool escape hatch keeps counting an exempt member as enabled.
- **Ranking and `select` are unaffected.** An exempt out-of-quota provider is
  never ranked for reordering and never appears in `select` recommendations —
  it stays enabled-but-last in provider-only mode and in the reserve partition
  of managed chains.

Prerequisites and side effects: the key lives inside the `quota` section, so a
provider-only mapping enrolled **without** a quota section cannot express it.
In legacy mode, a mapping can carry a quota section only when its key is a
known adapter name, and **adding a quota section flips the mapping into
routing/ranking participation and scheduled polling** — if you add `quota_gate`
solely to suspend gating, that mapping will now be polled and ranked.

## `selection.jev`

Optional persistent consent and settings for task assessment. Absence disables remote assessment. This section belongs only in operator `desired.yaml` (the standard application home, overridden by `POLYTOKEN_QUOTA_HOME`), never in candidate policy.

```yaml
selection:
  jev:
    enabled: false
    model: jev-1.13.0
    timeout: 10s
```

`enabled` defaults to false; `model` is a versioned classifier pin; `timeout` must be a positive finite duration and defaults to 10s, not a latency SLA. Unknown keys in this new section are rejected. First initialization leaves it absent; forced initialization preserves it and refuses to overwrite unreadable or corrupt existing configuration.

Only the runtime process environment's `TYPESAFE_API_KEY` supplies the credential, immediately before an enabled remote call. There is no key or endpoint field. Do not place the key in the validation subprocess environment file; it is excluded from that forwarding path. The fixed endpoint is `https://api.typesafe.ai/v1/systemone`. Explicit `--difficulty` needs neither consent nor a credential and does not read stdin. See [selection](selection.md) for disclosure limits, retention caveats, strict candidate policy and the separately authorized live evaluation gate.

## `global` and `projects`

`global` describes the global Polytoken configuration root; each entry in
`projects` registers an additional target. A project root is never
discovered or adopted unless it is listed.

In provider-only mode `projects` entries carry exactly `id` and `root`:

```yaml
version: 1
mode: provider-only
providers:
  codex:
    keep_enabled: true
  zai: {}
global:
  root: /home/user/.config/polytoken
projects:
  - id: web-app
    root: /home/user/src/web-app
  - id: cli-tool
    root: /home/user/src/cli-tool/.polytoken
```

Each registered root's composed candidate is staged and validated (config
validate + doctor) before a disable publishes; register every root whose
configuration must be validated before the gate changes provider state.

| Field | Set it | Meaning |
|-------|-------|---------|
| `root` | always | Configuration root for the target. The global root is the Polytoken configuration directory (for example `~/.config/polytoken`). A project root may be the project directory: when the root itself holds no `config.yaml`, its `.polytoken` subdirectory is used as the configuration root automatically. A root with neither is rejected with a clear error. |
| `id` | projects | Target identifier used in output. |
| `full`, `mini`, `nano`, `classifier` | when you want default chains | Default chains for the target. Every entry must resolve to a model some mapping owns; reasoning suffixes like `codex/gpt-5(medium)` are allowed. |
| `definitions` | when managing facet/subagent files | Managed files. Each entry has `path` (relative to the target's configuration root, so a project registered at its project directory needs no `.polytoken/` prefix) and `chain`, validated like the default chains. |

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

Editing `enabled` in place preserves every other byte of `desired.yaml`.
YAML anchors, aliases, and merge keys elsewhere in the file are tolerated;
the edit is refused — with the file left unchanged — only when an anchor,
alias, merge key, or duplicate key involves `routing.enabled` or the
`routing` mapping itself (for example `routing: &r ...` or `enabled: *ref`),
including a `<<` merge key at the document root.

## `operational`

Optional; every field also defaults individually, so a partial section is
valid.

| Field | Default | Meaning |
|-------|---------|---------|
| `validation_timeout` | `30s` | Budget for validating a staged reconcile candidate. |
| `lock_wait` | `10s` | How long to wait for the state mutation lock. |
| `recovered_retention` | `168h` | How long recovered-error history is kept. |
| `backup_count` | `1` | Per-file pre-apply backups of managed files retained. Default 1; minimum 1; pruned oldest-first as files change. |
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

Provider-only policies publish a provider status document instead of model or
route projections:

```json
{
  "schema": 1,
  "revision": 43,
  "provider_only": true,
  "providers": [{"id": "codex", "enabled": false}]
}
```

It is published only after a committed provider-field change; conflict, failed
validation, recovery without a new edit, and no-edit reconciliation do not
produce a change notice. The existing schema-v1 route payload below is retained
for legacy policies.

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
