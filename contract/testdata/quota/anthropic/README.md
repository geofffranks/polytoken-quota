# Anthropic quota contract fixtures

Synthetic, sanitized fixtures for the Anthropic spend-budget quota adapter.
The adapter polls the Admin API cost report
(`GET /v1/organizations/cost_report`, `sk-ant-admin…` key) for month-to-date
organization spend and reports it against the mapping's `monthly_budget_usd`
as one "monthly" window. Each fixture is a JSON object of the shape:

```json
{ "status": 200, "body": { ...cost report page... } }
```

The body shape follows the real cost-report contract: an envelope
`{data:[{starting_at,ending_at,results:[…]}],has_more,next_page}` where each
result carries `amount` (a decimal string in **cents**), `currency` (`"USD"`),
`cost_type`, and grouping metadata. All values are synthetic; no account,
workspace, or API-key identifiers from a real organization appear in any
fixture (the `workspace_id: null` entries mirror the ungrouped default
response).

- `midmonth.json` — spend summing to 15000 cents ($150) against the test
  budget of $200; fresh, available, 75% used.
- `empty_month.json` — no spend recorded (start of month, or the open day's
  bucket has not materialized yet); fresh, 0% used.
- `over_budget.json` — 25000 cents ($250) against the $200 budget; fresh
  observation, treated as exhausted (usage percent capped at 100).
- `auth_failure.json` — 401 (e.g. a standard key instead of an admin key);
  failed attempt with an ANTHROPIC_ADMIN_API_KEY remediation.
