# 0090 — A session's later segments are documents of their own: no re-extraction on append

- **Date**: 2026-11-12
- **Status**: accepted
- **Decided by**: author (2026-11-12: "Run all fixes that you can do deterministically and you think useful right now, especially for the poisoning fixes and the redaction fixes, as well as cost cut. Make it as migratable as possible (keep the old memories working anyways). You are autonomous on that"), on the deep review of the write path and the measurement below
- **Amends**: 0050 (segment 2 onwards `update_mode: append`), 0031 (successive segments appended to `conversation:<session_id>`), 0038 (the client reads one document for the position), 0083 C1 (its expected effect)

## Context
Three days after 0083 C1 shipped (appends wait for 30,000 characters or 6 h), conversation appends still churn. Read-only SQL on the author's server, `async_operations.result_metadata->'document_events'`, last 3 days:

| source | kind | events | facts added | facts removed |
|---|---|---|---|---|
| conversation | append | 85 | 2,774 | 1,561 |
| subagent | retain | 79 | 473 | 46 |
| note | retain | 109 | 351 | 0 |
| conversation | retain (first send) | 28 | 338 | 0 |

56 % of the facts appends add replace facts they deleted (0083 C1 expected under 20 %). Consolidation is about 75 % of the server's LLM spend (929 calls in 3 days at about 20,000 input tokens each, over about 5,290 facts); the 1,561 replacement facts are consolidated again and rewrite observations, whose ids then change under the feedback ledger.

The cause is in Hindsight itself (the pinned image, `engine/retain/orchestrator.py`): `update_mode: "append"` reads the stored document, prepends its whole text to the new content as one item and retains the body again; the delta path (`_try_delta_retain`, `_classify_chunk_diff`) compares chunks by index and hash. The stored text is chunked again as one piece, so once it holds two segments or more its chunks no longer match the stored ones (the segments were chunked apart): from the third segment on, each append deletes and extracts again the facts of chunks it did not change, whatever its size. Measured on a scratch stack (mock LLM, three segments of one session): appends `facts_added`/`facts_removed` 10/0 then **20/20**; as parts, 10/0, 10/0, 10/0. The pinned version has no append-only or chunk-preserving mode (`update_mode` is `replace | append`; `force_reextract` only widens the work). The gate of 0083 C1 makes appends rarer, not cheaper.

Extraction context (`fact_extraction._build_user_message`): each chunk is extracted alone, with the item's `context` and `metadata`, never the text of the chunks before it. A segment in its own document therefore loses nothing an appended one had: Hindsight already chunks the new content apart from the stored text it prepends.

## Decision

### P1 — Segment 2 onwards is its own document (amends 0050 "Segment", 0031)
- Segment `s ≥ 2` of a session's document `D` (`conversation:<sid>`, `conversation:<sid>@<project>`, with a purge's `#<n>`: the live id) is retained as a new document **`D#part<s>`**, without `update_mode`. Segment 1 stays `D`, with its `REF-ID` line; a part has no `REF-ID` line (the metadata the extractor sees names the session and the segment).
- Same strategy, context, tags, observation scopes and metadata as an appended segment had: `session_id`, `segment` (= `s`), `last_uuid`, `records`, `machine_id`, `binary_version`, `source`, `harness`, `project`, `entrypoint`, `legacy_last_uuid`.
- `#part<s>` collides with nothing: `#<n>` (digits only) is a purge successor, `@` a project; every parser of a session's id cuts at the first `@` or `#` (`session.OfDocument`, the metrics), and an older `BaseDocumentID` leaves `#part<s>` alone (not a number).
- The **operation id** keeps its formula over `D`, not the part's id: the same records sent again (a crash between the send and the cursor's save, across an update too) collapse on the server.

