# shellcheck shell=bash
# The M6 proofs (decision 0073 block 4): several PCs sharing one memory
# server. Each check is one function, m6_check_<name with _ for ->, run by
# m6_check; it reaches a machine through the driver alone, so the VM bench
# (scripts/vm-bench.sh) and the CI chain run the same proofs.
#
# Sourced, never run. Driver contract (the caller defines):
#   m6_on <a|b|srv> <backsight words…>  that machine's backsight; stdin,
#                                       stdout, stderr and status passed through
#   m6_sh <a|b|srv> <sh command>        an sh command there, stdin passed through
#   m6_put <who> <path>                 stdin to that file there, mode 0600
#   m6_get <who> <path>                 that file's bytes
#   m6_home <who>                       the home there (absolute)
#   M6_RESULTS                          the JSONL file the results go to
#   M6_WORK                             a private scratch directory here
#   M6_VERSION, M6_COMMIT               what the results are of
#   M6_ABORT (optional)                 a file: once it holds a line (the
#                                       run's watchdog), the check running
#                                       FAILs with it, the rest NOT RUN
#   fail <message>                      stops the caller
# A is a PC with Claude Code (the bench's bs-m6-client), B the other one
# (the bench's host client), srv the server's host. The checks of the VM
# bench alone (ssh-*, login-shells, timers) also need:
#   M6_SERVER_IP, M6_ROOT (this checkout)
#   m6_link a <up|down>                 A's network link
#   m6_host_key <new|old>               the server's ed25519 host key, and the
#                                       driver's own trust of it
#
# Secrets: a key or token is never in an argv, a trace or a result. B's
# API key is read in a process substitution (m6_auth, curl -K). Every
# result's detail and values, and every fail message, are scrubbed
# (m6_scrub): connection tokens (bst1.…), and the API keys the run has
# learnt (m6_learn_key: the server's, before and after a rotation, and each
# client's), read from a 0600 file, never an argv.

# shellcheck source=scripts/proof-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/proof-lib.sh"

M6_STOPPED=

# m6_q <word…>: the words quoted for sh, space separated.
m6_q() {
  local w out='' sq="'\\''"
  for w in "$@"; do out+="${out:+ }'${w//\'/$sq}'"; done
  printf '%s' "$out"
}

# m6_keys: the file of the API keys this run knows, one a line (0600,
# under M6_WORK); empty when M6_WORK is not set.
m6_keys() { printf '%s' "${M6_WORK:+$M6_WORK/known-keys}"; }

# m6_learn_key <who>: adds that machine's API key (api_key in its
# secrets.toml), when it has one, to the keys m6_scrub masks. The key goes
# from the file there to the keys file through pipes, with xtrace off:
# never an argv, a trace or the terminal. Best effort: a machine with no
# backsight or no key adds nothing, and never fails the caller.
m6_learn_key() {
  local f out sec
  f=$(m6_keys)
  [ -n "$f" ] || return 0
  out=$(m6_on "$1" paths --json < /dev/null 2> /dev/null) || return 0
  sec=$(jq -r '.data.secrets_file // empty' <<<"$out" 2> /dev/null) || return 0
  [ -n "$sec" ] || return 0
  (
    { set +x; } 2> /dev/null
    umask 077
    m6_get "$1" "$sec" 2> /dev/null | m6_toml '' api_key | grep -E '^.{16,}$' >> "$f" || true
    [ ! -s "$f" ] || sort -u -o "$f" "$f"
  ) || true
}

# m6_scrub: stdin with every connection token (bst1.…) masked as [token],
# and every key m6_learn_key learnt (16 bytes or more) as [key].
m6_scrub() {
  local keys
  keys=$(m6_keys)
  if [ -n "$keys" ] && [ -s "$keys" ]; then
    sed -E 's/bst1\.[A-Za-z0-9_.=-]*/[token]/g' | awk '
      NR == FNR { if (length($0) >= 16) k[++n] = $0; next }
      { for (i = 1; i <= n; i++) while ((p = index($0, k[i])) > 0) $0 = substr($0, 1, p - 1) "[key]" substr($0, p + length(k[i])); print }' "$keys" -
  else
    sed -E 's/bst1\.[A-Za-z0-9_.=-]*/[token]/g'
  fi
}

# m6_secret_hits: how many lines of stdin hold the mock LLM key, a token
# (bst1.) or a key m6_learn_key learnt (read from its file, never an argv).
m6_secret_hits() {
  local keys
  keys=$(m6_keys)
  if [ -n "$keys" ] && [ -s "$keys" ]; then
    grep -c -F -e mock-not-a-key -e bst1. -f "$keys" || true
  else
    grep -c -F -e mock-not-a-key -e bst1. || true
  fi
}

# m6_result <check> <PASS|FAIL|RECORDED|NOT RUN> <detail> [values-json]:
# one line `bs-m6 <check>: <STATUS> <detail>` and one JSONL record. A key
# named for a secret (api_key, *_token, secret, password, *_key) is dropped
# from values (a count such as tokens or max_tokens stays), and a string
# holding a token or a known key masked (m6_scrub).
m6_result() {
  local check=$1 status=$2 detail values=${4:-'{}'} rec keys
  detail=$(printf '%s' "$3" | m6_scrub)
  keys=$(m6_keys)
  [ -n "$keys" ] && [ -s "$keys" ] || keys=/dev/null
  rec=$(jq -cn --arg check "$check" --arg status "$status" --arg detail "$detail" --argjson values "$values" --rawfile keys "$keys" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg version "${M6_VERSION:-}" --arg commit "${M6_COMMIT:-}" '
    ($keys | split("\n") | map(select(length >= 16))) as $ks |
    def scrub: walk(if type == "object" then with_entries(select(.key | test("(^|_)token$|api_key|secret|password|(^|_)key$"; "i") | not))
      elif type == "string" then gsub("bst1\\.[A-Za-z0-9_.=-]*"; "[token]") | reduce $ks[] as $k (.; split($k) | join("[key]")) else . end);
    {check: $check, status: $status, detail: $detail, values: ($values | scrub), at: $at, version: $version, commit: $commit}') ||
    fail "the result of $check has values that are not JSON"
  printf 'bs-m6 %s: %s %s\n' "$check" "$status" "$detail"
  printf '%s\n' "$rec" >> "$M6_RESULTS"
}

# m6_results_of <check>: how many records M6_RESULTS holds for it.
m6_results_of() {
  [ -s "$M6_RESULTS" ] || { echo 0; return; }
  jq -s --arg c "$1" '[.[] | select(.check == $c)] | length' "$M6_RESULTS"
}

# m6_check <name> <function>: runs the function in a subshell under set -e
# (inherited by command substitutions), where fail ends it; a fail (or any
# non-zero exit) is the check's FAIL with the message, and every check
# after it is NOT RUN (later checks build on the state earlier ones
# left: M6_STOPPED names the failed one). A function must record its own
# PASS or RECORDED: one that records nothing fails. Never call m6_check
# in a condition (if, &&, ||): bash turns set -e off there, even in a
# subshell, and a failing step would go on; it refuses.
m6_check() {
  local name=$1 fn=$2 rc=0 errexit=0 before probe
  if [ -n "$M6_STOPPED" ]; then
    m6_result "$name" "NOT RUN" "after the FAIL of $M6_STOPPED"
    return 0
  fi
  if [ -n "${M6_ABORT:-}" ] && [ -s "$M6_ABORT" ]; then
    M6_STOPPED=$name
    m6_result "$name" FAIL "$(head -n 1 "$M6_ABORT")"
    return 0
  fi
  case $- in *e*) errexit=1 ;; esac
  set +e
  probe=$(
    set -e
    false
    echo on
  )
  if [ "$errexit" = 1 ]; then set -e; fi
  [ -z "$probe" ] || fail "m6_check $name was called in a condition (if, &&, ||), where set -e is off: run it as a command of its own"
  M6_FAIL_FILE=$M6_WORK/fail-$name
  rm -f "$M6_FAIL_FILE"
  before=$(m6_results_of "$name")
  set +e
  (
    set -e
    shopt -s inherit_errexit
    fail() {
      local msg
      msg=$(printf '%s' "$*" | m6_scrub)
      printf '%s\n' "$msg" >> "$M6_FAIL_FILE"
      printf 'bs-m6 %s: %s\n' "$name" "$msg" >&2
      exit 1
    }
    "$fn"
  )
  rc=$?
  if [ "$errexit" = 1 ]; then set -e; fi
  # Stopped by the run's watchdog: its reason first.
  if [ -n "${M6_ABORT:-}" ] && [ -s "$M6_ABORT" ]; then
    { head -n 1 "$M6_ABORT"; cat "$M6_FAIL_FILE" 2> /dev/null || true; } > "$M6_FAIL_FILE.abort"
    mv "$M6_FAIL_FILE.abort" "$M6_FAIL_FILE"
  fi
  # A fail its caller swallowed (x=$(…) || true) still fails the check.
  if [ "$rc" = 0 ] && [ ! -s "$M6_FAIL_FILE" ] && [ "$(m6_results_of "$name")" -gt "$before" ]; then
    return 0
  fi
  M6_STOPPED=$name
  if [ -s "$M6_FAIL_FILE" ]; then
    m6_result "$name" FAIL "$(awk 'NR > 1 { printf "; " } { printf "%s", $0 }' "$M6_FAIL_FILE")"
  elif [ "$rc" = 0 ]; then
    m6_result "$name" FAIL "the check recorded no result"
  else
    m6_result "$name" FAIL "exited $rc without a message"
  fi
}

# m6_json <what> <text>: the text when it is one JSON value, else fail.
m6_json() {
  jq -e . > /dev/null 2>&1 <<<"$2" || fail "$1 answered no JSON: $(printf '%s' "$2" | tail -n 3 | m6_scrub)"
  printf '%s\n' "$2"
}

# m6_paths <who>: backsight paths --json's data there.
m6_paths() {
  local out
  out=$(m6_on "$1" paths --json < /dev/null) || fail "backsight paths on $1 failed"
  m6_json "paths on $1" "$out" | jq -c .data
}

# m6_toml <section> <key>: a string value of stdin's TOML ("" for the top level).
m6_toml() {
  awk -v want="$1" -v key="$2" '
    /^[[:space:]]*\[/ { sec = $0; gsub(/^[[:space:]]*\[|\][[:space:]]*$/, "", sec); next }
    sec == want && $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      v = $0; sub(/^[^=]*=[[:space:]]*/, "", v); sub(/[[:space:]]*(#.*)?$/, "", v)
      gsub(/^["\047]|["\047]$/, "", v); print v; exit }'
}

# m6_server: "<url> <bank>" of B's config, the server the documents are read on.
m6_server() {
  local cfg url bank
  cfg=$(m6_paths b | jq -r .config_file)
  cfg=$(m6_get b "$cfg") || fail "cannot read B's config"
  url=$(m6_toml server url <<<"$cfg")
  bank=$(m6_toml server bank <<<"$cfg")
  [ -n "$url" ] || fail "B's config names no server url"
  printf '%s %s\n' "${url%/}" "${bank:-claude-memory}"
}

# m6_auth: a curl config holding B's key, for `curl -K <(m6_auth)`: the
# key never sits in a traced argv. Only in a process substitution (it
# turns xtrace off for good in the shell it runs in).
m6_auth() {
  { set +x; } 2> /dev/null
  local sec
  sec=$(m6_paths b | jq -r .secrets_file)
  printf 'header = "Authorization: Bearer %s"\n' "$(m6_get b "$sec" | m6_toml '' api_key)"
}

# m6_urlencode <s>: s with every byte but [A-Za-z0-9._~-] %-encoded.
m6_urlencode() { jq -rn --arg s "$1" '$s | @uri'; }

# m6_doc <docid> [bank suffix]: the document's JSON on B's server with B's
# key, in B's bank (or the bank of that name plus the suffix: -control,
# the control bank); empty on 404. Any other answer (401, 5xx, no server)
# fails: it never reads as "absent".
m6_doc() {
  local out code server
  server=$(m6_server) || exit 1
  out=$(mktemp "$M6_WORK/doc.XXXXXX")
  code=$(pl_untraced curl -sS -o "$out" -w '%{http_code}' -K <(m6_auth) \
    "${server% *}/v1/default/banks/${server#* }${2:-}/documents/$(m6_urlencode "$1")") || code=000
  case $code in
    200) cat "$out" ;;
    404) ;;
    *)
      rm -f "$out"
      fail "reading document $1: HTTP $code"
      ;;
  esac
  rm -f "$out"
}

