# z.ai (Zai / BigModel) quota fixtures

These JSON files are **fully synthetic**. They reproduce only the structural
shape of the z.ai `/api/monitor/usage/quota/limit` response as recorded in the
release-owned contract evidence.

They contain **no real API keys, account IDs, organization/project IDs, emails,
or secrets**. All values are placeholder numerics and synthetic identifiers used
to exercise the adapter's lenient, per-element decoding, the millisecond reset
conversion, and the success/auth gate.

| Fixture             | Purpose                                                                   |
|---------------------|---------------------------------------------------------------------------|
| `pro.json`          | Global Pro: one `TIME_LIMIT` (MCP monthly, unit=5/number=1) + one `TOKENS_LIMIT` (5-hour), with raw counts |
| `bigmodel_cn.json`  | Three limits: weekly + 5-hour `TOKENS_LIMIT` and an MCP `TIME_LIMIT`; `level:"pro"`, no `msg` |
| `exhausted.json`    | A `TOKENS_LIMIT` at `percentage:100` (exhaustion inside a success body)   |
| `missing_counts.json` | Limits with only `percentage` (no `usage`/`currentValue`/`remaining`)   |
| `credit_limit.json` | Live-shaped `CREDIT_LIMIT`-only response: 5-hour + weekly entries with raw counts; counts-derived percentage beats the stale server `percentage` |
| `auth_failure.json` | Envelope `{code:1001, msg:"Authorization Token Missing", success:false}` |
