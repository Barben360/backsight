#!/usr/bin/env bash
# Renames backsight's Codex MCP server from memory to backsight in the
# recorded fixtures (the user's decision of 2026-11-11), so the replays
# match what backsight now generates. Recorded with the server named
# memory, the fixtures are otherwise kept as Codex 0.167.0 wrote them; a
# recording made since names the server backsight (scripts/codex/
# server-name.sh shows Codex 0.167.0 accepting it).
#   scripts/codex/rename-server-fixtures.sh [fixtures dir]
# What it rewrites, in every text file (JSON escaped or not):
#   - tool names mcp__memory__<tool> and the namespace mcp__memory;
#   - "server": "memory" and "server_name": "memory" (rollouts, codex exec);
#   - [plugins."backsight@backsight".mcp_servers.memory.…] (approvals);
#   - "the memory MCP server" (the approval prompt's screen);
# and file names holding mcp__memory__. A second run changes nothing.
set -euo pipefail
dir=${1:-$(cd "$(dirname "$0")/../.." && pwd)/internal/testdata/codex/0.167.0}
find "$dir" -type f ! -name '*.zst' ! -name README.md -print0 | xargs -0 perl -pi -e '
  s/mcp__memory(?=__|\\?")/mcp__backsight/g;
  s/(\\?"server(?:_name)?\\?"\s*:\s*\\?")memory(?=\\?")/${1}backsight/g;
  s/mcp_servers\.memory\./mcp_servers.backsight./g;
  s/the memory MCP server/the backsight MCP server/g;
'
find "$dir" -depth -name '*mcp__memory__*' | while read -r f; do
  mv "$f" "$(dirname "$f")/$(basename "$f" | sed 's/mcp__memory__/mcp__backsight__/g')"
done
if grep -rlE 'mcp__memory|"server(_name)?": ?"memory"' "$dir" --exclude='*.zst' --exclude=README.md; then
  echo "rename-server-fixtures: names left (above)" >&2
  exit 1
fi
