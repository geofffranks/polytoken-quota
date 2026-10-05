# SwiftBar plugin: polytoken-quota status

A read-only macOS menu-bar plugin for SwiftBar that shows provider quota
state, per-window utilization bars, reset times, persisted diagnostics, and
the best available pace signal from `polytoken-quota`. The plugin runs two
commands and nothing else: `polytoken-quota status --json` and read-only
`polytoken-quota doctor --json`. It never checks quotas against providers,
reconciles, changes routing, operates a daemon, keeps a status cache, or
stores credentials.

## Requirements

- macOS 11 (Big Sur) or newer. The menu-bar icon uses SF Symbols, which
  require macOS 11+.
- [SwiftBar](https://github.com/swiftbar/SwiftBar).
- `jq` 1.6 or newer, for example via `brew install jq`.
- The `polytoken-quota` CLI **built from this same branch** on the Mac. The
  pace signal and freshness labels come from additive `status --json` fields
  that only this branch's CLI emits, so the branch CLI is required for the
  first install. If your Mac already runs a CLI built from this branch,
  copying the updated plugin alone is enough to get this menu layout — the
  plugin does not require a CLI upgrade just for the layout, and an older
  CLI shows the built-in compatibility warning (details still render; pace
  does not). Build it on the Mac from your checkout of this branch:

  ```sh
  cd /path/to/polytoken-quota
  go build -o "$HOME/.local/bin/polytoken-quota" ./cmd/polytoken-quota
  ```

  Then point the plugin at that exact path with `quota_bin` (see
  Configuration and overrides). Quote the path as shown. This repository
  never executes a host install for you; the build and copy happen on your
  Mac. The quota CLI requires a `polytoken` executable at startup even for
  `status` and `doctor`, so both must resolve.
- Stock Bash 3.2 (the macOS system shell) runs the plugin; no newer Bash and
  no GNU `timeout` are needed.

Get the plugin from this repository, not from a release archive: release
assets contain only the CLI (`README.md` file set) and never ship the plugin.
Copy `contrib/swiftbar/polytoken-quota.1m.sh` into your SwiftBar plugin
folder and make it executable:

```sh
mkdir -p "$HOME/SwiftBarPlugins"
cp /path/to/this/repository/contrib/swiftbar/polytoken-quota.1m.sh \
  "$HOME/SwiftBarPlugins/"
chmod +x "$HOME/SwiftBarPlugins/polytoken-quota.1m.sh"
```

The `.1m` in the filename is the refresh interval: SwiftBar runs the plugin
once per minute. Point SwiftBar at `$HOME/SwiftBarPlugins` (SwiftBar
preferences, "Plugin Folder"), and enable "Launch SwiftBar on login" in the
same preferences pane for launch-at-login. Uninstall by removing the plugin
file from the folder (and, if desired, SwiftBar itself); the plugin keeps no
other files except a temporary directory that is deleted after each run.

Clicking `Refresh status` in the menu makes SwiftBar run the plugin again.
That is the only action: it re-runs the same two read-only commands. It is
not the provider quota check — scheduled `polytoken-quota check` runs (or any
other scheduling you use) update the saved evidence the plugin displays.

## Configuration and overrides

In a GUI launch SwiftBar inherits a minimal `PATH`, so the plugin resolves
`polytoken-quota`, `polytoken`, and `jq` in this order: PATH, then common
locations (`/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin`,
`$HOME/bin`, `$HOME/.local/bin`, `$HOME/go/bin`).

Optional persistent overrides live in a config file next to the plugin
(`polytoken-quota.conf`) or at the path named by the `POLYTOKEN_SWIFTBAR_CONFIG`
environment variable. Format: one `key=value` per line, `#` comments, blank
lines ignored.

```sh
cat > "$HOME/SwiftBarPlugins/polytoken-quota.conf" <<'EOF'
# Executable name (searched like above) or absolute path.
quota_bin=/usr/local/bin/polytoken-quota
polytoken_bin=/usr/local/bin/polytoken
jq_bin=/opt/homebrew/bin/jq
EOF
```

- `quota_bin` — the `polytoken-quota` CLI.
- `polytoken_bin` — the `polytoken` executable the quota CLI requires.
- `jq_bin` — `jq`.

Rules:

- The config file is parsed line by line. It is never sourced or evaluated,
  so it cannot execute anything; only the three keys above are accepted, and
  any other key, a duplicate key, or an empty value is a visible
  configuration error in the menu.
- An explicit override that does not resolve to an executable fails visibly
  with its own warning line. The plugin never silently falls back past a bad
  override.
- Configuration diagnostics name the line number and, for value problems, the
  known key only. Rejected lines, unsupported key names, override values, and
  file paths are never echoed anywhere in the menu.
- The quota CLI inherits your environment, so `POLYTOKEN_QUOTA_HOME`,
  `POLYTOKEN_CONFIG_DIR`, and `POLYTOKEN_BINARY` work as documented by the
  CLI. The quota CLI itself resolves `polytoken` from `POLYTOKEN_BINARY` or
  `PATH`, so the plugin exports the path it resolved for both reads: a
  `polytoken_bin` config override wins, an inherited `POLYTOKEN_BINARY` is
  checked and passed through unchanged, and with neither set the plugin's own
  search result is exported so a minimal GUI PATH still works. Setting policy
  roots in a GUI context usually requires `launchctl setenv` or the config
  file above, because a plain GUI launch does not read your shell profile.

## Reading the display

The menu stays concise by design: the root shows one-line summaries, and
longer explanations live in submenus (pace details under the pace line,
per-provider and per-route rows, and an `About this status` section that
groups every disclaimer). Every informational line carries SwiftBar's
`length=85` cap — verified against SwiftBar's source, a set `length` shorter
than the text truncates the visible row and moves the full text into the
row's tooltip — so no line can grow to screen width. Crucial raw values are
not left to truncation alone: used/limit/usage numbers, reset times, and
warning reasons each get their own line. Informational rows without actions
may appear dimmed in SwiftBar; that is normal, and no click actions were
added to change it.

Timestamps are shown exactly as the CLI supplied them, RFC3339 with their own
zone: a trailing `Z` is labeled `(UTC)`, and an explicit offset such as
`-04:00` is shown as-is without a UTC label.

The menu-bar icon shows the best available pace, not fleet health:

- `+N.NN` with an up arrow (green): the projection says unused quota
  accumulates toward the next reset for the best candidate provider.
- `0` with a right arrow: exactly on pace. A displayed zero is a real zero
  from the engine, never a placeholder for missing data.
- `-N.NN` with a down arrow: usage is running ahead of pace.
- A triangle with amber color: warning precedence (below). Details stay in
  the menu.
- A red circle-slash: every quota-observed, non-disabled provider is fresh
  and explicitly unavailable. This is observed unavailability, not
  necessarily quota exhaustion.
- A question-mark circle: pace is unavailable (no candidate: gated,
  disabled, stale, never observed, ineligible, unavailable, or no computable
  signal). This is never shown as a healthy zero.

The pace number is a use-it-or-lose-it projection the quota engine computes
from saved quota evidence at the listed `as_of` time. It is not measured
traffic, a token balance, or a guarantee about serving. Tiny nonzero signals
are displayed as `~+0` or `~-0` so rounding cannot flip the direction of the
arrow. Ties between candidates break deterministically by signal, then rank,
then provider name.

Warning precedence: command failure, timeout, malformed or unexpected output,
an older CLI without the signal/freshness fields (shown as a compatibility
warning; provider details still render, pace does not), actionable doctor
findings, status errors or quota problems, or pending reconciler targets all
turn the icon amber ahead of any pace display. Pace details remain in the
menu beneath the warning line.

Each provider row shows its consolidated state (`available`, `gated`,
`disabled`, `unavailable`, or `enabled` before first observation) and reason,
availability distinct from routing eligibility, checked time with freshness
(`fresh`, `stale`, `missing`), rank, off-peak, eligibility, next reset, and
its pace signal with meaning. A signal is shown even when a quota gate holds
the provider off; the row says so. Each quota window shows a fixed-width bar
plus every supplied raw number: used, limit, and usage percent, with the
reset time exactly as supplied (a `Z` value is UTC). Bars prefer a reported
percent, otherwise derive from
nonnegative used and positive limit; a missing or invalid denominator renders
an unknown bar. Only the visual fill is clamped at 100%, so over-limit raw
values stay visible. When a reported percent disagrees with `used/limit` by
more than 5 percentage points, both are kept and the row is marked. Zero is
data: `0` values are shown, not hidden.

The Diagnostics section lists `doctor` findings (sanitized code, severity,
target/file context, message, and remediation when present) plus recovered
journal entries. A finding's kind (quota evidence, reconciliation/pending,
journal/publication, persisted state, policy/config) is a hint from its code,
not a reclassification. Remediation text is informational: the plugin has no
mutation buttons. Pending target IDs come from `status`; they mean
outstanding reconciler work, and reported routing may not yet be applied.
Routing shows the enabled state, provider-only mode (where routes are not
applicable), and per-route desired/effective chains with skipped-model
reasons and target/source provenance.

