# 0013 — Every hook runs behind a supervisor that always exits 0

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
`hook <event>` = parent process that re-executes the binary as a child with a hard deadline and exits 0 whatever happens; it forwards the child's stdout only when the child exited 0 with valid hook JSON.

## Why
Exit 2 blocks prompts, tools and Stop (C06); Go fatal errors (concurrent map writes, deadlocks, OOM) bypass `recover()` and exit 2. A supervisor is the only way to guarantee the anti-goal of decision 0010.
