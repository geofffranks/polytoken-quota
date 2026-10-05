#!/usr/bin/env bash
# Fixture tests for the SwiftBar plugin ../polytoken-quota.1m.sh.
#
# Stub commands only: no live accounts, no live polytoken config, no daemons,
# no network. Every case drives the plugin through a fixture directory and a
# strict plugin config file (or controlled HOME/PATH for dependency search).
#
# Usage: contrib/swiftbar/tests/run_tests.sh
# Exit status: 0 when every assertion passes, 1 otherwise.

set -u

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PLUGIN="$HERE/../polytoken-quota.1m.sh"

PASS=0
FAIL=0
FAILED_CASES=""
CASE_N=0
CASE_NAME=""
CASE_DIR=""
OUT=""
RC=""

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/swiftbar-plugin-tests.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT

# --- harness -----------------------------------------------------------------

start_case() {
  CASE_N=$((CASE_N + 1))
  CASE_NAME=$1
  CASE_DIR="$ROOT/$CASE_N-$1"
  mkdir -p "$CASE_DIR/bin"
  OUT=""
  RC=""
}

# Fixture-driven stub: reads $STUB_DIR/<cmd>.json, <cmd>.rc, <cmd>.sleep.
# Optional per-command flags: <cmd>.trapterm (trap SIGTERM and exit 0),
# <cmd>.flood (emit output forever until SIGPIPE), <cmd>.child (spawn a
# long-lived sleep 42 child so process-group cleanup is observable).
write_stub_quota() {
  cat > "$CASE_DIR/bin/polytoken-quota" <<'STUB'
#!/bin/sh
d="$STUB_DIR"
c="$1"
if [ "$#" -ne 2 ] || [ "$2" != "--json" ]; then
  exit 64
fi
: > "$d/ran"
printf '%s' "${POLYTOKEN_BINARY-}" > "$d/polytoken_seen"
if [ -f "$d/$c.trapterm" ]; then
  trap 'exit 0' TERM
fi
if [ -f "$d/$c.termresist" ]; then
  # TERM-resistant member: SIG_IGN survives exec, so only the group's KILL
  # escalation can remove it.
  sh -c 'trap "" TERM; exec sleep 42' &
  echo $! > "$d/child.pid"
elif [ -f "$d/$c.child" ]; then
  sh -c 'exec sleep 42' &
  echo $! > "$d/child.pid"
fi
if [ -f "$d/$c.sleep" ]; then
  sleep "$(cat "$d/$c.sleep")"
fi
if [ -f "$d/$c.flood" ]; then
  while :; do
    printf '0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789\n'
  done
fi
if [ -f "$d/$c.json" ]; then
  cat "$d/$c.json"
fi
if [ -f "$d/$c.rc" ]; then
  exit "$(cat "$d/$c.rc")"
fi
exit 0
STUB
  chmod +x "$CASE_DIR/bin/polytoken-quota"
}

write_stub_polytoken() {
  printf '#!/bin/sh\nexit 0\n' > "$CASE_DIR/bin/polytoken"
  chmod +x "$CASE_DIR/bin/polytoken"
}

write_default_config() {
  {
    printf 'quota_bin=%s/bin/polytoken-quota\n' "$CASE_DIR"
    printf 'polytoken_bin=%s/bin/polytoken\n' "$CASE_DIR"
    printf 'jq_bin=%s\n' "$JQ_ABS"
  } > "$CASE_DIR/plugin.conf"
}

status_fixture() { printf '%s\n' "$1" > "$CASE_DIR/status.json"; }
doctor_fixture() { printf '%s\n' "$1" > "$CASE_DIR/doctor.json"; }
stub_rc()        { printf '%s\n' "$2" > "$CASE_DIR/$1.rc"; }
stub_sleep()     { printf '%s\n' "$2" > "$CASE_DIR/$1.sleep"; }

run_plugin() {
  local conf=${1:-}
  local envs=()
  local path="/usr/bin:/bin:$JQ_DIR"
  if [ -n "${PATH_PREPEND:-}" ]; then
    path="$PATH_PREPEND:$path"
  fi
  if [ -n "$conf" ]; then
    envs=(POLYTOKEN_SWIFTBAR_CONFIG="$conf")
  fi
  if [ -n "${PLUGIN_ENV_POLYTOKEN_BINARY:-}" ]; then
    envs=("${envs[@]+"${envs[@]}"}" POLYTOKEN_BINARY="$PLUGIN_ENV_POLYTOKEN_BINARY")
  fi
  OUT=$(env -i PATH="$path" HOME="$CASE_DIR" STUB_DIR="$CASE_DIR" \
    "${envs[@]+"${envs[@]}"}" "$PLUGIN_BASH" "$PLUGIN" 2>/dev/null)
  RC=$?
}

pid_gone() {
  # $1 = exact PID recorded by this fixture in its private staging. True when
  # the PID no longer exists or is a zombie (stat Z): an unreaped corpse is
  # not a live descendant. A live non-zombie match keeps polling up to ~4s
  # before being declared a survivor.
  local pid=$1 stat i=0
  case "$pid" in
    ''|*[!0-9]*) return 0 ;;
  esac
  while [ "$i" -lt 8 ]; do
    stat=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
    if [ -z "$stat" ] || [ "${stat#Z}" != "$stat" ]; then
      return 0
    fi
    sleep 0.5
    i=$((i + 1))
  done
  return 1
}

assert_member_gone() {
  # Inspects ONLY the PID the fixture stub recorded in its private staging —
  # never a global process-table search, which races concurrently running
  # suites and matches unrelated command lines. Strict: any live non-zombie
  # match of the recorded PID is a survivor and fails the case.
  local pid stat
  pid=$(cat "$CASE_DIR/child.pid" 2>/dev/null)
  if [ -z "$pid" ]; then
    bad "fixture recorded no child pid to inspect"
    return
  fi
  if pid_gone "$pid"; then
    ok
  else
    stat=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
    bad "recorded child pid $pid still alive (stat: ${stat:-unknown})"
  fi
}

# --- assertions ----------------------------------------------------------------

