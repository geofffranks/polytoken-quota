Synthetic fixtures for the Antigravity adapter, which runs
`agy -p /quota --output-format json` and parses its stdout.

These are **not** live captures; all values are synthetic. `live_shape.json`
reproduces the structure a live, eligible account returned on 2026-09-28: a
print-mode envelope (`status`, `response` text table, zero-token `usage`)
carrying `command: {name: "usage", data: {groups: [...]}}` with snake_case
bucket fields and a `window` of `weekly` or `5h`. The other fixtures follow
quota-axi's agy normalizer, including its camelCase aliases. A mismatch fails
closed.

- `live_shape.json`: the live structure with a Gemini group (weekly 60% left,
  5-hour 90% left) and an exhausted Claude/GPT group that must be ignored.
- `quota.json`: a `/quota` result with a Gemini group (5-hour bucket with a
  numeric remaining fraction and ISO reset; weekly bucket with a
  `{case, value}` remaining fraction and epoch-millisecond reset) and a
  Claude/GPT group that the adapter ignores.
- `no_gemini.json`: a result with only a Claude/GPT group (fails closed).
- `wrong_command.json`: a result for a different slash command (fails closed).
- `disabled_bucket.json`: a `/usage` result under `summary`, with a disabled
  Gemini 5-hour bucket (ignored) and snake_case field names.

All values are synthetic; no account names, emails, or tokens are included.
