# OpenCode Go (Zen) usage fixtures

These JSON files are **fully synthetic**. They reproduce only the structural
shape of the OpenCode Go `GET /v1/usage` response on
`https://opencode.ai/zen/go/v1/usage` as recorded in the adapter's contract
evidence (`quota.OpenCodeGoEvidence`).

They contain **no real account IDs, API keys, emails, tokens, or captured
responses**. All percentages are placeholder numerics and all timestamps are
synthetic instants, not observed values.

Exercised features: the `usage` envelope carrying the three known windows
(`rolling`, `weekly`, `monthly`), each a `{status, percent, resetsAt}` object
where `percent` is percent of the cap **USED** (never dollars); the advisory
`status` values `ok` and `rate-limited`; RFC3339 `resetsAt`; and the HTTP 401
`AuthError` error envelope.

| Fixture             | Purpose                                                                     |
|---------------------|-----------------------------------------------------------------------------|
| `usage.json`        | All three windows, low-to-moderate percentages, `status: ok`, valid resets — fresh 200 |
| `exhausted.json`    | A window at `percent: 100` with `rate-limited` — exhaustion inside a 200 body |
| `partial.json`      | Only some windows present plus an unparseable `resetsAt` — partial decode    |
| `auth_failure.json` | The 401 `AuthError` envelope — fail-closed authentication handling           |
