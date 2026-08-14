# polytoken-quota

`polytoken-quota` polls provider quota directly, records durable sanitized state, and reconciles explicitly managed Polytoken model fields. It supports one global Polytoken configuration and registered project configurations.

Quota polling and runtime routing are opt-in. When enabled, the tool ranks configured providers by quota projection pace (how fast each is burning its quota relative to its reset cycle), availability, balance group, off-peak schedules, and weight, then applies the resulting order through the normal validated reconciliation flow.

The tool never controls a running Polytoken process, stores provider credentials, or persists raw provider responses, auth headers, or account IDs.

## Minimum versions

| Component | Minimum | Notes |
|-----------|---------|-------|
| Go toolchain | `go 1.26.5` | Exact version match (`go env GOVERSION`). |
| Polytoken | `0.6.6` | Supported validation contract (resolved from `PATH`). |

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

This creates `~/.polytoken-quota/desired.yaml`. `init` without `--force` refuses to overwrite a valid existing file. To replace a valid existing policy with one generated from the current managed state, use:

```sh
polytoken-quota init --force
```

Review the generated policy before enabling quota polling or routing. Use `polytoken-quota doctor` for actionable configuration, mapping, quota, drift, and validation diagnostics.

## Release downloads

Releases, when published, provide one archive for each supported target:

- `polytoken-quota-darwin-arm64.tar.gz`
- `polytoken-quota-darwin-amd64.tar.gz`
- `polytoken-quota-linux-arm64.tar.gz`
- `polytoken-quota-linux-amd64.tar.gz`

Download all five release assets (the four archives and `checksums.txt`) from the same GitHub release before verifying the archive:

```sh
sha256sum --check checksums.txt
# macOS:
shasum -a 256 -c checksums.txt
```

The archive contains the `polytoken-quota` executable and a `VERSION` file. Extract it and place the executable somewhere on `PATH`, for example:

```sh
tar -xzf polytoken-quota-linux-amd64.tar.gz
install -m 0755 polytoken-quota ~/.local/bin/polytoken-quota
./polytoken-quota --version
```

Release builds embed the `vX.Y.Z` release tag in the executable, and the same tag is recorded in `VERSION`; `--version` reports that embedded version. No release is assumed to exist yet: if no release download is available, build from the repository with the commands above.

## Configuration

All `polytoken-quota` configuration lives in:

```text
~/.polytoken-quota/desired.yaml
```

The file is versioned YAML and is created by `polytoken-quota init`. It has four main sections:

- `providers.<id>` is the provider mapping identity and enumerates the exact concrete models managed by that mapping.
- `global` defines the global Polytoken root, default chains (`full`, `mini`, `nano`, `classifier`), and the definition files whose `polytoken.model` or `polytoken.fallback_models` fields are managed.
- `projects` registers additional targets with the same fields. A project root is not discovered or adopted unless it is listed here.
- `operational` controls validation timeout, lock wait, recovered-error retention, and backup count. If omitted, defaults are 30s, 10s, 168h, and 5 backups.

Quota polling and managed routing use the same `desired.yaml` file. Add a `quota` block under each quota-participating `providers.<id>` mapping, then enable quota-based reordering with the top-level `routing.enabled` flag. The `status` command shows only mappings with a `quota` block. Mappings without a `quota` block remain managed routing participants: they keep their configured chain positions, are not quota-ranked, and still honor explicit disable or unavailable state.

A complete minimal shape is:

