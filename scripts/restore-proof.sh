#!/usr/bin/env bash
# The restore proof on the author's machine (M5 block 1, decision 0067): the
# newest real archive of the live install is restored into a scratch stack
# (its own Compose project, ports, HOME and Claude Code profile, Hindsight's
# mock LLM so nothing is billed). It must pass restore's checks (counts,
# health, recall) with the manifest's counts, and its client must reconcile:
# a synthetic session sent after a first restore is lost by restoring the
# same archive again, and the next jobs tick sends it again, once.
#
# The live install is only read: `backsight paths` and `backup list` of its
# binary, its newest archive (copied) and the age identity in its
# secrets.toml (copied to a 0600 file). Both copies, and everything the
# scratch install writes (the archive's decrypted files included), live in
# one 0700 directory removed on exit with the scratch stack.
#
# Usage: scripts/restore-proof.sh [api port] (default 48888; the UI takes
# port + 1111). PROJECT names the scratch Compose project (default
# bs-restore-proof); LIVE is the live install's binary (default
# ~/.local/share/backsight/bin/current); WORK is the parent of the run's own
# directory (must allow executables; default ~/.cache/backsight-restore-proof).
# KEEP=1 leaves the stack and the directory (the copied identity included).
set -euo pipefail
port=${1:-48888}
ui_port=$((port + 1111))
project=${PROJECT:-bs-restore-proof}
root=$(cd "$(dirname "$0")/.." && pwd)
real_home=$HOME
live=${LIVE:-$real_home/.local/share/backsight/bin/current}
fail() { echo "restore-proof: $*" >&2; exit 1; }
[ "$project" != backsight ] || fail "PROJECT=backsight is the live install"

# 0. The live install, read only: where it is, its newest archive.
"$live" paths --json > /dev/null || fail "cannot run the live binary $live"
live_secrets=$("$live" paths --json | jq -r '.data.secrets_file')
live_url=$(sed -n "s/^url *= *[\"']\([^\"']*\)[\"'].*/\1/p" "$("$live" paths --json | jq -r '.data.config_file')" | head -n 1)
live_url=${live_url:-http://127.0.0.1:8888}
for p in "$port" "$ui_port"; do
  case "$live_url" in *":$p" | *":$p/"*) fail "port $p is the live server's" ;; esac
  ! curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$p/" || fail "port $p is already in use"
done
# A project already in use would be taken down with its volumes on exit.
in_use=$(docker ps -aq --filter "label=com.docker.compose.project=$project"; docker volume ls -q --filter "label=com.docker.compose.project=$project") ||
  fail "cannot list Docker's containers and volumes"
[ -z "$in_use" ] || fail "Compose project $project already has containers or volumes: pick another PROJECT, or remove it yourself (docker compose -p $project down -v)"
newest=$("$live" backup list --json | jq -c '.data[0] // empty')
[ -n "$newest" ] || fail "the live install has no backup"
src=$(jq -r '.path' <<<"$newest")
manifest=$(jq -c '.manifest' <<<"$newest")
[ "$(jq -r '.format' <<<"$manifest")" -ge 1 ] || fail "$src has no manifest beside it"

base=${WORK:-$real_home/.cache/backsight-restore-proof}
mkdir -p "$base"
work=$(mktemp -d "$base/run.XXXXXX")
chmod 700 "$work"
bin=$work/bin/backsight
mkdir -p "$work/bin" "$work/home" "$work/live"
compose() { # the project the scratch install rendered, never the live one
  local f=$BACKSIGHT_HOME/data/server/compose.yaml p
  p=$(sed -n 's/^name: *//p' "$f" | head -n 1)
  [ -n "$p" ] && [ "$p" = "$project" ] || { echo "refusing compose project '$p'" >&2; return 1; }
  docker compose -p "$p" -f "$f" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"
}
cleanup() {
  status=$?
  if [ $status -ne 0 ] && [ -f "${BACKSIGHT_HOME:-/nonexistent}/data/server/compose.yaml" ]; then
    compose logs --tail 40 hindsight >&2 || true
  fi
  if [ "${KEEP:-}" != 1 ]; then
    # The scratch volumes hold a copy of the live memory: a failed removal
    # is a failure of the run, never swallowed.
    if [ -f "${BACKSIGHT_HOME:-/nonexistent}/data/server/compose.yaml" ] && ! compose down -v >"$work/down.log" 2>&1; then
      cat "$work/down.log" >&2
      echo "restore-proof: ERROR: 'compose down -v' of project $project failed: its volumes, a copy of the live data, are still there; remove them: docker compose -p $project down -v" >&2
      [ $status -ne 0 ] || status=1
    fi
    [ -e "$work/live/identity" ] && { shred -u "$work/live/identity" 2>/dev/null || rm -f "$work/live/identity"; }
    rm -rf "$work"
  fi
  [ $status -eq 0 ] && echo "restore-proof: PASS" || echo "restore-proof: FAIL (exit $status)"
  exit $status
}
trap cleanup EXIT

# Copies, never the originals: the archive with its manifest, the identity.
archive=$work/live/$(basename "$src")
cp "$src" "$archive"
cp "$src.json" "$archive.json"
(
  umask 077
  sed -n "s/^backup_identity *= *[\"']\(AGE-SECRET-KEY-1[0-9A-Z]*\)[\"'] *$/\1/p" "$live_secrets" > "$work/live/identity"
)
grep -q '^AGE-SECRET-KEY-1' "$work/live/identity" || fail "no age identity in $live_secrets"
RESTORE_ARGS="--identity $work/live/identity"

