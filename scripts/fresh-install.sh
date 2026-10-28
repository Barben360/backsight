#!/usr/bin/env bash
# The fresh-install proof (M4 block 4, plan 0063): backsight set up from
# nothing by `setup --non-interactive`, in an isolated HOME and Claude Code
# profile, on this machine's Docker with Hindsight's mock LLM, through to a
# stored memory found again, a backup restored and the session it lacked
# sent again (M5 block 1), then an update to a newer signed test build,
# after a failing one rolled back (M5 block 2). No hand edit: the only inputs are the answers,
# one `secret set` and the synthetic transcripts.
#
# Usage: scripts/fresh-install.sh [api port] (default 28400 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; the UI takes port + 1111). PROJECT names the Compose project and scheduler entries
# (default bs-fresh) so a machine's own install is left alone; KEEP=1
# leaves the stack and the work directory; WORK is the parent of the run's
# own directory (it must allow executables); only the run's is removed.
# BACKSIGHT_HF_CACHE (scripts/hf-cache.sh) seeds the server's model cache
# from a tar and saves it there.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/ports.sh
. "$root/scripts/ports.sh"
port=$(slot_port 28400 "${1:-}")
project=${PROJECT:-bs-fresh}
[ "$project" != backsight ] || { echo "PROJECT=backsight would be the machine's own install" >&2; exit 2; }
if [ -n "${WORK:-}" ]; then
  mkdir -p "$WORK"
  work=$(mktemp -d "$WORK/run.XXXXXX")
else
  work=$(mktemp -d)
fi
bin=$work/bin/backsight
# Built before HOME moves: Go's caches stay the caller's. The install's
# binary is the update proof's old test build (scripts/update-chain.sh).
# shellcheck source=scripts/update-chain.sh
. "$root/scripts/update-chain.sh"
uc_build "$bin"
# shellcheck source=scripts/hf-cache.sh
. "$root/scripts/hf-cache.sh"
export HOME=$work/home BACKSIGHT_HOME=$work/backsight
unset CLAUDE_CONFIG_DIR XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME
mkdir -p "$HOME"
compose() { # the project the install rendered, whatever PROJECT says now
  local f=$BACKSIGHT_HOME/data/server/compose.yaml p
  p=$(sed -n 's/^name: *//p' "$f" | head -n 1)
  [ -n "$p" ] && [ "$p" != backsight ] || { echo "refusing compose project '$p'" >&2; return 1; }
  docker compose -p "$p" -f "$f" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"
}
cleanup() {
  status=$?
  if [ $status -ne 0 ] && [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ]; then
    compose logs --tail 40 hindsight >&2 || true
  fi
  if [ "${KEEP:-}" != 1 ]; then
    [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ] && compose down -v >/dev/null 2>&1 || true
    [ -z "${BACKSIGHT_HF_CACHE:-}" ] || docker volume rm -f "${project}_hf_cache" >/dev/null 2>&1 || true # seeded, if setup never ran
    rm -rf "$work"
  fi
  exit $status
}
trap cleanup EXIT
fail() { echo "fresh-install: $*" >&2; exit 1; }
# What setup edits in the profile's settings.json and undo must put back:
# hooks, allow rules and the transcript retention ({} and [] count as absent).
claude_rules() {
  { cat "$HOME/.claude/settings.json" 2>/dev/null || echo '{}'; } |
    jq -cS '{hooks: (.hooks // {}), allow: (.permissions.allow // []), cleanupPeriodDays}'
}
rules_before=$(claude_rules)

# 1. An agent's first call: the questions, then the answers.
"$bin" setup --questions --json > "$work/questions.json"
jq -e '.ok and (.data.questions | map(.id) | index("provider") != null)' "$work/questions.json" >/dev/null || fail "no question list"

# 2. Without the key, setup stops with exit 10 and names the secret.
answers=(--answer provider=mock --answer model=mock-model --answer consent=yes --answer retention=3650
  --answer project="$project" --answer api_port="$port" --answer ui_port=$((port + 1111)) --answer scheduler=off)
set +e
"$bin" setup --non-interactive --json "${answers[@]}" > "$work/needs-key.json" 2>/dev/null
code=$?
set -e
[ $code -eq 10 ] || fail "setup without a key: exit $code, want 10"
jq -e '.error.secrets == ["llm_api_key"]' "$work/needs-key.json" >/dev/null || fail "exit 10 does not name llm_api_key"
[ ! -e "$BACKSIGHT_HOME/config/config.toml" ] || fail "config.toml written before the key"

