# Opt-in task assessment and model selection

`select` recommends one registered model; it never launches an agent or reserves quota. Automatic assessment sends the stdin task to Jev. Explicit difficulty keeps the task local. This feature is not enabled by default and does not migrate any existing workflow.

Both `select` and `select-eval` retain the normal startup prerequisite: a supported `polytoken` binary must be resolvable through `PATH` or `POLYTOKEN_BINARY`. Even explicit-tier selection requires this prerequisite. Operator configuration is the usual `desired.yaml`, relocated with `POLYTOKEN_QUOTA_HOME`; `--policy` never names that configuration.

## Two separate policies

Only the operator's desired configuration can permit disclosure:

```yaml
selection:
  jev:
    enabled: true
    model: jev-1.13.0
    timeout: 10s
```

The runtime credential is `TYPESAFE_API_KEY`, supplied externally by the operator. Do not put it in desired configuration, a candidate file, arguments, or the Polytoken validation environment file. The selector reads it only immediately before an enabled remote request. The timeout is operational, not a latency guarantee. Missing consent or credential is an error; neither is needed for explicit difficulty.

A separately supplied candidate file contains only version and phase/tier groups:

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

These model names are illustrative, not a private inventory or capability recommendation. Every base model anywhere in the file must already be explicitly registered under `providers` in desired configuration. Reasoning suffixes are preserved exactly. Phase names are authored keys; tiers are exactly `routine`, `normal`, `difficult`, and `very_difficult`. Missing requested phases/tiers are errors, not permission to borrow another tier. Empty groups, duplicate references within a tier, duplicate keys, unknown structural fields, extra YAML documents, and oversized files are rejected.

When migrating an ordered Markdown list, make each old entry a **singleton group** in the same order. Combine candidates into one group only when you explicitly judge them interchangeable for that phase/tier; the selector does not infer equivalence or use global routing ranks as quality scores.

## Invocation and result handling

Create a local synthetic task file containing, for example, “Rename the heading in a test README to match the established style.” Then:

```sh
polytoken-quota select --policy candidates.yaml --phase execute --json < synthetic-task.txt
polytoken-quota select --policy candidates.yaml --phase execute --min-difficulty difficult --json < synthetic-task.txt
```

For sensitive tasks, assign difficulty yourself. This path does not read stdin or call Jev:

```sh
polytoken-quota select --policy candidates.yaml --phase execute --difficulty very_difficult --json
```

`--difficulty` and `--min-difficulty` conflict. A floor raises a valid assessment only; it cannot rescue an abstention or failed assessment. A review can exclude families repeatedly:

```sh
polytoken-quota select --policy candidates.yaml --phase execute --difficulty normal \
  --exclude-family codex --exclude-family anthropic --json
```

Family means the exact prefix before `/`, independently of the quota-owner mapping. It does not establish model lineage or reviewer independence.

| Exit | Selection meaning |
|---|---|
| 0 | `confirmed`: selected using fresh, complete, positive quota evidence |
| 2 | `uncertain`, `no_selection`, or `assessment_unavailable`: accepted result requiring caller handling |
| 1 | `error`: invalid invocation, policy, configuration, state, or another fatal failure |

JSON version 1 exposes a nullable model and status rather than a bare model name. Human output also labels uncertainty. Do not treat exit 2 as success without examining status. For example, with `jq` installed:

```sh
code=0
polytoken-quota select --policy candidates.yaml --phase execute --difficulty normal --json > selection-result.json || code=$?
case "$code" in
  0) jq -r '.model' selection-result.json ;;
  2) jq '{status, model}' selection-result.json # review uncertainty; do not auto-dispatch
     ;;
  *) printf '%s\n' 'Selection failed; stop.' >&2; exit 1 ;;
esac
```

Output contains no prompt, credential, raw upstream response, or arbitrary upstream error text. Do not add those to caller logs.

## Quota semantics and waves

Default selection reads one saved desired/state/as-of snapshot. It does not poll, reconcile targets, signal sessions, or write state. `--refresh` explicitly performs one quota check without reconciliation, finishes that transaction, then snapshots and assesses. A fatal check stops selection; an accepted check with provider problems may still leave usable last-good evidence. Refresh retains ordinary `check` semantics with reconciliation disabled; it does not request target publication or session actions.

For a wave, refresh once and then invoke multiple selectors without `--refresh`:

```sh
polytoken-quota check --json
# Inspect the check result: exit 1 stops; exit 2 requires reviewing provider problems.
polytoken-quota select --policy candidates.yaml --phase execute --difficulty normal --json
polytoken-quota select --policy candidates.yaml --phase execute --difficulty difficult --json
```

