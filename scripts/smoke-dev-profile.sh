#!/usr/bin/env bash
# Live smoke test of the Claude Code wiring (decision 0054, block 3), by hand:
# installs the plugin into a separate Claude Code profile (never ~/.claude),
# copies the user's credentials there for one headless prompt, checks that
# the plugin's MCP server connected, the SessionStart hook injected and a
# memory tool answered, the skills are listed and the MCP server found its
# session through the hooks' record (M3, decision 0056), then deletes the
# copied credentials whatever happens.
# Usage: scripts/smoke-dev-profile.sh [profile dir] (default ~/.claude-backsight-dev)
# Needs a configured backsight (server reachable) and a logged-in ~/.claude.
set -euo pipefail
profile=${1:-$HOME/.claude-backsight-dev}
case "$profile" in "$HOME/.claude" | "$HOME/.claude/") echo "refusing the main profile" >&2; exit 2 ;; esac
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
# Credentials the profile already had are put back; ours are deleted.
had=""
if [ -e "$profile/.credentials.json" ]; then
  had=$work/credentials.backup
fi
cleanup() {
  rm -f "$profile/.credentials.json"
  if [ -n "$had" ] && [ -e "$had" ]; then mv "$had" "$profile/.credentials.json"; fi
  rm -rf "$work"
}
trap cleanup EXIT

if [ -n "$had" ]; then cp -p "$profile/.credentials.json" "$had"; fi
(cd "$root" && go build -o "$work/backsight" ./cmd/backsight)
"$work/backsight" client install --config-dir "$profile"
cp -p "$HOME/.claude/.credentials.json" "$profile/.credentials.json"
started=$(date -u +%Y-%m-%dT%H:%M:%S)
CLAUDE_CONFIG_DIR=$profile claude -p "Search your long-term memory for one decision about this project, then answer in one sentence and cite the memory exactly as your instructions say." \
  --output-format stream-json --verbose --max-turns 6 </dev/null >"$work/stream.jsonl"
python3 - "$work/stream.jsonl" <<'PY'
import json, sys
ok = {"mcp": False, "hook": False, "tool": False, "cite": False, "skills": False}
for line in open(sys.argv[1]):
    try:
        m = json.loads(line)
    except ValueError:
        continue
    if m.get("type") == "system" and m.get("subtype") == "init":
        ok["mcp"] = any(s.get("name") == "plugin:backsight:memory" and s.get("status") == "connected" for s in m.get("mcp_servers", []))
        names = json.dumps(m.get("slash_commands", [])) + json.dumps(m.get("skills", []))
        ok["skills"] = all("backsight:" + n in names for n in ("remember", "pause", "memory-retro", "memory"))
    if m.get("type") == "system" and m.get("subtype") == "hook_response" and "Long-term memory" in json.dumps(m):
        ok["hook"] = True
    if m.get("type") == "assistant":
        ok["tool"] |= any(b.get("type") == "tool_use" and b.get("name", "").startswith("mcp__plugin_backsight_memory__") for b in m["message"]["content"])
    if m.get("type") == "result":
        ok["cite"] = "From Hindsight memory" in (m.get("result") or "") or "nothing in memory" in (m.get("result") or "")
print(ok)
sys.exit(0 if all(ok.values()) else 1)
PY
# The MCP server read the live record of its Claude Code process, in this
# run: tool events written since it started.
state=$("$work/backsight" paths --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["state"])')
python3 - "$state/telemetry" "$started" <<'PY'
import glob, json, sys
n = 0
for f in glob.glob(sys.argv[1] + "/events-*.jsonl"):
    for line in open(f):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        n += e.get("event") == "tool" and e.get("live") == "record" and e.get("ts", "") >= sys.argv[2]
print("mcp tool calls that found their session in this run:", n)
sys.exit(0 if n else 1)
PY
