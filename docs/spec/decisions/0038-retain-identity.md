# 0038 — Retain identity: the server holds the cursor; record uuids guard forks

- **Date**: 2026-10-24
- **Status**: accepted (agent)
- **Decided by**: agent, after a check on Claude Code 2.1.310 (2026-10-24)

## Facts
- `--resume` keeps the session id and appends to the same transcript file.
- `--fork-session` writes a new session id and a new file that **copies the parent's records with their original `uuid` and `timestamp`** (only `sessionId` is rewritten); no parent marker.
- Session ids are random per machine; one session lives on two machines only if transcripts are copied (restore, sync, new PC).
- Hindsight documents keep `document_metadata` and a `content_hash`; the reference npm plugin already uses `conversation:<session_id>`.

## Decision
- Document id `conversation:<session_id>` (same as the npm plugin). Each segment's `document_metadata` records `{last_uuid, records, segment, machine_id, binary_version}`.
- **The server is the source of truth for what was sent**; the local cursor is a cache. With no local cursor (new machine, restored state, backfill), the client reads the document's metadata first and resumes after `last_uuid`, so a shared server never receives a session twice.
- **Forks**: the normaliser skips a leading run of records whose `uuid` is in the local sent-records index (`<state>/sent-index/`, compact 8-byte hashes of user/assistant record uuids); only the fork's own turns are sent.
- **Resume**: nothing special — the cursor is past the old turns; new turns go out as the next appended segment when the session settles again.
- `operation_id` = UUIDv5(bank, document, first_uuid, last_uuid, content hash) keeps resends idempotent server-side.

## To verify (M1)
That an `update_mode: append` retain replaces `document_metadata` with the new segment's metadata (seen only for first segments so far). Fallback if not: one small marker document per session `cursor:<session_id>` rewritten after each segment.

## Why
Hash comparison of whole documents breaks on append and on forks; record uuids are stable across both. Keeping the cursor on the server makes the shared-server case and machine moves safe without a coordination service.
