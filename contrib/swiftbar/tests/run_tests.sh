#!/usr/bin/env bash
# Isolated SwiftBar fixtures: no live accounts, config, daemon, or network.
# PLUGIN_BASH=/bin/bash selects the plugin interpreter on a Mac.
set -u
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PLUGIN="$HERE/../polytoken-quota.1m.sh"
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/swiftbar-plugin-tests.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT
PASS=0; FAIL=0; CASE_N=0; CASE_NAME=""; CASE_DIR=""; OUT=""; RC=""
JQ_ABS=$(command -v jq) || exit 1
MKFIFO_ABS=$(command -v mkfifo) || exit 1
PLUGIN_BASH=${PLUGIN_BASH:-bash}
bash -n "$PLUGIN" || exit 1
command -v "$PLUGIN_BASH" >/dev/null || exit 1
PATH_PREPEND=""; PLUGIN_ENV_POLYTOKEN_BINARY=""
ok() { PASS=$((PASS+1)); }
bad() { FAIL=$((FAIL+1)); printf 'FAIL [%s]: %s\n' "$CASE_NAME" "$1"; }
assert_has() { if printf '%s\n' "$OUT" | grep -qF -- "$1"; then ok; else bad "missing: $1"; fi; }
assert_lacks() { if printf '%s\n' "$OUT" | grep -qF -- "$1"; then bad "unexpected: $1"; else ok; fi; }
assert_header() { local first; first=$(printf '%s\n' "$OUT" | head -n 1); if [ "$first" = "$1" ]; then ok; else bad "header: $first (wanted $1)"; fi; }
assert_order() {
  local actual
  actual=$(printf '%s\n' "$OUT" | sed -n 's/^Provider: \([^ ]*\) —.*/\1/p' | tr '\n' ' ')
  if [ "$actual" = "$1 " ]; then ok; else bad "provider order: $actual (wanted $1)"; fi
}
assert_before() {
  local a b
  a=$(printf '%s\n' "$OUT" | grep -nF -- "$1" | head -n 1 | cut -d: -f1)
  b=$(printf '%s\n' "$OUT" | grep -nF -- "$2" | head -n 1 | cut -d: -f1)
  if [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]; then ok; else bad "expected $1 before $2"; fi
}
assert_layout() {
  local violations
  violations=$(printf '%s\n' "$OUT" | awk '
    /^Quota / || /^---$/ || /^Refresh status \| refresh=true$/ { next }
    /^-/ { c++; next }
    $0 !~ / \| emojize=false symbolize=false( font=Menlo)?$/ { c++ }
    END { print c+0 }')
  [ "$violations" = 0 ] && ok || bad "nonliteral or nested rows: $violations"
  [ "$(printf '%s\n' "$OUT" | grep -c '^Refresh status | refresh=true$')" = 1 ] && ok || bad "refresh not unique"
  if printf '%s\n' "$OUT" | grep -v '^Refresh status | refresh=true$' | grep -qE '\|.*(refresh=|href=|bash=|terminal=|shell=|params[0-9]=|alternate=)'; then bad 'data-driven action'; else ok; fi
  assert_lacks 'length='
  assert_lacks '===PROVIDERS==='
}
assert_member_gone() {
  local pid stat i=0
  pid=$(cat "$CASE_DIR/child.pid" 2>/dev/null)
  case "$pid" in ''|*[!0-9]*) bad 'child PID not recorded'; return ;; esac
  while [ "$i" -lt 8 ]; do
    stat=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
    if [ -z "$stat" ] || [ "${stat#Z}" != "$stat" ]; then ok; return; fi
    sleep 0.5; i=$((i+1))
  done
  bad "owned descendant $pid remains alive"
}
start_case() {
  CASE_N=$((CASE_N+1)); CASE_NAME=$1; CASE_DIR="$ROOT/$CASE_N-$1"
  mkdir -p "$CASE_DIR/bin"; OUT=""; RC=""
  cat > "$CASE_DIR/bin/polytoken-quota" <<'STUB'
#!/bin/sh
c=$1; d=$STUB_DIR
[ "$#" -eq 2 ] && [ "$2" = --json ] || exit 64
case "$c" in status|doctor) ;; *) exit 64 ;; esac
printf '%s\n' "$c" >> "$d/ran"
printf '%s' "${POLYTOKEN_BINARY-}" > "$d/polytoken_seen"
[ ! -f "$d/$c.trapterm" ] || trap 'exit 0' TERM
if [ -f "$d/$c.termresist" ]; then
  sh -c 'trap "" TERM; exec sleep 42' & echo $! > "$d/child.pid"
