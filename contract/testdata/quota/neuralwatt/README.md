Synthetic, sanitized fixture for the Neuralwatt Cloud `GET /v1/quota` adapter.

The fixture models the live PAYG/balance response shape observed during contract
verification: USD credits, numeric usage and energy fields, a numeric rate-limit
tier, a nullable overage limit, and no subscription allowance. All values are
synthetic and no account, key, or request identifiers are included.

The adapter also tests documented key-allowance and subscription variants in
`internal/quota/neuralwatt_test.go`; those variants take precedence over the
account balance when present and valid.
