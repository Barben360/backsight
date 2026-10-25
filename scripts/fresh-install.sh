#!/usr/bin/env bash
# The fresh-install proof (M4 block 4, plan 0063): backsight set up from
# nothing by `setup --non-interactive`, in an isolated HOME and Claude Code
# profile, on this machine's Docker with Hindsight's mock LLM, through to a
# stored memory found again. No hand edit: the only inputs are the answers
# and one `secret set`.
#
# Usage: scripts/fresh-install.sh [api port] (default 38888; the UI takes
# port + 1111). PROJECT names the Compose project and scheduler entries
# (default bs-fresh) so a machine's own install is left alone; KEEP=1
# leaves the stack and the work directory; WORK sets the work directory
# (it must allow executables).
set -euo pipefail
port=${1:-38888}
project=${PROJECT:-bs-fresh}
root=$(cd "$(dirname "$0")/.." && pwd)
work=${WORK:-$(mktemp -d)}
mkdir -p "$work"
bin=$work/bin/backsight
# Built before HOME moves: Go's caches stay the caller's.
(cd "$root" && go build -o "$bin" ./cmd/backsight)
export HOME=$work/home BACKSIGHT_HOME=$work/backsight
unset CLAUDE_CONFIG_DIR XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME
mkdir -p "$HOME"
compose() {
  docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"
}
cleanup() {
  status=$?
  if [ $status -ne 0 ] && [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ]; then
    compose logs --tail 40 hindsight >&2 || true
  fi
  if [ "${KEEP:-}" != 1 ]; then
    [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ] && compose down -v >/dev/null 2>&1 || true
    rm -rf "$work"
  fi
  exit $status
}
trap cleanup EXIT
fail() { echo "fresh-install: $*" >&2; exit 1; }

# 1. An agent's first call: the questions, then the answers.
"$bin" setup --questions --json > "$work/questions.json"
jq -e '.ok and (.data.questions | map(.id) | index("provider") != null)' "$work/questions.json" >/dev/null || fail "no question list"

# 2. Without the key, setup stops with exit 10 and names the secret.
answers=(--answer provider=mock --answer model=mock-model --answer consent=yes --answer retention=3650
  --answer project="$project" --answer api_port="$port" --answer ui_port=$((port + 1111)) --answer scheduler=off)
set +e
"$bin" setup --non-interactive --json "${answers[@]}" > "$work/needs-key.json" 2>/dev/null
code=$?
set -e
[ $code -eq 10 ] || fail "setup without a key: exit $code, want 10"
jq -e '.error.secrets == ["llm_api_key"]' "$work/needs-key.json" >/dev/null || fail "exit 10 does not name llm_api_key"
[ ! -e "$BACKSIGHT_HOME/config/config.toml" ] || fail "config.toml written before the key"

# 3. The user types the key (here: the mock's placeholder), the agent resumes.
printf 'mock-not-a-key\n' | "$bin" secret set llm_api_key >/dev/null
set +e
"$bin" setup --resume --non-interactive --json > "$work/setup.json" 2> "$work/progress.jsonl"
code=$?
set -e
[ $code -eq 0 ] || { cat "$work/setup.json" >&2; fail "setup --resume: exit $code"; }
jq -e '.ok and ([.data.steps[] | select(.name == "verify")][0].status == "done")' "$work/setup.json" >/dev/null || fail "verify did not pass"
grep -q '"event":"progress"' "$work/progress.jsonl" || fail "no progress lines on stderr"

# 4. A memory stored now is found again (retrieval, not ranking: the mock
# LLM echoes its input, so scores mean little).
"$bin" store --json --kind decision "The fresh-install proof stores this decision: billing keeps PostgreSQL 16 with partial indexes." > "$work/store.json" ||
  { cat "$work/store.json" >&2; fail "store failed"; }
for _ in $(seq 1 60); do
  "$bin" search --all --min-score 0 --json "Which PostgreSQL version does billing keep?" > "$work/search.json" 2>&1 || true
  if jq -e '[.data.hits[]? | select(.text | test("PostgreSQL"))] | length > 0' "$work/search.json" >/dev/null 2>&1; then
    found=1
    break
  fi
  sleep 3
done
[ "${found:-}" = 1 ] || { cat "$work/store.json" "$work/search.json" >&2; fail "the stored decision was never found"; }

# 5. Undo leaves the profile as it found it, keeping config and data.
"$bin" setup --undo --json > "$work/undo.json" || fail "setup --undo failed"
"$bin" doctor --json > "$work/doctor.json" || true
echo "fresh-install: ok (setup, verify, store, search, undo) on project $project"
