# AGENTS.md — polytoken-quota reconciler

Short-lived Go CLI (`polytoken-quota`) that consumes the CodexBar hook contract,
maintains durable independent quota/availability state, and safely reconciles only
explicitly managed Polytoken model fields across a global target and registered
project targets.

Approved spec: `docs/superpowers/polytoken-quota-reconciler/design_spec.md`
Approved plan: `docs/superpowers/polytoken-quota-reconciler/plan.md`

> These artifacts are **not** committed (see Artifact Policy). They live at the
> canonical home above and are linked here for reference only.

## Toolchain and platform matrix

- **Go toolchain:** the `go` directive in `go.mod` is authoritative. CI verifies `go env GOVERSION` matches it; the documented copies in `README.md` and this file are maintained by the Go version workflow.
- **Module path:** `github.com/geofffranks/polytoken-quota`
- **Supported targets (GOOS/GOARCH):** `darwin/arm64`, `darwin/amd64`,
  `linux/amd64`, `linux/arm64`
- **Polytoken contract binary:** resolved from `PATH` (currently
  `0.6.1`), overridable via `POLYTOKEN_BINARY`.
  Publication is gated on passing its complete-root contract tests. Version policy:
  minimum-current — keep the supported binary at the latest stable release.
- **CodexBar minimum:** `0.44.0` (hook contract).

## Install / release convention

Build locally with `go build` or `go install`, or download a published release archive as documented in `README.md`. Release assets are exactly `polytoken-quota-darwin-arm64.tar.gz`, `polytoken-quota-darwin-amd64.tar.gz`, `polytoken-quota-linux-arm64.tar.gz`, and `polytoken-quota-linux-amd64.tar.gz`, plus `checksums.txt`. No release is assumed to exist yet. The release/install file set is `README.md` only (`INSTALL_CONVENTION_FILES=README.md`).

The `go` directive in `go.mod` is the sole authority for the required Go version; the current directive is `go 1.26.5`. CI and release use `actions/setup-go` with `go-version-file: go.mod` and verify `go env GOVERSION` against that directive; the copies in `README.md` and this file are maintained by the Go Version Bump workflow.

The six repository automation surfaces are:

- **CI** (`.github/workflows/ci.yml`): pull requests and pushes to `main`; test, race-test, vet, and build.
- **Release** (`.github/workflows/release.yml`): build the four archives for an existing `vX.Y.Z` GitHub release and upload checksums.
- **Weekly Patch Release** (`.github/workflows/weekly-patch-release.yml`): Monday `09:00 UTC` plus manual dispatch; create the next patch release only after a prior stable release exists.
- **Dependabot Auto-Merge** (`.github/workflows/dependabot-auto-merge.yml`): metadata-only patch/minor dependency updates with auto-merge.
- **Go Version Bump** (`.github/workflows/go-version-bump.yml`): Monday `10:00 UTC` plus manual dispatch; update `go.mod`, `go.sum`, and documentation through an auto-merged pull request.
- **Dependabot** (`.github/dependabot.yml`): weekly Go module and GitHub Actions update proposals.

Before relying on weekly automation, a maintainer must manually create the first stable `vX.Y.Z` GitHub release and matching repository tag, then manually dispatch **Release** with that tag to publish its archives. The weekly workflow deliberately refuses to create the first release. If the Release workflow cannot be dispatched, create the release and run the release packaging workflow manually from its `workflow_dispatch` input; do not remove the first-release guard.

Repository settings required for these workflows: enable **Allow auto-merge**; set Actions workflow permissions to **Read and write permissions** (the workflows still declare least-privilege job permissions); and allow GitHub Actions to create and approve pull requests where repository policy requires that setting. Weekly release requires `contents: write` and `actions: write`; Dependabot auto-merge and Go Version Bump require `contents: write` and `pull-requests: write`.

## Commands

| Purpose | Command |
|---|---|
| Build | `go build ./...` |
| Install | `go install ./cmd/polytoken-quota` |
| Test (all) | `go test ./... -count=1` |
| Test (focused) | `go test ./internal/<pkg> -run <Test> -count=1` |
| Race | `go test -race ./...` |
| Vet | `go vet ./...` |
| Fuzz | `go test -run=^$ -fuzz=<FuzzTarget> -fuzztime=20s` |
| Workflow policy | `scripts/test-workflows.sh` |
| Contract | `scripts/test-contract.sh` (opt-in external binary) |

Contract tests invoke the real Polytoken binary against complete private staging roots; they are opt-in and never run as part of the default `go test ./...`. They require `POLYTOKEN_BINARY` or a `polytoken` binary on `PATH`, and they must not target live configuration.

## Artifact policy

- Keep the approved design and plan **outside commits**. They are linked from this
  file and from the local setup record, never added to a repository commit.
- `.setup/`, `.tickets/`, `.superpowers/`, `.worktrees/`, `.polytoken/`, the
  `docs/superpowers/` planning tree, and temporary research/validation scratch
  directories are gitignored and must remain uncommitted.
- Never commit generated secret-bearing fixtures, red tests, or the design/plan.
- Do **not** delete temporary research scratch directories; leave them ignored.

## Security rules (non-negotiable)

- **No live accounts / credentials.** Never persist account names, provider
  credentials, auth blocks, inherited secrets, or raw unrelated config. Sanitize all
  diagnostics and command output. Transient staging is the sole narrowly scoped
  exception and must be private and always deleted.
- **No daemon / process control.** Never inspect, restart, signal, or otherwise
  control a live Polytoken daemon/session. `status` must always advise that existing
  sessions may need a user restart/reload.
- **Scoped ownership.** Modify only exact managed fields in explicitly registered
  definition files. Never scan arbitrary workspace roots or adopt new files implicitly.
- **Validation isolation.** Validate against a complete standalone staging root with
  a neutral working directory containing no `.polytoken`; never against live files.

## Commits

Commit after each green task with the plan's imperative message. Never commit red
tests, the design, or the plan. Work happens on `feat/polytoken-quota-reconciler` in
its isolated worktree, never on `main`.