# 3. The user types the key (here: the mock's placeholder), the agent resumes.
printf 'mock-not-a-key\n' | "$bin" secret set llm_api_key >/dev/null
hfc_seed "$project"
set +e
"$bin" setup --resume --non-interactive --json > "$work/setup.json" 2> "$work/progress.jsonl"
code=$?
set -e
[ $code -eq 0 ] || { cat "$work/setup.json" >&2; fail "setup --resume: exit $code"; }
jq -e '.ok and ([.data.steps[] | select(.name == "verify")][0].status == "done")' "$work/setup.json" >/dev/null || fail "verify did not pass"
grep -q '"event":"progress"' "$work/progress.jsonl" || fail "no progress lines on stderr"
[ "$(claude_rules)" != "$rules_before" ] || fail "setup left settings.json as it was: the undo check below would prove nothing"
hfc_save "$project"

# 4. The client's own paths reach the memory: a note is stored, and a
# search answers with hits. Hindsight's mock LLM "extracts" the lines of
# its own prompt, so the hits' content proves nothing here: recall of real
# content is verify's round trip (step 3) and the live runs with a real LLM.
"$bin" store --json --kind decision "The fresh-install proof stores this decision: billing keeps PostgreSQL 16 with partial indexes." > "$work/store.json" ||
  { cat "$work/store.json" >&2; fail "store failed"; }
jq -e '.ok and .data.status == "stored"' "$work/store.json" >/dev/null || { cat "$work/store.json" >&2; fail "the note was not stored"; }
for _ in $(seq 1 60); do
  "$bin" search --all --min-score 0 --json "Which PostgreSQL version does billing keep?" > "$work/search.json" 2>&1 || true
  if jq -e '.ok and (.data.hits | length > 0)' "$work/search.json" >/dev/null 2>&1; then
    found=1
    break
  fi
  sleep 3
done
[ "${found:-}" = 1 ] || { cat "$work/store.json" "$work/search.json" >&2; fail "search never answered with hits"; }

# 5. doctor finds nothing broken in the fresh install.
"$bin" doctor --json > "$work/doctor.json" || true
jq -e '(.data.checks | length > 0) and (.data.checks | map(select(.status == "error")) | length == 0)' "$work/doctor.json" >/dev/null ||
  { jq -c '.data.checks[]? | select(.status == "error")' "$work/doctor.json" >&2 || cat "$work/doctor.json" >&2; fail "doctor reports errors"; }

# 5b. The docs (decision 0069): setup indexed them into the docs bank with
# no LLM call (chunks only, like the control bank), and guide search
# answers a French question from it.
jq -e '[.data.steps[] | select(.name == "docs")][0].status == "done"' "$work/setup.json" >/dev/null || fail "setup did not index the docs"
"$bin" guide search --json "comment désinstaller backsight ?" > "$work/guide.json" || { cat "$work/guide.json" >&2; fail "guide search failed"; }
jq -e '.ok and .data.mode == "semantic" and (.data.hits[:3] | map(.ref) | any(startswith("uninstall")))' "$work/guide.json" >/dev/null ||
  { cat "$work/guide.json" >&2; fail "guide search did not answer from the docs bank with an uninstall section in its top 3"; }