ok()  { PASS=$((PASS + 1)); }
bad() {
  FAIL=$((FAIL + 1))
  case ",$FAILED_CASES," in *",$CASE_NAME,"*) : ;; *) FAILED_CASES="$FAILED_CASES $CASE_NAME" ;; esac
  printf 'FAIL [%s]: %s\n' "$CASE_NAME" "$1"
  if [ -n "${SUITE_DEBUG:-}" ]; then
    printf '%s\n' "$OUT" | sed 's/^/    | /' | head -n 40
  fi
}

assert_menu_ok() {
  if [ "$RC" = 0 ] && [ -n "$OUT" ]; then ok; else bad "exit=$RC, output bytes=${#OUT}"; fi
}
assert_has() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then ok; else bad "missing text: $1"; fi
}
assert_lacks() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then bad "unexpected text: $1"; else ok; fi
}
assert_header() {
  local first
  first=$(printf '%s\n' "$OUT" | head -n 1)
  if [ "$first" = "$1" ]; then ok; else bad "header was: $first"; fi
}
assert_count() {
  local n
  n=$(printf '%s\n' "$OUT" | grep -cF -- "$2")
  if [ "$n" -eq "$1" ]; then ok; else bad "expected $1 occurrences of [$2], got $n"; fi
}
assert_occurrences() {
  # Counts total occurrences, not lines: joined warnings share one line.
  local n
  n=$(printf '%s\n' "$OUT" | grep -oF -- "$2" | wc -l)
  case "$n" in ''|*[!0-9]*) n=0 ;; esac
  if [ "$n" -eq "$1" ]; then ok; else bad "expected $1 occurrences of [$2], got $n"; fi
}
assert_count_re() {
  local n
  n=$(printf '%s\n' "$OUT" | grep -cE -- "$2")
  if [ "$n" -eq "$1" ]; then ok; else bad "expected $1 regex matches of [$2], got $n"; fi
}
assert_no_stderr_leak() {
  # stderr is discarded by run_plugin; a leaked fragment would appear in OUT.
  if printf '%s\n' "$OUT" | grep -qE '^(usage|/usr/bin|/bin/|sh:|bash:)'; then
    bad "raw stderr-looking line in menu"
  else
    ok
  fi
}

assert_layout_compact() {
  # Two structural guarantees on the rendered menu:
  # 1. flag/length safety — every informational line ends with the literal
  #    parser flags and length cap; submenu parents also carry a fixed color.
  # 2. concise root — root-level (non-child) titles stay within 60 chars;
  #    child titles within 85 (the SwiftBar length cap then only ever clips
  #    redundant text, with the full value in the tooltip).
  local viol treeviol tover rootover line title
  viol=$(printf '%s\n' "$OUT" | awk '
    /^Quota / { next }
    /^---$/ { next }
    /^Refresh status \| refresh=true$/ { next }
    $0 ~ /color=black,white/ {
      if ($0 !~ /\| emojize=false symbolize=false length=85 color=black,white$/ || $0 ~ / (refresh|href|bash)=/) c++
      next
    }
    $0 !~ /\| emojize=false symbolize=false length=85$/ { c++ }
    END { print c + 0 }')
  if [ "$viol" = 0 ]; then ok; else bad "$viol informational line(s) missing literal flags/length or unsafe parent metadata"; fi
  treeviol=$(printf '%s\n' "$OUT" | awk '
    /^Quota / || /^Refresh status / { next }
    {
      lines[NR] = $0
      depth[NR] = 0
      while (substr($0, depth[NR] + 1, 1) == "-") depth[NR]++
      count = NR
    }
    END {
      for (i = 1; i <= count; i++) {
        if (lines[i] ~ /^---$/ || lines[i] ~ /color=black,white$/) continue
        for (j = i + 1; j <= count; j++) {
          if (lines[j] ~ /^---$/) break
          if (depth[j] <= depth[i]) break
          if (depth[j] > depth[i]) { bad++; break }
        }
      }
      print bad + 0
    }')
  if [ "$treeviol" = 0 ]; then ok; else bad "$treeviol generated menu parent(s) with deeper descendants missing fixed appearance colors"; fi
  if [ "$(printf '%s\n' "$OUT" | grep -c 'refresh=true')" = 1 ] && ! printf '%s\n' "$OUT" | grep 'color=black,white' | grep -qE ' (refresh|href|bash)='; then ok; else bad "submenu display metadata added an action or refresh is not unique"; fi
  for parent in 'Best available pace:' 'Observed unavailability:' 'Row validity:' 'Providers:' '--Provider:' '----Window:' 'Routing:' '--Route:' 'Pending:' '--Error:' '--Finding:' 'Findings:' 'Recovered:' '--Recovered:' 'Diagnostics:' 'About this status'; do
    if printf '%s\n' "$OUT" | grep -F -- "$parent" | grep -vq 'color=black,white'; then
      bad "submenu parent [$parent] missing fixed appearance colors"
    else
      ok
    fi
  done
  rootover=$(printf '%s\n' "$OUT" | awk '
    /^---$/ { next }
    /^Quota / { next }
    /^Refresh status / { next }
    /^About this status$/ { next }
    {
      line = $0
      sub(/\| emojize.*$/, "", line)
      sub(/[ \t]+$/, "", line)
      if (line !~ /^-/) c += (length(line) > 60)
    }
    END { print c + 0 }')
  if [ "$rootover" = 0 ]; then ok; else bad "$rootover root line(s) exceed 60 chars"; fi
  tover=$(printf '%s\n' "$OUT" | awk '
    /^---$/ { next }
    /^Quota / { next }
    /^Refresh status / { next }
    /^About this status$/ { next }
    {
      line = $0
      sub(/\| emojize.*$/, "", line)
      sub(/[ \t]+$/, "", line)
      if (line ~ /^-/) c += (length(line) > 85)
    }
    END { print c + 0 }')
  if [ "$tover" = 0 ]; then ok; else bad "$tover child line(s) exceed 85 chars"; fi
}

# --- preflight ------------------------------------------------------------------

if [ ! -f "$PLUGIN" ]; then
  echo "plugin not found: $PLUGIN" >&2
  exit 1
fi
if [ ! -x "$PLUGIN" ]; then
  echo "plugin is not executable: $PLUGIN" >&2
  exit 1
fi
if ! bash -n "$PLUGIN"; then
  echo "plugin failed bash -n" >&2
  exit 1
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required to run the plugin under test" >&2; exit 1; }
JQ_ABS=$(command -v jq)
JQ_DIR=$(dirname "$JQ_ABS")
# PLUGIN_BASH runs the plugin itself (default: bash on PATH). Point it at
# another interpreter to verify compatibility, e.g. stock bash 3.2 on macOS:
#   PLUGIN_BASH=/bin/bash contrib/swiftbar/tests/run_tests.sh
PLUGIN_BASH=${PLUGIN_BASH:-bash}
command -v "$PLUGIN_BASH" >/dev/null 2>&1 || { echo "interpreter not found: $PLUGIN_BASH" >&2; exit 1; }

# --- shared fixture builders ------------------------------------------------------

# provider PROVIDER STATUS SIGNAL [EXTRA jq-object-fragment]
prov_base() {
  # $1 provider, $2 status, $3 signal json literal (number or null)
  printf '{"provider":"%s","status":"%s","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"next_reset_at":"2026-10-01T00:00:00Z","checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":%s}' "$1" "$2" "$3"
}

wrap_status() {
  printf '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[%s],"routes":[],"pending_targets":[],"problem":false,"errors":[]}' "$1"
}

empty_doctor() {
  printf '{"as_of":"2026-09-21T10:00:00Z","actionable":false,"findings":[],"recovered":[]}'
}

# Standard setup: working stubs + default config, no fixtures (healthy empty).
standard_setup() {
  write_stub_quota
  write_stub_polytoken
  write_default_config
}

command -v mkfifo >/dev/null 2>&1 || { echo "mkfifo is required for lifecycle fixtures" >&2; exit 1; }
MKFIFO_ABS=$(command -v mkfifo)

# --- cases -----------------------------------------------------------------------

# Pace: positive / zero / negative / tiny-preserving-direction.
start_case pace_positive
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available 0.42),$(prov_base beta available -2)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota +0.42 :arrow.up: | color=green'
assert_has 'Best available pace: +0.42 — provider alpha'
assert_has '--Pace meaning: unused quota accumulating toward reset (projection)'
assert_lacks 'WARNING:'
assert_layout_compact

start_case pace_zero
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available 0)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota 0 :arrow.right:'
assert_has 'on pace (projection)'
assert_lacks 'WARNING:'

start_case pace_negative
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available -1.25)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota -1.25 :arrow.down:'
assert_has 'usage ahead of pace (projection)'
assert_lacks 'WARNING:'

start_case pace_tiny_positive
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available 0.004)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota ~+0 :arrow.up: | color=green'

