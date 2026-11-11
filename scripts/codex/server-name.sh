#!/usr/bin/env bash
# The Codex memory server named backsight (user decision 2026-11-11), on a
# real Codex 0.167.0, with the plugins backsight generates for Codex
# (client install --codex), in the prepared container:
#   HARNESS_CMD=/opt/harness/server-name.sh scripts/codex/run.sh
# It checks, one line each in server-name/results.txt: Codex adds the
# plugins; `codex mcp list` names the server backsight; the model is offered
# the namespace mcp__backsight; PostToolUse fires with tool_name
# mcp__backsight__<tool>, as backsight's own hook (the tap) and the probe see
# it; memory_store and memory_credit run in codex exec (workspace-write,
# policy never) with no approval; a user server named memory is no clash
# any more, one named backsight is refused.
set -euo pipefail
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
fx=/out/fixtures/server-name
mkdir -p "$fx"
result() { echo "$*" | tee -a "$fx/results.txt" >&2; }
probe_hooks=$CODEX_HOME/hooks.json
export CODEX_HOME=$HOME/codex-name
mkdir -p "$CODEX_HOME"
cp /out/config-start.toml "$CODEX_HOME/config.toml"
cp "$probe_hooks" "$CODEX_HOME/hooks.json" # the recording hooks, user level
cd "$HOME/work"

shim=$BACKSIGHT_HOME/data/bin/current
cp "$shim" /tmp/tap-shim # client install points the shim at the binary again: the tap goes back after
backsight client install --codex --json > "$fx/install.json" 2> "$fx/install.err" || true
rm -f "$shim" && cp /tmp/tap-shim "$shim" && chmod +x "$shim"
result "1 install: ok=$(jq -r .ok "$fx/install.json") $(jq -r '[.data.steps[] | select(.name=="codex") | .detail] | join(" | ")' "$fx/install.json")"
jq '.mcpServers | keys' "$BACKSIGHT_HOME/data/codex-marketplace/plugins/backsight/.mcp.json" > "$fx/mcp-json-keys.json"
jq -r '.hooks.PostToolUse[0].matcher' "$BACKSIGHT_HOME/data/codex-marketplace/plugins/backsight-hooks/hooks/hooks.json" > "$fx/post-tool-use-matcher.txt"
result "2 generated: .mcp.json server $(jq -c . "$fx/mcp-json-keys.json"), PostToolUse matcher $(cat "$fx/post-tool-use-matcher.txt")"
codex mcp list --json > "$fx/mcp-list.json"
result "3 codex mcp list: $(jq -c '[.[] | .name]' "$fx/mcp-list.json")"

trust_all
tmux kill-server 2>/dev/null || true
mark=$fx/.start
touch "$mark"
prompt n "server name" '[{"call":"memory_search","args":{"queries":["server name check"]}},{"call":"memory_store","args":{"kind":"decision","text":"Decision (2026-11-11): the Codex memory server is named backsight."}},{"call":"memory_credit","args":{"verdict":"none"}},{"say":"N-DONE"}]'
codex exec -s workspace-write --json "$(cat /tmp/prompts/n)" < /dev/null 2> "$fx/exec.err" |
  jq -c 'select(.type=="item.completed" and .item.type=="mcp_tool_call") | .item | {server, tool, status, error: (.error.message // null)}' > "$fx/exec-calls.jsonl" || true
result "4 codex exec (workspace-write): $(jq -r '(.server // "?") + "/" + .tool + "=" + .status' "$fx/exec-calls.jsonl" | tr '\n' ' ')"
req=$(grep -l '"server name check' /out/model/*.json 2>/dev/null | head -1 || true)
if [ -n "$req" ]; then
  jq -c '[.. | objects | select(.type? == "namespace") | {name, tools: [.tools[]?.name]}]' "$req" > "$fx/model-namespaces.json" || true
fi
result "5 the model is offered: $(cat "$fx/model-namespaces.json" 2>/dev/null || echo none)"
for p in /out/probe/*-PostToolUse.json; do
  [ "$p" -nt "$mark" ] || continue
  jq -r '.tool_name' "$p"
done | sort | uniq -c > "$fx/post-tool-use-names.txt"
result "6 PostToolUse tool_name (probe): $(tr -s ' \n' ' ' < "$fx/post-tool-use-names.txt")"
for p in /out/tap/*-hook-post-tool-use.in.json; do
  [ "$p" -nt "$mark" ] || continue
  jq -r '.tool_name' "$p"
done | sort | uniq -c > "$fx/backsight-hook-names.txt"
result "7 backsight's PostToolUse hook saw: $(tr -s ' \n' ' ' < "$fx/backsight-hook-names.txt")"
for p in /out/tap/*-hook-*.in.json; do
  [ "$p" -nt "$mark" ] || continue
  b=${p%.in.json}
  echo "$(jq -r .hook_event_name "$p") rc=$(cat "$b.rc") $(jq -r 'if .decision then "decision=" + .decision else "" end' "$b.out" 2>/dev/null)"
done | sort | uniq -c > "$fx/backsight-hook-runs.txt"
# Stop with no decision: the credit was journaled (PostToolUse) and paired.
result "8 backsight's hook runs: $(tr -s ' \n' ' ' < "$fx/backsight-hook-runs.txt")"

# A user server named memory is no clash now; one named backsight is.
printf '\n[mcp_servers.memory]\ncommand = "/bin/true"\n' >> "$CODEX_HOME/config.toml"
backsight client install --codex --json > "$fx/install-memory-server.json" 2>&1 || true
result "9 with a user server named memory: ok=$(jq -r .ok "$fx/install-memory-server.json")"
printf '\n[mcp_servers.backsight]\ncommand = "/bin/true"\n' >> "$CODEX_HOME/config.toml"
backsight client install --codex --json > "$fx/install-backsight-server.json" 2>&1 || true
result "10 with a user server named backsight: ok=$(jq -r .ok "$fx/install-backsight-server.json") $(jq -r '.error.message // ""' "$fx/install-backsight-server.json" | cut -c1-120)"
rm -f "$mark"
log "server name check done: $fx"
