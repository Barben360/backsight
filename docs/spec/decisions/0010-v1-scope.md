# 0010 — v1 scope, non-goals and anti-goals (D1)

- **Date**: 2026-10-23
- **Status**: accepted
- **Decided by**: author (proposal approved as is)

## Decision

| Topic | v1 |
|---|---|
| Clients | Claude Code only. The client side sits behind a clean boundary so other agents can be added later. |
| Users | One user per install. Several machines of that same user may share one server (remote mode, decision 0003). |
| Languages | English and French presets for hook text, the CLAUDE.md section and the bank missions. The language facts are written in is a setting. |
| Platforms | Built for Linux x86_64/arm64, macOS arm64, Windows x86_64; tested on Linux x86_64 only, the others at the end (decision 0007). |

**Non-goals**: team or multi-user server, hosted service, redistributing Hindsight's image, our own LLM provider code, a fork of the Hindsight UI, issues opened automatically (decision 0002).

**Anti-goals** (a failure even if everything else works): breaking the user's existing Claude Code settings; background use of a Claude subscription against Anthropic's terms; any hook that can block or stall Claude Code.

## Why
- One bearer key opens every bank and Hindsight has no per-user isolation (research/C10): multi-user would need our own auth layer.
- The author's two machines already run English/French in practice; other languages are a preset away once the text is configuration.
- A Go panic exits 2, which blocks in Claude Code (research/C06): hooks must recover and exit 0, hence the third anti-goal.
