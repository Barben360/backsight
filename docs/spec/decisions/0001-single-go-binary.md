# 0001 — Everything in Go, one binary with subcommands

- **Date**: 2026-10-23
- **Status**: accepted
- **Decided by**: author

## Context
The reference stack mixes Go-less pieces: Python hooks (`memory-search.py`, `hindsight-memory-mcp.py`, `memory-retro.py`), a Node npm plugin, shell scripts, a Python/torch GPU reranker. Python and Node are friction on Windows and macOS (`python3` absent on Windows, versions, venvs).

## Options
1. Go CLI for operations, keep Python hooks.
2. Go for everything we write: CLI, Claude Code hooks, MCP server, periodic jobs.

## Decision
Option 2: a single Go binary with subcommands (CLI, hooks, MCP server, jobs). All existing Python is rewritten. Third-party servers (Hindsight, Postgres, inference engines) are not "our code" and stay what they are; how they run is decided separately (D3, D8).

## Consequences
- One artifact per OS/arch; no interpreter to install.
- The npm plugin's behaviour must be reimplemented (see check C4, decision D4).
- The GPU reranker cannot stay Python+torch without breaking the goal: inference path decided after check C8.
