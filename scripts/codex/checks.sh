#!/usr/bin/env bash
# The hook-trust, approval and identity checks of the Codex harness, as
# fixtures (run in the prepared container, with the generated tree kept as
# is: CODEXIFY=0 HARNESS_CMD=/opt/harness/checks.sh scripts/codex/run.sh).
# Each step writes its evidence under /out/fixtures/checks/ and one line in
# checks/results.txt:
#   1. the tree as generated: the review screen shows the hook command with
#      no arguments, and a trusted hook runs the shim with none;
#   2. one plugin, adapted for Codex: trusted, then a replaced binary behind
#      the shim, a changed skill, a bumped version;
#   3. two plugins (the hooks alone in backsight-hooks): trusted, then a
#      changed skill in the main plugin, a bumped hooks plugin;
#   4. approvals with the hints on memory_store and memory_credit (the tap
#      adds them to the real server's tools/list): the TUI, then codex exec;
#   5. the MCP _meta session against the hooks' session_id after /clear,
#      resume and fork;
#   6. what `codex plugin remove` and `marketplace remove` leave behind;
#   7. that the container cannot reach the internet.
set -euo pipefail
fx=/out/fixtures/checks
mkdir -p "$fx"
cd "$HOME/work"
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
mkt=$BACKSIGHT_HOME/data/marketplace
main=$mkt/plugins/backsight
result() { echo "$*" | tee -a "$fx/results.txt" >&2; }
starts() { find /out/tap -name '*-hook-session-start.in.json' | wc -l; }
# exec_ran <label>: one codex exec turn; did backsight's SessionStart hook run?
exec_ran() {
  local before after
  before=$(starts)
  prompt e "$1" '[{"say":"ok"}]'
  quiet codex exec -s workspace-write "$(cat /tmp/prompts/e)" || true
  after=$(starts)
  if [ "$after" -gt "$before" ]; then echo yes; else echo no; fi
}
# review_screen <file>: a new TUI; its first screen (the review prompt, or
# the composer when nothing needs review); the TUI is left closed.
review_screen() {
  tui
  wait_for "Ask Codex to do anything" 20 2>/dev/null || wait_for "Hooks need review" 10
  sleep 1
  shot "$1"
  tmux kill-server 2>/dev/null || true
}
cache_of() { ls -d "$CODEX_HOME/plugins/cache/backsight/$1"/*/ | head -1; }

# --- 1. the generated tree as is ------------------------------------------
log "1. as generated"
mkdir -p "$fx/1-as-generated"
cp "$(cache_of backsight)hooks/hooks.json" "$fx/1-as-generated/hooks.json"
tui
wait_for "Hooks need review"
tmux send-keys -t t Enter # 1. Review hooks
wait_for "need review before they can run"
tmux send-keys -t t Enter # PreToolUse: the user-level probe, then backsight's hook
sleep 1
for _ in 1 2 3; do
  screen | grep -qF "Plugin - backsight@backsight" && break
  tmux send-keys -t t Down
  sleep 0.5
done
shot "$fx/1-as-generated/review-hook.txt"
tmux send-keys -t t Escape
sleep 0.5
tmux send-keys -t t t # trust all
sleep 1
tmux kill-server 2>/dev/null || true
before=$(find /out/tap -name '*-other.args' | wc -l)
prompt a "as generated" '[{"say":"ok"}]'
quiet codex exec -s workspace-write "$(cat /tmp/prompts/a)" || true
noargs=$(find /out/tap -name '*-other.args' -newer "$fx/1-as-generated/review-hook.txt" -exec head -1 {} \; | grep -c '^0$' || true)
result "1 as-generated: trusted hooks ran the shim with no arguments: $noargs times (before: $before); backsight hook runs: $(starts)"
grep -F "Command" "$fx/1-as-generated/review-hook.txt" > "$fx/1-as-generated/review-command.txt" || true

# --- 2. one plugin, adapted for Codex -------------------------------------
log "2. one plugin"
mkdir -p "$fx/2-one-plugin"
/opt/harness/codexify.sh "$mkt"
quiet codex plugin add backsight@backsight
review_screen "$fx/2-one-plugin/review-after-adapt.txt"
trust_all
tmux kill-server 2>/dev/null || true
cp "$CODEX_HOME/config.toml" "$fx/2-one-plugin/config-trusted.toml"
result "2a one plugin, trusted: hooks run: $(exec_ran trusted)"
shim=$BACKSIGHT_HOME/data/bin/current
real=$(sed -n 's/^real=//p' "$shim")
mkdir -p "$HOME/bs2"
cp "$real" "$HOME/bs2/backsight"
printf x >> "$HOME/bs2/backsight" # other bytes: another binary
sed -i "s#^real=.*#real=$HOME/bs2/backsight#" "$shim"
result "2b binary behind the shim replaced ($(sha256sum "$real" | cut -c1-12) -> $(sha256sum "$HOME/bs2/backsight" | cut -c1-12)): hooks run: $(exec_ran binary)"
echo "<!-- changed -->" >> "$main/skills/pause/SKILL.md"
quiet codex plugin add backsight@backsight
result "2c a skill changed, plugin added again: hooks run: $(exec_ran skill)"
review_screen "$fx/2-one-plugin/review-after-skill-change.txt"
trust_all
tmux kill-server 2>/dev/null || true
jq '.version += ".bumped"' "$main/.claude-plugin/plugin.json" > /tmp/pj && cp /tmp/pj "$main/.claude-plugin/plugin.json"
quiet codex plugin add backsight@backsight
result "2d version bumped, plugin added again: hooks run: $(exec_ran version)"
review_screen "$fx/2-one-plugin/review-after-version-bump.txt"

# --- 3. two plugins: the hooks alone ---------------------------------------
log "3. two plugins"
mkdir -p "$fx/3-two-plugins"
hp=$mkt/plugins/backsight-hooks
mkdir -p "$hp/.claude-plugin"
mv "$main/hooks" "$hp/hooks"
echo '{"name":"backsight-hooks","version":"1.0.0","description":"backsight hooks","author":{"name":"backsight"}}' > "$hp/.claude-plugin/plugin.json"
jq '.plugins += [{"name":"backsight-hooks","source":"./plugins/backsight-hooks","description":"backsight hooks"}]' "$mkt/.claude-plugin/marketplace.json" > /tmp/mk && cp /tmp/mk "$mkt/.claude-plugin/marketplace.json"
quiet codex plugin add backsight@backsight
quiet codex plugin add backsight-hooks@backsight
codex plugin list --json > "$fx/3-two-plugins/plugin-list.json"
find "$mkt" -type f | sed "s#^$mkt/##" | sort > "$fx/3-two-plugins/marketplace-files.txt"
review_screen "$fx/3-two-plugins/review-after-split.txt"
trust_all
tmux kill-server 2>/dev/null || true
cp "$CODEX_HOME/config.toml" "$fx/3-two-plugins/config-trusted.toml"
result "3a two plugins, trusted: hooks run: $(exec_ran split)"
echo "<!-- changed again -->" >> "$main/skills/pause/SKILL.md"
quiet codex plugin add backsight@backsight
result "3b a skill changed in the main plugin, added again: hooks run: $(exec_ran split-skill)"
review_screen "$fx/3-two-plugins/review-after-main-change.txt"
jq '.version = "1.0.1"' "$hp/.claude-plugin/plugin.json" > /tmp/pj && cp /tmp/pj "$hp/.claude-plugin/plugin.json"
quiet codex plugin add backsight-hooks@backsight
result "3c the hooks plugin's version bumped, added again: hooks run: $(exec_ran split-hooks)"
review_screen "$fx/3-two-plugins/review-after-hooks-change.txt"

# --- 4. approvals with the hints -------------------------------------------
log "4. approvals with hints"
mkdir -p "$fx/4-approvals-with-hints"
touch /tmp/bs-hints
trust_all
prompt h "hints" '[{"call":"memory_store","args":{"kind":"decision","text":"Decision (2026-11-10): the approvals check runs with the hints."}},{"call":"memory_credit","args":{"verdict":"none"}},{"say":"H-DONE"}]'
paste /tmp/prompts/h
asked=no
for _ in $(seq 1 60); do
  screen | grep -qF "Allow the backsight MCP server" && { asked=yes; break; }
  screen | grep -qF "• H-DONE" && break
  sleep 0.5
done
shot "$fx/4-approvals-with-hints/tui-screen.txt"
tmux kill-server 2>/dev/null || true
result "4a TUI, memory_store and memory_credit with destructiveHint/openWorldHint false: approval asked: $asked"
f=$(find /out/tap -name '*-mcp.out.jsonl' -newer /tmp/bs-hints | sort | head -1)
jq -s '[.[] | select(.result.tools?) | .result.tools[] | {name, annotations}]' "$f" > "$fx/4-approvals-with-hints/tools-list-annotations.json"
codex exec -s workspace-write --json "$(cat /tmp/prompts/h)" < /dev/null 2>/dev/null |
  jq -c 'select(.type=="item.completed" and .item.type=="mcp_tool_call") | .item | {tool, status, error: (.error.message // (if .status == "failed" then .result.content[0].text else null end))}' > "$fx/4-approvals-with-hints/exec-workspace-write.jsonl"
result "4b codex exec (workspace-write): $(jq -r '.tool + "=" + .status + (if .error then " (" + .error + ")" else "" end)' "$fx/4-approvals-with-hints/exec-workspace-write.jsonl" | tr '\n' ' ')"
rm -f /tmp/bs-hints

# --- 5. _meta.sessionId against the hooks' session_id ----------------------
log "5. identity after clear, resume, fork"
mkdir -p "$fx/5-identity"
search='[{"call":"memory_search","args":{"queries":["identity check"]}},{"say":"I-DONE"}]'
mark=$fx/5-identity/.start
touch "$mark"
trust_all
prompt i1 "identity first" "$search"
paste /tmp/prompts/i1
wait_for "• I-DONE"
sleep 2
slash /clear
sleep 2
prompt i2 "identity after clear" "$search"
paste /tmp/prompts/i2
wait_for "• I-DONE" && sleep 2
slash /exit
sleep 3
tmux kill-server 2>/dev/null || true
first=$(jq -r 'select(.prompt | startswith("identity first")) | .session_id' /out/probe/*-UserPromptSubmit.json | tail -1)
prompt i3 "identity resumed" "$search"
quiet codex exec resume "$first" "$(cat /tmp/prompts/i3)" || true
prompt i4 "identity forked" "$search"
quiet codex exec fork "$first" "$(cat /tmp/prompts/i4)" || true
# Pair each memory_search call's _meta with the hook payload of the same call.
for f in $(find /out/tap -name '*-mcp.in.jsonl' -newer "$mark"); do
  grep '"tools/call"' "$f" | jq -c '.params._meta | {callId, sessionId, threadId, turn: ."x-codex-turn-metadata".turn_id}'
done > /tmp/meta.jsonl
for p in /out/probe/*-PostToolUse.json; do
  [ "$p" -nt "$mark" ] || continue
  jq -c 'select(.tool_name=="mcp__backsight__memory_search") | {tool_use_id, session_id, turn_id}' "$p"
done > /tmp/hooks.jsonl
for p in /out/probe/*-UserPromptSubmit.json; do
  [ "$p" -nt "$mark" ] || continue
  jq -c 'select(.prompt | startswith("identity")) | {case: (.prompt | split(" <<")[0]), session_id, turn_id}' "$p"
done > /tmp/prompts.jsonl
jq -s --slurpfile hooks /tmp/hooks.jsonl --slurpfile cases /tmp/prompts.jsonl '
  map(. as $m | ($hooks | map(select(.tool_use_id == $m.callId and .turn_id == $m.turn)) | first) as $h
      | ($cases | map(select(.turn_id == $m.turn)) | first) as $c
      | {case: ($c.case // "?"), callId, meta_sessionId: .sessionId, meta_threadId: .threadId,
         hook_session_id: ($h.session_id // null), equal: (.sessionId == ($h.session_id // ""))})' /tmp/meta.jsonl > "$fx/5-identity/pairs.json"
result "5 identity: $(jq -r '.[] | .case + ": " + (.equal | tostring)' "$fx/5-identity/pairs.json" | tr '\n' ';')"
rm -f "$mark"

# --- 6. removal: what Codex's own commands leave ---------------------------
log "6. removal"
mkdir -p "$fx/6-removal"
cp "$CODEX_HOME/config.toml" "$fx/6-removal/config-before.toml"
for p in backsight backsight-hooks; do quiet codex plugin remove "$p@backsight" || true; done
quiet codex plugin marketplace remove backsight || true
cp "$CODEX_HOME/config.toml" "$fx/6-removal/config-after.toml"
diff "$fx/6-removal/config-before.toml" "$fx/6-removal/config-after.toml" > "$fx/6-removal/config.diff" || true
{
  echo "hooks.state tables left for backsight: $(grep -c '^\[hooks.state."backsight' "$fx/6-removal/config-after.toml" || true)"
  echo "plugins/cache/backsight left: $(find "$CODEX_HOME/plugins/cache/backsight" 2>/dev/null | sed "s#^$CODEX_HOME/##" | tr '\n' ' ')"
  echo ".hook-trust files: $(find "$CODEX_HOME/.hook-trust" -type f 2>/dev/null | wc -l)"
} > "$fx/6-removal/left.txt"
result "6 removal: $(tr '\n' ';' < "$fx/6-removal/left.txt")"

# --- 7. offline -----------------------------------------------------------
if curl -fsS -m 5 -o /dev/null https://github.com 2>/dev/null; then offline=no; else offline=yes; fi
result "7 the container cannot reach the internet: $offline"

# Scrub what identifies this machine (as collect.sh does).
install_id=$(cat "$CODEX_HOME/installation_id" 2>/dev/null || echo none)
find "$fx" -type f -print0 | xargs -0 sed -i -e "s/$install_id/00000000-0000-4000-8000-000000000000/g" -e "s/$(hostname)/codex-test/g"
log "checks done: $fx"
