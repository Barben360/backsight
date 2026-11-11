# 0032 — Claude subscription lane offered, with a terms warning

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (Q2)

## Decision
The `claude-code` provider is a listed LLM lane for server-side extraction, never the default. Setup shows a warning before it can be chosen (Anthropic's terms restrict routing Pro/Max credentials through third-party products; Hindsight labels the lane "personal development only"; usage counts against the subscription limits) and needs an explicit acknowledgement (`--answer subscription_terms=ack` for agents, after showing it to the user). The user runs `claude setup-token` themselves and pastes the token into `secret set llm` (stored in `secrets.toml`, rendered as `HINDSIGHT_API_LLM_API_KEY`). Nothing is mounted into the container.

## Why
The author wants the lane. Verified 2026-10-24 that the Hindsight 1.1.0 image already bundles the Claude Agent SDK with its own Claude Code CLI (2.1.301, `claude_agent_sdk/_bundled/claude`) and that `claude_code_llm.py` passes the configured key as `CLAUDE_CODE_OAUTH_TOKEN` to a CLI started with a fresh `CLAUDE_CONFIG_DIR`, so a host `~/.claude` mount would be neither read nor needed (C07 §a). An end-to-end extraction with a real token is part of the M4 provider matrix.
