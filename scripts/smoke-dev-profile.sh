#!/usr/bin/env bash
# Live smoke test of the Claude Code wiring (decision 0054, block 3), by hand:
# installs the plugin into a separate Claude Code profile (never ~/.claude),
# copies the user's credentials there for one headless prompt, checks that
# the plugin's MCP server connected, the SessionStart hook injected and a
# memory tool answered, then deletes the copied credentials whatever happens.
# Usage: scripts/smoke-dev-profile.sh [profile dir] (default ~/.claude-backsight-dev)
# Needs a configured backsight (server reachable) and a logged-in ~/.claude.
set -euo pipefail
profile=${1:-$HOME/.claude-backsight-dev}
case "$profile" in "$HOME/.claude" | "$HOME/.claude/") echo "refusing the main profile" >&2; exit 2 ;; esac
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
cleanup() { rm -f "$profile/.credentials.json"; rm -rf "$work"; }
trap cleanup EXIT

(cd "$root" && go build -o "$work/backsight" ./cmd/backsight)
"$work/backsight" client install --config-dir "$profile"
cp -p "$HOME/.claude/.credentials.json" "$profile/.credentials.json"
CLAUDE_CONFIG_DIR=$profile claude -p "Search your long-term memory for one decision about this project, then answer in one sentence and cite the memory exactly as your instructions say." \
  --output-format stream-json --verbose --max-turns 6 >"$work/stream.jsonl"
python3 - "$work/stream.jsonl" <<'PY'
import json, sys
ok = {"mcp": False, "hook": False, "tool": False, "cite": False}
for line in open(sys.argv[1]):
    try:
        m = json.loads(line)
    except ValueError:
        continue
    if m.get("type") == "system" and m.get("subtype") == "init":
        ok["mcp"] = any(s.get("name") == "plugin:backsight:memory" and s.get("status") == "connected" for s in m.get("mcp_servers", []))
    if m.get("type") == "system" and m.get("subtype") == "hook_response" and "Long-term memory" in json.dumps(m):
        ok["hook"] = True
    if m.get("type") == "assistant":
        ok["tool"] |= any(b.get("type") == "tool_use" and b.get("name", "").startswith("mcp__plugin_backsight_memory__") for b in m["message"]["content"])
    if m.get("type") == "result":
        ok["cite"] = "From Hindsight memory" in (m.get("result") or "") or "nothing in memory" in (m.get("result") or "")
print(ok)
sys.exit(0 if all(ok.values()) else 1)
PY