mem_bank=$(sed -n "s/^bank *= *[\"']\([^\"']*\)[\"'] *\$/\1/p" "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
mem_bank=${mem_bank:-claude-memory}
docs_bank=$mem_bank-docs
# llm_calls <bank>: the LLM calls the server logged for bank today; every
# bucket must carry its total, or a renamed field would read as 0 calls.
llm_calls() {
  curl -fsS -H "Authorization: Bearer $(sed -n "s/^api_key *= *[\"']\([^\"']*\)[\"'] *\$/\1/p" "$BACKSIGHT_HOME/config/secrets.toml" | head -n 1)" \
    "http://127.0.0.1:$port/v1/default/banks/$1/llm-requests/stats?period=1d&call_type=llm" |
    jq -e 'if (.buckets | type) == "array" and (.buckets | all(has("total"))) then [.buckets[].total] | add // 0 else error("no bucket totals") end'
}
# Positive control: step 4's note was extracted by the mock LLM in the
# memory bank, so the count sees LLM calls (the stats lag a little).
for _ in $(seq 1 30); do
  mem_llm=$(llm_calls "$mem_bank") || fail "the LLM request stats of $mem_bank have no bucket totals"
  [ "$mem_llm" -gt 0 ] && break
  sleep 2
done
[ "$mem_llm" -gt 0 ] || fail "the memory bank $mem_bank logged no LLM call after an extracting retain: the docs bank's 0 below would prove nothing"
docs_llm=$(llm_calls "$docs_bank") || fail "the LLM request stats of $docs_bank have no bucket totals"
[ "$docs_llm" = 0 ] || fail "the docs bank $docs_bank logged $docs_llm LLM call(s): indexing must only embed"

# 6. A session goes out through the profile's transcripts before the
# backup: the backup holds it, so the restore below must leave it alone.
# A first jobs tick records retain_since: live retain takes the sessions
# started after it. The live session's first segment goes out before the
# backup, its second after it.
# shellcheck source=scripts/restore-chain.sh
. "$root/scripts/restore-chain.sh"
"$bin" jobs tick --json > "$work/tick-since.json" || { cat "$work/tick-since.json" >&2; fail "the first jobs tick failed"; }
jq -e '.ok and .data.status == "done"' "$work/tick-since.json" >/dev/null || { cat "$work/tick-since.json" >&2; fail "the first jobs tick did not run"; }
rc_send restore-proof-kept
rc_live restore-proof-live 1 2 >/dev/null

# 7. A backup of the real Postgres restores to the same row counts (SPEC §16).
"$bin" backup create --json > "$work/backup.json" || { cat "$work/backup.json" >&2; fail "backup create failed"; }
archive=$(jq -r 'select(.ok and (.data.skipped // "") == "") | .data.path // empty' "$work/backup.json")
[ -n "$archive" ] && [ -s "$archive" ] || { cat "$work/backup.json" >&2; fail "backup create wrote no archive"; }
"$bin" backup verify --json "$archive" > "$work/backup-verify.json" || { cat "$work/backup-verify.json" >&2; fail "backup verify failed"; }
jq -e '.ok and .data.ok and ([.data.restored[]] | add > 0)' "$work/backup-verify.json" >/dev/null ||
  { cat "$work/backup-verify.json" >&2; fail "the backup restored no rows"; }

# 8. Restore (decision 0044): a session sent after the backup is lost by
# restoring it; the next jobs tick reconciles to the new epoch and sends it
# again, once, while the one the backup holds is not sent again; the live
# session's second segment, lost too, is appended again, alone.
rc_send restore-proof-lost
rc_live restore-proof-live 3 4 >/dev/null

# 8a. A purge made after the backup survives the restore (M5 review rules):
# a session of its own project goes out, is purged with forget --project,
# and must not come back, neither from the restore nor from the ticks after.
purged=restore-proof-purged
rc_session "$purged" purge-proof
"$bin" retain flush --json --session "$purged" > "$work/flush-$purged.json" || { cat "$work/flush-$purged.json" >&2; fail "retain flush of $purged failed"; }
rc_wait "$purged" >/dev/null
rc_queue_wait # its facts extracted, tagged project:purge-proof
"$bin" forget --json --project purge-proof --purge --answer confirm_project=purge-proof > "$work/forget-purge.json" ||
  { cat "$work/forget-purge.json" >&2; fail "forget --project purge-proof --purge failed"; }
jq -e --arg d "conversation:$purged" '.ok and (.data.action == "purged") and ([.data.documents[].id] | index($d) != null) and ((.data.note // "") | contains("not yet in bank") | not)' \
  "$work/forget-purge.json" >/dev/null || { cat "$work/forget-purge.json" >&2; fail "the purge did not delete conversation:$purged and record it in the control bank"; }
[ -z "$(rc_meta "$purged")" ] || fail "conversation:$purged survived its purge"
jq -e --arg d "conversation:$purged" '[.tombstones[]?.document] | index($d) != null' "$BACKSIGHT_HOME/state/state.json" >/dev/null || fail "no tombstone for $purged in state.json"
"$bin" restore --plan --json "$archive" > "$work/restore-plan.json" || { cat "$work/restore-plan.json" >&2; fail "restore --plan failed"; }
jq -e --arg d "conversation:$purged" '([.data.purged_after_archive[]?.document] | index($d) != null) and ([.data.steps[].name] | index("purges") != null)' \
  "$work/restore-plan.json" >/dev/null || { cat "$work/restore-plan.json" >&2; fail "restore --plan does not list the purge made after the archive"; }

rc_restore_resends "$archive" restore-proof-lost restore-proof-kept restore-proof-live
# The restored control bank got the purge back, and the ticks sent nothing of it.
curl -fsS -H "Authorization: Bearer $(rc_key)" "http://127.0.0.1:$port/v1/default/banks/$(rc_bank)-control/documents/backsight%3Atombstones" |
  jq -e --arg d "conversation:$purged" '.document_metadata.tombstones | fromjson | map(.document) | index($d) != null' >/dev/null ||
  fail "the restored control bank lacks the purge of $purged"
[ -z "$(rc_meta "$purged")" ] || fail "the purged session $purged was sent again after the restore"
# A purged document id is never written again (issue 285): the session's
# later turns would go to conversation:<sid>#2, and nothing went there.
[ -z "$(rc_doc "$purged%232")" ] || fail "the purged session $purged was sent again after the restore, to conversation:$purged#2"
jq -e '(.segments // {}) | length == 0' "$BACKSIGHT_HOME/state/cursors/$purged.json" >/dev/null ||
  fail "the purged session $purged's cursor still counts segments of the purged document"

# 9. Update (SPEC §11.7): a failing migration and a failing server smoke
# each roll back and leave the install working, then the update to the new
# signed build, server upgrade included, passes.
uc_proof

# 9a. Exposure and the connection token (M6 block 1, decision 0073): the
# updated build opens the server on this machine's private address, hands
# out its token, keeps the server id across hands-out, and a rotation
# kills the old key. The token files hold the key: they are only checked
# for existence and never printed. With no private IPv4 (the source of
# the default route), the leg fails on CI and is skipped on a dev machine.
sv=$(uc_shim)
priv_ip=
if command -v ip >/dev/null; then
  cand=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1)
  case $cand in
    10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|100.6[4-9].*|100.[7-9][0-9].*|100.1[01][0-9].*|100.12[0-7].*) priv_ip=$cand ;;
  esac
