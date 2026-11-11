#!/usr/bin/env bash
# The recording run of the Codex harness (run.sh's default command; it
# runs in the container prepare.sh set up). It drives a real Codex through
# the scenarios the Codex blocks need and writes what Codex produced under
# /out/fixtures: hook payloads per event, backsight's hook inputs and
# outputs, the MCP traffic with its _meta, the model requests, rollouts and
# config.toml snapshots. Hook trust goes through Codex's own review screen
# (driven with tmux), never a bypass flag.
set -euo pipefail
fx=/out/fixtures
mkdir -p "$fx" /tmp/prompts
cd "$HOME/work"
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh

# --- 1. hook trust: the review screen, then trust all ---------------------
log "trust review"
tui
wait_for "Hooks need review"
mkdir -p "$fx/trust"
screen | grep -v '^\s*$' > "$fx/trust/review-prompt.txt"
tmux send-keys -t t Enter # 1. Review hooks
wait_for "need review before they can run"
screen | grep -v '^\s*$' > "$fx/trust/review-list.txt"
tmux send-keys -t t Enter # the first event's hooks
sleep 1
screen | grep -v '^\s*$' > "$fx/trust/review-hook.txt"
tmux send-keys -t t Escape
sleep 0.5
tmux send-keys -t t t # trust all
sleep 1
tmux send-keys -t t Escape
wait_for "Ask Codex to do anything"
cp "$CODEX_HOME/config.toml" "$fx/trust/config-after-trust.toml"

# --- 2. an interactive session: tools, approval, subagent, compaction ----
log "interactive session"
sub=$(jq -nc '[{call:"memory_search",args:{queries:["subagent check"]}},{call:"exec_command",args:{cmd:"env | sort > /out/fixtures-env-subagent-shell.txt; echo ok"}},{say:"SUB-DONE"}]')
prompt t1 "first turn" "$(jq -nc '[
  {call:"memory_search",args:{queries:["codex spike"]}},
  {call:"exec_command",args:{cmd:"env | sort > /out/fixtures-env-shell.txt; echo ok"}},
  {call:"exec_command",args:{cmd:"apply_patch <<'\''EOF'\''\n*** Begin Patch\n*** Add File: hello.txt\n+hello from the fake model\n*** End Patch\nEOF\n"}},
  {call:"request_user_input",args:{questions:[{id:"pick",header:"Pick",question:"Which one?",options:[{label:"A (Recommended)",description:"a"},{label:"B",description:"b"}]}]}},
  {call:"memory_store",args:{kind:"decision",text:"Decision (2026-11-10): the Codex harness records fixtures with a fake model."}},
  {say:"T1-DONE"}]')"
prompt t2 "second turn with a subagent" "$(jq -nc --arg m "sub task <<fake $sub>>" '[
  {call:"spawn_agent",args:{message:$m}},
  {call:"wait_agent",args:{targets:["{{last:agent_id}}"],timeout_ms:30000}},
  {say:"T2-DONE"}]')"
prompt t3 "third turn after compaction" '[{"say":"T3-DONE"}]'
paste /tmp/prompts/t1
wait_for "Allow the backsight MCP server to run tool"
mkdir -p "$fx/approval"
screen | grep -v '^\s*$' | tail -12 > "$fx/approval/memory_store-prompt.txt"
tmux send-keys -t t Enter # 1. Allow (once)
wait_for "• T1-DONE"
paste /tmp/prompts/t2
wait_for "• T2-DONE"
# The turn's end hooks may still run: /compact is refused until it is idle.
for _ in 1 2 3 4 5 6; do
  slash /compact
  wait_for "Context compacted" 10 2>/dev/null && break
