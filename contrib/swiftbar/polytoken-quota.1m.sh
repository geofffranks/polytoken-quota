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
#   Doctor findings are parsed regardless of its exit code.
# - A bad explicit override fails visibly; it never silently falls back.

set -u

# --- tunables ---------------------------------------------------------------

CMD_TIMEOUT=10          # seconds per subprocess (SIGTERM; SIGKILL 1s later)
JQ_TIMEOUT=5            # seconds per jq pass
OUTPUT_LIMIT=262144     # bytes of subprocess stdout kept (enforced during capture)
BUDGET_SECONDS=50       # outer refresh budget; later stages skip when exhausted
BAR_WIDTH=20            # characters in each quota window bar
PCT_DIFF_LIMIT=5.0      # percentage points marking reported-vs-derived disagreement

LIT="emojize=false symbolize=false length=85"

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
COMPAT=0; STPROBLEM=0; STTOPERR=0; STERRS=0; STPENDING=0; STINVALID=0
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
# Body: complete menu lines; every data string passes through `safe`.

JQ_HELPERS='
def isnum($x): ($x|type)=="number" and (($x|isnan)|not) and (($x|isinfinite)|not);
def safe: if . == null then "" else (tostring
  | gsub("[\\x00-\\x1f\\x7f\\x{0085}\\x{2028}\\x{2029}|]"; " ")
  | gsub("^ +| +$"; "")
  | .[0:240]) end;
def s2: if . == null then "unknown" else safe end;
# Every informational data line carries the SwiftBar length cap: first-party
# source (MenuBarItem.patchMenuItem) shows a set length shorter than the
# title truncates the visible text and moves the full text into the tooltip.
def lit: " | emojize=false symbolize=false length=85";
# Label a supplied RFC3339 timestamp with its own zone: a trailing Z is UTC;
# any explicit offset (e.g. -04:00) is already shown by the value itself, so
# no annotation is added.
def tz($ts): ($ts|tostring) as $s | if ($s|endswith("Z") or ($s|endswith("z"))) then " (UTC)" else "" end;
def fmt2($n): ((($n*100)|round)/100) | tostring;
def fmtsig($s):
  if isnum($s) then
    if $s == 0 then "0"
    elif (($s|fabs) < 0.005) then (if $s > 0 then "~+0" else "~-0" end)
    else (if $s > 0 then "+" else "-" end) + fmt2($s|fabs)
    end
  else "unknown" end;
def pace_meaning($s):
  if $s == 0 then "on pace (projection)"
  elif $s > 0 then "unused quota accumulating toward reset (projection)"
  else "usage ahead of pace (projection)" end;