start_case pace_tiny_negative
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available -0.004)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota ~-0 :arrow.down:'

start_case pace_overdrawn_best_no_warning
# An overdrawn provider stays the chosen pace; negativity alone is not a warning.
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available -3.5)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota -3.5 :arrow.down:'
assert_lacks 'WARNING:'

# Deterministic ties: signal, then rank, then name.
start_case tie_by_rank
standard_setup
status_fixture "$(wrap_status "$(prov_base lowrank available 0.5),$(prov_base midrank available 0.5)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_header 'Quota +0.5 :arrow.up: | color=green'
assert_has 'Best available pace: +0.5 — provider lowrank'

start_case tie_by_name
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"zeta","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.5},{"provider":"alpha","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.5}],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_has 'Best available pace: +0.5 — provider alpha'

# Exclusions: none of these may supply pace; pure exclusion is not a warning.
start_case exclusions_all
standard_setup
GATED='{"provider":"g","status":"gated","rank":1,"off_peak":false,"eligible":false,"reason":"quota gate","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":9.9}'
DISABLED='{"provider":"d","status":"disabled","rank":2,"off_peak":false,"eligible":false,"reason":"manual disable","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":9.9}'
STALE='{"provider":"s","status":"available","rank":3,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-20T09:59:30Z","availability":"available","freshness":"stale","signal":9.9}'
MISSINGF='{"provider":"m","status":"enabled","rank":4,"off_peak":false,"eligible":true,"reason":"never observed","windows":[],"freshness":"missing","signal":null}'
INELIG='{"provider":"i","status":"available","rank":5,"off_peak":true,"eligible":false,"reason":"ineligible: reserved","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":9.9}'
UNAVAIL='{"provider":"u","status":"unavailable","rank":6,"off_peak":false,"eligible":false,"reason":"unreachable","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"unavailable","freshness":"fresh","signal":null}'
status_fixture "$(wrap_status "$GATED,$DISABLED,$STALE,$MISSINGF,$INELIG,$UNAVAIL")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_has 'Best available pace: unavailable'
assert_has '--Pace unavailable detail: no fresh, available, eligible provider'
assert_has '--Pace unavailable counts: gated 1; disabled 1'
assert_lacks 'WARNING:'
assert_has '----Pace signal: +9.9 (freshness: fresh)'
assert_has '----Pace signal meaning: unused quota accumulating toward reset (projection)'
assert_has '----Pace signal note: computed despite the quota gate; the gate holds routing off'
assert_has '----Pace signal note: provider manually disabled; shown for information only'
assert_count 6 '--Provider: '

start_case no_signal_fresh_available
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh"}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_has 'Best available pace: unavailable'
assert_has '--Pace unavailable detail: no fresh, available, eligible provider'
assert_has '----Pace signal: unavailable'
assert_has '----Pace signal note: not computable from saved evidence; a real zero is shown as 0'
assert_lacks 'WARNING:'

# Red only when every non-disabled quota-observed provider is fresh + unavailable.
start_case red_all_unavailable
standard_setup
U1='{"provider":"u1","status":"unavailable","rank":1,"off_peak":false,"eligible":false,"reason":"unreachable","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"unavailable","freshness":"fresh"}'
U2='{"provider":"u2","status":"unavailable","rank":2,"off_peak":false,"eligible":false,"reason":"unreachable","windows":[],"checked_at":"2026-09-21T09:58:00Z","availability":"unavailable","freshness":"fresh"}'
status_fixture "$(wrap_status "$U1,$U2")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :xmark.circle: | color=red'
assert_has 'Observed unavailability: observed state, not necessarily exhaustion'
assert_has '--Unavailability detail: every quota-observed, non-disabled provider is fresh'
assert_lacks 'WARNING:'

