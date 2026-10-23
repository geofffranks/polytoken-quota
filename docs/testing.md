# Testing

Use the Go version in `go.mod`. Tests use synthetic data, private temporary roots, and supplied clocks; do not run them against live configuration or accounts. The real Polytoken contract suite is opt-in and is not required for ordinary checks.

## Repository checks

```sh
go test ./... -count=1 -timeout=5m
go test -race ./... -count=1 -timeout=10m
go vet ./...
go build ./...
scripts/test-workflows.sh
scripts/test-property-report.sh
swiftbar/tests/run_tests.sh
```

The SwiftBar fixture script's provider-order assertion uses GNU sed's basic-regex alternation. On macOS, put GNU sed on `PATH` as `sed` when running that script; the plugin itself does not acquire this test-only dependency.

The workflow-policy script also cross-builds and packages release archives. Its packaging check requires GNU tar for deterministic timestamp options. It prefers `tar` on `PATH`, then checks the standard Homebrew GNU tar locations; if neither is available, install GNU tar and put it first on `PATH` rather than skipping or weakening the check.

## Property suites and budgets

The ordinary `go test ./...` and race runs use deterministic fixed-domain properties and the existing Rapid properties with CI defaults (`RAPID_CHECKS=100`, `RAPID_SEED=1`, `RAPID_SHRINKTIME=10s`). Publisher filesystem histories run a bounded representative baseline: ten single-file seeds cycle every journal/backup/temp/rename/state fault boundary, while ten two-/three-file seeds cycle every post-journal boundary and fail at every live-rename position (including the second rename). Full mode expands single-file histories to 80 seeds and multi-file histories to 48; repeated filesystem histories can make the publish package the longest default package. All test files and publisher artifacts use private `t.TempDir` roots and synthetic bytes. These tests model recovery outcomes and do **not** claim a multi-file atomic reader snapshot.

The retained suite inventory is:

| Area | Tests | Mode | Model boundary |
|---|---|---|---|
| Quota, selection, policy import, target ownership | deterministic boundary tests and Rapid generators | fast + full Rapid | synthetic states/files; no live provider calls |
| Notice delivery and reload authority | generated histories with fake HTTP transport and synthetic session marker | fast + full | fake transport, not a live daemon; tests authority checks, not reservation against a new turn |
| Coordinator retry and partial target writes | finite schedules and fake publishers | fast + full | partial-history model does not implement filesystem journaling |
| Publisher recovery and reader visibility | real OS filesystem/journal under temporary roots | bounded default + full | final recovery is asserted; intermediate multi-file visibility may be mixed |
| Native Go fuzz targets | explicit allowlist in `property-discovery.yml` | Linux discovery only | offline synthetic inputs; one target at a time |

For local full filesystem discovery, record the positive seed and use an explicit finite case budget. The same configuration and revision reproduce the generated schedule; changing generator code or Go/Rapid versions can change behavior.

```sh
PUBLISH_HISTORY_MODE=full PUBLISH_HISTORY_SEED=20261022 PUBLISH_MULTI_SEED=20261022 \
  go test ./internal/publish ./internal/notice ./internal/service \
  -run 'Generated|History|ReloadAuthority|ReaderAtRename' -count=1 -timeout=15m
```

Publisher defaults are bounded; use `PUBLISH_HISTORY_CASES` (1–500) to set the single-file case budget and `PUBLISH_MULTI_CASES` (1–500) to set the multi-file case budget. `PUBLISH_MULTI_SEED` selects the first multi-file seed; `PUBLISH_HISTORY_SEED` selects the first single-file seed. Seed variables must be positive decimal integers. `PUBLISH_HISTORY_MODE=full` expands the default single-file histories to 80 cases. Notice history counts can be set with `NOTICE_HISTORY_CASES` or `NOTICE_AUTHORITY_CASES` (1–500). Seeds alone do not guarantee replay across revisions: record tested SHA, Go version, platform, mode and budget.

For weekly/manual Linux Rapid discovery, the workflow uses 1,000 checks and a run-specific seed (manual `replay_seed` overrides it). Fixed-domain exhaustive loops remain fixed; varying Rapid does not broaden those finite cases. Each allowlisted native fuzz target runs separately with `-parallel=1`, 60 seconds and a finite 90-second Go timeout; no retries hide failures.

