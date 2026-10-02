# polytoken-quota

`polytoken-quota` polls provider quota directly, records durable sanitized state, and reconciles explicitly managed Polytoken model fields. It supports one global Polytoken configuration and registered project configurations.

Quota polling participates per supported provider mapping: omitted or empty `quota` uses adapter defaults, while Anthropic requires either a positive `monthly_budget_usd` for API spend polling or `mode: subscription` for experimental Claude subscription-window polling. Quota-based routing is enabled by default. The tool ranks configured, pollable providers by a use-it-or-lose-it signal (how much quota each would forfeit unused at reset, net of how far it has overspent), availability, balance group, off-peak schedules, and weight, then applies the resulting order through the normal validated reconciliation flow. Set `routing: {enabled: false}` in `desired.yaml` to opt out.

The tool never contacts a running Polytoken daemon from host commands; change propagation to live sessions is opt-in and session-scoped (see [Change propagation to running sessions](#change-propagation-to-running-sessions)). Provider-only policies are an explicit opt-in: they gate enrolled Polytoken providers without managing groups, defaults, model flags, or facet/subagent assignments, and do not guarantee a particular fallback. The tool stores no provider credentials and persists no raw provider responses, auth headers, or account IDs.

## Minimum versions

| Component | Minimum | Notes |
|-----------|---------|-------|
| Go toolchain | `go 1.26.5` | Exact version match (`go env GOVERSION`). |
| Polytoken | none | Opt-in contract suite pins behavior, not a release number; resolved from `POLYTOKEN_CONTRACT_BIN` (or `POLYTOKEN_BIN`). |

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

The `models` list is the ownership boundary: only listed concrete models and the listed target chains/definition fields are managed. Preserve unmanaged Polytoken settings outside those fields. Model entries may be bare names, as shown above, or explicit mappings such as `codex/gpt-5: {enabled: true}`. When your Polytoken config uses `modelgroups`, legacy reconcile leaves the tier defaults unwritten and operator-owned instead of producing a candidate Polytoken rejects (details in the configuration reference).

See **[docs/configuration.md](docs/configuration.md)** for the complete reference: every quota field (`monthly_budget_usd`, `freshness_ttl`, `balance_group`, `weight`, `schedule`), routing opt-out, provider-only mode, and the `operational` knobs, with their defaults.

#### Provider-only mode for Polytoken model groups

For version-4 configurations built around Polytoken-owned model groups, provider-only mode is an explicit opt-in. It tracks provider IDs and changes only enrolled global `providers.<id>.enabled` fields:

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

Reserve and exhausted/disabled quota gate a provider off; recovery restores only the baseline the quota process recorded and still owns. Setting `quota_gate: {enabled: false}` inside a provider's `quota` block suspends this automatic quota gating for that provider: an out-of-quota observation never plans a disable, and a provider quota had gated off is re-enabled on the next pass, while `status` and `doctor` keep reporting the raw out-of-quota state and name the suspension (`quota_gate_suspended` on the status provider projection, a `quota-gate-suspended` doctor finding). The signal gate (`signal_gate`, `keep_enabled`) is a separate axis and still applies, and only an explicit `routing disable` turns an exempt provider off. Groups, tier defaults, model enabled flags, and facet/subagent assignments remain operator-owned. Same-name global and project groups have been observed to concatenate global leaves before project leaves while retaining duplicates; this mode does not select or promise a specific remaining model. When migrating from legacy policy, old quota-authored chains and model flags remain in Polytoken as operator-owned edits; review `init --provider-only --preview` and its rollback information before applying. See the [configuration reference](docs/configuration.md#policy-modes-legacy-and-provider-only).

### Neuralwatt adapter

The `neuralwatt` adapter polls Neuralwatt Cloud's read-only quota endpoint (`GET /v1/quota`) with a transient `NEURALWATT_API_KEY` Bearer credential. It selects the first present boundary in this order: key-specific allowance, subscription energy allowance, then provider-reported USD credit balance for PAYG accounts. A present but malformed boundary fails closed rather than falling back to a weaker signal. A blocked key, subscription overage, exhausted balance, authentication failure, or missing/invalid selected limit is never treated as healthy.

The adapter reports the selected provider boundary as one routing window. Usage and energy totals are retained only as provider diagnostics in the response contract; they are not used as a synthetic quota when no enforceable allowance or balance is available. The account balance path does not invent a reset time when the provider does not report one.

### OpenCode Go adapter

The `opencode-go` adapter polls OpenCode Go's read-only usage endpoint (`GET /zen/go/v1/usage`) with a transient Bearer credential: `OPENCODE_GO_API_KEY` when set, otherwise the key OpenCode itself saved at login in `auth.json` (`$XDG_DATA_HOME/opencode/auth.json`, else `~/.local/share/opencode/auth.json`; the `opencode-go` entry, then `opencode`). The response reports up to three usage windows — `rolling` (5 hours), `weekly`, and `monthly` — each as a percentage of a monthly per-model cap alongside a reset timestamp.

`percent` is **percent used**, never dollars, so this adapter needs no `monthly_budget_usd`. All three windows participate in routing, and the provider's effective remaining allowance is the minimum across them — the same conservative rule the Codex adapter applies to its 5-hour session window. A `rolling` window at 100% therefore demotes the provider for the remainder of that 5-hour window even when the monthly allowance is largely unused. That is deliberate and matches existing repo-wide behaviour, not an OpenCode-specific rule.

Reset anchoring uses the longest window reporting a future reset (in practice `monthly`). The 5-hour `rolling` window is shorter than the one-day quota-cycle floor, so it never drives next-reset or the routing signal — but it does still govern availability.

Fail-closed semantics:

- Window `status` is advisory (`ok` or `rate-limited`) and availability derives from `percent`. A window with a missing or unrecognized `status`, or a missing, non-numeric, or non-finite `percent`, fails that window closed rather than falling back to a weaker signal. A payload in which no window decodes is an error, never an empty healthy snapshot.
- A window without a parseable reset timestamp still decodes, without a reset time, and marks the snapshot `partial`. Reset times are never invented.
- An exhausted window (`percent: 100`) is reported as unavailable and classed `exhausted` — not as an error.
- `401` (missing or invalid key) and `403` (valid key with no OpenCode Go subscription) fail closed with distinct, sanitized diagnostics. `429` fails closed with a `Retry-After`-aware message and retries on the next scheduled check.
- A check with absent, expired, or incomplete contract evidence — or with neither `OPENCODE_GO_API_KEY` nor a usable `auth.json` key — returns an error **without making any HTTP request**.

Not synthesized: the endpoint exposes no dollar amounts and no per-model breakdown, so the adapter reports no monetary budget and no per-model windows. Nothing is inferred from the provider's Zen credit balance. The `auth.json` fallback reads only the key field of those two entries, transiently, for the one request; the file's contents never reach an error, snapshot, or state.

The endpoint contract is derived from OpenCode's first-party open-source console code and is **not officially documented**, so its evidence is reviewed quarterly rather than annually. If OpenCode changes or withdraws the route, the adapter fails closed instead of reporting a stale or invented allowance.

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

## Task-aware model selection

`polytoken-quota select` recommends **one suitable, explicitly registered model** for a task. It separates two decisions:

1. **Assess difficulty:** use a pinned Jev classifier to assess a task supplied on stdin, or provide `--difficulty` to keep the task entirely local.
2. **Select a candidate:** apply your phase/tier policy, model and provider exclusions, and saved quota evidence deterministically. Known out-of-quota providers stay excluded from recommendations even when `quota_gate` spares them from automatic gating — the exemption changes gating, never the observation.

The command does not launch a subagent, reserve quota, rewrite model chains, or migrate existing workflows. By default it does not poll providers or write state. Both `select` and `select-eval` retain the normal startup requirement for a supported `polytoken` binary on `PATH` or configured through `POLYTOKEN_BINARY`, including explicit-tier mode.

### Configure candidates separately from remote consent

`--policy` names a **candidate file**, not the application's `desired.yaml`. Registered models, provider ownership, freshness TTLs, and permission to send tasks remotely come from the standard operator configuration, relocated with `POLYTOKEN_QUOTA_HOME` when needed.

Save a candidate policy such as this as `candidates.yaml`:

```yaml
version: 1
phases:
  execute:
    routine:
      - [codex/example]
    normal:
      - [codex/example(high), anthropic/example]
      - [other/example]
    difficult:
      - [codex/example(high)]
    very_difficult:
      - [anthropic/example]
  plan:
    normal:
      - [codex/example]
    difficult:
      - [codex/example(high)]
    very_difficult:
      - [anthropic/example]
  orchestrate:
    normal:
      - [codex/example]
```

These are illustrative names, not model capability recommendations. **Every base model anywhere in the candidate file must already be registered in `desired.yaml`.** Exact reasoning suffixes such as `(high)` are preserved in the result. The provider mapping that owns a model need not have the same name as its provider family.

A tier contains ordered groups. Members of one group are candidates you explicitly consider interchangeable for that phase and tier; separate groups express preference order. When converting an existing ordered list, make each entry a singleton group unless you intentionally want quota-based choice within a group. Global routing rank is not used as a model-quality score.

Phase names are your explicit keys; the only tiers are `routine`, `normal`, `difficult`, and `very_difficult`. A phase may omit tiers, but requesting an absent phase/tier is an error—selection never borrows another tier. Unknown fields, duplicate YAML keys, duplicate exact references within a tier, empty groups, unregistered references, trailing documents, and candidate files over 64 KiB are rejected.

### Use an explicit tier for local or sensitive tasks

Explicit difficulty needs **no Jev consent or credential and does not read stdin**:

```sh
polytoken-quota select --policy candidates.yaml --phase execute --difficulty normal --json
```

Choose the tier based on the hardest required part of the work, not its length:

| Tier | Rubric |
|---|---|
| `routine` | Mechanical, well-specified work following an established procedure. |
| `normal` | A clear approach with established patterns to follow. |
| `difficult` | Interpretation, cross-cutting changes, concurrency/performance reasoning, or persisted-data implications. |
| `very_difficult` | Architecture, migration policy, security-sensitive work, deep coupling, or expensive novel errors. |

### Enable automatic task assessment deliberately

Remote assessment is disabled by default. To enable it, the operator adds this section to **application `desired.yaml`**, not `candidates.yaml`:

```yaml
selection:
  jev:
    enabled: true
    model: jev-1.13.0
    timeout: 10s
```

Supply `TYPESAFE_API_KEY` externally in the runtime process environment. Do not put the key in arguments, candidate policy, desired configuration, or the Polytoken validation environment file. It is read immediately before an enabled request and excluded from validation-subprocess forwarding. The model must be a versioned `jev-X.Y.Z` pin; 10 seconds is the default timeout, not a latency guarantee. Forced initialization preserves these settings.

Create `synthetic-task.txt` with a non-sensitive task such as “Rename the README heading using the supplied exact replacement.” Then run:

```sh
polytoken-quota select --policy candidates.yaml --phase execute --json < synthetic-task.txt
```

Only task text is sent as request state, alongside the trusted rubric and classifier pin, to the fixed Typesafe endpoint. Candidate identities, configuration, history, and attachments are not included. Prompts must be nonempty valid UTF-8 and at most 64 KiB; oversized tasks are rejected, not truncated. This byte bound is not a tokenizer or the provider's token limit. Output does not echo the prompt, key, or raw upstream response.

Automatic assessment can abstain with `insufficient_information`; this is not a fifth tier. Invalid responses, remote errors, timeout, or abstention produce `assessment_unavailable`, with no automatic retries. Exact maximum-probability ties choose the harder tier; a tie involving abstention abstains. There is no confidence cutoff. Treat classification as probabilistic and potentially misleading for incomplete or adversarial task descriptions. The provider's privacy terms do not establish zero data retention; review them before opting in.

### Floors, review exclusions, and refresh

| Option | Behavior |
|---|---|
| `--policy PATH` | Required candidate-policy file; cannot grant remote consent. |
| `--phase NAME` | Required policy phase. |
| `--difficulty TIER` | Bypass Jev and stdin entirely. |
| `--min-difficulty TIER` | Raise a valid automatic assessment to at least this tier. Conflicts with `--difficulty`; cannot rescue abstention or failure. |
| `--exclude-family NAME` | Repeatable exclusion of the exact prefix before `/`, independent of quota ownership. Not a model-lineage guarantee. |
| `--refresh` | Perform one quota check without reconciliation, finish its transaction, then read the selection snapshot. |
| `--json` | Emit a version-1 result with explicit status and a nullable model. |

Examples:

```sh
# Require at least difficult work after a valid automatic assessment.
polytoken-quota select --policy candidates.yaml --phase execute \
  --min-difficulty difficult --json < synthetic-task.txt

# Exclude a prior worker's provider family for a review recommendation.
polytoken-quota select --policy candidates.yaml --phase execute \
  --difficulty normal --exclude-family codex --json

# Refresh quota once before selecting, without reconciling targets.
polytoken-quota select --policy candidates.yaml --phase execute \
  --difficulty normal --refresh --json
```

A fatal refresh stops selection before assessment. An accepted check with provider problems can continue using independently usable evidence, including a still-fresh last-good snapshot after a failed refresh. For a wave of tasks, run `polytoken-quota check --json` once, inspect its result, then run multiple selectors without `--refresh`.

### How quota evidence determines the recommendation

Selection searches **all groups for confirmed candidates before using any uncertain fallback**. It picks the first group with a confirmed candidate, then maximizes the minimum usable remaining fraction across that candidate's quota windows. Authored order breaks ties. If no candidate is confirmed anywhere in the tier, it returns the first non-excluded uncertain candidate in authored order. If none remain, it returns `no_selection`.

- **Excluded:** baseline-disabled models, manually disabled providers, known unavailable/exhausted states, nonpositive usable headroom, excluded families, or corrupt state values. Stale data never rescues these exclusions.
- **Confirmed:** supported normal/reserve state and fresh, complete, successful, available quota evidence with positive usable headroom. Reserve/low is not itself disabled.
- **Uncertain:** missing, stale, partial, failed, sparse, or unknown evidence that cannot support confirmation. A missing state file supplies missing evidence; an unreadable/corrupt state file is fatal. Future or inconsistent timestamps are handled conservatively.

Headroom fractions are not comparable token counts across providers. Positive headroom is not a reservation: multiple same-snapshot selections can choose the same provider. Neither quota confirmation nor your suitability policy guarantees the model will succeed or have capacity when dispatched.

### Handle statuses rather than assuming a model is available

| Status | Exit | Caller action |
|---|---|---|
| `confirmed` | `0` | Recommendation has fresh usable quota evidence; still not reserved capacity. |
| `uncertain` | `2` | A model is recommended without confirmed quota evidence; review before dispatch. |
| `no_selection` | `2` | No eligible candidate remains; model is null. |
| `assessment_unavailable` | `2` | No usable automatic assessment; model is null. |
| `error` | `1` | Invalid invocation, policy/configuration/state, missing consent/credential, or another fatal failure. |

Human output labels uncertainty explicitly. JSON includes `version`, `explicit_tier`, `abstained`, `refreshed`, `model`, `mapping` (the quota owner), effective `tier`, original `assessed_tier`, `assessed_model`, `reason`, quota `evidence` and `headroom`, and snapshot timestamps `as_of` / `evidence_checked_at` when relevant. Automatic results also carry validated `confidence` and `probabilities`; explicit-tier selection has no classifier assessment. Do not interpret its default confidence value as a classifier judgment.

For shell automation, preserve exit 2 and inspect the status instead of extracting a model blindly. This example requires `jq`:

```sh
code=0
polytoken-quota select --policy candidates.yaml --phase execute \
  --difficulty normal --json > selection-result.json || code=$?
case "$code" in
  0) jq -r '.model' selection-result.json ;;
  2) jq '{status, model, reason, evidence}' selection-result.json
     # Review this result; do not automatically dispatch.
     ;;
  *) printf '%s\n' 'Selection failed; stop.' >&2; exit 1 ;;
esac
```

### Evaluate Jev before adopting an automatic workflow

`select-eval` runs synthetic rubric fixtures through the same assessment client, without quota polling or dispatch. It requires both persistent consent and explicit `--live`; an operator must separately authorize the paid requests and supply the credential:

```sh
polytoken-quota select-eval --policy candidates.yaml \
  --fixtures docs/selection-fixtures.yaml --live --json
```

The candidate policy must cover every fixture's phase and expected tier, and all candidate models must be registered. The shipped fixtures cover all four tiers, abstention, misleading embedded instructions, short high-risk work, and non-English tasks. Fixture documents are bounded to 256 KiB and 64 cases per invocation and use non-sensitive synthetic prompts. Each assessed case can incur one paid request; larger evaluations require separately authorized batches.

The report includes case IDs, expected/actual assessments, safe errors, confusion counts, under/over-classification and abstention rates, classifier/rubric versions, latency, and token usage when provided—not task text or raw responses. Exit `0` means all fixtures matched; `2` means mismatches, unavailable assessments, or policy-rejected cases; `1` means invalid input/configuration, cancellation, or a missing credential. This report is not an adopted quality threshold.

Offline repository tests verify protocol handling, selection, persistence boundaries, and report arithmetic, **not live Jev quality or latency**. Review a separately authorized evaluation before adoption and repeat it when the model pin or rubric changes. No live evaluation or workflow migration is automatic. See the [selection guide](docs/selection.md) for the full contract and provider references, and the [configuration reference](docs/configuration.md#selectionjev) for operator settings.

## Commands

| Command | Description |
|---------|-------------|
| `init [--force]` | Create `desired.yaml` from current managed state. `--force` overwrites a valid existing file while preserving selection consent/configuration. |
| `select --policy PATH --phase NAME [--difficulty TIER | --min-difficulty TIER] [--exclude-family NAME] [--refresh] [--json]` | Recommend a candidate; automatic mode reads stdin, explicit tier stays local. |
| `select-eval --policy PATH --fixtures PATH --live [--json]` | Operator-authorized rubric evaluation; never invoked by normal selection or tests. |
| `status [--json]` | Show the merged quota and routing view: routing enablement, one global last-checked time, every configured mapping's status/reason, raw per-window quota numbers, next resets, compact target/source route rows with first desired/effective models, and a pending-config warning pointing at `doctor`. `--json` additionally retains ranking fields, route provenance, and complete desired/effective chains. |
| `check [--provider <id>] [--reconcile] [--json] [--quiet]` | Poll quota once; optionally filter a mapping, reconcile after saving (including provider-only gating), emit JSON, or suppress all output (for cron/launchd/systemd). |
| `reconcile [--dry-run [--keep-staging]] [--verbose]` | Reconcile managed fields; in provider-only mode gates enrolled providers after staged validation of the composed candidate. Quiet by default. `--keep-staging` is dry-run only; retained candidates may contain merged configuration. |
| `routing enable <mapping-id>` | Enable a provider mapping (clear manual disable). Overrides `keep_enabled` (the keep_enabled flag constrains automatic gating only). |
| `routing disable <mapping-id>` | Disable a provider mapping (hard exclusion). Overrides `keep_enabled`: an explicit operator disable works even on a keep_enabled provider. |
| `routing reset` | Clear all manual disables while preserving automatic observations. |
| `doctor [--json]` | Run configuration, quota, journal, and persisted-error diagnostics. |
| `history [--limit N] [--revision N] [--json]` | Show the meaningful provider/routing event timeline. `--limit` (1–100, default 20) limits event rows; `--revision` shows all events for one revision; `--json` emits deterministic structured events. |
| `install-hook [--config-dir DIR] [--handler-path PATH] [--notice PATH] [--dry-run] [--remove]` | Install or remove the in-session Polytoken hook entries (see below). |
| `notice-hook [--notice PATH]` | Internal: handle one in-session hook event. Invoked by the installed hooks, not for direct use. |

The `routing` commands manage per-mapping routing state. The top-level `routing.enabled` field in `desired.yaml` is edited in place by the routing toggle, which tolerates YAML anchors, aliases, and merge keys anywhere else in the file (see [Configuration](#configuration)).

Existing commands use `0` for an accepted clean result, `1` for a rejected request or diagnostic failure, and `2` for an accepted operation with a pending provider, quota, target, or validation problem. `check --json` and `status --json` emit one sanitized structured envelope for accepted and rejected requests.

For `select`, `0` means `confirmed`; `2` means `uncertain`, `no_selection`, or `assessment_unavailable`; `1` means a fatal input/configuration/state error. Inspect status before using a recommendation: uncertainty is not known availability. For `select-eval`, `0` means every fixture matched, `2` means mismatches, unavailable assessments, or policy-rejected cases, and `1` means invalid input/configuration or cancellation. Mocked tests do not establish Jev quality; the operator must review an explicitly authorized live evaluation before adoption.

## Meaningful event history

`polytoken-quota history` is a newest-first timeline of meaningful provider and routing events, not a generic reconcile counter. It records quota low/reached/reset transitions, provider failures/recoveries, manual disable/enable/reset actions, ignored stale hooks, quota-poll failures, and routing changes such as rank, eligibility, signal explanation, or peak/off-peak changes. Unchanged quota observations and unchanged routing decisions are suppressed.

```text
EVENT HISTORY
Reported at: 2026-08-14 02:30:00 UTC

WHEN                 PROVIDER   EVENT                    RESULT
2026-08-14 02:22:35  zai        quota_reached             disabled; removed from managed chains
2026-08-13 14:04:03  codex      routing_changed           rank 1 -> 3; overdrawn
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

PROVIDER   STATUS       REASON                                              QUOTA                     NEXT RESET
codex      available    peak, signal +1.00                                  5h 41/80, weekly 120/400  2026-08-15 00:00 UTC
zai        available    off-peak, signal -0.19                              5h 41/80, weekly 120/400  2026-08-15 00:00 UTC
gp         gated        signal-gated (-0.72 <= +0.00); peak, signal -0.72   5h 41/80                  —
neuralwatt unavailable  ineligible: out of quota                            unavailable (in overage)  —
minime     enabled      not configured                                      no data                   —

TARGET  SOURCE                 ROUTE     DESIRED       EFFECTIVE
global  config.yaml            global    glm-4.6       glm-4.6
work    subagents/work-api.md  work-api  glm-4.6       glm-4.6

warning: 1 target(s) pending — shown values may not be live; run polytoken-quota doctor
```

Provider STATUS consolidates the axes: `disabled` (manual `routing disable`) wins over everything; a provider an ownership gate holds off shows `gated` — the signal- and reserve-axis gates, where quota holds the provider's `enabled` field off (a conflicted claim, where the operator re-enabled the field, keeps its quota-health status, and a deliberately gated provider keeps a clean exit code: the red row is informational, while exit codes still reflect quota health); a configured mapping with no quota observation yet shows `enabled`; otherwise the availability axis decides `available`/`unavailable`.

When a poll succeeds but the provider is out of quota, the QUOTA column names the fail-closed condition — `unavailable (key blocked)`, `unavailable (in overage)`, or `unavailable (balance drained)` — and NEXT RESET stays `—`; the REASON column reads `ineligible: out of quota` for the same rows; an observed windowless snapshot without a named condition renders `no data (unavailable)`.

The provider table shows status, the ranking explanation, raw quota windows, and the next reset. Route rows show target/source provenance and only the first desired/effective model; `status --json` retains ranking fields, complete route chains, raw window numbers, `skipped` arrays, `pending_targets`, and `problem`, plus per-provider `condition`, `checked_at`, and `availability`. Exit codes are `1` for a fatal error or failed route projection and `2` for actionable quota problems.

Use `check --reconcile` when scheduled runs should apply the fresh routing decision to the live managed configs. Without `--reconcile`, `check` refreshes quota state only. In interactive use `check` prints each provider's polling status; pass `--quiet` in cron, launchd, or systemd timers to suppress all output (exit codes still reflect success or failure).

When routing is enabled, a successful fresh snapshot can make a provider eligible; stale, unavailable, unknown, partial-without-usable-data, and missing alias observations fail closed. Peak windows are expressed once, for example Monday–Friday 14:00–18:00 in `Asia/Singapore`; all other times are off-peak for ranking. Provider failures preserve the last good snapshot and are reported by `status` and `doctor`.

Routing uses a deterministic lexicographic ranking, not a blended score:

1. Providers must be eligible: their mode is `normal` or `reserve`, their snapshot is fresh, and it contains usable remaining quota.
2. Eligible providers stay grouped by `balance_group`; groups appear in their first configured order and do not interleave.
3. Within each group, providers are ordered by their **use-it-or-lose-it signal**, highest first (see [How the routing signal is scored](#how-the-routing-signal-is-scored)). Positive means quota will reach reset unused, zero is exactly on pace, and negative means overdrawn.
4. Signals are clustered: after sorting, a new cluster starts wherever two neighboring signals differ by 0.20 or more. Providers in the same cluster are tied on the signal, and off-peak before peak, then higher `weight`, break the tie.
5. If providers remain equal after signal, schedule, and weight, they share a routing rank. Each desired route then keeps its own authored order for those providers; mapping ID is used only to keep diagnostic presentation deterministic. If any eligible provider in a balance group has no signal (no qualifying window), the signal is skipped for that whole group. Ineligible providers remain at the end and are never disabled by routing.

For example, if `codex` and `neuralwatt` are both eligible in the same balance group with signals +0.40 and +0.30, and equal schedule and weight, they share a rank. A researcher chain authored as `neuralwatt` then `codex` stays Neuralwatt-first, while an implementer chain authored as `codex` then `neuralwatt` stays Codex-first. If Codex's signal were +0.90 instead, Codex would rank first in both chains.

### How the routing signal is scored

Subscription quota is use-it-or-lose-it: whatever is left when a window resets is gone. The signal asks, for each provider, "how much paid allowance am I about to forfeit, net of how far I have already overspent?" and routes work to whoever would waste the most by being skipped.

**Which windows count.** A window contributes when it reports a period of **at least one day**, a reset time, and a remaining fraction. Shorter windows, such as 5-hour session limits, are rate limits rather than quota cycles, so they never move the signal. An exhausted short window still makes the provider ineligible, so it drops out of ranking entirely until that window resets.

**Per-window gap.** For a window with period `P`, remaining fraction `r` (0–1), and time until reset `R`:

```text
left     = max( clamp(R, 0, P) / P ,  1 hour / P )       # fraction of the period still to come
elapsed  = max( min(ceil_to_day(P - R), P) / P ,  5 minutes / P )  # fraction already gone
gap      = r / left  -  (1 - r) / elapsed
```

- `r / left` is the **forfeiture pressure**. Quota left over with little time remaining is about to expire, so this term grows as reset approaches.
- `(1 - r) / elapsed` is the **burn rate**. It measures how much has been used relative to how much of the cycle has passed. Using quota faster than time passes makes it large.
- On exact pace, for example 3/7 used on day 3 of a 7-day window, the two terms cancel and the gap is 0.

**Combining windows.** When a provider reports several qualifying windows, such as weekly and monthly, their gaps are averaged weighted by period length, so a 30-day window counts about four times as much as a 7-day one. The result is clamped to ±100.

**Why the floors and rounding.**
- Elapsed time is **rounded up to whole days**. Right after a reset, a few minutes of use would otherwise look like a huge overdraft. The rounding means the signal leans slightly positive within a day, which is intentional.
- The **one-hour floor** on time left keeps a window at or past its reset finite. Without it, a window about to reset with quota left would divide by zero.
- The **five-minute floor** on elapsed time does the same for a window whose reset is a full period away.

**Worked examples** (one 7-day window each):

| Situation | Used | Day | Signal | Reading |
|---|---|---|---|---|
| Exactly on pace | 3/7 | 3 | **0.00** | nothing to reclaim, nothing overdrawn |
| Under pace | 1/7 | 3 | **+1.17** | quota is piling up; prefer this provider |
| Over pace | 5/7 | 3 | **−1.17** | burning too fast; route elsewhere |
| Early, half pace | 1/7 | 2 | **+0.70** | ahead, but plenty of time to catch up |
| Late, half pace | 3/7 | 6 | **+3.50** | 4/7 of the week expires tomorrow; drain it now |

The last two rows show the difference from the old pace ranking. Both have used quota at half the rate time has passed, so pace called them equal. The signal knows the late one is about to lose most of its allowance and puts it first.

**Rank ties.** The signal is continuous, but tiny differences shouldn't override your authored chain order on every poll. Providers are sorted by signal and grouped whenever neighbors differ by less than **0.20**. Mid-cycle, 0.20 of signal corresponds to about 0.10 of the used ÷ elapsed ratio, the same sensitivity the old pace bands had. Within a group, off-peak, `weight`, and then each chain's own authored order decide. If any eligible provider in a balance group reports no qualifying window, the signal is ignored for that whole group rather than guessed. `status` shows each provider's value as `peak, signal +0.42` / `off-peak, signal -1.30`.

**Credit.** The signal is adapted from the `spendPriority` metric in [quota-axi](https://github.com/kunchenguid/quota-axi) by Kun Chen (MIT). This project reimplements those ideas in Go, keeps its own fail-closed eligibility rules, and ignores sub-day windows, so the numbers will not match quota-axi's exactly.

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

Host commands never contact Polytoken daemons. A committed managed-field change
publishes a small tool-neutral notice at `operational.notice_path` (default
`~/.local/polytoken-quota/notice.json`, atomically written and never containing
credentials). Legacy notices carry route facts; provider-only notices carry
only provider IDs and enabled status. Two opt-in delivery mechanisms can
consume the notice:

**In-session convergence (opt-in).** `polytoken-quota install-hook` installs
two entries into Polytoken's `hooks.json` (backup kept, unrelated entries
untouched, `--remove` to uninstall, `--dry-run` to preview). The handler is
the `notice-hook` subcommand, which acts only on its **own** session's daemon
via the documented loopback API with that session's own credential:

- After a model turn, a session with a newer shared notice reloads only its
  own daemon. A busy-turn `409` leaves the consumed marker unchanged, so a later
  hook event can retry; the marker advances only after a successful `200`.
  Reloading does not restart or compact the session. Prompt hooks accept without
  blocking and provider-only notices do not claim a particular model change or
  fallback.
- Provider-only notices report the provider IDs and enabled status involved in
  the committed change. They do not identify a serving model, assert a forced
  switch, or guarantee which group leaf Polytoken will choose. The pinned
  synthetic contract suite proves successful next-turn continuation for its
  covered routes; this is not a promise for every configuration or provider.

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

Changing provider availability may affect which configured group leaves are
usable. An installed hook asks each session's own daemon to reload after a turn;
it does not issue a model-selection request or block a prompt. Provider-only
mode does not promise a particular fallback or surface a drift reminder.