```yaml
version: 1
providers:
  codex:
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

- `adapter`: `codex`, `zai`, `anthropic`, or `neuralwatt`.
- `monthly_budget_usd`: required for (and only used by) the `anthropic` adapter — the monthly spend ceiling treated as that provider's quota. Neuralwatt uses its provider-reported USD balance and does not require this field.
- `freshness_ttl`: how long a successful snapshot remains eligible; defaults to `30m`.
- `balance_group`: isolates ranking comparisons between provider groups; defaults to `default`.
- `weight`: deterministic tie-break weight; defaults to `1`.
- `schedule`: optional IANA timezone and `peak` windows. Each window has lowercase `days` (`mon` through `sun`), `start`, and `end`. Peak windows are written in the provider's local timezone; outside them the provider is treated as off-peak for ranking.

### Neuralwatt adapter

The `neuralwatt` adapter polls Neuralwatt Cloud's read-only quota endpoint (`GET /v1/quota`) with a transient `NEURALWATT_API_KEY` Bearer credential. It selects the first present boundary in this order: key-specific allowance, subscription energy allowance, then provider-reported USD credit balance for PAYG accounts. A present but malformed boundary fails closed rather than falling back to a weaker signal. A blocked key, subscription overage, exhausted balance, authentication failure, or missing/invalid selected limit is never treated as healthy.

The adapter reports the selected provider boundary as one routing window. Usage and energy totals are retained only as provider diagnostics in the response contract; they are not used as a synthetic quota when no enforceable allowance or balance is available. The account balance path does not invent a reset time when the provider does not report one.

### Anthropic adapter

The `anthropic` adapter is for pay-as-you-go Anthropic **API** accounts
(Claude subscription plans expose no usable API and are not supported). A
metered API has no token allowance to deplete — the meaningful quota is the
money you are willing to spend. The adapter therefore polls the Admin API
cost report (`GET /v1/organizations/cost_report`, read-only) for the
organization's month-to-date spend and reports it against your
`monthly_budget_usd` as a single monthly window: at 100% the provider is
treated as exhausted and routing demotes it, exactly like a depleted codex or
z.ai allowance. The window resets at the first of each month (UTC).

- Credentials: `ANTHROPIC_ADMIN_API_KEY` — an **Admin** key (`sk-ant-admin…`,
  created in Console → Settings → Admin keys; a standard API key gets 401 on
  the Admin API). Resolved transiently per check and never persisted.
- Spend visibility is coarse: the cost report only supports daily (UTC)
  buckets and omits the still-open day until the UTC day closes, so the
  reported month-to-date spend can trail reality by up to a day's usage. Set
  `monthly_budget_usd` with at least a heavy day's margin below your true
  ceiling. The default 30m `freshness_ttl` is fine.
- `monthly_budget_usd` is your number, known only to this tool — Anthropic
  never sees it, and no Anthropic API exposes the account's real limits (the
  usage-tier spend cap, a console-configured spend limit, or the prepaid
  credit balance are all console-only). The two mechanisms fail differently:
  Anthropic *enforces* its own limits by hard-failing API requests once you
  hit them, while this tool watches spend against `monthly_budget_usd` and
  demotes the provider in your model chains *before* that happens. Set it
  comfortably below the enforced ceiling so routing steers away from
  Anthropic instead of sessions slamming into request errors.
- Anthropic's per-minute rate limits are deliberately not polled: they refill
  in about a minute, so a scheduled snapshot of them carries no
  session-start routing signal.

`routing.enabled` defaults to `false`. Enabling it changes only the effective managed order; the desired chains in `desired.yaml` remain the user-authored baseline and are restored when routing is disabled. There is no mutation command for `routing.enabled`; set it directly in the YAML file.

## Commands

| Command | Description |
|---------|-------------|
| `init [--force]` | Create `desired.yaml` from current managed state. `--force` overwrites a valid existing file. |
| `status [--json]` | Show the merged quota and routing view: routing enablement, one global last-checked time, per-provider consolidated status (`disabled`/`unavailable`/`available`/`enabled`), raw per-window quota numbers, next resets, effective routes with skip reasons, and a pending-config warning pointing at `doctor`. |
| `check [--provider <id>] [--reconcile] [--json] [--quiet]` | Poll quota once; optionally filter a mapping, reconcile after saving, emit JSON, or suppress all output (for cron/launchd/systemd). |
| `reconcile [--dry-run [--keep-staging]]` | Reconcile managed Polytoken fields toward desired state. `--keep-staging` (dry-run only) retains a failed validation candidate's staging root for inspection; the retained path is printed and the caller owns deleting it (it may contain merged configuration). |
| `routing enable <mapping-id>` | Enable a provider mapping (clear manual disable). |
| `routing disable <mapping-id>` | Disable a provider mapping (hard exclusion). |
| `routing reset` | Clear all manual disables while preserving automatic observations. |
| `doctor [--json]` | Run configuration, quota, journal, and persisted-error diagnostics. |
| `history [--limit N] [--revision N] [--json]` | Show the meaningful provider/routing event timeline. `--limit` (1–100, default 20) limits event rows; `--revision` shows all events for one revision; `--json` emits deterministic structured events. |

Exit codes are `0` for an accepted clean result, `1` for a rejected request or diagnostic failure, and `2` for an accepted operation with a pending provider, quota, target, or validation problem. `check --json` and `status --json` emit one sanitized structured envelope for accepted and rejected requests.

## Meaningful event history

`polytoken-quota history` is a newest-first timeline of meaningful provider and routing events, not a generic reconcile counter. It records quota low/reached/reset transitions, provider failures/recoveries, manual disable/enable/reset actions, ignored stale hooks, quota-poll failures, and routing changes such as rank, eligibility, pace explanation, or peak/off-peak changes. Unchanged quota observations and unchanged routing decisions are suppressed.

```text
EVENT HISTORY
Reported at: 2026-08-14 02:30:00 UTC

