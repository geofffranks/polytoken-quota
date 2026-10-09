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
PLUGIN_APPEARANCE=""; EXPECTED_COLOR=black
ok() { PASS=$((PASS+1)); }
bad() { FAIL=$((FAIL+1)); printf 'FAIL [%s]: %s\n' "$CASE_NAME" "$1"; }
# Expected provider headers use the display name, without a diagnostic prefix.
assert_has() { local text=$1; case "$text" in 'Provider '*) text=${text#Provider } ;; esac; if printf '%s\n' "$OUT" | grep -qF -- "$text"; then ok; else bad "missing: $text"; fi; }
assert_lacks() { local text=$1; if printf '%s\n' "$OUT" | grep -qF -- "$text"; then bad "unexpected: $text"; else ok; fi; }
assert_header() { local first wanted=$1; first=$(printf '%s\n' "$OUT" | head -n 1); case "$wanted" in *exclamationmark.triangle*|*color=orange*) wanted='​ | sfimage=gauge.medium sfcolor=orange dropdown=false' ;; *) wanted='​ | sfimage=gauge.medium dropdown=false' ;; esac; if [ "$first" = "$wanted" ]; then ok; else bad "header: $first (wanted $wanted)"; fi; }
assert_order() {
  local actual
  actual=$(printf '%s\n' "$OUT" | sed -n 's/^\([^ ]*\) — \(Available\|Enabled\|Gated\|Unavailable\|Disabled\).*/\1/p' | tr '\n' ' ')
  if [ "$actual" = "$1 " ]; then ok; else bad "provider order: $actual (wanted $1)"; fi
}
assert_before() {
  local a b
  local left=$1 right=$2
  case "$left" in 'Provider '*) left=${left#Provider } ;; esac
  case "$right" in 'Provider '*) right=${right#Provider }; right="$right —" ;; esac
  a=$(printf '%s\n' "$OUT" | grep -nF -- "$left" | head -n 1 | cut -d: -f1)
  b=$(printf '%s\n' "$OUT" | grep -nF -- "$right" | head -n 1 | cut -d: -f1)
  if [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]; then ok; else bad "expected $1 before $2"; fi
}
assert_layout() {
  local violations
  violations=$(printf '%s\n' "$OUT" | awk -v color="$EXPECTED_COLOR" '
    NR == 1 || /^---$/ { next }
    $0 == "Refresh status | refresh=true emojize=false symbolize=false color=" color { next }
    /^-/ { c++; next }
    $0 !~ (" \\| emojize=false symbolize=false color=" color "( font=Menlo)?$") { c++ }
    END { print c+0 }')
  [ "$violations" = 0 ] && ok || bad "nonliteral or nested rows: $violations"
  [ "$(printf '%s\n' "$OUT" | grep -c "^Refresh status | refresh=true emojize=false symbolize=false color=$EXPECTED_COLOR\$")" = 1 ] && ok || bad "refresh not unique"
  if printf '%s\n' "$OUT" | grep -v "^Refresh status | refresh=true emojize=false symbolize=false color=$EXPECTED_COLOR\$" | grep -qE '\|.*(refresh=|href=|bash=|terminal=|shell=|params[0-9]=|alternate=)'; then bad 'data-driven action'; else ok; fi
  assert_lacks 'length='
  assert_lacks '===PROVIDERS==='
  assert_lacks 'Evaluation time:'
  assert_lacks 'Doctor errors: 0'
  assert_lacks 'Evidence:'
  assert_lacks 'Polling:'
  assert_lacks 'Latest attempt: fresh'
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
  [ -z "$PLUGIN_APPEARANCE" ] || envs=("${envs[@]+"${envs[@]}"}" OS_APPEARANCE="$PLUGIN_APPEARANCE")
  OUT=$(env -i PATH="$path" HOME="$CASE_DIR" STUB_DIR="$CASE_DIR" "${envs[@]+"${envs[@]}"}" "$PLUGIN_BASH" "$PLUGIN" 2>/dev/null); RC=$?
  [ "$RC" = 0 ] && [ -n "$OUT" ] && ok || bad "plugin exit=$RC or empty output"
  assert_layout
}

# Explicit colors must agree across shell summaries, jq details and actions.
for appearance in Light Dark '' 'Dark | href=https://invalid.example'; do
  PLUGIN_APPEARANCE=$appearance
  case "$appearance" in Dark) EXPECTED_COLOR=white ;; *) EXPECTED_COLOR=black ;; esac
  start_case appearance_normal
  run_plugin
  assert_has 'alpha — Available'
  assert_has 'Weekly ['
  assert_lacks 'color=black,white'
  assert_lacks 'invalid.example'

  start_case appearance_degraded
  stub_rc status 3
  run_plugin
  assert_has 'Quota observation: 30s ago · Attention needed'
  assert_header 'color=orange'

  start_case appearance_bad_config
  printf 'unsupported=value\n' > "$CASE_DIR/plugin.conf"
  run_plugin
  assert_has 'Quota observation: time unknown · Attention needed'
  assert_header 'color=orange'
  [ ! -f "$CASE_DIR/ran" ] && ok || bad 'command ran with bad config'

