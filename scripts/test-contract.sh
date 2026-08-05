#!/usr/bin/env bash
# scripts/test-contract.sh — explicit opt-in runner for the Polytoken contract
# suite. The contract tests invoke the real supported Polytoken binary, so they
# are skipped by default (`go test ./...`). Run this script when a supported
# binary is available.
#
# Usage:
#   POLYTOKEN_CONTRACT_BIN="$POLYTOKEN_BIN" ./scripts/test-contract.sh
#
# Environment:
#   POLYTOKEN_CONTRACT_BIN  path to the supported Polytoken binary (required)
#   POLYTOKEN_VERSION       expected version substring override (development
#                           only; the supported version is enforced by default)
#
# The contract suite isolates HOME and XDG_* so doctor never loads the operator's
# real ~/.config/polytoken definitions; the staging root is the sole source.
set -euo pipefail

if [[ -z "${POLYTOKEN_CONTRACT_BIN:-}" ]]; then
  echo "error: POLYTOKEN_CONTRACT_BIN is not set" >&2
  echo "set it to the supported polytoken binary path, e.g." >&2
  echo "  POLYTOKEN_CONTRACT_BIN=\"\$POLYTOKEN_BIN\" ./scripts/test-contract.sh" >&2
  exit 64
fi

if [[ ! -f "$POLYTOKEN_CONTRACT_BIN" || ! -x "$POLYTOKEN_CONTRACT_BIN" ]]; then
  echo "error: POLYTOKEN_CONTRACT_BIN=$POLYTOKEN_CONTRACT_BIN is not an executable file" >&2
  exit 64
fi

# Run from the repository root so contract/testdata resolves.
cd "$(dirname "$0")/.."

# Run the whole contract package: the real-binary cases plus the static
# staging/quota/CodexBar contract checks, so "the contract suite passed" means
# all of it ran.
exec go test ./contract -v -count=1 -timeout 300s