# m6_facts <docid>: how many facts (memory units but observations) the
# server holds from that document, read with B's key. Any answer but 200,
# a page that does not hold them all, or an item of another document,
# fails: it never reads as fewer, nor as more.
m6_facts() {
  local out code server
  server=$(m6_server) || exit 1
  out=$(mktemp "$M6_WORK/facts.XXXXXX")
  code=$(pl_untraced curl -sS -o "$out" -w '%{http_code}' -K <(m6_auth) \
    "${server% *}/v1/default/banks/${server#* }/memories/list?document_id=$(m6_urlencode "$1")&limit=500") || code=000
  if [ "$code" != 200 ]; then
    rm -f "$out"
    fail "reading the memories of $1: HTTP $code"
  fi
  jq -e '(.items | length) == .total' > /dev/null < "$out" || {
    code=$(jq -r '"\(.items | length) of \(.total)"' < "$out")
    rm -f "$out"
    fail "the memories of $1: $code read"
  }
  # A server that ignored the filter would count another document's.
  code=$(jq -r --arg d "$1" '[.items[] | select(.document_id != $d) | .document_id // "none"] | unique | join(", ")' < "$out")
  if [ -n "$code" ]; then
    rm -f "$out"
    fail "the memories listed for $1 include another document's ($code)"
  fi
  jq '[.items[] | select(.fact_type != "observation")] | length' < "$out"
  rm -f "$out"
}

# m6_uuid_counts <sid>: "<uuid> <n>" for every [rec <uuid>] in the
# original_text of conversation:<sid> (every segment appended to it), one
# a line, sorted. Fails when the document is absent or has no
# original_text: an empty count must never read as "none sent twice".
m6_uuid_counts() {
  local doc
  doc=$(m6_doc "conversation:$1") || exit 1
  [ -n "$doc" ] || fail "conversation:$1 is not on the server"
  jq -e '.original_text | type == "string" and length > 0' > /dev/null <<<"$doc" ||
    fail "conversation:$1 has no original_text"
  jq -r .original_text <<<"$doc" | grep -o '\[rec [^]]*\]' | sed 's/^\[rec \(.*\)\]$/\1/' | sort | uniq -c | awk '{ print $2, $1 }'
}

# m6_queue_wait: waits until B's server has no pending or processing
# operation (async retains): what is read before may lag.
m6_queue_wait() {
  local server n total status _
  server=$(m6_server) || exit 1
  for _ in $(seq 1 100); do
    n=0
    for status in pending processing; do
      total=$(pl_untraced curl -fsS -K <(m6_auth) "${server% *}/v1/default/banks/${server#* }/operations?status=$status&limit=1" | jq -er .total) ||
        fail "cannot read the bank's $status operations"
      n=$((n + total))
    done
    [ "$n" -eq 0 ] && return 0
    sleep 3
  done
  fail "the bank's queue never drained ($n operation(s) left)"
}

# m6_tick <who> [partial]: `jobs tick --json` there, its envelope on
# stdout. A skipped tick (another one running, the scheduler's; a
# maintenance) is no tick: it is tried again every M6_TICK_EVERY s (5) for
# M6_TICK_WAIT s (120), then fails. A tick that did not succeed (ok not
# true) fails, but with partial a partial one (data.status partial: the
# server refused a send) is taken.
m6_tick() {
  local who=$1 mode=${2:-} out rc deadline=$((SECONDS + ${M6_TICK_WAIT:-120}))
  while :; do
    rc=0
    out=$(m6_on "$who" jobs tick --json < /dev/null) || rc=$?
    out=$(m6_json "jobs tick on $who (exit $rc)" "$out") || exit 1
    if [ "$mode" = partial ] && [ "$(jq -r '.data.status // ""' <<<"$out")" = partial ]; then
      printf '%s\n' "$out"
      return 0
    fi
    jq -e '.ok == true' > /dev/null <<<"$out" ||
      fail "jobs tick on $who did not succeed (exit $rc): $(jq -r '.error.message // .data.status // "no message"' <<<"$out" | m6_scrub)"
    if [ "$(jq -r '.data.status // ""' <<<"$out")" != skipped ]; then
      printf '%s\n' "$out"
      return 0
    fi
    [ "$SECONDS" -lt "$deadline" ] || fail "jobs tick on $who: still skipped ($(jq -r '.data.reason // ""' <<<"$out")) after ${M6_TICK_WAIT:-120} s"
    sleep "${M6_TICK_EVERY:-5}"
  done
}

# m6_flush <who> <sid>: `retain flush --json --session <sid>` there, its
# envelope on stdout.
m6_flush() {
  local out rc=0
  out=$(m6_on "$1" retain flush --json --session "$2" < /dev/null) || rc=$?
  m6_json "retain flush of $2 on $1 (exit $rc)" "$out"
}

# m6_cursor <who> <sid>: the session's cursor there (JSON), empty when it
# has none.
m6_cursor() {
  local c out
  c=$(m6_paths "$1" | jq -r .state)/cursors/$2.json
  out=$(m6_sh "$1" "if [ -e $(m6_q "$c") ]; then cat $(m6_q "$c"); fi") || fail "cannot read $1's cursor of $2"
  # Without its pending first-segment checks: a restore's reconcile drops
  # them without sending anything (as restore-chain.sh's rc_cursor).
  [ -z "$out" ] || jq -cS 'del(.replaces)' <<<"$out"
}

# m6_flush_sent <who> <sid> <uuid>: flushes the session there until its
# records up to <uuid> are out, and says how: sent (this flush sent
# them) or tick (a jobs tick, A's timer, sent them first: the flush finds
# nothing new and the cursor already ends at <uuid>; only the cursor
# proves it, never a flush that sent nothing alone). Another flusher
# holding the session (locked) is waited for as m6_tick waits; any other
# status fails.
m6_flush_sent() {
  local who=$1 sid=$2 want=$3 out st last deadline=$((SECONDS + ${M6_TICK_WAIT:-120}))
  while :; do
    out=$(m6_flush "$who" "$sid") || exit 1
    st=$(jq -r --arg s "$sid" '[.data.results[]? | select(.session == $s) | .status][0] // "nothing"' <<<"$out")
    case $st in
      sent)
        echo sent
        return 0
        ;;
      locked) [ "$SECONDS" -lt "$deadline" ] || fail "the flush of $sid on $who: still locked after ${M6_TICK_WAIT:-120} s" ;;
      nothing)
        last=$(m6_cursor "$who" "$sid" | jq -r '.last_uuid // empty')
        [ "$last" = "$want" ] || fail "the flush of $sid on $who sent nothing, and its cursor ends at ${last:-none}, not $want"
        echo tick
        return 0
        ;;
      *) fail "the flush of $sid on $who: $st (exit $(jq -r '.error.code // 0' <<<"$out")): $(jq -c --arg s "$sid" '[.data.results[]? | select(.session == $s) | .detail]' <<<"$out" | m6_scrub)" ;;
    esac
    sleep "${M6_TICK_EVERY:-5}"
  done
}

# m6_doctor <who>: `doctor --json` there (non-zero when it finds an error).
m6_doctor() {
  local out rc=0
  out=$(m6_on "$1" doctor --json < /dev/null) || rc=$?
  m6_json "doctor on $1 (exit $rc)" "$out"
}

# m6_doctor_status <who> <check id>: that check's status ("missing" when absent).
m6_doctor_status() {
  local out
  out=$(m6_doctor "$1") || exit 1
  jq -r --arg id "$2" '[.data.checks[]? | select(.id == $id) | .status][0] // "missing"' <<<"$out"
}

# m6_session <who> <sid> <project> <from> <to> [tag] [age_min]: appends
# records <sid>-<tag><from>…<sid>-<tag><to> (tag r) to
# <home>/.claude/projects/-bs-m6-<project>/<sid>.jsonl, in rc_session's
# format, each text carrying [rec <uuid>], the newest age_min (0) minutes
# old and each earlier one a second older, all after the file's records
# already there; the file's mtime is the newest record's time. The
# records' cwd is <home>/work/<project>.
m6_session() {
  local who=$1 sid=$2 project=$3 from=$4 to=$5 tag=${6:-r} age=${7:-0} home dir cwd newest last i ts uuid role content
  home=$(m6_home "$who")
  dir=$home/.claude/projects/-bs-m6-$project cwd=$home/work/$project
  newest=$(($(date +%s) - age * 60))
  # Appended records follow the file's last one, whatever age says: a
  # session continued, here or on the PC it was copied to, goes on after it.
  last=$(m6_sh "$who" "tail -n 1 $(m6_q "$dir/$sid.jsonl") 2> /dev/null || true") || fail "cannot read session $sid on $who"
  if [ -n "$last" ]; then
    last=$(jq -r '.timestamp // empty' <<<"$last") && [ -n "$last" ] && last=$(date -u -d "$last" +%s) ||
      fail "session $sid on $who: its last record has no time this can read"
    [ $((newest - (to - from))) -gt "$last" ] || newest=$((last + 1 + to - from))
  fi
  for i in $(seq "$from" "$to"); do
    ts=$(date -u -d "@$((newest - (to - i)))" +%Y-%m-%dT%H:%M:%S.000Z)
    uuid=$sid-$tag$i
    if [ $((i % 2)) = 0 ]; then
      role=assistant content="[{\"type\":\"text\",\"text\":\"Noted for $sid: the export pages by 200. [rec $uuid]\"}]"
    else
      role=user content="\"$sid turn $i: the export job of the M6 bench failed at page $i. [rec $uuid]\""
    fi
    printf '{"type":"%s","uuid":"%s","sessionId":"%s","cwd":"%s","timestamp":"%s","message":{"role":"%s","content":%s}}\n' \
      "$role" "$uuid" "$sid" "$cwd" "$ts" "$role" "$content"
  done | m6_sh "$who" "mkdir -p $(m6_q "$dir" "$cwd") && cat >> $(m6_q "$dir/$sid.jsonl") && touch -d @$newest $(m6_q "$dir/$sid.jsonl")" ||
    fail "cannot write session $sid on $who"
}

# m6_transcript <who> <sid> <project>: the transcript's path there.
m6_transcript() { printf '%s/.claude/projects/-bs-m6-%s/%s.jsonl\n' "$(m6_home "$1")" "$3" "$2"; }

