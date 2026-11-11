# 0034 — Name: backsight

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (Q1)

## Decision
The product, binary, Claude Code plugin and marketplace are named `backsight`. Repository `github.com/Barben360/backsight` (private until M7), Go module `github.com/Barben360/backsight`, licence MIT. Compatibility is stated in plain text only ("a self-hosted Hindsight memory for Claude Code"). Names derived from it: MCP server `memory` → tools `mcp__plugin_backsight_memory__memory_*`, skills `/backsight:<skill>`, env vars `BACKSIGHT_*`, project file `.backsight.toml`. The spec repository keeps its name `hindsight-claude-spec`.

## Why
A backsight is the surveying reading taken back on a known point before moving forward, which is what the tool does with past sessions. No "claude" (Anthropic's naming rules, plugin-validator warning) and no "hindsight" prefix (Vectorize's mark); free on GitHub (6 unrelated repos, ≤ 1★), npm and Homebrew on 2026-10-24 (C02 second round).
