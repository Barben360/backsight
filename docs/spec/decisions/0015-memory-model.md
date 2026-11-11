# 0015 — One bank, one project tag per write, project-only observation scopes

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Every write carries `project:<name>` and `observation_scopes: [["project:<name>"]]`; the bank default is `[["project:*"]]`. Other tags (`source:*`, `harness:*`, `kind:*`) stay on raw facts. Reads use observations; raw facts are evidence.

## Why
E10: with `combined` scopes chat and retro facts form duplicate observations (12 vs 5). E11: only project-only pools let consolidation rewrite a stale observation after a correction. E12: an existing bank can be rebuilt under these scopes (all observations tagged, recall usefulness equivalent).
