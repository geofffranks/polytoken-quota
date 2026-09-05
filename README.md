# polytoken-quota

`polytoken-quota` polls provider quota directly, records durable sanitized state, and reconciles explicitly managed Polytoken model fields. It supports one global Polytoken configuration and registered project configurations.

Quota polling participates per supported provider mapping: omitted or empty `quota` uses adapter defaults, while Anthropic requires either a positive `monthly_budget_usd` for API spend polling or `mode: subscription` for experimental Claude subscription-window polling. Quota-based routing is enabled by default. The tool ranks configured, pollable providers by quota projection pace (how fast each is burning its quota relative to its reset cycle), availability, balance group, off-peak schedules, and weight, then applies the resulting order through the normal validated reconciliation flow. Set `routing: {enabled: false}` in `desired.yaml` to opt out.

The tool never contacts a running Polytoken daemon from the host; change propagation to live sessions is opt-in and session-scoped (see [Change propagation to running sessions](#change-propagation-to-running-sessions)). It stores no provider credentials and persists no raw provider responses, auth headers, or account IDs.

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

Review the generated policy before adding quota blocks. Use `polytoken-quota doctor` for actionable configuration, mapping, quota, drift, and validation diagnostics.

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

The file is versioned YAML and is created by `polytoken-quota init`. Its main sections:

- `providers.<id>` is the provider mapping identity, enumerates the exact concrete models managed by that mapping, and optionally carries a `quota` block that enrolls the provider in quota polling and routing. The mapping key selects the quota adapter (`codex`, `zai`, `anthropic`, or `neuralwatt`).
- `global` defines the global Polytoken root, default chains (`full`, `mini`, `nano`, `classifier`), and the definition files whose `polytoken.model` or `polytoken.fallback_models` fields are managed.
- `projects` registers additional targets with the same fields. A project root is not discovered or adopted unless it is listed here.
- `routing`, `operational`, and quota fields are optional with sane defaults for supported non-Anthropic mappings. Anthropic may omit `quota` or use `quota: {}` to remain visible but unpollable; set a positive `monthly_budget_usd` for API spend polling or `mode: subscription` for experimental Claude subscription polling.

YAML anchors are welcome in `desired.yaml`: `reconcile`, `check`, and `status` read the resolved values and never rewrite the file, and the `routing.enabled` toggle edits that single value in place, tolerating anchors, aliases, merge keys, and duplicate keys anywhere else — it refuses only when one of them involves `routing.enabled` itself. Replacing the policy with `init --force` regenerates the file in canonical form, which flattens anchors and comments.

A complete minimal shape is:

```yaml
version: 1
providers:
  codex:
    models: [codex/gpt-5]
    quota: {}
  anthropic:
    models: [anthropic/claude-sonnet-4-6]
    quota:
      monthly_budget_usd: 250

global:
  root: /home/user/.config/polytoken
  full: [codex/gpt-5, anthropic/claude-sonnet-4-6]
projects: []
```

Unknown/manual mappings without a supported quota adapter remain managed routing participants: they keep their configured chain positions, are not quota-ranked or polled, and still honor explicit disable or unavailable state. Every configured mapping appears in diagnostics, including defaulted supported mappings and unpollable Anthropic/manual mappings.

The `models` list is the ownership boundary: only listed concrete models and the listed target chains/definition fields are managed. Preserve unmanaged Polytoken settings outside those fields. Model entries may be bare names, as shown above, or explicit mappings such as `codex/gpt-5: {enabled: true}`.

See **[docs/configuration.md](docs/configuration.md)** for the complete reference: every quota field (`monthly_budget_usd`, `freshness_ttl`, `balance_group`, `weight`, `schedule`), routing opt-out, and the `operational` knobs, with their defaults.

### Neuralwatt adapter

The `neuralwatt` adapter polls Neuralwatt Cloud's read-only quota endpoint (`GET /v1/quota`) with a transient `NEURALWATT_API_KEY` Bearer credential. It selects the first present boundary in this order: key-specific allowance, subscription energy allowance, then provider-reported USD credit balance for PAYG accounts. A present but malformed boundary fails closed rather than falling back to a weaker signal. A blocked key, subscription overage, exhausted balance, authentication failure, or missing/invalid selected limit is never treated as healthy.

The adapter reports the selected provider boundary as one routing window. Usage and energy totals are retained only as provider diagnostics in the response contract; they are not used as a synthetic quota when no enforceable allowance or balance is available. The account balance path does not invent a reset time when the provider does not report one.

### Anthropic adapters

The default `anthropic` API mode is for pay-as-you-go Anthropic **API** accounts.
A metered API has no token allowance to deplete — the meaningful quota is the
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
- Anthropic's per-minute API rate limits are deliberately not polled: they
  refill in about a minute, so a scheduled snapshot carries no session-start
  routing signal.

For Claude Pro/Max subscription accounts, set `providers.anthropic.quota.mode:
subscription`. This experimental mode reads Claude Code OAuth credentials
transiently from the active config directory or macOS Keychain, calls the
undocumented OAuth usage endpoint, and reports its five-hour and seven-day
windows as `session` and `weekly`. It never reads or writes the refresh token.
Because the endpoint is undocumented and can return 429, poll conservatively
(15–30 minutes), honor the default freshness window, and expect future contract
review. A 401 fails closed and requires Claude Code to re-authenticate.

`routing.enabled` defaults to `true` (an omitted `routing` section means enabled). Disabling it changes only the effective managed order; the desired chains in `desired.yaml` remain the user-authored baseline and are restored when routing is disabled. There is no mutation command for `routing.enabled`; set it directly in the YAML file.

## Commands

| Command | Description |
|---------|-------------|
| `init [--force]` | Create `desired.yaml` from current managed state. `--force` overwrites a valid existing file. |
| `status [--json]` | Show the merged quota and routing view: routing enablement, one global last-checked time, every configured mapping's status/reason, raw per-window quota numbers, next resets, compact target/source route rows with first desired/effective models, and a pending-config warning pointing at `doctor`. `--json` additionally retains ranking fields, route provenance, and complete desired/effective chains. |
| `check [--provider <id>] [--reconcile] [--json] [--quiet]` | Poll quota once; optionally filter a mapping, reconcile after saving, emit JSON, or suppress all output (for cron/launchd/systemd). |
| `reconcile [--dry-run [--keep-staging]]` | Reconcile managed Polytoken fields toward desired state. `--keep-staging` (dry-run only) retains a failed validation candidate's staging root for inspection; the retained path is printed and the caller owns deleting it (it may contain merged configuration). |
| `routing enable <mapping-id>` | Enable a provider mapping (clear manual disable). |
| `routing disable <mapping-id>` | Disable a provider mapping (hard exclusion). |
| `routing reset` | Clear all manual disables while preserving automatic observations. |
| `doctor [--json]` | Run configuration, quota, journal, and persisted-error diagnostics. |
| `history [--limit N] [--revision N] [--json]` | Show the meaningful provider/routing event timeline. `--limit` (1–100, default 20) limits event rows; `--revision` shows all events for one revision; `--json` emits deterministic structured events. |
| `install-hook [--config-dir DIR] [--handler-path PATH] [--notice PATH] [--dry-run] [--remove]` | Install or remove the in-session Polytoken hook entries (see below). |
| `notice-hook [--notice PATH]` | Internal: handle one in-session hook event. Invoked by the installed hooks, not for direct use. |

The `routing` commands manage per-mapping routing state. The top-level `routing.enabled` field in `desired.yaml` is edited in place by the routing toggle, which tolerates YAML anchors, aliases, and merge keys anywhere else in the file (see [Configuration](#configuration)).

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

Quota polling runs per provider with a `quota` block, and quota-based routing is enabled by default. Run a one-shot check manually or schedule it with an external scheduler:

```sh
polytoken-quota check --reconcile
polytoken-quota status
```

`status` is the single read surface for quota and routing state:

```
routing: enabled    last checked: 2026-08-14 09:12 UTC

PROVIDER  STATUS     REASON                    QUOTA                     NEXT RESET
codex     available  peak, pace 50%            5h 41/80, weekly 120/400  2026-08-15 00:00 UTC
zai       available  off-peak, pace 109%       5h 41/80, weekly 120/400  2026-08-15 00:00 UTC
minime    enabled    not configured             no data                   —

TARGET  SOURCE                 ROUTE     DESIRED       EFFECTIVE
global  config.yaml            global    glm-4.6       glm-4.6
work    subagents/work-api.md  work-api  glm-4.6       glm-4.6

warning: 1 target(s) pending — shown values may not be live; run polytoken-quota doctor
```

Provider STATUS consolidates the axes: `disabled` (manual `routing disable`) wins over everything; a configured mapping with no quota observation yet shows `enabled`; otherwise the availability axis decides `available`/`unavailable`. The provider table shows status, the ranking explanation, raw quota windows, and the next reset. Route rows show target/source provenance and only the first desired/effective model; `status --json` retains ranking fields, complete route chains, raw window numbers, `skipped` arrays, `pending_targets`, and `problem`. Exit codes are `1` for a fatal error or failed route projection and `2` for actionable quota problems.

Use `check --reconcile` when scheduled runs should apply the fresh routing decision to the live managed configs. Without `--reconcile`, `check` refreshes quota state only. In interactive use `check` prints each provider's polling status; pass `--quiet` in cron, launchd, or systemd timers to suppress all output (exit codes still reflect success or failure).

When routing is enabled, a successful fresh snapshot can make a provider eligible; stale, unavailable, unknown, partial-without-usable-data, and missing alias observations fail closed. Peak windows are expressed once, for example Monday–Friday 14:00–18:00 in `Asia/Singapore`; all other times are off-peak for ranking. Provider failures preserve the last good snapshot and are reported by `status` and `doctor`.

Routing uses a deterministic lexicographic ranking, not a blended score:

1. Providers must be eligible: their mode is `normal` or `reserve`, their snapshot is fresh, and it contains usable remaining quota.
2. Eligible providers stay grouped by `balance_group`; groups appear in their first configured order and do not interleave.
3. Within each group, providers are first separated into two pace tiers. Providers with **projection pace below 90%** are treated as equally under-paced: their exact pace does not differentiate them. They rank ahead of providers at or above 90%, and ties break by off-peak before peak, then higher `weight`.
4. Among providers at or above 90%, lower projection pace ranks first. Providers within 10% absolute pace are treated as tied, with off-peak and `weight` breaking ties. Pace is the ratio of used-fraction to elapsed-fraction from each provider's longest qualifying quota window (period + reset + remaining, minimum one day).
5. If providers remain equal after pace, schedule, and weight, they share a routing rank. Each desired route then keeps its own authored order for those providers; mapping ID is used only to keep diagnostic presentation deterministic. If any eligible provider in a balance group cannot compute a pace (no qualifying window), pace is skipped for that whole group. Ineligible providers remain at the end and are never disabled by routing.

For example, if `codex` and `neuralwatt` are both eligible in the same balance group, both have pace below 90%, and have equal schedule and weight, they share a rank. A researcher chain authored as `neuralwatt` then `codex` stays Neuralwatt-first, while an implementer chain authored as `codex` then `neuralwatt` stays Codex-first. If one provider is below 90% and the other is at or above 90%, the under-paced provider ranks first.

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

## Change propagation to running sessions

Host commands never contact Polytoken daemons. Instead, every reconcile that
changes managed fields publishes a small tool-neutral notice document (schema
version, revision, effective chains, changed fields, disabled models) at
`operational.notice_path` (default `~/.local/polytoken-quota/notice.json`,
atomically written, never containing credentials). Two delivery mechanisms
consume it:

**In-session convergence (opt-in).** `polytoken-quota install-hook` installs
two entries into Polytoken's `hooks.json` (backup kept, unrelated entries
untouched, `--remove` to uninstall, `--dry-run` to preview). The handler is
the `notice-hook` subcommand, which acts only on its **own** session's daemon
via the documented loopback API with that session's own credential:

- After each model turn, a session whose notice revision is newer than its
  consumed marker reloads its daemon's configuration. Reloads are
  turn-safe (a busy turn defers to the next one), preserve history, and
  never restart or compact the session. A model whose provider was disabled
  falls back to the configured chain head; routine quota rebalancing only
  reorders chains and never forces a switch.
- When you submit a prompt, a session running a model that dropped out of
  its configured chain receives one non-blocking reminder per revision —
  actionable if the model is disabled, informational if you deliberately
  picked a model outside the chain. A reload-forced model change is reported
  once (context on the new provider starts uncached). Switching models
  always remains your choice; nothing compacts or swaps a session.

Because agent containers each run their own loopback-only daemon, the notice
path must be visible inside them: bind-mount `~/.local/polytoken-quota` at
the same path (the install output reminds you), or point
`operational.notice_path` at an already-shared location. The handler binary
must likewise exist at the `--handler-path` inside containers (for example
`/home/dev/bin/polytoken-quota`).

**Host-side actions (opt-in).** The `operational.on_change` list (see
[docs/configuration.md](docs/configuration.md)) runs operator-configured
absolute executables on the host after a committed change, with the notice
JSON on stdin — the generic hook for reconfiguring other CLIs or notifying
yourself. Failures are recorded as events and never affect reconciliation.

Changing quota policy or enabling routing may change the choices seen by
existing Polytoken sessions; with the hook installed those sessions converge
on their own, and the drift reminders keep you informed without forcing
costly model swaps.
