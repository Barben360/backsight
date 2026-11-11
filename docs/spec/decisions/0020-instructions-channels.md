# 0020 — Agent instructions through SessionStart and per-prompt text; CLAUDE.md opt-in

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
One versioned instruction set per language; channels: SessionStart guide, MCP instructions and descriptions, per-prompt reminder/nudge/detector, gate reason, SubagentStart text. `setup --claude-md` adds a marked block on request.

## Why
E6: with the SessionStart guide the agent searched in 14/14 runs; adding the per-prompt reminder raised search-first to 13/14, equal to a CLAUDE.md block; without guidance 2/5.
