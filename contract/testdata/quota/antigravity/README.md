Synthetic fixtures for the Antigravity adapter, which runs
`agy -p /quota --output-format json` and parses its stdout.

These are **not** live captures. The shapes follow quota-axi's agy print-mode
normalizer. Run `polytoken-quota check` with the `agy` CLI installed and logged
in to confirm the vendor output still matches. A mismatch fails closed.

- `quota.json`: a `/quota` result with a Gemini group (5-hour bucket with a
  numeric remaining fraction and ISO reset; weekly bucket with a
  `{case, value}` remaining fraction and epoch-millisecond reset) and a
  Claude/GPT group that the adapter ignores.
- `no_gemini.json`: a result with only a Claude/GPT group (fails closed).
- `wrong_command.json`: a result for a different slash command (fails closed).
- `disabled_bucket.json`: a `/usage` result under `summary`, with a disabled
  Gemini 5-hour bucket (ignored) and snake_case field names.

All values are synthetic; no account names, emails, or tokens are included.
