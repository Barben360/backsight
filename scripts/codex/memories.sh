#!/usr/bin/env bash
# Codex's own memories on disk (run in the prepared container:
# HARNESS_CMD="/opt/harness/memories.sh <variant>" scripts/codex/run.sh).
# Variants: v2 (a fresh profile, Codex's default), v1 (memories.version =
# "v1"), dual (v1 with dual_write: both stores). It turns the feature on,
# makes a few interactive sessions, ages them past the idle floor (one
# hour, the least Codex accepts) in Codex's state database, then starts one
# more session, which runs the extraction and the consolidation against the
# fake model. What Codex wrote lands in /out/fixtures/memories/<variant>/.
set -euo pipefail
variant=${1:?variant: v2, v1 or dual}
fx=/out/fixtures/memories/$variant
mkdir -p "$fx" /tmp/prompts
cd "$HOME/work"
export TERM=xterm-256color

{
  echo
  echo '[features]'
  echo 'memories = true'
  echo
  echo '[memories]'
  echo 'min_rollout_idle_hours = 1'
  echo 'max_rollouts_per_startup = 8'
  case $variant in
    v1) echo 'version = "v1"' ;;
    dual) echo 'version = "v1"'; echo 'dual_write = true' ;;
  esac
} >> "$CODEX_HOME/config.toml"

screen() { tmux capture-pane -p -t t -S -200; }
wait_for() {
  local i
  for ((i = 0; i < ${2:-60}; i++)); do
    screen | grep -qF -- "$1" && return 0
    sleep 0.5
  done
  echo "memories: timed out waiting for: $1" >&2
  screen | grep -v '^\s*$' | tail -20 >&2
  return 1
}
session() { # session <n>: one interactive session with one scripted turn
  printf 'session %s: remember that the build uses make check <<fake [{"call":"exec_command","args":{"cmd":"echo build %s"}},{"say":"S%s-DONE"}]>>' "$1" "$1" "$1" > "/tmp/prompts/s$1"
  tmux kill-server 2>/dev/null || true
  tmux new-session -d -s t -x 220 -y 50 "codex --no-daemon; sleep 3600"
  if wait_for "Hooks need review" 20 2>/dev/null; then
    tmux send-keys -t t Down Down # 3. continue without trusting: hooks play no part here
    sleep 0.3
    tmux send-keys -t t Enter
  fi
  wait_for "Ask Codex to do anything"
  tmux load-buffer "/tmp/prompts/s$1"
  tmux paste-buffer -t t
  sleep 0.5
  tmux send-keys -t t Enter
  wait_for "• S$1-DONE"
  sleep 2
  tmux send-keys -t t -l /exit
  sleep 0.3
  tmux send-keys -t t Enter
  sleep 3
}

for n in 1 2 3; do session "$n"; done
# Past the idle floor: the threads' last update two hours ago.
sqlite3 "$CODEX_HOME/state_5.sqlite" "UPDATE threads SET updated_at_ms = updated_at_ms - 7200000;"
# The first sessions' consolidation (of an empty store) succeeded: Codex
# waits 6 hours before the next one. Its end, 7 hours earlier.
for db in "$CODEX_HOME"/memories*_1.sqlite; do
  sqlite3 "$db" "UPDATE jobs SET finished_at = finished_at - 25200 WHERE finished_at IS NOT NULL;"
done
# The next session starts the pipeline in the background.
session 4
tmux new-session -d -s w "sleep 3600"
# Done once each store holds the three sessions' summaries and its
# consolidation finished (Codex removes the workspace diff then). The first
# sessions already ran a consolidation on an empty store.
for _ in $(seq 1 180); do
  done=1
  for d in $(case $variant in v2) echo memories_v2 ;; v1) echo memories ;; dual) echo memories memories_v2 ;; esac); do
    n=$(find "$CODEX_HOME/$d/rollout_summaries" -name '*.md' 2>/dev/null | wc -l)
    { [ "$n" -ge 3 ] && [ ! -e "$CODEX_HOME/$d/phase2_workspace_diff.md" ]; } || done=0
  done
  [ "$done" = 1 ] && break
  sleep 1
done
tmux kill-server 2>/dev/null || true

for d in memories memories_v2 memories_extensions; do
  root=$CODEX_HOME/$d
  [ -d "$root" ] || continue
  {
    echo "# $d: every file, with its size in bytes (.git listed, not copied)"
    (cd "$root" && find . -path ./.git -prune -o -type f -printf '%P %s\n' | sort)
    [ -d "$root/.git" ] && echo ".git/ (a git repository: $(git -C "$root" log --oneline 2>/dev/null | wc -l) commit(s))"
  } > "$fx/$d.listing.txt"
  mkdir -p "$fx/$d"
  (cd "$root" && find . \( -path ./.git -o -path ./.codex-baseline \) -prune -o -type f -print0 | xargs -0 -r cp --parents -t "$fx/$d")
done
ls "$CODEX_HOME" | grep -i memor > "$fx/codex-home-memory-entries.txt" || true
sqlite3 "$CODEX_HOME/state_5.sqlite" ".tables" > "$fx/state-tables.txt"
for db in "$CODEX_HOME"/memories*_1.sqlite; do
  [ -f "$db" ] && { echo "== $(basename "$db")"; sqlite3 "$db" ".tables"; } >> "$fx/state-tables.txt"
done
echo "memories: done ($variant)" >&2
