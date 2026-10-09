#!/bin/bash
# polytoken-quota.1m.sh — read-only SwiftBar menu-bar plugin for polytoken-quota.
#
# Refreshes once per minute (SwiftBar filename interval) by running exactly two
# read-only commands: `polytoken-quota status --json` and
# `polytoken-quota doctor --json`. It never checks quotas against providers,
# reconciles, mutates routing, or contacts a daemon, and it keeps no status
# cache and no credentials. The only action is SwiftBar's own `refresh=true`;
# every other line is plain text with parser-literal flags.
#
# Requirements: stock macOS Bash 3.2, jq (1.6+), and the polytoken-quota CLI
# (which itself requires a `polytoken` executable at startup). macOS 11+ is
# required for SF Symbols. See README.md in this directory for installation,
# overrides, and the meaning of every display element.
#
# Design notes:
# - Every data-derived string is literalized before it reaches SwiftBar: pipes,
#   newlines, and control characters are replaced, so report/config text can
#   never create actions, attributes, separators, or submenus. Every data line
#   starts with a fixed non-hyphen label so it can never be parsed as a submenu
#   level, and carries `emojize=false symbolize=false` so `:x:` sequences stay
#   literal.
# - The two reads are independent; the menu never claims one atomic snapshot.
# - Each subprocess runs under a portable watchdog (no GNU timeout): SIGTERM at
#   the deadline, SIGKILL one second later.
# - Exit codes 1 (partial projection) and 2 (quota problem) from `status` are
#   valid partial reports and are rendered; anything else is a visible warning.
#   Doctor exits 0/1 are diagnostic reports; other exits warn, and valid
#   findings still render independently.
# - A bad explicit override fails visibly; it never silently falls back.

set -u

# --- tunables ---------------------------------------------------------------

CMD_TIMEOUT=10          # seconds per subprocess (SIGTERM; SIGKILL 1s later)
JQ_TIMEOUT=5            # seconds per jq pass
OUTPUT_LIMIT=262144     # bytes of subprocess stdout kept (enforced during capture)
BUDGET_SECONDS=50       # outer refresh budget; later stages skip when exhausted
BAR_WIDTH=10            # characters in each quota window bar
PCT_DIFF_LIMIT=5.0      # percentage points marking reported-vs-derived disagreement

# SwiftBar supplies the current appearance; emit only a fixed, known color.
case "${OS_APPEARANCE-}" in
  Dark) TEXT_COLOR=white ;;
  *) TEXT_COLOR=black ;;
esac
LIT="emojize=false symbolize=false color=$TEXT_COLOR"

# Fallback search dirs (newline-separated) for minimal GUI PATH environments.
SEARCH_DIRS="/opt/homebrew/bin
/usr/local/bin
/usr/bin
/bin
$HOME/bin
$HOME/.local/bin
$HOME/go/bin"

CONFIG_NAME="polytoken-quota.conf"

# --- state ------------------------------------------------------------------

WARN_REASONS=""
CONFIG_ERROR=""

# status digest state (defaults keep `set -u` safe on every path)
COMPAT=0; STPROBLEM=0; STTOPERR=0; STERRS=0; STPENDING=0; STINVALID=0; STDATA=0
CAND=0; SIG=""; DIRV="none"; REDRULE=0; STATUS_OK=0; STATUS_PARSED=0
D_FIND=0; D_ACT=0; D_REC=0; DOCTOR_OK=0; DOCTOR_PARSED=0
QUOTA_BIN_CFG=""; POLYTOKEN_BIN_CFG=""; JQ_BIN_CFG=""
QUOTA_BIN_CFG_SET=""; POLYTOKEN_BIN_CFG_SET=""; JQ_BIN_CFG_SET=""
QUOTA_BIN=""; POLYTOKEN_BIN=""; JQ_BIN=""
STATUS_RC=""; DOCTOR_RC=""
BUDGET_DEADLINE=0

warn() {
  # Reasons are kept one per line: the menu renders each on its own bounded
  # line instead of one near-screenwidth joined line.
  WARN_REASONS="${WARN_REASONS:+$WARN_REASONS
}$1"
}

fail_config() {
  # $1 = already-sanitized fixed-vocabulary reason
  CONFIG_ERROR=$1
}

# --- temp workspace ---------------------------------------------------------

TMPDIR_LOCAL="$(mktemp -d "${TMPDIR:-/tmp}/polytoken-quota-swiftbar.XXXXXX")" || TMPDIR_LOCAL=""
OWNED_GROUPS_FILE="$TMPDIR_LOCAL/owned-groups"
cleanup() {
  # Plugin termination: kill every process group this run still owns (best
  # effort; groups whose members already exited are gone), then remove the
  # private workspace. SIGKILL of the plugin itself cannot be trapped — every
  # trappable termination path goes through here.
  if [ -n "$TMPDIR_LOCAL" ] && [ -f "$OWNED_GROUPS_FILE" ]; then
    while IFS= read -r g; do
      case "$g" in ''|*[!0-9]*) continue ;; esac
      kill -KILL "-$g" 2>/dev/null
    done < "$OWNED_GROUPS_FILE"
  fi
  [ -n "$TMPDIR_LOCAL" ] && rm -rf "$TMPDIR_LOCAL"
}
trap cleanup EXIT

