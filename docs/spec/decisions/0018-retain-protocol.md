# 0018 — Session retain with a per-session cursor and write-ahead outbox

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments
- **Amended by**: 0031 (settled retain instead of per-Stop appends)

## Decision
Document `conversation:<session_id>`, append mode, deterministic UUIDv5 `operation_id`, cursor = byte offset + prefix fingerprint, outbox fsynced before sending, per-session try-lock, advance only on 2xx, quarantine on other 4xx, full replace on fingerprint mismatch. Same document ids and tags as the npm plugin for continuity.

## Why
Port of the npm plugin's tested design (C04 §4.1) hardened for concurrent flushers and crashes (robustness review).
