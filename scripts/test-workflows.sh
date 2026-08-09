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
  workflows=()
  while IFS= read -r workflow; do
    workflows+=("$workflow")
  done < <(find "$workflow_dir" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) -print | sort)
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

assert_exact_block() {
  local file=$1 start=$2 expected=$3 description=$4
  local actual
  actual=$(awk -v start="$start" '
    $0 == start { in_block = 1; next }
    in_block && $0 !~ /^  / { exit }
    in_block { print }
  ' "$file")
  [[ "$actual" == "$expected" ]] || {
    echo "FAIL: $description ($file)" >&2
    echo "expected:" >&2
    printf '%s\n' "$expected" >&2
    echo "actual:" >&2
    printf '%s\n' "$actual" >&2
    exit 1
  }
  echo "PASS: $description"
}

assert_exact_block "$ci" 'on:' $'  push:\n    branches: [main]\n  pull_request:' 'CI workflow has exactly push-to-main and pull-request triggers'
assert_exact_block "$ci" 'permissions:' '  contents: read' 'CI workflow has exactly read-only contents permission'
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

release="$workflow_dir/release.yml"
[[ -f "$release" ]] || { echo "error: required release workflow not found: $release" >&2; exit 1; }
assert_exact_block "$release" 'on:' $'  release:\n    types: [published]\n  workflow_dispatch:\n    inputs:\n      tag:\n        description: Release tag (vX.Y.Z)\n        required: true\n        type: string' 'release workflow has published and manual tag triggers'
assert_contains "$release" 'concurrency:' 'release workflow defines concurrency'
assert_contains "$release" 'group: release-\$\{\{ inputs\.tag \|\| github\.event\.release\.tag_name \}\}' 'release concurrency is keyed by tag'
assert_contains "$release" 'gh release view "\$tag"' 'release preflight verifies an existing release'
assert_contains "$release" 'if \[\[ ! "\$tag" =~ \^v\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\$ \]\]' 'release validates vX.Y.Z tags'
assert_contains "$release" 'ref: \$\{\{ needs\.preflight\.outputs\.tag \}\}' 'release checks out the selected tag'
assert_contains "$release" 'go-version-file: go\.mod' 'release setup-go reads go.mod'
assert_contains "$release" 'matrix:' 'release defines a build matrix'
for target in 'os: darwin' 'arch: arm64' 'arch: amd64' 'os: linux'; do
  assert_contains "$release" "$target" "release matrix includes $target"
done
[[ $(grep -Ec '^          - os:' "$release") -eq 4 ]] || { echo "FAIL: release matrix has exactly four targets" >&2; exit 1; }
echo 'PASS: release matrix has exactly four targets'
assert_contains "$release" 'CGO_ENABLED: 0' 'release builds with CGO disabled'
assert_contains "$release" 'go build -ldflags .*main\.Version=.*TAG' 'release embeds the selected tag in the binary'
assert_contains "$release" 'tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner' 'release archives have deterministic tar metadata'
assert_contains "$release" 'gzip -n' 'release archives suppress gzip timestamps'
assert_contains "$release" 'sha256sum polytoken-quota-' 'release checksums cover archives'
assert_contains "$release" '> checksums.txt' 'release writes checksums.txt'
assert_contains "$release" 'gh release upload .*checksums.txt.*--clobber' 'release uploads all assets with clobber'

# Exercise the archive recipe locally without invoking GitHub or the Go toolchain.
archive_tmp=$(mktemp -d)
trap 'rm -rf "$archive_tmp"' EXIT
mkdir -p "$archive_tmp/bin"
printf 'synthetic binary\\n' > "$archive_tmp/bin/polytoken-quota"
printf 'v1.2.3\\n' > "$archive_tmp/bin/VERSION"
chmod 0755 "$archive_tmp/bin/polytoken-quota"
chmod 0644 "$archive_tmp/bin/VERSION"
for pass in one two; do
  (cd "$archive_tmp/bin" && tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=go+u,go-w -cf - polytoken-quota VERSION | gzip -n > "../polytoken-quota-linux-amd64-$pass.tar.gz")
done
cmp "$archive_tmp/polytoken-quota-linux-amd64-one.tar.gz" "$archive_tmp/polytoken-quota-linux-amd64-two.tar.gz"
tar -tzf "$archive_tmp/polytoken-quota-linux-amd64-one.tar.gz" | diff -u <(printf 'polytoken-quota\nVERSION\n') -
echo 'PASS: deterministic archive recipe is repeatable with sorted members, normalized ownership, modes, mtime, and gzip'

echo "workflow contracts passed (${#workflows[@]} file(s))"
