#!/usr/bin/env bash
# The Codex parity checks on a throwaway CODEX_HOME (Codex epic, issue 276),
# run in the prepared container:
#   HARNESS_CMD=/opt/harness/parity.sh scripts/codex/run.sh
# 1. a past Codex session (a TUI turn) before backsight is wired, then
#    setup for Codex alone: its backfill question counts that session, the
#    step queues it, the verify step runs verify --codex;
# 2. client uninstall --codex: the plugins and hook trust gone, config.toml
#    back to its start but for what Codex wrote, no Codex session read after.
# One line per step in parity/results.txt.
set -euo pipefail
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
fx=/out/fixtures/parity
mkdir -p "$fx"
result() { echo "$*" | tee -a "$fx/results.txt" >&2; }
export CODEX_HOME=$HOME/codex-pg
mkdir -p "$CODEX_HOME"
cd "$HOME/work"
cp /out/config-start.toml "$CODEX_HOME/config.toml"
cp "$CODEX_HOME/config.toml" "$fx/config-start.toml"

# --- 1 ---------------------------------------------------------------------
tui
wait_for "Ask Codex to do anything"
prompt p1 "a past session" '[{"say":"PAST-DONE"}]'
paste /tmp/prompts/p1
wait_for "• PAST-DONE" 60 # the model's answer, not the pasted prompt
slash /quit
sleep 2
tmux kill-server 2>/dev/null || true
result "1 rollouts: $(find "$CODEX_HOME/sessions" -name 'rollout-*.jsonl*' | wc -l)"
backsight setup --questions --answer tools=codex --answer scheduler=off --json > "$fx/questions.json" 2> "$fx/questions.err" || true
result "1 question: $(jq -r '[.data.questions[] | select(.id=="backfill") | .prompt + " default=" + .default] | join("")' "$fx/questions.json")"
backsight setup --answer tools=codex --answer scheduler=off --answer backfill=yes --answer consent=yes --non-interactive --json > "$fx/setup.json" 2> "$fx/setup.err" || true
result "1 setup: ok=$(jq -r .ok "$fx/setup.json") $(jq -r '[.data.steps[] | select(.name|test("codex$|verify|backfill")) | .name + "=" + .status + " (" + .detail + ")"] | join("; ")' "$fx/setup.json")"
backsight retain backfill --status --json > "$fx/backfill-status.json" 2>&1 || true
result "1 queued: $(jq -r '.data.total // .data' "$fx/backfill-status.json" | tr '\n' ' ')"
backsight verify --codex --no-round-trip --json > "$fx/verify.json" 2> "$fx/verify.err" || true
result "1 verify --codex: ok=$(jq -r .ok "$fx/verify.json") $(jq -r '[.data.checks[] | .id + "=" + .status] | join(" ")' "$fx/verify.json")"
result "1 verify mcp: $(jq -r '.data.checks[] | select(.id=="mcp") | .summary' "$fx/verify.json")"

# --- 2 ---------------------------------------------------------------------
trust_all
tmux kill-server 2>/dev/null || true
result "2 trusted: $(grep -c '^\[hooks.state."backsight-hooks@backsight' "$CODEX_HOME/config.toml") backsight trust tables"
backsight client uninstall --codex --plan --json > "$fx/uninstall-plan.json" 2>&1 || true
result "2 plan: $(jq -r '[.data.steps[] | .name + "=" + .status] | join(" ")' "$fx/uninstall-plan.json")"
backsight client uninstall --codex --json > "$fx/uninstall.json" 2> "$fx/uninstall.err" || true
result "2 uninstall: ok=$(jq -r .ok "$fx/uninstall.json") $(jq -r '[.data.steps[] | .name + "=" + .status + " (" + .detail + ")"] | join("; ")' "$fx/uninstall.json")"
codex plugin list --json > "$fx/plugin-list-after.json"
result "2 plugins after: $(jq -r '.installed | length' "$fx/plugin-list-after.json")"
if diff "$fx/config-start.toml" "$CODEX_HOME/config.toml" > "$fx/after.diff"; then
  result "2 config.toml: byte-identical to its start"
else
  result "2 config.toml: differs from its start: $(grep '^[<>]' "$fx/after.diff" | tr '\n' ' ')"
fi
result "2 state codex: $(jq -c '.clients.codex // "none"' "$BACKSIGHT_HOME/state/state.json" 2>/dev/null || echo unreadable)"
backsight retain backfill --plan --tools codex --json > "$fx/backfill-after.json" 2>&1 || true
result "2 backfill --tools codex after: $(jq -r '.error.message // .data.plan.send' "$fx/backfill-after.json")"
backsight retain waiting --json > "$fx/waiting-after.json" 2>&1 || true
result "2 waiting after: $(jq -r '[.data.sessions[]? | .session] | length' "$fx/waiting-after.json") session(s)"
backsight verify --codex --no-round-trip --json > "$fx/verify-after.json" 2>&1 || true
result "2 verify --codex after: $(jq -r '.error.message // .ok' "$fx/verify-after.json")"
