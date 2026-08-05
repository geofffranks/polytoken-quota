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

- **Go toolchain:** `go1.26.5` (exact). The gate verifies `go env GOVERSION` equals this.
- **Module path:** `github.com/geofffranks/polytoken-quota`
- **Supported targets (GOOS/GOARCH):** `darwin/arm64`, `darwin/amd64`,
  `linux/amd64`, `linux/arm64`
- **Polytoken contract binary:** resolved from `PATH` (currently
  `0.6.1`), overridable via `POLYTOKEN_BINARY`.
  Publication is gated on passing its complete-root contract tests. Version policy:
  minimum-current — keep the supported binary at the latest stable release.
- **CodexBar minimum:** `0.44.0` (hook contract).

## Install / release convention

Local `go build` / `go install` first. CI and release packaging are **deferred** and
not part of this repository yet. The release/install file set is `README.md` only
(`INSTALL_CONVENTION_FILES=README.md`); it documents install and CodexBar setup and
is produced in a later task.

## Commands

Standard commands (module is bootstrapped in a later task; until `go.mod` exists
these are the canonical commands to use once code lands):

| Purpose        | Command                                            |
|---------------|---------------------------------------------------|
| Build         | `go build ./...`                                   |
| Test (all)    | `go test ./...`                                    |
| Test (focused)| `go test ./internal/<pkg> -run <Test> -count=1`    |
| Race          | `go test -race ./...`                              |
| Vet / lint    | `go vet ./...`                                     |
| Fuzz          | `go test -run=^$ -fuzz=<FuzzTarget> -fuzztime=20s` |
| Contract      | `scripts/test-contract.sh` (opt-in external binary)|

Contract tests invoke the real Polytoken binary against complete private staging
roots; they are opt-in and never run as part of the default `go test ./...`.

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