elif [ -f "$d/$c.child" ]; then
  sh -c 'exec sleep 42' & echo $! > "$d/child.pid"
fi
[ ! -f "$d/$c.sleep" ] || sleep "$(cat "$d/$c.sleep")"
if [ -f "$d/$c.flood" ]; then
  while :; do printf '012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789\n'; done
fi
[ ! -f "$d/$c.json" ] || cat "$d/$c.json"
[ ! -f "$d/$c.rc" ] || exit "$(cat "$d/$c.rc")"
exit 0
STUB
  printf '#!/bin/sh\nexit 0\n' > "$CASE_DIR/bin/polytoken"
  chmod +x "$CASE_DIR/bin/polytoken-quota" "$CASE_DIR/bin/polytoken"
  printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=%s\n' "$CASE_DIR" "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
  doctor_fixture '{"as_of":"2026-09-21T10:00:00Z","findings":[],"recovered":[]}'
  status_fixture "$(wrap_status "$(prov_base alpha available 0.3)")"
}
status_fixture() { printf '%s\n' "$1" > "$CASE_DIR/status.json"; }
doctor_fixture() { printf '%s\n' "$1" > "$CASE_DIR/doctor.json"; }
stub_rc() { printf '%s\n' "$2" > "$CASE_DIR/$1.rc"; }
prov_base() {
  printf '{"provider":"%s","status":"%s","rank":1,"eligible":true,"reason":"ok","availability":"available","freshness":"fresh","signal":%s,"checked_at":"2026-09-21T09:59:30Z","polling_status":"enabled","latest_attempt":{"status":"fresh","checked_at":"2026-09-21T09:59:30Z"},"windows":[{"name":"weekly","usage_percent":50,"reset_at":"2026-09-22T10:00:00Z"}]}' "$1" "$2" "$3"
}
wrap_status() { printf '{"as_of":"2026-09-21T10:00:00Z","last_checked":"2026-09-21T09:59:30Z","providers":[%s],"routes":[],"pending_targets":[],"problem":false,"errors":[]}' "$1"; }
run_plugin() {
  local conf=${1:-$CASE_DIR/plugin.conf} envs=() path="/usr/bin:/bin:$(dirname "$JQ_ABS")"
  [ -z "$PATH_PREPEND" ] || path="$PATH_PREPEND:$path"
  [ "$conf" = none ] || envs=(POLYTOKEN_SWIFTBAR_CONFIG="$conf")
  [ -z "$PLUGIN_ENV_POLYTOKEN_BINARY" ] || envs=("${envs[@]+"${envs[@]}"}" POLYTOKEN_BINARY="$PLUGIN_ENV_POLYTOKEN_BINARY")
  OUT=$(env -i PATH="$path" HOME="$CASE_DIR" STUB_DIR="$CASE_DIR" "${envs[@]+"${envs[@]}"}" "$PLUGIN_BASH" "$PLUGIN" 2>/dev/null); RC=$?
  [ "$RC" = 0 ] && [ -n "$OUT" ] && ok || bad "plugin exit=$RC or empty output"
  assert_layout
}

