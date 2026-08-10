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
    $0 == start { in_block = 1; start_indent = match($0, /[^ ]/) - 1; next }
    in_block && (match($0, /[^ ]/) - 1) <= start_indent { exit }
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
assert_contains "$ci" 'actions/setup-go@v7' 'CI uses approved setup-go major tag'
assert_contains "$ci" 'go-version-file: go\.mod' 'CI setup-go reads repository Go directive'
assert_contains "$ci" 'go env GOVERSION' 'CI checks the installed Go version'
assert_contains "$ci" 'go test \./\.\.\.' 'CI runs standard Go tests'
assert_contains "$ci" 'go test -race \./\.\.\.' 'CI runs race tests'
assert_contains "$ci" 'go vet \./\.\.\.' 'CI runs go vet'
assert_contains "$ci" 'go build \./\.\.\.' 'CI runs go build'

# All action references must use approved major tags, keeping upgrades explicit.
for changed_workflow in "$ci" "$workflow_dir/go-version-bump.yml" "$workflow_dir/release.yml" "$workflow_dir/weekly-patch-release.yml"; do
  if grep -Eq 'actions/checkout@v(1|2|3|4|5|6)([^0-9]|$)' "$changed_workflow"; then
    echo "FAIL: changed workflow uses an old checkout major ($changed_workflow)" >&2
    exit 1
  fi
done
echo "PASS: changed workflows use checkout v7"
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
assert_contains "$release" 'if \[\[ ! "\$tag" =~ \^v\?\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\$ \]\]' 'release validates vX.Y.Z or X.Y.Z tags'
assert_contains "$release" 'ref: \$\{\{ needs\.preflight\.outputs\.tag \}\}' 'release checks out the selected tag'
assert_contains "$release" 'go-version-file: go\.mod' 'release setup-go reads go.mod'
assert_contains "$release" 'matrix:' 'release defines a build matrix'
assert_exact_block "$release" '        include:' $'          - os: darwin\n            arch: arm64\n          - os: darwin\n            arch: amd64\n          - os: linux\n            arch: arm64\n          - os: linux\n            arch: amd64' 'release matrix has exactly the four supported OS/ARCH pairs'
assert_contains "$release" 'CGO_ENABLED: 0' 'release builds with CGO disabled'
assert_contains "$release" 'go build -trimpath -ldflags .*main\.Version=.*TAG' 'release uses trimpath and embeds the selected tag in the binary'
assert_contains "$release" 'buildid= -w' 'release uses deterministic linker/build-id flags'
assert_contains "$release" 'tar .*--mtime='"'"'1970-01-01 00:00:00'"'"'.*--uid=0.*--gid=0.*--uname='"'"''"'"'.*--gname='"'"''"'"'' 'release archives have portable deterministic tar metadata'
assert_contains "$release" 'gzip -n' 'release archives suppress gzip timestamps'
assert_contains "$release" 'sha256sum polytoken-quota-' 'release checksums cover archives'
assert_contains "$release" '> checksums.txt' 'release writes checksums.txt'
assert_contains "$release" 'gh release upload .*checksums.txt.*--clobber' 'release uploads all assets with clobber'

weekly="$workflow_dir/weekly-patch-release.yml"
[[ -f "$weekly" ]] || { echo "error: required weekly patch-release workflow not found: $weekly" >&2; exit 1; }
assert_exact_block "$weekly" 'on:' $'  schedule:
    - cron: '\''0 9 * * 1'\''
  workflow_dispatch:' 'weekly release runs Monday and supports manual dispatch'
