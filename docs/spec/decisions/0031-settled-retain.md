# 0031 — Settled retain: sessions are sent once they settle, in 30 000-char chunks

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after experiment E18b
- **Amends**: 0018

## Decision
The Stop hook no longer sends anything: the transcript file is the durable log and the per-session cursor marks what was sent. A session's unsent turns are retained when the session **settles**: at SessionEnd (detached flush) or, for sessions left open, by `jobs tick` once the last turn is ≥ 30 min old; very long open sessions also send segments whose turns are ≥ 2 h old. Strategy `conversation` uses 30 000-char chunks; successive segments use `update_mode: append` on `conversation:<session_id>`. Notes and subagent reports keep an immediate outbox. PreCompact needs no flush (compaction does not touch the transcript file).

## Why
E18b on a long debugging session: settled retain kept 1 overturned hypothesis vs 4-6 for per-turn appends, with equal or better coverage (7.5 vs 7.0 of 9 key items), 43 vs 75 observations and ~15 % vs ~35 % noise. Cost: memories reach other sessions ≥ 30 min later (the current session has its own context; explicit `memory_store` stays immediate). It also removes most of the outbox machinery for session text.
