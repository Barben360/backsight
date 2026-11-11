#!/usr/bin/env bash
# The Codex fresh-install chain (Codex epic, issue 284), CI's codex-chain
# job, in the prepared container:
#   HARNESS_CMD=/opt/harness/chain.sh scripts/codex/run.sh
# A new PC that runs Codex alone (no claude on its PATH), a client of the
# scratch server (config.toml and the API key as connect writes them), a
# real Codex 0.167.0 and the fake model; then, each step checked and the
# chain stopped at the first failure:
#   setup     setup --answer tools=codex: both plugins installed, Codex's
#             own memories turned off
#   trust     no hook runs before the user trusts them; trusted through
#             Codex's own review screen (the TUI, no bypass flag), every
#             handler gets its trust entry
#   session   codex exec in workspace-write (approval policy never): the
#             model stores a memory, backsight's hooks run, nothing is
#             denied (the tools' hints)
#   recall    the stored memory reaches the server's search; a new
#             session's prompt is recalled for, and the model's
#             memory_search finds it
#   credit    the model credits the memory it was shown (the session's
#             journal pairs the search and the credit); a session (in the
#             TUI) that leaves a shown memory uncredited is held once by
#             Stop
#   flush     the TUI session, ended with /exit, is on the server as its
#             conversation document (SessionEnd's flush); a codex exec
#             session (headless) is never sent
#   update    an update (backsight-next: client install --codex) re-adds
#             only the main plugin; the hooks stay trusted and run
#   undo      setup --undo: the plugins gone, Codex's memories back on
#   uninstall uninstall: nothing of backsight left in config.toml
# One line per step in /out/chain/results.txt; the files each step read are
# kept beside it.
set -euo pipefail
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
fx=/out/chain
mkdir -p "$fx"
results=$fx/results.txt
: > "$results"
step=start
pass() { echo "PASS $step: $*" | tee -a "$results" >&2; }
fail() {
  echo "FAIL $step: $*" | tee -a "$results" >&2
  for f in "$fx/$step".*; do
    [ -f "$f" ] || continue
    echo "--- $f" >&2
    tail -c 4000 "$f" >&2
  done
  exit 1
}

# The PC: its own homes, backsight and codex alone on its PATH.
pc=$HOME/pc
prepared=$BACKSIGHT_HOME/config # prepare.sh's client of the scratch server
export BACKSIGHT_HOME=$pc/backsight CODEX_HOME=$pc/codex
mkdir -p "$BACKSIGHT_HOME/config" "$CODEX_HOME" "$pc/bin"
cp /opt/bs-build/backsight "$pc/bin/backsight"
export PATH=$pc/bin:/usr/local/bin:/usr/bin:/bin
command -v claude > /dev/null && fail "claude is on the PC's PATH"
# The client config and key connect writes (the scratch server is reached
# by its service name on the offline network).
cp "$prepared/config.toml" "$BACKSIGHT_HOME/config/config.toml"
cp "$prepared/secrets.toml" "$BACKSIGHT_HOME/config/secrets.toml"
chmod 600 "$BACKSIGHT_HOME/config/secrets.toml"
# Codex pointed at the fake model, with its own memories on; no other MCP
# server.
sed '/^\[mcp_servers.probe\]/,$d' /out/config-start.toml > "$CODEX_HOME/config.toml"
printf '\n[features]\nmemories = true\n' >> "$CODEX_HOME/config.toml"
cp "$CODEX_HOME/config.toml" "$fx/config-start.toml"
cd "$HOME/work"