# --- config file (strict, never sourced) ------------------------------------
# Optional sibling file `$CONFIG_NAME` (or the path in $POLYTOKEN_SWIFTBAR_CONFIG).
# Format: `key=value` lines, `#` comments, blank lines. Allowed keys:
#   quota_bin / polytoken_bin / jq_bin — executable name or absolute path.
# Values are used only as quoted command words; the file is never sourced or
# evaluated, and an invalid entry fails visibly instead of falling back.
# Diagnostics use a fixed vocabulary: the line number and, for value problems,
# the known key only. Rejected lines, unsupported key names, override values,
# and file paths are never echoed — diagnostics are omission, not sanitized
# echo, because sanitized output can still leak content.

read_config() {
  local conf="$1" line key val size n=0
  size=$(wc -c < "$conf" 2>/dev/null) || size=0
  case "$size" in ''|*[!0-9]*) size=0 ;; esac
  if [ "$size" -gt 65536 ]; then
    fail_config "configuration file is unreasonably large"
    return 1
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    case "$line" in
      ''|'#'*) continue ;;
    esac
    case "$line" in
      *=*) key=${line%%=*}; val=${line#*=} ;;
      *) fail_config "config line $n is not in key=value form"; return 1 ;;
    esac
    key=${key#"${key%%[![:space:]]*}"}
    key=${key%"${key##*[![:space:]]}"}
    val=${val#"${val%%[![:space:]]*}"}
    val=${val%"${val##*[![:space:]]}"}
    case "$key" in
      quota_bin|polytoken_bin|jq_bin) ;;
      *) fail_config "unsupported config key at line $n"; return 1 ;;
    esac
    case "$val" in
      *[[:cntrl:]]*) fail_config "config value for $key at line $n contains control characters"; return 1 ;;
      '') fail_config "config value for $key at line $n is empty"; return 1 ;;
    esac
    case "$key" in
      quota_bin)
        if [ "$QUOTA_BIN_CFG_SET" = 1 ]; then fail_config "duplicate config key quota_bin at line $n"; return 1; fi
        QUOTA_BIN_CFG_SET=1; QUOTA_BIN_CFG=$val ;;
      polytoken_bin)
        if [ "$POLYTOKEN_BIN_CFG_SET" = 1 ]; then fail_config "duplicate config key polytoken_bin at line $n"; return 1; fi
        POLYTOKEN_BIN_CFG_SET=1; POLYTOKEN_BIN_CFG=$val ;;
      jq_bin)
        if [ "$JQ_BIN_CFG_SET" = 1 ]; then fail_config "duplicate config key jq_bin at line $n"; return 1; fi
        JQ_BIN_CFG_SET=1; JQ_BIN_CFG=$val ;;
    esac
  done < "$conf"
  return 0
}

# --- executable resolution ---------------------------------------------------