# Available pace keeps the same direction/rounding semantics as the CLI signal.
for spec in '0.42|Quota +0.42 :arrow.up: | color=green' '0|Quota 0 :arrow.right:' '-1.25|Quota -1.25 :arrow.down:' '0.004|Quota ~+0 :arrow.up: | color=green' '-0.004|Quota ~-0 :arrow.down:' '-3.5|Quota -3.5 :arrow.down:'; do
  signal=${spec%%|*}; header=${spec#*|}
  start_case "pace_$signal"; status_fixture "$(wrap_status "$(prov_base alpha available "$signal")")"
  run_plugin; assert_header "$header"; assert_lacks WARNING:
done

start_case three_tiers_unrounded
providers=$(jq -nc --argjson p "$(prov_base template available 1)" '
 [ $p+{provider:"near-low",signal:1.2341,rank:1},
   $p+{provider:"near-high",signal:1.2342,rank:9},
   $p+{provider:"tie-rank",signal:1,rank:1},
   $p+{provider:"tie-a",signal:1,rank:2}, $p+{provider:"tie-b",signal:1,rank:2},
   $p+{provider:"zero",signal:0}, $p+{provider:"negative",signal:-1},
   $p+{provider:"stale",freshness:"stale",signal:999,rank:3},
   ($p+{provider:"unknown",rank:2}|del(.signal)),
   $p+{provider:"missing",status:"enabled",freshness:"missing",availability:"unknown",rank:1,windows:[]},
   $p+{provider:"disabled-b",status:"disabled",signal:999,rank:1},
   $p+{provider:"disabled-a",status:"disabled",signal:999,rank:99},
   $p+{provider:"gated",status:"gated",signal:999},
   $p+{provider:"unavailable",status:"unavailable",availability:"unavailable",eligible:false,signal:999} ]')
status_fixture "$(jq -nc --argjson ps "$providers" --argjson r "$(wrap_status '')" '$r+{providers:$ps}')"
run_plugin
assert_order 'near-high near-low tie-rank tie-a tie-b zero negative missing unknown stale disabled-a disabled-b gated unavailable'
assert_has 'Provider: near-high — available · Pace +1.23'
assert_has 'Provider: unknown — available · Pace unknown'
assert_has 'Provider: stale — available · Pace +999 (not usable)'
assert_before 'Data issue: missing' 'Provider: near-high'
assert_before 'Data issue: stale' 'Provider: near-high'
assert_header 'Quota :exclamationmark.triangle: | color=orange'

start_case tie_candidate_agrees
status_fixture "$(wrap_status "$(prov_base beta available 0.5),$(prov_base alpha available 0.5)")"
run_plugin; assert_order 'alpha beta'; assert_header 'Quota +0.5 :arrow.up: | color=green'

start_case no_signal_not_zero
status_fixture "$(wrap_status "$(prov_base alpha available null)")"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'Pace unknown'; assert_lacks WARNING:

start_case red_fresh_unavailable
status_fixture "$(wrap_status "$(prov_base alpha unavailable -1)")" # correct the independent availability axis
status_fixture "$(jq '.providers[0].availability="unavailable" | .providers[0].eligible=false' "$CASE_DIR/status.json")"
run_plugin; assert_header 'Quota :xmark.circle: | color=red'; assert_has 'Observed unavailability:'

start_case disabled_readable
status_fixture "$(wrap_status "$(prov_base alpha disabled 9)")"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'Reason: manually disabled'; assert_has 'Window: weekly'; assert_lacks WARNING:

start_case empty_providers
status_fixture "$(wrap_status '')"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'No providers are configured or projected.'

start_case legacy_nonpace_order
status_fixture '{"providers":[{"provider":"zeta","status":"available","rank":1,"eligible":true,"signal":99,"windows":[]},{"provider":"alpha","status":"available","rank":2,"eligible":true,"signal":100,"windows":[]}],"pending_targets":[]}'
run_plugin; assert_order 'zeta alpha'; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Compatibility: required pace fields missing'; assert_has 'Polling: status unknown'; assert_has 'Pace unknown'

start_case latest_failed_older_fresh_snapshot
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0] += {observation_at:"2026-09-21T09:58:00Z",latest_attempt:{status:"failed",checked_at:"2026-09-21T09:59:00Z",error:"saved snapshot retained; adapter failed"}}')"
run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Latest quota observation:'; assert_has 'Evaluation time:'
assert_has 'Observation: 2026-09-21T09:58:00Z (UTC) · 2m ago'
assert_has 'Latest attempt: failed · 2026-09-21T09:59:00Z (UTC) · 1m ago'
assert_has 'latest quota attempt failed'; assert_has 'saved snapshot retained'
assert_before 'Attempt problem:' 'Provider: alpha'; assert_has 'Provider: alpha — available · Pace +0.3'

start_case partial_attempt
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].latest_attempt.status="partial"')"
run_plugin; assert_has 'latest quota attempt partial'; assert_header 'Quota :exclamationmark.triangle: | color=orange'

