# Neuralwatt synthetic quota fixtures

All data in these files is synthetic. No account, key, or request identifier
is included.

- `quota.json` — a healthy PAYG account: USD credit balance, numeric usage and
  energy fields, a numeric rate-limit tier, a nullable overage limit, and no
  subscription allowance.
- `subscription-overage.json` — an in-overage subscription with retained
  numeric details (`kwh_used` above `kwh_included`, `kwh_remaining` zero) and
  an explicit sub-second-precision `kwh_reset_date`. It exercises the
  adapter's overage-detail retention and explicit-reset precedence paths.
