# Task assessment and model selection

Last verified: 2026-09-20

## Purpose

Keep probabilistic difficulty assessment separate from deterministic, quota-aware recommendations. This module recommends; callers own dispatch and adoption decisions.

## Contracts and boundaries

- Operator `desired.yaml` owns Jev consent, model pin and timeout. Candidate YAML supplies only registered phase/tier groups and cannot authorize disclosure.
- At most one assessment backend may be enabled: `selection.jev` (remote, credential-resolved) or `selection.laya` (local loopback daemon, no credential). Load rejects both enabled; the consent gate is backend-neutral.
- The laya backend shares the Jev rubric, wire contract, bounds and sentinels, fixes its endpoint to the documented loopback daemon address, omits the per-request model pin (the daemon owns it and reports the served checkpoint), and treats a not-running or still-loading daemon as assessment-unavailable, never fatal.
- Explicit difficulty bypasses stdin, credentials and remote assessment. Floors apply only to valid assessments, never abstention.
- Select within one phase/tier and one desired/state/as-of snapshot. Known exclusions survive stale evidence; missing evidence cannot manufacture confirmation. Empty legacy provider axes are not evidence: a fresh complete quota snapshot confirms, while explicit bad state (corrupt axes, manual disable, exhausted, or a snapshot not explicitly available) still excludes or demotes. Search all confirmed groups before uncertain fallback.
- Return exact registered references, preserve suffixes, and expose uncertainty. Headroom is neither reserved capacity nor comparable token counts.
- Jev uses a fixed endpoint, trusted versioned rubric, bounded UTF-8 prompt and bounded response, cancellation, and no redirects or retries. Only task text is request state.
- Credential resolution occurs only immediately before enabled remote requests. Reports/errors contain safe codes and validated numbers, never prompt/key/raw response or arbitrary upstream text.
- CLI orchestration uses service snapshots and explicit quota checks without reconciliation. Target diagnostics do not invalidate independently usable selection inputs.

## Verification and adoption

Use fake transports, synthetic prompts and private temp stores. Repository-agent validation must not access live credentials/accounts or run paid requests. The operator runtime's opt-in capability is not permission for agents to exercise it. Live rubric evaluation requires separate operator authorization and review; offline tests establish protocol/plumbing correctness, not classifier quality.

See `../../docs/selection.md` for the public contract and `../../CONTEXT.md` for domain vocabulary. No global ranking rewrite, capacity reservations, workflow migration, or model-lineage inference belongs here.
