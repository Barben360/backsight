# 0093 — Lighter `memory_search` answers: one line past rank 8, repeats by id, credit answers trimmed

- **Date**: 2026-11-23
- **Status**: accepted
- **Decided by**: author, issue https://github.com/Barben360/backsight/issues/349 (opened from the 2026-11-19 measurement, https://github.com/Barben360/backsight/issues/230#issuecomment-7004145197, check 5)
- **Amends**: SPEC §6.2 (PostToolUse, SubagentStart, SubagentStop), §7 (`memory_search`, `memory_credit`), A.3 (`sessions/<id>.json`); 0074 (the accepted credit answer's prefix)

## Context
Since https://github.com/Barben360/backsight/pull/288 (every hit above the floor, up to 20), `memory_search` answers became the largest share of the memory text a session reads: 427 k of 771 k characters over 2026-11-12 → 19 (55 %), 7.4 → 15.0 hits and 3.5 k → 4.5 k characters per call, half of the hits past rank 8, 6 % already shown earlier in the session. The memory context per session missed its −40 % target (−19 % p50, −28 % p95). `memory_credit` answers grew to about 355 characters per call.

## Decision

### A. Past rank 8, one line
Ranks 1 to 8 keep their line unchanged. From rank 9, a hit is `- #N <text cut at 120 characters, …> (<date>[, from <source>], <id>, <score>)`: no kinds, no age. The source stays (0089 P2: every memory line names it). `memory_read` gives the whole memory. `backsight search` (the terminal) keeps every hit whole.

### B. Already shown: the id
A hit the call's context already holds whole is `- #N (already shown, <id>, <score>)`, at any rank. "Holds whole" is:
- in the session state's seen set, the main agent's ids since the last compaction (SessionStart `compact` leaves only what it replayed), and
- journaled shown to the main agent (`agent` empty) by any path but a `search` line past rank 8 (a shortened line is not the memory).

None is given by id only:
- while a subagent of the session runs: the server cannot tell a subagent's call from the main agent's, and a subagent has none of the main context. SubagentStart records every subagent (skip-listed ones too) in the session state's `subagents`, with its start; SubagentStop removes it; one recorded more than 12 h ago is taken as gone;
- unless the call's session is certainly its own (the by-pid record, or the session a Codex call names): not for a session found by its id only, which after a `/clear` names the previous one;
- when the session state or the journal cannot be read, or holds nothing (with `feedback.enabled` off the journal holds no shows: every hit is whole).

### C. Every line stays creditable
A shortened or id-only line keeps its rank, its full id and its score, so the PostToolUse hook journals the show as before (`- #N … , <id>, <score>)`), the seen set takes its id, and `memory_credit` and `memory_read` take its short id. The transcript importer's pattern already reads both forms.

### D. Credit answers: the line to print
`memory_credit` `used` and `confirmed` answer their lines alone (one per project), without `Credited. Print this line in your final message:` / `Confirmed. …`: the tool's description already says it returns the line to print. The PostToolUse hook knows an accepted credit by the line's start (`> 🧠 **From memory (`, `> 🧠✔ **Confirmed by memory (`), and still by `Credited.`/`Confirmed.` from a server started before an update. `none`, `wrong`, `outdated` and `Sources:` answers are unchanged: they carry what was stored, retired or listed.

### E. Instruction set v23
`memory_read`'s description adds: "A memory_search hit past #8 is cut (…), one already shown this session is its id only: read either here." The static context grows by 105 characters (5 215 in v23, budget 5 250).

## Measured
Replay of the last 50 `memory_search` answers with hits of the author's machine (Claude Code main transcripts, 2026-11-14 → 2026-11-22, read only), each parsed back into hits (all 50 re-render byte for byte in the old layout) and rendered with this layout; repeats from the transcript's own context (memory tool results and hook context since the last compaction), no repeat while a foreground subagent ran:

| | Before | After |
|---|---|---|
| Characters per call | 6 883 | 4 734 (−31.2 %; median call −26.1 %) |
| Hits per call | 15.7 | 15.7 (407 past rank 8, 26 id only) |
| Ids credited later in the session still returned | 119 | 119 (100 %): 64 whole, 49 shortened, 6 id only |
| `memory_credit` used/confirmed answer | 170 chars | 120 chars |

Over the last 200 searches (from 2026-11-02, partly before the 20-hit limit): 5 682 → 4 375 characters per call (−23.0 %), 479 credited ids all returned.

The −50 % target of the issue is not reached by A to D: ranks 1 to 8 alone held 47 % of the characters before (3 232 of 6 883 per call), and they are kept whole. Even hits past rank 8 reduced to their id would leave about −45 %.

## Not done
- **Shortening ranks 1 to 8, or fewer whole ranks**: what the −50 % would take (cutting ranks 1 to 8 at 300 characters as well gives about −47 %; 5 whole ranks about −42 %, estimated on the same 50). Left to the author: 41 % of the credited ids of the replay were past rank 8, so how much a shortened line still gets credited is measured first, on a week of data (issue 349's last check).
- **`memory_store`, `Sources:` and retirement answers**: unchanged (rare, and they say what was stored or retired).
- **A telemetry field for shortened or id-only hits**: `tool.result_chars` already measures the weight; the shows keep their ranks.

## Known limits
- A background subagent turns the repeats off for the main agent while it runs.
- Two searches sent in one message both answer whole: the second runs before the first's hook added its hits to the seen set.
- A memory a prompt shortened (to 160 characters, when its budget runs out), a briefing item (220) or a `memory_store` answer (200) showed counts as held whole: only the search line this layout cuts (120) is left out, so a memory shown that way once is never given by id only on the strength of that line.