start_case not_red_gated_plus_unknown
standard_setup
G1='{"provider":"g","status":"gated","rank":1,"off_peak":false,"eligible":false,"reason":"quota gate","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh"}'
UNK='{"provider":"k","status":"unavailable","rank":2,"off_peak":false,"eligible":false,"reason":"never observed","windows":[],"freshness":"missing","signal":null}'
status_fixture "$(wrap_status "$G1,$UNK")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_lacks 'Observed unavailability:'
assert_lacks 'WARNING:'

start_case neutral_no_providers
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":false,"provider_only":false,"providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_has 'Providers: 0'
assert_has 'No providers are configured or projected.'
assert_lacks 'WARNING:'

start_case only_disabled_no_warning
# A single disabled unmetered provider neither warns nor turns red.
standard_setup
status_fixture "$(wrap_status '{"provider":"off","status":"disabled","rank":1,"off_peak":false,"eligible":false,"reason":"manual disable","windows":[],"freshness":"missing","signal":null}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_lacks 'WARNING:'
assert_lacks 'Observed unavailability:'

# Older CLI without additive fields: details survive, pace unavailable, warning.
start_case older_cli_compat
standard_setup
status_fixture '{"routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"legacy","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"weekly","used":1,"limit":2,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available"}],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: older polytoken-quota CLI: no signal/freshness fields — pace unavailable'
assert_has '--Provider: legacy'
assert_has '----Window: weekly'
assert_has 'Best available pace: unavailable'
assert_has 'Note: connected CLI lacks signal/freshness fields — pace unavailable'

# Bars and raw numbers.
start_case bars_percent_only
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","usage_percent":37,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [#######.............] 37%'
assert_has '------Used: unknown'
assert_has '------Limit: unknown'
assert_has '------Usage: 37% (reported)'
assert_has '------Resets: 2026-09-25T00:00:00Z (UTC)'

start_case bars_ratio_only
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":25,"limit":200,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [###.................] 12.5%'
assert_has '------Usage: 12.5% (derived from used/limit)'

start_case bars_disagreement_marked
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":90,"limit":100,"usage_percent":50,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '------Usage: 50% (reported) (reported percent disagrees with used/limit)'
assert_has '------Used: 90'
assert_has '------Limit: 100'

start_case bars_over_limit_raw_retained
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":150,"limit":100,"usage_percent":150,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":-0.5}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [####################] 150% (over limit; raw value retained)'
assert_has '------Used: 150'
assert_has '------Limit: 100'
assert_has '------Usage: 150% (reported)'

start_case bars_zero_is_data
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":0,"limit":10,"usage_percent":0,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [....................] 0%'
assert_has '------Used: 0'
assert_has '------Limit: 10'
assert_has '------Usage: 0% (reported)'

start_case bars_zero_limit_unknown
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":5,"limit":0}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [unknown             ] unknown (invalid supplied numbers ignored)'
assert_has '------Limit: 0'

start_case bars_negative_used_invalid
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":-1,"limit":10}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [unknown             ] unknown (invalid supplied numbers ignored)'
assert_has '------Used: -1'

start_case bars_missing_all_unknown
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [unknown             ] unknown'
assert_has '------Used: unknown'
assert_has '------Limit: unknown'
assert_has '------Usage: unknown'
assert_has '------Resets: unknown'

start_case bars_nonnumeric_marked
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":"lots","limit":10}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '------Used: lots (non-numeric)'

start_case timestamps_and_newest_note
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.2)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Next reset: 2026-10-01T00:00:00Z (UTC)'
assert_has '----Checked: 2026-09-21T09:59:30Z — freshness fresh'
assert_has '--Newest observation: 2026-09-21T09:59:30Z'
assert_has '--Newest observation: newest across providers, not proof every provider is fresh.'
assert_has 'Signals as of: 2026-09-21T10:00:00Z (UTC)'

# Routing.
start_case routing_enabled_with_provenance
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[],"routes":[{"name":"main","target_id":"global","source_path":"/cfg/policy.yml","desired":["codex/a","anthropic/b"],"effective":["codex/a"],"skipped":[{"model":"anthropic/b","reason":"quota exhausted"}],"projection_error":false}],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Routing: enabled'
assert_has '--Route: main'
assert_has '----Target: global'
assert_has '----Source: /cfg/policy.yml'
assert_has '----Desired: codex/a, anthropic/b'
assert_has '----Effective: codex/a'
assert_has '----Skipped: anthropic/b — quota exhausted'
assert_has '----Projection error: no'

start_case routing_provider_only
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":true,"last_checked":"2026-09-21T09:59:30Z","providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Routing: provider-only mode — routes are not applicable in this mode.'

# Warnings: pending, status errors, exit codes, malformed output.
start_case pending_warning
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.2)")"
doctor_fixture "$(empty_doctor)"
printf '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[],"routes":[],"pending_targets":["t-abc"],"problem":false,"errors":[]}' > "$CASE_DIR/status.json"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: 1 pending reconciler target(s)'
assert_has 'Pending target: t-abc'
assert_has '--Pending detail: outstanding reconciler work.'
assert_has '--Pending detail: reported routing may not yet be applied.'
assert_has '--Pending target: t-abc | emojize=false symbolize=false length=85'

start_case status_errors_warning
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[{"scope":"provider","mapping_id":"codex","target_id":"global","source_path":"/cfg/policy.yml","summary":"provider projection failed"},{"scope":"route","target_id":"proj","summary":"route projection failed"}]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status reported 2 diagnostic error(s)'
assert_has 'Errors: 2 reported'
assert_has '--Error scopes: provider = provider/quota projection; route = route projection'
assert_has '--Error: provider — target global'
assert_has '----Summary: provider projection failed'
assert_has '----Mapping:'
assert_has '--Error: route — target proj'

start_case status_exit2_partial_rendered
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.2)")"
stub_rc status 2
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_count 1 'WARNING: quota issues reported'
assert_lacks 'WARNING: status exit 2 (quota problem report)'
assert_has '--Provider: a'
assert_has 'Best available pace: +0.2'

