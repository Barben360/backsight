#!/usr/bin/env bash
# The pinned Codex CLI and the fake model, for a run outside the harness's
# container (the M6 proofs' Codex PC, issue 284):
#   scripts/codex/fetch.sh <dir>
# puts in <dir> the codex binary of scripts/codex/Dockerfile's release (its
# sha256 checked, the same pin) and fakeresponses built from this tree.
# Kept: a codex already there at that version is not fetched again.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
dir=${1:?usage: scripts/codex/fetch.sh <dir>}
arg() { sed -n "s/^ARG $1=\\(.*\\)$/\\1/p" "$here/Dockerfile" | head -n 1; }
version=$(arg CODEX_VERSION) sum=$(arg CODEX_SHA256)
[ -n "$version" ] && [ -n "$sum" ] || { echo "fetch.sh: no CODEX_VERSION or CODEX_SHA256 in $here/Dockerfile" >&2; exit 1; }
mkdir -p "$dir"
if [ "$("$dir/codex" --version 2> /dev/null || true)" != "codex-cli $version" ]; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/codex.tar.gz" "https://github.com/openai/codex/releases/download/rust-v$version/codex-x86_64-unknown-linux-musl.tar.gz"
  echo "$sum  $tmp/codex.tar.gz" | sha256sum -c - > /dev/null
  tar -xzf "$tmp/codex.tar.gz" -C "$tmp"
  install -m 0755 "$tmp/codex-x86_64-unknown-linux-musl" "$dir/codex"
fi
[ "$("$dir/codex" --version)" = "codex-cli $version" ] || { echo "fetch.sh: $dir/codex is not $version" >&2; exit 1; }
(cd "$here/../.." && CGO_ENABLED=0 go build -o "$dir/fakeresponses" ./scripts/codex/fakeresponses)
