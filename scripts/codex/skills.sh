#!/usr/bin/env bash
# backsight's skills in a real Codex session (Codex epic, issue 282), run in
# the prepared container:
#   HARNESS_CMD=/opt/harness/skills.sh scripts/codex/run.sh
# backsight's plugins installed with `backsight client install --codex` on a
# throwaway CODEX_HOME, the hooks trusted in Codex's own review screen; then
# one `codex exec` per skill, its prompt naming it as a user does
# ($backsight:<name>), the fake model running the commands the installed
# SKILL.md gives. Per skill, in skills/results.txt: whether Codex lists it,
# whether the request carries the skill's own text (the mention loaded it),
# and what its commands answered. The model is fake: whether a real model
# follows the text is for a live session with one.
set -euo pipefail
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
fx=/out/fixtures/skills
mkdir -p "$fx"
result() { echo "$*" | tee -a "$fx/results.txt" >&2; }
export CODEX_HOME=$HOME/codex-sk
mkdir -p "$CODEX_HOME"
cp /out/config-start.toml "$CODEX_HOME/config.toml"
cd "$HOME/work"
git commit -q --allow-empty -m "billing: keep PostgreSQL 16" 2>/dev/null || git -c user.name=t -c user.email=t@t commit -q --allow-empty -m "billing: keep PostgreSQL 16"

backsight client install --codex --json > "$fx/install.json" 2> "$fx/install.err" || true
result "install: ok=$(jq -r .ok "$fx/install.json")"
trust_all
tmux kill-server 2>/dev/null || true
skills=$(find "$CODEX_HOME/plugins/cache/backsight/backsight" -path '*/skills' -type d | head -1)
shim=$BACKSIGHT_HOME/data/bin/current
result "skills installed: $(cd "$skills" && ls | tr '\n' ' ')"
for f in "$skills"/*/SKILL.md; do
  grep -qE 'allowed-tools|argument-hint|\$ARGUMENTS|/backsight:|!`' "$f" && result "$(basename "$(dirname "$f")"): Claude Code syntax left in SKILL.md" || true
done

# cmds <skill> <n>: the first n commands on the shim the skill's text gives.
cmds() { grep -o "\`$shim [^\`]*\`" "$skills/$1/SKILL.md" | tr -d '`' | head -"$2"; }
# steps <cmd>...: a fake-model script running each command, then a word.
steps() {
  local s="[" c
  for c in "$@"; do s+=$(jq -cn --arg c "$c" '{call: "exec_command", args: {cmd: $c}}')","; done
  echo "$s{\"say\":\"done\"}]"
}
# skill <name> <request> <steps>: one codex exec; the model's requests of
# that run land in $fx/<name>/.
skill() {
  local name=$1 before
  before=$(find /out/model -name '*.json' | wc -l)
  prompt "$name" "\$backsight:$name $2" "$3"
  codex exec --json "$(cat "/tmp/prompts/$name")" < /dev/null > "$fx/$name.jsonl" 2> "$fx/$name.err" || true
  mkdir -p "$fx/$name"
  find /out/model -name '*.json' | sort | tail -n +"$((before + 1))" | xargs -r cp -t "$fx/$name"
  local first last body
  first=$(find "$fx/$name" -name '*.json' | sort | head -1)
  last=$(find "$fx/$name" -name '*.json' | sort | tail -1)
  # The skill's first line of instructions, as the request would carry it.
  body=$(sed -n '/^---$/,/^---$/!p' "$skills/$name/SKILL.md" | grep -m1 -v '^$' | cut -c1-60)
  local listed=no loaded=no
  grep -qF "backsight:$name: " "$first" && listed=yes
  # Codex hands a named skill's whole file to the model as a <skill> item
  # (no pipe into grep -q here: pipefail would read its early exit as a miss).
  jq -r '.. | strings' "$first" > /tmp/request-strings
  grep -qF "<name>backsight:$name</name>" /tmp/request-strings && grep -qF -- "$body" /tmp/request-strings && loaded=yes
  result "$name: listed=$listed loaded=$loaded hooks=$(grep -ci 'hook: .*failed' "$fx/$name.err" || true) failed"
  # Each command's output, as the model read it.
  jq -r '.body.input[]? | select(.type=="function_call_output") | .output' "$last" 2>/dev/null |
    sed 's/^/    /' | head -12 | tee -a "$fx/results.txt" >&2 || true
}

skill remember "Billing keeps PostgreSQL 16 for partial indexes." \
  '[{"call":"memory_store","args":{"text":"Decision (2026-11-11): billing keeps PostgreSQL 16 because partial indexes are required.","kind":"decision"}},{"say":"done"}]'
mapfile -t c < <(cmds pause 1)
skill pause "" "$(steps "${c[@]}")"
sid=$(jq -r 'select(.type=="thread.started") | .thread_id' "$fx/pause.jsonl" | head -1)
result "pause: session $sid paused=$(jq -r .paused "$BACKSIGHT_HOME/state/sessions/$sid.json" 2>/dev/null || echo missing)"
skill memory-retro "" "$(steps "git log --oneline -15" "git branch --show-current" "$shim why")"
skill memory "billing postgres" "$(steps "$shim search 'billing postgres'" "$shim stats overview")"
skill doctor "is memory working?" "$(steps "$shim doctor")"
mapfile -t c < <(cmds update 2)
skill update "" "$(steps "${c[@]}")"
log "skills done: $fx"
