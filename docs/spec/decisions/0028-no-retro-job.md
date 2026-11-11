# 0028 — No scheduled retro; the session-retain strategy carries the durability rules

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments
- **Supersedes**: 0019

## Decision
No retro job in v1. The `conversation` and `document` strategies carry the language rule and the retro's durability rules (durable, typed through `kind` labels, absolute dates, exact identifiers, nothing for plans or hypotheses). The end-of-project retro is the `memory-retro` skill (agent-written facts through `memory_store`). The Python `memory-retro.py` is retired after a two-week comparison on the author's machine (ROADMAP M3).

## Why
E17: under project-only pools (0015) consolidation merges both writers, so a separate retro pass re-extracts what session retain already holds for a second extraction cost; the same rules inside the `conversation` strategy give the same coverage in one pass and fix the English drift of the stock strategy. Removes the 60-minute settle logic, tranche selection and budgets.
