# 0016 — Corrections are new facts; invalidation is a user command

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
A correction is stored with `memory_store` (kind correction) in the right project, or the transverse project for user-wide rules; consolidation rewrites the observation. Soft invalidation (`state: invalidated`) is exposed only as the user command `forget`.

## Why
E11 shows consolidation updates the observation with history. Agent-supplied ids for invalidation risk wrong or hallucinated ids and over-invalidation of multi-source observations (review, 2026-10-24).