done
PLUGIN_APPEARANCE=""; EXPECTED_COLOR=black

# Compactness is an observable contract, not just absence of submenus.
start_case compact_healthy
run_plugin
assert_has 'Quota observation: 30s ago · No errors'
assert_has 'alpha — Available'
assert_lacks 'Provider alpha'
assert_lacks 'Reason:'
assert_lacks 'WARNING:'
rows=$(printf '%s\n' "$OUT" | awk '/^alpha —/{on=1;next} on && /^---$/{exit} on{n++} END{print n+0}')
[ "$rows" = 1 ] && ok || bad "healthy provider has $rows quota rows, wanted 1"
assert_has 'Weekly [█████░░░░░] 50% used · Resets in 1d'
assert_lacks '===OBSERVATION==='
[ "$(printf '%s\n' "$OUT" | grep -c '^Quota observation:')" = 1 ] && ok || bad 'summary not unique'
  [ "$(printf '%s\n' "$OUT" | grep -c ' | sfimage=gauge.medium')" = 1 ] && ok || bad 'gauge not unique'
  assert_lacks 'Plugin configuration error'; assert_lacks 'Diagnostics:'; assert_lacks 'WARNING:'
start_case banked_resets_after_session
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].windows=[{name:"session",usage_percent:40},{name:"weekly",usage_percent:20}] | .providers[0] += {adapter:"codex",reset_credits:{freshness:"fresh",usable_count:2,available_expiries:["2026-09-22T10:00:00Z","2026-09-24T10:00:00+02:00"],discrepancy_count:0,latest_attempt:{status:"success"}}}')"
run_plugin; assert_has 'Banked resets: 2 available · Earliest expiry in 1d 0h'; assert_before 'Session [' 'Banked resets:'; assert_before 'Banked resets:' 'Weekly ['
[ "$(printf '%s\\n' "$OUT" | grep -c 'Banked resets:')" = 1 ] && ok || bad 'banked reset row not unique'
start_case banked_resets_partial_failed_no_session
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base renamed available 0.3)")" '$r | .providers[0] += {adapter:"codex",windows:[{name:"weekly",usage_percent:20}],reset_credits:{freshness:"stale",usable_count:1,available_expiries:["2026-09-22T10:00:00Z"],discrepancy_count:1,latest_attempt:{status:"failed",error:"must not display"}}}')"
run_plugin; assert_has 'Banked resets: 1 confirmed available · Partial data · Earliest known expiry in 1d 0h · Stale · Last known · Refresh failed'; assert_before 'Weekly [' 'Banked resets:'; assert_lacks 'must not display'
start_case banked_resets_unknown_and_legacy
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base codex available 0.3)")" '$r | .providers[0] += {adapter:"codex",windows:[{name:"weekly"}],reset_credits:{freshness:"missing",usable_count:"bad",latest_attempt:{status:"skipped"}}}')"
run_plugin; assert_has 'Banked resets: Unknown · Refresh skipped'
start_case banked_resets_non_codex_and_old_cli
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base codex available 0.3)")" '$r | .providers[0].adapter="other" | .providers[0].reset_credits={usable_count:9}')"
run_plugin; assert_lacks 'Banked resets:'
status_fixture "$(jq 'del(.providers[0].adapter,.providers[0].reset_credits)' "$CASE_DIR/status.json")"
run_plugin; assert_lacks 'Banked resets:'
start_case banked_resets_empty_mixed_and_bad_values
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].windows=[{name:"session"}] | .providers[0] += {adapter:"codex",reset_credits:{freshness:"fresh",usable_count:2,available_expiries:["2026-09-20T10:00:00Z","2026-09-21T10:00:00Z","bad | href=https://evil","2026-09-22T10:00:00+00:00"],discrepancy_count:1}}')"
run_plugin; assert_has 'Banked resets: 2 confirmed available · Partial data · Earliest known expiry in 1d 0h'; assert_lacks 'evil'
status_fixture "$(jq '.providers[0].reset_credits.usable_count=0 | .providers[0].reset_credits.available_expiries=[] | .providers[0].reset_credits.discrepancy_count=0' "$CASE_DIR/status.json")"
run_plugin; assert_has 'Banked resets: 0 available'
status_fixture "$(jq '.providers[0].reset_credits.usable_count=1 | .providers[0].reset_credits.available_expiries=["2026-09-21T10:00:00Z"] | .providers[0].reset_credits.discrepancy_count=0' "$CASE_DIR/status.json")"
run_plugin; assert_has 'Banked resets: 1 available · Expiry unknown'
status_fixture "$(jq '.providers[0].reset_credits="malformed"' "$CASE_DIR/status.json")"
run_plugin; assert_has 'Banked resets: Unknown'; assert_has 'Session ['
status_fixture "$(jq '.providers[0].reset_credits={usable_count:1,available_expiries:[42],latest_attempt:"bad"}' "$CASE_DIR/status.json")"
run_plugin; assert_has 'Banked resets: 1 confirmed available · Partial data · Expiry unknown'; assert_has 'Session ['
start_case compact_gated
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha gated -1)")" '$r | .providers[0].reason="signal-gated (-1 <= 0); peak, signal -1" | .providers[0].windows += [{name:"rolling",usage_percent:8,reset_at:"2026-09-26T01:00:00Z"}]')"
run_plugin
assert_has 'alpha — Gated · Pace -1 (not usable) · usage ahead of configured pace'
assert_has 'Rolling ['
assert_has 'Resets in 4d 15h'
assert_lacks 'Reason:'
assert_lacks 'peak, signal'
start_case combined_condition
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha gated -1)")" '$r | .providers[0].reason="signal-gated" | .providers[0].condition="waiting for reset"')"
run_plugin
assert_has 'alpha — Gated · Pace -1 (not usable) · usage ahead of configured pace · waiting for reset'
assert_lacks 'Condition:'
start_case long_condition
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha gated -1)")" '$r | .providers[0].condition=(("long condition " * 24) + "retained final cause")')"
run_plugin
assert_has 'Continued:'
assert_has 'retained final cause'
start_case unknown_summary
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | del(.last_checked)')"
run_plugin
assert_has 'Quota observation: time unknown · No errors'
start_case summary_errors
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .errors=[{scope:"global",summary:"saved failure"}]')"
doctor_fixture '{"findings":[{"severity":"error","code":"permission","target_id":"global","message":"permission denied","remediation":"repair permissions"}]}'
run_plugin
assert_has 'Quota observation: 30s ago · Attention needed · Errors: 2'
assert_lacks 'saved failure'
assert_lacks 'Remediation: repair permissions'
assert_lacks 'permission denied'
assert_lacks 'WARNING:'
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
assert_has 'near-high — Available · Pace +1.23'
assert_has 'unknown — Available'
assert_has 'stale — Available · Pace +999'
assert_lacks 'Provider near-high'
assert_lacks 'Ordered by available pace'
assert_lacks 'Data issue: missing'; assert_lacks 'Data issue: stale'; assert_lacks 'Ordered by available pace'
assert_header 'Quota :exclamationmark.triangle: | color=orange'

