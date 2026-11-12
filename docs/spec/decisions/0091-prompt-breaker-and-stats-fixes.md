# 0091 — A recall that times out opens the breaker; three stats numbers made honest

- **Date**: 2026-11-12
- **Status**: accepted
- **Decided by**: author (2026-11-12: "Run all fixes that you can do deterministically and you think useful right now … You are autonomous on that"), on the deep review of 2026-11-12

## Decision

### A. The prompt hook and a server that hangs
- **One timeout is enough.** A prompt recall (or a tool-failure recall) where a call failed because the search ran out of its own budget (`recall.Result.TimedOut`: the search's context hit its deadline; a server that took the call and did not answer) opens the breaker at once, for `network.breaker_pause_min`. A fast failure (an error status, a refused connection, the 3 s connect bound) keeps the old rule: `network.breaker_failures` (3) in a row.
- **The startup briefing counts too.** A SessionStart briefing whose read ran out of its 5 s opens the breaker the same way; a fast briefing failure still records nothing.
- **A bounded probe after the pause.** Once the pause is over (`breaker.json` `open_until` in the past, not reset), every recall is a probe and gets `probeBudget` = 8 s instead of the 17 s budget (the tool-failure recall keeps its 6 s, already shorter), until one answers and resets the breaker. A probe that times out opens the breaker again at once; one that fails fast counts toward the 3 as before.
- **The 17 s budget stays.** SPEC §6.2's budget is unchanged, as are the hook deadlines (25 s / 20 s).

### B. Stats
1. **`memory.yield` ("Memories that proved useful")**: the definition stays (0079 §4: per week of creation, the share used within 28 days; the weeks span 90 days whatever the view's window). The leading weeks that made no memory (a denominator of 0 from the server or the record) and none of whose memories was used are no longer reported, and the latest complete week is taken among the weeks reported; when none of the 90 days made a memory, the reason is `no_events`. A week whose 28 days are not over reads, in `stats cost`, "not complete until <date> (its last day, <date>, plus 28 days)", as the dashboard's row already did, and the weeks are listed under a `no_complete_week` headline too.
2. **"Searched"**: `search.rate` ("Prompts where Claude searched memory") counts `memory_search` and `memory_read`, no longer `memory_store`, the rule of `stats`' Searches line and of its injections followed by a search (0079 §4's row amended here). doctor's `searches` summary reads "injections followed by a search …" (the words of `stats`' Injections line), not "search after injection".
3. **"Often shown, never used"** (`stale.never_used`): its count is unchanged (`feedback.Stale`, the threshold `feedback.stale_shows`, default 10, over all time). The metric carries `extra.min_shows`, `stats` prints it under the count, and its definition says it is not the window's shows.

## Why
- **A**: a server that accepts the connection but does not answer (its reranker saturated by an extraction job, on a CPU) made each prompt wait the whole 17 s, three times, before the breaker opened, then once more for each prompt after every pause; the docs say hooks never block. A fast refusal costs a prompt milliseconds (3 s at most), so counting 3 of them is cheap; a timeout costs 17 s, so one is enough.
- **The budgets**: the author's CPU-only install measured over 457 prompt recalls (2026-10-14 to 2026-11-13, telemetry): p50 0.96 s, p95 2.3 s, p99 3.2 s, max 5.1 s; E23 measured p95 4.5 s and E25's baseline p95 6.3 s before 0057's reranker settings. 8 s is above all of them, so a probe on a working server answers, while a server still hanging costs a prompt 8 s per pause instead of 17 s. The 17 s stays because, with the breaker opening on the first timeout, it is paid once per stall; cutting it would trade successful slow recalls (a far server, a slower CPU, a model loading after a restart) for a few seconds on that one prompt, which the measurements do not justify.
- **B1**: on the author's install (bank from 2026-10-24), `stats cost --days 30` on 2026-11-12 listed weeks from 2026-08-10 as "0 of 0" and headlined "0 of 0 (20 more needed)": the latest complete week (2026-10-05) came before any memory. The cohorts themselves were right: in a copy of the index, the week of 2026-10-19 holds 4 304 listed memories, 29 of them used, and no week with memories is complete before 2026-11-23. "Not complete yet" for a week that is over read as a bug.
- **B2**: on the same data, `stats` said 90 of 269 prompts (33 %) searched, `stats quality` 68 % of the same 269 prompts, and doctor 50 % (of injections): stores, which the instruction set asks for often (256 stores against 144 searches), made the quality figure count prompts where Claude only wrote. "Does Claude look in memory on its own?" is about searching.
- **B3**: the most shown memory of the window had 8 shows and no use, under the threshold of 10, so the count of 0 was right but read as wrong without the threshold.

## Not done
- **A background probe** (the jobs tick probing while the breaker is open, so no prompt ever pays a probe): it needs a scheduler, and an install without one would never resume recall.
- **A shorter budget for every recall**: see Why.
- **Provisional figures for incomplete yield weeks** ("29 of 4 304 so far"): a new number, not a fix.
- **`feedback.stale_shows`' default**: left to the pull request that owns the feedback defaults.
- **Known limit**: a server whose recalls take between 8 s and 17 s and keeps doing so stays paused once a timeout opened the breaker: each probe times out. No such recall was measured.
