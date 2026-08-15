#!/usr/bin/env bash
# Static contract checks for documentation claims affected by quota/status relaxation.
# Usage: scripts/test-docs.sh
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

check_file() {
  local file=$1
  local pattern description
  while IFS=$'\t' read -r pattern description; do
    [[ -n "$pattern" ]] || continue
    if grep -nF -- "$pattern" "$file"; then
      echo "FAIL: $description ($file)" >&2
      return 1
    fi
  done <<'PATTERNS'
status command shows only mappings with a quota block	status documentation must include all configured mappings
any provider mapping with a `quota` block is polled	polling documentation must not require an explicit quota block
Mappings without a `quota` block remain managed routing participants	omitted recognized-provider mappings must not be documented as unranked
PATTERNS
}

check_docs() {
  local root=$1 file
  local files=(README.md docs/configuration.md AGENTS.md)
  for file in "${files[@]}"; do
    [[ -f "$root/$file" ]] || {
      echo "FAIL: required documentation file not found: $file" >&2
      return 1
    }
    check_file "$root/$file"
  done
}

self_check() {
  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN

  printf '%s\n' 'The status command shows only mappings with a quota block.' > "$tmp/stale.md"
  if check_file "$tmp/stale.md"; then
    echo "FAIL: stale-prose self-check did not reject a stale sentence" >&2
    return 1
  fi

  printf '%s\n' \
    'providers:' \
    '  unknown: {}' \
    '  codex:' \
    '    quota: {}' \
    'Mappings without a `quota` block may use any key when no adapter is selected.' \
    > "$tmp/allowed.md"
  if ! check_file "$tmp/allowed.md"; then
    echo "FAIL: allowed-example self-check rejected valid YAML or unknown-key wording" >&2
    return 1
  fi
}

self_check
check_docs "$repo_root"
echo 'PASS: quota/status documentation claims are current and scoped'
