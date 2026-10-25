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
set +e
"$bin" setup --resume --non-interactive --json > "$work/setup.json" 2> "$work/progress.jsonl"
code=$?
set -e
[ $code -eq 0 ] || { cat "$work/setup.json" >&2; fail "setup --resume: exit $code"; }
jq -e '.ok and ([.data.steps[] | select(.name == "verify")][0].status == "done")' "$work/setup.json" >/dev/null || fail "verify did not pass"
grep -q '"event":"progress"' "$work/progress.jsonl" || fail "no progress lines on stderr"
[ "$(claude_rules)" != "$rules_before" ] || fail "setup left settings.json as it was: the undo check below would prove nothing"

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
rc_restore_resends "$archive" restore-proof-lost restore-proof-kept restore-proof-live

# 9. Update (SPEC §11.7): a failing migration and a failing server smoke
# each roll back and leave the install working, then the update to the new
# signed build, server upgrade included, passes.
uc_proof

# 10. Undo unwires Claude Code, stops the server, and keeps config and data.
"$bin" setup --undo --json > "$work/undo.json" || fail "setup --undo failed"
if claude plugin list --json | jq -e 'map(.id) | index("backsight@backsight") != null' >/dev/null; then fail "the plugin survived undo"; fi
[ "$(claude_rules)" = "$rules_before" ] || { echo "before: $rules_before" >&2; echo "after:  $(claude_rules)" >&2; fail "undo left settings.json changed"; }
running=$(compose ps -q --status running) || fail "cannot list the install's containers"
[ -z "$running" ] || { compose ps >&2; fail "containers still running after undo"; }
[ -f "$BACKSIGHT_HOME/config/config.toml" ] || fail "undo removed config.toml"

# 11. Uninstall (SPEC §11.9): the plan changes nothing; the data goes only
# with the bank id; config, state, binaries and the server go, the backups
# stay. The shim runs the updated build.
shim=$BACKSIGHT_HOME/data/bin/current
tree() { find "$BACKSIGHT_HOME" "$HOME/.claude" \( -type f -o -type l \) -print0 | sort -z | xargs -0 sha256sum 2>/dev/null; }
tree_before=$(tree)
"$shim" uninstall --plan --delete-data --purge-local --json > "$work/uninstall-plan.json" || { cat "$work/uninstall-plan.json" >&2; fail "uninstall --plan failed"; }
[ "$(tree)" = "$tree_before" ] || fail "uninstall --plan changed files"
set +e
"$shim" uninstall --delete-data --purge-local --non-interactive --json > "$work/uninstall-noid.json"
code=$?
set -e
[ "$code" = 10 ] && jq -e '.error.questions | map(.id) | index("confirm_bank") != null' "$work/uninstall-noid.json" >/dev/null ||
  { cat "$work/uninstall-noid.json" >&2; fail "uninstall --delete-data without the bank id exited $code"; }
[ "$(tree)" = "$tree_before" ] || fail "an unconfirmed uninstall changed files"
bank=$(sed -n 's/^bank *= *"\(.*\)"/\1/p' "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
volume=$(sed -n 's/^name: *//p' "$BACKSIGHT_HOME/data/server/compose.yaml" | head -n 1)_pg_data
"$shim" uninstall --delete-data --purge-local --answer "confirm_bank=${bank:-claude-memory}" --answer identity_saved=yes --json > "$work/uninstall.json" ||
  { cat "$work/uninstall.json" >&2; fail "uninstall failed"; }
if docker volume inspect "$volume" >/dev/null 2>&1; then fail "the database volume $volume survived --delete-data"; fi
for p in "$BACKSIGHT_HOME/config" "$BACKSIGHT_HOME/state" "$BACKSIGHT_HOME/data/bin" "$BACKSIGHT_HOME/data/marketplace" "$BACKSIGHT_HOME/data/server/compose.yaml"; do
  [ ! -e "$p" ] || fail "uninstall left $p"
done
[ -s "$archive" ] || fail "uninstall removed the backup $archive"
echo "fresh-install: ok (setup with verify, store, search, doctor, backup, restore epoch $rc_epoch_before -> $rc_epoch_after with the lost session re-sent once and the live segment re-appended, update $uc_old -> $uc_new with its server upgrade after two rolled back ones, undo, uninstall) on project $project"
