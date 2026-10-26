#!/usr/bin/env bash
# Refuses a release signing key where none belongs (security review M3):
# only the release workflow, on a v* tag, signs with the release key, and a
# snapshot never does, so no install or update accepts a local build.
#
#   scripts/release-key-guard.sh [--snapshot] [PUB...]
#   scripts/release-key-guard.sh --list
#
# The key is read from MINISIGN_KEY_FILE (the file goreleaser signs with)
# and MINISIGN_SECRET_KEY (its content, as the release workflow's secret
# holds it); neither set, there is nothing to check. A key is a release key
# when its public half or its key id is one of PUB's (default: minisign.pub
# and minisign.next.pub). The release workflow is GitHub Actions running
# .github/workflows/release.yml for a v* tag: there, outside a snapshot,
# nothing is checked. Elsewhere a release key is refused, and so is an
# encrypted key, whose id cannot be read (make snapshot makes its own,
# unencrypted). --list prints the ids of the keys it compares with.
#
# A guard against mistakes (an exported key, goreleaser run by hand), not
# against whoever holds the key: they can sign with minisign directly.
set -euo pipefail
cd "$(dirname "$0")/.."

snapshot=false list=false
case ${1:-} in
--snapshot) snapshot=true; shift ;;
--list) list=true; shift ;;
esac
pubs=("$@")
[ ${#pubs[@]} -gt 0 ] || pubs=(minisign.pub minisign.next.pub)

die() { echo "release-key-guard: $*" >&2; exit 1; }
hex() { base64 -d 2>/dev/null | od -An -tx1 -v | tr -d ' \n'; }

# The release keys: "<id> <public key>" per line, in hex.
release=""
for p in "${pubs[@]}"; do
  h=$(sed -n 2p "$p" | hex) || true
  [ ${#h} -eq 84 ] && [ "${h:0:4}" = 4564 ] || die "$p: not a minisign public key"
  release+="${h:4:16} ${h:20:64}"$'\n'
done
if $list; then
  # minisign prints ids as the bytes reversed, upper case.
  while read -r id _; do
    [ -n "$id" ] || continue
    r=""; for ((i = 14; i >= 0; i -= 2)); do r+=${id:i:2}; done
    echo "${r^^}"
  done <<<"$release"
  exit 0
fi

if [ "${GITHUB_ACTIONS:-}" = true ] && ! $snapshot; then
  case ${GITHUB_WORKFLOW_REF:-} in
  */.github/workflows/release.yml@refs/tags/v*) exit 0 ;;
  esac
fi

# check <what> <key text>: refuses a release key or one it cannot read.
check() {
  local what=$1 h kdf id pk
  h=$(printf '%s\n' "$2" | sed -n 2p | hex) || true
  # sig alg "Ed", kdf alg, checksum alg, salt (32), opslimit (8), memlimit
  # (8), then the key id (8), the secret key (64: its last 32 bytes are the
  # public key) and a checksum (32): 158 bytes.
  [ ${#h} -eq 316 ] && [ "${h:0:4}" = 4564 ] || die "$what: not a minisign secret key"
  kdf=${h:4:4}
  [ "$kdf" = 0000 ] || die "$what: an encrypted key, whose id cannot be checked: a snapshot is signed with the throwaway key make snapshot makes"
  id=${h:108:16} pk=${h:188:64}
  while read -r rid rpk; do
    [ -n "$rid" ] || continue
    if [ "$id" = "$rid" ] || [ "$pk" = "$rpk" ]; then
      if $snapshot; then
        die "$what holds a release key: a snapshot is never signed with it (make snapshot signs with a throwaway key); unset it"
      fi
      die "$what holds a release key: only the release workflow signs with it (.github/workflows/release.yml, on a v* tag); unset it"
    fi
  done <<<"$release"
}

if [ -n "${MINISIGN_KEY_FILE:-}" ]; then
  k=$(cat "$MINISIGN_KEY_FILE" 2>/dev/null) || die "cannot read MINISIGN_KEY_FILE ($MINISIGN_KEY_FILE)"
  check "MINISIGN_KEY_FILE ($MINISIGN_KEY_FILE)" "$k"
fi
if [ -n "${MINISIGN_SECRET_KEY:-}" ]; then
  check MINISIGN_SECRET_KEY "$MINISIGN_SECRET_KEY"
fi
