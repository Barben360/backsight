# shellcheck shell=bash disable=SC2154 # bin, work and port are the caller's
# The restore chain shared by scripts/fresh-install.sh (CI) and
# scripts/restore-proof.sh (a real archive, on the author's machine): a
# synthetic session sent through the scratch install's own Claude Code
# profile, a restore that loses it, and the jobs tick that must send it again
# once (decision 0044, M5 block 1 in decision 0067).
#
# Sourced, never run. The caller sets: bin (the backsight binary), work (the
# run's directory), port (the scratch API port), BACKSIGHT_HOME and HOME (the
# scratch install and profile), and defines fail(). RESTORE_ARGS, when set,
# are extra arguments for `restore` (e.g. --identity <file>).

# The scratch install's bank and API key, read from its own files.
rc_toml() { sed -n "s/^$1 *= *[\"']\([^\"']*\)[\"'] *\$/\1/p" "$2" | head -n 1; } # rc_toml <key> <file>: a top-level string
rc_bank() { local b; b=$(rc_toml bank "$BACKSIGHT_HOME/config/config.toml"); echo "${b:-claude-memory}"; }
rc_key() { rc_toml api_key "$BACKSIGHT_HOME/config/secrets.toml"; }

# rc_session <sid>: a settled three-record session (two hours old, so the
# tick's 30-minute wait is over) in the scratch profile's transcripts.
rc_session() {
  local sid=$1 dir=$HOME/.claude/projects/-restore-proof cwd=$HOME/work/restore-proof i ts role content
  mkdir -p "$dir" "$cwd"
  : > "$dir/$sid.jsonl"
  for i in 1 2 3; do
    ts=$(date -u -d "-$((130 - i)) minutes" +%Y-%m-%dT%H:%M:%S.000Z)
    if [ "$i" = 2 ]; then
      role=assistant content="[{\"type\":\"text\",\"text\":\"Noted for $sid: the export pages by 200.\"}]"
    else
      role=user content="\"$sid turn $i: the export job of the restore proof failed at page $i.\""
    fi
    printf '{"type":"%s","uuid":"%s-r%d","sessionId":"%s","cwd":"%s","timestamp":"%s","message":{"role":"%s","content":%s}}\n' \
      "$role" "$sid" "$i" "$sid" "$cwd" "$ts" "$role" "$content" >> "$dir/$sid.jsonl"
  done
}

# rc_doc <sid>: the server's conversation document of sid; empty when the
# server answers 404. rc_session keeps every record in one directory, so the
# session's records all go to its home document conversation:<sid> (other
# projects' records would go to conversation:<sid>@<project>). Any other answer (401, 5xx, no server) fails the run:
# it must never read as "absent".
rc_doc() {
  local out code
  out=$(mktemp "$work/doc.XXXXXX")
  code=$(curl -sS -o "$out" -w '%{http_code}' -H "Authorization: Bearer $(rc_key)" \
    "http://127.0.0.1:$port/v1/default/banks/$(rc_bank)/documents/conversation%3A$1") || code=000
  case $code in
    200) cat "$out" ;;
    404) ;;
    *) rm -f "$out"; fail "reading document conversation:$1: HTTP $code" ;;
  esac
  rm -f "$out"
}

# rc_meta <sid>: what the server holds of sid, "segment records last_uuid";
# empty while the document is absent, "incomplete" when a field is missing
# (never three nulls that would match each other).
rc_meta() {
  local doc
  doc=$(rc_doc "$1") || exit 1
  [ -n "$doc" ] || return 0
  jq -r '.document_metadata // {} | if (.segment // "") != "" and (.records // "") != "" and (.last_uuid // "") != ""
    then "\(.segment) \(.records) \(.last_uuid)" else "incomplete" end' <<<"$doc"
}

# rc_wait <sid>: waits for the async retain to store sid's document with its
# metadata.
rc_wait() {
  local m _
  for _ in $(seq 1 100); do
    m=$(rc_meta "$1") || exit 1
    case $m in "" | incomplete) ;; *) echo "$m"; return 0 ;; esac
    sleep 3
  done
  fail "the server never stored session $1 (last read: '${m:-absent}')"
}

# rc_queue_wait: waits until the bank has no pending or processing operation
# (async retains): metadata read before that may lag what the server holds.
rc_queue_wait() {
  local n status total _
  for _ in $(seq 1 100); do
    n=0
    for status in pending processing; do
      total=$(curl -fsS -H "Authorization: Bearer $(rc_key)" \
        "http://127.0.0.1:$port/v1/default/banks/$(rc_bank)/operations?status=$status&limit=1" | jq -er '.total') ||
        fail "cannot read the bank's $status operations"
      n=$((n + total))
    done
    [ "$n" -eq 0 ] && return 0
    sleep 3
  done
  fail "the bank's queue never drained ($n operation(s) left)"
}