WHEN                 PROVIDER   EVENT                    RESULT
2026-08-14 02:22:35  zai        quota_reached             disabled; removed from managed chains
2026-08-13 14:04:03  codex      routing_changed           rank 1 -> 3; over pace
2026-08-13 10:19:59  zai        provider_recovered        available; quota remains exhausted
2026-08-13 10:15:00  zai        quota_low                 IGNORED; stale quota event
```

```sh
polytoken-quota history                    # newest 20 meaningful events
polytoken-quota history --limit 50         # newest 50 event rows
polytoken-quota history --revision 42      # all events attached to revision 42
polytoken-quota history --json             # deterministic structured event JSON
```

`--revision` is the forensic view for one state revision and includes every event recorded for that revision. Valid stale or duplicate hooks are durably recorded as `IGNORED` audit events without changing provider state or target files and return exit `0` after the event is saved. Malformed, contradictory, unknown, or ambiguous hook input is rejected with exit `1`. Accepted operations with pending targets or provider problems retain exit `2`.

History queries are strictly read-only: they load `state.json` without acquiring the mutation lock, running recovery, reading live Polytoken files, or saving. On the first later mutating command after the schema upgrade, obsolete reconcile-history records are discarded and the new event-history model is written; operational provider state, manual controls, routing snapshots, and target outcomes are preserved. The history query itself never rewrites or deletes state.

The event timeline is bounded and atomically persisted inside `state.json`. It never stores credentials, account names, auth values, raw errors, complete files, staging roots, arbitrary paths, or process information. Identifiers, explanations, statuses, and failure details are sanitized before save and display.

## Quota polling and routing

Quota polling and routing are disabled by default. Run a one-shot check manually or schedule it with an external scheduler:

```sh
polytoken-quota check --reconcile
polytoken-quota status
```

`status` is the single read surface for quota and routing state:

```
routing: enabled    last checked: 2026-08-14 09:12 UTC

PROVIDER    STATUS      QUOTA                     NEXT RESET
zai         available   5h 41/80, weekly 120/400  2026-08-15 00:00 UTC
minime      enabled     no data                   —

ROUTE           DESIRED                    EFFECTIVE     REASON
global          glm-4.6, gpt-5.2, sonnet   glm-4.6       gpt-5.2 skipped: quota exhausted; sonnet skipped: manual disable
work-api        glm-4.6                    glm-4.6

