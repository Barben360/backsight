# 0093 — Credit per show by source: measured in `stats`, the drop outside backsight accepted

- **Date**: 2026-11-23
- **Status**: proposed (issue https://github.com/Barben360/backsight/issues/348; accepted when the author merges its pull request)
- **Decided by**: investigation of issue 348 on this machine's journal (read only), for the author to accept
- **Amends**: 0079 §4 (one more developer metric on the Quality page's ranking card), 0074 (the journal's shows and recall items carry the memory's source), SPEC A.3 (index migration 0008)

## Context
The one-week acceptance check of the efficiency audit (#230, check 4b, https://github.com/Barben360/backsight/issues/230#issuecomment-7004145197) measured by hand that credit per show (a memory shown, then credited `used` or `confirmed` in the same session) fell by about half on every source between 2026-11-01 → 08 and 2026-11-12 → 19, even at search rank ≤ 8; flat on the backsight project, down on the two other projects present in both weeks. `used.by_path` (credited out of credited + confidently missed) showed no significant drop, and no `stats` figure gave credit per show by source. Suspects named: stale memories (64 out-of-date reports), the wider prompt recall (#330), the wider search (#288, every hit above the floor, up to 20), the guide wording since 0083 C7.

## Decision

### A. `used.by_source` (developer metric, `stats quality --dev`, the Quality page's ranking card)
- **A show** is a memory's first prompt or search show of its session, any agent; **credited** when a credit of that session marks it `used` or `confirmed` at or after that show, in any turn (no settle is read). Briefing pages, briefing, store and other paths are not counted. A session's project is its turn's, as every quality figure; the version filter applies.
- **Headline**: ranks 1 to 8, both paths, every source (8 is what a search returned before #288, so windows on either side compare on the same ranks). Beside it: every rank (`all_ranks`), each source × path at all ranks and at ranks 1 to 8 (`by_source`), each project at ranks 1 to 8 (`by_project`). Every cell has its own status (min_n 20) and MDC (`1.96·√(0.5/n)`, null below 20).
- **Sources**: `conversation` (facts of a session), `note`, `conversation+note` (an observation drawn from both), `subagent` (any fact from a subagent's report), `other` (import, a source the binary does not write, other mixes), `unknown` (not recorded).
- **Where the source comes from**: the journal item's new `source` field, the memory's source words (`recall.SourcesOf`: every word `SourceOf` sums up, so `note+session` where the context line says mixed), sorted, joined by `+`. Prompt, gate and failure shows write it from the recall's hits; a search's show is read back from the tool's text, so its source is the MCP server's recall line of the same session and memory nearest in time. That fallback applies to any show without a source of its own, a prompt show included (one written before this release, or whose recall did not tell), whenever the session has a recall line for the memory. An observation whose facts the recall did not list in full has none: unknown, never a guess. Index migration 0008 adds `source` to `shows` and `recalls` (NULL before; a migrated index equals a fresh build). The journal stays ids, numbers and enums; the index keeps only sources of that shape, anything else reads `other`.
- Shows from before this release read `unknown`: the 2026-11-01 → 08 split by source cannot be recomputed by `stats` (it was joined by hand to the server's database); its per-project and per-path figures can.

### B. The drop outside backsight: accepted, no fix
The cause is how much is shown, not what: #288 doubled the hits per search while Claude credits about as many memories per answer as before, and half of those credits now sit at ranks 9 to 20. Credit per show falls on every source at once, which is that signature. Per project the drop is not distinguishable from chance (each is smaller than the baseline's MDC, and the MDC assumes independent shows while shows cluster by session, so it is if anything too small); pooled over the two projects it is real, and the mechanism above explains it. The latest days are back near the baseline. Nothing is rolled back (see "Not done").

## Evidence (2026-11-23, this machine, the journal read only)
Per project, from `used.by_source` itself (a dev build on a copy of the journal, Europe/Paris windows), ranks 1 to 8:

| Project | 11-01 → 08 | 11-12 → 19 | 11-19 → 23 |
|---|---|---|---|
| backsight | 29.1 % (100/344, MDC 7.5 pts) | 25.4 % (52/205) | 20.8 % (10/48) |
| .claude | 19.8 % (18/91, MDC 14.5 pts) | 6.8 % (8/118): −13.0 pts | no session |
| my-homelab | 23.4 % (15/64, MDC 17.3 pts) | 11.4 % (14/123): −12.0 pts | 19.1 % (22/115): −4.3 pts |
| proton-calendar-bridge | none | 10.9 % (40/368) | 14.8 % (8/54) |
| every project | 26.6 % (133/500) | 13.6 % (115/845) | 18.4 % (40/217) |

The suspects, by hand from the journal (UTC days; the two projects pooled unless said):
- **Mix**: backsight held 69 % of the shows at ranks 1 to 8 in the baseline, 24 % after, and credits more than the others (29 % vs 22 % in the baseline). At the baseline's rates per group the mix alone moves the all-project figure 27.0 % → 23.9 %; the rest of its halving is the other projects' own drop, below. New projects with young memory (proton-calendar-bridge, m8-*) credit 0 to 15 %.
- **#288, the wider search: the cause.** Hits per search call 6.5 to 7.3 → 15.4 (17.7 in the last days). Search credits by the main agent stay the same (29 → 27 → 30: 1.8 and 1.7 per call, 2.7 in the last days) but 48 % and 50 % of them land at ranks 9 to 20 (all projects: 44 % and 36 %): at ranks 1 to 8 credits per turn fall 1.06 → 0.58, at every rank they hold (1.14 → 1.09 credited memories per main turn) while shows per main turn go 4.6 → 9.1 → 16.1. So capping at rank 8 does not match the baseline either: the credits a capped list would have earned partly went to hits ranked 9 to 20.
- **#330, the wider prompt recall: not the cause.** Prompt items per call 1.8 → 2.4 → 3.0, prompt credit per show 14 % (4/29) → 13 % (8/60) → 29 % (7/24).
- **Stale memories: not the cause.** Wrong or out-of-date ids reported in these projects: 1 → 15 → 4; of the 241 shows at ranks 1 to 8 in 11-12 → 19, 3 were of a memory the session then reported; without them 9.1 % → 9.2 %.
- **The guide wording (0083 C7): not the cause.** Main turns with a prompt or search show that credited a memory: 45 % → 53 % → 92 %; turns that declared only `none`: 52 % → 34 % → 0 %; ids per `used` credit 2.4 → 1.8 → 2.9.
- **Small samples**: my-homelab's baseline is 2 sessions, one holding 45 of its 64 shows; .claude's 11-12 → 19 has 4 sessions, one with 44 of 70 main-agent shows. Subagents' own searches are credited 0 to 5 %: 48 of .claude's 118 shows in 11-12 → 19 (main agent alone: 24 % → 11 %). Pooled, the two projects' drop is real (−13.2 pts, session-bootstrap 95 % interval −18.4 to −3.9), and the mechanism above explains it.

## Not done, and why
- **No rollback of #288**: ranks 9 to 20 hold 36 to 50 % of the search credits; the agent credits as often and as much per answer. The cost side (search results are 55 % of the memory context) is #349's: ranks past 8 as one line, repeat hits as id only, checked against `used.by_source` at every rank and at ranks 1 to 8 so that trimming loses no credit.
- **No new index**: `used.by_source` (current and previous window) takes 14 ms on a 7-day window and 59 ms on 30 days of 13 months of generated history (`used.by_path` 8 and 37 ms), 406 ms at the longest 395-day window (`used.by_path` 222 ms); on a copy of this machine's index, 30 days take 78 ms with the catch-up (`used.by_path` 54 ms). The default windows are well within the page's 300 ms; the credit lookup (about 100 ms of the 395-day figure) would be the one to index if the longest window ever matters.
- **No change to `used.by_path`**: it answers another question (confident misses, settled per turn) and stayed flat, as expected under a wider list.

## Follow-up
Seven days after the release that carries this, read `backsight stats quality --dev --project <name>` per project: at ranks 1 to 8 within the MDC of the 11-01 → 08 figures above (backsight 29.1 % ± 7.5, .claude 19.8 % ± 14.5, my-homelab 23.4 % ± 17.3), and the split by source now recorded. A drop past the MDC on one source alone, at every rank, would be a new finding.