start_case tie_candidate_agrees
status_fixture "$(wrap_status "$(prov_base beta available 0.5),$(prov_base alpha available 0.5)")"
run_plugin; assert_order 'alpha beta'; assert_header 'Quota +0.5 :arrow.up: | color=green'

start_case no_signal_not_zero
status_fixture "$(wrap_status "$(prov_base alpha available null)")"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'alpha — Available'; assert_lacks 'Pace 0'; assert_lacks WARNING:

start_case red_fresh_unavailable
status_fixture "$(wrap_status "$(prov_base alpha unavailable -1)")" # correct the independent availability axis
status_fixture "$(jq '.providers[0].availability="unavailable" | .providers[0].eligible=false' "$CASE_DIR/status.json")"
run_plugin; assert_header 'Quota :xmark.circle: | color=red'; assert_has 'Provider alpha — Unavailable'

start_case disabled_readable
status_fixture "$(wrap_status "$(prov_base alpha disabled 9)")"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'alpha — Disabled · Pace +9 (not usable) · manually disabled'; assert_has 'Weekly ['; assert_lacks WARNING:

start_case empty_providers
status_fixture "$(wrap_status '')"
run_plugin; assert_header 'Quota :questionmark.circle:'; assert_has 'No providers are configured or projected.'

start_case legacy_nonpace_order
status_fixture '{"providers":[{"provider":"zeta","status":"available","rank":1,"eligible":true,"signal":99,"windows":[]},{"provider":"alpha","status":"available","rank":2,"eligible":true,"signal":100,"windows":[]}],"pending_targets":[]}'
run_plugin; assert_order 'zeta alpha'; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Provider zeta — Available'; assert_lacks 'Polling: status unknown'; assert_lacks 'Compatibility: required pace fields missing'