fi
if [ -z "$priv_ip" ]; then
  # On CI a skip would turn the proof into a pass with no coverage.
  [ -z "${CI:-}${GITHUB_ACTIONS:-}" ] || fail "no private IPv4 on the default route (is ip installed?): the exposure leg cannot run on CI"
  echo "fresh-install: skipping the exposure leg: no private IPv4 on the default route" >&2
else
  health() { curl -fsS --max-time 10 "http://$1:$port/health" >/dev/null; } # health <ip>
  banks_status() { # banks_status <key>: the HTTP status of the banks list with that key
    curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $1" "http://127.0.0.1:$port/v1/default/banks"
  }
  # The commands' JSON never carries the key; a failure shows it anyway
  # with any api_key value masked.
  show() { sed 's/"api_key": *"[^"]*"/"api_key":"..."/g' "$1" >&2; }
  "$sv" server expose --private "$priv_ip" --answer cleartext_lan=yes --json --out "$work/token" > "$work/expose.json" ||
    { show "$work/expose.json"; fail "server expose --private $priv_ip failed"; }
  jq -e --arg ip "$priv_ip" '.ok and .data.exposure == "private" and .data.bind_ip == $ip' "$work/expose.json" >/dev/null ||
    { show "$work/expose.json"; fail "expose.json: want ok, exposure private, bind_ip $priv_ip"; }
  [ -s "$work/token" ] || fail "server expose wrote no token file"
  health "$priv_ip" || fail "/health does not answer on $priv_ip after expose --private"
  health 127.0.0.1 || fail "/health does not answer on 127.0.0.1 after expose --private"
  "$sv" server token --json --out "$work/token2" > "$work/token2.json" || { show "$work/token2.json"; fail "server token failed"; }
  "$sv" server token --json --out "$work/token2b" > "$work/token2b.json" || { show "$work/token2b.json"; fail "server token (second time) failed"; }
  sid=$(jq -r '.data.server_id // empty' "$work/token2.json")
  [ -n "$sid" ] && [ "$sid" = "$(jq -r '.data.server_id // empty' "$work/token2b.json")" ] || fail "server token handed out two different server ids"
  jq -e --arg ip "$priv_ip" '.ok and (.data.url | contains($ip)) and (.data.bank | length > 0) and (.data.written | length > 0) and (.data | has("api_key") | not)' "$work/token2.json" >/dev/null ||
    { show "$work/token2.json"; fail "token2.json: want ok, the private url, a bank, the file written and no key"; }
  [ -s "$work/token2" ] && [ -s "$work/token2b" ] || fail "server token wrote no token file"
  # The key stays in these variables: never echoed, never traced.
  { set +x; } 2>/dev/null
  old_key=$(rc_key)
  [ -n "$old_key" ] || fail "no api_key in the isolated secrets.toml"
  [ "$(banks_status "$old_key")" = 200 ] || fail "the banks list refuses the current key before the rotation"
  "$sv" server token --rotate --json --out "$work/token3" > "$work/token3.json" || { show "$work/token3.json"; fail "server token --rotate failed"; }
  [ -s "$work/token3" ] || fail "server token --rotate wrote no token file"
  new_key=$(rc_key)
  [ -n "$new_key" ] && [ "$new_key" != "$old_key" ] || fail "the rotation left the same api_key in secrets.toml"
  [ "$(banks_status "$old_key")" = 401 ] || fail "the old key still works after the rotation (want 401)"
  [ "$(banks_status "$new_key")" = 200 ] || fail "the new key is refused after the rotation (want 200)"
  [ "$(jq -r '.data.server_id' "$work/token3.json")" = "$sid" ] || fail "the rotation changed the server id"
  unset old_key new_key
  # 9a'. The client side (M6 block 2): a second PC, with its own HOME,
  # BACKSIGHT_HOME and Claude Code profile, joins this server with the
  # rotated token. scheduler=off: no entry named like the user's own install.
  # Its key stays in its secrets.toml, never printed.
  bank_docs() { # the host bank's document count; the key stays inside
    local k
    { set +x; } 2>/dev/null
    k=$(rc_key)
    curl -fsS --max-time 10 -H "Authorization: Bearer $k" "http://127.0.0.1:$port/v1/default/banks/$(rc_bank)/stats" | jq -er '.total_documents'
  }
  c2() { HOME=$work/home2 BACKSIGHT_HOME=$work/backsight2 "$@"; }
  mkdir -p "$work/home2"
  docs_before=$(bank_docs) || fail "cannot read the host bank's document count"
  c2 "$sv" connect --token-file "$work/token3" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json > "$work/connect.json" 2> "$work/connect.err" || { show "$work/connect.json"; show "$work/connect.err"; fail "connect with the rotated token failed"; }
  jq -e '.ok' "$work/connect.json" >/dev/null || { show "$work/connect.json"; fail "connect.json: want ok"; }
  c2 "$sv" doctor --json > "$work/doctor2.json" || true
  jq -e '(.data.checks | map(select(.id == "server-id"))[0].status == "ok") and (.data.checks | map(select(.status == "error")) | length == 0)' "$work/doctor2.json" >/dev/null ||
    { jq -c '.data.checks[]? | select(.status == "error" or .id == "server-id")' "$work/doctor2.json" >&2; fail "the client's doctor: want server-id ok and no error"; }
  c2 "$sv" store --json --kind decision "The client proof stores this decision: the second PC reads the host's memory." > "$work/store2.json" ||
    { show "$work/store2.json"; fail "the client's store failed"; }
  jq -e '.ok and .data.status == "stored"' "$work/store2.json" >/dev/null || { show "$work/store2.json"; fail "the client's note was not stored"; }
  for _ in $(seq 1 30); do
    docs_after=$(bank_docs) || fail "cannot read the host bank's document count"
    [ "$docs_after" -gt "$docs_before" ] && break
    sleep 2
  done
  [ "$docs_after" -eq $((docs_before + 1)) ] || fail "the host bank holds $docs_after documents, want $((docs_before + 1)) (before the client's note: $docs_before)"
  # The pre-rotation token's key is dead: connect refuses it and changes nothing.
  cp "$work/backsight2/config/secrets.toml" "$work/secrets2.before"
  set +e
  c2 "$sv" connect --token-file "$work/token2" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json > "$work/connect-old.json" 2> "$work/connect-old.err"
  code=$?
  set -e
  [ $code -eq 20 ] || { show "$work/connect-old.json"; fail "connect with the pre-rotation token: exit $code, want 20"; }
  cmp -s "$work/secrets2.before" "$work/backsight2/config/secrets.toml" || fail "the refused connect changed the client's secrets.toml"
  "$sv" server expose --loopback --json > "$work/expose-loopback.json" || { show "$work/expose-loopback.json"; fail "server expose --loopback failed"; }
  health 127.0.0.1 || fail "/health does not answer on 127.0.0.1 after expose --loopback"
  if health "$priv_ip"; then fail "/health still answers on $priv_ip after expose --loopback"; fi
  "$sv" server status --json > "$work/status-loopback.json" || { cat "$work/status-loopback.json" >&2; fail "the server is not healthy after closing the exposure"; }
