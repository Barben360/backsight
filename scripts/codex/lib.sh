# shellcheck shell=bash
# Helpers shared by the scenarios that drive Codex's TUI in the container
# (record.sh, memories.sh, checks.sh): the TUI runs in tmux session t.
export TERM=xterm-256color

log() { echo "codex harness: $*" >&2; }
tui() { # tui [codex args...]: a new TUI in session t
  tmux kill-server 2>/dev/null || true
  tmux new-session -d -s t -x 220 -y 50 "codex --no-daemon $*; sleep 3600"
}
screen() { tmux capture-pane -p -t t -S -200; }
shot() { screen | grep -v '^\s*$' > "$1"; } # shot <file>: the screen, blank lines dropped
wait_for() { # wait_for <fixed text> [tries of 0.5 s]
  local i
  for ((i = 0; i < ${2:-60}; i++)); do
    screen | grep -qF -- "$1" && return 0
    sleep 0.5
  done
  log "timed out waiting for: $1"
  screen | grep -v '^\s*$' | tail -30 >&2
  return 1
}
paste() { tmux load-buffer "$1"; tmux paste-buffer -t t; sleep 0.5; tmux send-keys -t t Enter; }
slash() { tmux send-keys -t t -l "$1"; sleep 0.5; tmux send-keys -t t Enter; }
# prompt <file> <text> <steps json>: a prompt carrying a fake-model script.
prompt() { mkdir -p /tmp/prompts; printf '%s <<fake %s>>' "$2" "$3" > "/tmp/prompts/$1"; }
quiet() { "$@" < /dev/null > /dev/null 2>&1; }
# trust_all: a new TUI; trust every hook it asks about; leaves it open.
trust_all() {
  tui
  if wait_for "Hooks need review" 20 2>/dev/null; then
    tmux send-keys -t t Down # 2. Trust all and continue
    sleep 0.3
    tmux send-keys -t t Enter
  fi
  wait_for "Ask Codex to do anything"
}
