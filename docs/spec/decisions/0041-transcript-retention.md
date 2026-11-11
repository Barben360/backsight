# 0041 — Setup offers to extend Claude Code's transcript retention

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (asked 2026-10-24: "suggest to extend the retention setting in Claude Code settings for later reimport"); details agent

## Decision
- **Pre-flight** reports the effective `cleanupPeriodDays` (user settings, and whether a managed or project setting overrides it), the oldest transcript, and the transcript count and size.
- **Choices** step asks `retention`: `3650` (recommended and default: keep transcripts ten years) or `keep` (leave the setting). Agents pass `--answer retention=3650|keep|<days>`. The question explains why: transcripts are the only source for backfill, a rebuild on a fresh server, and a move to another server (0036, 0037); Claude Code deletes them after 30 days by default.
- The edit goes through the Claude Code config editor (0042): pre-image, minimal in-place change, undo.
- A managed setting that forces a lower value is reported, never fought.
- Standalone command `client retention [--days N] [--plan]` (no server needed), so a machine can protect its history before installing anything.
- `doctor` warns when retention is below 365 days, with the fix command.

## Why
The author's whole migration, and any future rebuild, depends on transcripts that Claude Code silently deletes after 30 days. On the author's machine 90 transcripts weigh 216 MB, so ten years of retention costs little disk.