done
wait_for "Context compacted" 2
paste /tmp/prompts/t3
wait_for "• T3-DONE"
slash /exit
sleep 3
root=$(jq -r 'select(.hook_event_name=="SessionStart" and .source=="startup") | .session_id' /out/probe/*-SessionStart.json | tail -1)
echo "$root" > "$fx/session-root.txt"

# --- 3. resume and fork (exec) -------------------------------------------
log "resume, fork"
prompt resume "resumed turn" '[{"say":"RESUMED"}]'
quiet codex exec resume "$root" "$(cat /tmp/prompts/resume)"
prompt fork "forked turn" '[{"say":"FORKED"}]'
quiet codex exec fork "$root" "$(cat /tmp/prompts/fork)"

# --- 4. /clear in the TUI ------------------------------------------------
log "clear"
prompt c1 "before clear" '[{"say":"C1-DONE"}]'
prompt c2 "after clear" '[{"say":"C2-DONE"}]'
tui
wait_for "Ask Codex to do anything"
paste /tmp/prompts/c1
wait_for "• C1-DONE"
slash /clear
sleep 2
paste /tmp/prompts/c2
wait_for "• C2-DONE"
slash /exit
sleep 3
tmux kill-server 2>/dev/null || true

# --- 5. hook outputs Codex accepts (exec, the probe answers) -------------
log "hook outputs"
answer() { # answer <event> <rc> [stdout file] [stderr text]: once
  rm -f /tmp/probe/"$1".*
  echo "$2" > "/tmp/probe/$1.rc"
  [ -z "${3:-}" ] || cp "$3" "/tmp/probe/$1.stdout"
  [ -z "${4:-}" ] || echo "$4" > "/tmp/probe/$1.stderr"
  touch "/tmp/probe/$1.once"
}
for i in $(seq 1 800); do printf 'L%04d-abcdefghijklmnopqrstuvwxyz-0123456789-ABCDEFGHI\n' "$i"; done > /tmp/big.txt
jq -Rs '{hookSpecificOutput:{hookEventName:"UserPromptSubmit",additionalContext:.}}' /tmp/big.txt > /tmp/big.json
answer UserPromptSubmit 0 /tmp/big.json
prompt cap "OUT-CAP context cap" '[{"say":"ok"}]'
quiet codex exec "$(cat /tmp/prompts/cap)"
echo PLAIN-UPS-MARKER > /tmp/plain.txt
answer UserPromptSubmit 0 /tmp/plain.txt
answer PreToolUse 2 "" PRETOOL-BLOCK-MARKER
answer Stop 2 "" STOP-EXIT2-MARKER
prompt outs "OUT-EXIT2 exit codes" '[{"call":"memory_search","args":{"queries":["x"]}},{"say":"ok"}]'
quiet codex exec "$(cat /tmp/prompts/outs)"
answer UserPromptSubmit 2 "" UPS-BLOCK-MARKER
prompt upsblock "OUT-UPSBLOCK prompt blocked" '[{"say":"must not run"}]'
quiet codex exec --json "$(cat /tmp/prompts/upsblock)" || true
rm -f /tmp/probe/*

# --- 6. approvals: exec denies what needs one, unless config approves ----
log "approvals"
prompt appr "approvals" '[{"call":"readonly"},{"call":"closed"},{"call":"plain"},{"call":"destructive"},{"call":"memory_store","args":{"kind":"decision","text":"Decision (2026-11-10): approvals probe."}},{"say":"ok"}]'
mkdir -p "$fx/approval"
# The sandbox matters: with full disk access and approval "never", exec
# approves every MCP tool; workspace-write is a user's default.
codex exec -s workspace-write --json "$(cat /tmp/prompts/appr)" < /dev/null > /tmp/appr-default.jsonl 2>/dev/null
codex exec --json "$(cat /tmp/prompts/appr)" < /dev/null > /tmp/appr-fullaccess.jsonl 2>/dev/null
cp "$CODEX_HOME/config.toml" /tmp/config.bak
cat >> "$CODEX_HOME/config.toml" <<'TOML'

[plugins."backsight@backsight".mcp_servers.backsight.tools.memory_store]
approval_mode = "approve"

[mcp_servers.probe.tools.plain]
approval_mode = "approve"
TOML
codex exec -s workspace-write --json "$(cat /tmp/prompts/appr)" < /dev/null > /tmp/appr-config.jsonl 2>/dev/null
cp "$CODEX_HOME/config.toml" "$fx/approval/config-with-approvals.toml"
cp /tmp/config.bak "$CODEX_HOME/config.toml"
for k in default fullaccess config; do
  jq -c 'select(.type=="item.completed" and .item.type=="mcp_tool_call") | .item | {server, tool, status, error: (.error.message // null)}' "/tmp/appr-$k.jsonl" > "$fx/approval/exec-$k.jsonl"
done

# --- 7. one MCP process per thread, or shared behind development flags ---
log "mcp sharing"
psub=$(jq -nc '[{call:"readonly"},{say:"SUB-DONE"}]')
prompt share "share" "$(jq -nc --arg m "sub task <<fake $psub>>" '[{call:"readonly"},{call:"spawn_agent",args:{message:$m}},{call:"wait_agent",args:{targets:["{{last:agent_id}}"],timeout_ms:30000}},{say:"ok"}]')"
mkdir -p "$fx/mcp"
rm -rf /out/probemcp
quiet codex exec "$(cat /tmp/prompts/share)"
ls /out/probemcp | grep -c start > "$fx/mcp/processes-default.txt" || true
sed -i 's#^args = \["-log", "/out/probemcp"\]$#&\nenv = { CODEX_MCP_PROTOCOL_VERSION = "2026-07-28" }#' "$CODEX_HOME/config.toml"
rm -rf /out/probemcp
quiet codex exec --enable mcp_2026_07_28 --enable mcp_connection_sharing "$(cat /tmp/prompts/share)"
ls /out/probemcp | grep -c start > "$fx/mcp/processes-shared.txt" || true
cat /out/probemcp/*-call-*.json | jq -s '.' > "$fx/mcp/shared-process-calls.json"
cp /tmp/config.bak "$CODEX_HOME/config.toml"

# --- 8. rollout compression ----------------------------------------------
log "compression"
# A session of its own, aged past the 7 days the compressor waits.
prompt z0 "to compress" '[{"say":"ok"}]'
tid=$(codex exec --json "$(cat /tmp/prompts/z0)" < /dev/null 2>/dev/null | jq -r 'select(.type=="thread.started") | .thread_id')
echo "$tid" > "$fx/session-compressed.txt"
old=$(find "$CODEX_HOME/sessions" -name "rollout-*-$tid.jsonl")
cp "$old" /tmp/old.jsonl
touch -d '9 days ago' "$old"
prompt z "compression" '[{"say":"ok"}]'
quiet codex exec --enable local_thread_store_compression "$(cat /tmp/prompts/z)"
for _ in $(seq 1 20); do [ -f "$old.zst" ] && break; sleep 0.5; done
mkdir -p "$fx/rollouts"
if [ -f "$old.zst" ] && zstd -dc "$old.zst" | cmp -s - /tmp/old.jsonl; then
  echo "compressed in place to $(basename "$old").zst, identical once decompressed" > "$fx/rollouts/compression.txt"
else
  echo "no compressed rollout" > "$fx/rollouts/compression.txt"
fi

# --- 9. collect -----------------------------------------------------------
log "collect"
/opt/harness/collect.sh
log "done: $fx"
