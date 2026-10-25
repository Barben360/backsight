#!/usr/bin/env bash
# The release check of make verify-keys (decision 0067 and its block
# review): a release binary trusts minisign.pub and minisign.next.pub and
# nothing else, has no test hooks, and pins the source's Hindsight image.
#
#   scripts/verify-keys.sh DIST [BIN TARGET]
#
# Every binary's build settings (go version -m, which reads any target's
# binary): no testkey or testhooks tag, the only ways to a test key or
# another pin (the variables behind them are set in init, after the
# linker, so -X alone changes nothing), and no -X of them when the build
# recorded its -ldflags (a build without -trimpath). The binary this
# machine runs is also asked: version --json. BIN and TARGET (its os_arch)
# check one binary, as goreleaser's post-build hook does for each target;
# without BIN, every backsight binary in DIST, this machine's included.
set -euo pipefail
cd "$(dirname "$0")/.."
dist=${1:-dist} bin=${2:-} target=${3:-}
host=$(go env GOOS)_$(go env GOARCH)
pin=$(sed -n 's/^const pinnedHindsight = "\(.*\)"$/\1/p' internal/server/render.go)
[ -n "$pin" ] || { echo "verify-keys: no pinnedHindsight in internal/server/render.go" >&2; exit 1; }
cur=$(sed -n 's/^untrusted comment: minisign public key //p' minisign.pub)
nxt=$(sed -n 's/^untrusted comment: minisign public key //p' minisign.next.pub)
failed=0

# settings <binary>: its recorded build settings are a release's.
settings() {
  local b=$1 info tags ld
  info=$(go version -m "$b" 2>&1) || { echo "verify-keys: $b has no Go build information: $info"; return 1; }
  tags=$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*build[[:space:]]*-tags=//p')
  ld=$(printf '%s\n' "$info" | sed -n 's/^[[:space:]]*build[[:space:]]*-ldflags=//p')
  case ",$tags," in
    *,testkey,* | *,testhooks,*) echo "verify-keys: $b was built with -tags=$tags: a test build"; return 1 ;;
  esac
  case "$ld" in
    *internal/signing.*Key* | *internal/server.*) echo "verify-keys: $b was linked with $ld: a test key or another pin"; return 1 ;;
  esac
}

# runs <binary>: what this machine's binary trusts and pins.
runs() {
  local b=$1 want
  want=$(printf '[{"id":"%s","role":"current"},{"id":"%s","role":"next"}]' "$cur" "$nxt")
  "$b" version --json | jq -e --argjson want "$want" --arg pin "$pin" \
    '.data.signing_keys == $want and .data.test_key == false and .data.test_hooks == false and .data.hindsight_image == $pin' >/dev/null || {
    echo "verify-keys: $b trusts other keys than minisign.pub ($cur) and minisign.next.pub ($nxt), has test hooks, or pins another image than $pin:"
    "$b" version --json | jq -c '.data | {signing_keys, test_key, test_hooks, hindsight_image}'
    return 1
  }
  echo "$b trusts $cur and $nxt only and pins $pin"
}

if [ -n "$bin" ]; then
  settings "$bin" || failed=1
  if [ -z "$target" ] || [ "$target" = "$host" ]; then runs "$bin" || failed=1; fi
else
  shopt -s nullglob
  bins=("$dist"/backsight_*/backsight "$dist"/backsight_*/backsight.exe)
  mine=("$dist"/backsight_"$host"*/backsight)
  [ ${#mine[@]} -gt 0 ] || { echo "verify-keys: no $host binary in $dist"; exit 1; }
  for b in "${bins[@]}"; do settings "$b" || failed=1; done
  runs "${mine[0]}" || failed=1
fi
exit $failed