start_case polling_support_evidence
status_fixture "$(jq -nc --argjson p "$(prov_base template enabled null)" --argjson r "$(wrap_status '')" '$r+{providers:[($p+{provider:"disabled",freshness:"missing",polling_status:"disabled",windows:[]}|del(.latest_attempt,.availability)),($p+{provider:"unsupported",freshness:"missing",polling_status:"unsupported",windows:[]}|del(.latest_attempt,.availability)),($p+{provider:"unknown",freshness:"missing",windows:[]}|del(.latest_attempt,.polling_status,.availability))]}')"
run_plugin; assert_has 'Polling: disabled'; assert_has 'Polling: unsupported'; assert_has 'Polling: status unknown'
assert_lacks 'Data issue: disabled'; assert_lacks 'Data issue: unsupported'; assert_has 'Data issue: unknown'
assert_lacks 'latest quota attempt failed'

start_case pending_latest_attempt
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r+{pending_targets:["global","project"],pending_details:[{target_id:"global",last_attempt_at:"2026-09-21T09:48:00Z"}]}')"
run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Pending: global — duration unknown; last attempt 2026-09-21T09:48:00Z'
assert_has '12m ago'; assert_has 'Pending: project — duration unknown; last attempt time unknown'
assert_before 'Pending: global' 'Provider: alpha'; assert_has 'may not yet be reconciled'
status_fixture "$(jq '.pending_details[0].last_attempt_at="2026-09-21T09:59:00Z"' "$CASE_DIR/status.json")"
run_plugin; assert_has 'duration unknown'; assert_has '1m ago'; assert_lacks 'duration 1m'

start_case doctor_warning_only_omitted
stub_rc doctor 1
doctor_fixture '{"findings":[{"code":"legacy-config-keys","severity":"warning","message":"routine policy advisory"}],"recovered":[{"target_id":"global","summary":"recovered history"}]}'
run_plugin; assert_header 'Quota +0.3 :arrow.up: | color=green'; assert_has 'Doctor errors: 0'
assert_lacks 'routine policy advisory'; assert_lacks 'recovered history'; assert_lacks WARNING:

start_case doctor_errors_and_independent_problems
stub_rc doctor 1
doctor_fixture '{"findings":[{"code":"permission","target_id":"global","file":"/private/stage","chain":"main","severity":"error","message":"permission denied","remediation":"repair permissions in the standalone root"},{"code":"quota-partial","target_id":"alpha","severity":"info","message":"some quota windows missing"},{"code":"target-pending","target_id":"project","severity":"warning","message":"latest attempt at 2026-09-01T00:00:00Z"},{"code":"journal-incomplete","severity":"warning","message":"publication interrupted"},{"code":"ordinary","severity":"warning","message":"omit this"}]}'
run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Doctor errors: 1'
assert_has 'Error: global — permission'; assert_has 'Cause: permission denied'; assert_has 'Remediation: repair permissions'
assert_has 'File: /private/stage'; assert_has 'Chain: main'; assert_has 'Data issue: alpha — quota-partial'
assert_has 'Pending timing: duration unknown; last attempt time unknown'
assert_has 'publication interrupted'; assert_lacks 'omit this'; assert_before 'Cause: permission denied' 'Provider: alpha'

start_case full_essential_details_literal
doctor_fixture "$(jq -nc '{findings:[{code:"permission",severity:"error",target_id:"global",message:(("x"*130)+"TAIL_CAUSE | bash=/bin/sh\n--submenu"),remediation:(("r"*130)+"TAIL_REMEDIATION")}]}')"
run_plugin; assert_has 'TAIL_CAUSE'; assert_has 'TAIL_REMEDIATION'; assert_has 'Cause (continued):'; assert_has 'Remediation (continued):'

# Window validity: only fill is clamped; raw conflicts remain root-visible.
start_case window_semantics
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].windows=[{name:"zero",used:0,limit:10,usage_percent:0},{name:"ratio",used:1,limit:8},{name:"tiny",usage_percent:0.0001},{name:"over",usage_percent:150},{name:"conflict",used:90,limit:100,usage_percent:50},{name:"zero-limit",used:0,limit:0},{name:"negative-used",used:-1,limit:10},{name:"missing"},{name:"bad",used:"lots",limit:10},{name:"reset",usage_percent:37,reset_at:"2026-09-21T12:00:00+02:00"}]')"
run_plugin
assert_has 'Window: zero [....................] 0% used'
assert_has 'Window: ratio [###.................] 12.5% used'
assert_has 'Window: tiny [....................] 0.0001% used'
assert_has 'Window: over [####################] 150% used (over limit)'
assert_has 'Window: conflict [##########..........] 50% used'
assert_has 'Raw quota: used 90 / limit 100; percent 50'
assert_has 'reported percentage disagrees'; assert_has 'Window: zero-limit [unknown             ] No data'
assert_has 'Raw quota: used 0 / limit 0'; assert_has 'Raw quota: used -1 / limit 10'; assert_has 'Raw quota: used lots / limit 10'
assert_has 'Window: missing [unknown             ] No data'; assert_has 'Reset: time unknown'
assert_has 'Reset: 2026-09-21T12:00:00+02:00 · 0s ago'; assert_lacks '+02:00 (UTC)'

