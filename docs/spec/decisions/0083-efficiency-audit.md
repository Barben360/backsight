# 0083 — Efficiency: stop re-extraction churn, floor briefing refreshes, gate subagent reports, trim Claude-side context

- **Date**: 2026-11-08
- **Status**: accepted
- **Decided by**: author (2026-11-08: "Change briefing refresh floor with your recommendation. On a further release, it should also be forcefully applied for users, not only here." · "Run the purge. Why isn't it done automatically in backsight engine? It should be something automated if it's waste." · "I follow your roadmap, your recommendations are excellent."), on the agent's audit of this machine's statistics, Hindsight's `llm_requests` and `async_operations` tables, 69 transcripts and the code at backsight c66ddda

## Context
The audit (report in the epic issue) measured, for the Haiku period 2026-11-01 16:15 → 2026-11-08 (6.86 days): server LLM 13.0 USD (≈ 57 USD/month): consolidation 7.22 (56 %), briefing refresh 4.01 (31 %), retain 1.71 (13 %). Two pipeline mechanics, not memory content, drive most of it, and one of them also damages quality.

## Decision

### C1 — Conversation appends wait for enough text (amends E18b / 0031 "settled retain")
Hindsight re-chunks a whole document on append and re-extracts every chunk whose text changed; with `retain_chunk_size` 30,000 the tail chunk changes on nearly every append, so its facts and the observations drawn from them are deleted and recreated under new ids. The long-open rule (segments ≥ 2 h old, sent by every 15-min tick) makes this happen dozens of times per long session.
- An **append** to a conversation document that already exists on the server is sent only when one of these holds: the pending rendered text is at least `retain.append_min_chars` (default 20,000); or the document's last send is at least `retain.append_max_wait` old (default 6 h) and the turns are settled (≥ 30 min) or long-open (≥ 2 h); or the session ended (SessionEnd, `--force`, resync).
- The **first** send of a session, resyncs, purge continuations (`#2`) and every other path are unchanged.
- Expected: appends −80 % or more; the share of `facts_removed` in `facts_added` for appends (63 % measured) under 20 %; stable observation ids, so the feedback ledger's credits stop pointing at deleted observations (79 of ≈ 198 did).

