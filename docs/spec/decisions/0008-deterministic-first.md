# 0008 — Credo: everything that can be deterministic stays deterministic

- **Date**: 2026-10-23
- **Status**: accepted
- **Decided by**: author

## Decision
Every operation that can be deterministic is implemented deterministically in the binary (detection, configuration writing, merges, migrations, checks, backups). An LLM, typically the agent helping a user install, update or repair, is used only for **contextual decisions** (interpreting the user's situation, choosing among options the binary presents, explaining) and always acts **through deterministic commands** of the binary, never by editing files or improvising shell.

## Consequences
- Every step the agent can trigger exists as a binary subcommand with machine-readable output (e.g. JSON) and a dry-run.
- The agent guide describes decisions to make and commands to run, not procedures to improvise.
- Jobs that need an LLM by nature (fact extraction in Hindsight, the retro synthesis) keep it, behind deterministic orchestration (selection, budgets, dedup, writes).