start_case timestamp_fractional_offset
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .as_of="2026-09-21T06:00:00.123-04:00" | .providers[0].observation_at="2026-09-21T09:58:00.999Z"')"
run_plugin; assert_has 'Observation: 2026-09-21T09:58:00.999Z (UTC) · 2m ago'; assert_lacks '-04:00 (UTC)'

# Data cannot author attributes, submenu structure, symbols or extra actions.
start_case injection_actions_and_structure
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0] += {provider:"ev | refresh=true href=https://evil.example",reason:"--reason\nctrl\u0007 :arrow.up: :mushroom:",windows:[{name:"--w | bash=/bin/echo params0=x",usage_percent:50}]} | .pending_targets=["t | terminal=false href=x"]')"
doctor_fixture '{"findings":[{"code":"--error | refresh=true","severity":"error","message":"--body\n| bash=/bin/sh"}]}'
run_plugin; assert_has ':arrow.up: :mushroom:'; assert_has 'Window: --w   bash=/bin/echo params0=x'
assert_has 'Pending: t   terminal=false href=x'; assert_lacks 'symbolize=true'

# Typed contradictions must never enter pace or red-state decisions.
for mutation in '.status="unavailable"' '.freshness="banana" | .availability="unavailable"' '.status="banana"' '.eligible="yes"'; do
  start_case invalid_row
  status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 9)")" '$r | .providers[0] |= ('"$mutation"')')"
  run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Invalid provider: alpha'
  assert_lacks 'Observed unavailability:'
done

# CLI command contracts, malformed/empty/partial reports and independent reads.
for rc in 1 2 3 137 143; do
  start_case "status_exit_$rc"; stub_rc status "$rc"; run_plugin
  assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Provider: alpha'; assert_lacks 'timed out'
  case "$rc" in 1) assert_has 'status exit 1 (partial projection report)' ;; 2) assert_has 'quota issues reported' ;; *) assert_has 'status command failed' ;; esac
done
for rc in 0 1 3 137 143 127; do
  start_case "doctor_exit_$rc"; stub_rc doctor "$rc"; run_plugin; assert_has 'Doctor errors: 0'; assert_has 'Provider: alpha'; assert_lacks 'timed out'
  case "$rc" in 0|1) assert_lacks 'doctor command failed' ;; *) assert_has 'doctor command failed'; assert_header 'Quota :exclamationmark.triangle: | color=orange' ;; esac
done
for source in status doctor; do
  for kind in empty malformed shape; do
    start_case "${source}_$kind"
    case "$kind" in empty) : > "$CASE_DIR/$source.json" ;; malformed) printf '{bad' > "$CASE_DIR/$source.json" ;; shape) printf '{"unexpected":[]}' > "$CASE_DIR/$source.json" ;; esac
    run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
    case "$kind" in empty) assert_has "$source output is empty" ;; malformed) assert_has "$source output is malformed or unparseable" ;; shape) assert_has "$source output has an unexpected shape" ;; esac
    [ "$source" != doctor ] || assert_has 'Provider: alpha'
  done
done
start_case status_errors_and_top_errors
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r+{error:"status read failed",errors:[{scope:"provider",target_id:"global",mapping_id:"alpha",summary:"projection failed"}],problem:true}')"
doctor_fixture '{"error":"doctor read failed","findings":[]}'
run_plugin; assert_has 'Status error: status read failed'; assert_has 'Doctor error: doctor read failed'; assert_has 'Error: provider — global / alpha: projection failed'; assert_before 'projection failed' 'Provider: alpha'