resolve_exec() {
  # $1 = name or absolute/relative path. Prints resolved path; returns 1 if none.
  local name=$1 d
  case "$name" in
    */*)
      if [ -x "$name" ]; then printf '%s' "$name"; return 0; fi
      return 1 ;;
  esac
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
    return 0
  fi
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if [ -x "$d/$name" ]; then
      printf '%s' "$d/$name"
      return 0
    fi
  done <<EOF
$SEARCH_DIRS
EOF
  return 1
}

resolve_exec_check() {
  local out
  out=$(resolve_exec "$1") || return 1
  [ -n "$out" ] || return 1
  [ -x "$out" ] || return 1
  printf '%s' "$out"
}

# --- bounded subprocess runner (no GNU timeout) -------------------------------
# Each invocation runs inside a job-control subshell (`set -m`), so the
# command and every descendant it spawns form one process group owned by this
# plugin. The group — not a single PID — is signalled on deadline, and again
# after a successful exit if descendants linger; the plugin's EXIT trap kills
# any group still owned. The deadline is recorded in the private workspace
# independent of the command's exit status: a command that traps SIGTERM and
# exits 0 at the deadline is still reported as a timeout, and a command that
# naturally exits 143/137 is not. stdout flows through a FIFO into a
# `head -c` collector that stops reading one byte past the budget, so an
# unlimited producer is stopped early by SIGPIPE and cannot fill the disk;
# overflow beyond the budget is then reported visibly.

run_bounded() {
  # $1 = timeout seconds, $2 = stdout capture file, rest = command.
  # Returns 124 when the deadline fired, 125 when setup failed (the producer
  # is never run unbounded — fail closed), else the command's status.
  local t=$1 cap=$2 jpid coll pgid wpid cw outerpg rc fire fifo
  shift 2
  : > "$cap"
  fire="$TMPDIR_LOCAL/fired.$RANDOM.$RANDOM.$RANDOM"
  fifo="$TMPDIR_LOCAL/pipe.$RANDOM.$RANDOM"
  if ! mkfifo "$fifo" 2>/dev/null; then
    # Fail closed: without the FIFO the in-flight byte cap cannot exist, so
    # the producer is never started and the caller reports a setup failure.
    return 125
  fi
  (
    set -m 2>/dev/null
    exec 2>/dev/null    # silence job-control termination notices
    # The producer, its FIFO reader (head, the in-flight byte cap), and the
    # watchdog are separate jobs and therefore separate owned process groups.
    # Waiting on the producer alone means a descendant that inherited the FIFO
    # write end cannot stall the producer's exit status; the group kill below
    # releases the reader.
    "$@" < /dev/null > "$fifo" 2>/dev/null &
    jpid=$!
    head -c $((OUTPUT_LIMIT + 1)) "$fifo" > "$cap" 2>/dev/null &
    coll=$!
    # Under job control a simple background command leads its own process
    # group named by the child's PID. ps cannot be queried for it: a fast
    # producer may already have exited, so detect job control by comparing
    # against the outer shell's group and confirming the group exists.
    outerpg=$(ps -o pgid= -p "$$" 2>/dev/null | tr -d ' ')
    pgid=$jpid
    case "$outerpg" in ''|*[!0-9]*) outerpg="" ;; esac
    if [ -n "$outerpg" ] && [ "$pgid" = "$outerpg" ]; then
      # Job control unavailable: the producer shares this shell's group and
      # could not be bounded — kill it and fail closed below.
      pgid=""
    fi
    if [ -z "$pgid" ]; then
      # Fail closed: an unidentifiable group means neither the deadline nor
      # the cleanup could bound the producer, so it is killed and reported
      # as a setup failure rather than run unbounded. No direct-PID or
      # unbounded fallback exists.
      kill -KILL "$jpid" 2>/dev/null
      kill -KILL "$coll" 2>/dev/null
      wait "$jpid" 2>/dev/null
      wait "$coll" 2>/dev/null
      exit 125
    fi
    printf '%s\n' "$pgid" >> "$OWNED_GROUPS_FILE"
    ( sleep "$t"
      printf 'deadline-fired' > "$fire" 2>/dev/null
      kill -TERM "-$pgid" 2>/dev/null
      sleep 1
      kill -KILL "-$pgid" 2>/dev/null
    ) >/dev/null 2>&1 &
    wpid=$!
    wait "$jpid" 2>/dev/null
    rc=$?
    # The watchdog is never needed again: kill it and its whole group, which
    # takes its sleeping child with it (deterministic cleanup, no orphans).
    kill -TERM "-$wpid" 2>/dev/null
    wait "$wpid" 2>/dev/null
    # Clean the producer group on EVERY exit, not only success: a partial
    # report's descendants can hold the FIFO write end and would stall the
    # collector indefinitely. The KILL escalation removes TERM-resistant
    # members. The producer's own status and the deadline sentinel are
    # untouched by this cleanup.
    if kill -0 "-$pgid" 2>/dev/null; then
      kill -TERM "-$pgid" 2>/dev/null
      sleep 1
      kill -KILL "-$pgid" 2>/dev/null
    fi
    wait "$jpid" 2>/dev/null
    # Bound the collector shutdown: normal paths see EOF immediately; a
    # pathological holder gets SIGTERM after the grace period.
    ( sleep 2 && kill -TERM "-$coll" 2>/dev/null ) >/dev/null 2>&1 &
    cw=$!
    wait "$coll" 2>/dev/null
    kill -TERM "-$cw" 2>/dev/null
    wait "$cw" 2>/dev/null
    exit "$rc"
  ) 2>/dev/null
  rc=$?
  if [ -s "$fire" ]; then
    return 124
  fi
  return "$rc"
}

cap_check() {
  # Truncate a capture file to OUTPUT_LIMIT after the run (head already
  # stopped the producer at OUTPUT_LIMIT+1 during capture). Returns 0 when
  # overflow occurred so the caller can report it visibly.
  local f=$1 size
  size=$(wc -c < "$f" 2>/dev/null) || size=0
  case "$size" in ''|*[!0-9]*) size=0 ;; esac
  if [ "$size" -gt "$OUTPUT_LIMIT" ]; then
    head -c "$OUTPUT_LIMIT" "$f" > "$f.bounded" && mv "$f.bounded" "$f"
    return 0
  fi
  return 1
}

# --- outer refresh budget ------------------------------------------------------

budget_init() {
  local now
  now=$(date +%s 2>/dev/null) || now=0
  case "$now" in ''|*[!0-9]*) now=0 ;; esac
  BUDGET_DEADLINE=$((now + BUDGET_SECONDS))
}

budget_remaining() {
  local now
  now=$(date +%s 2>/dev/null) || now=0
  case "$now" in ''|*[!0-9]*) now=0 ;; esac
  echo $((BUDGET_DEADLINE - now))
}

# --- jq programs ---------------------------------------------------------------
# Digest: fixed-vocabulary key=value lines the shell reads without parsing JSON.
# Body: provider and quota-window menu lines; every data string passes through `safe`.

JQ_HELPERS='
def isnum($x): ($x|type)=="number" and (($x|isnan)|not) and (($x|isinfinite)|not);
def safe: if . == null then "" else (tostring
  | gsub("[\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}|]"; " ")
  | gsub("^ +| +$"; "")
  | .[0:240]) end;
def s2: if . == null then "unknown" else safe end;
def lit: " | emojize=false symbolize=false color='"$TEXT_COLOR"'";
# Split sanitized essential details into root rows, never tooltip-only text.
def detail($label; $text):
  ($text|tostring|gsub("[\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}|]"; " ")) as $s
  | if ($s|length) == 0 then "\($label): unknown\(lit)"
    else ($s | [scan(".{1,64}(?= +|$)|.{1,64}")]) as $parts
      | range(0; ($parts|length)) as $i
      | "\($label|safe)\(if $i > 0 then " (continued)" else "" end): \($parts[$i]|gsub("^ +| +$"; ""))\(lit)" end;
def epoch($ts):
  try ((($ts | capture("^(?<date>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$")) // error("invalid timestamp")) as $m
    | ($m.date + "Z" | fromdateiso8601) -
      (if $m.zone == "Z" then 0 else
         (($m.zone[1:3]|tonumber)*3600 + ($m.zone[4:6]|tonumber)*60) *
         (if $m.zone[0:1] == "+" then 1 else -1 end) end))
  catch null;
def span($s):
  if $s < 60 then "\($s|floor)s" elif $s < 3600 then "\($s/60|floor)m"
  elif $s < 86400 then "\($s/3600|floor)h" else "\($s/86400|floor)d \(($s%86400/3600)|floor)h" end;
def countdown($s):
  if $s < 0 then "now" else "\($s/86400|floor)d \(($s%86400/3600)|floor)h" end;
def tz($ts): ($ts|tostring) as $s | if ($s|endswith("Z") or ($s|endswith("z"))) then " (UTC)" else "" end;
def stamp($ts; $asof):
  if $ts == null or $ts == "" then "time unknown"
  else epoch($ts) as $t | epoch($asof) as $n
    | if $t != null and $n != null then
        (if $n >= $t then span($n-$t) + " ago" else "in " + span($t-$n) end)
      else ($ts|safe) + tz($ts) end end;
def fmt2($n): ((($n*100)|round)/100) | tostring;
def fmtsig($s):
  if isnum($s) then
    if $s == 0 then "0"
    elif (($s|fabs) < 0.005) then (if $s > 0 then "~+0" else "~-0" end)
    else (if $s > 0 then "+" else "-" end) + fmt2($s|fabs)
    end
  else "unknown" end;
def bar($pct):
  (if $pct < 0 then 0 elif $pct > 100 then 100 else $pct end) as $v
  | ((($v/100)*'$BAR_WIDTH')|round) as $n
  | ((("█"*$n) + ("░"*('$BAR_WIDTH'-$n))));
def window_pct($w):
  (isnum($w.usage_percent)) as $has_rep
  | (isnum($w.used) and ($w.used >= 0) and isnum($w.limit) and ($w.limit > 0)) as $can_derive
  | { pct: (if $has_rep then $w.usage_percent
            elif $can_derive then (($w.used/$w.limit)*100)
            else null end),
      src: (if $has_rep then "reported"
            elif $can_derive then "derived from used/limit"
            else "unknown" end),
      bad: (( $w.used != null and (isnum($w.used)|not) )
            or ( $w.used != null and isnum($w.used) and $w.used < 0 )
            or ( $w.limit != null and (isnum($w.limit)|not) )
            or ( $w.limit != null and isnum($w.limit) and $w.limit <= 0 )) };
# Row validity: typed/enum checks for the fields pace and unavailability
# decisions rely on. $compat suppresses the freshness check when the connected
# CLI predates the additive fields (their absence is expected there, not a
# data error). A list of problem strings; empty means the row is valid.
def row_problems($compat):
  [ (if ((.provider|type) == "string" and (.provider|length) > 0)
     then empty else "missing provider name" end),
    (if .status == null then "missing consolidated status"
     elif .status == "enabled" or .status == "gated" or .status == "disabled" or .status == "available" or .status == "unavailable"
     then empty else "unsupported consolidated status" end),
    (if $compat or .freshness == "fresh" or .freshness == "stale" or .freshness == "missing"
     then empty else "missing or unsupported freshness" end),
    (if .availability == null then empty
     elif .availability == "available" or .availability == "unavailable" or .availability == "unknown"
     then empty else "unsupported availability" end),
    (if .status == "unavailable" and .availability == "available"
     then "consolidated status contradicts availability" else empty end),
    (if (.eligible|type) == "boolean" then empty else "missing or non-boolean eligibility" end),
    (if (.rank|type) == "number" then empty else "missing or non-numeric rank" end),
    (if .signal == null or ((.signal|type) == "number") then empty else "non-numeric signal" end)];
def row_valid($compat): ((row_problems($compat)) | length) == 0;
def compat_mode($r):
  any($r.providers[]?; .freshness == null or (.eligible|type) != "boolean" or .rank == null
      or (.status == "available" and .availability == null))
  or (($r.providers|length) == 0 and ($r.as_of // "") == "" and ($r.error // "") == "");
def usable($compat):
  ($compat|not) and .status == "available" and .availability == "available"
  and .eligible == true and isnum(.signal) and .freshness == "fresh" and row_valid(false);
def candidates($ps; $compat):
  [$ps[]? | select(usable($compat))] | sort_by([(-.signal), .rank, .provider]);
def tier($compat):
  if usable($compat) then 0
  elif .status == "gated" or .status == "unavailable" or .status == "disabled" then 2
  else 1 end;
def ordered($ps; $compat):
  $ps | sort_by(if $compat then [(if isnum(.rank) then .rank else 1e99 end), (.provider|s2)]
    else [(tier($compat)), (if tier($compat) == 0 then -.signal else 0 end),
          (if tier($compat) == 2 then (.status|s2) else "" end),
          (if tier($compat) == 2 then 0 elif isnum(.rank) then .rank else 1e99 end), (.provider|s2)] end);
def reason:
  if .status == "disabled" then "manually disabled"
  elif .status == "gated" then
    if ((.reason // "")|startswith("signal-gated")) then "usage ahead of configured pace"
    elif ((.reason // "")|contains("out of quota")) then "out of quota"
    elif .freshness == "missing" then "quota evidence unavailable/unknown"
    else "policy gate holds provider off" end
  elif .status == "unavailable" then "quota unavailable (not necessarily exhausted)"
  elif .freshness == "missing" then "quota evidence unavailable/unknown"
  elif .freshness == "stale" then "saved quota evidence stale"
  elif .eligible == false then "not eligible for routing"
  else "" end;
def title_safe: safe | if startswith("-") then "· " + . else . end;
# Keep ordinary headers on one row; retain long conditions in root continuations.
def provider_header:
  gsub("[\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}|]"; " ")
  | [scan(".{1,120}(?= +|$)|.{1,120}")] as $parts
  | range(0; $parts|length) as $i
  | (if $i == 0 then $parts[$i] else "Continued: " + $parts[$i] end) + lit;
def reset_label($ts; $asof):
  epoch($ts) as $t | epoch($asof) as $n
  | if $t != null and $n != null then
      if $t <= $n then "Reset due" else "Resets in " + span($t-$n) end
    elif $ts == null then "" else "Reset: " + stamp($ts; $asof) end;
def data_problems:
  [ (if .latest_attempt != null and (.latest_attempt.status != "fresh")
       then "latest quota attempt \((.latest_attempt.status|s2))" + (if (.latest_attempt.error // "") != "" then " — " + .latest_attempt.error else "" end) else empty end),
    (if .polling_status != "disabled" and .polling_status != "unsupported" then
       if .freshness == "missing" then "missing quota evidence"
       elif .freshness == "stale" then "stale quota evidence"
       elif .availability == "unknown" then "unknown quota availability"
       elif .availability == "available" and ((.windows // [])|length) == 0 then "missing quota windows"
       else empty end else empty end),
    (.windows[]? | window_pct(.) | select(.pct == null or .bad) | "missing or invalid quota window data") ];
def independent_problem:
  .code == "target-pending" or .code == "quota-reconcile-pending"
  or .code == "journal-incomplete" or .code == "state-unreadable"
  or .code == "quota-attempt-failed" or .code == "quota-partial"
  or .code == "quota-partial-unusable" or .code == "quota-unusable"
  or .code == "quota-missing-snapshot" or .code == "quota-stale-snapshot";
def red_rule($ps; $compat):
  (([$ps[]? | select(row_valid($compat) and .status != "disabled")] | length) > 0)
  and (([$ps[]?
         | select(row_valid($compat)
                  and .status != "disabled"
                  and (.freshness != "fresh" or .availability != "unavailable"))]
        | length) == 0)
  and (([$ps[]? | select(row_valid($compat)|not)] | length) == 0);
'

JQ_STATUS_DIGEST="$JQ_HELPERS"'
. as $r
| if ($r|type) != "object" or ($r.providers|type) != "array"
  then "shape=bad"
  else
    compat_mode($r) as $compat
    | (if $compat then "compat=1" else "compat=0" end),
      (if $r.problem == true then "problem=1" else "problem=0" end),
      (if (($r.error // "") != "") then "toperr=1" else "toperr=0" end),
      "errs=\([$r.errors[]?] | length)",
      "pending=\([$r.pending_targets[]?] | length)",
      "invalid=\([$r.providers[]? | select(row_valid($compat)|not)] | length)",
      "data=\([$r.providers[]? | data_problems[]] | length)",
      (candidates($r.providers; $compat)) as $cands
      | (if ($cands|length) > 0 then
           "cand=1",
           "sig=\(fmtsig($cands[0].signal))",
           "dir=\(if $cands[0].signal > 0 then "up" elif $cands[0].signal < 0 then "down" else "right" end)"
         else
           "cand=0", "sig=unknown", "dir=none"
         end),
      (if red_rule($r.providers; $compat) then "red=1" else "red=0" end)
  end
'

JQ_STATUS_BODY="$JQ_HELPERS"'
. as $r
| if ($r|type) != "object" or ($r.providers|type) != "array" then "Status output has an unexpected shape.\(lit)"
  else
    $r.providers as $ps | compat_mode($r) as $compat
    | "===OBSERVATION===\(stamp($r.last_checked; $r.as_of))",
      "===PROVIDERS===",
      (if ($ps|length) == 0 then "No providers are configured or projected.\(lit)" else empty end),
      (ordered($ps; $compat)[] | . as $p
       | "---",
         ("\(($p.provider|title_safe)) — \(if $p.status == "available" then "Available" elif $p.status == "enabled" then "Enabled (quota unknown)" elif $p.status == "gated" then "Gated" elif $p.status == "unavailable" then "Unavailable" elif $p.status == "disabled" then "Disabled" else "Status unknown" end)\(if isnum($p.signal) and ($compat|not) then " · Pace " + fmtsig($p.signal) + (if usable($compat) then "" else " (not usable)" end) else "" end)\(if (reason|length) > 0 then " · " + reason else "" end)\(if (($p.condition // "")|safe) != "" and ($p.status == "unavailable" or $p.status == "gated") then " · " + ($p.condition|tostring) else "" end)" | provider_header),
         (if (($p.windows // [])|length) == 0 then
            "Quota: No data\(if $p.polling_status == "disabled" then " (polling disabled)" elif $p.polling_status == "unsupported" then " (polling unsupported)" elif $p.polling_status == null or $p.polling_status == "unknown" then " (polling status unknown)" else "" end)\(lit)"
          else empty end),
         (($p.windows // [])[]? | . as $w | window_pct($w) as $wp
          | (isnum($w.usage_percent) and isnum($w.used) and $w.used >= 0 and isnum($w.limit) and $w.limit > 0
              and ((($w.usage_percent) - (($w.used/$w.limit)*100))|fabs) > '$PCT_DIFF_LIMIT') as $conflict
          | "\(if $w.name == "subscription_kwh" then "Subscription" elif $w.name == "rolling" then "Rolling" elif $w.name == "session" then "Session" elif $w.name == "weekly" then "Weekly" elif $w.name == "daily" then "Daily" else ($w.name|title_safe) end) [\(if $wp.pct == null then ("unknown" + (" "*('$BAR_WIDTH'-7))) else bar($wp.pct) end)] \(if $wp.pct == null then "No data" else "\($wp.pct)% used" end)\(if $wp.pct != null and $wp.pct > 100 then " (over limit)" else "" end)\(reset_label($w.reset_at; $r.as_of) as $label | if $label != "" then " · " + $label else "" end)\(lit) font=Menlo",
            (if $wp.bad or $conflict then
               "Raw quota: used \(($w.used|s2)) / limit \(($w.limit|s2)); percent \(($w.usage_percent|s2))\(lit)",
               (if $conflict then "Data note: reported percentage disagrees with used/limit; both retained.\(lit)" else "Data note: invalid supplied numbers ignored for bar.\(lit)" end)
             else empty end)))
  end
'


# --- startup: config + dependencies -------------------------------------------

CONFIG_PATH="${POLYTOKEN_SWIFTBAR_CONFIG:-}"
if [ -n "$CONFIG_PATH" ]; then
  if [ ! -f "$CONFIG_PATH" ] || [ ! -r "$CONFIG_PATH" ]; then
    fail_config "override configuration file is not readable"
  fi
else
  SCRIPT_PATH=${BASH_SOURCE[0]:-$0}
  SCRIPT_DIR=$(cd "$(dirname "$SCRIPT_PATH")" 2>/dev/null && pwd) || SCRIPT_DIR=""
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/$CONFIG_NAME" ]; then
    CONFIG_PATH="$SCRIPT_DIR/$CONFIG_NAME"
  fi
fi
if [ -z "$CONFIG_ERROR" ] && [ -n "$CONFIG_PATH" ]; then
  read_config "$CONFIG_PATH" || true
fi

if [ -n "$CONFIG_ERROR" ]; then
  printf '%s\n' "​ | sfimage=gauge.medium sfcolor=orange dropdown=false"
  printf '%s\n' "---"
  printf '%s | %s\n' "Quota observation: time unknown · Attention needed" "$LIT"
  printf '%s\n' "Refresh status | refresh=true $LIT"
  exit 0
fi

# quota CLI
if [ -n "$QUOTA_BIN_CFG" ]; then
  QUOTA_BIN=$(resolve_exec_check "$QUOTA_BIN_CFG") || warn "configured quota_bin override is not executable"
else
  QUOTA_BIN=$(resolve_exec_check "polytoken-quota") || warn "polytoken-quota CLI not found (install it or set quota_bin)"
fi
# jq
if [ -n "$JQ_BIN_CFG" ]; then
  JQ_BIN=$(resolve_exec_check "$JQ_BIN_CFG") || warn "configured jq_bin override is not executable"
else
  JQ_BIN=$(resolve_exec_check "jq") || warn "required dependency not found: jq"
fi
# polytoken prerequisite (mandatory for the quota CLI at startup). The quota
# CLI resolves Polytoken itself from POLYTOKEN_BINARY or PATH, so the plugin
# exports the resolved path for both reads. Precedence: the config override
# wins over an inherited POLYTOKEN_BINARY, which is validated and passed
# through unchanged; with neither set, the search result is exported so a
# minimal GUI PATH still works.
if [ -n "$POLYTOKEN_BIN_CFG" ]; then
  POLYTOKEN_BIN=$(resolve_exec_check "$POLYTOKEN_BIN_CFG") || warn "configured polytoken_bin override is not executable"
  if [ -n "$POLYTOKEN_BIN" ]; then
    export POLYTOKEN_BINARY="$POLYTOKEN_BIN"
  fi
elif [ -n "${POLYTOKEN_BINARY:-}" ]; then
  POLYTOKEN_BIN=$(resolve_exec_check "$POLYTOKEN_BINARY") || warn "POLYTOKEN_BINARY override is not executable"
else
  POLYTOKEN_BIN=$(resolve_exec_check "polytoken") || warn "polytoken prerequisite not found — set polytoken_bin"
  if [ -n "$POLYTOKEN_BIN" ]; then
    export POLYTOKEN_BINARY="$POLYTOKEN_BIN"
  fi
fi

# --- reads ---------------------------------------------------------------------

STATUS_FILE="$TMPDIR_LOCAL/status.json"
DOCTOR_FILE="$TMPDIR_LOCAL/doctor.json"

budget_init

if [ -n "$QUOTA_BIN" ] && [ -n "$JQ_BIN" ] && [ -n "$POLYTOKEN_BIN" ]; then
  if [ "$(budget_remaining)" -ge $((CMD_TIMEOUT + 2)) ]; then
    if run_bounded "$CMD_TIMEOUT" "$STATUS_FILE" "$QUOTA_BIN" status --json; then
      STATUS_RC=0
    else
      STATUS_RC=$?
    fi
  else
    STATUS_RC=nobudget
    warn "refresh budget exhausted before the status read"
  fi
  if cap_check "$STATUS_FILE"; then
    warn "status output exceeded the capture limit and was truncated"
  fi
  if [ "$STATUS_RC" != nobudget ] && [ "$(budget_remaining)" -ge $((CMD_TIMEOUT + 2)) ]; then
    if run_bounded "$CMD_TIMEOUT" "$DOCTOR_FILE" "$QUOTA_BIN" doctor --json; then
      DOCTOR_RC=0
    else
      DOCTOR_RC=$?
    fi
  else
    DOCTOR_RC=nobudget
    warn "refresh budget exhausted before the diagnostics read"
  fi
  if cap_check "$DOCTOR_FILE"; then
    warn "diagnostics output exceeded the capture limit and was truncated"
  fi
else
  STATUS_RC="skipped"
  DOCTOR_RC="skipped"
fi

# --- classification helpers -----------------------------------------------------

rc_text() {
  case "$1" in
    0) printf 'exit 0' ;;
    1) printf 'exit 1 (partial projection report)' ;;
    2) printf 'exit 2 (quota problem report)' ;;
    124) printf 'timed out' ;;
    125) printf 'capture or process-group setup failed' ;;
    skipped) printf 'not run (missing dependency)' ;;
    nobudget) printf 'not run (refresh budget exhausted)' ;;
    *) printf 'unexpected exit %s' "$1" ;;
  esac
}

# --- status interpretation -------------------------------------------------------

case "$STATUS_RC" in
  1) warn "status $(rc_text 1)" ;;
  2) : ;; # the problem flag below supplies the single quota-issues warning
  124) warn "status command timed out after ${CMD_TIMEOUT}s" ;;
  125) warn "status capture setup failed — command not run" ;;
  0|skipped|nobudget) : ;;
  *) warn "status command failed ($(rc_text "$STATUS_RC"))" ;;
esac

STATUS_DIGEST="$TMPDIR_LOCAL/status.digest"
STATUS_BODY="$TMPDIR_LOCAL/status.body"

if [ "$STATUS_RC" != "skipped" ] && [ "$STATUS_RC" != nobudget ] && [ "$STATUS_RC" != 125 ] && [ -n "$JQ_BIN" ] && [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
  run_bounded "$JQ_TIMEOUT" "$STATUS_DIGEST" "$JQ_BIN" -r "$JQ_STATUS_DIGEST" "$STATUS_FILE" 2>/dev/null
  jqrc=$?
  if [ "$jqrc" -eq 0 ] && [ -s "$STATUS_DIGEST" ]; then
    BADSHAPE=0
    while IFS= read -r dline; do
      case "$dline" in
        shape=bad) BADSHAPE=1 ;;
        compat=1) COMPAT=1 ;;
        problem=1) STPROBLEM=1 ;;
        toperr=1) STTOPERR=1 ;;
        errs=*) STERRS=${dline#errs=} ;;
        pending=*) STPENDING=${dline#pending=} ;;
        invalid=*) STINVALID=${dline#invalid=} ;;
        data=*) STDATA=${dline#data=} ;;
        cand=1) CAND=1 ;;
        sig=*) SIG=${dline#sig=} ;;
        dir=*) DIRV=${dline#dir=} ;;
        red=1) REDRULE=1 ;;
        *) : ;;
      esac
    done < "$STATUS_DIGEST"
    case "$STERRS" in ''|*[!0-9]*) STERRS=0 ;; esac
    case "$STPENDING" in ''|*[!0-9]*) STPENDING=0 ;; esac
    case "$STINVALID" in ''|*[!0-9]*) STINVALID=0 ;; esac
    case "$SIG" in 0|'~+0'|'~-0'|+*|-*) : ;; *) SIG="" ;; esac
    case "$DIRV" in up|down|right|none) : ;; *) DIRV="none" ;; esac
    if [ "$BADSHAPE" = 1 ]; then
      warn "status output has an unexpected shape"
    else
      STATUS_OK=1
      [ "$COMPAT" = 1 ] && warn "required pace fields absent or incompatible — pace unavailable; order by rank/name"
      if [ "$STPROBLEM" = 1 ] || [ "$STATUS_RC" = 2 ]; then
        warn "quota issues reported"
      fi
      [ "$STTOPERR" = 1 ] && warn "status reported an error"
      [ "$STERRS" -gt 0 ] && warn "status reported $STERRS diagnostic error(s)"
      [ "$STPENDING" -gt 0 ] && warn "$STPENDING pending reconciler target(s)"
      [ "$STINVALID" -gt 0 ] && warn "status contained $STINVALID invalid or contradictory provider row(s)"
      case "$STDATA" in ''|*[!0-9]*) STDATA=0 ;; esac
      [ "$STDATA" -gt 0 ] && warn "provider collection/data problems: $STDATA"
      if [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
        run_bounded "$JQ_TIMEOUT" "$STATUS_BODY" "$JQ_BIN" -r "$JQ_STATUS_BODY" "$STATUS_FILE" 2>/dev/null
        jqrc=$?
        if cap_check "$STATUS_BODY"; then
          warn "status rendered details exceeded the capture limit — details omitted"
        elif [ "$jqrc" -eq 0 ]; then
          STATUS_PARSED=1
        elif [ "$jqrc" -eq 125 ]; then
          warn "jq capture setup failed — status details omitted"
        else
          warn "status output could not be rendered"
        fi
      else
        warn "refresh budget exhausted; status details omitted"
      fi
    fi
  else
    if [ "$jqrc" -eq 125 ]; then
      warn "jq capture setup failed — status details omitted"
    elif [ -s "$STATUS_FILE" ]; then
      warn "status output is malformed or unparseable"
    else
      warn "status output is empty"
    fi
  fi
fi

# Pace claims in the header require renderable details beneath them.
if [ "$STATUS_PARSED" != 1 ]; then
  CAND=0; REDRULE=0; SIG=""; DIRV="none"
fi

# --- doctor interpretation --------------------------------------------------------

case "$DOCTOR_RC" in
  0|1) : ;; # findings parsed independently below; the actionable count decides
  124) warn "doctor command timed out after ${CMD_TIMEOUT}s" ;;
  125) warn "doctor capture setup failed — command not run" ;;
  skipped|nobudget) : ;;
  *) warn "doctor command failed ($(rc_text "$DOCTOR_RC"))" ;;
esac

DOCTOR_DIGEST="$TMPDIR_LOCAL/doctor.digest"

if [ "$DOCTOR_RC" != "skipped" ] && [ "$DOCTOR_RC" != nobudget ] && [ "$DOCTOR_RC" != 125 ] && [ -n "$JQ_BIN" ] && [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
  run_bounded "$JQ_TIMEOUT" "$DOCTOR_DIGEST" "$JQ_BIN" -r "$JQ_HELPERS"'
      . as $r
      | if ($r|type) != "object" or ($r.findings|type) != "array" then "shape=bad"
        else "find=\([$r.findings[]? | select(.severity == "error")] | length)",
             "act=\([$r.findings[]? | select(.severity == "error" or independent_problem)] | length)",
             "rec=0",
             (if (($r.error // "") != "") then "toperr=1" else "toperr=0" end)
        end' "$DOCTOR_FILE" 2>/dev/null
  jqrc=$?
  if [ "$jqrc" -eq 0 ] && [ -s "$DOCTOR_DIGEST" ]; then
    BADDSHAPE=0
    while IFS= read -r dline; do
      case "$dline" in
        shape=bad) BADDSHAPE=1 ;;
        find=*) D_FIND=${dline#find=} ;;
        act=*) D_ACT=${dline#act=} ;;
        rec=*) D_REC=${dline#rec=} ;;
        toperr=1) warn "doctor reported an error" ;;
        *) : ;;
      esac
    done < "$DOCTOR_DIGEST"
    case "$D_FIND" in ''|*[!0-9]*) D_FIND=0 ;; esac
    case "$D_ACT" in ''|*[!0-9]*) D_ACT=0 ;; esac
    case "$D_REC" in ''|*[!0-9]*) D_REC=0 ;; esac
    if [ "$BADDSHAPE" = 1 ]; then
      warn "doctor output has an unexpected shape"
    else
      DOCTOR_OK=1
      [ "$D_FIND" -gt 0 ] && warn "doctor error-level findings: $D_FIND"
      [ "$D_ACT" -gt "$D_FIND" ] && warn "doctor reports independent data/pending problems"
    fi
  else
    if [ "$jqrc" -eq 125 ]; then
      warn "jq capture setup failed — diagnostics details omitted"
    elif [ -s "$DOCTOR_FILE" ]; then
      warn "doctor output is malformed or unparseable"
    else
      warn "doctor output is empty"
    fi
  fi
fi

# --- header decision ----------------------------------------------------------------

if [ -n "$WARN_REASONS" ]; then
  HEADER="​ | sfimage=gauge.medium sfcolor=orange dropdown=false"
else
  HEADER="​ | sfimage=gauge.medium dropdown=false"
fi

# --- assemble menu --------------------------------------------------------------------

printf '%s\n' "$HEADER"
printf '%s\n' "---"

OBSERVATION="time unknown"
if [ "$STATUS_PARSED" = 1 ]; then
  while IFS= read -r line; do
    case "$line" in
      '===OBSERVATION==='*) OBSERVATION=${line#===OBSERVATION===}; break ;;
    esac
  done < "$STATUS_BODY"
fi
SUMMARY="No errors"
[ -z "$WARN_REASONS" ] || SUMMARY="Attention needed"
ERROR_COUNT=$((STERRS + D_FIND))
[ "$ERROR_COUNT" -eq 0 ] || SUMMARY="$SUMMARY · Errors: $ERROR_COUNT"
printf '%s | %s\n' "Quota observation: $OBSERVATION · $SUMMARY" "$LIT"

if [ "$STATUS_PARSED" = 1 ]; then
  printf '%s\n' '---'
  providers=0
  while IFS= read -r line; do
    if [ "$line" = '===PROVIDERS===' ]; then providers=1; continue; fi
    [ "$providers" = 1 ] && printf '%s\n' "$line"
  done < "$STATUS_BODY"
fi
printf '%s\n' "---"
printf '%s\n' "Refresh status | refresh=true $LIT"
exit 0