start_case status_exit1_partial_rendered
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.2}],"routes":[{"name":"main","desired":["codex/a"],"effective":[],"projection_error":true}],"pending_targets":[],"problem":false,"errors":[]}'
stub_rc status 1
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status exit 1 (partial projection report)'
assert_has '--Provider: a'
assert_has '----Projection error: yes'

start_case status_unexpected_exit
standard_setup
stub_rc status 3
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status command failed (unexpected exit 3)'
assert_has 'Best available pace: unavailable — status unexpected exit 3 with unusable output'

start_case status_empty_output
standard_setup
: > "$CASE_DIR/status.json"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status output is empty'

start_case status_malformed
standard_setup
printf 'not json at all {{{' > "$CASE_DIR/status.json"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status output is malformed or unparseable'

start_case status_bad_shape
standard_setup
printf '{"providers": 5}' > "$CASE_DIR/status.json"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status output has an unexpected shape'

start_case status_problem_flag
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":false,"provider_only":false,"providers":[],"routes":[],"pending_targets":[],"problem":true,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: quota issues reported'
assert_count 1 'WARNING: quota issues reported'

# Doctor: parsed independently of exit status; classification; precedence.
start_case doctor_actionable_warning
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":true,"findings":[{"code":"quota-stale-snapshot","severity":"error","target_id":"codex","message":"snapshot is stale","remediation":"run check to refresh the snapshot"}],"recovered":[]}'
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: diagnostics need attention: 1 of 1'
assert_has 'Diagnostics: 1 need attention · 1 total · 0 recovered'
assert_has 'Findings: 1 total; 1 need attention'
assert_has '--Finding: quota-stale-snapshot [severity: error] — kind: quota evidence'
assert_has '----Remediation (informational; the plugin performs no actions): run check to refresh the snapshot'
assert_lacks 'WARNING: status'

start_case doctor_nonzero_findings_still_parsed
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":true,"findings":[{"code":"target-pending","severity":"warning","target_id":"proj-1","message":"target has pending changes"},{"code":"journal-incomplete","severity":"info","message":"journal tail incomplete"}],"recovered":[]}'
stub_rc doctor 1
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has '--Finding: target-pending [severity: warning] — kind: reconciliation/pending'
assert_has '--Finding: journal-incomplete [severity: info] — kind: journal/publication'

start_case doctor_ok_no_findings
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Diagnostics: 0 need attention · 0 total · 0 recovered'
assert_has 'Findings: none reported'
assert_lacks 'WARNING:'

start_case doctor_recovered_shown
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":false,"findings":[],"recovered":[{"target_id":"proj-1","stage":"reconcile","summary":"recovered from incomplete journal"}]}'
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Recovered: 1'
assert_has '--Recovered: proj-1 — stage reconcile'
assert_has '----Summary: recovered from incomplete journal'
assert_has '--Recovered: proj-1 — stage reconcile | emojize=false symbolize=false length=85 color=black,white'

start_case doctor_error_field_warns_but_finding_shown
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":false,"findings":[{"code":"orphaned-provider-state","severity":"info","message":"orphan"}],"recovered":[],"error":"partial journal read"}'
stub_rc doctor 1
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: doctor reported an error'
assert_has '--Finding: orphaned-provider-state [severity: info] — kind: persisted state'

start_case doctor_natural_rc143_not_timeout
# A doctor that naturally exits 143 with usable output must not be reported
# as a timeout (deadline detection is sentinel-based, not exit-status based).
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture "$(empty_doctor)"
stub_rc doctor 143
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: doctor command failed (unexpected exit 143)'
assert_lacks 'timed out'
assert_has 'Diagnostics: 0 need attention · 0 total · 0 recovered'

start_case doctor_natural_rc137_not_timeout
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture "$(empty_doctor)"
stub_rc doctor 137
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: doctor command failed (unexpected exit 137)'
assert_lacks 'timed out'

# One read succeeding cannot conceal the other's failure.
start_case one_source_failure_concealed_not
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.2)")"
doctor_fixture "$(empty_doctor)"
stub_rc doctor 127
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: doctor command failed (unexpected exit 127)'
assert_has 'Best available pace: +0.2'
assert_has '--Provider: a'
# Findings are parsed independently of the problem exit: usable doctor output
# still renders its tables even though the failure warns.
assert_has 'Diagnostics: 0 need attention · 0 total · 0 recovered'

# Screenshot-shaped regression: status exit 2 and problem flag produce one
# quota warning; doctor count clearly separates actionable findings from total.
start_case screenshot_warning_and_doctor_counts
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":false,"provider_only":false,"providers":[],"routes":[],"pending_targets":[],"problem":true,"errors":[]}'
stub_rc status 2
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":true,"findings":[{"code":"quota-stale-snapshot","severity":"error","message":"stale evidence"},{"code":"target-pending","severity":"warning","message":"pending work"},{"code":"journal-incomplete","severity":"info","message":"journal information"},{"code":"orphaned-provider-state","severity":"info","message":"persisted information"},{"code":"policy-schema","severity":"info","message":"policy information"},{"code":"misc-a","severity":"info","message":"info a"},{"code":"misc-b","severity":"info","message":"info b"},{"code":"misc-c","severity":"info","message":"info c"}],"recovered":[]} '
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_count 1 'WARNING: quota issues reported'
assert_count 1 'WARNING: diagnostics need attention: 2 of 8'
assert_has 'Diagnostics: 2 need attention · 8 total · 0 recovered'
assert_has 'Findings: 8 total; 2 need attention'
assert_has '--Finding: journal-incomplete [severity: info] — kind: journal/publication'
assert_has '----Message: journal information'

# Dependencies: missing/bad jq, quota CLI, polytoken prerequisite.
start_case dep_missing_jq
standard_setup
printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=/nonexistent/jq\n' "$CASE_DIR" "$CASE_DIR" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: configured jq_bin override is not executable'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'

start_case dep_missing_quota_bin
standard_setup
printf 'quota_bin=/nonexistent/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: configured quota_bin override is not executable'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'

start_case dep_missing_polytoken
standard_setup
rm -f "$CASE_DIR/bin/polytoken"
printf 'quota_bin=%s/bin/polytoken-quota\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: polytoken prerequisite not found — set polytoken_bin'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'

