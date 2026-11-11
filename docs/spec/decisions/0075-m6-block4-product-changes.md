# 0075 — M6 block 4's product changes: the connect window, split numbering, the replaced first segment, the connect bound

- **Date**: 2026-10-29
- **Status**: accepted (agent, as built in https://github.com/Barben360/backsight/pull/84, following the author's "never lose" of 2026-10-27)
- **Decided by**: agent, under the author's delegation (2026-10-28: "autonomous until M6 is finished"), on what the VM bench and the block 4 reviews found; results and rulings in `plans/2026-10-29-m6-block4-followups.md`
- **Amends**: 0073 block 2 (the sends generation; a session continued on two PCs is detected only without a matching cursor), 0038 (the document is read first only with no local cursor), SPEC §9.1 and §6.1

## Decision

### 1. The connect window
- Every sending command (`jobs tick`, `retain flush`, backfill, `store`, the MCP server, `retain reattribute`, the purge re-apply) reads the sends generation (`state.json` `sends_generation`) before its config, and sends nothing once it changed.
- `connect` and `connect --undo` raise it when they start this PC's sends over (cursors, sent-index and split sessions moved aside), and again once config and key are written. A rollback raises it once more and never lowers it.
- The tick's feedback upload and briefing prune follow the same gate.

### 2. Split numbering
- The client reads the document before every append that is not a resync (one read per document, only when something is to send; a failed read sends nothing).
- A document another PC moved (a later segment, the same segment ending at another record, a document this cursor never wrote to) is resynced once: this PC's segment is numbered after the server's, and the split recorded (cursor `split_bases`: the server's segment and last record it numbered from; `split-sessions.json`, doctor `split-session`).
- A document at an earlier segment of this PC's own, or at its split base, is this PC's lag (an append the server has not applied yet), not a move.

### 3. The repair of a first segment another PC replaced
- Each segment 1 this PC sends replaces its document; the cursor remembers it (`replaces`: document, the parts sent, the restore epoch, `sent_at`, `seen_at`).
- From 5 min after the send, once the bank's queue holds no retain of that document, a run reads the document's `original_text` and matches each of this PC's records by its whole encoded line; a record whose time was clamped to the send is matched without its time. Found: seen, then confirmed at a second look 5 min later. Missing: appended as the next segments, numbered after the server's, and the split recorded.
- A purge (tombstone) or a restore epoch change drops the mark.
- 24 h after the send, a mark that still cannot be confirmed is given up with telemetry `replace_unconfirmed {session, document, reason}`, never quarantined; the session's sends go on. A mark the run had no read budget for (50 documents a run) stays pending, even past 24 h.
- These reads happen only in `jobs tick`, the detached `retain flush` and backfill, never in a hook.
- Verified on Hindsight 1.1.0: `original_text` is kept byte for byte, and `/operations` names `document_id` for retains.

### 4. The connect bound
The Hindsight client bounds name resolution plus the TCP connect at `min(3 s, timeout)`, inside each call's total deadline.

### Also recorded
- `verify --live` passes a session whose later segment carries another non-empty `machine_id`, and names it in `continued_elsewhere`.
- `unsent-cost`: +90 ms for a tick with 200 uncursored transcripts (39 → 129 ms), under the 2000 ms follow-up threshold. No action.

## Why
- **Connect window**: a process that read the generation after the first raise but loaded the config before connect wrote it would pass the gate with the old address; a second raise after the write closes that window, and a generation that only grows means a process that loaded a config a rollback replaced never matches again.
- **Split numbering**: a PC holding a cursor appended without looking, so it could not see that another PC had continued the document; the bench's `dedup-split` needs both continuations on the server and both PCs to name the split, and a lag of this PC's own must not resync again on every append the queue has not applied.
- **Replaced first segment**: Hindsight has no compare-and-set, so two PCs that both read a document absent and send its segment 1 within the queue delay keep only the later one; "never lose" forbids accepting that loss, and keeping the reads out of hooks keeps their deadlines.
- **Connect bound**: a server that silently drops packets cost a prompt 17 s (the whole budget) and now costs 3 s; a LAN or tailnet connect takes milliseconds, so 3 s leaves room for a far tailnet peer.

## Amended by the M6 review (2026-10-29)

As built on backsight branch `m6/block5-docs-review` (909ef9d); the text above is kept as decided.

- **§3, a restore no longer drops the mark** (review R1 I-1). The reconcile judges only cursors sent after the restored backup (its time minus 1 h), so a race before the backup is in the backup, and the mark is its only repair. A restore since the send, told as the reconcile tells it (another epoch number, or the same number with another `restored_at`), now gives the mark the new epoch and its `restored_at` (`replaces.epoch_restored_at`), clears `seen_at`, and starts the looks over against the restored document: missing records go as the next segment, the split recorded. A mark written before `epoch_restored_at` counts a restore whose `restored_at` is after its send or its last restore look, within 1 h; the re-stamp records the identity, so this happens once. The 24 h give-up then runs from the first run that found the restore (cursor `replaces.restored_at`), not from the send. A tombstone still drops the mark; a cursor sent after the backup is removed by the reconcile, its mark with it.
- **The restore epoch's identity** (review R2 I-1; amends 0044's "only a higher epoch reconciles"). A host restored on a fresh machine from an older archive counts from what that machine holds, and may write an epoch number a client already holds, or a lower one. A client therefore also reconciles, purge re-apply included, when the epoch's `restored_at` differs from the one it recorded (`state.json` `restore_epoch.restored_at`), at any number. A record from before the field reconciles once when its `reconciled_at` is earlier than the epoch's `restored_at` plus 1 h, and otherwise only gains `restored_at`. `connect` records it from its probe; doctor's `restore-epoch` warns "client on an earlier restore" until the tick reconciles. Restore numbering is unchanged.
