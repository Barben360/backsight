# 0056 — M3 in four blocks, and what the spec leaves open for it

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, at the start of M3 (author: "Go … you are autonomous", 2026-10-25)

## Decision

### Blocks (each closed by its own review, then the M3 review)
1. **User commands and pause**:
   - `pause [--project]` and `resume` (session flag; `--project` writes `privacy.exclude_paths` through a line edit of config.toml);
   - `forget <id> [--purge]`: soft invalidation, reversible; `--purge` deletes the memory, its source facts and its document, and lists the backups that still hold it;
   - `memory scan` (stored memories matching secret shapes, with the `forget --purge` command for each);
   - `ask <question>` (Hindsight reflect) and `projects` (project tags with counts and last activity).
   - **The MCP server finds its session.** Claude Code starts hooks and MCP servers as direct children of the same process. The hook supervisor records `<state>/sessions/by-pid/<claude pid>.json` = `{session_id, cwd, project, at}` at every event. The MCP server reads the record of its own parent pid before each tool call, which gives it the live session (pause) and the live project (a `cd` is followed). Without a record (an older Claude Code, a manual run), it falls back to its start directory and no session.
2. **Skills, stats, doctor thresholds**:
   - the plugin gains the skills `remember`, `pause`, `memory-retro` and `memory` (SPEC §9.2, §17), generated like the rest;
   - `stats [days]` from telemetry and Hindsight's LLM stats (§15);
   - the doctor thresholds computable before M4/M5: recall failure rate > 5 % over 7 days; no retain accepted in 24 h while sessions ran; prompt-hook p95 > 2.5 s over 7 days; search-after-injection rate halved week over week; unknown transcript records rising; job last run > 2× cadence once a scheduler exists. Backups, restore epoch and updates come with M4/M5.
3. **Quality items carried from M2 (0055)**:
   - French prompts against English facts: a measured fix, evaluated on prompts outside E23's;
   - prompt-hook latency toward p95 ≤ 2.5 s: measured levers, such as fewer rerank candidates or skipping the global scope when the project scope already answers;
   - E14 (a mental model as the briefing): an experiment that decides whether it replaces the E8 briefing.
4. **Gate**:
   - **Detector follow-up** is measured on the replay of this machine's transcripts: for each prompt where the detector fires, whether an explicit save followed in the same turn or the next. In-use telemetry starts with the M4 cut-over.
   - **Replay gate rerun (E24)** with 0054's rule on prompts typed after 2026-10-24.
     - With fewer than 50 new prompts at the end of M3, E24 moves to the M4 cut-over.
     - M3 then closes on the detector measure and the reviews, as M2 closed on E23.

## Why
- Block 1 makes the user's own controls exist before the cut-over, starting with the pause the first-session notice should be able to name.
- The parent-pid record solves the MCP limit recorded at the end of M2 deterministically, with no protocol support needed.
- Blocks 2 and 3 are what the M4 cut-over will rely on.
- The gate keeps E23's no-tuning-on-test-prompts rule.
