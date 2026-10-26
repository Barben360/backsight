#!/usr/bin/env bash
# replay-families.sh: the recall replay that decides recall.families.
#
# Usage: scripts/replay-families.sh <data dir> <backsight binary>
#
# <data dir>/prompts.json holds frozen prompts, [{qid, cwd, text, lang}]
# (the recall bench's format). Each prompt runs through
# `backsight recall-prompt --json --stdin --cwd <cwd>` twice, one at a time
# (parallel runs overload a CPU reranker), in two throwaway BACKSIGHT_HOMEs
# made of copies of the source config, secrets, entity index and families
# file: "base" with recall.families = false, "families" with it true. The
# source is BACKSIGHT_SOURCE_CONFIG and BACKSIGHT_SOURCE_STATE (defaults:
# ~/.config/backsight and ~/.local/state/backsight); they are only read, and
# the server they point at only answers recalls. Run it after the
# families file is built (jobs tick) and, for a fair reading, after
# `backsight retain reattribute` moved the past documents.
#
# Writes <data dir>/replay-base.json and replay-families.json:
#   {qid: {ranked: [[id, score]], injected: [ids], block, ms, project,
#          linked: [...], family: [...], projects: {id: project}, scopes: {id: scope}}}
# merges the memory texts into <data dir>/mem.json (the throwaway homes, with
# their copy of the secrets, are removed at the end), and prints per variant:
# the prompts whose project has a family, the injections, and the
# out-of-family injections (an injected memory from outside the project
# scope whose project is neither the prompt's nor in its family).
# The blind grading and nDCG@3 / useful-injection comparison are the
# bench's judge and score steps on these files (score <dir> base families).
set -euo pipefail
[ $# -eq 2 ] || { sed -n '3,4p' "$0" >&2; exit 2; }
data=$(cd "$1" && pwd)
bin=$2
src_cfg=${BACKSIGHT_SOURCE_CONFIG:-$HOME/.config/backsight}
src_state=${BACKSIGHT_SOURCE_STATE:-$HOME/.local/state/backsight}
[ -s "$data/prompts.json" ] || { echo "no $data/prompts.json" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is needed" >&2; exit 2; }
[ -s "$src_state/families.json" ] || echo "warning: no families file in $src_state: run backsight jobs tick first" >&2
# The homes hold a copy of secrets.toml: removed on exit (KEEP_HOMES=1 keeps them).
cleanup() { [ "${KEEP_HOMES:-}" = 1 ] || rm -rf "$data/homes"; }
trap cleanup EXIT

# home <variant> <families>: a BACKSIGHT_HOME holding copies of the source.
home() {
  local h=$data/homes/$1
  rm -rf "$h"
  mkdir -p "$h/config" "$h/state" "$h/data"
  chmod 700 "$h" "$h/config" "$h/state"
  cp "$src_cfg/config.toml" "$h/config/config.toml"
  install -m 600 "$src_cfg/secrets.toml" "$h/config/secrets.toml"
  for f in entities.json families.json; do
    [ ! -f "$src_state/$f" ] || cp "$src_state/$f" "$h/state/$f"
  done
  # recall.families = <families>, in [recall] whether the section exists or not
  awk -v v="$2" '
    /^\[/ { if (inrecall && !done) { print "families = " v; done = 1 } inrecall = ($0 == "[recall]") }
    inrecall && /^[[:space:]]*families[[:space:]]*=/ { if (!done) { print "families = " v; done = 1 } next }
    { print }
    END { if (!done) { if (!inrecall) print "[recall]"; print "families = " v } }
  ' "$h/config/config.toml" > "$h/config/config.toml.new"
  mv "$h/config/config.toml.new" "$h/config/config.toml"
  echo "$h"
}

[ -f "$data/mem.json" ] || echo '{}' > "$data/mem.json"
n=$(jq length "$data/prompts.json")
for variant in base families; do
  on=false
  [ "$variant" = families ] && on=true
  h=$(home "$variant" "$on")
  out=$data/replay-$variant.json
  echo '{}' > "$out"
  failed=0
  for i in $(seq 0 $((n - 1))); do
    qid=$(jq -r ".[$i].qid" "$data/prompts.json")
    cwd=$(jq -r ".[$i].cwd" "$data/prompts.json")
    start=$(date +%s%3N)
    if res=$(jq -r ".[$i].text" "$data/prompts.json" | BACKSIGHT_HOME=$h "$bin" recall-prompt --json --stdin --cwd "$cwd" 2>/dev/null) &&
      jq -e '.data' >/dev/null 2>&1 <<<"$res"; then
      ms=$(($(date +%s%3N) - start))
      jq --arg q "$qid" --argjson ms "$ms" --argjson r "$res" '.[$q] = ($r.data | {
          ranked: [.ranked[] | [.id, .score]], injected, block, ms: $ms, project, linked, family,
          projects: ([.ranked[] | {(.id): .project}] | add // {}),
          scopes: ([.ranked[] | {(.id): .scope}] | add // {})})' "$out" > "$out.new"
      mv "$out.new" "$out"
      jq --argjson r "$res" '. + ([$r.data.ranked[] | {(.id): .text}] | add // {})' "$data/mem.json" > "$data/mem.json.new"
      mv "$data/mem.json.new" "$data/mem.json"
    else
      failed=$((failed + 1))
      jq --arg q "$qid" '.[$q] = {ranked: [], injected: [], block: "", ms: 0, failed: true}' "$out" > "$out.new"
      mv "$out.new" "$out"
    fi
  done
  jq -r --arg v "$variant" --argjson failed "$failed" '
    [.[] | select(.failed | not)] as $ok
    | [$ok[] | . as $r | .injected[] | . as $id | ($r.projects[$id] // "") as $p
        | select(($r.scopes[$id] // "") != "project" and $p != $r.project and (any(($r.family // [])[]; . == $p) | not))] as $out
    | "\($v): \($ok | length) prompts (\($failed) failed), \([$ok[] | select((.family // []) | length > 0)] | length) with a family, \([$ok[].injected[]] | length) injections, \($out | length) out of family"' "$out"
done
