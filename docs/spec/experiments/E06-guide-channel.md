# E6 — Which instruction channel makes the agent search memory first?

Date: 2026-10-23. Claude Code 2.1.309, model Sonnet, `claude -p`, isolated config, small git repo, fake `memory_search` returning "No memory found". **V** = measured.

Guide text (≈ 700 chars, English, in `experiments/` notes below) delivered through:
- `none`: only the MCP tool description;
- `hook`: SessionStart `additionalContext`;
- `claudemd`: user `CLAUDE.md`;
- `hook+ups`: SessionStart guide + one-line reminder in every UserPromptSubmit `additionalContext`.

Prompts: factual about past setup ("Which port should the reranker service listen on?"), task start ("Add a CI workflow"), recommendation ("pytest or unittest?"), past reference ("Like last time, set up the backup timer"), naming, "What did we decide about the database backups?", "Write tests for app.py".

| Condition | Runs | Searched memory | Searched **first** (before any other tool) |
|---|---|---|---|
| none (round 1) | 5 | 2 | 1 |
| hook | 14 | 14 | 9 |
| claudemd | 14 | 14 | 13 |
| hook+ups | 14 | 14 | 13 |

Reading:
- Without any guide the agent rarely searches: the tool description alone is not enough.
- The SessionStart guide makes it search every time; adding a short per-prompt reminder brings "search first" to the level of a CLAUDE.md block.
- Limits: one model, one-prompt sessions (the SessionStart guide is at its most visible), fake empty results, small n. Long sessions are where per-prompt cues matter more, which favours the reminder.

Decision input: no CLAUDE.md edit by default; guide through SessionStart (re-fired on compact/clear/resume) + the per-prompt reminder logic already in production; CLAUDE.md block as an opt-in.
