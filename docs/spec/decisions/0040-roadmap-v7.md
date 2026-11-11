# 0040 — Roadmap order v7: write side and backfill before the read side

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (§20 Q6, following the agent's recommendation)

## Decision
ROADMAP v7: M1 fresh server + write side + backfill as a dev install beside the reference stack; M2 read side gated by a replay harness on real prompts; M3 behaviour; M4 onboarding and the author's cut-over (fresh setup + full backfill); M5-M7 unchanged. The reference stack stays the author's daily memory until M4.

## Why
With migration by rebuild (0036) there is no migrated data: the read side can only be tested against a bank filled by backfill, so the write side comes first.