# Config/dependency resolution remains fail-closed and never echoes secrets.
for key in jq_bin quota_bin polytoken_bin; do
  start_case "missing_$key"
  printf '%s=/nonexistent/SECRET_PATH_X\n' "$key" >> "$CASE_DIR/plugin.conf"
  # A duplicate is a config error, so supply one entry per key.
  case "$key" in
    jq_bin) printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=%s/bin/polytoken\njq_bin=/nonexistent/SECRET_PATH_X\n' "$CASE_DIR" "$CASE_DIR" > "$CASE_DIR/plugin.conf" ;;
    quota_bin) printf 'quota_bin=/nonexistent/SECRET_PATH_X\npolytoken_bin=%s/bin/polytoken\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf" ;;
    polytoken_bin) printf 'quota_bin=%s/bin/polytoken-quota\npolytoken_bin=/nonexistent/SECRET_PATH_X\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf" ;;
  esac
  run_plugin; assert_has "configured $key override is not executable"; assert_lacks SECRET_PATH_X
  [ ! -f "$CASE_DIR/ran" ] && ok || bad 'command ran with missing dependency'
done
for kind in unknown malformed duplicate empty control large; do
  start_case "config_$kind"
  case "$kind" in
    unknown) printf 'bogus=SECRET_VALUE_X\n' > "$CASE_DIR/plugin.conf" ;;
    malformed) printf 'SECRET_VALUE_X\n' > "$CASE_DIR/plugin.conf" ;;
    duplicate) printf 'jq_bin=%s\njq_bin=%s\n' "$JQ_ABS" "$JQ_ABS" > "$CASE_DIR/plugin.conf" ;;
    empty) printf 'quota_bin=\n' > "$CASE_DIR/plugin.conf" ;;
    control) printf 'quota_bin=SECRET_VALUE_X\001\n' > "$CASE_DIR/plugin.conf" ;;
    large) yes 'SECRET_VALUE_X' | head -c 70000 > "$CASE_DIR/plugin.conf" ;;
  esac
  run_plugin; assert_has 'Plugin configuration error'; assert_lacks SECRET_VALUE_X; assert_lacks bogus
  [ ! -f "$CASE_DIR/ran" ] && ok || bad 'command ran with bad config'
done
start_case config_explicit_missing
run_plugin "$ROOT/SECRET_VALUE_X/no.conf"; assert_has 'override configuration file is not readable'; assert_lacks SECRET_VALUE_X

start_case deps_home_search
run_plugin none; assert_header 'Quota +0.3 :arrow.up: | color=green'

start_case path_spaces_export
SPACED="$ROOT/$CASE_N directory with spaces"; mkdir -p "$SPACED"
cp "$CASE_DIR/bin/polytoken-quota" "$SPACED/polytoken-quota"; cp "$CASE_DIR/bin/polytoken" "$SPACED/polytoken"
printf 'quota_bin=%s/polytoken-quota\npolytoken_bin=%s/polytoken\njq_bin=%s\n' "$SPACED" "$SPACED" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
run_plugin; assert_header 'Quota +0.3 :arrow.up: | color=green'
[ "$(cat "$CASE_DIR/polytoken_seen")" = "$SPACED/polytoken" ] && ok || bad 'resolved Polytoken path not exported'
for mode in config-wins env-only bad-env; do
  start_case "polytoken_$mode"
  PLUGIN_ENV_POLYTOKEN_BINARY="$CASE_DIR/bin/polytoken"
  if [ "$mode" = config-wins ]; then PLUGIN_ENV_POLYTOKEN_BINARY=/nonexistent/SECRET_ENV_PATH; fi
  if [ "$mode" = env-only ] || [ "$mode" = bad-env ]; then
    printf 'quota_bin=%s/bin/polytoken-quota\njq_bin=%s\n' "$CASE_DIR" "$JQ_ABS" > "$CASE_DIR/plugin.conf"
  fi
  [ "$mode" != bad-env ] || PLUGIN_ENV_POLYTOKEN_BINARY=/nonexistent/SECRET_ENV_PATH
  run_plugin; assert_lacks SECRET_ENV_PATH
  if [ "$mode" = bad-env ]; then assert_has 'POLYTOKEN_BINARY override is not executable'
  else assert_header 'Quota +0.3 :arrow.up: | color=green'; [ "$(cat "$CASE_DIR/polytoken_seen")" = "$CASE_DIR/bin/polytoken" ] && ok || bad 'wrong exported override'; fi
  PLUGIN_ENV_POLYTOKEN_BINARY=""
done

