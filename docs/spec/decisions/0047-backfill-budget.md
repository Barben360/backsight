# 0047 — Backfill spends within a cap

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- `retain backfill` takes `--max-cost <amount>` (in the provider's currency, from Hindsight's LLM token traces and the price table of §12). The default is 1.5 × the `--plan` estimate, shown and confirmed at setup.
- When the cap is reached, backfill stops cleanly between sessions, keeps its state (resumable with a new cap), and `doctor` shows "backfill paused: budget reached".
- Lanes with no price (Ollama, subscription when allowed) are capped in tokens instead.

## Why
Backfill runs unattended through `jobs tick`. An estimate shown once is not a limit.
