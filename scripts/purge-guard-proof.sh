#!/usr/bin/env bash
# The proof of the look for recreated purged documents (backsight issue 5)
# on a real Hindsight: a throwaway stack (its own Compose project, ports,
# HOME and BACKSIGHT_HOME, Hindsight's mock LLM, removed on exit).
#   1. A session is sent, then purged with forget --purge; its later turn
#      goes to the successor document (#2).
#   2. An older client, unaware of tombstones, appends to the purged id as
#      it always did: Hindsight 1.1.0 recreates the document, holding only
#      the new segment, and extracts facts from it.
#   3. doctor names the recreated document and its session.
#   4. jobs tick purges it again: the document and the facts extracted from
#      the append are gone; the successor stays.
#   5. A second tick and retain purge-again do nothing.
#
# Usage: scripts/purge-guard-proof.sh [api port] (default 28550 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; the UI takes port + 1111).
# PROJECT names the Compose project (default bs-purge-guard-proof; never
# backsight). KEEP=1 leaves the stack and its directory.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/ports.sh
. "$root/scripts/ports.sh"
# shellcheck source=scripts/hf-cache.sh
. "$root/scripts/hf-cache.sh"
port=$(slot_port 28550 "${1:-}")
project=${PROJECT:-bs-purge-guard-proof}
fail() { echo "purge-guard-proof: $*" >&2; exit 1; }
[ "$project" != backsight ] || fail "PROJECT=backsight is the machine's own install"
for p in "$port" "$((port + 1111))"; do
  ! curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$p/" || fail "port $p is already in use"
done
in_use=$(docker ps -aq --filter "label=com.docker.compose.project=$project"; docker volume ls -q --filter "label=com.docker.compose.project=$project")
[ -z "$in_use" ] || fail "Compose project $project already has containers or volumes"

work=$(mktemp -d)
bin=$work/backsight-bin
(cd "$root" && go build -o "$bin" ./cmd/backsight) # with the real HOME: its module cache
export BACKSIGHT_HOME=$work/backsight HOME=$work/home CLAUDE_CONFIG_DIR=$work/home/.claude
mkdir -p "$BACKSIGHT_HOME/config" "$HOME"
compose() { docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"; }
cleanup() {
  if [ "${KEEP:-}" != 1 ]; then
    [ ! -f "$BACKSIGHT_HOME/data/server/compose.yaml" ] || compose down -v >/dev/null 2>&1 || true
    rm -rf "$work"
  else
    echo "kept: $work (compose project $project)" >&2
  fi
}
trap cleanup EXIT

cat > "$BACKSIGHT_HOME/config/config.toml" <<TOML
schema_version = 1
[server]
url = "http://127.0.0.1:$port"
bank = "proof"
ui_port = $((port + 1111))
compose_dir = "$BACKSIGHT_HOME/data/server"
project = "$project"
[llm]
provider = "mock"
model = "mock-model"
TOML
printf 'proof-not-a-key\n' | "$bin" secret set llm_api_key >/dev/null
for name in api_key ui_key db_password; do openssl rand -hex 24 | "$bin" secret set "$name" >/dev/null; done
"$bin" server render >/dev/null
hfc_seed "$project"
compose up -d --quiet-pull
for _ in $(seq 1 120); do curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1 && break; sleep 5; done
curl -fsS "http://127.0.0.1:$port/health" >/dev/null || fail "the server did not come up"
hfc_save "$project"
"$bin" bank apply >/dev/null
key=$(sed -n "s/^api_key *= *[\"']\([^\"']*\)[\"'] *\$/\1/p" "$BACKSIGHT_HOME/config/secrets.toml")
B="http://127.0.0.1:$port/v1/default/banks/proof"
api() { curl -fsS -H "Authorization: Bearer $key" -H 'Content-Type: application/json' "$@" || fail "request failed: $*"; }
status() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $key" "$1"; }
enc() { jq -rn --arg d "$1" '$d|@uri'; }
idle() { for _ in $(seq 1 90); do n=$(( $(api "$B/operations?status=pending&limit=1" | jq '.total // 0') + $(api "$B/operations?status=processing&limit=1" | jq '.total // 0') )); [ "$n" = 0 ] && return; sleep 2; done; fail "operations still queued"; }
units() { api "$B/memories/list?document_id=$(enc "$1")&limit=100" | jq '.total'; }
unit_ids() { api "$B/memories/list?document_id=$(enc "$1")&limit=100" | jq -r '[.items[].id] | sort | join(",")'; }
doc() { status "$B/documents/$(enc "$1")"; }
all_units() { api "$B/memories/list?limit=1" | jq '.total'; }

project_dir=$HOME/src/billing
mkdir -p "$project_dir"
tdir=$CLAUDE_CONFIG_DIR/projects/-src-billing
mkdir -p "$tdir"
rec() { # sid uuid role text minutes-ago
  local content ts
  ts=$(date -u -d "-$5 minutes" +%Y-%m-%dT%H:%M:%SZ)
  if [ "$3" = user ]; then content=$(jq -cn --arg t "$4" '$t'); else content=$(jq -cn --arg t "$4" '[{type: "text", text: $t}]'); fi
  jq -cn --arg s "$1" --arg u "$2" --arg r "$3" --arg c "$project_dir" --arg ts "$ts" --argjson m "$content" \
    '{type: $r, uuid: $u, sessionId: $s, cwd: $c, timestamp: $ts, entrypoint: "cli", message: {role: $r, content: $m}}' >> "$tdir/$1.jsonl"
}
S1=11111111-1111-4111-8111-111111111111
D1=conversation:$S1

# 1. The session goes out, then is purged.
rec $S1 u1 user "The billing deploy token is hunter2-leaked, keep it handy." 90
rec $S1 a1 assistant "Noted: the billing deploy uses the token hunter2-leaked." 89
"$bin" retain flush --session "$S1" --json > /dev/null
idle
[ "$(doc "$D1")" = 200 ] || fail "the session was not sent"
sent_units=$(units "$D1")
[ "$sent_units" -gt 0 ] || fail "no fact extracted from the session"
fact=$(api "$B/memories/list?document_id=$(enc "$D1")&limit=1" | jq -r '.items[0].id')
"$bin" forget "$fact" --purge --yes --json > "$work/forget.json" || { cat "$work/forget.json"; fail "forget --purge failed"; }
idle
[ "$(doc "$D1")" = 404 ] || fail "the purge left the document"
jq -e --arg d "$D1" '[.tombstones[] | select(.document == $d)] | length == 1' "$BACKSIGHT_HOME/state/state.json" > /dev/null || fail "no tombstone"
# A later turn of the same session: today's build sends it to the successor.
rec $S1 u2 user "Billing invoices now round half up." 30
rec $S1 a2 assistant "Noted: billing rounds half up." 29
"$bin" retain flush --session "$S1" --json > /dev/null
idle
[ "$(doc "$D1#2")" = 200 ] && [ "$(doc "$D1")" = 404 ] || fail "the later turn did not go to the successor"
successor_units=$(units "$D1#2")
echo "== sent and purged: $D1 had $sent_units fact(s), purged; later turns in $D1#2 ($successor_units fact(s))"

# 2. An older client appends to the purged id, as builds before tombstones
# did (async, update_mode append): the server recreates the document.
api -X POST "$B/memories" -d "$(jq -nc --arg d "$D1" --arg s "$S1" '{async: true, items: [{
  content: "user: The billing deploy token was rotated to hunter3-leaked.\nassistant: Noted: hunter3-leaked.",
  document_id: $d, update_mode: "append", tags: ["project:billing", "source:chat", "harness:claude-code"],
  metadata: {session_id: $s, last_uuid: "old-a3", segment: "2", source: "chat"}}]}')" > /dev/null
