#!/usr/bin/env bash
# Adapts backsight's generated Claude Code plugin tree for Codex 0.167, the
# two changes the spike found necessary (the real generator comes with
# issue 278):
#   - a Codex hook handler is one shell command string: the exec form's
#     "args" are ignored, so the shim would run with no arguments;
#   - Codex names the memory tools mcp__memory__<tool> in hook payloads,
#     so the PostToolUse matcher becomes mcp__memory__.*.
# A version suffix makes Codex take the changed tree as a new version.
#   codexify.sh <marketplace dir> [version suffix]
set -euo pipefail
mkt=${1:?marketplace dir}
suffix=${2:-codex}
p=$mkt/plugins/backsight
jq '.hooks |= with_entries(.value |= map(
      (if .matcher == "mcp__plugin_backsight_memory__.*" then .matcher = "mcp__memory__.*" else . end)
      | .hooks |= map(if .args then .command = ([.command] + .args | join(" ")) | del(.args) else . end)))' \
  "$p/hooks/hooks.json" > "$p/hooks/hooks.json.new"
mv "$p/hooks/hooks.json.new" "$p/hooks/hooks.json"
jq --arg s "$suffix" '.version += "." + $s' "$p/.claude-plugin/plugin.json" > "$p/.claude-plugin/plugin.json.new"
mv "$p/.claude-plugin/plugin.json.new" "$p/.claude-plugin/plugin.json"