### C2 — Briefing refresh floor, enforced on every install (amends 0060)
- Pages are created with `trigger.min_refresh_interval_seconds` = `recall.briefing_refresh_interval` (default 6 h, 21600 s), keeping `refresh_after_consolidation: true`, `mode: full`, `budget: low`.
- **Enforced, not only for new pages**: backsight reconciles the trigger of every `backsight-brief-*` page of its bank (a PATCH when it differs from the wanted one) in `jobs maintain` and right after an update (`update apply`), so existing installs get the floor without a new page. The page read at SessionStart reports a drifted trigger to `jobs maintain` (no PATCH in the hook's budget).
- Doctor's `bank` check reports a page whose trigger drifted.
- Applied by hand on the author's server on 2026-11-08 (the 4 pages).

### C3 — Subagent reports: a gate at the door and an automatic sweep of the legacy waste (amends SPEC §9 subagent strategy, 0071 C5)
0071 C5 assumed a status-only report yields no fact. Measured: 1,274 of 1,406 subagent documents were under 150 characters ("Keep going with block 2"), they produced 875 facts, and subagent-only observations were credited 1 time in 112 shows.
- SubagentStop queues a report only when its trimmed text is at least `hooks.subagent_report_min_chars` (default 400) or it holds a URL or an issue/PR reference (`#123`, `owner/repo#123`).
- **Automatic sweep** (the author: waste must be removed automatically): `jobs maintain` purges, under the maintenance lock and with the existing purge path (documents, facts, the observations drawn from them, tombstones), every `subagent:*` document whose text fails the same gate. It runs once per gate version (state marker) and again only when the gate changes; it logs the count and emits telemetry; the daily backup taken before it keeps the data.
- Answer to the author's "why isn't it automated": backsight never deleted anything on its own (retirements only, 0061 and SPEC §8), and nothing measured the value of a source; this decision makes a measured, rule-based purge of one source automatic, and keeps that rule narrow.
- The author's legacy stub session (41a3ab69, 1,102 documents tagged `project:ref-stack`) was purged by hand on 2026-11-08.

### C4 — `memory_store` replies: one similar memory at most, and credit accepts what a store showed (amends SPEC §7 store)
Measured over 21 days: store replies were 887 k of 2.2 M characters of memory context (40 %); the 3 "similar" memories matched the new fact (word Jaccard ≥ 0.3) in 1.4 % of stores and 0 of 387 replies said "Updated"; 16 of 139 `memory_credit` calls were refused because their ids were shown only by a store, losing 8 of 22 corrections.
- The reply lists at most 1 similar memory, only when it passes `recall.store_similar_min_score` (default 0.8, the reranker score), cut at 200 characters, then the print line. Otherwise the reply is the saved line alone.
- Memories a store reply showed are recorded as shown (`how: store`), so `memory_credit` accepts them.

### C5 — After a compaction, the briefing only when it changed (amends 0060 compaction re-injection)
The guide is sent again after every compaction; the briefing and "the last memories shown" only when the page's `last_refreshed_at` differs from the one injected earlier in the session. The "Stored this session" line keeps the short ids, not full uuids.

### C6 — Status trivia out of extraction and out of stores (amends the retain strategies' custom instructions and the memory guide)
- The `conversation`, `document` and `subagent` strategies keep one fact per **delivered outcome** (a PR merged with what it closes, a release published) and stop extracting **process steps**: commits made, pushes, branches created or deleted, rebases, CI runs and their status, PRs opened or updated.
- The memory guide stops asking for `memory_store` of merges, releases and CI status (the conversation records them); it keeps decisions, conventions, pitfalls, corrections and lessons.
- Gate before merge: replay ≥ 10 past sessions into a throwaway bank with the old and the new instructions; durable facts kept (manual classification, ≥ 95 % of the old durable ones), process-step facts −50 % or more.

### C7 — Context text: one guide, shorter tool descriptions, conventions in the briefing
- The SessionStart guide and the MCP server instructions stop repeating each other (≈ 740 characters overlap); `memory_credit` and `memory_store` descriptions are cut by ≈ 40 % with no rule lost.
- The briefing query asks for the project's standing process rules (release, PR, worktree conventions) under "In force" (13 % of `memory_search` queries looked them up again each session).

### C8 — Measure what the audit could not (telemetry)
Session id on every note's metadata (174 of 452 had none); briefing page ids in the shown record; per-query hit counts and `result_chars` on the `tool` event; Stop-check block outcome per path. The credit loop (Stop-check blocks on search turns, implicit `none`) is decided after one week of data from C1 to C8, not now.

### C9 — Small cleanups
`PostToolUseFailure` is registered only when `recall.failure_pitfalls = true` (doctor's wiring check follows).

## Not done, and why
Skipping conversation retain when the session stored notes (it would drop 84–96 % of conversation facts, and observations drawn from both sources are the best credited, 12.7 % per show); dedup before retain (≈ 3–8 % near-duplicates); a recall pre-filter (no signal predicts empty recalls); a lower search limit (ranks 6–8 hold 30 % of credited hits); a per-session search cache (3 % repeats); prompt-cache work (stable prefix already cached; the rest is upstream); a cheaper model per operation (after C1–C3). Later, as exploration: briefing refresh on demand at SessionStart instead of after consolidation; recall scopes merged into one tag-filtered call (needs an A/B, overlap@10 ≥ 0.9).

## Evidence (2026-11-08, this machine)
- `async_operations.result_metadata.document_events` since 2026-11-01 16:15: conversation appends 210 events, 5,470 facts added, 4,256 removed; first conversation retains 32 / 357 / 0; notes 168 / 653 / 0; subagent 131 / 310 / 74. Over 14 days: 603 appends on 34 documents (8 documents: 560), 16,310 observations removed.
- Mental models: 4 pages, trigger `refresh_after_consolidation: true`, `min_refresh_interval_seconds: null`; 353 refreshes in 6.86 days vs ≈ 5 reads a day; one refresh ≈ 132 k input tokens ≈ 0.012 USD; a 1 h floor removes 74 % of refreshes, 6 h 92 % (replayed on the 14-day job log).
- Claude-side (21 days, 69 sessions): store replies 887 k, search results 604 k, startup and compaction 367 k, prompt recall 85 k characters.
- Ingestion overlap: 4–16 % of conversation facts have a same-session note twin; credit per show of observations: conversation + note 12.7 %, conversation only 7.7 %, note only 5.5 %, subagent only 0.9 %.