start_case latest_failed_older_fresh_snapshot
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0] += {observation_at:"2026-09-21T09:58:00Z",latest_attempt:{status:"failed",checked_at:"2026-09-21T09:59:00Z",error:"saved snapshot retained; adapter failed"}}')"
run_plugin; assert_header 'Quota | sfimage=exclamationmark.triangle sfcolor=orange dropdown=false'
assert_has 'Quota observation: 30s ago · Attention needed'
assert_lacks 'Data issue:'; assert_lacks 'saved snapshot retained'; assert_lacks 'adapter failed'
assert_lacks '2026-09-21T09:59:00Z'; assert_has 'Provider alpha — Available · Pace +0.3'
assert_lacks 'WARNING:'

start_case matching_attempt_once
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].latest_attempt={status:"failed",error:"adapter failed"}')"
doctor_fixture '{"findings":[{"code":"quota-attempt-failed","target_id":"alpha","severity":"warning","message":"provider alpha quota attempt failed: adapter failed","remediation":"retry the quota check"}]}'
run_plugin
assert_has 'Quota observation: 30s ago · Attention needed'
assert_lacks 'adapter failed'; assert_lacks 'retry the quota check'; assert_lacks 'different failure'
doctor_fixture '{"findings":[{"code":"quota-attempt-failed","target_id":"alpha","severity":"warning","message":"provider alpha quota attempt failed: different failure"}]}'
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks 'different failure'
start_case independent_stale_context
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].freshness="stale" | .providers[0].checked_at="2026-09-21T09:00:00Z"')"
doctor_fixture '{"findings":[{"code":"quota-stale-snapshot","target_id":"alpha","severity":"warning","message":"provider alpha quota snapshot is stale (last checked 2026-09-20T01:00:00Z; freshness TTL 30m)","remediation":"refresh the snapshot"}]}'
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks '2026-09-20T01:00:00Z'; assert_lacks 'freshness TTL 30m'; assert_lacks 'refresh the snapshot'
start_case partial_attempt
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].latest_attempt.status="partial"')"
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks 'Data issue:'; assert_header 'Quota :exclamationmark.triangle: | color=orange'