Status and diagnostics are two independent reads. The menu labels them as
such and never claims one atomic snapshot; `last_checked` is the newest
observation across providers, not proof that every provider is fresh.

## Safety

- All report-derived text is literalized before it reaches SwiftBar: pipes,
  newlines, and control characters cannot create actions, attributes,
  separators, or submenus, and emoji/SF-symbol parsing is disabled on every
  data line.
- The only action is the fixed `Refresh status | refresh=true` line. No
  command, URL, or eval can be injected through data or config values.
- stderr is never printed; failures render as fixed-vocabulary warnings with
  exit codes. No credentials, raw config, or secrets are displayed, and
  configuration problems never echo the rejected content.
- Each subprocess runs in a process group owned by the plugin: on a deadline,
  an output overflow, or plugin termination the whole group (descendants
  included) is signalled and reaped, a producer that leaves children behind
  after any exit is cleaned up too, and the collector shutdown is bounded.
  The cleanup's KILL escalation removes TERM-resistant descendants while the
  command's own exit status and the deadline verdict are preserved.
- Captured output is capped while the command runs, not afterwards; a
  producer that writes past the cap is stopped early and the truncation is
  reported in the menu.
- Setup is fail-closed: if the capture FIFO cannot be created or the owned
  process group cannot be established, the command is not run (an in-flight
  producer is killed) and the refresh reports the setup failure visibly.
  There is no unbounded or direct-PID fallback.

## Host and container notes

This plugin is documented for a stock macOS host. In a Linux container that
mounts this repository, the plugin's `$HOME`- and Homebrew-relative search
paths see container paths, not your Mac's; container runs (including the
fixture suite) exercise logic only, not GUI resolution or menu rendering.

## Tests

`contrib/swiftbar/tests/run_tests.sh` exercises the plugin against stub
commands only (no live accounts, config, daemons, or network) and runs in any
POSIX-ish environment with Bash and `jq`; repository CI runs it on Linux.

## Known limitations

- The icon, submenu layout, colors in light/dark mode, and the refresh action
  are verified against SwiftBar's documented behavior and source, but only
  rendered checks on a real Mac confirm the visual result.
- Each quota-CLI subprocess is bounded at 10 seconds (SIGKILL one second
  later) and each jq pass at 5 seconds; an outer 50-second budget skips
  whichever stages remain once it runs out. With every bound exhausted the
  worst case is about 46 seconds of subprocess time plus the outer cap, which
  still stays inside SwiftBar's 60-second refresh interval. SwiftBar itself
  has no plugin-wide timeout, so the budget is the plugin's own.
