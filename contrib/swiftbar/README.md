# SwiftBar plugin: polytoken-quota status

A read-only macOS menu-bar plugin for SwiftBar that shows provider quota
state, per-window utilization bars, reset times, provider pace, and a single
observation-age/attention summary from `polytoken-quota`. The plugin runs two
commands and nothing else: `polytoken-quota status --json` and read-only
`polytoken-quota doctor --json`. It never checks quotas against providers,
reconciles, changes routing, operates a daemon, keeps a status cache, or
stores credentials.

## Requirements

- macOS 12 (Monterey) or newer, and SwiftBar 2.1.0 or newer. Explicit
  light/dark text colors use SwiftBar 2.1.0's native menu-row styling.
- [SwiftBar](https://github.com/swiftbar/SwiftBar).
- `jq` 1.6 or newer, for example via `brew install jq`.
- The `polytoken-quota` CLI built from the same revision as this plugin on
  the Mac. The flat layout works with older JSON reports, but latest-attempt
  problems and explicit polling support require the additive diagnostic fields
  in this revision. Missing fields cannot establish those conditions; detailed
  attempt and pending timing information is available through the CLI.
  If required pace fields are absent, the menu indicates attention, pace ordering
  is disabled, and the icon remains a gauge without a pace number. Build the CLI on your Mac:

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
  any other key, a duplicate key, or an empty value causes the menu summary
  and amber gauge to indicate attention.
- An explicit override that does not resolve to an executable causes the menu
  to indicate attention. The plugin never silently falls back past a bad
  override; use the terminal CLI for detailed troubleshooting.
- Configuration failures show only attention status. Rejected lines,
  unsupported key names, override values, and file paths are never shown
  in the menu.
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

The root menu contains one quota-observation summary row, then provider headers
and quota-window rows when status data is available, followed by the sole action,
`Refresh status`. The menu-bar title is only a monochrome gauge when no attention
condition was observed, or the same gauge in amber when attention is needed; it
never shows quota text or a pace number. Diagnostic details, errors, pending-work
causes, and remediation are intentionally not shown in the menu. Use
`polytoken-quota status` and `polytoken-quota doctor` in a terminal when details
are needed. The summary distinguishes reported errors from other attention
conditions; data, pending, malformed, and unavailable observations do not
implicitly become error-level counts.

Informational rows explicitly specify black text in light appearance and white
text in dark appearance, including bars; SwiftBar 2.1.0 applies this foreground
color to otherwise actionless menu items. SwiftBar internally uses a no-op handler
for colored rows to avoid disabled-grey rendering; the plugin adds no dummy
action. This behavior is documented in the [2.1.0 release notes](https://github.com/swiftbar/SwiftBar/releases/tag/v2.1.0)
and implemented in its [menu-item renderer](https://github.com/swiftbar/SwiftBar/blob/v2.1.0/SwiftBar/MenuBar/MenuBarItem.swift).
Disabled providers are status information, not clickable disabled controls.
Native visual confirmation in both appearances remains pending.

### Timing and problems

`Quota observation` reports the age available from the status snapshot and
`No errors` or `Attention needed`; a positive count is shown only for reported
status/doctor errors. Data problems and pending work can require attention but
are not counted as errors unless the reports classify them as such. The age is
not a claim that the whole check succeeded or a display-refresh timestamp. If
status is missing, malformed, or unusable, age is `time unknown` and the summary
requires attention. Detailed causes, findings, file paths, and remediation stay
in the CLI output and are deliberately omitted from this menu.

Status and doctor are independent reads, not an atomic snapshot or a record of
one complete check. Reset times show a concise countdown in days/hours when
parseable, without changing the stored data.

### Providers and quota bars

Providers retain the shared three-tier ordering: usable fresh/available/eligible
providers first by unrounded pace, other enabled providers by rank/name, then
gated, unavailable, and manually disabled providers. Headers show provider name,
human-readable state, meaningful pace, and material reason or condition. Ineligibility
continues to affect whether a provider can lead.

Each quota window is a compact 10-cell bar, exact used percentage, and concise
reset countdown when parseable. Common window identifiers use human-readable
labels (for example, `subscription_kwh` becomes `Subscription`). Bars prefer
the reported percentage; otherwise they derive it from nonnegative used and
positive limit. Missing or invalid numbers render `No data` with an unknown bar.
Only visual fill is clamped, so over-limit percentages and real zero remain
visible. Percentage conflicts and invalid supplied numbers retain a compact raw
data row.

### Menu-bar icon

The menu-bar title is an icon only: a monochrome `gauge.medium` SF Symbol for
ordinary status, and that same gauge in amber when an error, data issue, pending
work, command problem, malformed report, or other attention condition is
observed. It does not show pace, availability text, or a warning triangle. The
`dropdown=false` setting keeps the icon out of the menu rows. The menu summary
reports status observation age and error presence separately from attention.

`Refresh status` is the sole action. It rereads saved status and doctor data;
it does not collect quotas, reconcile, or control a daemon. After replacing the
installed plugin script with an updated copy, select Refresh (or wait up to one
minute for SwiftBar's filename-based refresh interval); no restart is required.

## Safety

- All report-derived text is literalized before it reaches SwiftBar: pipes,
  newlines, and control characters cannot create actions, attributes,
  separators, or submenus, and emoji/SF-symbol parsing is disabled on every
  data line.
- The only action is the fixed `Refresh status | refresh=true` line. No
  command, URL, or eval can be injected through data or config values.
- stderr is never printed; failures indicate attention in the summary and
  gauge color. No credentials, raw config, or secrets are displayed, and
  configuration problems never echo the rejected content.
- Each subprocess runs in a process group owned by the plugin: on a deadline,
  an output overflow, or plugin termination the whole group (descendants
  included) is signalled and reaped, a producer that leaves children behind
  after any exit is cleaned up too, and the collector shutdown is bounded.
  The cleanup's KILL escalation removes TERM-resistant descendants while the
  command's own exit status and the deadline verdict are preserved.
- Captured output is capped while the command runs, not afterwards; a
  producer that writes past the cap is stopped early. Status menu output has the
  same cap; if it is too large to render, provider details are omitted and the
  summary indicates attention. A large report may therefore require the CLI for
  full details.
- Setup is fail-closed: if the capture FIFO cannot be created or the owned
  process group cannot be established, the command is not run (an in-flight
  producer is killed) and the menu indicates attention. Use CLI output for
  details. There is no unbounded or direct-PID fallback.

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
