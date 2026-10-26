#!/usr/bin/env bash
# The proof of the look for recreated purged documents (backsight issue 5)
# on a real Hindsight: a throwaway stack (its own Compose project, ports,
# HOME and BACKSIGHT_HOME, Hindsight's mock LLM, removed on exit).
#   1. A session is sent, then purged with forget --purge; its later turn
#      goes to the successor document (#2).
#   2. An older client, unaware of tombstones, appends to the purged id as
#      it always did: Hindsight 1.1.0 recreates the document, holding only
#      the new segment, and extracts facts from it.
#   A tick has looked before (its window starts at the server's newest
#   document), so only its incremental look can find the recreated one.
#   3. The server's operation rows name the document of a one-document
#      retain, and none for a batch of several (the queue-wait rule).
#   4. doctor names it; retain purge-again --plan lists it and writes
#      nothing; with the claude-code provider, purge-again exits 10 and the
#      tick only records it.
#   5. jobs tick purges it again, by its incremental look: the document
#      and the facts extracted from the append are gone; the successor
#      stays; the window's newest is the server's.
#   6. A second tick and retain purge-again do nothing.
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
state_sum() { sha256sum "$BACKSIGHT_HOME/state/state.json" | cut -d' ' -f1; }
epoch_of() { date -u -d "$1" +%s; }
set_provider() { sed -i "s/^provider = .*/provider = \"$1\"/" "$BACKSIGHT_HOME/config/config.toml"; }
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

# The tick looks before anything is recreated: its window starts at the
# server's newest document.
"$bin" jobs tick --json > "$work/tick0.json" || { cat "$work/tick0.json"; fail "jobs tick failed"; }
jq -e '.purge_guard.newest != null and ((.purge_guard.full_at // "0001") | startswith("0001"))' "$BACKSIGHT_HOME/state/state.json" > /dev/null ||
  fail "the first tick recorded no window: $(jq -c .purge_guard "$BACKSIGHT_HOME/state/state.json")"
sleep 2

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
echo "== recreated by an older client: $D1 holds only the appended segment, $recreated_units fact(s)"

# 3. What the queue-wait rule reads: a one-document retain names its
# document in the operation rows, a batch of several names none.
api "$B/operations?status=completed&limit=20" | jq -e --arg d "$D1" '[.operations[] | select(.document_id == $d)] | length >= 1' > /dev/null ||
  fail "the operation rows do not name the recreated document: $(api "$B/operations?status=completed&limit=5" | jq -c '[.operations[] | {operation_type, document_id}]')"
api -X POST "$B/memories" -d '{"async": true, "items": [
  {"content": "Other client note one.", "document_id": "proof-batch-a", "tags": []},
  {"content": "Other client note two.", "document_id": "proof-batch-b", "tags": []}]}' > /dev/null
idle
batch=$(api "$B/operations?status=completed&limit=20" | jq -c '[.operations[] | select(.operation_type | test("retain"))][0] | {operation_type, document_id, items_count}')
[ "$(jq -r .items_count <<< "$batch")" = 2 ] || fail "the newest retain is not the batch of two: $batch"
[ "$(jq -r .document_id <<< "$batch")" = null ] || fail "a batch of two documents names one: $batch"
echo "== operation rows: a one-document retain names $D1; a batch of two names none ($batch)"

# 4. doctor names it (a warning: the tick looked within the hour); a plan
# lists it and writes nothing; with the claude-code provider nothing is
# purged unattended and purge-again asks for the consent.
"$bin" doctor --json > "$work/doctor.json" || true
jq -e --arg s "$S1" '.data.checks[] | select(.id == "purged-documents") | .status == "warn" and (.summary | contains("session " + $s))' "$work/doctor.json" > /dev/null ||
  { jq '.data.checks[] | select(.id == "purged-documents")' "$work/doctor.json"; fail "doctor did not name the recreated document"; }