start_case polling_support_evidence
status_fixture "$(jq -nc --argjson p "$(prov_base template enabled null)" --argjson r "$(wrap_status '')" '$r+{providers:[($p+{provider:"disabled",freshness:"missing",polling_status:"disabled",windows:[]}|del(.latest_attempt,.availability)),($p+{provider:"unsupported",freshness:"missing",polling_status:"unsupported",windows:[]}|del(.latest_attempt,.availability)),($p+{provider:"unknown",freshness:"missing",windows:[]}|del(.latest_attempt,.polling_status,.availability))]}')"
run_plugin; assert_has 'Provider disabled — Enabled'; assert_has 'Provider unsupported — Enabled'; assert_has 'Provider unknown — Enabled'
assert_lacks 'Data issue: disabled'; assert_lacks 'Data issue: unsupported'; assert_lacks 'Data issue: unknown'
assert_lacks 'latest quota attempt failed'

start_case pending_latest_attempt
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r+{pending_targets:["global","project"],pending_details:[{target_id:"global",last_attempt_at:"2026-09-21T09:48:00Z"}]}')"
run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks 'Pending:'; assert_lacks 'duration unknown'; assert_lacks '12m ago'
status_fixture "$(jq '.pending_details[0].last_attempt_at="2026-09-21T09:59:00Z"' "$CASE_DIR/status.json")"
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks '1m ago'; assert_lacks 'duration unknown'

start_case doctor_warning_only_omitted
stub_rc doctor 1
doctor_fixture '{"findings":[{"code":"legacy-config-keys","severity":"warning","message":"routine policy advisory"}],"recovered":[{"target_id":"global","summary":"recovered history"}]}'
run_plugin; assert_header 'Quota +0.3 :arrow.up: | color=green'; assert_lacks 'Doctor errors:'
assert_lacks 'routine policy advisory'; assert_lacks 'recovered history'; assert_lacks WARNING:

start_case doctor_errors_and_independent_problems
stub_rc doctor 1
doctor_fixture '{"findings":[{"code":"permission","target_id":"global","file":"/private/stage","chain":"main","severity":"error","message":"permission denied","remediation":"repair permissions in the standalone root"},{"code":"quota-partial","target_id":"alpha","severity":"info","message":"some quota windows missing"},{"code":"target-pending","target_id":"project","severity":"warning","message":"latest attempt at 2026-09-01T00:00:00Z"},{"code":"journal-incomplete","severity":"warning","message":"publication interrupted"},{"code":"ordinary","severity":"warning","message":"omit this"}]}'
run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
assert_has 'Quota observation: 30s ago · Attention needed · Errors: 1'
assert_lacks 'permission denied'; assert_lacks 'Remediation:'; assert_lacks '/private/stage'
assert_lacks 'quota-partial'; assert_lacks 'publication interrupted'; assert_lacks 'omit this'; assert_lacks 'WARNING:'

start_case full_essential_details_literal
doctor_fixture "$(jq -nc '{findings:[{code:"permission",severity:"error",target_id:"global",message:(("x"*130)+"TAIL_CAUSE | bash=/bin/sh\n--submenu"),remediation:(("r"*130)+"TAIL_REMEDIATION")}]}')"
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed · Errors: 1'; assert_lacks 'TAIL_CAUSE'; assert_lacks 'TAIL_REMEDIATION'; assert_lacks 'Cause'; assert_lacks 'Remediation'