start_case deps_via_home_search
# No config: resolution must fall back to common user paths ($HOME/bin).
write_stub_quota
write_stub_polytoken
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
run_plugin ""
assert_menu_ok
assert_header 'Quota +0.3 :arrow.up: | color=green'
assert_lacks 'WARNING:'

# Paths with spaces and timeouts.
start_case path_with_spaces
standard_setup
SPACED="$ROOT/$CASE_N dir with spaces"
mkdir -p "$SPACED"
cp "$CASE_DIR/bin/polytoken-quota" "$SPACED/polytoken-quota"
cp "$CASE_DIR/bin/polytoken" "$SPACED/polytoken"
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
printf 'quota_bin=%s/polytoken-quota\npolytoken_bin=%s/polytoken\njq_bin=%s\n' "$SPACED" "$SPACED" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota +0.3 :arrow.up: | color=green'

start_case status_timeout_real
standard_setup
stub_sleep status 12
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status command timed out after 10s'
assert_has 'Best available pace: unavailable — status timed out with unusable output'

# Output-safety: adversarial data cannot create actions/attributes/submenus.
start_case injection_actions_and_structure
standard_setup
EVIL_PROVIDER='ev | refresh=true href=https://evil.example'
EVIL_WINDOW='w | bash=/bin/echo pwned params0=x'
EVIL_MODEL='codex/new\nline\u0007ctrl :arrow.up: :mushroom:'
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"'"$EVIL_PROVIDER"'","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"'"$EVIL_WINDOW"'","used":1,"limit":2,"usage_percent":50,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}],"routes":[{"name":"r | href=https://evil2.example","desired":["'"$EVIL_MODEL"'"],"effective":[],"projection_error":false}],"pending_targets":["t | terminal=false href=x"],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
# The fixed Refresh action is the only refresh attribute anywhere; the
# injected "refresh=true" text may only appear inside titles (not at a line
# end, which is the attribute position).
assert_count_re 1 'refresh=true$'
# Any data-borne action syntax must sit before the literal flags, i.e. inside
# the title, never in the attribute position.
if printf '%s\n' "$OUT" | grep 'href=\|bash=\|terminal=\|params0' | grep -qvE 'length=85( color=black,white)?$'; then
  bad "action syntax outside title: $(printf '%s\n' "$OUT" | grep 'href=\|bash=\|terminal=\|params0' | grep -vE 'length=85( color=black,white)?$' | head -n 1)"
else
  ok
fi
# Newlines/control chars in data must not create extra menu lines.
assert_count 1 '--Provider: '
assert_count 1 '----Window: '
assert_has '------Window name: w'
assert_count 1 'Pending: 1 outstanding target(s)'
# Colon sequences must stay literal on flagged lines (no emoji substitution).
assert_has 'codex/new line ctrl :arrow.up: :mushroom:'

# Config errors fail visibly with fixed-vocabulary diagnostics: line numbers
# and known keys only — never rejected lines, unsupported key names, override
# values, or file paths (synthetic secret markers prove it).
start_case config_unknown_key
standard_setup
printf 'bogus=SECRET_VALUE_X\n' > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Plugin configuration error — override ignored, no fallback'
assert_has 'Reason: unsupported config key at line 1'
assert_lacks 'SECRET_VALUE_X'
assert_lacks 'bogus'

start_case config_malformed_line_secret
standard_setup
printf 'quota_bin=%s/bin/polytoken-quota\nSECRET_TOKEN_LINE\n' "$CASE_DIR" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Reason: config line 2 is not in key=value form'
assert_lacks 'SECRET_TOKEN_LINE'

start_case config_bad_override_value_secret
standard_setup
printf 'quota_bin=/nonexistent/SECRET_PATH_X\npolytoken_bin=%s/bin/polytoken\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: configured quota_bin override is not executable'
assert_lacks 'SECRET_PATH_X'

start_case config_missing_explicit_file_secret
standard_setup
run_plugin "$ROOT/SECRET_DIR_X/does-not-exist.conf"
assert_menu_ok
assert_has 'Plugin configuration error — override ignored, no fallback'
assert_has 'Reason: override configuration file is not readable'
assert_lacks 'SECRET_DIR_X'

start_case config_duplicate_key
standard_setup
printf 'jq_bin=%s\njq_bin=%s\n' "$JQ_ABS" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Reason: duplicate config key jq_bin at line 2'

start_case config_empty_value
standard_setup
printf 'quota_bin=\n' > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Reason: config value for quota_bin at line 1 is empty'

# C1: the resolved Polytoken path is exported as POLYTOKEN_BINARY for both
# reads, with config override > inherited env > search precedence.
start_case c1_export_config_override
standard_setup
SPACED="$ROOT/$CASE_N spaced polytoken"
mkdir -p "$SPACED"
printf '#!/bin/sh\nexit 0\n' > "$SPACED/polytoken"
chmod +x "$SPACED/polytoken"
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/polytoken\njq_bin=%s\n' "$CASE_DIR" "$SPACED" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota +0.3 :arrow.up: | color=green'
assert_has "Best available pace: +0.3"
if [ -f "$CASE_DIR/polytoken_seen" ] && [ "$(cat "$CASE_DIR/polytoken_seen")" = "$SPACED/polytoken" ]; then
  ok
else
  bad "POLYTOKEN_BINARY not exported from config override (saw: $(cat "$CASE_DIR/polytoken_seen" 2>/dev/null))"
fi

start_case c1_env_conflict_config_wins
standard_setup
SPACED="$ROOT/$CASE_N spaced polytoken cfg"
mkdir -p "$SPACED"
printf '#!/bin/sh\nexit 0\n' > "$SPACED/polytoken"
chmod +x "$SPACED/polytoken"
ENVFAKE="$ROOT/$CASE_N env-fake"
mkdir -p "$ENVFAKE"
printf '#!/bin/sh\nexit 0\n' > "$ENVFAKE/polytoken"
chmod +x "$ENVFAKE/polytoken"
PLUGIN_ENV_POLYTOKEN_BINARY="$ENVFAKE/polytoken"
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/polytoken\njq_bin=%s\n' "$CASE_DIR" "$SPACED" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
if [ -f "$CASE_DIR/polytoken_seen" ] && [ "$(cat "$CASE_DIR/polytoken_seen")" = "$SPACED/polytoken" ]; then
  ok