warning: 1 target(s) pending — shown values may not be live; run polytoken-quota doctor
```

Provider STATUS consolidates the axes: `disabled` (manual `routing disable`) wins over everything; a provider with no quota observation yet shows `enabled`; otherwise the availability axis decides `available`/`unavailable`. Quota exhaustion shows in the raw window numbers and the route REASON, not STATUS. The route REASON lists each desired model dropped from the effective chain and why (`manual disable`, `unavailable`, `quota exhausted`). `status --json` emits the same data as one sanitized envelope (raw window numbers, `skipped` arrays, `pending_targets`, `problem`); exit codes are `1` for a fatal error or failed route projection and `2` for actionable quota problems.

Use `check --reconcile` when scheduled runs should apply the fresh routing decision to the live managed configs. Without `--reconcile`, `check` refreshes quota state only. In interactive use `check` prints each provider's polling status; pass `--quiet` in cron, launchd, or systemd timers to suppress all output (exit codes still reflect success or failure).

When routing is enabled, a successful fresh snapshot can make a provider eligible; stale, unavailable, unknown, partial-without-usable-data, and missing alias observations fail closed. Peak windows are expressed once, for example Monday–Friday 14:00–18:00 in `Asia/Singapore`; all other times are off-peak for ranking. Provider failures preserve the last good snapshot and are reported by `status` and `doctor`.

Routing uses a deterministic lexicographic ranking, not a blended score:

1. Providers must be eligible: their mode is `normal` or `reserve`, their snapshot is fresh, and it contains usable remaining quota.
2. Eligible providers stay grouped by `balance_group`; groups appear in their first configured order and do not interleave.
3. Within each group, providers are ranked by **projection pace** — the ratio of used-fraction to elapsed-fraction of their quota window. Lower pace ranks first (under-utilized providers catch up; over-utilized ones ease back). Providers within 10% absolute pace are treated as tied. Pace is computed from each provider's longest qualifying quota window (period + reset + remaining, minimum one day). Ties break by off-peak before peak, then higher `weight`, then alphabetical provider ID.
4. If either provider in a pair cannot compute a pace (no qualifying window), the pace comparison is skipped for that pair. Ineligible providers remain at the end and are never disabled by routing.

For example, if `codex` and `zai` are both eligible in the same balance group and `codex` is burning its weekly quota faster than `zai` (higher pace), `zai` ranks first to balance depletion. If both are within 10% on pace, the off-peak provider wins; if both are in the same schedule state, weight and provider ID decide the tie.

The utility does not install, start, stop, or control timers. Set up scheduling manually and choose a cadence permitted by each provider. If desired, add jitter in the external scheduler or wrapper so multiple machines do not poll at once.

Example launchd user agent (adjust the binary and utility-home paths):

```xml
<!-- ~/Library/LaunchAgents/com.example.polytoken-quota.plist -->
<plist version="1.0"><dict>
  <key>Label</key><string>com.example.polytoken-quota</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/polytoken-quota</string><string>check</string><string>--reconcile</string><string>--quiet</string>
  </array>
  <key>StartInterval</key><integer>1800</integer>
</dict></plist>
```

Example systemd user timer:

```ini
# ~/.config/systemd/user/polytoken-quota.service
[Service]
Type=oneshot
ExecStart=%h/go/bin/polytoken-quota check --reconcile --quiet

# ~/.config/systemd/user/polytoken-quota.timer
[Timer]
OnBootSec=5m
OnUnitActiveSec=30m
Persistent=true
```

Example cron entry (run `crontab -e`):

```cron
*/30 * * * * /usr/local/bin/polytoken-quota check --reconcile --quiet
```

Changing quota policy or enabling routing may change the choices seen by existing Polytoken sessions. The utility never controls those sessions. Quota polling never persists credentials, raw provider responses, auth headers, or account IDs. Only bounded, sanitized quota observations and error summaries are stored in the utility state.