### P2 — The server's position is the chain's end (amends 0038)
- The position of `D` is the last found of `D`, `D#part<seg(D)+1>`, `D#part<seg(D)+2>`…, where `seg(D)` is `D`'s metadata segment. One missing part is stepped over (parts are separate documents, which the server may store out of order); two in a row end the chain.
- Before a send that is not a resync (`movedElsewhere`), the read starts at the part of the cursor's last segment (two or three reads), falling back to `D` when that part is not there (a legacy document, or this PC's part not stored yet). Resync, the restore reconcile and `retain reattribute` read from `D`.
- The cursor is unchanged: it keeps `D` and the last segment number. `verify --live` compares the chain's end, counts the facts of every part, and fails on a part missing between stored ones while nothing is queued (a hole the chain steps over).

### P2b — A part can be replaced or lost: every part sent is checked (amends 0075; review of PR https://github.com/Barben360/backsight/pull/341)
- A retain without `update_mode` replaces its document. Two PCs continuing a copied session can both read the chain at the same end (one PC's part still queued) and send the same part number: the later replaces the earlier, where two appends were both kept. A part the server accepted can also fail in its worker, or be lost to a restore, while the next part is stored: the chain steps over the hole.
- So every part sent starts or joins the pending check segment 1 already had (0075, `Cursor.Replaces`), which now records the segments sent (`segments`). Its look, at least 5 minutes after the send and once the queue names none of them, reads each document the check's sends wrote (one read each, nothing else), and what is missing (a part replaced by another PC's, or absent) goes again after the chain's end, under a salted operation id; the split is recorded only when another PC's send was found there. Same limits as 0075: confirmed at a second look, given up after 24 h, reported by `replace-unconfirmed`.
- Not prevented, repaired: Hindsight has no compare-and-set, and a queue read before every part (hooks' detached flush included) would cost requests and still leave two sends that both read the queue clear. The window is the turns of one part, sent again within one tick after the queue clears.
- A look never reads past the run's budget (`maxReplaceReads`, 50) and never gives up a long send for its size: it reads a window of at most 50 of the check's documents (`done` counts those confirmed or sent again, one range per segment), confirms it by two looks or sends again what it lacks, then moves to the next window at the next run. A window that does not fit what is left of a run's reads waits for the next run.
- `doctor`'s `queue-age` warns when the oldest pending or processing retain on the server is 6 h old or more and no retain completed in the last hour (`LatestOperation`, completed, retain): while the queue is stuck, these repairs wait, and so does the purge of a late part (P3). Six hours: a healthy server stores a part in seconds to minutes, while a warning at 6 h leaves most of the 24 h before a check gives up. The completion test: a long import or manual backfill drains in order, 20 s or more a document, so its oldest retain can pass 6 h on a healthy server; with a retain completed within the hour, doctor reports information ("still draining") and no fix, never a restart of a working server.
- A check of a mark from before parts (no `segments`) reads `D` alone, as before.

### P3 — Purges cover the whole chain (amends SPEC §13 `forget --purge` and the tombstones of 0073)
- `forget --purge` and `forget --project --purge` delete, for each conversation document a purged fact came from, `D` and every part of it (read from the bank's listing), each with its tombstone.
- A purge is never held by the server's queue (author, 2026-11-13: "a purge must never be blocked indefinitely. Privacy matters more: a user must always be able to delete a leaked secret"). A part whose retain is still queued lands after the delete: the purge deletes what exists, and tombstones each part that may still land (the two after each chain's last segment, `D`'s own when an older build appended to it, and any part the queue names), each covering what the chain's last part covered. When the queue names such a part, or cannot tell (unread, longer than the 5 pages per status read, a retain of no single document), the purge says so: the late part is purged automatically when it lands. Exit 0 with that note (the agent contract has no "done, with a follow-up" code; `Partial`, 40, means resumable by the same command).
- The tombstone of a predicted part that never lands stays, as every tombstone does: it covers no record its chain's last part did not, its chain is purged anyway, and it costs one read in `jobs maintain`'s daily look by id (counted in its cheaper-way choice). Not expired: telling it from a real part's tombstone would need a new field in the control bank's format.
- That automatic delete is the look for recreated documents (`jobs tick`, `jobs maintain`), which already purges a tombstoned id the server holds again, unattended except with the `claude-code` provider or a server this machine does not run (then `retain purge-again --allow-subscription`, named by doctor). It is within CONTRIBUTING's rule that memory is deleted only by an explicit user command: the user commanded this purge, and the late part is part of the session's document they purged.
- A tombstone of any member of a chain marks the chain purged: the next segment goes to the successor `D#2` (`LiveDocumentID`), a cursor whose chain lost a member is resynced (`lostToPurged`), and a pending replace on it is dropped.
- The look for recreated documents matches chains wherever it lists the bank: in the tick's listing of new documents, and in `jobs maintain`'s full look when it lists rather than reading the tombstoned ids one by one (its cost-aware choice is unchanged); a listed document whose chain is tombstoned counts as recreated. Read by id, the tombstoned ids are read, then the two parts after each chain's last tombstoned one (a part landed after an older binary's purge, which tombstones no predicted part); the cost-aware choice counts those reads.
- An older binary's purge deletes `D` (or the one part a purged fact came from) and leaves the session's other parts on the server. They do not hold the purged facts (each part is its own extraction), and their tombstoned chain sends the session's next segment to the successor. A newer binary's look that lists the bank (the `jobs maintain` one, when listing is the cheaper way) counts them as recreated and purges them again like any recreated document: their turns after the tombstone's last record go to the successor, so none is lost.

### P4 — Migration
- **Existing documents stay as they are**: an appended `D` holds segments 1…K; the session's next segment is `D#partK+1`, whose chain read starts at `seg(D) = K`.
- **A session in flight** (cursor: segment K of `D`): its next segment goes to `D#partK+1`; the cursor needs no change. A part the server lacks is resent with the same operation id.
- **Rollback to an older binary on the same PC**: it appends to `D` (its read of `D` finds an earlier segment ending at one of its own records); `D`'s metadata then holds that segment, and the chain read goes on from it. Nothing doubled or lost.
- **An older binary on another PC**, only for a session whose transcript was copied to both PCs: it reads `D` alone, so it may send again turns already in parts (stored twice, never lost); the newer PC reads its append in the chain and numbers after it. No check names such a PC: doctor's `version-skew` compares the server's Hindsight version with this backsight's pin, not backsight releases. The user docs (several PCs, how it works) say to update every PC together. A signal from the parts' `binary_version` metadata is not built (it needs a record of the other PCs' versions from the flush's reads).
- Telemetry and the metrics see each part as its own document (the server's operations name it); the pipeline counts each part as an item sent.

### P5 — Kept
- The append gate (0083 C1, `retain.append_min_chars` 30,000, `retain.append_max_wait` 6 h): it no longer prevents re-extraction, but it bounds the number of parts, each one retain operation and one more consolidation pass.
- Notes, subagent reports, imports and the first segment: unchanged.

## Expected
- Conversation segments after the first remove no fact (`facts_removed` 0 on their `document_events`, against 1,561 of 2,774 added in 3 days). The 1,561 replacement facts are about 30 % of the facts consolidated: roughly 20–30 % of the server's LLM spend, and stable observation ids under the feedback ledger.
- Measured a week after release with the same query (by document id `#part`).

## Not done, and why
- Rewriting `D`'s metadata after each part, so an older binary sees the position: Hindsight has no metadata update (`PATCH /documents` takes tags and scopes only), and a byte-identical re-retain to refresh it re-sends the whole text and depends on the delta path matching every chunk.
- A cursor marker document per session (0038's fallback): an older binary would not read it either, and it would cost a retain per send.
- A larger `retain_chunk_size`: fewer, larger tail chunks, the same churn per append.