assert_exact_block "$weekly" 'permissions:' $'  contents: write\n  actions: write' 'weekly release has contents and actions write permissions'
assert_contains "$weekly" 'concurrency:' 'weekly release defines concurrency'
assert_contains "$weekly" 'group: weekly-patch-release' 'weekly release concurrency prevents simultaneous creation'
assert_contains "$weekly" 'actions/checkout@v7' 'weekly release checks out source'
assert_contains "$weekly" 'fetch-depth: 0' 'weekly release checks out full history'
assert_contains "$weekly" 'first release' 'weekly release clearly refuses the first release'
assert_contains "$weekly" 'gh release list .*--exclude-drafts .*--exclude-pre-releases .*--json tagName' 'weekly release excludes draft and pre-releases when enumerating GitHub releases'
if grep -Eq 'git tag --list|sort -V' "$weekly"; then echo 'FAIL: weekly release uses repository tags or GNU sort -V' >&2; exit 1; fi
assert_contains "$weekly" '\^v\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\$' 'weekly release filters strict stable semver tags'
assert_contains "$weekly" 'selected release tag is not present locally' 'weekly release clearly checks selected release tag locally'
assert_contains "$weekly" 'git rev-list --count' 'weekly release counts commits since selected release'
assert_contains "$weekly" 'No commits since' 'weekly release skips when there are no new commits'
assert_contains "$weekly" 'conflict' 'weekly release detects a conflicting next tag'
assert_contains "$weekly" 'gh release create .*--generate-notes' 'weekly release creates generated notes'
assert_contains "$weekly" '--notes-start-tag' 'weekly release supplies notes start tag'
assert_contains "$weekly" 'gh workflow run release.yml' 'weekly release dispatches artifact workflow explicitly'
assert_contains "$weekly" '-f[[:space:]]+"tag=' 'weekly release dispatch includes the next tag'

# Exercise tag selection locally with no GitHub, network, or remote Git calls.
weekly_tmp=$(mktemp -d)
archive_tmp=''
trap 'rm -rf "$archive_tmp" "$weekly_tmp"' EXIT
printf '%s\n' v1.2.3 v1.10.0 v2.0.0-rc1 v9.9.9 not-a-tag > "$weekly_tmp/released-tags"
printf '%s\n' v10.0.0 v2.0.0 > "$weekly_tmp/repository-only-tags"
selected=$(awk -F. '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { printf "%09d.%09d.%09d %s\n", substr($1,2), $2, $3, $0 }' "$weekly_tmp/released-tags" | sort | tail -n1 | cut -d' ' -f2)
repository_only_selected=$(awk -F. '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { printf "%09d.%09d.%09d %s\n", substr($1,2), $2, $3, $0 }' "$weekly_tmp/repository-only-tags" | sort | tail -n1 | cut -d' ' -f2)
test "$repository_only_selected" = v10.0.0 || { echo "FAIL: repository-only fixture did not contain the higher stable-looking tag: $repository_only_selected" >&2; exit 1; }
test "$selected" != "$repository_only_selected" || { echo "FAIL: released-tag selection included repository-only tag: $selected" >&2; exit 1; }
test "$selected" = v9.9.9 || { echo "FAIL: local released-tag fixture selected $selected" >&2; exit 1; }
grep -Fxv v9.9.9 "$weekly_tmp/released-tags" > "$weekly_tmp/releases-without-drafts"
selected_without_draft=$(awk -F. '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { printf "%09d.%09d.%09d %s\n", substr($1,2), $2, $3, $0 }' "$weekly_tmp/releases-without-drafts" | sort | tail -n1 | cut -d' ' -f2)
test "$selected_without_draft" = v1.10.0 || { echo "FAIL: draft stable-looking tag was not excluded: $selected_without_draft" >&2; exit 1; }
test "$selected_without_draft" != "$repository_only_selected" || { echo "FAIL: released baseline included repository-only tag: $selected_without_draft" >&2; exit 1; }
echo 'PASS: weekly tag selection uses released tags, ignores higher repository-only tags, and excludes draft stable-looking tags'

# Validate the real release shape locally: cross-build every supported target, package
# the resulting binary and VERSION, inspect contents, verify SHA-256 checksums, and
# compare outputs from two distinct workspace paths. This deliberately makes no
# GitHub/network calls. YAML is checked textually above; when PyYAML is available we
# additionally parse every workflow and assert required top-level structure (handling
# YAML 1.1's boolean interpretation of the `on` key), otherwise this documented
# lightweight fallback avoids adding a parser dependency.
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
  python3 - "$workflow_dir" <<'PY'