# exec_turn <name> <prompt text> <steps json> [codex exec args…]: one
# codex exec turn (JSON events to <name>.jsonl, Codex's stderr, with its
# hook lines, to <name>.err).
exec_turn() {
  local name=$1 text=$2 steps=$3
  shift 3
  prompt "$name" "$text" "$steps"
  codex exec -s workspace-write --json "$@" "$(cat "/tmp/prompts/$name")" < /dev/null > "$fx/$name.jsonl" 2> "$fx/$name.err" || true
}
# plain_turn <name> <prompt text>: a turn without --json, so Codex's
# stderr (<name>.err) carries its hook lines ("hook: <event> Completed").
plain_turn() {
  prompt "$1" "$2" '[{"say":"ok"}]'
  codex exec -s workspace-write "$(cat "/tmp/prompts/$1")" < /dev/null > "$fx/$1.out" 2> "$fx/$1.err" || true
}
# end_tui: /exit in the TUI, as a user ends a session, then wait (a fixed
# number of polls) for the codex process to be gone, so its SessionEnd hook
# has run (and spawned its flush); only then is tmux killed.
end_tui() {
  local i
  slash /exit
  for i in $(seq 1 120); do
    pgrep -x codex > /dev/null || break
    sleep 0.5
  done
  ! pgrep -x codex > /dev/null || fail "codex still runs after /exit"
  tmux kill-server 2> /dev/null || true
}
# calls <name>: the turn's MCP calls, one "<server>/<tool>=<status>" a line.
calls() {
  jq -r 'select(.type=="item.completed" and .item.type=="mcp_tool_call") | .item | "\(.server)/\(.tool)=\(.status)"' "$fx/$1.jsonl"
}
# result <name> <tool>: the text a tool answered in that turn.
result() {
  jq -r --arg t "$2" 'select(.type=="item.completed" and .item.type=="mcp_tool_call" and .item.tool==$t) | .item.result.content[]?.text // empty' "$fx/$1.jsonl"
}
# said <name>: the model's last message of the turn.
said() { jq -r 'select(.type=="item.completed" and .item.type=="agent_message") | .item.text' "$fx/$1.jsonl" | tail -n 1; }
thread() { jq -r 'select(.type=="thread.started") | .thread_id' "$fx/$1.jsonl" | head -n 1; }
# hooks <name>: Codex's hook lines of that turn (a turn run without
# --json), counted.
hooks() { grep '^hook: ' "$fx/$1.err" | sort | uniq -c | tr -s ' \n' ' ' || true; }
# events <name>: what backsight's hooks and memory server recorded for that
# turn's session (its telemetry), to <name>.events.jsonl; prints the event
# names, counted.
events() {
  local state
  state=$(backsight paths --json | jq -r .data.state)
  cat "$state"/telemetry/events-*.jsonl 2> /dev/null | jq -c --arg s "$(thread "$1")" 'select(.session == $s)' > "$fx/$1.events.jsonl" || true
  jq -r '.client + ":" + .event' "$fx/$1.events.jsonl" | sort | uniq -c | tr -s ' \n' ' '
}

# docs_of <memory id>: the documents a memory comes from, one a line: its
# own, or its source facts' (an observation has none of its own).
docs_of() {
  local out
  out=$(backsight read --json "$1" 2> /dev/null) || return 0
  jq -r '.data.document_id // empty' <<< "$out"
  jq -r '.data.source_memories[]?.id' <<< "$out" | while read -r f; do
    backsight read --json "$f" 2> /dev/null | jq -r '.data.document_id // empty'
  done
}

# --- setup -----------------------------------------------------------------
step=setup
backsight setup --answer tools=codex --answer codex_memories=off --answer scheduler=off --answer consent=yes --non-interactive --json \
  > "$fx/setup.json" 2> "$fx/setup.err" || fail "setup exited $?"
jq -e '.ok' "$fx/setup.json" > /dev/null || fail "setup: $(jq -c '.error' "$fx/setup.json")"
st=$(jq -r '[.data.steps[] | select(.name=="codex" or .name=="codex_memories" or .name=="client") | .name + "=" + .status] | join(" ")' "$fx/setup.json")
[ "$st" = "client=skipped codex=done codex_memories=done" ] || fail "steps $st, want client skipped, codex and codex_memories done"
codex plugin list --json > "$fx/setup.plugins.json"
plugins=$(jq -r '[.installed[] | select(.installed and .enabled) | .pluginId] | sort | join(" ")' "$fx/setup.plugins.json")
[ "$plugins" = "backsight-hooks@backsight backsight@backsight" ] || fail "plugins installed and enabled: '$plugins'"
! grep -q '^memories = true' "$CODEX_HOME/config.toml" || fail "Codex's own memories still on"
pass "client steps skipped, plugins $plugins, Codex's memories off"

