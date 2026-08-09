#!/usr/bin/env bash
# Static contract checks for GitHub Actions workflow policy.
# Usage: scripts/test-workflows.sh [workflow ...]
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow_dir="$repo_root/.github/workflows"

if [[ ! -d "$workflow_dir" ]]; then
  echo "error: workflow directory not found: $workflow_dir" >&2
  exit 1
fi

if (($#)); then
  workflows=()
  for workflow in "$@"; do
    [[ "$workflow" = /* ]] && workflows+=("$workflow") || workflows+=("$repo_root/$workflow")
  done
else
  mapfile -t workflows < <(find "$workflow_dir" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) -print | sort)
fi

if ((${#workflows[@]} == 0)); then
  echo "error: no workflow files found" >&2
  exit 1
fi

for workflow in "${workflows[@]}"; do
  [[ -f "$workflow" ]] || { echo "error: workflow not found: $workflow" >&2; exit 1; }
done

ci="$workflow_dir/ci.yml"
[[ -f "$ci" ]] || { echo "error: required CI workflow not found: $ci" >&2; exit 1; }

assert_contains() {
  local file=$1 pattern=$2 description=$3
  grep -Eq -- "$pattern" "$file" || { echo "FAIL: $description ($file)" >&2; exit 1; }
  echo "PASS: $description"
}

assert_contains "$ci" '^on:' 'CI workflow declares triggers'
assert_contains "$ci" '^  push:' 'CI workflow triggers on push'
assert_contains "$ci" '^    branches: \[main\]$' 'CI workflow limits pushes to main'
assert_contains "$ci" '^  pull_request:' 'CI workflow triggers on pull requests'
assert_contains "$ci" '^permissions: *$' 'CI workflow declares permissions'
assert_contains "$ci" '^  contents: read$' 'CI workflow uses read-only contents permission'
assert_contains "$ci" 'actions/setup-go@v6' 'CI uses approved setup-go major tag'
assert_contains "$ci" 'go-version-file: go\.mod' 'CI setup-go reads repository Go directive'
assert_contains "$ci" 'go env GOVERSION' 'CI checks the installed Go version'
assert_contains "$ci" 'go test \./\.\.\.' 'CI runs standard Go tests'
assert_contains "$ci" 'go test -race \./\.\.\.' 'CI runs race tests'
assert_contains "$ci" 'go vet \./\.\.\.' 'CI runs go vet'
assert_contains "$ci" 'go build \./\.\.\.' 'CI runs go build'

# All action references must use approved major tags, keeping upgrades explicit.
while IFS=: read -r file line text; do
  [[ "$text" =~ uses: ]] || continue
  if [[ ! "$text" =~ uses:[[:space:]]*[^@[:space:]]+@(v[0-9]+|[0-9]+)$ ]]; then
    echo "FAIL: action reference is not an approved major tag ($file:$line)" >&2
    exit 1
  fi
done < <(grep -nH -E 'uses:' "${workflows[@]}" || true)
echo "PASS: action references use approved major tags"

echo "workflow contracts passed (${#workflows[@]} file(s))"
