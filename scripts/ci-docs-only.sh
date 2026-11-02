#!/usr/bin/env bash
# ci-docs-only.sh: reads a pull request's changed paths on stdin, one per
# line (renames: both names), and prints code=false when every one is
# documentation, code=true otherwise (a GitHub step output line).
#
# Documentation: the Markdown files of docs/ (docs/releases/ included) and
# of the repository's root. docs/'s Go files and assets are code; so is
# Markdown anywhere else (skills, instruction texts). No path at all is
# code too: a listing that came back empty runs everything.
#
# ci.yml's changes job: a docs-only pull request skips the jobs the docs
# cannot reach (m6-chain, native-chain, native-chain-windows,
# test-windows). The sandbox tests and fresh-install search the embedded
# docs (guide search), so they always run, as do the Linux tests.
set -euo pipefail

n=0
while IFS= read -r f || [ -n "$f" ]; do
  f=${f%$'\r'}
  [ -n "$f" ] || continue
  n=$((n + 1))
  case "$f" in
    docs/*/*.md) ;;
    docs/*.md) ;;
    */*) echo "code=true"; exit 0 ;;
    *.md) ;;
    *) echo "code=true"; exit 0 ;;
  esac
done
if [ "$n" -eq 0 ]; then
  echo "code=true"
else
  echo "code=false"
fi
