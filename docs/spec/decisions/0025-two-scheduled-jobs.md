# 0025 — Two scheduled entries: `jobs tick` (15 min) and `jobs maintain` (daily)

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments
- **Amended by**: 0028 (no retro in `tick`); the entity index is rebuilt lazily by hooks when older than 24 h and refreshed by `jobs maintain`

## Decision
Native OS schedulers (systemd user timers, launchd agents, Task Scheduler) call the shim; one lock per job. tick = outbox flush, retro, entity index; maintain = backup, update check, telemetry rotation, doctor snapshot.

## Why
Fewer entries to install, inspect and remove; the retag job is no longer needed with project-only scopes (0015).