fi

# 10. Undo unwires Claude Code, stops the server, and keeps config and data.
"$bin" setup --undo --json > "$work/undo.json" || fail "setup --undo failed"
if claude plugin list --json | jq -e 'map(.id) | index("backsight@backsight") != null' >/dev/null; then fail "the plugin survived undo"; fi
[ "$(claude_rules)" = "$rules_before" ] || { echo "before: $rules_before" >&2; echo "after:  $(claude_rules)" >&2; fail "undo left settings.json changed"; }
running=$(compose ps -q --status running) || fail "cannot list the install's containers"
[ -z "$running" ] || { compose ps >&2; fail "containers still running after undo"; }
[ -f "$BACKSIGHT_HOME/config/config.toml" ] || fail "undo removed config.toml"

# 10a. Setting up again over kept data (M5 review rules): uninstall
# --stop --purge-local keeps the server's volume and its .env, and deletes
# secrets.toml. setup with the build the update installed adopts the
# database password and the keys from that .env, reaches step 4's memory,
# and its client install makes the PATH command uninstall removed.
new=$work/new/backsight
env_password() { sed -n "s/^BACKSIGHT_DB_PASSWORD='\(.*\)'$/\1/p" "$BACKSIGHT_HOME/data/server/.env"; }
kept_password=$(env_password)
[ -n "$kept_password" ] || fail "no database password in the server's .env before the purge"
"$new" uninstall --stop --purge-local --non-interactive --answer identity_saved=yes --answer drop_queued=yes --json > "$work/uninstall-keep.json" ||
  { cat "$work/uninstall-keep.json" >&2; fail "uninstall --stop --purge-local failed"; }