echo "== doctor: $(jq -r '.data.checks[] | select(.id == "purged-documents") | .status + ": " + .summary' "$work/doctor.json")"
sum=$(state_sum)
"$bin" retain purge-again --plan --json > "$work/plan.json" || { cat "$work/plan.json"; fail "--plan failed"; }
jq -e --arg d "$D1" '.data.action == "planned" and ([.data.waiting[].document] == [$d])' "$work/plan.json" > /dev/null || { cat "$work/plan.json"; fail "--plan did not list it"; }
[ "$(state_sum)" = "$sum" ] && [ "$(doc "$D1")" = 200 ] || fail "--plan wrote"
set_provider claude-code
set +e; "$bin" retain purge-again --json > "$work/gate.json"; code=$?; set -e
[ "$code" = 10 ] && [ "$(doc "$D1")" = 200 ] || { cat "$work/gate.json"; fail "claude-code without consent: exit $code"; }
"$bin" jobs tick --json > "$work/tick-cc.json" || { cat "$work/tick-cc.json"; fail "jobs tick failed"; }
[ "$(doc "$D1")" = 200 ] || fail "the claude-code tick purged unattended"
jq -e '.purge_guard.waiting[0].needs_user == true' "$BACKSIGHT_HOME/state/state.json" > /dev/null || fail "the claude-code tick did not record it"
set_provider mock
echo "== --plan: listed, state.json unchanged; claude-code: purge-again exit 10, tick: $(jq -r .data.purges "$work/tick-cc.json")"

# 5. jobs tick purges it again, by its incremental look, with the facts
# drawn from the append.
before_all=$(all_units)
# (purge-again's look above was a full one: the tick must not make one.)
full_at=$(jq -r .purge_guard.full_at "$BACKSIGHT_HOME/state/state.json")
"$bin" jobs tick --json > "$work/tick1.json" || { cat "$work/tick1.json"; fail "jobs tick failed"; }
jq -e --arg d "$D1" '.data.purges | contains("purged again") and contains($d)' "$work/tick1.json" > /dev/null || { cat "$work/tick1.json"; fail "the tick did not purge it again"; }
[ "$(jq -r .purge_guard.full_at "$BACKSIGHT_HOME/state/state.json")" = "$full_at" ] || fail "the tick made a full look"
idle
[ "$(doc "$D1")" = 200 ] && fail "the recreated document is still there"
[ "$(units "$D1")" = 0 ] || fail "facts of the recreated document survived"
for id in ${recreated_ids//,/ }; do
  [ "$(status "$B/memories/$id")" = 404 ] || fail "fact $id of the recreated document survived"
done
[ "$(doc "$D1#2")" = 200 ] && [ "$(units "$D1#2")" = "$successor_units" ] || fail "the successor was touched"
after_all=$(all_units)
server_newest=$(api "$B/documents?time_field=created_at&limit=1" | jq -r '.items[0].created_at')
state_newest=$(jq -r .purge_guard.newest "$BACKSIGHT_HOME/state/state.json")
[ "$(epoch_of "$server_newest")" = "$(epoch_of "$state_newest")" ] || fail "window's newest $state_newest, the server's $server_newest"
echo "== jobs tick (incremental look): $(jq -r .data.purges "$work/tick1.json"); its $recreated_units fact(s) gone (bank $before_all -> $after_all memories); $D1#2 kept ($successor_units fact(s)); newest $state_newest = the server's $server_newest"

# 6. Nothing more to do.
"$bin" jobs tick --json > "$work/tick2.json" || { cat "$work/tick2.json"; fail "second tick failed"; }
jq -e '(.data.purges // "") == ""' "$work/tick2.json" > /dev/null || { cat "$work/tick2.json"; fail "the second tick did something"; }
"$bin" retain purge-again --json > "$work/again.json" || { cat "$work/again.json"; fail "retain purge-again failed"; }
jq -e '.data.action == "nothing"' "$work/again.json" > /dev/null || { cat "$work/again.json"; fail "retain purge-again found something"; }
"$bin" doctor --json > "$work/doctor2.json" || true
jq -e '.data.checks[] | select(.id == "purged-documents") | .status == "ok"' "$work/doctor2.json" > /dev/null || fail "doctor still reports it"
echo "== second tick: nothing; retain purge-again (every tombstone): $(jq -c '{action: .data.action, tombstones_checked: .data.tombstones_checked, requests: .data.requests}' "$work/again.json"); doctor ok"
echo "purge-guard-proof: ok"