else
  bad "config override must win over inherited POLYTOKEN_BINARY"
fi
PLUGIN_ENV_POLYTOKEN_BINARY=""

start_case c1_env_passthrough_unchanged
standard_setup
SPACED="$ROOT/$CASE_N spaced polytoken env"
mkdir -p "$SPACED"
printf '#!/bin/sh\nexit 0\n' > "$SPACED/polytoken"
chmod +x "$SPACED/polytoken"
PLUGIN_ENV_POLYTOKEN_BINARY="$SPACED/polytoken"
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
printf 'quota_bin=%s/bin/polytoken-quota\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
if [ -f "$CASE_DIR/polytoken_seen" ] && [ "$(cat "$CASE_DIR/polytoken_seen")" = "$SPACED/polytoken" ]; then
  ok
else
  bad "validated inherited POLYTOKEN_BINARY must be passed through unchanged"
fi
PLUGIN_ENV_POLYTOKEN_BINARY=""

start_case c1_env_bad_override_fails
standard_setup
printf 'quota_bin=%s/bin/polytoken-quota\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
PLUGIN_ENV_POLYTOKEN_BINARY="/nonexistent/SECRET_ENV_PATH"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: POLYTOKEN_BINARY override is not executable'
assert_lacks 'SECRET_ENV_PATH'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'
PLUGIN_ENV_POLYTOKEN_BINARY=""

# C2: a command that traps SIGTERM and exits 0 at the deadline is still a
# timeout; deadline detection is independent of the exit status.
start_case c2_term_trap_exit0_is_timeout
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
stub_sleep status 12
printf '1\n' > "$CASE_DIR/status.trapterm"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status command timed out after 10s'
assert_has 'Best available pace: unavailable — status timed out with unusable output'

# ADV1: output is capped during capture; overflow is reported visibly.
start_case adv1_overflow_large_truncated
standard_setup
yes '0123456789012345678901234567890123456789012345678901234567890123456789' | head -c 400000 > "$CASE_DIR/status.json"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status output exceeded the capture limit and was truncated'

start_case adv1_overflow_continuous_producer
standard_setup
printf '1\n' > "$CASE_DIR/status.flood"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status output exceeded the capture limit and was truncated'
assert_has 'Best available pace: unavailable'

# ADV2: the owned process group is killed completely — timeout and success.
start_case adv2_descendant_killed_on_timeout
standard_setup
stub_sleep status 12
printf '1\n' > "$CASE_DIR/status.child"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status command timed out after 10s'
assert_member_gone

start_case adv2_descendant_killed_on_success
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
printf '1\n' > "$CASE_DIR/status.child"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota +0.3 :arrow.up: | color=green'
assert_member_gone

# C3: row validity — contradictions and unsupported values warn and are
# excluded from pace and unavailability decisions, while valid partial
# details still render.
start_case c3_contradictory_row_excluded
standard_setup
status_fixture "$(wrap_status '{"provider":"x","status":"unavailable","rank":1,"off_peak":false,"eligible":true,"reason":"odd","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.3}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_lacks 'Best available pace: +0.3'
assert_has 'Best available pace: unavailable'
assert_has 'WARNING: status contained 1 invalid or contradictory provider row(s)'
assert_has '----Validity: invalid row'
assert_has '--Provider: x'

start_case c3_freshness_banana_blocks_red
standard_setup
status_fixture "$(wrap_status '{"provider":"x","status":"unavailable","rank":1,"off_peak":false,"eligible":false,"reason":"odd","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"unavailable","freshness":"banana"}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_lacks 'Observed unavailability:'
assert_has 'WARNING: status contained 1 invalid or contradictory provider row(s)'

start_case c3_unsupported_status_warns
standard_setup
status_fixture "$(wrap_status '{"provider":"x","status":"banana","rank":1,"off_peak":false,"eligible":true,"reason":"odd","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":9}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status contained 1 invalid or contradictory provider row(s)'
assert_lacks 'Best available pace: +9'

start_case c3_nonboolean_eligibility_warns
standard_setup
status_fixture "$(wrap_status '{"provider":"x","status":"available","rank":1,"off_peak":false,"eligible":"yes","reason":"odd","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":9}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status contained 1 invalid or contradictory provider row(s)'
assert_header 'Quota :exclamationmark.triangle: | color=orange'

start_case c3_empty_older_cli_notice
# An older CLI with an empty provider list and no as_of still gets the
# compatibility notice; a newer no-clock error envelope has an error field
# instead and must not be misread as an old CLI.
standard_setup
status_fixture '{"routing_enabled":false,"provider_only":false,"providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: older polytoken-quota CLI: no signal/freshness fields — pace unavailable'
assert_has 'Note: connected CLI lacks signal/freshness fields — pace unavailable'

start_case c3_no_clock_error_not_compat
standard_setup
status_fixture '{"routing_enabled":false,"provider_only":false,"providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[],"error":"clock unavailable"}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status reported an error'
assert_lacks 'older polytoken-quota CLI'

# C4: raw reported usage keeps unrounded values; rounding exists only where
# labeled (the ~-prefixed pace signal).
start_case c4_tiny_reported_percent_raw
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","usage_percent":0.0001,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '------Usage: 0.0001% (reported)'
assert_has '----Window: w [....................] 0.0001%'

start_case c4_over_limit_fraction_raw
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":1000,"limit":1000,"usage_percent":100.0001,"reset_at":"2026-09-25T00:00:00Z"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":-0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '------Usage: 100.0001% (reported)'
assert_has '----Window: w [####################] 100.0001% (over limit; raw value retained)'

# Minor: an absent next reset is shown explicitly as unknown.
start_case minor_next_reset_unknown_explicit
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Next reset: unknown'