[ ! -e "$BACKSIGHT_HOME/config/secrets.toml" ] && [ ! -e "$(uc_path)" ] || fail "uninstall --purge-local kept secrets.toml or the PATH command"
[ "$(env_password)" = "$kept_password" ] || fail "uninstall --purge-local did not keep the server's .env"
[ "$(claude_rules)" = "$rules_before" ] || { echo "before: $rules_before" >&2; echo "after:  $(claude_rules)" >&2; fail "uninstall --purge-local left settings.json changed"; }
printf 'mock-not-a-key\n' | "$new" secret set llm_api_key >/dev/null
"$new" setup --non-interactive --json "${answers[@]}" --answer backfill=no > "$work/setup-again.json" 2>/dev/null ||
  { cat "$work/setup-again.json" >&2; fail "setup over the kept data failed"; }
jq -e '.ok and ([.data.steps[] | select(.name == "verify")][0].status == "done")' "$work/setup-again.json" >/dev/null || fail "verify did not pass after setting up again"
grep -qxF "db_password = '$kept_password'" "$BACKSIGHT_HOME/config/secrets.toml" ||
  grep -qxF "db_password = \"$kept_password\"" "$BACKSIGHT_HOME/config/secrets.toml" || fail "setup did not adopt the kept database password"
"$new" search --all --min-score 0 --json "Which PostgreSQL version does billing keep?" > "$work/search-again.json" 2>&1 || true
jq -e '.ok and (.data.hits | length > 0)' "$work/search-again.json" >/dev/null || { cat "$work/search-again.json" >&2; fail "the kept memory is not found after setting up again"; }
[ -L "$(uc_path)" ] && [ "$(readlink "$(uc_path)")" = "$(uc_shim)" ] || fail "client install did not make the PATH command $(uc_path)"
# Undone again, so 10b and 11 start from the state step 10 left.
"$new" setup --undo --json > "$work/undo-again.json" || { cat "$work/undo-again.json" >&2; fail "setup --undo after setting up again failed"; }
[ "$(claude_rules)" = "$rules_before" ] || { echo "before: $rules_before" >&2; echo "after:  $(claude_rules)" >&2; fail "the second undo left settings.json changed"; }