# Built before HOME moves: Go's caches stay the caller's.
(cd "$root" && go build -o "$bin" ./cmd/backsight)
export HOME=$work/home BACKSIGHT_HOME=$work/backsight
unset CLAUDE_CONFIG_DIR XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME CLAUDECODE

# 1. The scratch install, as an agent would set it up, on the mock LLM.
answers=(--answer provider=mock --answer model=mock-model --answer consent=yes --answer retention=3650
  --answer project="$project" --answer api_port="$port" --answer ui_port="$ui_port" --answer scheduler=off)
printf 'mock-not-a-key\n' | "$bin" secret set llm_api_key >/dev/null
"$bin" setup --non-interactive --json "${answers[@]}" > "$work/setup.json" 2> "$work/progress.jsonl" ||
  { cat "$work/setup.json" >&2; fail "scratch setup failed"; }
grep -q '^provider *= *.mock' "$BACKSIGHT_HOME/config/config.toml" || fail "the scratch install is not on the mock LLM"
grep -q "127.0.0.1:$port" "$BACKSIGHT_HOME/config/config.toml" || fail "the scratch install does not use port $port"
[ "$(sed -n 's/^name: *//p' "$BACKSIGHT_HOME/data/server/compose.yaml" | head -n 1)" = "$project" ] || fail "the scratch compose project is not $project"
grep -q '^HINDSIGHT_API_LLM_PROVIDER=mock$' "$BACKSIGHT_HOME/data/server/.env" || fail "the scratch server is not on the mock LLM"
# shellcheck source=scripts/restore-chain.sh
. "$root/scripts/restore-chain.sh"
[ "$(rc_bank)" = "$(jq -r '.bank' <<<"$manifest")" ] || fail "the archive holds bank $(jq -r '.bank' <<<"$manifest"), the scratch install uses $(rc_bank)"

# 2. The real archive restored: restore's own checks, then the counts read
# again from the scratch database, against the manifest written beside it.
db_counts() { # the memory bank's rows per table (the control bank is restore's own)
  local t
  for t in documents memory_units mental_models; do
    printf '%s %s\n' "$t" "$(compose exec -T db psql -U hindsight -d hindsight -tAc "select count(*) from $t where bank_id <> '$(rc_bank)-control'")"
  done | jq -R -s -c 'split("\n") | map(select(. != "") | split(" ") | {(.[0]): (.[1] | tonumber)}) | add'
}
want=$(jq -cS '.counts' <<<"$manifest")
# shellcheck disable=SC2086 # RESTORE_ARGS is a word list
"$bin" restore --json $RESTORE_ARGS "$archive" > "$work/restore-first.json" || { cat "$work/restore-first.json" >&2; fail "restore of the real archive failed"; }
jq -e '.ok and ([.data.steps[] | select(.status == "done") | .name] as $s | ["check", "safety backup", "restore", "counts", "start", "health", "recall", "epoch"] - $s == [])' \
  "$work/restore-first.json" >/dev/null || { cat "$work/restore-first.json" >&2; fail "the real archive's restore skipped a check"; }
[ "$(jq -cS '.data.restored_counts' "$work/restore-first.json")" = "$want" ] || fail "restored counts $(jq -c '.data.restored_counts' "$work/restore-first.json") differ from the manifest's $want"
[ "$(jq -cS '.data.manifest.counts' "$work/restore-first.json")" = "$want" ] || fail "the archive's own manifest differs from the one beside it"
got=$(db_counts | jq -cS .)
[ "$got" = "$want" ] || fail "the scratch database holds $got, the manifest $want"
first_epoch=$(jq -r '.data.epoch.epoch' "$work/restore-first.json")
"$bin" jobs tick --json > "$work/tick-first.json" || { cat "$work/tick-first.json" >&2; fail "jobs tick after the first restore failed"; }
jq -e --argjson to "$first_epoch" '.ok and (.data.epoch | startswith("restore epoch 0 -> \($to): 0 session"))' "$work/tick-first.json" >/dev/null ||
  { cat "$work/tick-first.json" >&2; fail "the scratch client did not reconcile to epoch $first_epoch"; }
"$bin" doctor --json > "$work/doctor-first.json" || true
[ "$(rc_check "$work/doctor-first.json" restore-epoch)" = ok ] || fail "doctor's restore-epoch is not ok after the first restore"

# 3. The client reconciles: a session sent now is lost by restoring the
# same archive again; the next jobs tick sends it again, once.
rc_send restore-proof-lost
rc_restore_resends "$archive" restore-proof-lost
got_again=$(db_counts | jq -cS .)

echo "restore-proof: archive $(basename "$src") of $(jq -r '.created_at' <<<"$manifest"), Hindsight $(jq -r '.hindsight_image' <<<"$manifest" | sed 's/@.*//')"
echo "restore-proof: manifest counts $want; restored $(jq -cS '.data.restored_counts' "$work/restore-first.json"); scratch database $got"
echo "restore-proof: first restore: steps $(jq -r '[.data.steps[] | "\(.name)=\(.status)"] | join(" ")' "$work/restore-first.json"); epoch $first_epoch"
echo "restore-proof: second restore: epoch $rc_epoch_before -> $rc_epoch_after; session restore-proof-lost re-sent once ($(rc_meta restore-proof-lost)); database $got_again"
echo "restore-proof: doctor restore-epoch $(rc_check "$work/doctor-$(basename "$archive").json" restore-epoch)"
