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

# rc_doc <sid>: the server's conversation document of sid; empty when absent.
rc_doc() {
  curl -fsS -H "Authorization: Bearer $(rc_key)" \
    "http://127.0.0.1:$port/v1/default/banks/$(rc_bank)/documents/conversation%3A$1" 2>/dev/null || true
}

# rc_meta <sid>: what the server holds of sid, "segment records last_uuid";
# empty while the document is absent.
rc_meta() { rc_doc "$1" | jq -r 'select(.id != null) | .document_metadata | "\(.segment) \(.records) \(.last_uuid)"' 2>/dev/null || true; }

# rc_wait <sid>: waits for the async retain to store sid's document.
rc_wait() {
  local m _
  for _ in $(seq 1 100); do
    m=$(rc_meta "$1")
    [ -n "$m" ] && { echo "$m"; return 0; }
    sleep 3
  done
  fail "the server never stored session $1"
}

# rc_send <sid>: writes and sends a session now, as `retain flush --session`.
rc_send() {
  rc_session "$1"
  "$bin" retain flush --json --session "$1" > "$work/flush-$1.json" || { cat "$work/flush-$1.json" >&2; fail "retain flush of $1 failed"; }
  jq -e --arg s "$1" '.ok and ([.data.results[] | select(.session == $s and .status == "sent")] | length == 1)' "$work/flush-$1.json" >/dev/null ||
    { cat "$work/flush-$1.json" >&2; fail "session $1 was not sent"; }
  rc_wait "$1" >/dev/null
}

rc_check() { # rc_check <doctor.json> <check id>: prints its status
  jq -r --arg id "$2" '[.data.checks[] | select(.id == $id) | .status][0] // "missing"' "$1"
}

# rc_restore_resends <archive> <lost sid> [kept sid]: <lost sid> was sent
# after <archive> was taken, <kept sid> (optional) before it. Restores the
# archive, then asserts: the epoch went up, doctor's restore-epoch warns
# until a jobs tick, which re-sends <lost sid> once and nothing of <kept
# sid>, and the server holds <lost sid> as before the restore (same segment
# and records: not doubled). Sets rc_epoch_before and rc_epoch_after.
rc_restore_resends() {
  local archive=$1 lost=$2 kept=${3:-} before kept_meta kept_cursor="" lost_cursor tag
  tag=$(basename "$archive")
  before=$(rc_meta "$lost")
  [ -n "$before" ] || fail "session $lost is not on the server before the restore"
  [ -z "$kept" ] || { kept_meta=$(rc_meta "$kept"); kept_cursor=$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$kept.json"); }
  lost_cursor=$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")
  rc_epoch_before=$(jq -r '.restore_epoch.epoch // 0' "$BACKSIGHT_HOME/state/state.json")

  # shellcheck disable=SC2086 # RESTORE_ARGS is a word list
  "$bin" restore --json ${RESTORE_ARGS:-} "$archive" > "$work/restore-$tag.json" || { cat "$work/restore-$tag.json" >&2; fail "restore of $tag failed"; }
  jq -e '.ok and (.data.rolled_back // false | not) and (.data.epoch.epoch > 0) and
    ([.data.steps[] | select(.status == "done") | .name] | index("counts") != null and index("recall") != null and index("epoch") != null)' \
    "$work/restore-$tag.json" >/dev/null || { cat "$work/restore-$tag.json" >&2; fail "restore of $tag did not pass its checks"; }
  rc_epoch_after=$(jq -r '.data.epoch.epoch' "$work/restore-$tag.json")
  [ "$rc_epoch_after" -gt "$rc_epoch_before" ] || fail "restore epoch $rc_epoch_after is not above the client's $rc_epoch_before"
  [ -z "$(rc_meta "$lost")" ] || fail "session $lost survived the restore: the chain would prove nothing"

  "$bin" doctor --json > "$work/doctor-stale-$tag.json" || true
  [ "$(rc_check "$work/doctor-stale-$tag.json" restore-epoch)" = warn ] ||
    { jq -c '.data.checks[] | select(.id == "restore-epoch")' "$work/doctor-stale-$tag.json" >&2; fail "doctor does not warn about the old restore epoch"; }

  "$bin" jobs tick --json > "$work/tick-$tag.json" || { cat "$work/tick-$tag.json" >&2; fail "jobs tick after the restore failed"; }
  jq -e --argjson from "$rc_epoch_before" --argjson to "$rc_epoch_after" \
    '.ok and (.data.epoch | startswith("restore epoch \($from) -> \($to): 1 session")) and .data.sessions.sent == 1' "$work/tick-$tag.json" >/dev/null ||
    { cat "$work/tick-$tag.json" >&2; fail "jobs tick did not reconcile and re-send exactly one session"; }
  [ "$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$lost.json")" != "$lost_cursor" ] || fail "session $lost's cursor did not move: not re-sent"
  if [ -n "$kept" ]; then
    [ "$(jq -cS . "$BACKSIGHT_HOME/state/cursors/$kept.json")" = "$kept_cursor" ] || fail "session $kept, held by the backup, was sent again"
    [ "$(rc_meta "$kept")" = "$kept_meta" ] || fail "session $kept changed on the server: $(rc_meta "$kept"), was $kept_meta"
  fi
  [ "$(rc_wait "$lost")" = "$before" ] || fail "session $lost on the server: $(rc_meta "$lost"), before the restore: $before"

  "$bin" doctor --json > "$work/doctor-$tag.json" || true
  [ "$(rc_check "$work/doctor-$tag.json" restore-epoch)" = ok ] ||
    { jq -c '.data.checks[] | select(.id == "restore-epoch")' "$work/doctor-$tag.json" >&2; fail "doctor's restore-epoch is not ok after the tick"; }
  [ "$(jq -r '.restore_epoch.epoch' "$BACKSIGHT_HOME/state/state.json")" = "$rc_epoch_after" ] || fail "the client did not record epoch $rc_epoch_after"

  # Once: the next tick sends nothing more.
  "$bin" jobs tick --json > "$work/tick-again-$tag.json" || { cat "$work/tick-again-$tag.json" >&2; fail "the second jobs tick failed"; }
  jq -e '.ok and ((.data.sessions.sent // 0) == 0) and ((.data.epoch // "") == "")' "$work/tick-again-$tag.json" >/dev/null ||
    { cat "$work/tick-again-$tag.json" >&2; fail "the second jobs tick sent again"; }
  [ "$(rc_meta "$lost")" = "$before" ] || fail "session $lost changed after the second tick: $(rc_meta "$lost")"
}
