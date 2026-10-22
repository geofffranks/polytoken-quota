# Testing

Use the Go version in `go.mod`. Tests use synthetic data, private temporary roots, and supplied clocks; do not run them against live configuration or accounts. The real Polytoken contract suite is opt-in and is not required for ordinary checks.

## Repository checks

```sh
go test ./... -count=1 -timeout=5m
go test -race ./... -count=1 -timeout=10m
go vet ./...
go build ./...
scripts/test-workflows.sh
swiftbar/tests/run_tests.sh
```

The SwiftBar fixture script's provider-order assertion uses GNU sed's basic-regex alternation. On macOS, put GNU sed on `PATH` as `sed` when running that script; the plugin itself does not acquire this test-only dependency.

The workflow-policy script also cross-builds and packages release archives. Its packaging check requires a `tar` supporting deterministic timestamp options; if the host's BSD tar rejects `--mtime`, run it with GNU tar first on `PATH` rather than skipping the check.

If the backup-retention integration fixture fails on macOS while using a symlinked temporary directory, supply a fully resolved private `TMPDIR`. The fixture seeds backups by path string while target resolution canonicalizes paths, so both spellings must match:

```sh
mkdir -p /absolute/private/test-tmp
TMPDIR="$(cd /absolute/private/test-tmp && pwd -P)" \
  go test ./... -count=1 -timeout=5m
```

Resolve every symlink component, including any in your home/state directory.

## Structured properties

The state, reconciliation, document, and routing packages use test-only `pgregory.net/rapid`, pinned to v1.3.0. Ordinary tests run the properties without an opt-in flag. CI supplies `RAPID_CHECKS=100`, `RAPID_SEED=1`, and `RAPID_SHRINKTIME=10s` to both normal and race tests. These environment variables avoid passing Rapid-specific flags to packages that do not import Rapid.

A fixed CI seed is a reproducible baseline, not ongoing discovery. For local discovery, choose and record a nonzero seed and use a larger finite sample budget:

```sh
RAPID_CHECKS=1000 RAPID_SEED=20261022 RAPID_SHRINKTIME=10s \
  go test ./internal/state ./internal/reconcile ./internal/document ./internal/routing \
  -count=1 -timeout=5m
```

Repeat with another recorded seed, such as `20261023`. Select a package and exact property with Go's `-run '^TestName$'` when investigating one failure. Always use `-count=1` so the test cache does not replace generation.

The suites bound providers, chains, operation histories, and document sizes. The shrink budget is cooperative: it does not preempt an individual test case. Keep the Go test deadline as well.

## Preserve and reproduce a failure

Before changing a generator or updating Rapid, preserve:

- The exact test name, semantic trace and synthetic initial facts from test output.
- The generated `testdata/rapid/.../*.fail` file and reported replay command.
- The revision (`git rev-parse HEAD`), Go version (`go env GOVERSION`), module versions (`go list -m all`), seed and command.

Rapid automatically replays retained local failure files before new samples, so total executions can exceed `RAPID_CHECKS`. Failure files are ignored by Git. CI uploads only `**/testdata/rapid/**/*.fail` after failure and retains them for seven days; it does not upload workspace configuration or arbitrary logs. Download useful artifacts before they expire.

For an explicit replay, select only the affected package/property and use the saved file's absolute path:

```sh
RAPID_FAILFILE=/absolute/path/to/case.fail RAPID_CHECKS=1 RAPID_SEED=1 \
  RAPID_SHRINKTIME=10s go test ./internal/state \
  -run '^TestName$' -count=1 -timeout=2m -v
```

Replace the package and `TestName` with the failure's actual values. `RAPID_FAILFILE` is the environment equivalent of the emitted `-rapid.failfile` flag.

Replaying depends on the generator and Rapid version. An invalid or version-mismatched file can be ignored; a green run alone does not prove that such a file replayed. Use the emitted replay command in the pinned environment and confirm the reported case actually ran. Once a confirmed defect is resolved, promote its essential facts into a readable deterministic passing regression. Do not commit opaque failure files or red tests.

Classify a failure before repairing it: production contract violation, model/generator defect, or unresolved contract ambiguity. Do not accept arbitrary errors as successful checks or weaken an independent expectation to match production. Production changes require their own authorized scope.

## Native malformed-input fuzzing

The existing Go fuzz targets remain separate from structured properties. List target names with:

```sh
go test ./internal/state ./internal/reconcile ./internal/document -list '^Fuzz'
```

Run one target at a time from its package, replacing `FuzzTarget` with a listed name:

```sh
go test ./internal/document -run='^$' -fuzz='^FuzzTarget$' \
  -fuzztime=20s -timeout=2m
```

Keep a native fuzz failure's corpus input and the exact reproduction command. Normal `go test` runs existing seed corpora but does not perform timed fuzz discovery. Neither local discovery method requires a daemon, live account, remote assessment, or scheduled service.