## Summaries, artifacts and triage

Each discovery job writes a safe manifest before starting tests with suite, platform, tested SHA, Go version, seed, case/check/fuzz budget and exact replay command. Suite outputs distinguish test failure from timeout/infrastructure where Actions exposes the step outcome; a hard job timeout, cancellation or runner loss can prevent later summary/artifact steps, so missing evidence remains inconclusive. Artifacts are scoped to manifests, allowlisted Rapid `.fail` files, and the six explicitly listed fuzz target corpus directories; they exclude logs, workspace/config/environment trees and arbitrary temp files. Discovery artifacts are retained 14 days; existing PR Rapid failure retention remains seven days.

The reporter has no checkout and no artifact input. It validates repository, default branch, event, GitHub server URL, SHA and run ID from trusted Actions context; receives only `issues: write`; and uses fixed safe text and suite/platform markers. It paginates open issues and recognizes only an open, bot-authored issue with the exact immutable marker-only body; issue bodies are never updated. Run history lives in a bot-authored marked comment, and an existing comment is updated only when its complete generated schema and every Actions link validate. Otherwise a new fixed comment is created without copying prior comment text. The comment retains at most ten validated Actions run links; human issue text and comments are never edited. Closed issues are not reopened; a later failure can create a fresh issue. Successful, cancelled, and skipped suites are not reported. Any failed discovery job is described as failed or inconclusive pending inspection, so timeout or infrastructure trouble is not misrepresented as a minimized counterexample. Each suite/platform API error is included in the reporter summary without preventing attempts for other categories; no issue is auto-closed. The local fixture extracts and executes this exact inline JavaScript and replay-seed validation against mocked APIs and does not create test issues.

Triage procedure:

1. Download the scoped artifact before expiry; preserve its revision, Go/platform and seed/budget manifest.
2. Replay on the recorded revision using the exact package/test/seed command in the run summary. Distinguish property failure from timeout, cancellation or runner/infrastructure failure.
3. Inspect the model and production behavior independently; do not treat an automated issue as a confirmed product bug.
4. Minimize the case and turn the essential behavior into a readable deterministic passing regression with a focused fix.
5. Do not commit opaque `.fail` or fuzz corpus files without a reviewed reason; never copy raw failures into issue text.

## Existing Rapid reproduction

The state, reconciliation, document, and routing packages use test-only `pgregory.net/rapid`, pinned to v1.3.0. For local broader Rapid discovery, select a seed and finite sample budget:

```sh
RAPID_CHECKS=1000 RAPID_SEED=20261022 RAPID_SHRINKTIME=10s \
  go test ./internal/state ./internal/reconcile ./internal/document ./internal/routing \
  -count=1 -timeout=5m
```

Select an exact property with Go's `-run '^TestName$'` when investigating a failure. Always use `-count=1` so the test cache does not replace generation. Rapid's shrink budget is cooperative and does not preempt an individual test case; keep the Go test deadline as well.

Before changing a generator or updating Rapid, preserve the exact test name, semantic trace, synthetic initial facts, revision, Go version, module versions, seed and command. Rapid automatically replays local failure files before new samples; an invalid or version-mismatched file can be ignored, so a green run alone does not prove replay. Confirm the reported case ran, then promote a confirmed defect into a readable deterministic regression. Classify failures as production contract violation, model/generator defect, or unresolved contract ambiguity; do not weaken an independent expectation merely to match production.

## Native malformed-input fuzzing

List target names with:

```sh
go test ./internal/quota ./internal/reconcile ./internal/document -list '^Fuzz'
```

Run one target at a time from its package, replacing `FuzzTarget` with a listed name:

```sh
go test ./internal/document -run='^$' -fuzz='^FuzzTarget$' \
  -fuzztime=60s -parallel=1 -timeout=90s
```

Normal `go test` runs fuzz seed corpora but does not perform timed fuzz discovery. Discovery requires no daemon, live account, remote assessment or external service. Repository Actions settings and actual scheduled issue creation remain operational checks; they are not exercised by local fixtures.