# 10b. Wired again by hand after undo (decision 0067 block 3), so that
# uninstall's own client and scheduler removal is checked, not skipped:
# the client (plugin and permission rule) and the jobs entries, through
# the shim, which runs the updated build. The run has no systemd user
# session (the runner is a system service, a hosted runner has none
# either) and the machine's own user manager must stay untouched: from
# here on a stand-in systemctl answers every call and records it. The
# unit files are the real ones, in the isolated HOME; their activation by
# a real user manager is not part of this proof.
shim=$BACKSIGHT_HOME/data/bin/current
link=$(uc_path)
mkdir -p "$work/stand-in"
sysctl_log=$work/systemctl.log
cat > "$work/stand-in/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> '$sysctl_log'
EOF
chmod +x "$work/stand-in/systemctl"
: > "$sysctl_log"
PATH=$work/stand-in:$PATH
# sysctl_since <line>: the calls that change the scheduler's state since
# that line of the log (queries such as show-environment are left out).
sysctl_since() { tail -n +"$1" "$sysctl_log" | grep -E -- '(^| )(enable|disable|start|stop|restart|kill|daemon-reload|reset-failed)( |$)' || true; }
sysctl_mark() { echo $(($(wc -l < "$sysctl_log") + 1)); }
plugin_wired() { claude plugin list --json | jq -e 'map(.id) | index("backsight@backsight") != null' >/dev/null; }
# rule_wired: exit 0 with the rule, 1 without it (no settings.json
# counts as without), and the run fails on a settings.json jq cannot read.
rule_wired() {
  local f=$HOME/.claude/settings.json
  [ -e "$f" ] || return 1
  jq -e . "$f" >/dev/null || fail "$f is not JSON"
  jq -e '(.permissions.allow // []) | map(select(startswith("mcp__plugin_backsight"))) | length > 0' "$f" >/dev/null
}
"$shim" client install --json > "$work/client-again.json" || { cat "$work/client-again.json" >&2; fail "client install after undo failed"; }
# The entries' files must land in the isolated HOME: planned first.
"$shim" jobs install --plan --json > "$work/jobs-plan.json" || { cat "$work/jobs-plan.json" >&2; fail "jobs install --plan failed"; }
mapfile -t unit_files < <(jq -r '.data.files[].target' "$work/jobs-plan.json")
[ "${#unit_files[@]}" -ge 4 ] || { cat "$work/jobs-plan.json" >&2; fail "jobs install plans ${#unit_files[@]} entry files, want the tick and maintain timers and services"; }
for f in "${unit_files[@]}"; do
  case $f in "$HOME"/*) ;; *) fail "jobs install would write $f, outside the isolated HOME" ;; esac
done
"$shim" jobs install --json > "$work/jobs-again.json" || { cat "$work/jobs-again.json" >&2; fail "jobs install after undo failed"; }
# Positive controls: each piece uninstall must remove is there now.
plugin_wired || fail "client install after undo did not install the plugin"
rule_wired || fail "client install after undo wrote no mcp__plugin_backsight permission rule"
for f in "${unit_files[@]}"; do
  [ -f "$f" ] || fail "jobs install after undo did not write $f"
done
units=$(dirname "${unit_files[0]}")
grep -q -- "enable.*$project-tick\.timer" <<< "$(sysctl_since 1)" || { cat "$sysctl_log" >&2; fail "jobs install did not enable the $project timers"; }
[ -L "$link" ] && [ "$(readlink "$link")" = "$shim" ] || fail "the PATH command $link is not a link to the shim before uninstall"

# 11. Uninstall (SPEC §11.9): the plan changes nothing; the data goes only
# with the bank id; config, state, binaries, the PATH link, the plugin,
# its rule, the jobs entries and the server go, the backups stay.
# tree hashes every file uninstall may touch: one it cannot read fails the
# run rather than dropping out of the comparison.
tree() { find "$BACKSIGHT_HOME" "$HOME/.claude" "$units" \( -type f -o -type l \) -print0 | sort -z | xargs -0 -r sha256sum; }
tree_now() { tree || fail "cannot hash every file of the install (above)"; }
tree_before=$(tree_now)
# The backups as they are now: earlier archives may have been pruned by
# the update's server upgrade (backup retention), every one left must stay.
backups_dir=$BACKSIGHT_HOME/data/backups
backups_before=$(cd "$backups_dir" && find . -type f -print0 | sort -z | xargs -0 -r sha256sum)
[ -n "$backups_before" ] || fail "no backup left before uninstall: the check below would prove nothing"
volume=$(sed -n 's/^name: *//p' "$BACKSIGHT_HOME/data/server/compose.yaml" | head -n 1)_pg_data
docker volume inspect "$volume" >/dev/null || fail "no database volume $volume before uninstall: the --delete-data check would prove nothing"
cache=${volume%_pg_data}_hf_cache
docker volume inspect "$cache" >/dev/null || fail "no model cache volume $cache before uninstall: the --delete-data check would prove nothing"
mark=$(sysctl_mark)
"$shim" uninstall --plan --delete-data --purge-local --json > "$work/uninstall-plan.json" || { cat "$work/uninstall-plan.json" >&2; fail "uninstall --plan failed"; }
[ "$(tree_now)" = "$tree_before" ] || fail "uninstall --plan changed files"
[ -z "$(sysctl_since "$mark")" ] || { sysctl_since "$mark" >&2; fail "uninstall --plan changed the scheduler"; }
mark=$(sysctl_mark)
set +e
"$shim" uninstall --delete-data --purge-local --non-interactive --json > "$work/uninstall-noid.json"
code=$?
set -e
[ "$code" = 10 ] && jq -e '.error.questions | map(.id) | index("confirm_bank") != null' "$work/uninstall-noid.json" >/dev/null ||
  { cat "$work/uninstall-noid.json" >&2; fail "uninstall --delete-data without the bank id exited $code"; }