# --- trust -----------------------------------------------------------------
step=trust
plain_turn trust.untrusted "before trust"
grep -q '^ok$' "$fx/trust.untrusted.out" || fail "the turn before trust did not run"
! grep -q '^hook: ' "$fx/trust.untrusted.err" || fail "a hook ran before the user trusted it: $(hooks trust.untrusted)"
trust_all
end_tui
cp "$CODEX_HOME/config.toml" "$fx/trust.config.toml"
hooks_json=$BACKSIGHT_HOME/data/codex-marketplace/plugins/backsight-hooks/hooks/hooks.json
want=$(jq '[.hooks[][] | .hooks[]] | length' "$hooks_json")
got=$(grep -c '^\[hooks.state."backsight-hooks@backsight:' "$fx/trust.config.toml" || true)
[ "$want" -gt 0 ] && [ "$got" = "$want" ] || fail "$got trust entries for $want handlers"
plain_turn trust.trusted "after trust"
for h in SessionStart UserPromptSubmit Stop; do
  grep -q "^hook: $h Completed" "$fx/trust.trusted.err" || fail "once trusted, hook $h did not complete: $(hooks trust.trusted)"
done
pass "no hook before trust; $got of $want handlers trusted in the review screen; then $(hooks trust.trusted)"

# --- session: a memory stored ----------------------------------------------
step=session
nonce=chain-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
fact="Decision (2026-11-11): the codex chain $nonce keeps billing on PostgreSQL 17 with partial indexes."
exec_turn session "Remember the billing database decision for $nonce." \
  "$(jq -cn --arg f "$fact" '[{call: "memory_store", args: {kind: "decision", text: $f}}, {say: "S1-DONE"}]')"
s1=$(thread session)
[ -n "$s1" ] || fail "no thread id"
[ "$(calls session | tr '\n' ' ')" = "backsight/memory_store=completed " ] || fail "calls: $(calls session | tr '\n' ' ')"
note=$(result session memory_store | sed -n 's/^stored .* \(note:[0-9a-f-]*\)$/\1/p')
[ -n "$note" ] || fail "memory_store answered: $(result session memory_store | head -n 3)"
[ "$(said session)" = S1-DONE ] || fail "the turn ended with '$(said session)'"
# backsight's own record of the session: SessionStart's briefing, the
# prompt hook, PostToolUse's journal of the store, Stop's reply check.
ev=$(events session)
for e in briefing prompt tool_use reply; do
  jq -e --arg e "$e" 'select(.client == "codex" and .event == $e)' "$fx/session.events.jsonl" > /dev/null || fail "backsight recorded no codex $e event for $s1: $ev"
done
pass "thread $s1: memory_store ran with no approval prompt ($note); backsight recorded $ev"

# --- recall ----------------------------------------------------------------
step=recall
# Hindsight's mock LLM "extracts" the lines of its own prompt, so a hit's
# relevance means nothing here: the PC's recall floors are lowered (the
# search's min_score too), and what proves the recall is the source of
# what is shown, the note stored above.
printf '\n[recall]\nmin_score = 0.01\nglobal_min_score = 0.01\n' >> "$BACKSIGHT_HOME/config/config.toml"
query="Which database does the codex chain $nonce keep billing on?"
found=
for _ in $(seq 1 60); do
  backsight search --all --min-score 0 --json "$query" > "$fx/recall.search.json" 2>&1 || true
  if jq -e '.ok and (.data.hits | length > 0)' "$fx/recall.search.json" > /dev/null 2>&1; then
    found=1
    break
  fi
  sleep 3
done
[ -n "$found" ] || fail "the server's search never answered with the stored memory"
# A memory id in memory_search's answer: "- #1 … (<date>, <age> ago, <id>, <score>)".
idre='ago, ([0-9a-f-]{36})'
exec_turn recall "$query" "$(jq -cn --arg q "$query" --arg re "{{match:$idre}}" \
  '[{call: "memory_search", args: {queries: [$q], min_score: 0, limit: 20}}, {call: "memory_credit", args: {ids: [$re], verdict: "used"}}, {say: "S2-DONE"}]')"