# Window validity: only fill is clamped; raw conflicts remain root-visible.
start_case window_semantics
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0].windows=[{name:"zero",used:0,limit:10,usage_percent:0},{name:"ratio",used:1,limit:8},{name:"tiny",usage_percent:0.0001},{name:"over",usage_percent:150},{name:"conflict",used:90,limit:100,usage_percent:50},{name:"zero-limit",used:0,limit:0},{name:"negative-used",used:-1,limit:10},{name:"missing"},{name:"bad",used:"lots",limit:10},{name:"reset",usage_percent:37,reset_at:"2026-09-21T12:00:00+02:00"}]')"
run_plugin
assert_has 'zero [░░░░░░░░░░] 0% used'
assert_has 'ratio [█░░░░░░░░░] 12.5% used'
assert_has 'tiny [░░░░░░░░░░] 0.0001% used'
assert_has 'over [██████████] 150% used (over limit)'
assert_has 'conflict [█████░░░░░] 50% used'
assert_has 'Raw quota: used 90 / limit 100; percent 50'
assert_has 'reported percentage disagrees'; assert_has 'zero-limit [unknown   ] No data'
assert_has 'Raw quota: used 0 / limit 0'; assert_has 'Raw quota: used -1 / limit 10'; assert_has 'Raw quota: used lots / limit 10'
assert_has 'missing [unknown   ] No data'; assert_lacks 'Reset: time unknown'
assert_has 'Reset due'; assert_lacks '+02:00 (UTC)'

start_case timestamp_fractional_offset
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .as_of="2026-09-21T06:00:00.123-04:00" | .providers[0].observation_at="2026-09-21T09:58:00.999Z"')"
run_plugin; assert_has 'Quota observation: 30s ago · No errors'; assert_lacks 'Observation:'; assert_lacks '-04:00 (UTC)'

# Data cannot author attributes, submenu structure, symbols or extra actions.
start_case injection_actions_and_structure
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .providers[0] += {provider:"ev | refresh=true href=https://evil.example",reason:"--reason\nctrl\u0007 :arrow.up: :mushroom:",windows:[{name:"--w | bash=/bin/echo params0=x",usage_percent:50}]} | .pending_targets=["t | terminal=false href=x"]')"
doctor_fixture '{"findings":[{"code":"--error | refresh=true","severity":"error","message":"--body\n| bash=/bin/sh"}]}'
run_plugin; assert_has 'ev   refresh=true href=https://evil.example — Available'; assert_has '· --w   bash=/bin/echo params0=x'; if printf '%s\n' "$OUT" | grep -q '^--w'; then bad 'window created submenu'; else ok; fi
status_fixture "$(jq '.providers[0].provider="--hostile"' "$CASE_DIR/status.json")"
run_plugin; assert_has '· --hostile — Available'
assert_lacks 'Pending:'; assert_lacks 'terminal=false'; assert_lacks 'symbolize=true'

# Typed contradictions must never enter pace or red-state decisions.
for mutation in '.status="unavailable"' '.freshness="banana" | .availability="unavailable"' '.status="banana"' '.eligible="yes"'; do
  start_case invalid_row
  status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 9)")" '$r | .providers[0] |= ('"$mutation"')')"
  run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Quota observation: 30s ago · Attention needed'
  assert_lacks 'Invalid provider:'; assert_lacks 'Observed unavailability:'
done

# CLI command contracts, malformed/empty/partial reports and independent reads.
for rc in 1 2 3 137 143; do
  start_case "status_exit_$rc"; stub_rc status "$rc"; run_plugin
  assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Provider alpha'; assert_lacks 'timed out'
  assert_has 'Quota observation:'; assert_lacks 'status command failed'; assert_lacks 'quota issues reported'; assert_lacks 'partial projection report'
done
for rc in 0 1 3 137 143 127; do
  start_case "doctor_exit_$rc"; stub_rc doctor "$rc"; run_plugin; assert_lacks 'Doctor errors:'; assert_has 'Provider alpha'; assert_lacks 'timed out'
  case "$rc" in 0|1) : ;; *) assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_has 'Quota observation: 30s ago · Attention needed' ;; esac
  assert_lacks 'doctor command failed'
