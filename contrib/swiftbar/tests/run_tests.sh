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
write_stub_quota() {
  cat > "$CASE_DIR/bin/polytoken-quota" <<'STUB'
#!/bin/sh
d="$STUB_DIR"
c="$1"
if [ "$#" -ne 2 ] || [ "$2" != "--json" ]; then
  exit 64
fi
if [ -f "$d/$c.sleep" ]; then
  sleep "$(cat "$d/$c.sleep")"
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
  if [ -n "$conf" ]; then
    envs=(POLYTOKEN_SWIFTBAR_CONFIG="$conf")
  fi
  OUT=$(env -i PATH="/usr/bin:/bin:$JQ_DIR" HOME="$CASE_DIR" STUB_DIR="$CASE_DIR" \
    "${envs[@]+"${envs[@]}"}" "$PLUGIN_BASH" "$PLUGIN" 2>/dev/null)
  RC=$?
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

# --- cases -----------------------------------------------------------------------

# Pace: positive / zero / negative / tiny-preserving-direction.
start_case pace_positive
standard_setup
status_fixture "$(wrap_status "$(prov_base alpha available 0.42),$(prov_base beta available -2)")"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota +0.42 :arrow.up: | color=green'
assert_has 'Best available pace: +0.42 — unused quota accumulating toward reset (projection) (provider alpha'
assert_lacks 'WARNING:'

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
assert_has 'Best available pace: +0.5 — unused quota accumulating toward reset (projection) (provider lowrank'

start_case tie_by_name
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[{"provider":"zeta","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.5},{"provider":"alpha","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.5}],"routes":[],"pending_targets":[],"problem":false,"errors":[]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_has 'Best available pace: +0.5 — unused quota accumulating toward reset (projection) (provider alpha'

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
assert_has 'Best available pace: unavailable — no fresh, available, eligible provider with a computable signal'
assert_lacks 'WARNING:'
assert_has '----Pace signal: +9.9 — unused quota accumulating toward reset (projection) (freshness: fresh) — computed despite the quota gate; the gate holds routing off'
assert_has '— provider manually disabled; shown for information only'
assert_count 6 '--Provider: '

start_case no_signal_fresh_available
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh"}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :questionmark.circle:'
assert_has 'Best available pace: unavailable'
assert_has '----Pace signal: unavailable (not computable from saved evidence; a real zero is shown as 0)'
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
assert_has 'Observed unavailability: every quota-observed, non-disabled provider is fresh and explicitly unavailable — observed state, not necessarily quota exhaustion.'
assert_lacks 'WARNING:'

start_case not_red_gated_plus_unknown
standard_setup
G1='{"provider":"g","status":"gated","rank":1,"off_peak":false,"eligible":false,"reason":"quota gate","windows":[],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh"}'
UNK='{"provider":"k","status":"unavailable","rank":2,"off_peak":false,"eligible":false,"reason":"never observed","windows":[],"freshness":"missing"}'
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
status_fixture "$(wrap_status '{"provider":"off","status":"disabled","rank":1,"off_peak":false,"eligible":false,"reason":"manual disable","windows":[],"freshness":"missing"}')"
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
assert_has 'WARNING: older polytoken-quota CLI without signal/freshness fields — pace unavailable'
assert_has '--Provider: legacy'
assert_has '----Window: weekly'
assert_has 'Best available pace: unavailable'

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
assert_has '----Window: w [####################] 150% (over limit; raw value retained above)'
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
assert_has '----Window: w [ unknown ] unknown (invalid supplied numbers ignored)'
assert_has '------Limit: 0'

start_case bars_negative_used_invalid
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w","used":-1,"limit":10}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [ unknown ] unknown (invalid supplied numbers ignored)'
assert_has '------Used: -1'

start_case bars_missing_all_unknown
standard_setup
status_fixture "$(wrap_status '{"provider":"a","status":"available","rank":1,"off_peak":false,"eligible":true,"reason":"ok","windows":[{"name":"w"}],"checked_at":"2026-09-21T09:59:30Z","availability":"available","freshness":"fresh","signal":0.1}')"
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has '----Window: w [ unknown ] unknown'
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
assert_has 'Note: 2026-09-21T09:59:30Z is the newest observation across providers, not proof every provider is fresh.'
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
assert_has 'Note: pending means outstanding reconciler work; reported routing may not yet be applied.'

start_case status_errors_warning
standard_setup
status_fixture '{"as_of":"2026-09-21T10:00:00Z","routing_enabled":true,"provider_only":false,"last_checked":"2026-09-21T09:59:30Z","providers":[],"routes":[],"pending_targets":[],"problem":false,"errors":[{"scope":"provider","mapping_id":"codex","target_id":"global","source_path":"/cfg/policy.yml","summary":"provider projection failed"},{"scope":"route","target_id":"proj","summary":"route projection failed"}]}'
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status reported 2 diagnostic error(s)'
assert_has 'Errors: 2 reported (scope provider = provider/quota projection, route = route projection)'
assert_has '--Error: provider — target global'
assert_has '----Summary: provider projection failed'
assert_has '--Error: route — target proj'

start_case status_exit2_partial_rendered
standard_setup
status_fixture "$(wrap_status "$(prov_base a available 0.2)")"
stub_rc status 2
doctor_fixture "$(empty_doctor)"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: status exit 2 (quota problem report)'
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
assert_has 'WARNING: status reported quota problems'

# Doctor: parsed independently of exit status; classification; precedence.
start_case doctor_actionable_warning
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":true,"findings":[{"code":"quota-stale-snapshot","severity":"error","target_id":"codex","message":"snapshot is stale","remediation":"run check to refresh the snapshot"}],"recovered":[]}'
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: doctor reported 1 actionable finding(s)'
assert_has 'Diagnostics: 1 finding(s), 1 actionable, 0 recovered'
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
assert_has 'Diagnostics: 0 finding(s), 0 actionable, 0 recovered'
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

start_case doctor_error_field_warns_but_finding_shown
standard_setup
status_fixture "$(wrap_status "")"
doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","actionable":false,"findings":[{"code":"orphaned-provider-state","severity":"info","message":"orphan"}],"recovered":[],"error":"partial journal read"}'
stub_rc doctor 1
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: doctor reported an error'
assert_has '--Finding: orphaned-provider-state [severity: info] — kind: persisted state'

start_case doctor_timeout_rc143
standard_setup
status_fixture "$(wrap_status "")"
stub_rc doctor 143
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: doctor command timed out after 10s'
assert_has 'Diagnostics: unavailable — doctor timed out with unusable output'

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
assert_has 'Diagnostics: 0 finding(s), 0 actionable, 0 recovered'

# Dependencies: missing/bad jq, quota CLI, polytoken prerequisite.
start_case dep_missing_jq
standard_setup
printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=/nonexistent/jq\n' "$CASE_DIR" "$CASE_DIR" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'WARNING: configured jq_bin override is not executable: /nonexistent/jq'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'

start_case dep_missing_quota_bin
standard_setup
printf 'quota_bin=/nonexistent/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=jq\n' "$CASE_DIR" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: configured quota_bin override is not executable: /nonexistent/polytoken-quota'
assert_has 'Best available pace: unavailable — status was not run (missing dependency)'

start_case dep_missing_polytoken
standard_setup
rm -f "$CASE_DIR/bin/polytoken"
printf 'quota_bin=%s/bin/polytoken-quota\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'WARNING: polytoken prerequisite not found (the quota CLI requires it at startup; set polytoken_bin)'
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
if printf '%s\n' "$OUT" | grep 'href=\|bash=\|terminal=\|params0' | grep -qvE 'emojize=false symbolize=false$'; then
  bad "action syntax outside title: $(printf '%s\n' "$OUT" | grep 'href=\|bash=\|terminal=\|params0' | grep -vE 'emojize=false symbolize=false$' | head -n 1)"
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

# Config errors fail visibly with no fallback.
start_case config_unknown_key
standard_setup
printf 'bogus=1\n' > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Plugin configuration error — override ignored, no fallback'
assert_has 'Reason: unknown config key: bogus'
assert_lacks 'Providers:'

start_case config_duplicate_key
standard_setup
printf 'jq_bin=jq\njq_bin=jq\n' > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Reason: duplicate config key: jq_bin'

start_case config_missing_explicit_file
standard_setup
run_plugin "$ROOT/does-not-exist.conf"
assert_menu_ok
assert_has 'Plugin configuration error — override ignored, no fallback'
assert_has 'Reason: override configuration file not readable:'

start_case config_empty_value
standard_setup
printf 'quota_bin=\n' > "$CASE_DIR/plugin.conf"
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_has 'Reason: config value for quota_bin is empty'

# Fixed footer always present; menu never blank.
start_case footer_fixed
standard_setup
run_plugin "$CASE_DIR/plugin.conf"
assert_menu_ok
assert_count 1 'Refresh status | refresh=true'
assert_has 'Note: the menu-bar icon is best available pace, not fleet health, a token balance, or actual serving.'
assert_has 'Note: status and diagnostics are two independent reads and may observe different state revisions.'
assert_no_stderr_leak

# --- summary -----------------------------------------------------------------------

TOTAL=$((PASS + FAIL))
printf '\n%d assertions, %d passed, %d failed (%d cases)\n' "$TOTAL" "$PASS" "$FAIL" "$CASE_N"
if [ "$FAIL" -ne 0 ]; then
  printf 'failed cases:%s\n' "$FAILED_CASES"
  exit 1
fi
printf 'PASS: SwiftBar plugin fixture suite green\n'