idle
[ "$(doc "$D1")" = 200 ] || fail "the append did not recreate the purged document"
recreated_units=$(units "$D1") recreated_ids=$(unit_ids "$D1")
[ "$recreated_units" -gt 0 ] || fail "no fact extracted from the append"
api "$B/documents/$(enc "$D1")" | jq -e '.original_text | test("hunter3") and (test("hunter2") | not)' > /dev/null ||
  fail "the recreated document does not hold only the appended segment"
before_all=$(all_units)
echo "== recreated by an older client: $D1 holds only the appended segment, $recreated_units fact(s) ($recreated_ids); bank $before_all fact(s)"

# 3. doctor names it.
"$bin" doctor --json > "$work/doctor.json" || true
jq -e --arg s "$S1" '.data.checks[] | select(.id == "purged-documents") | .status == "error" and (.summary | contains("session " + $s))' "$work/doctor.json" > /dev/null ||
  { jq '.data.checks[] | select(.id == "purged-documents")' "$work/doctor.json"; fail "doctor did not name the recreated document"; }
echo "== doctor: $(jq -r '.data.checks[] | select(.id == "purged-documents") | .status + ": " + .summary' "$work/doctor.json")"

# 4. jobs tick purges it again, with the facts drawn from the append.
"$bin" jobs tick --json > "$work/tick1.json" || { cat "$work/tick1.json"; fail "jobs tick failed"; }
jq -e --arg d "$D1" '.data.purges | contains("purged again") and contains($d)' "$work/tick1.json" > /dev/null || { cat "$work/tick1.json"; fail "the tick did not purge it again"; }
idle
[ "$(doc "$D1")" = 404 ] || fail "the recreated document is still there"
[ "$(units "$D1")" = 0 ] || fail "facts of the recreated document survived"
for id in ${recreated_ids//,/ }; do
  [ "$(status "$B/memories/$id")" = 404 ] || fail "fact $id of the recreated document survived"
done
[ "$(doc "$D1#2")" = 200 ] && [ "$(units "$D1#2")" = "$successor_units" ] || fail "the successor was touched"
after_all=$(all_units)
jq -e --arg d "$D1" '.purge_guard.purged == [$d] and (.purge_guard.waiting // []) == []' "$BACKSIGHT_HOME/state/state.json" > /dev/null || fail "state: $(jq -c .purge_guard "$BACKSIGHT_HOME/state/state.json")"
echo "== jobs tick: $(jq -r .data.purges "$work/tick1.json"); its $recreated_units fact(s) gone (bank $before_all -> $after_all); $D1#2 kept ($successor_units fact(s))"

# 5. Nothing more to do.
"$bin" jobs tick --json > "$work/tick2.json" || { cat "$work/tick2.json"; fail "second tick failed"; }
jq -e '(.data.purges // "") == ""' "$work/tick2.json" > /dev/null || { cat "$work/tick2.json"; fail "the second tick did something"; }
"$bin" retain purge-again --json > "$work/again.json" || { cat "$work/again.json"; fail "retain purge-again failed"; }
jq -e '.data.action == "nothing"' "$work/again.json" > /dev/null || { cat "$work/again.json"; fail "retain purge-again found something"; }
"$bin" doctor --json > "$work/doctor2.json" || true
jq -e '.data.checks[] | select(.id == "purged-documents") | .status == "ok"' "$work/doctor2.json" > /dev/null || fail "doctor still reports it"
echo "== second tick: nothing; retain purge-again (every tombstone): $(jq -c '{action: .data.action, tombstones_checked: .data.tombstones_checked, requests: .data.requests}' "$work/again.json"); doctor ok"
echo "purge-guard-proof: ok"
