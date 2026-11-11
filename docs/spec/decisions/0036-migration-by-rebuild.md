# 0036 — Migration by rebuild: fresh install + session history, no in-place adoption

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author ("we will rerun everything from a brand new Hindsight install and my sessions history at the end; this way we have no possible restrictions")
- **Supersedes**: 0030 (guarded SQL); removes the adopt-in-place part of SPEC §11.5

## Decision
backsight never takes over an existing Hindsight database. Moving to backsight = a fresh install + `retain backfill` over the local Claude Code transcripts (0037). On a machine with a reference install, `setup` only **retires its client pieces** (hooks, MCP entries, timers, CLAUDE.md section; pre-images kept, undoable) and leaves the old server untouched (`doctor` says it can be stopped). No guarded SQL, no `server rebuild-observations`, no `server retag` in v1; `doctor` still counts untagged observations (fix: report upstream).

## Why
Every memory is built under the final rules from the first write (project-only scopes, `kind` labels, language and durability rules) with no legacy shapes to repair; the riskiest code (SQL against Hindsight's schema) disappears. Cost: transcripts are the only source, so they must be kept (`cleanupPeriodDays`, set to 3650 on the author's Linux machine on 2026-10-24 after a backup), and knowledge that exists only in the old memory (manual notes) is re-derived from the sessions that produced it (0037 replays their store calls).
