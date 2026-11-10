#!/usr/bin/env bash
# A stand-in claude CLI for the Codex container: `backsight client install`
# runs it to check the version and register the plugin, and the generated
# plugin tree is what the harness hands to Codex. It answers what client
# install reads and accepts the rest.
case "$*" in
  --version) echo "9.9.9 (Claude Code)" ;;
  "plugin list --json" | "plugin marketplace list --json") echo "[]" ;;
  *) : ;;
esac
