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
- The `polytoken-quota` CLI built from the same revision as this plugin on
  the Mac. The flat layout works with older JSON reports, but complete latest
  attempt and polling labels require the additive diagnostic fields in this
  revision. Without them, polling status and pending attempt time are unknown.
  If required pace fields are absent, a visible compatibility warning disables
  pace ordering and the icon uses no pace candidate. Build the CLI on your Mac:

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

Opening the menu shows timing, data problems, pending work, doctor errors,
and every configured provider and quota window directly. There are no provider
or diagnostic submenus and no Option-only details. Long causes and remediation
are split into continued root rows rather than hidden in tooltips. Routing
chains, ranking internals, ordinary doctor info/warning findings, and recovered
history stay available through the CLI rather than in this dashboard.

Informational rows use the system appearance; only quota bars use a fixed-width
font. Disabled providers are status information, not clickable disabled controls.
Actual inactive-row contrast still needs inspection in SwiftBar on a Mac in
both light and dark appearances. The plugin adds no dummy action to force styling.

### Timing and problems

`Latest quota observation` is the newest saved quota snapshot across providers,
not a successful whole-check result. `Evaluation time` is when the CLI evaluates
saved evidence, not a provider request or the display refresh time. Latest known
per-provider failed/partial attempts remain visible even if an older saved
snapshot is still fresh. Missing evidence is not a healthy zero. Polling labels
use explicit configuration/support evidence; absent evidence says `status unknown`,
not that a check failed.

Pending targets always show `duration unknown`. Their latest reconciliation
attempt time/age, when supplied, is not the duration of continuous pending work.
Retries can update that attempt time without implying the pending work is new.
Doctor-only pending reports lack structured timing and say time unknown; the
plugin does not extract timestamps from prose. Reported fields may not yet be
reconciled while work is pending.

The doctor count includes only error-level findings. Each error shows target
context, cause, and remediation when supplied. Collection/data problems and
interrupted reconciliation are shown independently even if doctor classifies
them as info or warning. Ordinary omitted warnings alone do not cause an amber
icon. Doctor exits 0 and 1 with valid diagnostic JSON are accepted reports.

Status and doctor are independent reads, not an atomic snapshot or a record of
one complete check. Supplied RFC3339 timestamps retain their zone: trailing `Z`
is labeled `(UTC)`, while explicit offsets are shown as-is. Relative times are
computed against the status evaluation time when both timestamps are parseable.

### Providers and quota bars

The list is labeled `Ordered by available pace`, not routing priority:

1. Fresh, explicitly available, eligible providers with a finite computable
   signal come first, ordered by unrounded signal descending, then routing rank
   and provider name ascending.
2. Other enabled providers follow by rank and name, including stale/missing
   evidence and unknown signals. Unknown signal is never zero.
3. Gated, unavailable, and manually disabled providers follow by status and name.
   A positive signal cannot move them above usable providers.

A header shows consolidated status and pace. Non-usable signals are qualified;
reasons and evidence keep manual disable, quota exhaustion, policy gates,
availability, and eligibility distinct. If an older CLI lacks required fields,
all providers instead use deterministic rank/name order with an explicit
compatibility warning and no pace candidate.

Each supplied window has a fixed-width text bar, used percentage, and reset
row with time/countdown when available. Bars prefer the reported percentage;
otherwise they derive it from nonnegative used and positive limit. Missing or
invalid numbers render `No data` with an unknown bar. Only visual fill is
clamped, so over-limit percentages stay visible and real zero remains zero.
If reported percent differs from used/limit by more than 5 percentage points,
a compact raw-data row retains both and marks the disagreement. Invalid
supplied numbers also retain a raw-data row.

### Menu-bar icon

The icon uses the first usable provider's pace, not fleet health or actual
routing priority. Pace is the quota engine's saved-evidence use-it-or-lose-it
projection, not measured traffic or a serving guarantee:

- Positive pace: green up arrow.
- Real zero: right arrow; never a placeholder for missing data.
- Negative pace: down arrow.
- Tiny nonzero pace: `~+0` or `~-0`, retaining its direction.
- Amber triangle: command/JSON/compatibility failures, provider data problems,
  error-level doctor findings, pending work, or status quota issues. Every
  warning reason is visible above providers.
- Red unavailable icon: all observed non-disabled providers are fresh and
  explicitly unavailable, unless an amber problem takes precedence.
- Question mark: no usable pace candidate.

`Refresh status` is the sole action. It rereads saved status and doctor data;
it does not collect quotas, reconcile, or control a daemon.

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
  reported in the menu. Expanded status/doctor menu bodies have the same cap;
  an overflowing body is omitted with a visible warning, never displayed as
  complete. A large report may therefore require the CLI for full details.
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

- Fixture tests verify output and safety, not native SwiftBar rendering. On a
  reachable Mac, inspect both light and dark appearances for readable inactive
  rows, reasonable width, bar alignment, and no submenu navigation. Until then,
  contrast is unverified. If SwiftBar dims plain status rows beyond readability,
  report that limitation rather than adding unsafe actions.
- Many providers/windows make a long flat menu. Use the system menu scrolling;
  essential data is not hidden in submenus to shorten it.
- Each quota-CLI subprocess is bounded at 10 seconds (SIGKILL one second
  later) and each jq pass at 5 seconds; an outer 50-second budget skips
  whichever stages remain once it runs out. With every bound exhausted the
  worst case is about 46 seconds of subprocess time plus the outer cap, which
  still stays inside SwiftBar's 60-second refresh interval. SwiftBar itself
  has no plugin-wide timeout, so the budget is the plugin's own.
