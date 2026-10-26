#!/usr/bin/env bash
# The proof of `backsight retain reattribute` on a real Hindsight: a
# throwaway stack (its own Compose project, ports, HOME and BACKSIGHT_HOME,
# Hindsight's mock LLM, removed on exit) holding what older builds left:
#   - a subagent report filed under its session's directory while its work
#     was in another of the user's repositories (S3);
#   - a session whose document an append relabelled to a plain directory's
#     name (S2: tagged dist, every record billing's);
#   - a session sent as one document mixing two repositories (S1).
# It checks: the plan; the free moves (PATCH, no LLM call, the memory units
# follow) after a verified backup, the re-send waiting for its answer
# (exit 10); the re-send once answered, sent again per project by the next
# flush (the operation ids salted: Hindsight collapses a repeated one); a
# last run that finds nothing (idempotent).
#
# Usage: scripts/reattribute-proof.sh [api port] (default 28500 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; the UI takes port + 1111).
# PROJECT names the Compose project (default bs-reattr-proof; never
# backsight). KEEP=1 leaves the stack and its directory.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/ports.sh
. "$root/scripts/ports.sh"
# shellcheck source=scripts/hf-cache.sh
. "$root/scripts/hf-cache.sh"
port=$(slot_port 28500 "${1:-}")
project=${PROJECT:-bs-reattr-proof}
fail() { echo "reattribute-proof: $*" >&2; exit 1; }
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
price_input = 1
price_output = 2
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
enc() { jq -rn --arg d "$1" '$d|@uri'; }
tags() { api "$B/documents/$(enc "$1")" | jq -c '.tags'; }
llm() { api "$B/llm-requests/stats?period=1d&call_type=llm" | jq '[.buckets[]?.total] | add // 0'; }
# settled_llm: the LLM call count once it stopped moving for 30 s (the
# server logs calls late, and consolidates after a retain returns).
settled_llm() {
  local a b
  a=$(llm)
  for _ in $(seq 1 12); do sleep 30; b=$(llm); [ "$a" = "$b" ] && { echo "$b"; return; }; a=$b; done
  fail "LLM calls still moving after 6 minutes"
}
idle() { for _ in $(seq 1 90); do n=$(api "$B/operations?status=pending" | jq '[.operations[]?] | length'); [ "$n" = 0 ] && return; sleep 2; done; fail "operations still pending"; }
unit_tags() { api "$B/memories/list?document_id=$(enc "$1")&limit=100" | jq -c '[.items[].tags | map(select(startswith("project:")))[]] | unique'; }
reattr() { set +e; "$bin" retain reattribute --json "$@" > "$work/out.json"; code=$?; set -e; }

# The user's repositories (owner me) and a plain directory.
repo() { mkdir -p "$1" && git -C "$1" init -q && git -C "$1" remote add origin "git@github.com:me/$(basename "$1").git"; }
billing=$HOME/src/billing shop=$HOME/src/shop dist=$HOME/.npm-global/lib/x/dist
repo "$billing"; repo "$shop"; mkdir -p "$dist"
tdir=$CLAUDE_CONFIG_DIR/projects/-src-billing
mkdir -p "$tdir"
ts=$(date -u -d '-3 hours' +%Y-%m-%dT%H:%M:%SZ)
rec() { # sid uuid role cwd text
  local content
  if [ "$3" = user ]; then content=$(jq -cn --arg t "$5" '$t'); else content=$(jq -cn --arg t "$5" '[{type: "text", text: $t}]'); fi
  jq -cn --arg s "$1" --arg u "$2" --arg r "$3" --arg c "$4" --arg ts "$ts" --argjson m "$content" \
    '{type: $r, uuid: $u, sessionId: $s, cwd: $c, timestamp: $ts, entrypoint: "cli", message: {role: $r, content: $m}}' >> "$tdir/$1.jsonl"
}
S1=11111111-1111-4111-8111-111111111111 S2=22222222-2222-4222-8222-222222222222 S3=33333333-3333-4333-8333-333333333333
rec $S1 s1u1 user "$billing" "Billing invoices round half up since the audit."
rec $S1 s1a1 assistant "$billing" "Noted: billing rounds half up."
rec $S1 s1u2 user "$shop" "The shop cart caches prices for ten minutes."
rec $S1 s1a2 assistant "$shop" "Noted: the cart cache lasts ten minutes."
rec $S2 s2u1 user "$billing" "Billing exports run every night at two."
rec $S2 s2a1 assistant "$dist" "The nightly export is at two in the morning."
rec $S3 s3u1 user "$billing" "Ask an agent to fix the shop's VAT."
rec $S3 s3a1 assistant "$billing" "I asked a subagent."
mkdir -p "$tdir/$S3/subagents"
for i in 1 2; do
  jq -cn --arg c "$billing" --arg f "$shop/vat.go" --arg u "ag$i" '{type: "assistant", isSidechain: true, uuid: $u, cwd: $c, message: {role: "assistant", content: [{type: "tool_use", id: $u, name: "Edit", input: {file_path: $f}}]}}' >> "$tdir/$S3/subagents/agent-ag1.jsonl"
done