done
for source in status doctor; do
  for kind in empty malformed shape; do
    start_case "${source}_$kind"
    case "$kind" in empty) : > "$CASE_DIR/$source.json" ;; malformed) printf '{bad' > "$CASE_DIR/$source.json" ;; shape) printf '{"unexpected":[]}' > "$CASE_DIR/$source.json" ;; esac
    run_plugin; assert_header 'Quota :exclamationmark.triangle: | color=orange'
    assert_has 'Quota observation:'; assert_lacks 'output is empty'; assert_lacks 'malformed or unparseable'; assert_lacks 'unexpected shape'
    [ "$source" != doctor ] || assert_has 'Provider alpha'
  done
done
start_case status_errors_and_top_errors
status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r+{error:"status read failed",errors:[{scope:"provider",target_id:"global",mapping_id:"alpha",summary:"projection failed"}],problem:true}')"
doctor_fixture '{"error":"doctor read failed","findings":[]}'
run_plugin; assert_has 'Quota observation: 30s ago · Attention needed · Errors: 1'; assert_lacks 'status read failed'; assert_lacks 'doctor read failed'; assert_lacks 'projection failed'; assert_has 'Provider alpha'

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
  run_plugin; assert_has 'Quota observation: time unknown · Attention needed'; assert_header 'Quota :exclamationmark.triangle: | color=orange'; assert_lacks 'configured'; assert_lacks SECRET_PATH_X
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
  run_plugin; assert_header 'Quota | sfimage=exclamationmark.triangle sfcolor=orange dropdown=false'; assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'Plugin configuration error'; assert_lacks 'Reason:'; assert_lacks 'Fix:'; assert_lacks SECRET_VALUE_X; assert_lacks bogus
  [ ! -f "$CASE_DIR/ran" ] && ok || bad 'command ran with bad config'
done
start_case config_explicit_missing
run_plugin "$ROOT/SECRET_VALUE_X/no.conf"; assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'override configuration'; assert_lacks SECRET_VALUE_X

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
  if [ "$mode" = bad-env ]; then assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'POLYTOKEN_BINARY override'
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
  case "$kind" in timeout|termtrap|child-timeout) assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'timed out' ;; child-success) assert_header 'Quota +0.3 :arrow.up: | color=green' ;; *) assert_has 'Provider alpha' ;; esac
  case "$kind" in child-*|partial-*|termresist) assert_member_gone ;; esac
done
for kind in large continuous; do
  start_case "overflow_$kind"
  case "$kind" in large) yes '0123456789012345678901234567890123456789' | head -c 400000 > "$CASE_DIR/status.json" ;; continuous) touch "$CASE_DIR/status.flood" ;; esac
  run_plugin; assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'capture limit'; assert_header 'Quota :exclamationmark.triangle: | color=orange'
done
# Valid input can expand beyond the rendered cap; never emit incomplete rows.
for source in status doctor; do
  start_case "rendered_overflow_$source"
  if [ "$source" = doctor ]; then
    doctor_fixture "$(jq -nc '{findings:[range(0;67)|{code:"permission",severity:"error",target_id:"global",message:("x"*2048),remediation:("r"*100)}]}')"
  else
    status_fixture "$(jq -nc --argjson r "$(wrap_status "$(prov_base alpha available 0.3)")" '$r | .error=("x"*180000)')"
  fi
  [ "$(wc -c < "$CASE_DIR/$source.json")" -lt 262144 ] && ok || bad 'overflow input not below cap'
  run_plugin; assert_has 'Quota observation: 30s ago · Attention needed'; assert_lacks 'rendered details exceeded'
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
    commands) assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'capture setup failed'; [ ! -f "$CASE_DIR/ran" ] && ok || bad 'failed capture ran command' ;;
    jq) assert_has 'Quota observation: time unknown · Attention needed'; assert_lacks 'capture setup failed'; assert_lacks 'Provider alpha' ;;
  esac
done
printf '\n%d passed, %d failed across %d isolated cases\n' "$PASS" "$FAIL" "$CASE_N"
[ "$FAIL" = 0 ]