def bar($pct):
  (if $pct < 0 then 0 elif $pct > 100 then 100 else $pct end) as $v
  | ((($v/100)*'$BAR_WIDTH')|round) as $n
  | ((("#"*$n) + ("."*('$BAR_WIDTH'-$n))));
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
  ([$r.providers[]? | select((.freshness|type) == "string")] | length) as $freshrows
  | (($r.providers|length) > 0 and $freshrows == 0)
    or ($freshrows == 0 and ($r.providers|length) == 0
        and (($r.as_of // "") == "") and (($r.error // "") == ""));
def candidates($ps; $compat):
  [$ps[]?
   | select(.status == "available"
            and .availability == "available"
            and .eligible == true
            and isnum(.signal)
            and .freshness == "fresh"
            and row_valid($compat))]
  | sort_by([(-.signal), .rank, .provider]);
def red_rule($ps; $compat):
  (([$ps[]? | select(row_valid($compat) and .status != "disabled")] | length) > 0)
  and (([$ps[]?
         | select(row_valid($compat)
                  and .status != "disabled"
                  and (.freshness != "fresh" or .availability != "unavailable"))]
        | length) == 0)
  and (([$ps[]? | select(row_valid($compat)|not)] | length) == 0);
def kind($c):
  if $c == "target-pending" or $c == "quota-reconcile-pending" then "reconciliation/pending"
  elif $c == "journal-incomplete" then "journal/publication"
  elif $c == "state-unreadable" or $c == "orphaned-provider-state" then "persisted state"
  elif $c == "policy-schema" or $c == "legacy-config-keys" or $c == "legacy-quota-adapter" or $c == "quota-gate-suspended" then "policy/config"
  elif ($c|startswith("quota-")) then "quota evidence"
  else "finding" end;
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
    ($r.providers // []) as $ps
    | compat_mode($r) as $compat
    | ([$ps[]? | select(row_valid($compat)|not)] | length) as $invrows
    | (candidates($ps; $compat)) as $cands
    | (red_rule($ps; $compat)) as $isred
    | (if ($r.as_of|s2) != "unknown" then "Signals as of: \(($r.as_of|safe))\(tz($r.as_of))\(lit)" else empty end),
      "---",
      (if ($cands|length) > 0 then
         $cands[0] as $b
         | "Best available pace: \(fmtsig($b.signal)) — provider \(($b.provider|safe))\(lit)",
           "--Pace meaning: \(pace_meaning($b.signal))\(lit)",
           "--Pace basis: fresh; available; eligible\(lit)",
           "--Pace tie-break: deterministic, by signal then rank then name\(lit)",
           "--Pace scope: use-it-or-lose-it projection from saved quota evidence at as_of\(lit)",
           "--Pace limits: not live traffic, fleet health, or a token balance guarantee\(lit)"
       else
         ([$ps[]? | select(.status == "gated")] | length) as $g
         | ([$ps[]? | select(.status == "disabled")] | length) as $d
         | ([$ps[]? | select(.status == "unavailable")] | length) as $u
         | ([$ps[]? | select(.freshness == "stale")] | length) as $stale
         | ([$ps[]? | select(.freshness == "missing")] | length) as $miss
         | ([$ps[]? | select(isnum(.signal)|not)] | length) as $nosig
         | "Best available pace: unavailable\(lit)",
           "--Pace unavailable detail: no fresh, available, eligible provider\(lit)",
           "--Pace unavailable detail: no provider with a computable signal (counts below)\(lit)",
           "--Pace unavailable counts: gated \($g); disabled \($d)\(lit)",
           "--Pace unavailable counts: unavailable \($u); stale \($stale); never observed \($miss)\(lit)",
           "--Pace unavailable counts: no signal \($nosig); invalid rows \($invrows)\(lit)"
       end),
      (if $isred then
         "Observed unavailability: observed state, not necessarily exhaustion\(lit)",
         "--Unavailability detail: every quota-observed, non-disabled provider is fresh\(lit)"
       else empty end),
      (if $compat then
         "Note: connected CLI lacks signal/freshness fields — pace unavailable\(lit)"
       else empty end),
      (if $invrows > 0 then
         "Row validity: \($invrows) invalid row(s) excluded from pace\(lit)",
         "--Validity detail: invalid or contradictory rows are excluded from pace\(lit)",
         "--Validity detail: and from unavailability decisions; valid details still show.\(lit)"
       else empty end),
      "---",
      "Providers: \($ps|length)\(lit)",
      (if (($r.last_checked|s2) != "unknown") then
         "--Newest observation: \(($r.last_checked|safe))\(lit)",
         "--Newest observation: newest across providers, not proof every provider is fresh.\(lit)"
       else empty end),
      (if ($ps|length) == 0 then "No providers are configured or projected.\(lit)" else empty end),
      ($ps[]? | . as $p
       | "--Provider: \(($p.provider|safe))\(lit)",
         "----State: \(($p.status|s2))\(if (($p.reason // "")|safe) != "" then " — \(($p.reason|safe))" else "" end)\(lit)",
         (if row_valid($compat)|not then
            "----Validity: invalid row (\([row_problems($compat)[]|safe]|join("; "))) — excluded from pace and unavailability decisions\(lit)"
          else empty end),
         "----Availability: \(($p.availability|s2))\(lit)",
         "----Availability note: snapshot availability, distinct from routing eligibility\(lit)",
         (if (($p.condition // "")|safe) != "" then "----Condition: \(($p.condition|safe))\(lit)" else empty end),
         "----Checked: \(($p.checked_at|s2)) — freshness \(($p.freshness|s2))\(lit)",
         "----Next reset: \(($p.next_reset_at|s2))\(tz($p.next_reset_at))\(lit)",
         "----Rank: \(($p.rank|s2)) — off-peak: \(if $p.off_peak == true then "yes" elif $p.off_peak == false then "no" else "unknown" end)\(lit)",
         "----Routing eligibility: \(if $p.eligible == true then "eligible" elif $p.eligible == false then "not eligible" else "unknown" end)\(lit)",
         (if isnum($p.signal) then
            "----Pace signal: \(fmtsig($p.signal)) (freshness: \(($p.freshness|safe)))\(lit)",
            "----Pace signal meaning: \(pace_meaning($p.signal))\(lit)",
            (if $p.status == "gated" then "----Pace signal note: computed despite the quota gate; the gate holds routing off\(lit)"
             elif $p.status == "disabled" then "----Pace signal note: provider manually disabled; shown for information only\(lit)"
             else empty end)
          else
            "----Pace signal: unavailable\(lit)",
            "----Pace signal note: not computable from saved evidence; a real zero is shown as 0\(lit)"
          end),
         (($p.windows // [])[]? | . as $w
          | window_pct($w) as $wp
          | (if $wp.pct == null then "unknown" else "\($wp.pct|tostring)%" end) as $vispct
          | (if $wp.pct != null and $wp.pct > 100 then " (over limit; raw value retained)" else "" end) as $over
          | (if $wp.bad then " (invalid supplied numbers ignored)" else "" end) as $badnote
          | (if isnum($w.usage_percent) and isnum($w.used) and isnum($w.limit) and $w.limit > 0 and $w.used >= 0
               and ((($w.usage_percent) - (($w.used/$w.limit)*100))|fabs) > '$PCT_DIFF_LIMIT'
             then " (reported percent disagrees with used/limit)" else "" end) as $disagree
          | "----Window: \(($w.name|s2)) [\(if $wp.pct == null then ("unknown" + (" "*('$BAR_WIDTH'-7))) else bar($wp.pct) end)] \($vispct)\($over)\($badnote)\(lit)",
            "------Window name: \(($w.name|s2))\(lit)",
            "------Used: \(if isnum($w.used) then ($w.used|tostring) elif $w.used == null then "unknown" else "\(($w.used|safe)) (non-numeric)" end)\(lit)",
            "------Limit: \(if isnum($w.limit) then ($w.limit|tostring) elif $w.limit == null then "unknown" else "\(($w.limit|safe)) (non-numeric)" end)\(lit)",
            "------Usage: \(if isnum($w.usage_percent) then "\($w.usage_percent|tostring)% (reported)" elif $wp.src == "derived from used/limit" then "\($wp.pct|tostring)% (derived from used/limit)" else "unknown" end)\($disagree)\(lit)",
            (if ($w.reset_at|s2) != "unknown" then "------Resets: \(($w.reset_at|safe))\(tz($w.reset_at))\(lit)" else "------Resets: unknown\(lit)" end))),
      "---",
      (if ($r.provider_only == true) then
         "Routing: provider-only mode — routes are not applicable in this mode.\(lit)"
       elif $r.routing_enabled == true then "Routing: enabled\(lit)"
       elif $r.routing_enabled == false then "Routing: disabled\(lit)"
       else "Routing: unknown\(lit)" end),
      (($r.routes // [])[]? | . as $rt
       | "--Route: \(($rt.name|safe))\(lit)",
         (if ($rt.target_id|s2) != "" then "----Target: \(($rt.target_id|safe))\(lit)" else empty end),
         (if ($rt.source_path|s2) != "" then "----Source: \(($rt.source_path|safe))\(lit)" else empty end),
         "----Desired: \(if (($rt.desired // [])|length) > 0 then [($rt.desired[]?)|safe]|join(", ") else "(none)" end)\(lit)",
         "----Effective: \(if (($rt.effective // [])|length) > 0 then [($rt.effective[]?)|safe]|join(", ") else "(none)" end)\(lit)",
         (($rt.skipped // [])[]? | "----Skipped: \((.model|safe)) — \((.reason|safe))\(lit)"),
         "----Projection error: \(if $rt.projection_error == true then "yes" else "no" end)\(lit)"),
      (if (($r.pending_targets // [])|length) > 0 then
         "Pending: \(($r.pending_targets|length)) outstanding target(s)\(lit)",
         (($r.pending_targets // [])[]? | "Pending target: \(safe)\(lit)"),
         "--Pending detail: outstanding reconciler work.\(lit)",
         "--Pending detail: reported routing may not yet be applied.\(lit)"
       else empty end),
      (if (($r.errors // [])|length) > 0 then
         "Errors: \(($r.errors|length)) reported\(lit)",
         "--Error scopes: provider = provider/quota projection; route = route projection\(lit)",
         (($r.errors // [])[]? | . as $e
          | "--Error: \(($e.scope|s2))\(if (($e.target_id // "")|safe) != "" then " — target \(($e.target_id|safe))" else "" end)\(lit)",
            (if ($e.mapping_id|s2) != "" then "----Mapping: \(($e.mapping_id|safe))\(lit)" else empty end),
            (if ($e.source_path|s2) != "" then "----File: \(($e.source_path|safe))\(lit)" else empty end),
            "----Summary: \(($e.summary|safe))\(lit)")
       else empty end)
  end
'

JQ_DOCTOR_BODY="$JQ_HELPERS"'
. as $r
| if ($r|type) != "object" or ($r.findings|type) != "array" then "Diagnostics output has an unexpected shape.\(lit)"
  else
    "Doctor as of: \(($r.as_of|s2))\(tz($r.as_of)) — actionable: \(if $r.actionable == true then "yes" else "no" end)\(lit)",
    ([$r.findings[]?]) as $fs
    | (if ($fs|length) == 0 then "Findings: none reported\(lit)" else "Findings: \(($fs|length))\(lit)" end),
    ($fs[]? | . as $f
     | "--Finding: \(($f.code|s2)) [severity: \(($f.severity|s2))] — kind: \(kind($f.code))\(lit)",
       "----Message: \(($f.message|safe))\(lit)",
       (if (($f.target_id // "")|safe) != "" then "----Target: \(($f.target_id|safe))\(lit)" else empty end),
       (if (($f.file // "")|safe) != "" then "----File: \(($f.file|safe))\(lit)" else empty end),
       (if (($f.chain // "")|safe) != "" then "----Chain: \(($f.chain|safe))\(lit)" else empty end),
       (if (($f.remediation // "")|safe) != "" then "----Remediation (informational; the plugin performs no actions): \(($f.remediation|safe))\(lit)" else empty end)),
    (($r.recovered // []) as $rec
     | if ($rec|length) > 0 then
         "Recovered: \($rec|length)\(lit)",
         ($rec[]? | "--Recovered: \((.target_id|safe)) — stage \((.stage|safe))\(lit)",
                    "----Summary: \((.summary|safe))\(lit)")
       else empty end),
    "--Note: doctor reports persisted/reported findings.\(lit)",
    "--Note: not a complete historical error log or an exact unapplied field diff.\(lit)"
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
  printf '%s\n' "Quota :exclamationmark.triangle: | color=orange"
  printf '%s\n' "---"
  printf '%s | %s\n' "Plugin configuration error — override ignored, no fallback" "$LIT"
  printf '%s | %s\n' "Reason: $CONFIG_ERROR" "$LIT"
  printf '%s | %s\n' "Fix: correct $CONFIG_NAME (keys: quota_bin, polytoken_bin, jq_bin) or remove it" "$LIT"
  printf '%s\n' "Refresh status | refresh=true"
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
  2) warn "status $(rc_text 2)" ;;
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
      [ "$COMPAT" = 1 ] && warn "older polytoken-quota CLI: no signal/freshness fields — pace unavailable"
      [ "$STPROBLEM" = 1 ] && warn "status reported quota problems"
      [ "$STTOPERR" = 1 ] && warn "status reported an error"
      [ "$STERRS" -gt 0 ] && warn "status reported $STERRS diagnostic error(s)"
      [ "$STPENDING" -gt 0 ] && warn "$STPENDING pending reconciler target(s)"
      [ "$STINVALID" -gt 0 ] && warn "status contained $STINVALID invalid or contradictory provider row(s)"
      if [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
        run_bounded "$JQ_TIMEOUT" "$STATUS_BODY" "$JQ_BIN" -r "$JQ_STATUS_BODY" "$STATUS_FILE" 2>/dev/null
        jqrc=$?
        if [ "$jqrc" -eq 0 ]; then
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
DOCTOR_BODY="$TMPDIR_LOCAL/doctor.body"

if [ "$DOCTOR_RC" != "skipped" ] && [ "$DOCTOR_RC" != nobudget ] && [ "$DOCTOR_RC" != 125 ] && [ -n "$JQ_BIN" ] && [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
  run_bounded "$JQ_TIMEOUT" "$DOCTOR_DIGEST" "$JQ_BIN" -r '
      . as $r
      | if ($r|type) != "object" or ($r.findings|type) != "array" then "shape=bad"
        else "find=\([$r.findings[]?] | length)",
             "act=\([$r.findings[]? | select(.severity == "warning" or .severity == "error")] | length)",
             "rec=\([$r.recovered[]?] | length)",
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
      [ "$D_ACT" -gt 0 ] && warn "doctor reported $D_ACT actionable finding(s)"
      if [ "$(budget_remaining)" -ge $((JQ_TIMEOUT + 2)) ]; then
        run_bounded "$JQ_TIMEOUT" "$DOCTOR_BODY" "$JQ_BIN" -r "$JQ_DOCTOR_BODY" "$DOCTOR_FILE" 2>/dev/null
        jqrc=$?
        if [ "$jqrc" -eq 0 ]; then
          DOCTOR_PARSED=1
        elif [ "$jqrc" -eq 125 ]; then
          warn "jq capture setup failed — diagnostics details omitted"
        else
          warn "doctor output could not be rendered"
        fi
      else
        warn "refresh budget exhausted; diagnostics details omitted"
      fi
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
  HEADER="Quota :exclamationmark.triangle: | color=orange"
elif [ "$REDRULE" = 1 ]; then
  HEADER="Quota :xmark.circle: | color=red"
elif [ "$CAND" = 1 ] && [ -n "$SIG" ]; then
  case "$DIRV" in
    up) HEADER="Quota $SIG :arrow.up: | color=green" ;;
    down) HEADER="Quota $SIG :arrow.down:" ;;
    right) HEADER="Quota $SIG :arrow.right:" ;;
    *) HEADER="Quota :questionmark.circle:" ;;
  esac
else
  HEADER="Quota :questionmark.circle:"
fi

# --- assemble menu --------------------------------------------------------------------

printf '%s\n' "$HEADER"
printf '%s\n' "---"

if [ -n "$WARN_REASONS" ]; then
  printf '%s\n' "$WARN_REASONS" | while IFS= read -r wr; do
    [ -n "$wr" ] && printf '%s | %s\n' "WARNING: $wr" "$LIT"
  done
  printf '%s | %s\n' "--Note: pace details remain below when the underlying read succeeded." "$LIT"
  printf '%s\n' "---"
fi

if [ "$STATUS_PARSED" = 1 ]; then
  cat "$STATUS_BODY"
  printf '%s\n' "---"
elif [ "$STATUS_RC" = "skipped" ]; then
  printf '%s | %s\n' "Best available pace: unavailable — status was not run (missing dependency)" "$LIT"
  printf '%s | %s\n' "Quota status: unavailable — fix the missing dependency and refresh" "$LIT"
  printf '%s\n' "---"
else
  printf '%s | %s\n' "Best available pace: unavailable — status $(rc_text "$STATUS_RC") with unusable output" "$LIT"
  printf '%s | %s\n' "Quota status: unavailable — no provider details could be read" "$LIT"
  printf '%s\n' "---"
fi

if [ "$DOCTOR_OK" = 1 ]; then
  if [ "$DOCTOR_PARSED" = 1 ]; then
    printf '%s | %s\n' "Diagnostics: $D_FIND finding(s), $D_ACT actionable, $D_REC recovered" "$LIT"
    printf '%s\n' "---"
    cat "$DOCTOR_BODY"
  else
    printf '%s | %s\n' "Diagnostics: $D_FIND finding(s), $D_ACT actionable — details could not be rendered" "$LIT"
    printf '%s\n' "---"
  fi
else
  printf '%s | %s\n' "Diagnostics: unavailable — doctor $(rc_text "$DOCTOR_RC") with unusable output" "$LIT"
  printf '%s\n' "---"
fi

# Static disclaimers live in one submenu so they survive even when a read
# fails; the menu stays concise without losing the required disclosures.
printf '%s\n' "About this status"
printf '%s | %s\n' "--Note: the menu-bar icon is best available pace." "$LIT"
printf '%s | %s\n' "--Note: it is not fleet health, a token balance, or actual serving." "$LIT"
printf '%s | %s\n' "--Note: status and diagnostics are two independent reads." "$LIT"
printf '%s | %s\n' "--Note: they may observe different state revisions." "$LIT"
printf '%s | %s\n' "--Note: gated is a quota-gate claim on the enabled field, not observed exhaustion." "$LIT"
printf '%s | %s\n' "--Note: pace is a use-it-or-lose-it projection from saved quota evidence." "$LIT"
printf '%s | %s\n' "--Note: it is not live traffic or a guarantee." "$LIT"
printf '%s | %s\n' "--Note: pending means outstanding reconciler work." "$LIT"
printf '%s | %s\n' "--Note: reported routing may not yet be applied." "$LIT"
printf '%s\n' "Refresh status | refresh=true"
exit 0
