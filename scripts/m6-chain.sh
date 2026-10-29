#!/usr/bin/env bash
# The M6 chain (block 4, decision 0073): the VM bench's dedup and key
# rotation proofs (scripts/m6-proofs.sh), re-run on every pull request on
# containers. One server (Hindsight's mock LLM, on this machine's Docker)
# is opened on the machine's private IPv4; two client homes, a and b, on
# the same runner connect to it there with a token, each with its own
# HOME, BACKSIGHT_HOME and Claude Code profile. The checks reach the three
# homes through the driver below; no VM, no systemd timer: a client's
# `jobs tick` is run directly where the bench waits for its timer. The
# clients run with scheduler=off, so CI does not cover a timer's tick
# interleaving with a manual one: the VM bench's `timers` check does.
#
# Usage: scripts/m6-chain.sh [api port] (default 28600 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; the UI takes port + 1111).
# PROJECT names the Compose project (default bs-m6-chain); KEEP=1 leaves
# the stack and the work directory; WORK is the parent of the run's own
# directory (it must allow executables); BACKSIGHT_HF_CACHE as in
# scripts/fresh-install.sh. M6_DRY=1 lists the checks and stops.
# With no private IPv4 on the default route it fails on CI and is skipped
# elsewhere.

M6C_CHECKS=(two-clients dedup-copied dedup-concurrent dedup-split key-rotation)

# shellcheck disable=SC2154 # work is main's, set before any driver call
# m6_home <a|b|srv>: that machine's home, a directory of this run.
m6_home() {
  case $1 in
    a | b | srv) echo "$work/$1" ;;
    *) fail "no machine $1 (a, b or srv)" ;;
  esac
}

# m6c_env <who> <cmd…>: cmd as that machine's user: its own HOME and
# BACKSIGHT_HOME, no profile of the caller's.
m6c_env() {
  local h
  h=$(m6_home "$1")
  shift
  (cd "$h" && env -u CLAUDE_CONFIG_DIR -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CACHE_HOME \
    -u SSH_AUTH_SOCK -u BACKSIGHT_STATE_DIR HOME="$h" BACKSIGHT_HOME="$h/backsight" DISABLE_AUTOUPDATER=1 "$@")
}

m6_sh() { m6c_env "$1" sh -c "$2"; }

# m6_on <who> <backsight words…>. A client home never runs a server
# command nor an ssh install: the server is srv's alone, and OpenSSH would
# read the runner user's ~/.ssh whatever HOME says.
m6_on() {
  local who=$1 a
  shift
  case $who in
    srv) ;;
    a | b)
      for a in "$@"; do
        case $a in
          server) fail "client $who never runs a server command (backsight server …): the server is srv's" ;;
          --ssh | --ssh=*) fail "client $who never runs an --ssh install: OpenSSH would use the runner's ~/.ssh" ;;
        esac
      done
      ;;
    *) fail "no machine $who (a, b or srv)" ;;
  esac
  m6c_env "$who" "$bin" "$@"
}

m6_put() { m6_sh "$1" "umask 077 && cat > $(m6_q "$2") && chmod 600 $(m6_q "$2")"; }
m6_get() { m6_sh "$1" "cat $(m6_q "$2")"; }

# m6c_private_ip: the private IPv4 (RFC 1918 or tailnet) the default route
# leaves by, empty when there is none.
m6c_private_ip() {
  local cand
  command -v ip > /dev/null || return 0
  cand=$(ip -4 route get 1.1.1.1 2> /dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1)
  case $cand in
    10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | 100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) echo "$cand" ;;
  esac
}

fail() { echo "m6-chain: $*" >&2; exit 1; }

# compose …: the project the server's install rendered, never `backsight`.
compose() {
  local f=$work/srv/backsight/data/server/compose.yaml p
  p=$(sed -n 's/^name: *//p' "$f" | head -n 1)
  [ -n "$p" ] && [ "$p" != backsight ] || { echo "refusing compose project '$p'" >&2; return 1; }
  docker compose -p "$p" -f "$f" --env-file "$work/srv/backsight/data/server/.env" "$@"
}

m6c_cleanup() {
  local status=$?
  if [ $status -ne 0 ] && [ -f "$work/srv/backsight/data/server/compose.yaml" ]; then
    compose logs --tail 40 hindsight >&2 || true
  fi
  if [ "${KEEP:-}" != 1 ]; then
    [ -f "$work/srv/backsight/data/server/compose.yaml" ] && compose down -v > /dev/null 2>&1 || true
    [ -z "${BACKSIGHT_HF_CACHE:-}" ] || docker volume rm -f "${project}_hf_cache" > /dev/null 2>&1 || true # seeded, if setup never ran
    rm -rf "$work"
  fi
  exit $status
}

