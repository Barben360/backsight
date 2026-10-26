#!/usr/bin/env bash
# Installs minisign on a CI runner from its pinned upstream release, checked
# against the checksum below, with bounded network waits: an apt mirror once
# held the test job for 2 h. The checksum is that of the archive whose
# signature verified with the minisign author's key on 2026-10-26.
#
#   scripts/ci-minisign.sh
set -euo pipefail

command -v minisign >/dev/null && exit 0

version=0.12
sha256=9a599b48ba6eb7b1e80f12f36b94ceca7c00b7a5173c95c3efc88d9822957e73
case "$(uname -m)" in
x86_64) arch=x86_64 ;;
aarch64) arch=aarch64 ;;
*) echo "ci-minisign.sh: no minisign build for $(uname -m)" >&2; exit 1 ;;
esac

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
curl -fsSL --connect-timeout 20 --max-time 120 --retry 3 \
  -o "$dir/minisign.tar.gz" \
  "https://github.com/jedisct1/minisign/releases/download/$version/minisign-$version-linux.tar.gz"
echo "$sha256  $dir/minisign.tar.gz" | sha256sum -c --quiet
tar -xzf "$dir/minisign.tar.gz" -C "$dir" "minisign-linux/$arch/minisign" 2>/dev/null
sudo install -m 0755 "$dir/minisign-linux/$arch/minisign" /usr/local/bin/minisign
minisign -v
