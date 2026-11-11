# 0050 — Write side: normaliser, segments, cursors, notes outbox (M1 block 2)

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, from SPEC §9.1, decisions 0018/0031/0038, research C04, E21 and a survey of the author's transcripts (Claude Code 2.1.128 → 2.1.310)

## Decision
- **Normaliser** (`internal/transcript`):
  - Records with `type` user or assistant only. Dropped: `isMeta`, `isSidechain`, `isCompactSummary`, records without `message`, `thinking` blocks and `tool_result` blocks.
  - Each `tool_use` becomes one `action` turn, `<Tool> <target>`. The target is the first non-empty of `file_path, path, notebook_path, command, pattern, query, url, name, id`: its first line, at most 100 characters (C04 parity).
  - Harness tag blocks are stripped from user text (`system-reminder`, `command-*`, `local-command-*`, `task-notification`, `bash-*`); a turn left empty is dropped.
  - Other record types (about 20 seen: `attachment`, `mode`, `ai-title`…) are counted, never fatal.
  - Each kept record yields its `uuid`, timestamp, `cwd` and its byte offset in the file.
- **Redaction** (`internal/redact`):
  - The patterns of SPEC §13, replaced with `[redacted:<kind>]`.
  - It runs on every turn before anything leaves the machine.
  - A corpus test (true positives and look-alikes that must survive) is a release gate.
- **Project**:
  - The `project` of `<repo>/.backsight.toml` (0045), else the basename of the git main worktree (`git rev-parse --git-common-dir` from the session's `cwd`), else the `cwd` basename.
  - A `cwd` equal to the home directory or `~/.claude` gives the first transverse alias.
- **Opt-outs**:
  - `privacy.exclude_paths` (prefix of `cwd`);
  - `.backsight.toml` with `retain = false`;
  - a paused session (`sessions/<id>.json` with `paused`, written from M2).
  An opted-out session advances its cursor without sending.
- **Segment** (one retain item):
  - Content: JSON lines `{role, content, timestamp}`. The first segment starts with `{"role":"system","content":"REF-ID: conversation:<sid>"}` (npm plugin parity).
  - Document `conversation:<sid>`, strategy `conversation`, context `coding agent session`.
  - Tags `project:<p>`, `source:chat`, `harness:claude-code`; scopes `[["project:<p>"]]`.
  - Timestamp: the segment's first turn.
  - `update_mode: append` from segment 2.
  - Metadata (strings): `{session_id, last_uuid, records, segment, machine_id, binary_version, source: chat, harness: claude-code}`. E21: the append replaces it.
  - Sent with `async: true` and `operation_id` = UUIDv5(backsight namespace, `bank\ndocument\nfirst_uuid\nlast_uuid\nsha256(content)`). E21: resends collapse.
- **Cursor** `<state>/cursors/<sid>.json`:
  - Fields: `{transcript, offset, fingerprint, last_uuid, records, segment, document, bank, last_turn_at, sent_at, quarantined}`.
  - The fingerprint is a SHA-256 of the transcript's first 4 KiB. On a mismatch the session is re-read from the start, and records are matched against the server's `last_uuid`.
  - With no cursor, the client reads the document: found means resume after its `last_uuid` with segment + 1. A `last_uuid` missing from the transcript quarantines the session; it is never sent twice.
- **Fork guard**: `<state>/sent-index/index.bin`, 8-byte SHA-256 prefixes of every sent record `uuid`. A new session skips its leading records found in the index.
- **When**:
  - A session's unsent records are sent once its last record is ≥ 30 min old.
  - A session still open sends its records older than 2 h.
  - SessionEnd flush arrives with the hooks (M2).
  - Live retain only takes sessions whose first record is after `state.retain_since`, set when the first `jobs tick` runs. Older sessions belong to backfill (block 3), which uses the same cursors, so nothing is sent twice.
- **Errors**:
  - 2xx: advance the cursor and the index.
  - Timeout, 429, 5xx, 503: keep, retry next tick.
  - Other 4xx: `quarantined` with the server answer in `quarantine/<sid>.jsonl`, skipped until `retain retry-quarantine`.
  - One try-lock per session (`locks/session-<sid>.lock`), stale after 10 min or with a dead pid.
  - Under the maintenance lock, nothing is sent.
- **Notes**:
  - `backsight store --kind <k> [--project p] <text>` redacts and refuses text that held a secret (exit 2).
  - It appends `{seq, operation_id, document_id: note:<uuid>, kind, project, content, created_at, attempts, last_error}` to `<state>/outbox/notes.jsonl` (fsync), then flushes at once. A server that is down leaves the note queued (exit 0, "queued").
  - Each note: strategy `document`, tags `project:<p>`, `source:note`, `kind:<k>`, `harness:claude-code`; scopes `[["project:<p>"]]`.
  - Warning at 50 MB, refusal at 500 MB (§9.1).
- **`jobs tick`**: settled sessions, then the notes outbox, one `job` telemetry line and one `retain` line per item. `retain flush [--session id]` does the same for one session or all, ignoring the 30-minute wait.

## Why
It ports the npm plugin's tested transcript reading, with the settled-retain timing of 0031 and the server-held identity of 0038, which E21 verified on Hindsight 1.1.0. The `retain_since` boundary lets live retain start on a machine that already has history without racing the backfill.