m6c_main() {
  local root port ip c state tok out code
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
  # shellcheck source=scripts/ports.sh
  . "$root/scripts/ports.sh"
  port=$(slot_port 28600 "${1:-}") || exit 2
  project=${PROJECT:-bs-m6-chain}
  [ "$project" != backsight ] || { echo "PROJECT=backsight would be the machine's own install" >&2; exit 2; }
  if [ "${M6_DRY:-}" = 1 ]; then
    printf '%s\n' "${M6C_CHECKS[@]}"
    exit 0
  fi
  ip=$(m6c_private_ip)
  if [ -z "$ip" ]; then
    # On CI a skip would turn the proof into a pass with no coverage.
    [ -z "${CI:-}${GITHUB_ACTIONS:-}" ] || fail "no private IPv4 on the default route (is ip installed?): the chain cannot run on CI"
    echo "m6-chain: skipped: no private IPv4 on the default route" >&2
    exit 0
  fi
  if [ -n "${WORK:-}" ]; then
    mkdir -p "$WORK"
    work=$(mktemp -d "$WORK/run.XXXXXX")
  else
    work=$(mktemp -d)
  fi
  trap m6c_cleanup EXIT
  bin=$work/bin/backsight
  # Built before HOME moves: Go's caches stay the caller's.
  make -s -C "$root" build OUT="$bin" VERSION=0.940.0
  # shellcheck source=scripts/hf-cache.sh
  . "$root/scripts/hf-cache.sh"
  # shellcheck source=scripts/proof-lib.sh
  . "$root/scripts/proof-lib.sh"
  # shellcheck source=scripts/m6-proofs.sh
  . "$root/scripts/m6-proofs.sh"
  export HOME=$work/home BACKSIGHT_HOME=$work/home/backsight
  unset CLAUDE_CONFIG_DIR XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME
  mkdir -p "$HOME" "$work/srv" "$work/a" "$work/b" "$work/m6"
  M6_RESULTS=$work/results.jsonl M6_WORK=$work/m6 M6_ROOT=$root M6_VERSION=0.940.0
  M6_COMMIT=$(git -C "$root" describe --always --dirty --abbrev=7 2> /dev/null || echo unknown)
  : > "$M6_RESULTS"

  # The server: its key (the mock's placeholder), setup, then opened on the
  # private address; the token goes to A, which connects.
  printf 'mock-not-a-key\n' | m6_on srv secret set llm_api_key > /dev/null
  hfc_seed "$project"
  m6_on srv setup --role server --non-interactive --json --answer provider=mock --answer model=mock-model --answer consent=yes \
    --answer project="$project" --answer api_port="$port" --answer ui_port=$((port + 1111)) --answer scheduler=off \
    < /dev/null > "$work/setup.json" 2> "$work/setup.err" ||
    { m6_learn_key srv; m6_scrub < "$work/setup.json" >&2; m6_scrub < "$work/setup.err" >&2; fail "setup --role server failed"; }
  m6_learn_key srv # its API key, masked in every result and failure from here on
  hfc_save "$project"
  m6_on srv server expose --private "$ip" --answer cleartext_lan=yes --json < /dev/null > "$work/expose.json" ||
    { m6_scrub < "$work/expose.json" >&2; fail "server expose --private $ip failed"; }
  curl -fsS --max-time 10 "http://$ip:$port/health" > /dev/null || fail "/health does not answer on $ip:$port after expose"
  state=$(m6_paths srv | jq -r .state)
  m6_on srv server token --out "$state/a-token" --json < /dev/null > "$work/token-a.json" || fail "server token for A failed"
  tok=$(m6_home a)/token
  m6_get srv "$state/a-token" | m6_put a "$tok" || fail "cannot copy the token to A"
  m6_sh srv "rm -f $(m6_q "$state/a-token")" || fail "cannot remove the token on the server"
  m6_on a connect --token-file "$tok" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json < /dev/null > "$work/connect-a.json" 2> "$work/connect-a.err" ||
    { m6_scrub < "$work/connect-a.json" >&2; m6_scrub < "$work/connect-a.err" >&2; fail "connect on A failed"; }
  m6_sh a "rm -f $(m6_q "$tok")" || fail "cannot remove the token on A"

  for c in "${M6C_CHECKS[@]}"; do
    m6_check "$c" "m6_check_${c//-/_}" < /dev/null
  done
  echo "bs-m6 chain results (project $project):"
  jq -r '"\(.check)\t\(.status)\t\(.detail)"' "$M6_RESULTS" | column -t -s "$(printf '\t')" || true
  code=$(jq -s '[.[] | select(.status != "PASS")] | length' "$M6_RESULTS")
  out=$(jq -s 'length' "$M6_RESULTS")
  [ "$out" = "${#M6C_CHECKS[@]}" ] && [ "$code" = 0 ] || fail "$code of $out result(s) are not PASS"
  echo "m6-chain: ok (${M6C_CHECKS[*]}) on project $project"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  m6c_main "$@"
fi