s2=$(thread recall)
[ "$(calls recall | tr '\n' ' ')" = "backsight/memory_search=completed backsight/memory_credit=completed " ] || fail "calls: $(calls recall | tr '\n' ' ')"
shown=$(result recall memory_search)
ids=$(grep -oE 'ago, [0-9a-f-]{36}' <<< "$shown" | cut -c6- || true)
[ -n "$ids" ] || fail "memory_search showed no memory: $(head -n 5 <<< "$shown")"
from=
for id in $ids; do
  if docs_of "$id" | grep -qxF "$note"; then
    from=$id
    break
  fi
done
[ -n "$from" ] || fail "none of the $(wc -w <<< "$ids") memories shown comes from $note"
ev=$(events recall)
inj=$(jq -r 'select(.event == "prompt") | .injected' "$fx/recall.events.jsonl" | head -n 1)
[ "${inj:-0}" -gt 0 ] || fail "the prompt hook injected nothing ($(jq -c 'select(.event == "prompt") | {decision, reason, empty, injected}' "$fx/recall.events.jsonl"))"
asked=$(grep -l -F "$query" /out/model/*.json 2> /dev/null || true)
grep -l 'Possibly relevant memories' $asked > /dev/null 2>&1 || fail "no model request carrying the prompt holds the recalled memories"
pass "thread $s2: the prompt hook injected $inj memory(ies) into the model's request; memory_search showed $(wc -w <<< "$ids"), $from built from $note"

# --- credit ----------------------------------------------------------------
step=credit
credit=$(result recall memory_credit)
grep -q '^Credited\.' <<< "$credit" || fail "memory_credit answered: $(head -n 3 <<< "$credit")"
[ "$(said recall)" = S2-DONE ] || fail "the turn ended with '$(said recall)'"
# The control, in the TUI (Stop's check skips codex exec, a headless
# session: no one to ask): a shown memory left uncredited is held once by
# Stop, then the turn ends.
outcomes() {
  cat "$(backsight paths --json | jq -r .data.state)"/telemetry/events-*.jsonl 2> /dev/null |
    jq -r 'select(.client == "codex" and .event == "credit_check" and .outcome != "check_skipped") | .outcome' | tr '\n' ' '
}
[ -z "$(outcomes)" ] || fail "a credit check ran before the TUI session: $(outcomes)"
tui
wait_for "Ask Codex to do anything" || fail "the TUI did not start"
prompt credit.missing "$query" "$(jq -cn --arg q "$query" '[{call: "memory_search", args: {queries: [$q], min_score: 0}}, {say: "NO-CREDIT"}]')"
paste /tmp/prompts/credit.missing
for _ in $(seq 1 120); do
  case $(outcomes) in *undeclared*) break ;; esac
  sleep 0.5
done
shot "$fx/credit.missing.screen.txt"
[ "$(outcomes)" = "blocked undeclared " ] || fail "Stop's credit checks of the uncredited session: '$(outcomes)', want blocked then undeclared"
# The TUI session, ended as a user ends it: SessionEnd sends it (flush).
tsid=$(cat "$(backsight paths --json | jq -r .data.state)"/telemetry/events-*.jsonl |
  jq -r 'select(.client == "codex" and .event == "credit_check" and .outcome == "blocked") | .session' | head -n 1)
end_tui
pass "credited $from: $(sed -n 2p <<< "$credit"); a session leaving a shown memory uncredited is held once by Stop"

# --- flush -----------------------------------------------------------------
step=flush
key=$(sed -n "s/^api_key *= *[\"']\(.*\)[\"']$/\1/p" "$BACKSIGHT_HOME/config/secrets.toml")
url=$(sed -n 's/^url *= *"\(.*\)"$/\1/p' "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
bank=$(sed -n 's/^bank *= *"\(.*\)"$/\1/p' "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
# doc <session>: HTTP status of its conversation document, the body to
# flush.doc.json.
doc() { curl -sS -o "$fx/flush.doc.json" -w '%{http_code}' -K <(printf 'header = "Authorization: Bearer %s"\n' "$key") "$url/v1/default/banks/$bank/documents/conversation:$1"; }
[ -n "$tsid" ] || fail "no session id for the TUI session"
code=
for _ in $(seq 1 40); do
  code=$(doc "$tsid") || code=000
  [ "$code" != 200 ] || break
  sleep 3
done
[ "$code" = 200 ] || fail "conversation:$tsid (the TUI session, ended with /exit) is not on the server (HTTP $code)"
grep -qF "$nonce" "$fx/flush.doc.json" || fail "conversation:$tsid does not hold the session's prompt"
# codex exec sessions are headless: never sent (the privacy default).
code=$(doc "$s1") || code=000
[ "$code" = 404 ] || fail "conversation:$s1 (codex exec) is on the server (HTTP $code): a headless session is never sent"
pass "conversation:$tsid on the server with the session's prompt; the codex exec session $s1 kept out"

# --- update ----------------------------------------------------------------
step=update
before=$(jq -r '.installed[] | select(.pluginId=="backsight-hooks@backsight") | .version' "$fx/setup.plugins.json")
mkdir -p "$pc/next"
cp /opt/bs-build/backsight-next "$pc/next/backsight"
"$pc/next/backsight" client install --codex --json > "$fx/update.json" 2> "$fx/update.err" || fail "client install --codex of the new build exited $?"
jq -e '.ok' "$fx/update.json" > /dev/null || fail "update: $(jq -c .error "$fx/update.json")"
codex plugin list --json > "$fx/update.plugins.json"
main=$(jq -r '.installed[] | select(.pluginId=="backsight@backsight") | .version' "$fx/update.plugins.json")
after=$(jq -r '.installed[] | select(.pluginId=="backsight-hooks@backsight") | .version' "$fx/update.plugins.json")
case $main in 0.0.1-next*) ;; *) fail "backsight@backsight is $main, want the new build's 0.0.1-next" ;; esac
[ "$after" = "$before" ] || fail "backsight-hooks changed ($before → $after): the user would review the hooks again"
plain_turn update.session "after the update"
for h in SessionStart UserPromptSubmit Stop; do
  grep -q "^hook: $h Completed" "$fx/update.session.err" || fail "after the update, hook $h did not complete: $(hooks update.session)"
done
pass "backsight@backsight $main, backsight-hooks unchanged ($after); hooks still trusted: $(hooks update.session)"

# --- undo ------------------------------------------------------------------
step=undo
backsight setup --undo --json > "$fx/undo.json" 2> "$fx/undo.err" || fail "setup --undo exited $?"
jq -e '.ok' "$fx/undo.json" > /dev/null || fail "undo: $(jq -c .error "$fx/undo.json")"
codex plugin list --json > "$fx/undo.plugins.json"
[ "$(jq '.installed | length' "$fx/undo.plugins.json")" = 0 ] || fail "plugins left: $(jq -c '[.installed[].pluginId]' "$fx/undo.plugins.json")"
grep -q '^memories = true' "$CODEX_HOME/config.toml" || fail "Codex's own memories not back on"
! grep -q 'backsight' "$CODEX_HOME/config.toml" || fail "config.toml still names backsight: $(grep -n backsight "$CODEX_HOME/config.toml" | head -n 3)"
pass "plugins removed, Codex's memories back on, config.toml names no backsight"

# --- uninstall -------------------------------------------------------------
step=uninstall
backsight uninstall --json > "$fx/uninstall.json" 2> "$fx/uninstall.err" || fail "uninstall exited $?"
jq -e '.ok' "$fx/uninstall.json" > /dev/null || fail "uninstall: $(jq -c .error "$fx/uninstall.json")"
! grep -q 'backsight' "$CODEX_HOME/config.toml" || fail "config.toml names backsight"
[ ! -e "$CODEX_HOME/plugins/cache/backsight" ] || fail "the plugin cache is left: $CODEX_HOME/plugins/cache/backsight"
diff "$fx/config-start.toml" "$CODEX_HOME/config.toml" > "$fx/uninstall.diff" || true
pass "config.toml as it started but for what Codex wrote itself ($(grep -c '^>' "$fx/uninstall.diff" || true) line(s): $(grep '^> \[' "$fx/uninstall.diff" | cut -c3- | tr '\n' ' '))"
log "codex chain: every step passed ($results)"