# Deadline verdict is independent of exit status; whole owned groups are killed.
for kind in timeout termtrap child-timeout child-success partial-one partial-two termresist; do
  start_case "lifecycle_$kind"
  case "$kind" in
    timeout) printf 12 > "$CASE_DIR/status.sleep" ;;
    termtrap) printf 12 > "$CASE_DIR/status.sleep"; touch "$CASE_DIR/status.trapterm" ;;
    child-timeout) printf 12 > "$CASE_DIR/status.sleep"; touch "$CASE_DIR/status.child" ;;
    child-success) touch "$CASE_DIR/status.child" ;;
    partial-one) stub_rc status 1; touch "$CASE_DIR/status.child" ;;
    partial-two) stub_rc status 2; touch "$CASE_DIR/status.child" ;;
    termresist) stub_rc status 1; touch "$CASE_DIR/status.termresist" ;;
  esac
  run_plugin
  case "$kind" in timeout|termtrap|child-timeout) assert_has 'status command timed out after 10s' ;; child-success) assert_header 'Quota +0.3 :arrow.up: | color=green' ;; *) assert_has 'Provider: alpha' ;; esac
  case "$kind" in child-*|partial-*|termresist) assert_member_gone ;; esac
done
for kind in large continuous; do
  start_case "overflow_$kind"
  case "$kind" in large) yes '0123456789012345678901234567890123456789' | head -c 400000 > "$CASE_DIR/status.json" ;; continuous) touch "$CASE_DIR/status.flood" ;; esac
  run_plugin; assert_has 'status output exceeded the capture limit and was truncated'; assert_header 'Quota :exclamationmark.triangle: | color=orange'
done
# Valid input can expand beyond the rendered cap; never emit incomplete rows.
for source in status doctor; do
  start_case "rendered_overflow_$source"
  if [ "$source" = doctor ]; then
    doctor_fixture "$(jq -nc '{findings:[range(0;67)|{code:"permission",severity:"error",target_id:"global",message:("x"*2048),remediation:("r"*100)}]}')"
  else
    status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].reason=("x"*180000)')"
  fi
  [ "$(wc -c < "$CASE_DIR/$source.json")" -lt 262144 ] && ok || bad 'overflow input not below cap'
  run_plugin; assert_has "$source rendered details exceeded the capture limit — details omitted"
  assert_header 'Quota :exclamationmark.triangle: | color=orange'
  if [ "$source" = doctor ]; then assert_lacks 'Cause: x'; else assert_lacks 'Provider: alpha'; fi
done
# Capture setup must fail closed for commands and for jq rendering independently.
for mode in commands jq; do
  start_case "mkfifo_fail_$mode"
  mkdir -p "$CASE_DIR/fake"
  if [ "$mode" = commands ]; then
    printf '#!/bin/sh\nexit 1\n' > "$CASE_DIR/fake/mkfifo"
  else
    cat > "$CASE_DIR/fake/mkfifo" <<'STUB'
#!/bin/sh
n=0
[ ! -f "$STUB_DIR/mkfifo-count" ] || n=$(cat "$STUB_DIR/mkfifo-count")
n=$((n+1)); printf '%s' "$n" > "$STUB_DIR/mkfifo-count"
[ "$n" -le 2 ] || exit 1
exec /usr/bin/mkfifo "$@"
STUB
    # Use the actual system mkfifo, also on hosts where it is not /usr/bin.
    sed "s|/usr/bin/mkfifo|$MKFIFO_ABS|" "$CASE_DIR/fake/mkfifo" > "$CASE_DIR/fake/mkfifo.resolved"
    mv "$CASE_DIR/fake/mkfifo.resolved" "$CASE_DIR/fake/mkfifo"
  fi
  chmod +x "$CASE_DIR/fake/mkfifo"; PATH_PREPEND="$CASE_DIR/fake"
  run_plugin; PATH_PREPEND=""
  case "$mode" in
    commands) assert_has 'status capture setup failed'; assert_has 'doctor capture setup failed'; [ ! -f "$CASE_DIR/ran" ] && ok || bad 'failed capture ran command' ;;
    jq) assert_has 'jq capture setup failed'; assert_lacks 'Provider: alpha' ;;
  esac
done
printf '\n%d passed, %d failed across %d isolated cases\n' "$PASS" "$FAIL" "$CASE_N"
[ "$FAIL" = 0 ]