# rc_send <sid>: writes and sends a session now, as `retain flush --session`.
rc_send() {
  rc_session "$1"
  "$bin" retain flush --json --session "$1" > "$work/flush-$1.json" || { cat "$work/flush-$1.json" >&2; fail "retain flush of $1 failed"; }
  jq -e --arg s "$1" '.ok and ([.data.results[] | select(.session == $s and .status == "sent")] | length == 1)' "$work/flush-$1.json" >/dev/null ||
    { cat "$work/flush-$1.json" >&2; fail "session $1 was not sent"; }
  rc_wait "$1" >/dev/null
}

# rc_live <sid> <from> <to>: appends records r<from>..r<to>, timestamped now
# (after retain_since, which an earlier `jobs tick` recorded), and sends them
# through the live path: `retain flush` of every session, which keeps
# retain_since and only skips the 30-minute wait. Prints the server's
# metadata once the segment is stored.
rc_live() {
  local sid=$1 from=$2 to=$3 dir=$HOME/.claude/projects/-restore-proof cwd=$HOME/work/restore-proof i ts
  mkdir -p "$dir" "$cwd"
  for i in $(seq "$from" "$to"); do
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
    printf '{"type":"user","uuid":"%s-r%d","sessionId":"%s","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"%s live turn %d: the invoice export of the restore proof."}}\n' \
      "$sid" "$i" "$sid" "$cwd" "$ts" "$sid" "$i" >> "$dir/$sid.jsonl"
  done
  rc_flush_live "$sid" "$to"
}

# rc_flush_live <sid> <last record>: `retain flush` must send sid (and only
# sid's new records, as one segment), then the server holds it up to
# r<last record>.
rc_flush_live() {
  local m
  "$bin" retain flush --json > "$work/flush-live-$1.json" || { cat "$work/flush-live-$1.json" >&2; fail "retain flush of the live session $1 failed"; }
  jq -e --arg s "$1" '.ok and ([.data.results[] | select(.session == $s and .status == "sent")] | length == 1) and
    ([.data.results[] | select(.status == "sent")] | length == 1)' "$work/flush-live-$1.json" >/dev/null ||
    { cat "$work/flush-live-$1.json" >&2; fail "the live session $1 was not sent alone"; }
  rc_queue_wait
  m=$(rc_wait "$1")
  [ "${m##* }" = "$1-r$2" ] || fail "the server holds the live session $1 up to ${m##* }, want $1-r$2"
  echo "$m"
}

rc_check() { # rc_check <doctor.json> <check id>: prints its status
  jq -r --arg id "$2" '[.data.checks[] | select(.id == $id) | .status][0] // "missing"' "$1"
}