Across the requested tier, all groups are searched for confirmed candidates before any uncertain fallback. The first group with confirmation wins; within it, maximum minimum usable remaining fraction wins, with authored order breaking ties. If none are confirmed, the first non-excluded uncertain candidate wins. Otherwise the result is `no_selection`.

Baseline-disabled models, manually disabled providers, known unavailable/exhausted states and nonpositive usable headroom remain excluded even when stale. Reserve/low is not itself disabled. Missing, stale, partial, failed or unknown evidence is not manufactured confirmation; a failed refresh alone does not invalidate a still-fresh complete last-good snapshot. Corrupt state files are fatal; a missing state file yields missing evidence, not assumed health. Future/inconsistent timestamps are handled conservatively.

Positive headroom is not reserved capacity. Fractions are not comparable token counts across providers. Multiple same-snapshot selections can choose the same provider; there are no wave counters, balancing reservations, retries, or random spreading. Neither confirmed quota nor authored suitability guarantees successful execution.

## Assessment boundary

The versioned rubric judges the hardest required part, not task length:

- **Routine:** mechanical work following an established procedure.
- **Normal:** clear approach and established patterns.
- **Difficult:** interpretation, cross-cutting work, concurrency/performance or persisted-data implications.
- **Very difficult:** architecture, migration policy, security-sensitive work, deep coupling, or expensive novel errors.

`insufficient_information` is abstention, not a fifth tier. Exact maximum-probability ties choose the harder tier; an abstention tie abstains. No uncalibrated confidence cutoff is imposed. Invalid or contradictory responses, timeout, cancellation, redirects, HTTP errors and abstention return safe assessment-unavailable results, without automatic retry.

Only the task is sent as `state`, alongside trusted rubric/questions and the pinned classifier model, to the fixed `https://api.typesafe.ai/v1/systemone` endpoint. Candidate identities, config, history and attachments are not sent. HTTPS uses Go's standard transport and host trust configuration, including configured proxy and CA environment settings; operators must trust those settings before enabling assessment. Task text is untrusted assessment material; prompt injection and incomplete descriptions remain risks, not problems guaranteed solved by a sanitizer.

Prompts must be nonempty valid UTF-8 and at most 64 KiB; responses and whole fixture documents are capped at 256 KiB, and candidate policy files at 64 KiB. This conservative byte limit is not a tokenizer or the provider's token limit. Rejected tasks are not truncated; use explicit difficulty when appropriate. No prompt/response persistence is performed by this feature.

Official references: [API](https://docs.typesafe.ai/api), [Python usage/schema](https://docs.typesafe.ai/sdk/python/usage), [models and version pinning](https://docs.typesafe.ai/models), [privacy policy](https://typesafe.ai/legal/privacy-policy). Published input pricing at design time was $0.042/million tokens, not a permanent price promise. The privacy policy states no input training but does not guarantee fixed ordinary-account retention or zero data retention. Review current terms before enabling disclosure.

## Operator evaluation gate

Offline tests validate plumbing, protocol handling, and report arithmetic—not Jev accuracy or real latency. Before workflow adoption, the operator must separately authorize and run an evaluation with externally supplied credentials:

```sh
polytoken-quota select-eval --policy candidates.yaml --fixtures docs/selection-fixtures.yaml --live --json
```

Persistent consent and explicit `--live` are both required. Each invocation accepts at most 64 fixture cases, with at most one paid assessment request per case; split larger evaluations into separately authorized batches. Fixtures use non-sensitive synthetic IDs, phases, prompts and expected tier/abstention; all phase/tier references must exist in the supplied candidate policy. Adapt the illustrative policy's phase/tier coverage to the fixtures without using private prompts. Evaluation does not poll quota or dispatch agents. It reports case IDs, expected/actual assessment, safe errors, confusion counts, under/over-classification and abstention counts/rates, classifier/rubric versions, latency and token usage when provided. It never echoes task text or raw responses.

Exit 0 means all cases matched; 2 means mismatches, unavailable assessments, or policy-rejected cases; 1 means invalid local input/configuration or cancellation. This is a report, not an adopted quality threshold. Review boundary errors, short high-risk tasks, embedded misleading classification instructions, insufficient context and non-English cases. Record the model pin and rubric version with your review, rerun when either changes, and choose your adoption threshold deliberately. No live evaluation, account access, or workflow adoption is part of repository-agent validation.
