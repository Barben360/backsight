# 0022 — Agent-driving contract: JSON, plan, answers, exit codes, needs_user

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Every command supports `--json` (versioned), `--plan`, `--non-interactive`; inputs via `--answer k=v` or an answers file (no secrets); exit codes 0 / 10 needs user / 20 blocked / 30 failed and rolled back / 40 partial; `needs_user` lists questions, secret names and user actions with a resume command.

## Why
Decision 0008 requires the agent to act only through deterministic commands; the onboarding review found no input channel or question schema in v1.