# rc_restore_resends <archive> <lost sid> [kept sid] [live sid]: <lost sid>
# was sent after <archive> was taken, <kept sid> (optional) before it, and
# <live sid> (optional) is a live-path session whose first segment the
# archive holds and whose second it lacks. Restores the archive, then
# asserts: the epoch went up, doctor's restore-epoch warns until a jobs
# tick, which reconciles and re-sends <lost sid> once and nothing of <kept
# sid>; the next live flush re-appends only <live sid>'s second segment;
# the server holds each session as before the restore (same segment,
# records and last record: nothing doubled, nothing short). Sets
# rc_epoch_before and rc_epoch_after.
rc_restore_resends() {
  local archive=$1 lost=$2 kept=${3:-} live=${4:-} before kept_meta kept_cursor="" lost_cursor tag live_before="" live_last="" resend=1 m
  tag=$(basename "$archive")
  before=$(rc_meta "$lost")
  [[ $before =~ ^[0-9]+\ [0-9]+\ $lost-r3$ ]] || fail "session $lost on the server before the restore: '$before', want its 3 records up to $lost-r3"
  if [ -n "$kept" ]; then
    kept_meta=$(rc_meta "$kept")
    kept_cursor=$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$kept.json")
    [[ $kept_meta =~ ^[0-9]+\ [0-9]+\ $kept-r3$ ]] || fail "session $kept on the server before the restore: '$kept_meta'"
  fi
  if [ -n "$live" ]; then
    live_before=$(rc_meta "$live")
    live_last=${live_before##* }
    [[ $live_before =~ ^2\ [0-9]+\ $live-r[0-9]+$ ]] || fail "live session $live on the server before the restore: '$live_before', want segment 2"
    resend=2
  fi
  lost_cursor=$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")
  rc_epoch_before=$(jq -r '.restore_epoch.epoch // 0' "$BACKSIGHT_HOME/state/state.json")

  # shellcheck disable=SC2086 # RESTORE_ARGS is a word list
  "$bin" restore --json ${RESTORE_ARGS:-} "$archive" > "$work/restore-$tag.json" || { cat "$work/restore-$tag.json" >&2; fail "restore of $tag failed"; }
  jq -e '.ok and (.data.rolled_back // false | not) and (.data.epoch.epoch > 0) and
    ([.data.steps[] | select(.status == "done") | .name] | index("counts") != null and index("recall") != null and index("epoch") != null)' \
    "$work/restore-$tag.json" >/dev/null || { cat "$work/restore-$tag.json" >&2; fail "restore of $tag did not pass its checks"; }
  rc_epoch_after=$(jq -r '.data.epoch.epoch' "$work/restore-$tag.json")
  [ "$rc_epoch_after" -gt "$rc_epoch_before" ] || fail "restore epoch $rc_epoch_after is not above the client's $rc_epoch_before"
  m=$(rc_meta "$lost")
  [ -z "$m" ] || fail "session $lost survived the restore ($m): the chain would prove nothing"
  if [ -n "$live" ]; then
    m=$(rc_meta "$live")
    [[ $m =~ ^1\ [0-9]+\ $live-r[0-9]+$ ]] || fail "live session $live after the restore: '$m', want the archive's first segment only"
  fi

  "$bin" doctor --json > "$work/doctor-stale-$tag.json" || true
  [ "$(rc_check "$work/doctor-stale-$tag.json" restore-epoch)" = warn ] ||
    { jq -c '.data.checks[] | select(.id == "restore-epoch")' "$work/doctor-stale-$tag.json" >&2; fail "doctor does not warn about the old restore epoch"; }

  # The restored queue first: reconcile waits for it.
  rc_queue_wait
  "$bin" jobs tick --json > "$work/tick-$tag.json" || { cat "$work/tick-$tag.json" >&2; fail "jobs tick after the restore failed"; }
  jq -e --argjson from "$rc_epoch_before" --argjson to "$rc_epoch_after" --argjson n "$resend" \
    '.ok and .data.status == "done" and (.data.epoch | startswith("restore epoch \($from) -> \($to): \($n) session")) and .data.sessions.sent == 1' "$work/tick-$tag.json" >/dev/null ||
    { cat "$work/tick-$tag.json" >&2; fail "jobs tick did not reconcile $resend session(s) and re-send exactly one"; }
  [ "$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")" != "$lost_cursor" ] || fail "session $lost's cursor did not move: not re-sent"
  if [ -n "$kept" ]; then
    [ "$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$kept.json")" = "$kept_cursor" ] || fail "session $kept, held by the backup, was sent again"
    m=$(rc_meta "$kept")
    [ "$m" = "$kept_meta" ] || fail "session $kept changed on the server: $m, was $kept_meta"
  fi
  rc_queue_wait
  m=$(rc_wait "$lost")
  [ "$m" = "$before" ] || fail "session $lost on the server: $m, before the restore: $before"
  # The live session waits for its 30 minutes in the tick; the next live
  # flush re-appends what the restore lost, and only that.
  if [ -n "$live" ]; then
    m=$(rc_flush_live "$live" "${live_last##*-r}")
    [ "$m" = "$live_before" ] || fail "live session $live on the server: $m, before the restore: $live_before"
  fi

  "$bin" doctor --json > "$work/doctor-$tag.json" || true
  [ "$(rc_check "$work/doctor-$tag.json" restore-epoch)" = ok ] ||
    { jq -c '.data.checks[] | select(.id == "restore-epoch")' "$work/doctor-$tag.json" >&2; fail "doctor's restore-epoch is not ok after the tick"; }
  [ "$(rc_check "$work/doctor-$tag.json" restore)" = ok ] ||
    { jq -c '.data.checks[] | select(.id == "restore")' "$work/doctor-$tag.json" >&2; fail "doctor reports an unfinished restore"; }
  [ "$(jq -r '.restore_epoch.epoch' "$BACKSIGHT_HOME/state/state.json")" = "$rc_epoch_after" ] || fail "the client did not record epoch $rc_epoch_after"

  # Once: the next tick sends nothing more. It must have run the sessions
  # (a skipped tick would send nothing either).
  lost_cursor=$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")
  "$bin" jobs tick --json > "$work/tick-again-$tag.json" || { cat "$work/tick-again-$tag.json" >&2; fail "the second jobs tick failed"; }
  jq -e '.ok and .data.status == "done" and (.data.sessions | type == "object" and (to_entries | map(.value) | add) >= 1) and
    (.data.sessions.sent // 0) == 0 and ((.data.epoch // "") == "")' "$work/tick-again-$tag.json" >/dev/null ||
    { cat "$work/tick-again-$tag.json" >&2; fail "the second jobs tick sent again or did not run"; }
  [ "$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")" = "$lost_cursor" ] || fail "session $lost's cursor moved on the second tick"
  rc_queue_wait
  m=$(rc_meta "$lost")
  [ "$m" = "$before" ] || fail "session $lost changed after the second tick: $m"
  if [ -n "$live" ]; then
    m=$(rc_meta "$live")
    [ "$m" = "$live_before" ] || fail "live session $live changed after the second tick: $m"
  fi
}