# Today's build sends the three sessions per project (named: they started
# before this install's live retain).
for s in $S1 $S2 $S3; do "$bin" retain flush --session "$s" --json > /dev/null; done
idle
# What older builds left instead.
api -X PATCH "$B/documents/$(enc "conversation:$S2")" -d '{"tags":["project:dist","source:chat","harness:claude-code"]}' > /dev/null
api -X DELETE "$B/documents/$(enc "conversation:$S1")" > /dev/null
api -X DELETE "$B/documents/$(enc "conversation:$S1@shop")" > /dev/null
api -X POST "$B/memories" -d "$(jq -nc --arg d "conversation:$S1" --arg s "$S1" '{async: false, items: [{
  content: "user: Billing invoices round half up since the audit.\nassistant: Noted: billing rounds half up.\nuser: The shop cart caches prices for ten minutes.\nassistant: Noted: the cart cache lasts ten minutes.",
  document_id: $d, tags: ["project:shop", "source:chat", "harness:claude-code"],
  metadata: {session_id: $s, last_uuid: "s1a2", segment: "1", records: "4", source: "chat"}}]}')" > /dev/null
api -X POST "$B/memories" -d "$(jq -nc --arg d "subagent:$S3:ag1" '{async: false, items: [{
  content: "Fixed the shop VAT rounding in vat.go.", document_id: $d, tags: ["project:billing", "source:subagent", "harness:claude-code"],
  metadata: {source: "subagent"}}]}')" > /dev/null
idle
echo "== before: S1 $(tags "conversation:$S1"), S2 $(tags "conversation:$S2"), S3's report $(tags "subagent:$S3:ag1") $(unit_tags "subagent:$S3:ag1")"

# 1. The plan: two free moves, one re-send; nothing written.
reattr --plan
[ "$code" = 0 ] || fail "--plan exited $code"
jq -e --arg s1 "$S1" --arg s2 "$S2" --arg s3 "$S3" '.data.plan
  | (.retags | map({document, from, to}) | sort_by(.document)) == ([{document: ("conversation:" + $s2), from: "dist", to: "billing"}, {document: ("subagent:" + $s3 + ":ag1"), from: "billing", to: "shop"}] | sort_by(.document))
    and (.resends | length) == 1 and .resends[0].session == $s1 and .resends[0].reason == "mixed" and (.resends[0].projects | sort) == ["billing", "shop"] and .cost.cost > 0' \
  "$work/out.json" > /dev/null || { cat "$work/out.json"; fail "unexpected plan"; }
proj() { tags "$1" | jq -r 'map(select(startswith("project:")))[0]'; } # the server keeps no tag order
[ "$(proj "subagent:$S3:ag1")" = project:billing ] || fail "--plan wrote"

# 2. No answer: verified backup, free moves, the re-send waits (exit 10).
calls=$(settled_llm)
reattr
[ "$code" = 10 ] || { cat "$work/out.json"; fail "without resend=yes: exit $code, want 10"; }
idle
after=$(settled_llm)
[ "$after" = "$calls" ] || fail "the free moves made LLM calls: $calls -> $after"
jq -e '.data.backup | test("backsight-.*\\.tar\\.gz\\.age$")' "$work/out.json" > /dev/null || { cat "$work/out.json"; fail "no backup named"; }
jq -e '.backup_verify.ok == true' "$BACKSIGHT_HOME/state/state.json" > /dev/null || fail "the backup was not verified"
[ "$(proj "subagent:$S3:ag1")" = project:shop ] || fail "the report was not moved: $(tags "subagent:$S3:ag1")"
[ "$(unit_tags "subagent:$S3:ag1")" = '["project:shop"]' ] || fail "the report's memory units did not follow: $(unit_tags "subagent:$S3:ag1")"
[ "$(proj "conversation:$S2")" = project:billing ] || fail "S2 was not moved"
[ "$(proj "conversation:$S1")" = project:shop ] || fail "S1 changed without the answer"
echo "== free moves: report $(unit_tags "subagent:$S3:ag1"), S2 $(tags "conversation:$S2"); LLM calls $calls -> $after"

# 3. The re-send, answered; the next flush sends S1 per project.
reattr --answer resend=yes
[ "$code" = 0 ] || { cat "$work/out.json"; fail "resend=yes: exit $code"; }
jq -e --arg s1 "$S1" '.data.queued == [$s1]' "$work/out.json" > /dev/null || fail "S1 not queued"
calls=$(llm)
"$bin" retain flush --session "$S1" --json > "$work/flush.json"
idle
home_doc=$(api "$B/documents/$(enc "conversation:$S1")" | jq -c '{tags: (.tags | map(select(startswith("project:")))[0]), project: (.document_metadata // .metadata).project, last: (.document_metadata // .metadata).last_uuid}')
shop_doc=$(api "$B/documents/$(enc "conversation:$S1@shop")" | jq -c '{tags: (.tags | map(select(startswith("project:")))[0]), project: (.document_metadata // .metadata).project, last: (.document_metadata // .metadata).last_uuid}')
[ "$home_doc" = '{"tags":"project:billing","project":"billing","last":"s1a1"}' ] || fail "S1's home document after the re-send: $home_doc"
[ "$shop_doc" = '{"tags":"project:shop","project":"shop","last":"s1a2"}' ] || fail "S1's shop document after the re-send: $shop_doc"
after=$(settled_llm)
[ "$after" -gt "$calls" ] || fail "the re-send extracted nothing"
echo "== re-sent: $home_doc $shop_doc; LLM calls $calls -> $after"

# 4. Idempotent: nothing left.
reattr
[ "$code" = 0 ] && jq -e '.data.action == "nothing" and (.data.plan.retags | length) == 0 and (.data.plan.resends | length) == 0' "$work/out.json" > /dev/null ||
  { cat "$work/out.json"; fail "a second run is not empty (exit $code)"; }
grep -c '"action":"retag"' "$BACKSIGHT_HOME/state/reattribute.jsonl" | grep -qx 2 || fail "journal: $(cat "$BACKSIGHT_HOME/state/reattribute.jsonl")"
echo "reattribute-proof: ok"