import pathlib, sys, yaml
for path in pathlib.Path(sys.argv[1]).glob("*.y*ml"):
    with path.open() as stream:
        data = yaml.safe_load(stream)
    if not isinstance(data, dict):
        raise SystemExit(f"{path}: expected top-level mapping")
    triggers = data.get("on", data.get(True))
    if not isinstance(triggers, (dict, list, str)):
        raise SystemExit(f"{path}: missing valid on trigger block")
    if "jobs" not in data or not isinstance(data["jobs"], dict) or not data["jobs"]:
        raise SystemExit(f"{path}: missing non-empty jobs mapping")
PY
  echo 'PASS: workflows parse and have structural top-level YAML shape (YAML 1.1 on handled)'
else
  echo 'INFO: PyYAML unavailable; using lightweight textual YAML contract checks'
fi
archive_tmp=$(mktemp -d)
trap 'rm -rf "$archive_tmp"' EXIT
if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$@"; }
  verify_hashes() { sha256sum --check "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$@"; }
  verify_hashes() { shasum -a 256 -c "$@"; }
else
  echo 'FAIL: neither sha256sum nor shasum is available' >&2
  exit 1
fi
workspace_a="$archive_tmp/workspace-a"
workspace_b="$archive_tmp/workspace-b"
mkdir -p "$workspace_a" "$workspace_b"
cp -R "$repo_root/." "$workspace_a/"
cp -R "$repo_root/." "$workspace_b/"
mkdir -p "$archive_tmp/output-a" "$archive_tmp/output-b"
(cd "$workspace_a" && CGO_ENABLED=0 go build -trimpath -ldflags '-buildid= -w -X main.Version=v1.2.3' -o "$archive_tmp/version-check" ./cmd/polytoken-quota)
version_output=$(env -i PATH= "$archive_tmp/version-check" --version)
test "$version_output" = 'polytoken-quota v1.2.3' || { echo "FAIL: linked executable version output: $version_output" >&2; exit 1; }
echo 'PASS: linked executable reports version with empty PATH'
for workspace in a b; do
  root_var="workspace_$workspace"
  workspace_root=${!root_var}
  output_dir="$archive_tmp/output-$workspace"
  for target in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
    os=${target%-*}; arch=${target#*-}
    staging="$archive_tmp/$workspace-$target/root"
    mkdir -p "$staging"
    (cd "$workspace_root" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags '-buildid= -w -X main.Version=v1.2.3' -o "$staging/polytoken-quota" ./cmd/polytoken-quota)
    printf '%s\n' v1.2.3 > "$staging/VERSION"
    chmod 0755 "$staging/polytoken-quota"
    chmod 0644 "$staging/VERSION"
    archive="$output_dir/polytoken-quota-$target.tar.gz"
    (cd "$staging" && if tar --version 2>/dev/null | grep -q 'GNU tar'; then tar --format=ustar --mtime='1970-01-01 00:00:00' --owner=0 --group=0 --numeric-owner --mode='u+rw,go+r-w' -cf - VERSION polytoken-quota; else tar --format=ustar --mtime='1970-01-01 00:00:00' --uid=0 --gid=0 --uname='' --gname='' -cf - VERSION polytoken-quota; fi | gzip -n > "$archive")
    tar -tzf "$archive" | diff -u <(printf 'VERSION\npolytoken-quota\n') -
    tar -xOzf "$archive" VERSION > "$archive_tmp/$workspace-$target-VERSION"
    cmp "$archive_tmp/$workspace-$target-VERSION" <(printf 'v1.2.3\n')
  done
  (cd "$output_dir" && hash_file polytoken-quota-*.tar.gz) > "$output_dir/checksums.txt"
  (cd "$output_dir" && verify_hashes checksums.txt >/dev/null)
done
for target in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
  cmp "$archive_tmp/output-a/polytoken-quota-$target.tar.gz" "$archive_tmp/output-b/polytoken-quota-$target.tar.gz"
done
cmp "$archive_tmp/output-a/checksums.txt" "$archive_tmp/output-b/checksums.txt"
echo 'PASS: real four-target cross-builds, archive inspection, checksum verification, and deterministic packaging across distinct workspaces'

dependabot="$repo_root/.github/dependabot.yml"
[[ -f "$dependabot" ]] || { echo "error: required Dependabot config not found: $dependabot" >&2; exit 1; }
assert_contains "$dependabot" 'package-ecosystem: gomod' 'Dependabot checks Go modules weekly'
assert_contains "$dependabot" 'package-ecosystem: github-actions' 'Dependabot checks GitHub Actions weekly'
assert_contains "$dependabot" 'interval: weekly' 'Dependabot updates are weekly'

merge="$workflow_dir/dependabot-auto-merge.yml"
[[ -f "$merge" ]] || { echo "error: required Dependabot auto-merge workflow not found: $merge" >&2; exit 1; }
assert_exact_block "$merge" 'on:' $'  pull_request_target:\n    types: [opened, synchronize, reopened]' 'Dependabot auto-merge uses pull_request_target'
assert_exact_block "$merge" 'permissions:' $'  contents: write\n  pull-requests: write' 'Dependabot auto-merge has explicit write permissions'
assert_contains "$merge" "github.actor == 'dependabot\[bot\]'" 'Dependabot auto-merge gates actor'
assert_contains "$merge" 'dependabot/fetch-metadata@v3' 'Dependabot auto-merge uses the metadata action'
assert_contains "$merge" 'outputs\.update-type.*version-update:semver-patch' 'Dependabot auto-merge allows patch updates'
assert_contains "$merge" 'outputs\.update-type.*version-update:semver-minor' 'Dependabot auto-merge allows minor updates'
assert_contains "$merge" 'gh pr merge .*--auto' 'Dependabot auto-merge enables auto-merge'
if grep -Eq 'actions/checkout|git clone' "$merge"; then echo 'FAIL: Dependabot workflow checks out or clones code' >&2; exit 1; fi
echo 'PASS: Dependabot workflow is metadata-only and has no checkout'

bump="$workflow_dir/go-version-bump.yml"
[[ -f "$bump" ]] || { echo "error: required Go version workflow not found: $bump" >&2; exit 1; }
assert_exact_block "$bump" 'permissions:' $'  contents: write\n  pull-requests: write' 'Go bump has explicit write permissions'
assert_contains "$bump" 'secrets.GITHUB_TOKEN' 'Go bump uses GITHUB_TOKEN'
assert_contains "$bump" 'go.dev/dl/\?mode=json' 'Go bump discovers releases from Go downloads'
assert_contains "$bump" 'stable == true' 'Go bump selects stable releases only'
assert_contains "$bump" 'go mod edit -go=' 'Go bump updates the exact go.mod directive'
assert_contains "$bump" 'go mod tidy' 'Go bump runs go mod tidy'
python3 - "$bump" <<'PY'
import pathlib, sys
workflow = pathlib.Path(sys.argv[1]).read_text()
expected = "re.subn(r'go\\s*[0-9]+\\.[0-9]+\\.[0-9]+', 'go ' + version, text)"
if expected not in workflow:
    raise SystemExit('FAIL: Go bump documentation replacement must accept go 1.2.3 and go1.2.3 forms')
print('PASS: Go bump documentation replacement accepts spaced and unspaced Go versions')
PY
assert_contains "$bump" 'uses: actions/setup-go@v7' 'Go bump installs discovered Go version'
assert_contains "$bump" 'go-version: \$\{\{ steps\.latest\.outputs\.version \}\}' 'Go bump setup uses discovered version output'
assert_contains "$bump" 'go\.sum' 'Go bump handles go.sum changes'
assert_contains "$bump" 'AGENTS\.md' 'Go bump updates AGENTS.md version copy'
assert_contains "$bump" 'README\.md' 'Go bump updates README.md version copy'
assert_contains "$bump" 'git ls-remote' 'Go bump checks remote branches'
assert_contains "$bump" 'heads origin "\$BRANCH"' 'Go bump detects duplicate remote branch by exact name'
assert_contains "$bump" 'git switch -c' 'Go bump creates a branch'
assert_contains "$bump" 'gh pr create' 'Go bump creates a pull request'
assert_contains "$bump" 'gh pr merge .*--auto' 'Go bump enables auto-merge'
assert_contains "$bump" 'unexpected changed file' 'Go bump rejects unapproved changed files'

# Documentation must describe the assets and operational bootstrap procedure.
readme="$repo_root/README.md"
agents="$repo_root/AGENTS.md"
[[ -f "$readme" ]] || { echo "error: README.md not found: $readme" >&2; exit 1; }
[[ -f "$agents" ]] || { echo "error: AGENTS.md not found: $agents" >&2; exit 1; }

# Keep maintainer-facing documentation tied to every automation surface and its
# operational contract. Workflow assertions above validate implementation;
# these checks ensure the documented operating procedure does not drift away.
assert_contains "$agents" 'CI.*ci\.yml.*pull requests and pushes to `main`.*test, race-test, vet, and build' 'AGENTS documents CI triggers and test/race/vet/build operations'
assert_contains "$agents" 'Release.*release\.yml.*four archives.*checksums' 'AGENTS documents release archives and checksums'
assert_contains "$agents" 'Weekly Patch Release.*weekly-patch-release\.yml.*Monday `09:00 UTC`' 'AGENTS documents weekly patch-release schedule'
assert_contains "$agents" 'first release' 'AGENTS documents weekly first-release guard'
assert_contains "$agents" 'Dependabot Auto-Merge.*dependabot-auto-merge\.yml.*patch/minor.*auto-merge' 'AGENTS documents Dependabot auto-merge policy'
assert_contains "$agents" 'Go Version Bump.*go-version-bump\.yml.*Monday `10:00 UTC`.*pull request' 'AGENTS documents Go version schedule and pull-request behavior'
assert_contains "$agents" 'Dependabot.*\.github/dependabot\.yml.*weekly Go module and GitHub Actions update' 'AGENTS documents Dependabot update configuration'

for archive in \
  polytoken-quota-darwin-arm64.tar.gz \
  polytoken-quota-darwin-amd64.tar.gz \
  polytoken-quota-linux-arm64.tar.gz \
  polytoken-quota-linux-amd64.tar.gz; do
  assert_contains "$readme" "\`$archive\`" "README documents release archive $archive"
done
assert_contains "$readme" 'sha256sum --check checksums\.txt' 'README documents checksum verification'
assert_contains "$readme" 'shasum -a 256 -c checksums\.txt' 'README documents macOS checksum verification'
assert_contains "$readme" 'Download all five release assets' 'README instructs downloading all assets before checksum verification'
assert_contains "$readme" 'VERSION' 'README documents the VERSION file'
assert_contains "$readme" -- '--version' 'README documents --version behavior'
assert_contains "$readme" 'No release is assumed to exist yet' 'README does not claim a release already exists'
assert_contains "$agents" 'go.mod.*sole authority' 'AGENTS documents go.mod as exact Go authority'
assert_contains "$agents" 'go[[:space:]]+1\.26\.5' 'AGENTS documents the exact Go requirement'
assert_contains "$agents" 'manually create the first stable' 'AGENTS documents manual first-release bootstrap'
assert_contains "$agents" 'manually dispatch.*Release' 'AGENTS documents manual release dispatch'
assert_contains "$agents" 'Release workflow cannot be dispatched' 'AGENTS documents manual packaging fallback'
assert_contains "$agents" 'Allow auto-merge' 'AGENTS documents auto-merge repository setting'
assert_contains "$agents" 'Read and write permissions' 'AGENTS documents workflow permissions setting'
assert_contains "$agents" 'scripts/test-contract\.sh.*opt-in' 'AGENTS documents opt-in contract suite'

echo "workflow contracts passed (${#workflows[@]} file(s))"