# Followup 1: a mkfifo capture-setup failure fails closed — the producer is
# never started and both reads report the setup failure visibly.
start_case fail1_mkfifo_fail_closed
standard_setup
FAILBIN="$CASE_DIR/failbin"
mkdir -p "$FAILBIN"
printf '#!/bin/sh\nexit 1\n' > "$FAILBIN/mkfifo"
chmod +x "$FAILBIN/mkfifo"
PATH_PREPEND="$FAILBIN"
run_plugin "$CASE_DIR/plugin.conf"
PATH_PREPEND=""
assert_menu_ok
if [ -f "$CASE_DIR/ran" ]; then
  bad "quota CLI ran despite capture setup failure"
else
  ok
fi
assert_has 'WARNING: status capture setup failed — command not run'
assert_has 'doctor capture setup failed — command not run'
assert_lacks '--Provider: '
assert_has 'Refresh status | refresh=true'

# The same fail-closed rule applies to the jq passes: a setup failure after
# the reads completed warns visibly and omits only the affected details.
start_case fail2_mkfifo_jq_fail_closed
standard_setup
FAILBIN="$CASE_DIR/failbin"
mkdir -p "$FAILBIN"
{
  printf '#!/bin/sh\n'
  printf 'cf=%s/mkfifo.count\n' "$CASE_DIR"
  printf 'c=$(cat "$cf" 2>/dev/null || echo 0)\n'
  printf 'c=$((c + 1))\n'
  printf 'echo "$c" > "$cf"\n'
  printf '[ "$c" -ge 3 ] && exit 1\n'
  printf 'exec %s "$@"\n' "$MKFIFO_ABS"
} > "$FAILBIN/mkfifo"
chmod +x "$FAILBIN/mkfifo"
PATH_PREPEND="$FAILBIN"
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
PATH_PREPEND=""
assert_menu_ok
if [ -f "$CASE_DIR/ran" ]; then
  ok
else
  bad "reads should have completed before the jq setup failure"
fi
assert_has 'jq capture setup failed — status details omitted'
assert_has 'jq capture setup failed — diagnostics details omitted'
assert_has 'Best available pace: unavailable — status exit 0 with unusable output'
assert_occurrences 2 'capture setup failed'

# Followup 2: the producer group is cleaned on EVERY exit. A partial report
# (exit 1 or 2) whose child holds the FIFO write end must not stall the
# runner, and the child must not survive.
start_case lc_partial_exit1_with_child
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
stub_rc status 1
printf '1\n' > "$CASE_DIR/status.child"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status exit 1 (partial projection report)'
assert_has 'Best available pace: +0.3'
assert_has '--Provider: a'
assert_member_gone

start_case lc_partial_exit2_with_child
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":false,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":-0.2}],"routes":[],"pending_targets":[],"problem":true,"errors":[]}'
stub_rc status 2
printf '1\n' > "$CASE_DIR/status.child"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_lacks 'WARNING: status exit 2 (quota problem report)'
assert_count 1 'WARNING: quota issues reported'
assert_has '--Provider: a'
assert_member_gone

start_case lc_termresist_member_killed
# A SIGTERM-ignoring member survives the watchdog's TERM; the cleanup's KILL
# escalation removes it while the partial exit status is preserved.
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.3)")"
stub_rc status 1
printf '1\n' > "$CASE_DIR/status.termresist"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status exit 1 (partial projection report)'
assert_has 'Best available pace: +0.3'
assert_member_gone

# Screenshot-driven layout regression: the root menu stays concise (long
# explanations live in submenus/About), every informational line carries the
# literal flags and the length=85 cap, and timestamps keep their supplied
# RFC3339 zone (Z labeled UTC, explicit offsets never mislabeled UTC).
start_case root_layout_concise
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available 0.42)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_layout_compact
assert_count 1 'Best available pace:'
assert_has '--Pace tie-break: deterministic, by signal then rank then name'
if printf '%s\n' "$OUT" | awk '
    /^---$/ { next }
    /^Quota / { next }
    /^Refresh status / { next }
    /^About this status$/ { next }
    { line = $0; sub(/\| emojize.*$/, "", line); sub(/[ \t]+$/, "", line)
      if (line !~ /^-/ && line ~ /tie-break|not fleet health|independent reads/) bad = 1 }
    END { exit !bad }'; then
  bad "long explanation found at root level instead of a submenu"
else
  ok
fi

start_case tz_doctor_offset_as_of
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-10-04T19:26:47.033928-04:00","actionable":true,"findings":[{"code":"quota-stale-snapshot","severity":"error","target_id":"codex","message":"snapshot is stale"}],"recovered":[]}'
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Doctor as of: 2026-10-04T19:26:47.033928-04:00'
assert_lacks '033928-04:00 (UTC)'

start_case tz_status_timestamps_offset
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":1,"limit":2,"usage_percent":50,"reset_at":"2026-09-25T12:00:00+02:00"}],"next_reset_at":"2026-09-25T12:00:00+02:00","checked_at":"2026-09-21T09:59:30-04:00","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Next reset: 2026-09-25T12:00:00+02:00'
assert_has '------Resets: 2026-09-25T12:00:00+02:00'
assert_has '----Checked: 2026-09-21T09:59:30-04:00 — freshness fresh'
assert_lacks '+02:00 (UTC)'
assert_lacks '-04:00 (UTC)'

start_case about_survives_unusable_status
standard_setup
printf 'not json {{{' > "$CASE_DIR/status.json"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: status output is malformed or unparseable'
assert_has 'About this status'
assert_has '--Status and diagnostics are independent reads and may differ.'
assert_has '--Pace projects saved quota; details remain below warnings when available.'

# Fixed footer always present; menu never blank.
start_case footer_fixed
standard_setup
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_count 1 'Refresh status | refresh=true'
assert_has 'About this status'
assert_has '--Pace projects saved quota; details remain below warnings when available.'
assert_has '--Status and diagnostics are independent reads and may differ.'
assert_has '--Newest observation is not proof every provider is fresh.'
assert_lacks 'gated is a quota-gate claim'
assert_no_stderr_leak

# --- summary -----------------------------------------------------------------------

TOTAL=$((PASS + FAIL))
printf '\n%d assertions, %d passed, %d failed (%d cases)\n' "$TOTAL" "$PASS" "$FAIL" "$CASE_N"
if [ "$FAIL" -ne 0 ]; then
  printf 'failed cases:%s\n' "$FAILED_CASES"
  exit 1
fi
printf 'PASS: SwiftBar plugin fixture suite green\n'
