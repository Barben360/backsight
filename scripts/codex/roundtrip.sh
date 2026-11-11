#!/usr/bin/env bash
# The install round trip on a throwaway CODEX_HOME (Codex epic, issue 278),
# run in the prepared container:
#   HARNESS_CMD=/opt/harness/roundtrip.sh scripts/codex/run.sh
# A: setup (Codex alone, its memories off) → a session (hooks trusted in the
#    TUI, a codex exec turn) → an update (backsight-next: client install
#    --codex) → setup --undo → uninstall;
# B: client install --codex → trusted → codex-memories off → uninstall.
# After each, config.toml is compared with its start: only what Codex wrote
# itself may differ. One line per step in roundtrip/results.txt.
set -euo pipefail
# shellcheck source=scripts/codex/lib.sh
. /opt/harness/lib.sh
fx=/out/fixtures/roundtrip
mkdir -p "$fx"
result() { echo "$*" | tee -a "$fx/results.txt" >&2; }
export CODEX_HOME=$HOME/codex-rt
mkdir -p "$CODEX_HOME"
cd "$HOME/work"
# The start: prepare.sh's config (the fake model), Codex's memories on.
cp /out/config-start.toml "$CODEX_HOME/config.toml"
printf '\n[features]\nmemories = true\n' >> "$CODEX_HOME/config.toml"
cp "$CODEX_HOME/config.toml" "$fx/config-start.toml"
# compare <label>: config.toml against the start; the diff kept.
compare() {
  if diff "$fx/config-start.toml" "$CODEX_HOME/config.toml" > "$fx/$1.diff"; then
    result "$1: config.toml byte-identical to its start"
  else
    result "$1: config.toml differs from its start: $(grep -c '^[<>]' "$fx/$1.diff") line(s), see $1.diff"
  fi
  echo "cache: $(find "$CODEX_HOME/plugins/cache" -mindepth 1 2>/dev/null | sed "s#^$CODEX_HOME/##" | tr '\n' ' ')" >> "$fx/$1.diff"
}
# hooks_ran <label>: one codex exec turn; Codex's own hook lines on stderr.
hooks_ran() {
  prompt e "$1" '[{"say":"ok"}]'
  codex exec -s workspace-write "$(cat /tmp/prompts/e)" < /dev/null > /dev/null 2> "$fx/$1.exec.err" || true
  grep -i 'hook' "$fx/$1.exec.err" | sort | uniq -c | tr '\n' ';' || true
}

# --- A ---------------------------------------------------------------------
backsight setup --answer tools=codex --answer scheduler=off --answer consent=yes --non-interactive --json > "$fx/a-setup.json" 2> "$fx/a-setup.err" || true
result "a1 setup: ok=$(jq -r .ok "$fx/a-setup.json") steps: $(jq -r '[.data.steps[] | select(.name|test("codex|client|import")) | .name + "=" + .status] | join(" ")' "$fx/a-setup.json")"
result "a1 setup next: $(jq -r '.data.next_steps | join(" | ")' "$fx/a-setup.json")"
grep -q 'memories' "$CODEX_HOME/config.toml" && result "a1 memories: still in config.toml" || result "a1 memories: removed from config.toml"
codex plugin list --json > "$fx/a-plugin-list.json"
result "a1 plugins: $(jq -r '[.installed[] | .pluginId + "@" + .version] | join(" ")' "$fx/a-plugin-list.json")"
result "a2 before trust: $(hooks_ran a2-untrusted)"
trust_all
tmux kill-server 2>/dev/null || true
cp "$CODEX_HOME/config.toml" "$fx/a-config-trusted.toml"
result "a2 trusted: $(grep -c '^\[hooks.state."backsight-hooks@backsight' "$fx/a-config-trusted.toml") backsight trust tables; $(hooks_ran a2-trusted)"
mkdir -p "$HOME/next"
cp /opt/bs-build/backsight-next "$HOME/next/backsight"
"$HOME/next/backsight" client install --codex --json > "$fx/a-update.json" 2> "$fx/a-update.err" || true
result "a3 update: ok=$(jq -r .ok "$fx/a-update.json") codex: $(jq -r '[.data.steps[] | select(.name=="codex" or .name=="trust") | .detail] | join(" | ")' "$fx/a-update.json") trust=$(jq -r '.data.trust // ""' "$fx/a-update.json")"
result "a3 after update: $(hooks_ran a3-updated)"
backsight setup --undo --json > "$fx/a-undo.json" 2> "$fx/a-undo.err" || true
result "a4 undo: ok=$(jq -r .ok "$fx/a-undo.json") $(jq -r '[.data.actions[] | .name + "=" + .status] | join(" ")' "$fx/a-undo.json")"
codex plugin list --json > "$fx/a-plugin-list-after-undo.json"
result "a4 plugins after undo: $(jq -r '.installed | length' "$fx/a-plugin-list-after-undo.json")"
compare a4-after-undo
backsight uninstall --json > "$fx/a-uninstall.json" 2> "$fx/a-uninstall.err" || true
result "a5 uninstall: ok=$(jq -r .ok "$fx/a-uninstall.json")"
compare a5-after-uninstall

# --- B ---------------------------------------------------------------------
cp /opt/bs-build/backsight "$HOME/bin/backsight"
backsight client install --codex --json > "$fx/b-install.json" 2> "$fx/b-install.err" || true
result "b1 install: ok=$(jq -r .ok "$fx/b-install.json") trust=$(jq -r '.data.trust // ""' "$fx/b-install.json" | cut -c1-40)…"
trust_all
tmux kill-server 2>/dev/null || true
backsight client codex-memories off --json > "$fx/b-memories.json" 2>&1 || true
result "b2 memories off: ok=$(jq -r .ok "$fx/b-memories.json") $(jq -r '[.data.files[].changes[].path] | join(" ")' "$fx/b-memories.json")"
result "b2 session: $(hooks_ran b2-trusted)"
backsight uninstall --json > "$fx/b-uninstall.json" 2> "$fx/b-uninstall.err" || true
result "b3 uninstall: ok=$(jq -r .ok "$fx/b-uninstall.json") $(jq -r '[.data.steps[] | select(.name|startswith("codex")) | .status + ": " + .detail] | join(" | ")' "$fx/b-uninstall.json")"
compare b3-after-uninstall
log "round trip done: $fx"
