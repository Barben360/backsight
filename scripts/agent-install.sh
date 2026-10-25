#!/usr/bin/env bash
# The agent-driven fresh install (M4 block 4, plan 0063): a headless Claude
# Code agent installs backsight for a "user" by following
# docs/agent-guide.md, in an isolated HOME and Claude Code profile, on this
# machine's Docker with Hindsight's mock LLM. The agent may only run
# backsight and read files. Round 1 must end with the agent asking for the
# key (nothing written); the "user" then sets it; round 2 must finish setup
# with verify passing. Undo and cleanup follow, whatever happens.
#
# Usage: scripts/agent-install.sh [api port] (default 38888). Needs a
# logged-in ~/.claude (its credentials are copied into the throwaway
# profile for the run, then shredded). WORK: work directory (must allow
# executables; default ~/.cache/backsight-agent-install). KEEP=1 keeps it.
set -euo pipefail
port=${1:-38888}
project=${PROJECT:-bs-fresh}
root=$(cd "$(dirname "$0")/.." && pwd)
real_home=$HOME
work=${WORK:-$real_home/.cache/backsight-agent-install}
rm -rf "$work"
mkdir -p "$work/bin"
(cd "$root" && go build -o "$work/bin/backsight" ./cmd/backsight)
export HOME=$work/home BACKSIGHT_HOME=$work/backsight PATH=$work/bin:$PATH
unset CLAUDE_CONFIG_DIR XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME CLAUDECODE
mkdir -p "$HOME/.claude"
compose() {
  docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"
}
cleanup() {
  status=$?
  if [ -e "$HOME/.claude/.credentials.json" ]; then
    shred -u "$HOME/.claude/.credentials.json" 2>/dev/null || rm -f "$HOME/.claude/.credentials.json"
  fi
  [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ] && compose down -v >/dev/null 2>&1 || true
  [ "${KEEP:-}" = 1 ] || rm -rf "$work"
  exit $status
}
trap cleanup EXIT
fail() { echo "agent-install: $*" >&2; exit 1; }
cp -p "$real_home/.claude/.credentials.json" "$HOME/.claude/.credentials.json"

agent() { # agent <out> <claude args…>
  local out=$1
  shift
  (cd "$HOME" && claude -p "$@" --output-format json --max-turns 40 \
    --allowedTools "Bash(backsight *)" "Bash(backsight:*)" "Read" </dev/null >"$out") || true
  jq -r '.result // "(no result)"' "$out"
}

guide=$root/docs/agent-guide.md
prompt="Please give me a long-term memory with backsight. Follow the guide at $guide exactly; the backsight binary is already installed and on PATH, so skip its step 1. My answers to its questions: provider mock, model mock-model, retention 3650, backups off. This machine already runs another backsight, so also use the hidden answers project=$project, api_port=$port, ui_port=$((port + 1111)) and scheduler=off. For any other question, take its recommended option. I have read the plan you will show and I consent to it. If you need anything only I can do, stop and tell me exactly what to run."

echo "== round 1"
agent "$work/round1.json" "$prompt" | tee "$work/round1.txt"
grep -q "secret set llm_api_key" "$work/round1.txt" || fail "round 1 did not ask the user to set llm_api_key"
[ ! -e "$BACKSIGHT_HOME/config/config.toml" ] || fail "config.toml written before the key"
session=$(jq -r '.session_id' "$work/round1.json")

echo "== the user sets the key"
printf 'mock-not-a-key\n' | backsight secret set llm_api_key >/dev/null

echo "== round 2"
agent "$work/round2.json" --resume "$session" "Done: I ran ! backsight secret set llm_api_key. Please continue and finish." | tee "$work/round2.txt"
jq -e '.finished and ([.steps[] | select(.name == "verify")][0].status == "done")' "$BACKSIGHT_HOME/state/setup.json" >/dev/null ||
  { cat "$BACKSIGHT_HOME/state/setup.json" >&2 2>/dev/null; fail "setup did not finish with verify passing"; }
grep -qi "restart" "$work/round2.txt" || fail "round 2 did not relay restart_required"
if grep -q "mock-not-a-key" "$work/round1.json" "$work/round2.json"; then fail "the key passed through the agent"; fi

echo "== undo"
backsight setup --undo --json >"$work/undo.json" || fail "setup --undo failed"
echo "agent-install: ok (the agent asked for the key, then finished setup with verify passing)"
