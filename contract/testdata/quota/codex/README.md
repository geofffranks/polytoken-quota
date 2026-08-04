# Codex (ChatGPT) usage fixtures

These JSON files are **fully synthetic**. They reproduce only the structural
shape of the Codex `wham/usage` response recorded in the contract evidence
(`docs/superpowers/specs/2026-08-03-pq-pa5n-provider-contract-evidence.md`).

They contain **no real tokens, account IDs, emails, or secrets**. All values are
placeholder numerics and synthetic identifiers used to exercise the adapter's
lenient, per-element decoding.

| Fixture                | Purpose                                                        |
|------------------------|----------------------------------------------------------------|
| `pro.json`             | Full "pro" response: primary + secondary + individual_limit + credits |
| `additional_limits.json` | Model-specific `additional_rate_limits[]` named windows       |
| `exhausted.json`       | A window at `used_percent: 100` (exhaustion inside a 200 body) |
| `partial.json`         | A malformed `secondary_window` (lenient per-element decode)    |
| `minimal.json`         | Only `primary_window`                                          |
