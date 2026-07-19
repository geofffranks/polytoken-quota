# polytoken-quota

`polytoken-quota` is a short-lived Go CLI that consumes the [CodexBar](https://github.com) hook
contract, maintains durable independent quota/availability state, and safely reconciles only the
explicitly managed Polytoken model fields across a global target and registered project targets.

> **Status:** the command tree, typed adapters, and exit-code mapping are in place. The reconciler
> state machine, store, and daemon coordinator are wired by later tasks. Until then the binary is a
> parse-and-dispatch shell.

## Safety and non-goals (non-negotiable)

- **No daemon / process control.** This tool never inspects, restarts, signals, or otherwise controls
  a live Polytoken daemon or session. `status` always advises that existing sessions may need a user
  restart/reload.
- **No live accounts or credentials.** Provider credentials, auth blocks, inherited secrets, and raw
  unrelated config are never persisted or echoed in diagnostics. All diagnostic output is sanitized.
- **Scoped ownership.** Only exact managed fields in explicitly registered definition files are
  modified. The tool never scans arbitrary workspace roots or adopts new files implicitly.
- **Validation isolation.** Validation runs against a complete standalone staging root with a neutral
  working directory containing no `.polytoken` — never against live files.

## Minimum versions

| Component | Minimum | Notes |
|-----------|---------|-------|
| Go toolchain | `go1.26.5` | Exact version match (`go env GOVERSION`). |
| CodexBar | `0.44.0` | Hook contract. |
| Polytoken | `0.5.0-unstable.10` | Supported binary; publication gated on its contract tests. |

## Install

Local `go build` / `go install` first. CI and release packaging are **deferred** and not part of this
repository yet.

```sh
# Build the binary into the current directory.
go build -o polytoken-quota ./cmd/polytoken-quota

# Or install into $GOBIN / $GOPATH/bin.
go install ./cmd/polytoken-quota
```

## Commands

| Command | Description |
|---------|-------------|
| `init` | Create the initial `desired.yaml` from the current managed state. |
| `hook` | Read a CodexBar hook event from stdin and apply it. |
| `status [--json]` | Show current quota/availability state and drift. |
| `reconcile [--dry-run]` | Reconcile managed Polytoken fields toward desired state. |
| `sync --from-polytoken [--force]` | Sync desired state from the live Polytoken config. |
| `state set <provider> [--quota X] [--availability Y]` | Set a provider's managed state. |
| `state clear <provider> \| --all` | Clear a provider's (or all) managed state. |
| `doctor [--json]` | Run health and drift diagnostics. |

### `init` is strict create-only

v1 `init` has **no overwrite flag**. If `desired.yaml` already exists, `init` exits `1` and instructs
you to update it with `sync --from-polytoken` instead:

```sh
# desired.yaml already exists:
$ polytoken-quota init
# exit 1 — run: polytoken-quota sync --from-polytoken
```

To refresh an existing `desired.yaml`, use `sync --from-polytoken` (add `--force` to overwrite local
changes).

### What `init` proposes

`init` discovers the current global managed references and baseline model enablement and writes a
starter `desired.yaml`. It:

- proposes **only** definition files that already contain `polytoken.model` or
  `polytoken.fallback_models`; a file with a `polytoken` block but no model field is skipped;
- materializes **exact concrete model enumeration** verbatim from the live provider mappings — there
  is no implicit runtime model mapping;
- reports any model-bearing definition whose chain does not resolve against a provider mapping as an
  uncovered reference (a `doctor` finding) rather than silently proposing an unresolved chain;
- prints CodExBar setup instructions (the six events, the 0.44.0 minimum, and direct shell-free
  invocation) and **never** edits CodExBar for you.

### `sync --from-polytoken` is guarded

`sync` adopts the current managed fields as desired intent, but refuses while any provider is degraded
or when managed drift is ambiguous, unless `--force` is given:

- a managed definition that references a model outside the provider graph is **ambiguous drift**: it is
  reported, never silently adopted;
- unmanaged live edits remain valid and are preserved;
- forced sync emits a warning that the **temporary ordering may become durable intent**.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Success, or (for `status`/`doctor`) no action required. |
| `1` | Rejected: invalid command/syntax, a refused mutation (e.g. `init` refusing to overwrite `desired.yaml`), or actionable `doctor` findings. |
| `2` | Mutation accepted but one or more targets remain pending (not fully reconciled). |

`status` is always informational and exits `0` regardless of drift. `doctor` exits `1` only when its
findings are actionable.
