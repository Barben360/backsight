# 0044 — A restore makes every client re-read what the server holds

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- `restore` (and an update rollback that restores) writes a new **restore epoch** to the bank (document `backsight:epoch`, metadata `{epoch, restored_at, backup_created_at}`).
- Each client records the epoch with its cursors. On a mismatch (read at `jobs tick` and before any flush) it drops its local cursors and sent-index entries newer than `backup_created_at`, then resumes each session from the server's `document_metadata` (0038). Sessions whose segments were lost are re-sent from the transcript.
- `doctor` reports a client still on an old epoch.

## Why
Without this, segments sent after the backup was taken stay marked "sent" locally and are silently missing on the server. This matters most on a shared server, where other machines' cursors are stale too.