[ "$(tree_now)" = "$tree_before" ] || fail "an unconfirmed uninstall changed files"
[ -z "$(sysctl_since "$mark")" ] || { sysctl_since "$mark" >&2; fail "an unconfirmed uninstall changed the scheduler"; }
docker volume inspect "$volume" >/dev/null || fail "an unconfirmed uninstall removed the database volume $volume"
docker volume inspect "$cache" >/dev/null || fail "an unconfirmed uninstall removed the model cache volume $cache"
plugin_wired || fail "an unconfirmed uninstall removed the plugin"
bank=$(sed -n 's/^bank *= *"\(.*\)"/\1/p' "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
mark=$(sysctl_mark)
"$shim" uninstall --delete-data --purge-local --answer "confirm_bank=${bank:-claude-memory}" --answer identity_saved=yes --json > "$work/uninstall.json" ||
  { cat "$work/uninstall.json" >&2; fail "uninstall failed"; }
jq -e --arg backups "$backups_dir" '.ok and .data.server == "delete-data"
    and (.data.steps | length > 0) and all(.data.steps[]; .status == "done")
    and any(.data.kept[]; contains($backups))' "$work/uninstall.json" >/dev/null ||
  { jq -c '.ok, .data.server, [.data.steps[] | {name, status}], .data.kept' "$work/uninstall.json" >&2; fail "uninstall.json: want ok, server delete-data, every step done, the backups kept"; }
if docker volume inspect "$volume" >/dev/null 2>&1; then fail "the database volume $volume survived --delete-data"; fi
if docker volume inspect "$cache" >/dev/null 2>&1; then fail "the model cache volume $cache survived --delete-data"; fi
if plugin_wired; then fail "the plugin survived uninstall"; fi
if rule_wired; then fail "uninstall left the mcp__plugin_backsight permission rule"; fi
[ "$(claude_rules)" = "$rules_before" ] || { echo "before: $rules_before" >&2; echo "after:  $(claude_rules)" >&2; fail "uninstall left settings.json changed"; }
for f in "${unit_files[@]}"; do
  [ ! -e "$f" ] || fail "uninstall left the jobs entry $f"
done
grep -q -- "disable.*$project-tick\.timer" <<< "$(sysctl_since "$mark")" || { cat "$sysctl_log" >&2; fail "uninstall did not disable the $project timers"; }
# Then the services, once their timers are off (a running job is waited for
# through its lock first).
grep -q -- "stop .*$project-tick\.service.*$project-maintain\.service" <<< "$(sysctl_since "$mark")" || { cat "$sysctl_log" >&2; fail "uninstall did not stop the $project services"; }
[ "$(sysctl_since "$mark" | grep -n -- "disable" | head -n 1 | cut -d: -f1)" -lt "$(sysctl_since "$mark" | grep -n -- "stop " | head -n 1 | cut -d: -f1)" ] ||
  { cat "$sysctl_log" >&2; fail "uninstall stopped the $project services before disabling their timers"; }
[ ! -e "$link" ] && [ ! -L "$link" ] || fail "uninstall left the PATH command $link"
for p in "$BACKSIGHT_HOME/config" "$BACKSIGHT_HOME/state" "$BACKSIGHT_HOME/data/bin" "$BACKSIGHT_HOME/data/marketplace" "$BACKSIGHT_HOME/data/server/compose.yaml"; do
  [ ! -e "$p" ] || fail "uninstall left $p"
done
backups_after=$(cd "$backups_dir" && find . -type f -print0 | sort -z | xargs -0 -r sha256sum)
[ "$backups_after" = "$backups_before" ] || { echo "before: $backups_before" >&2; echo "after:  $backups_after" >&2; fail "uninstall changed the backups"; }
echo "fresh-install: ok (setup with verify, store, search, doctor, backup, restore epoch $rc_epoch_before -> $rc_epoch_after with the lost session re-sent once, the live segment re-appended and a purge kept, update $uc_old -> $uc_new with its server upgrade after two rolled back ones, ${priv_ip:+exposure on $priv_ip with token and rotation, }undo, set up again over kept data, client and jobs wired again, uninstall) on project $project"
