# 0037 — Backfill of past sessions is in v1

- **Date**: 2026-10-24
- **Status**: accepted (agent; the author asked whether it is v1, and made it the migration path in 0036)
- **Decided by**: agent

## Decision
`retain backfill [--since D] [--project P] [--plan]` replays local transcripts (`~/.claude/projects/*/*.jsonl`) through the same normaliser, redaction, opt-outs and settled-retain protocol as live sessions, oldest first, throttled by `jobs tick`. Each segment carries the session's own timestamps (`timestamp` on retain items) so consolidation orders old facts before new ones. Explicit store calls found in old transcripts (`memory_store`, and the reference `hindsight_ingest_document`) are replayed as notes. `--plan` shows sessions, characters, expected cost and duration per provider; setup offers it after verify (consent step names it). Sessions already on the server are skipped or resumed (0038).

## Why
An empty memory is the weakest onboarding moment, and for the author it is the whole migration (0036). It reuses the write path, so it costs little beyond the plan and throttling. Claude Code deletes transcripts after `cleanupPeriodDays` (30 by default): setup reports the setting and the oldest transcript, and suggests raising it.
