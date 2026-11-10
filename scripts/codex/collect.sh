#!/usr/bin/env bash
# Sorts what record.sh produced into /out/fixtures, named for the blocks
# that replay it, and scrubs what identifies the machine (the install id,
# the host name, temporary directory names). Session and turn ids stay:
# the files refer to each other through them.
set -euo pipefail
fx=/out/fixtures
mkdir -p "$fx"/{hooks,backsight,mcp,model,rollouts,env,config}

# Hook payloads, one per event and variant (source, trigger, subagent, tool).
for f in $(ls /out/probe/*.json | sort); do
  key=$(jq -r '[.hook_event_name,
      (.source // .trigger // .reason // empty),
      (if .agent_id then "subagent" else empty end),
      (.tool_name // empty | gsub("[^A-Za-z0-9_]"; "_"))] | join(".")' "$f")
  [ -e "$fx/hooks/$key.json" ] || jq . "$f" > "$fx/hooks/$key.json"
done
first=$(ls /out/probe/*-SessionStart.env | sort | head -1)
cp "$first" "$fx/env/hook.env"

# backsight's own hooks: what Codex gave it and what it answered, the
# first run of each event that answered something (else the first run).
keep() { # keep <tap input file> <event>
  jq . "$1" > "$fx/backsight/$2.in.json" 2>/dev/null || cp "$1" "$fx/backsight/$2.in.json"
  rm -f "$fx/backsight/$2.out.json"
  if [ -s "${1%.in.json}.out" ]; then jq . "${1%.in.json}.out" > "$fx/backsight/$2.out.json"; fi
  cp "${1%.in.json}.rc" "$fx/backsight/$2.rc"
}
for f in $(ls /out/tap/*-hook-*.in.json | sort); do
  ev=${f##*-hook-}; ev=${ev%.in.json}
  if [ ! -e "$fx/backsight/$ev.in.json" ] || { [ ! -e "$fx/backsight/$ev.out.json" ] && [ -s "${f%.in.json}.out" ]; }; then
    keep "$f" "$ev"
  fi
done

# The MCP traffic: the root session's server and a subagent's.
root=$(cat "$fx/session-root.txt")
for f in $(ls /out/tap/*-mcp.in.jsonl | sort); do
  grep -q '"tools/call"' "$f" || continue
  meta=$(grep '"tools/call"' "$f" | head -1 | jq -r '.params._meta["x-codex-turn-metadata"].thread_source // ""')
  name=root
  [ "$meta" = subagent ] && name=subagent
  grep -q "$root" "$f" || continue
  [ -e "$fx/mcp/$name.in.jsonl" ] && continue
  cp "$f" "$fx/mcp/$name.in.jsonl"
  cp "${f%.in.jsonl}.out.jsonl" "$fx/mcp/$name.out.jsonl"
  cp "${f%.in.jsonl}.env" "$fx/env/mcp-$name.env"
done

# Model requests: what the model is sent (roles of the injected text).
pick() { # pick <name> <grep text> [first|last]
  local f
  f=$({ grep -lF -- "$2" /out/model/*.json || true; } | sort |{ if [ "${3:-first}" = last ]; then tail -1; else head -1; fi; })
  if [ -n "$f" ]; then jq . "$f" > "$fx/model/$1.json"; else echo "collect: no model request for $1" >&2; fi
}
pick turn-first-request "first turn <<fake"
pick subagent-first-request 'sub task <<fake [{\"call\":\"memory_search\"'
pick compaction-request '\"request_kind\":\"compaction\"'
pick additional-context-cap "OUT-CAP context cap" last
pick exit-codes "STOP-EXIT2-MARKER" last
pick stop-block "Memories were shown to you this turn" last

# Rollouts.
sessions=$CODEX_HOME/sessions
copy_rollout() { # copy_rollout <name> <thread id>
  local f
  f=$(find "$sessions" -name "rollout-*-$2.jsonl*" | head -1)
  [ -n "$f" ] || return 0
  local b
  b=$(basename "$f")
  cp "$f" "$fx/rollouts/$1.${b#*.}"
}
copy_rollout root "$root"
sub=$(jq -r 'select(.hook_event_name=="SubagentStart") | .agent_id' /out/probe/*-SubagentStart.json | head -1)
copy_rollout subagent "$sub"
fork=$(jq -r 'select(.source=="fork") | .session_id' /out/probe/*-SessionStart.json | head -1)
[ -z "$fork" ] || copy_rollout fork "$fork"
clear=$(jq -r 'select(.source=="clear") | .session_id' /out/probe/*-SessionStart.json | head -1)
[ -z "$clear" ] || copy_rollout clear-new "$clear"
z=$(find "$sessions" -name '*.jsonl.zst' | head -1)
[ -z "$z" ] || cp "$z" "$fx/rollouts/compressed.jsonl.zst"

# Environments and config.
[ -f /out/fixtures-env-shell.txt ] && mv /out/fixtures-env-shell.txt "$fx/env/shell.env"
[ -f /out/fixtures-env-subagent-shell.txt ] && mv /out/fixtures-env-subagent-shell.txt "$fx/env/shell-subagent.env"
cp /out/config-start.toml "$fx/config/config-start.toml"
cp "$CODEX_HOME/config.toml" "$fx/config/config-end.toml"
cp /out/codex-plugin-list.json "$fx/config/plugin-list.json"
cp /out/codex-marketplace-add.txt /out/codex-plugin-add.txt "$fx/config/"
codex features list > "$fx/config/features.txt"
codex --version > "$fx/config/codex-version.txt"

# Scrub what identifies this machine.
install_id=$(cat "$CODEX_HOME/installation_id" 2>/dev/null || echo none)
host=$(hostname)
find "$fx" -type f ! -name '*.zst' -print0 | xargs -0 sed -i \
  -e "s/$install_id/00000000-0000-4000-8000-000000000000/g" \
  -e "s/$host/codex-test/g" \
  -e 's/codex-arg0[A-Za-z0-9]\{6\}/codex-arg0XXXXXX/g' \
  -e 's#^TMUX=.*#TMUX=[scrubbed]#' \
  -e 's/"prompt_cache_key": "[^"]*"/"prompt_cache_key": "(the thread id)"/'
# (prompt_cache_key is the thread id; a secret scanner takes it for a key.)