# m6_copy <from> <to> <sid> <project>: the transcript copied as is (its
# records' cwd too), its mtime kept.
m6_copy() {
  local src dst mtime
  src=$(m6_transcript "$1" "$3" "$4") dst=$(m6_transcript "$2" "$3" "$4")
  mtime=$(m6_sh "$1" "stat -c %Y $(m6_q "$src")") || fail "no transcript $3 on $1"
  m6_get "$1" "$src" | m6_sh "$2" "mkdir -p $(m6_q "${dst%/*}") && cat > $(m6_q "$dst") && touch -d @$mtime $(m6_q "$dst")" ||
    fail "cannot copy session $3 from $1 to $2"
}

# m6_hash <who> <path>: the file's sha256 there, or "absent".
m6_hash() {
  m6_sh "$1" "if [ -e $(m6_q "$2") ]; then sha256sum < $(m6_q "$2") | cut -d' ' -f1; else echo absent; fi" || fail "cannot hash $2 on $1"
}

# m6_wait_doc <sid>: waits (5 min) until conversation:<sid> is on B's
# server with its metadata; prints the document.
m6_wait_doc() {
  local doc _
  for _ in $(seq 1 100); do
    doc=$(m6_doc "conversation:$1") || exit 1
    if [ -n "$doc" ] && jq -e '(.document_metadata.last_uuid // "") != ""' > /dev/null <<<"$doc"; then
      printf '%s\n' "$doc"
      return 0
    fi
    sleep 3
  done
  fail "conversation:$1 never reached the server with its metadata"
}

# m6_live_since <who> <minutes>: that PC has retained live for <minutes>:
# state.json's retain_since moves back to then, never forward, under the
# state lock (the product's own lock file, held by this edit's shell and
# waited for up to M6_LOCK_WAIT tries of 0.1 s). Only jobs tick writes
# retain_since, at its first run, and live retain and verify --live leave
# whatever started before it to backfill: a bench PC installed minutes
# ago would have no settled session for either.
m6_live_since() {
  local who=$1 min=$2 state at
  state=$(m6_paths "$who" | jq -r .state)
  at=$(date -u -d "@$(($(date +%s) - min * 60))" +%Y-%m-%dT%H:%M:%SZ)
  m6_sh "$who" "s=$(m6_q "$state") at=$(m6_q "$at") tries=${M6_LOCK_WAIT:-300}
$(
    cat << 'EOF'
umask 077
n=0
mkdir -p "$s/locks" || exit 1
until (set -C; printf '{"pid":%d,"command":"bs-m6 bench retain_since","since":"%s"}' $$ "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$s/locks/state.lock") 2> /dev/null; do
  n=$((n + 1))
  [ "$n" -lt "$tries" ] || exit 75
  sleep 0.1
done
trap 'rm -f "$s/locks/state.lock"' EXIT
if jq --arg at "$at" 'if (.retain_since // "") == "" or .retain_since > $at then .retain_since = $at else . end' "$s/state.json" > "$s/state.json.bs-m6" &&
  mv "$s/state.json.bs-m6" "$s/state.json"; then exit 0; fi
rm -f "$s/state.json.bs-m6"
exit 1
EOF
  )" || fail "cannot move $who's retain_since back to $at (the state lock stayed held, or state.json did not read)"
}

# m6_scheduler <who>: the scheduler answer (on, off) that machine's last
# setup recorded (setup.json), the one a reconnect keeps: the VM bench's A
# runs its timers, B and the CI chain's homes answered off. None recorded
# fails rather than guessing.
m6_scheduler() {
  local f v
  f=$(m6_paths "$1" | jq -r .state)/setup.json
  v=$(m6_sh "$1" "if [ -e $(m6_q "$f") ]; then cat $(m6_q "$f"); fi" | jq -r '.answers.scheduler // empty' 2> /dev/null || true)
  case $v in
    on | off) printf '%s\n' "$v" ;;
    *) fail "$1's setup recorded no scheduler answer" ;;
  esac
}

# m6_banks_code: the HTTP code of the banks list on B's server with B's key.
m6_banks_code() {
  local server code
  server=$(m6_server) || exit 1
  code=$(pl_untraced curl -sS -o /dev/null -w '%{http_code}' -K <(m6_auth) "${server% *}/v1/default/banks") || code=000
  printf '%s\n' "$code"
}

# m6_cursors <who>: "<sha256>  ./<sid>.json" for every cursor there,
# sorted, each hashed as m6_cursor reads it (without its pending
# first-segment checks, which a tick confirms without sending).
m6_cursors() {
  local c names f
  c=$(m6_paths "$1" | jq -r .state)/cursors
  names=$(m6_sh "$1" "cd $(m6_q "$c") && find . -maxdepth 1 -type f -name '*.json' | sort") || fail "cannot read $1's cursors"
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    printf '%s  %s\n' "$(m6_cursor "$1" "$(basename "$f" .json)" | sha256sum | cut -d' ' -f1)" "$f"
  done <<<"$names"
}

# m6_outbox <who>: how many notes wait in that PC's outbox.
m6_outbox() {
  local f
  f=$(m6_paths "$1" | jq -r .state)/outbox/notes.jsonl
  m6_sh "$1" "if [ -e $(m6_q "$f") ]; then grep -c . $(m6_q "$f") || true; else echo 0; fi" || fail "cannot read $1's outbox"
}

# m6_meta <sid>: segment, records, last_uuid and machine_id of
# conversation:<sid> on the server (it must be there).
m6_meta() { m6_wait_doc "$1" | jq -c '.document_metadata | {segment, records, last_uuid, machine_id}'; }

# m6_split_names <who> <sid>: doctor's split-session check there is info
# and names the session among those continued on two PCs.
m6_split_names() {
  local out c
  out=$(m6_doctor "$1") || exit 1
  c=$(jq -c '[.data.checks[]? | select(.id == "split-session")][0] // {}' <<<"$out")
  jq -e --arg s "$2" '.status == "info" and ((.detail // "") | split(", ") | index($s) != null)' > /dev/null <<<"$c" ||
    fail "$1's doctor split-session is $c, want info naming $2"
}

# m6_split_list <who>: the sessions doctor's split-session check names
# there, one a line; nothing when it is not info.
m6_split_list() {
  local out
  out=$(m6_doctor "$1") || exit 1
  jq -r '[.data.checks[]? | select(.id == "split-session" and .status == "info")][0].detail // "" | split(", ")[] | select(. != "")' <<<"$out"
}

# ---- The install chain: A installs the server over real ssh (VM bench).

# m6_install_args [plan]: A's install command, I, in M6_I; with plan, I
# --plan, which reads no key.
m6_install_args() {
  local h
  h=$(m6_home a)
  M6_I=(server install --ssh "bs@$M6_SERVER_IP" --source "$h/release" --provider mock --model mock-model)
  if [ "${1:-}" != plan ]; then M6_I+=(--llm-key-from-file "$h/mock-key"); fi
  M6_I+=(--answer consent=yes --answer project=bs-m6 --answer cleartext_lan=yes --answer retention=keep --answer backfill=no --non-interactive --json)
  if [ "${1:-}" = plan ]; then M6_I+=(--plan); fi
}

# m6_server_config: the path of the server's config.toml.
m6_server_config() { m6_paths srv | jq -r .config_file; }

# ssh-unknown-host-key: A has never connected to the server; the install
# refuses its unknown key, changes nothing anywhere, and names the ssh to
# run. Then the user, who compared the fingerprint, trusts it.
m6_check_ssh_unknown_host_key() {
  local h kh before after out rc=0 pub
  h=$(m6_home a) kh=$(m6_home a)/.ssh/known_hosts
  m6_sh srv 'test -d "$HOME" && test ! -e "$HOME/.local/share/backsight"' ||
    fail "the server already holds ~/.local/share/backsight (or cannot be read): the check would prove nothing"
  before=$(m6_hash a "$kh")
  pl_untraced printf 'mock-not-a-key\n' | m6_put a "$h/mock-key" || fail "cannot write A's mock key"
  m6_install_args
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> "$M6_WORK/unknown-host.err") || rc=$?
  out=$(m6_json "the install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 10 ] || fail "the install with an unknown host key: exit $rc, want 10 ($(jq -r '.error.message // ""' <<<"$out"))"
  jq -e --arg c "ssh bs@$M6_SERVER_IP" '[.error.actions[]?.cmd] | index($c) != null' > /dev/null <<<"$out" ||
    fail "exit 10 does not name the action ssh bs@$M6_SERVER_IP: $(jq -c '.error.actions' <<<"$out")"
  after=$(m6_hash a "$kh")
  [ "$after" = "$before" ] || fail "A's known_hosts changed: $before, now $after"
  m6_sh srv 'test ! -e "$HOME/.local/share/backsight"' || fail "the refused install left ~/.local/share/backsight on the server"
  pub=$(m6_sh srv 'cut -d" " -f1,2 /etc/ssh/ssh_host_ed25519_key.pub') && [ -n "$pub" ] || fail "cannot read the server's host key"
  printf '%s %s\n' "$M6_SERVER_IP" "$pub" | m6_sh a 'mkdir -p -m 700 ~/.ssh && cat >> ~/.ssh/known_hosts && chmod 600 ~/.ssh/known_hosts' ||
    fail "cannot add the server's key to A's known_hosts"
  m6_result ssh-unknown-host-key PASS "exit 10 naming ssh bs@$M6_SERVER_IP; A's known_hosts $before before and after; nothing on the server" \
    "$(jq -cn --arg kh "$before" '{known_hosts_before: $kh}')"
}

# ssh-dropped-mid-install: A's link is cut for M6_DROP_S s (90; ssh's
# keep-alives give up after 60 s) while the host runs its setup. The
# install stops resumable (exit 40), and nothing it printed holds a key:
# the mock LLM key, a token, or the server's API key when the cut setup
# already wrote one (ssh-install scans this output again with A's key,
# once the resume gave it one).
m6_check_ssh_dropped_mid_install() {
  local h d rc out seen=0 _ hits
  h=$(m6_home a) d=$(m6_home a)/m6-drop
  m6_install_args
  m6_sh a "rm -rf $(m6_q "$d") && mkdir -p $(m6_q "$d")" || fail "cannot make $d on A"
  printf 'cd %s || exit 1\n%s %s > out 2> err < /dev/null\necho $? > code\n' "$(m6_q "$d")" "$(m6_q "$h/backsight")" "$(m6_q "${M6_I[@]}")" |
    m6_put a "$d/run.sh" || fail "cannot write the detached install on A"
  m6_sh a "setsid nohup sh $(m6_q "$d/run.sh") > /dev/null 2>&1 < /dev/null &" || fail "cannot start the detached install on A"
  # The host's setup relays its steps: its server step (the image start
  # and health wait) is the longest.
  for _ in $(seq 1 300); do
    if m6_sh a "grep -q '\"step\":\"host:server\"' $(m6_q "$d/err")"; then
      seen=1
      break
    fi
    if m6_sh a "test -e $(m6_q "$d/code")"; then
      fail "the install ended (exit $(m6_get a "$d/code")) before the host's setup reached its server step"
    fi
    sleep 2
  done
  [ "$seen" = 1 ] || fail "the host's setup never reached its server step (10 min)"
  m6_link a down
  sleep "${M6_DROP_S:-90}"
  m6_link a up
  for _ in $(seq 1 100); do
    rc=$(m6_get a "$d/code" 2> /dev/null) && [ -n "$rc" ] && break
    rc=
    sleep 3
  done
  [ -n "$rc" ] || fail "the install on A did not end within 5 min of the link coming back"
  out=$(m6_get a "$d/out") || fail "cannot read the install's output"
  out=$(m6_json "the dropped install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 40 ] || fail "the dropped install: exit $rc, want 40 ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  jq -e '.error.resume // "" | startswith("backsight server install --ssh bs@")' > /dev/null <<<"$out" ||
    fail "exit 40 does not resume with backsight server install --ssh: $(jq -r '.error.resume // "none"' <<<"$out")"
  jq -r .error.resume <<<"$out" > "$M6_WORK/resume"
  # Positive control: the reader sees what the install printed.
  m6_sh a "grep -q '\"event\":\"progress\"' $(m6_q "$d/err") && test -s $(m6_q "$d/out")" || fail "the install printed no progress line: the key check would prove nothing"
  m6_learn_key srv
  hits=$({ m6_get a "$d/out" && m6_get a "$d/err"; } | m6_secret_hits) || fail "cannot read the install's output on A"
  [ "$hits" = 0 ] || fail "the install's output holds the LLM key, a token or the server's API key ($hits line(s))"
  m6_result ssh-dropped-mid-install PASS "link down ${M6_DROP_S:-90} s during host:server; exit 40, resumable; no key in the output ($(jq -r .error.message <<<"$out" | cut -c1-160))"
}

# ssh-install: the resume, once the host's setup cut by the drop is gone,
# installs the server and joins it from A. Neither the dropped install's
# output nor the resumed one's holds the server's API key (A's now), the
# mock LLM key or a token.
m6_check_ssh_install() {
  local h resume out rc=0 deadline state cfg url mode doc srvshell unit active at hits
  h=$(m6_home a)
  [ -s "$M6_WORK/resume" ] || fail "no resume command from ssh-dropped-mid-install"
  resume=$(cat "$M6_WORK/resume")
  deadline=$((SECONDS + 900))
  while m6_sh srv "pgrep -u bs -f 'setup --role server' > /dev/null"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "a backsight setup still runs on the server after 15 min"
    sleep 10
  done
  out=$(m6_sh a ". ~/.bs-m6-env 2> /dev/null; $(m6_q "$h/backsight") ${resume#backsight } --json" < /dev/null 2> "$M6_WORK/install.err") || rc=$?
  out=$(m6_json "the resumed install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] || fail "the resumed install: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  [ "$(jq -r '[.data.steps[] | select(.name == "release")][0].status' <<<"$out")" = skipped ] ||
    fail "the resume did not skip the release step: $(jq -c '[.data.steps[] | select(.name == "release")]' <<<"$out")"
  m6_learn_key a
  m6_learn_key srv
  # Positive controls: a key to look for, and output to look in.
  [ -s "$(m6_keys)" ] || fail "no API key learnt from A or the server: the key scan would prove nothing"
  grep -q '"event":"progress"' "$M6_WORK/install.err" || fail "the resumed install printed no progress line: the key scan would prove nothing"
  hits=$({ m6_get a "$h/m6-drop/out" && m6_get a "$h/m6-drop/err" && cat "$M6_WORK/install.err"; } | m6_secret_hits) ||
    fail "cannot read the dropped install's output on A"
  [ "$hits" = 0 ] || fail "the dropped or the resumed install's output holds the server's API key, the LLM key or a token ($hits line(s))"
  # Positive control: the install ran through bs's tcsh.
  srvshell=$(m6_sh srv 'getent passwd bs')
  [ "${srvshell##*:}" = /usr/bin/tcsh ] || fail "bs's login shell on the server is ${srvshell##*:}, not /usr/bin/tcsh: the install did not go through a non-POSIX shell"
  [ "$(m6_paths srv | jq -r .server_role)" = server ] || fail "the server's backsight is not in the server role"
  m6_sh srv 'docker compose ls --format json' | jq -e 'any(.[]; .Name == "bs-m6")' > /dev/null || fail "docker compose ls on the server lists no bs-m6"
  state=$(m6_paths srv | jq -r .state)
  m6_sh srv "test -d $(m6_q "$state") && test ! -e $(m6_q "$state/ssh-handback-token")" || fail "the handback token is still in $state on the server (or no state dir)"
  cfg=$(m6_paths a | jq -r .config_file)
  cfg=$(m6_get a "$cfg") || fail "cannot read A's config"
  mode=$(m6_toml server mode <<<"$cfg") url=$(m6_toml server url <<<"$cfg")
  [ "$mode $url" = "remote http://$M6_SERVER_IP:8888" ] || fail "A's config: mode '$mode' url '$url', want remote http://$M6_SERVER_IP:8888"
  doc=$(m6_doctor a) || exit 1
  jq -e '(.data.checks | length > 0) and ([.data.checks[] | select(.status == "error")] | length == 0)' > /dev/null <<<"$doc" ||
    fail "doctor on A reports errors: $(jq -c '[.data.checks[]? | select(.status == "error") | {id, summary}]' <<<"$doc")"
  [ "$(jq -r '[.data.checks[] | select(.id == "server-id") | .status][0]' <<<"$doc")" = ok ] ||
    fail "doctor's server-id on A: $(jq -c '[.data.checks[] | select(.id == "server-id")]' <<<"$doc")"
  # server-id ok is a live read of the server's id (retain.CheckServer):
  # it carries the proof that A reaches the server. server-reach, info at
  # best (never ok), must be evaluated (not skipped) and not warn.
  case $(jq -r '[.data.checks[] | select(.id == "server-reach") | "\(.status) \(.summary // "")"][0] // "missing"' <<<"$doc") in
    "info skipped"* | "ok skipped"*) fail "doctor's server-reach on A was skipped: $(jq -c '[.data.checks[] | select(.id == "server-reach")]' <<<"$doc")" ;;
    info\ * | ok\ *) ;;
    *) fail "doctor's server-reach on A: $(jq -c '[.data.checks[] | select(.id == "server-reach")]' <<<"$doc")" ;;
  esac
  # For timers: A's tick timer right after the install.
  unit=$(m6_sh a "systemctl --user list-unit-files --no-legend --plain '*-tick.timer' | awk '{ print \$1 }'")
  [ "$(printf '%s\n' "$unit" | grep -c .)" = 1 ] || fail "A has $(printf '%s\n' "$unit" | grep -c .) tick timer(s), want one: $unit"
  active=$(m6_sh a "systemctl --user is-active $(m6_q "$unit")" || true)
  at=$(m6_sh a 'date +%s')
  printf '%s %s %s\n' "$unit" "$active" "$at" > "$M6_WORK/timers"
  m6_result ssh-install PASS "resumed: exit 0, release skipped; server role, compose bs-m6, no handback token; A remote on $url, doctor clean, server-id ok; bs's shell tcsh; no key in either install's output" \
    "$(jq -cn --argjson steps "$(jq -c '[.data.steps[] | {name, status}]' <<<"$out")" '{steps: $steps}')"
}

# ssh-changed-host-key: the server's host key changes; A's install (plan)
# refuses it as changed, without telling to remove the old one, and
# changes nothing; with the old key back it plans again.
m6_check_ssh_changed_host_key() {
  local kh before cfg cfg_before out rc=0 msg trusted now
  kh=$(m6_home a)/.ssh/known_hosts
  before=$(m6_hash a "$kh")
  cfg=$(m6_server_config)
  cfg_before=$(m6_hash srv "$cfg")
  [ "$cfg_before" != absent ] || fail "the server has no config.toml: the check would prove nothing"
  m6_install_args plan
  m6_host_key new
  # Positive control: the server now shows another key than A trusts.
  trusted=$(m6_sh a "awk '\$1 == \"$M6_SERVER_IP\" { print \$3 }' ~/.ssh/known_hosts")
  now=$(m6_sh srv 'cut -d" " -f2 /etc/ssh/ssh_host_ed25519_key.pub' || true)
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> /dev/null) || rc=$?
  m6_host_key old
  [ -n "$trusted" ] && [ -n "$now" ] && [ "$trusted" != "$now" ] || fail "the server's key did not change (A trusts '$trusted', the server shows '$now')"
  out=$(m6_json "the plan on A with a changed key (exit $rc)" "$out") || exit 1
  msg=$(jq -r '.error.message // ""' <<<"$out")
  [ "$rc" = 20 ] || fail "the plan with a changed host key: exit $rc, want 20 ($msg)"
  case $msg in *"changed since"*) ;; *) fail "the refusal does not say the key changed: $msg" ;; esac
  case $(jq -c .error <<<"$out") in *"ssh-keygen -R"*) fail "the refusal tells to remove the old key (ssh-keygen -R): $msg" ;; esac
  [ "$(m6_hash a "$kh")" = "$before" ] || fail "A's known_hosts changed"
  [ "$(m6_hash srv "$cfg")" = "$cfg_before" ] || fail "the server's config.toml changed"
  rc=0
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> /dev/null) || rc=$?
  [ "$rc" = 0 ] || fail "the plan with the old key back: exit $rc ($(jq -r '.error.message // ""' <<<"$out" 2> /dev/null))"
  m6_result ssh-changed-host-key PASS "exit 20, the key changed, no ssh-keygen -R; A's known_hosts and the server's config unchanged; the old key back: plan exit 0"
}

# login-shells: the remote steps under every login shell, on the server
# (Task 3's TestRemoteStepsUnderEveryLoginShell, each shell required).
m6_check_login_shells() {
  local dir out rc=0 passed shells=(sh dash bash zsh fish tcsh csh) s n=0
  (cd "$M6_ROOT" && CGO_ENABLED=0 go test -c -o "$M6_WORK/cli.test" ./internal/cli/) > "$M6_WORK/cli.test.log" 2>&1 ||
    fail "go test -c ./internal/cli/ failed: $(tail -n 3 "$M6_WORK/cli.test.log")"
  dir=$(m6_home srv)/m6-shells
  m6_sh srv "rm -rf $(m6_q "$dir") && mkdir -p $(m6_q "$dir")" || fail "cannot make $dir on the server"
  m6_put srv "$dir/cli.test" < "$M6_WORK/cli.test" || fail "cannot copy cli.test to the server"
  out=$(m6_sh srv "cd $(m6_q "$dir") && chmod 700 cli.test && BACKSIGHT_REQUIRE_SHELLS=sh,dash,bash,zsh,fish,tcsh,csh ./cli.test -test.run '^TestRemoteStepsUnderEveryLoginShell\$' -test.v 2>&1") || rc=$?
  printf '%s\n' "$out" > "$M6_WORK/login-shells.log"
  [ "$rc" = 0 ] || fail "TestRemoteStepsUnderEveryLoginShell on the server: exit $rc: $(grep -E -- '--- (FAIL|SKIP)' <<<"$out" | head -n 3 | paste -sd ' ')"
  for s in "${shells[@]}"; do
    if grep -qE -- "^ *--- PASS: TestRemoteStepsUnderEveryLoginShell/$s " <<<"$out"; then n=$((n + 1)); else passed+="$s "; fi
  done
  [ "$n" = 7 ] || fail "$n of 7 shells passed (missing: $passed)"
  grep -q -- '^--- PASS: TestRemoteStepsUnderEveryLoginShell ' <<<"$out" || fail "the test itself did not pass"
  m6_result login-shells PASS "7 of 7 login shells pass on the server (${shells[*]})"
}

# two-clients: B joins the server A installed with a token from its host;
# each sends a session: both on the server once, each with its sender's
# machine id, both clients bound to the same server id.
m6_check_two_clients() {
  local state tok out rc=0 who sid mid ids doc counts sa sb ia ib
  m6_learn_key srv
  m6_learn_key a
  state=$(m6_paths srv | jq -r .state)
  out=$(m6_on srv server token --out "$state/bench-token" --json < /dev/null) || fail "server token on the server failed: $(jq -r '.error.message // ""' <<<"$out" 2> /dev/null)"
  tok=$(m6_home b)/token
  m6_get srv "$state/bench-token" | m6_put b "$tok" || fail "cannot copy the token to B"
  m6_sh srv "rm -f $(m6_q "$state/bench-token")" || fail "cannot remove the token on the server"
  [ "$(m6_sh b "stat -c %a $(m6_q "$tok")")" = 600 ] || fail "B's token file is not 0600"
  pl_untraced m6_sh b "grep -q '^bst1\\.' $(m6_q "$tok")" || fail "B's token file holds no token"
  out=$(m6_on b connect --token-file "$tok" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json < /dev/null 2> "$M6_WORK/connect-b.err") || rc=$?
  out=$(m6_json "connect on B (exit $rc)" "$out") || exit 1
  m6_learn_key b
  [ "$rc" = 0 ] && jq -e .ok > /dev/null <<<"$out" || fail "connect on B: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  for who in a b; do
    sid=tc-$who
    m6_session "$who" "$sid" bs-m6-two 1 3 r 120
    out=$(m6_flush "$who" "$sid") || exit 1
    jq -e --arg s "$sid" '.ok and ([.data.results[] | select(.session == $s and .status == "sent")] | length == 1)' > /dev/null <<<"$out" ||
      fail "$who did not send $sid: $(jq -c '.data.results // .error' <<<"$out")"
  done
  m6_queue_wait
  for who in a b; do
    sid=tc-$who
    doc=$(m6_wait_doc "$sid") || exit 1
    mid=$(m6_get "$who" "$(m6_paths "$who" | jq -r .state)/machine_id") || fail "cannot read $who's machine_id"
    [ -n "$mid" ] && [ "$(jq -r '.document_metadata.machine_id // ""' <<<"$doc")" = "$mid" ] ||
      fail "conversation:$sid's machine_id is '$(jq -r '.document_metadata.machine_id // ""' <<<"$doc")', $who's is '$mid'"
    printf '%s\n' "$mid" >> "$M6_WORK/machine-ids"
    counts=$(m6_uuid_counts "$sid") || exit 1
    [ "$counts" = "$(printf '%s 1\n' "$sid-r1" "$sid-r2" "$sid-r3")" ] || fail "conversation:$sid holds the records $(paste -sd ' ' <<<"$counts"), want r1 r2 r3 once each"
  done
  # Positive control: the two machine ids differ, so each document names its own sender.
  ids=$(sort -u "$M6_WORK/machine-ids" | grep -c .)
  [ "$ids" = 2 ] || fail "A and B have the same machine id: the machine_id check would prove nothing"
  for who in a b; do
    doc=$(m6_doctor "$who") || exit 1
    jq -e '(.data.checks | length > 0) and ([.data.checks[] | select(.status == "error")] | length == 0)' > /dev/null <<<"$doc" ||
      fail "doctor on $who reports errors: $(jq -c '[.data.checks[]? | select(.status == "error") | {id, summary}]' <<<"$doc")"
    [ "$(jq -r '[.data.checks[] | select(.id == "server-id") | .status][0]' <<<"$doc")" = ok ] ||
      fail "doctor's server-id on $who: $(jq -c '[.data.checks[] | select(.id == "server-id")]' <<<"$doc")"
  done
  sa=$(m6_paths a | jq -r .state) sb=$(m6_paths b | jq -r .state)
  ia=$(m6_get a "$sa/state.json" | jq -r '.remote_server.id // ""') ib=$(m6_get b "$sb/state.json" | jq -r '.remote_server.id // ""')
  [ -n "$ia" ] && [ "$ia" = "$ib" ] || fail "the clients' remote_server.id differ: A '$ia', B '$ib'"
  m6_result two-clients PASS "B joined; tc-a and tc-b on the server once each, with their sender's machine id; both doctors clean, server-id ok; server $ia" \
    "$(jq -cn --arg id "$ia" '{server_id: $id, records: 6}')"
}

# ---- Dedup and key rotation (also the CI chain's checks).

# The project of the dedup sessions, and how long the bench PCs have
# retained live (m6_live_since) when a check needs live retain or verify
# --live to see sessions 90 and 120 min old.
M6_DEDUP=bs-m6-dedup
M6_LIVE_MIN=150

# dedup-copied: a session A sent, copied to B before B's retain_since
# (backfill's), is not stored again by B's backfill nor its tick.
m6_check_dedup_copied() {
  local before after out rc=0 tick cur counts
  m6_session a cp1 "$M6_DEDUP" 1 3 r 120
  m6_flush_sent a cp1 cp1-r3 > /dev/null
  m6_queue_wait
  before=$(m6_meta cp1) || exit 1
  m6_copy a b cp1 "$M6_DEDUP"
  # Mock prices are unknown: --max-cost is refused, --max-tokens caps.
  out=$(m6_on b retain backfill --json --project "$M6_DEDUP" --max-tokens 1000000 < /dev/null 2> "$M6_WORK/backfill-b.err") || rc=$?
  out=$(m6_json "retain backfill on B (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] && jq -e '.ok == true' > /dev/null <<<"$out" || fail "retain backfill on B: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  tick=$(m6_tick b) || exit 1
  m6_queue_wait
  # Positive control: B saw the session, its cursor at the last record.
  cur=$(m6_cursor b cp1) || exit 1
  [ "$(jq -r '.last_uuid // ""' <<<"${cur:-null}")" = cp1-r3 ] ||
    fail "B's cursor of cp1 is '$(jq -c . <<<"${cur:-null}")', want last_uuid cp1-r3: B never saw the copy, the check would prove nothing"
  after=$(m6_meta cp1) || exit 1
  [ "$after" = "$before" ] || fail "conversation:cp1 was stored again: $before before B's backfill, $after after"
  counts=$(m6_uuid_counts cp1) || exit 1
  [ "$counts" = "$(printf 'cp1-r%s 1\n' 1 2 3)" ] || fail "conversation:cp1 holds the records $(paste -sd ' ' <<<"$counts"), want r1 r2 r3 once each"
  m6_result dedup-copied PASS "cp1 sent by A, copied to B: B's backfill and tick stored nothing again (segment $(jq -r .segment <<<"$after"), $(jq -r .records <<<"$after") records, last $(jq -r .last_uuid <<<"$after")); B's cursor at cp1-r3" \
    "$(jq -cn --argjson b "$(jq -c '.data | {total, done, finished}' <<<"$out")" --argjson t "$(jq -c '.data.sessions // {}' <<<"$tick")" '{backfill: $b, tick_sessions: $t}')"
}

# dedup-concurrent: five sessions on both PCs, each flushed on A and on B
# at once: each stored once, whoever won: its records once in the
# document, and no more facts than the same session sent by one PC (cb1,
# the baseline: the mock LLM extracts a fixed set from such a segment).
# At least one pair must have raced (both flushes sent): five pairs that
# all ran one after the other prove only what dedup-copied does.
m6_check_dedup_concurrent() {
  local i sid who w rc out st both doc counts base n facts=()
  w=$M6_WORK/concurrent
  rm -rf "$w"
  mkdir -p "$w"
  m6_session a cb1 "$M6_DEDUP" 1 3 r 120
  m6_flush_sent a cb1 cb1-r3 > /dev/null
  for i in 1 2 3 4 5; do
    m6_session a "cc$i" "$M6_DEDUP" 1 3 r 120
    m6_copy a b "cc$i" "$M6_DEDUP"
  done
  for i in 1 2 3 4 5; do
    sid=cc$i
    for who in a b; do
      (
        rc=0
        m6_on "$who" retain flush --json --session "$sid" < /dev/null > "$w/$sid.$who" 2> "$w/$sid.$who.err" || rc=$?
        echo "$rc" > "$w/$sid.$who.rc"
      ) &
    done
    wait
    for who in a b; do
      rc=$(cat "$w/$sid.$who.rc")
      out=$(m6_json "retain flush of $sid on $who (exit $rc)" "$(cat "$w/$sid.$who")") || exit 1
      [ "$rc" = 0 ] && jq -e '.ok == true' > /dev/null <<<"$out" ||
        fail "the flush of $sid on $who, at the same time as the other PC's: exit $rc ($(jq -c '.error // .data.results' <<<"$out" | m6_scrub))"
      st=$(jq -r --arg s "$sid" '[.data.results[]? | select(.session == $s) | .status][0] // "nothing"' <<<"$out")
      printf '%s %s %s\n' "$sid" "$who" "$st" >> "$w/statuses"
    done
  done
  both=$(awk '$3 == "sent" { n[$1]++ } END { c = 0; for (s in n) if (n[s] == 2) c++; print c }' "$w/statuses")
  [ "$both" -ge 1 ] || fail "no pair raced: in each of cc1-cc5 one PC sent and the other found it sent ($(awk '{ printf "%s%s %s %s", sep, $1, $2, $3; sep = ", " }' "$w/statuses")): the check would prove nothing dedup-copied does not"
  m6_queue_wait
  for i in 1 2 3 4 5; do
    sid=cc$i
    doc=$(m6_wait_doc "$sid") || exit 1
    [ "$(jq -r '.document_metadata.records // ""' <<<"$doc")" = 3 ] ||
      fail "conversation:$sid's last segment holds $(jq -r '.document_metadata.records // "no"' <<<"$doc") records, want 3"
    counts=$(m6_uuid_counts "$sid") || exit 1
    [ "$counts" = "$(printf '%s 1\n' "$sid-r1" "$sid-r2" "$sid-r3")" ] || fail "conversation:$sid holds the records $(paste -sd ' ' <<<"$counts"), want r1 r2 r3 once each"
  done
  # The queue drained: every extraction is done. Positive control: the
  # baseline produced facts, so a doubled store would show.
  m6_wait_doc cb1 > /dev/null
  base=$(m6_facts conversation:cb1) || exit 1
  [ "$base" -gt 0 ] || fail "conversation:cb1, sent by A alone, produced no fact: a doubled store would not show"
  for i in 1 2 3 4 5; do
    n=$(m6_facts "conversation:cc$i") || exit 1
    [ "$n" -le "$base" ] || fail "conversation:cc$i holds $n facts, the same session sent by one PC $base: it was stored twice"
    facts+=("$n")
  done
  m6_result dedup-concurrent PASS "cc1-cc5 flushed on A and B at once, both exit 0: each on the server once, 3 records, facts ${facts[*]} (one PC's: $base); both sent $both of 5" \
    "$(jq -Rn --argjson both "$both" --argjson base "$base" --arg facts "${facts[*]}" \
      '{both_sent: $both, baseline_facts: $base, facts: ($facts | split(" ") | map(tonumber)), statuses: [inputs | split(" ") | {session: .[0], pc: .[1], status: .[2]}]}' < "$w/statuses")"
}

# m6_verify_split <who>: verify --live --json there; its data on stdout.
m6_verify_split() {
  local out rc=0
  out=$(m6_on "$1" verify --live --json < /dev/null 2> /dev/null) || rc=$?
  out=$(m6_json "verify --live on $1 (exit $rc)" "$out") || exit 1
  jq -e '.data.status // empty' > /dev/null <<<"$out" || fail "verify --live on $1 (exit $rc) gave no verdict: $(jq -c '.error // .' <<<"$out" | m6_scrub)"
  jq -c .data <<<"$out"
}

# dedup-split: a session sent by A, copied to B and continued on both:
# nothing lost (every record on the server), each PC's doctor names the
# split, and verify --live passes on both, B's naming the document A
# continued after it (another PC's later segment is no loss). Recorded:
# the shared records stored twice.
m6_check_dedup_split() {
  local out st doc ma cur seg_b counts u n twice who v sess summary status deadline wait_s left va=pending vb=pending ce_b
  m6_session a sp1 "$M6_DEDUP" 1 3 r 90
  m6_flush_sent a sp1 sp1-r3 > /dev/null
  m6_queue_wait
  m6_wait_doc sp1 > /dev/null
  m6_copy a b sp1 "$M6_DEDUP"
  out=$(m6_flush b sp1) || exit 1
  st=$(jq -r '[.data.results[]? | select(.session == "sp1") | .status][0] // "nothing"' <<<"$out")
  [ "$st" = nothing ] && jq -e '.ok == true' > /dev/null <<<"$out" || fail "B's flush of its copy of sp1: $st ($(jq -c '.error // .data.counts' <<<"$out" | m6_scrub)), want nothing sent"
  [ "$(m6_cursor b sp1 | jq -r '.last_uuid // empty')" = sp1-r3 ] || fail "B's cursor of sp1 does not end at sp1-r3: B did not take the server's position"
  m6_session a sp1 "$M6_DEDUP" 4 5 a 90
  m6_flush_sent a sp1 sp1-a5 > /dev/null
  m6_queue_wait
  doc=$(m6_meta sp1) || exit 1
  [ "$(jq -r '"\(.segment) \(.last_uuid)"' <<<"$doc")" = "2 sp1-a5" ] || fail "after A's a4 a5, conversation:sp1 is $doc, want segment 2 ending at sp1-a5"
  m6_session b sp1 "$M6_DEDUP" 4 5 b 90
  m6_flush_sent b sp1 sp1-b5 > /dev/null
  m6_queue_wait
  m6_split_names b sp1
  m6_session a sp1 "$M6_DEDUP" 6 6 a 90
  m6_flush_sent a sp1 sp1-a6 > /dev/null
  m6_queue_wait
  m6_split_names a sp1
  counts=$(m6_uuid_counts sp1) || exit 1
  for u in r1 r2 r3 a4 a5 a6 b4 b5; do
    n=$(awk -v u="sp1-$u" '$1 == u { print $2 }' <<<"$counts")
    [ "${n:-0}" -ge 1 ] || fail "sp1-$u is not on the server (records: $(paste -sd ' ' <<<"$counts"))"
  done
  twice=$(awk '$2 > 1 { printf "%s%s", (n++ ? " " : ""), $1 }' <<<"$counts")
  # Positive control of the verify: A wrote the last segment, above the one
  # B's cursor holds, so B's verify meets another PC's later segment.
  doc=$(m6_meta sp1) || exit 1
  ma=$(m6_get a "$(m6_paths a | jq -r .state)/machine_id") || fail "cannot read A's machine_id"
  cur=$(m6_cursor b sp1) || exit 1
  seg_b=$(jq -r '.segment // 0' <<<"${cur:-null}")
  [ "$(jq -r .machine_id <<<"$doc")" = "$ma" ] && [ "$(jq -r .segment <<<"$doc")" -gt "$seg_b" ] ||
    fail "conversation:sp1 is $doc, B's cursor at segment $seg_b: A's later segment is not above B's, the verify would prove nothing"
  m6_live_since a "$M6_LIVE_MIN"
  m6_live_since b "$M6_LIVE_MIN"
  deadline=$((SECONDS + ${M6_VERIFY_WAIT:-600}))
  while :; do
    wait_s=300
    for who in a b; do
      [ "$who" = a ] && [ "$va" != pending ] && continue
      [ "$who" = b ] && [ "$vb" != pending ] && continue
      v=$(m6_verify_split "$who") || exit 1
      sess=$(jq -r '.session // ""' <<<"$v") status=$(jq -r .status <<<"$v") summary=$(jq -r '.summary // ""' <<<"$v")
      [ "$sess" = sp1 ] || fail "verify --live on $who checked '${sess:-no session}' ($status: $summary), not sp1: it would prove nothing"
      case $status in
        pass) ;;
        pending) [ "$(jq -r '.wait_s // 300' <<<"$v")" -ge "$wait_s" ] || wait_s=$(jq -r '.wait_s // 300' <<<"$v") ;;
        *) fail "verify --live on $who: $status: $summary ($(jq -r '.detail // ""' <<<"$v"))" ;;
      esac
      if [ "$who" = a ]; then va=$status; else vb=$status ce_b=$(jq -c '.continued_elsewhere // []' <<<"$v"); fi
    done
    [ "$va" = pass ] && [ "$vb" = pass ] && break
    left=$((deadline - SECONDS))
    [ "$left" -gt 0 ] || fail "verify --live still pending after ${M6_VERIFY_WAIT:-600} s: A $va, B $vb"
    sleep $((wait_s < 10 ? 10 : (wait_s > left ? left : wait_s)))
  done
  jq -e 'index("conversation:sp1") != null' > /dev/null <<<"$ce_b" ||
    fail "verify --live on B passed without naming conversation:sp1 as continued on another PC ($ce_b)"
  m6_result dedup-split PASS "sp1 continued on A and B: r1-3 a4 a5 a6 b4 b5 all on the server, both doctors name the split; verify --live passes on A and on B (continued elsewhere: $ce_b); stored twice: ${twice:-none}" \
    "$(jq -cn --arg twice "$twice" --arg va "$va" --arg vb "$vb" --argjson ce "${ce_b:-[]}" '{stored_twice: ($twice | split(" ") | map(select(. != ""))), verify: {a: $va, b: $vb}, continued_elsewhere_b: $ce}')"
}

# key-rotation: the server's key rotates; the old token is refused, and
# what both PCs wrote meanwhile (sessions, a note) reaches the server once
# each holds the new one, with no reset of their cursors.
m6_check_key_rotation() {
  local old state code out rc=0 note st tick who sec sec_before tok cb ca p=bs-m6-rot ask
  old=$(m6_home b)/token
  [ "$(m6_sh b "stat -c %a $(m6_q "$old") 2> /dev/null" || true)" = 600 ] || fail "two-clients left no old token in $old: the check needs it"
  m6_live_since a "$M6_LIVE_MIN"
  m6_live_since b "$M6_LIVE_MIN"
  # Positive control: the old key lists the banks before the rotation.
  code=$(m6_banks_code) || exit 1
  [ "$code" = 200 ] || fail "the banks list with B's key before the rotation: HTTP $code, want 200: a 401 after it would prove nothing"
  state=$(m6_paths srv | jq -r .state)
  # The new token leaves no copy on any machine, whatever way the check
  # ends: armed before the rotation writes it (a file not there yet is no
  # harm). The paths are expanded now: the function's locals are gone by
  # the time its subshell exits.
  trap "m6_sh srv $(m6_q "rm -f $(m6_q "$state/bench-token2")") > /dev/null 2>&1 || true
    m6_sh a $(m6_q "rm -f $(m6_q "$(m6_home a)/token2")") > /dev/null 2>&1 || true
    m6_sh b $(m6_q "rm -f $(m6_q "$(m6_home b)/token2")") > /dev/null 2>&1 || true" EXIT
  rc=0
  out=$(m6_on srv server token --rotate --out "$state/bench-token2" --json < /dev/null 2> "$M6_WORK/rotate.err") || rc=$?
  m6_learn_key srv # the new key, masked from here on
  [ "$rc" = 0 ] || fail "server token --rotate on the server failed: $(jq -r '.error.message // ""' <<<"$out" 2> /dev/null | m6_scrub)"
  code=$(m6_banks_code) || exit 1
  [ "$code" = 401 ] || fail "the banks list with the old key after the rotation: HTTP $code, want 401"
  # The old key's period: a note and a session on each PC, nothing sent.
  rc=0
  out=$(m6_on a store --json --kind decision --project "$p" "bs-m6 key-rotation: the export pages by 200, decided while the key rotated" < /dev/null 2> /dev/null) || rc=$?
  out=$(m6_json "store on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] && [ "$(jq -r '.data.status // ""' <<<"$out")" = queued ] || fail "store on A with the old key: exit $rc, $(jq -c '.data // .error' <<<"$out" | m6_scrub), want queued"
  note=$(jq -r .data.document <<<"$out")
  m6_session a kr-a "$p" 1 3 r 120
  m6_session b kr-b "$p" 1 3 r 120
  for who in a b; do
    tick=$(m6_tick "$who" partial) || exit 1
    [ "$(jq -r '.data.sessions.sent // 0' <<<"$tick")" = 0 ] || fail "with the old key, the tick on $who sent $(jq -c .data.sessions <<<"$tick")"
    [ "$(jq -r '.data.notes.sent // 0' <<<"$tick")" = 0 ] || fail "with the old key, the tick on $who sent a note"
    printf '%s\n' "$tick" > "$M6_WORK/rotation-tick-$who"
  done
  [ "$(jq -r '.data.notes.kept // 0' < "$M6_WORK/rotation-tick-a")" -ge 1 ] || fail "with the old key, A's tick kept no note queued: $(jq -c .data.notes < "$M6_WORK/rotation-tick-a")"
  # B with the old token: refused, its key untouched.
  sec=$(m6_paths b | jq -r .secrets_file)
  sec_before=$(m6_hash b "$sec")
  rc=0
  out=$(m6_on b connect --token-file "$old" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json < /dev/null 2> /dev/null) || rc=$?
  out=$(m6_json "connect on B with the old token (exit $rc)" "$out") || exit 1
  case "$rc $(jq -r '.error.message // ""' <<<"$out")" in
    "20 "*"the server rejected this token's key"*) ;;
    *) fail "connect on B with the old token: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub)), want 20, the server rejected this token's key" ;;
  esac
  [ "$(m6_hash b "$sec")" = "$sec_before" ] || fail "the refused connect changed B's secrets.toml"
  m6_sh b "rm -f $(m6_q "$old")" || fail "cannot remove the old token on B"
  # The new token, to each PC (0600, removed once used).
  for who in b a; do
    tok=$(m6_home "$who")/token2
    m6_get srv "$state/bench-token2" | m6_put "$who" "$tok" || fail "cannot copy the new token to $who"
    [ "$(m6_sh "$who" "stat -c %a $(m6_q "$tok")")" = 600 ] || fail "$who's new token file is not 0600"
  done
  m6_sh srv "rm -f $(m6_q "$state/bench-token2")" || fail "cannot remove the new token on the server"
  for who in b a; do
    tok=$(m6_home "$who")/token2
    cb=$(m6_cursors "$who" | grep -v ' \./kr-') || fail "$who has no session cursor: a reset would prove nothing"
    ask=(--answer consent=yes --answer backfill=no --answer retention=keep --answer "scheduler=$(m6_scheduler "$who")")
    rc=0
    out=$(m6_on "$who" connect --token-file "$tok" "${ask[@]}" --non-interactive --json < /dev/null 2> "$M6_WORK/connect2-$who.err") || rc=$?
    m6_learn_key "$who"
    m6_sh "$who" "rm -f $(m6_q "$tok")" || fail "cannot remove the new token on $who"
    out=$(m6_json "connect on $who with the new token (exit $rc)" "$out") || exit 1
    [ "$rc" = 0 ] && jq -e '.ok == true' > /dev/null <<<"$out" || fail "connect on $who with the new token: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
    st=$(jq -r '.data.situation // ""' <<<"$out")
    [ "$st" = same-server ] || fail "connect on $who with the new token: situation $st, want same-server"
    ca=$(m6_cursors "$who")
    [ -z "$(comm -23 <(printf '%s\n' "$cb") <(printf '%s\n' "$ca"))" ] || fail "connect on $who moved or changed its cursors (a reset): $(comm -23 <(printf '%s\n' "$cb") <(printf '%s\n' "$ca") | paste -sd ' ')"
    if [ "$who" = b ]; then
      # Read with B's new key: A, still on the old one, sent nothing.
      [ -z "$(m6_doc conversation:kr-a)" ] || fail "conversation:kr-a reached the server while A held the old key"
    fi
  done
  # For timers: the run judged comes after A's new key.
  m6_sh a 'date +%s' > "$M6_WORK/timers-after"
  for who in a b; do
    tick=$(m6_tick "$who") || exit 1
  done
  m6_queue_wait
  for who in a b; do
    m6_wait_doc "kr-$who" > /dev/null
    out=$(m6_uuid_counts "kr-$who") || exit 1
    [ "$out" = "$(printf 'kr-%s-r%s 1\n' "$who" 1 "$who" 2 "$who" 3)" ] || fail "conversation:kr-$who holds the records $(paste -sd ' ' <<<"$out"), want r1 r2 r3 once each"
    [ "$(m6_outbox "$who")" = 0 ] || fail "$who's outbox still holds $(m6_outbox "$who") note(s)"
  done
  [ -n "$(m6_doc "$note")" ] || fail "the note A stored with the old key ($note) is not on the server"
  m6_result key-rotation PASS "old key 401, old token refused (exit 20, B's key unchanged); with the new token both same-server, cursors kept; kr-a, kr-b and the note on the server once, outboxes empty" \
    "$(jq -cn --argjson ta "$(jq -c '.data | {sessions, notes}' < "$M6_WORK/rotation-tick-a")" --argjson tb "$(jq -c '.data | {sessions, notes}' < "$M6_WORK/rotation-tick-b")" '{old_key_ticks: {a: $ta, b: $tb}}')"
}

# ---- Restore and purge across clients (decision 0044 and its tombstones,
# seen from two PCs).

# m6_epoch <who>: the restore epoch that PC last reconciled with (0: none).
m6_epoch() {
  local st
  st=$(m6_get "$1" "$(m6_paths "$1" | jq -r .state)/state.json") || fail "cannot read $1's state.json"
  jq -r '.restore_epoch.epoch // 0' <<<"$st"
}

# m6_ticks <who> <since>: every jobs tick that PC ran since <since> (an
# ISO time of that machine's clock, UTC), skipped ones aside: its telemetry
# event (JSON), one a line. A tick the VM bench's timer ran is seen as one
# run by m6_tick: the telemetry is the product's own record of both. No
# telemetry is no tick; one that cannot be read fails.
m6_ticks() {
  local tel raw
  tel=$(m6_paths "$1" | jq -r .state)/telemetry
  raw=$(m6_sh "$1" "if [ -d $(m6_q "$tel") ]; then cat $(m6_q "$tel")/events-*.jsonl 2> /dev/null || true; fi") ||
    fail "cannot read $1's telemetry"
  jq -cR --arg s "$2" 'fromjson? | select(.event == "job" and .name == "tick" and .status != "skipped" and .ts >= $s)' <<<"$raw"
}

# m6_reconciles <who> <since>: one line per tick of m6_ticks: its restore
# epoch reconcile (details.epoch), "-" for a tick that reconciled nothing.
m6_reconciles() {
  local ticks
  ticks=$(m6_ticks "$1" "$2") || exit 1
  [ -z "$ticks" ] || jq -r '(.details.epoch // "") | if . == "" then "-" else . end' <<<"$ticks"
}

# m6_settle <who…>: those PCs have nothing left to send before a check
# counts what each tick sends (an earlier check, in whatever order the
# driver runs them, may have left a session or a note): each ticks, the
# queue drains, until every tick of a round sent nothing; still sending
# after M6_SETTLE_ROUNDS rounds (5) fails.
# It waits for sends only, never for a session still settling (its last
# turn under 30 min old, e.g. unsent-cost's 200 transcripts): such a
# session would go out during the counted ticks and break their strict
# counts. A tick that reports one fails at once, naming the transcripts
# written in the last 30 min: run restore and purge before such checks,
# or 30 min after them.
m6_settle() {
  local round who tick sent left waiting names
  for round in $(seq 1 "${M6_SETTLE_ROUNDS:-5}"); do
    left=
    for who in "$@"; do
      tick=$(m6_tick "$who") || exit 1
      waiting=$(jq -r '.data.sessions.waiting // 0' <<<"$tick")
      if [ "$waiting" != 0 ]; then
        names=$(m6_sh "$who" 'find "$HOME/.claude/projects" -name "*.jsonl" -mmin -30 -printf "%f\n" 2> /dev/null | sed "s/\.jsonl\$//" | sort | head -n 5 | paste -sd " " -' || true)
        fail "$who has $waiting session(s) still settling (last turn under 30 min old; written in the last 30 min: ${names:-none found}): they would be sent during the counted ticks; run this check before such sessions, or 30 min after them"
      fi
      sent=$(jq -r '(.data.sessions.sent // 0) + (.data.notes.sent // 0)' <<<"$tick")
      [ "$sent" = 0 ] || left+="${left:+; }$who sent $(jq -c '.data.sessions | {sent}' <<<"$tick") (notes $(jq -r '.data.notes.sent // 0' <<<"$tick"))"
    done
    m6_queue_wait
    [ -n "$left" ] || return 0
  done
  fail "a session from an earlier check was still pending after ${M6_SETTLE_ROUNDS:-5} settle round(s): $left"
}

# m6_tombstoned <docid>: the server's control bank lists that document
# among its purge tombstones (read with B's key; no tombstones document,
# or one that does not parse, fails).
m6_tombstoned() {
  local doc
  doc=$(m6_doc backsight:tombstones -control) || exit 1
  [ -n "$doc" ] || fail "the control bank holds no tombstones document"
  jq -e --arg d "$1" '.document_metadata.tombstones | fromjson | map(.document) | index($d) != null' > /dev/null <<<"$doc" 2> /dev/null ||
    fail "the control bank's tombstones do not name $1: $(jq -c '.document_metadata.tombstones // "none"' <<<"$doc" 2> /dev/null | cut -c1-300)"
}

# restore-across-clients: the host restores a backup taken before each PC
# sent a session (rs-a, rs-b): both PCs see the new restore epoch (doctor
# warns), each one's next tick reconciles and re-sends its own lost
# session once, and nothing the archive holds (tc-a, tc-b) goes again.
# A session continued on both PCs (doctor's split-session, left by
# dedup-split when it ran before) may go again too: its document has no
# position in the transcript of the PC that did not write last, and
# dropSession (internal/retain/epoch.go) takes that PC's records there
# as lost, stored twice rather than lost (decision 0073). Those, and
# only those, are allowed besides rs-<who>.
m6_check_restore_across_clients() {
  local p=bs-m6-restore out rc=0 archive since who sid tick ep new lines n by_a by_b by old_a old_b old meta cur sent splits want
  # Each tick below is counted (one session re-sent, then none): nothing
  # an earlier check left may go out with them.
  m6_settle a b
  for who in a b; do
    m6_split_list "$who" > "$M6_WORK/restore-splits-$who" || exit 1
  done
  # Before the archive: tc-a and tc-b (two-clients) as the server holds
  # them and as each PC's cursor says.
  for who in a b; do
    sid=tc-$who
    m6_meta "$sid" > "$M6_WORK/restore-$sid.meta" || exit 1
    cur=$(m6_cursor "$who" "$sid") || exit 1
    [ -n "$cur" ] || fail "$who has no cursor of $sid: two-clients sent it, the check would prove nothing"
    printf '%s\n' "$cur" > "$M6_WORK/restore-$sid.cursor"
  done
  out=$(m6_on srv backup create --json < /dev/null 2> "$M6_WORK/backup.err") || rc=$?
  out=$(m6_json "backup create on the server (exit $rc)" "$out") || exit 1
  archive=$(jq -r 'select(.ok == true and (.data.skipped // "") == "") | .data.path // empty' <<<"$out")
  [ "$rc" = 0 ] && [ -n "$archive" ] || fail "backup create on the server: exit $rc, no archive ($(jq -c '.data.skipped // .error.message // ""' <<<"$out"))"
  m6_sh srv "test -s $(m6_q "$archive")" || fail "the archive $archive is not on the server"
  # After the archive: one session from each PC, sent and stored.
  m6_session a rs-a "$p" 1 3 r 120
  m6_session b rs-b "$p" 1 3 r 120
  m6_flush_sent a rs-a rs-a-r3 > /dev/null
  out=$(m6_flush b rs-b) || exit 1
  jq -e '.ok and ([.data.results[] | select(.session == "rs-b" and .status == "sent")] | length == 1)' > /dev/null <<<"$out" ||
    fail "b did not send rs-b: $(jq -c '.data.results // .error' <<<"$out" | m6_scrub)"
  m6_queue_wait
  for who in a b; do
    m6_meta "rs-$who" > "$M6_WORK/restore-rs-$who.meta" || exit 1
    m6_cursor "$who" "rs-$who" > "$M6_WORK/restore-rs-$who.cursor" || exit 1
  done
  old_a=$(m6_epoch a)
  old_b=$(m6_epoch b)
  # Every tick the PCs run from here is read back from their telemetry
  # (A's timer may run one between the restore and m6_tick).
  since=$(m6_sh a 'date -u +%Y-%m-%dT%H:%M:%S.000Z')
  printf '%s\n' "$since" > "$M6_WORK/restore-since-a"
  m6_sh b 'date -u +%Y-%m-%dT%H:%M:%S.000Z' > "$M6_WORK/restore-since-b"
  rc=0
  out=$(m6_on srv restore --json --non-interactive "$archive" < /dev/null 2> "$M6_WORK/restore.err") || rc=$?
  out=$(m6_json "restore on the server (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] && jq -e '.ok and (.data.rolled_back // false | not) and (.data.epoch.epoch > 0) and
    ([.data.steps[] | select(.status == "done") | .name] | index("counts") != null and index("recall") != null and index("epoch") != null)' > /dev/null <<<"$out" ||
    fail "restore on the server: exit $rc ($(jq -c '.error.message // [.data.steps[]? | {name, status}]' <<<"$out" | m6_scrub))"
  new=$(jq -r .data.epoch.epoch <<<"$out")
  [ "$new" -gt "$old_a" ] && [ "$new" -gt "$old_b" ] || fail "the restore wrote epoch $new, not above A's $old_a and B's $old_b"
  # Positive controls: the restore lost rs-a and rs-b and kept tc-a and
  # tc-b, and both PCs are behind the bank's epoch (A's timer may have
  # reconciled A already: its telemetry then says so).
  for who in a b; do
    # Present again only if A's timer reconciled and re-sent it already.
    if [ -n "$(m6_doc "conversation:rs-$who")" ]; then
      [ "$(m6_reconciles "$who" "$(cat "$M6_WORK/restore-since-$who")" | grep -c '^restore epoch ')" = 1 ] ||
        fail "conversation:rs-$who survived the restore, and no tick of $who reconciled: the check would prove nothing"
    fi
    [ -n "$(m6_doc "conversation:tc-$who")" ] || fail "conversation:tc-$who, which the archive holds, is not on the server after the restore"
    case $(m6_doctor_status "$who" restore-epoch) in
      warn) ;;
      ok)
        [ "$(m6_reconciles "$who" "$(cat "$M6_WORK/restore-since-$who")" | grep -c '^restore epoch ')" = 1 ] ||
          fail "right after the restore, $who's doctor restore-epoch is ok, and no tick of $who reconciled: want warn"
        ;;
      *) fail "right after the restore, $who's doctor restore-epoch is $(m6_doctor_status "$who" restore-epoch), want warn" ;;
    esac
  done
  for who in a b; do
    old=$old_a
    [ "$who" = a ] || old=$old_b
    # A reconcile holds while the bank has queued operations: the restored
    # queue first, then the re-send of the PC that reconciled before.
    m6_queue_wait
    tick=$(m6_tick "$who") || exit 1
    ep=$(jq -r '.data.epoch // ""' <<<"$tick")
    if [ -n "$ep" ]; then
      by=tick sent=$(jq -c '.data.sessions' <<<"$tick")
    else
      # A's timer reconciled first: what its tick sent, from its telemetry.
      by=timer sent=$(m6_ticks "$who" "$(cat "$M6_WORK/restore-since-$who")" |
        jq -sc '[.[] | select((.details.epoch // "") | startswith("restore epoch "))][0].details.sessions // {}')
    fi
    # rs-<who>, and at most the split sessions doctor named before.
    splits=$(grep -c . "$M6_WORK/restore-splits-$who" || true)
    want=$(jq -r '.sent // 0' <<<"$sent")
    [ "$want" -ge 1 ] && [ "$want" -le $((1 + splits)) ] ||
      fail "the $by tick that reconciled $who sent $sent, want rs-$who and at most the $splits split session(s) ($(paste -sd ' ' "$M6_WORK/restore-splits-$who"))"
    if [ "$who" = a ]; then by_a=$by; else by_b=$by; fi
    # Every tick since the restore, this one too (positive control of the
    # telemetry): one reconciled, and only rs-<who> resumed.
    lines=$(m6_reconciles "$who" "$(cat "$M6_WORK/restore-since-$who")")
    [ -n "$lines" ] || fail "$who's telemetry holds no tick since the restore, not even this one: the reconcile would prove nothing"
    n=$(grep -c '^restore epoch ' <<<"$lines" || true)
    [ "$n" = 1 ] || fail "$n tick(s) of $who reconciled since the restore, want one: $(paste -sd '|' <<<"$lines")"
    ep=$(grep '^restore epoch ' <<<"$lines")
    case $ep in
      "restore epoch $old -> $new: $want session(s) resume from the server"*) ;;
      *) fail "$who's reconcile: '$ep', want restore epoch $old -> $new: $want session(s) resume from the server (what its tick sent)" ;;
    esac
    printf '%s\n' "$ep" > "$M6_WORK/restore-epoch-$who"
  done
  m6_queue_wait
  for who in a b; do
    sid=rs-$who
    meta=$(m6_meta "$sid") || exit 1
    [ "$meta" = "$(cat "$M6_WORK/restore-$sid.meta")" ] || fail "conversation:$sid after the reconcile: $meta, before the restore: $(cat "$M6_WORK/restore-$sid.meta")"
    out=$(m6_uuid_counts "$sid") || exit 1
    [ "$out" = "$(printf '%s 1\n' "$sid-r1" "$sid-r2" "$sid-r3")" ] || fail "conversation:$sid holds the records $(paste -sd ' ' <<<"$out"), want r1 r2 r3 once each"
    [ "$(m6_cursor "$who" "$sid")" != "$(cat "$M6_WORK/restore-$sid.cursor")" ] || fail "$who's cursor of $sid did not move: it was not sent again"
    sid=tc-$who
    [ "$(m6_meta "$sid")" = "$(cat "$M6_WORK/restore-$sid.meta")" ] || fail "conversation:$sid, which the archive holds, changed: $(m6_meta "$sid"), was $(cat "$M6_WORK/restore-$sid.meta")"
    [ "$(m6_cursor "$who" "$sid")" = "$(cat "$M6_WORK/restore-$sid.cursor")" ] || fail "$who's cursor of $sid, which the archive holds, changed: it was sent again"
    [ "$(m6_doctor_status "$who" restore-epoch)" = ok ] || fail "$who's doctor restore-epoch is not ok after the reconcile"
    [ "$(m6_epoch "$who")" = "$new" ] || fail "$who did not record epoch $new (it holds $(m6_epoch "$who"))"
  done
  # Once: the next tick on each PC reconciles and sends nothing more.
  for who in a b; do
    cur=$(m6_cursor "$who" "rs-$who") || exit 1
    tick=$(m6_tick "$who") || exit 1
    [ -z "$(jq -r '.data.epoch // ""' <<<"$tick")" ] && [ "$(jq -r '.data.sessions.sent // 0' <<<"$tick")" = 0 ] ||
      fail "the next tick on $who reconciled or sent again: epoch '$(jq -r '.data.epoch // ""' <<<"$tick")', sessions $(jq -c .data.sessions <<<"$tick")"
    [ "$(m6_cursor "$who" "rs-$who")" = "$cur" ] || fail "$who's cursor of rs-$who moved on the next tick"
  done
  m6_queue_wait
  for who in a b; do
    [ "$(m6_meta "rs-$who")" = "$(cat "$M6_WORK/restore-rs-$who.meta")" ] || fail "conversation:rs-$who changed after the next tick: $(m6_meta "rs-$who")"
  done
  m6_result restore-across-clients PASS "the host restored an archive taken before rs-a and rs-b (epoch $old_a/$old_b -> $new): both doctors warned, each tick reconciled and re-sent its own lost session once (by A's $by_a, B's $by_b); tc-a, tc-b not sent again; next ticks sent nothing" \
    "$(jq -cn --argjson new "$new" --argjson a "$old_a" --argjson b "$old_b" --arg ea "$(cat "$M6_WORK/restore-epoch-a")" --arg eb "$(cat "$M6_WORK/restore-epoch-b")" --arg ba "$by_a" --arg bb "$by_b" \
      '{epoch: {a: $a, b: $b, new: $new}, reconcile: {a: $ea, b: $eb}, reconciled_by: {a: $ba, b: $bb}}')"
}

# purge-across-clients: A purges a project B holds a copy of; B learns the
# tombstone, and its later sends of that session never write the purged
# document again nor any record the purge covered: only its turn written
# after it goes out, to the document's successor (conversation:pg1#2).
m6_check_purge_across_clients() {
  local p=bs-m6-purge out rc=0 st cur counts facts who
  # The ticks below must send nothing: nothing an earlier check left may
  # go out with them (restore-across-clients settles them too, before it).
  m6_settle a b
  m6_session a pg1 "$p" 1 3 r 120
  m6_flush_sent a pg1 pg1-r3 > /dev/null
  m6_queue_wait
  m6_wait_doc pg1 > /dev/null
  # Positive control of the purge: pg1 produced facts, which it deletes.
  facts=$(m6_facts conversation:pg1) || exit 1
  [ "$facts" -gt 0 ] || fail "conversation:pg1 produced no fact: their purge would prove nothing"
  m6_copy a b pg1 "$p"
  out=$(m6_flush b pg1) || exit 1
  st=$(jq -r '[.data.results[]? | select(.session == "pg1") | .status][0] // "nothing"' <<<"$out")
  [ "$st" = nothing ] && jq -e '.ok == true' > /dev/null <<<"$out" || fail "B's flush of its copy of pg1: $st ($(jq -c '.error // .data.counts' <<<"$out" | m6_scrub)), want nothing sent"
  # Positive control: B holds pg1 as sent, up to the server's last record:
  # a later send of B's would append to conversation:pg1.
  cur=$(m6_cursor b pg1) || exit 1
  [ "$(jq -r '.last_uuid // ""' <<<"${cur:-null}")" = pg1-r3 ] || fail "B's cursor of pg1 is '$(jq -c . <<<"${cur:-null}")', want last_uuid pg1-r3: the check would prove nothing"
  out=$(m6_on a forget --json --project "$p" --purge --answer "confirm_project=$p" < /dev/null 2> "$M6_WORK/forget.err") || rc=$?
  out=$(m6_json "forget --purge on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] && jq -e '.ok and .data.action == "purged" and ([.data.documents[].id] | index("conversation:pg1") != null) and ((.data.note // "") | contains("not yet in bank") | not)' > /dev/null <<<"$out" ||
    fail "forget --project $p --purge on A: exit $rc ($(jq -c '.error.message // {action: .data.action, documents: [.data.documents[]?.id], note: .data.note}' <<<"$out" | m6_scrub))"
  [ -z "$(m6_doc conversation:pg1)" ] || fail "conversation:pg1 survived A's purge"
  m6_tombstoned conversation:pg1
  # B continues the session after the purge: its flush learns the
  # tombstone and sends the new turn alone, to the successor id.
  m6_session b pg1 "$p" 4 4 b 120
  out=$(m6_flush b pg1) || exit 1
  jq -e '.ok and ([.data.results[] | select(.session == "pg1" and .status == "sent")] | length == 1)' > /dev/null <<<"$out" ||
    fail "B's flush of pg1-b4 after the purge: $(jq -c '.data.results // .error' <<<"$out" | m6_scrub), want it sent"
  m6_get b "$(m6_paths b | jq -r .state)/state.json" | jq -e '[.tombstones[]?.document] | index("conversation:pg1") != null' > /dev/null ||
    fail "B's state.json holds no tombstone of conversation:pg1 after its flush"
  for who in b a; do
    out=$(m6_tick "$who") || exit 1
    [ "$(jq -r '.data.sessions.sent // 0' <<<"$out")" = 0 ] || fail "$who's tick after the purge sent $(jq -c .data.sessions <<<"$out")"
  done
  m6_queue_wait
  [ -z "$(m6_doc conversation:pg1)" ] || fail "conversation:pg1 is on the server again after B's sends: B did not honour A's purge"
  counts=$(m6_uuid_counts 'pg1#2') || exit 1
  [ "$counts" = "pg1-b4 1" ] || fail "conversation:pg1#2 holds the records $(paste -sd ' ' <<<"$counts"), want pg1-b4 alone, once (the purged r1-r3 never again)"
  [ "$(m6_facts conversation:pg1)" = 0 ] || fail "conversation:pg1's facts are on the server again after B's sends: $(m6_facts conversation:pg1)"
  m6_result purge-across-clients PASS "A purged project $p (conversation:pg1 deleted and tombstoned); B, holding a copy, learned the tombstone: its later flush and tick left conversation:pg1 absent (its $facts facts too) and sent only pg1-b4, to conversation:pg1#2; A's tick sent nothing" \
    "$(jq -cn --arg c "$counts" --argjson f "$facts" '{facts_before_purge: $f, successor: "conversation:pg1#2", successor_records: ($c | split("\n"))}')"
}

# timers (evaluated last): A's tick timer was active right after the
# install, and once 15 min have passed the timer has run the tick, which
# exited 0, after the install and after key-rotation's new key (real
# timers, decision 0073).
m6_check_timers() {
  local unit active at svc show status exit_at now deadline since after=0
  [ -s "$M6_WORK/timers" ] || fail "ssh-install recorded no timer"
  read -r unit active at < "$M6_WORK/timers"
  [ "$active" = active ] || fail "right after the install, $unit was $active"
  svc=${unit%.timer}.service
  # A tick between key-rotation's rotation and A's new key fails on the
  # old one: the run judged comes after both the install and that change.
  if [ -s "$M6_WORK/timers-after" ]; then read -r after < "$M6_WORK/timers-after"; fi
  since=$((after > at ? after : at))
  deadline=$((at + 20 * 60 > since + 16 * 60 ? at + 20 * 60 : since + 16 * 60))
  while :; do
    now=$(m6_sh a 'date +%s')
    show=$(m6_sh a "systemctl --user show $(m6_q "$svc") -p ExecMainStatus -p ExecMainExitTimestamp --timestamp=unix")
    status=$(sed -n 's/^ExecMainStatus=//p' <<<"$show") exit_at=$(sed -n 's/^ExecMainExitTimestamp=@//p' <<<"$show")
    if [ "$now" -ge $((at + 15 * 60)) ] && [ -n "$exit_at" ] && [ "$exit_at" -gt "$since" ]; then break; fi
    [ "$now" -lt "$deadline" ] || fail "$svc did not run after the install and the last key change within $(((deadline - at) / 60)) min ($(paste -sd ' ' <<<"$show"))"
    sleep "${M6_TIMERS_EVERY:-30}"
  done
  [ "$status" = 0 ] || fail "$svc's last run exited $status"
  m6_result timers PASS "$unit active after the install; $svc ran $((exit_at - at)) s after the install$([ "$after" -le 0 ] || printf " and %s s after key-rotation's new key" "$((exit_at - after))"), exit 0" \
    "$(jq -cn --arg u "$unit" --argjson after "$((exit_at - at))" '{unit: $u, ran_after_install_s: $after}')"
}
