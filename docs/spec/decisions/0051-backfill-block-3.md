# 0051 — Backfill: order, notes, plan, budget, pacing (M1 block 3)

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, from decisions 0037, 0043, 0047, 0050, the live trial of block 2 and Hindsight 1.1.0's `llm-requests/stats`

## Decision
- **What**:
  - `retain backfill` takes every transcript under the Claude Code projects directory (`--projects-dir` for another profile), oldest first by first-record time.
  - It sends each one through the block-2 flusher with `Force` and `IgnoreSince`, so cursors, the fork guard, opt-outs, redaction and per-project runs all apply.
  - Sessions already on the server resume or are skipped (0038). A transcript's own record times date its segments.
  - Filters: `--since YYYY-MM-DD` (first record on or after) and `--project P`.
- **Explicit saves replayed as notes**:
  - Covered: every `tool_use` named `*memory_store` (input `kind`, `text`, optional `project`) and `*hindsight_ingest_document` (input `title`, `content`, `tags`).
  - Each becomes a note dated by its record.
  - The kind comes from the input, else from the title prefix (Décision/Decision, Correction, État/State, Convention, Piège/Pitfall, Leçon/Lesson), else `lesson`.
  - The project comes from the input, else a `project:` tag, else the session's.
  - The document id is `note:` UUIDv5(bank, session, tool_use id), so a rerun collapses server-side (E21).
  - Notes go through the outbox like `store`.
- **Plan** (`--plan`, also printed before a real run): sessions to send, those skipped (already sent, opted out), notes, characters, estimated tokens, cost and duration, and the cap.
  - Estimate per session: retain input = chunks × 3 300 + chars / 3.5 tokens.
  - Consolidation input = 1 600 tokens per fact, with one fact per 600 characters.
  - Output = 15 % of input.
  - Duration ≈ 1 min per 30 000 characters plus 0.5 s per fact.
  - Calibration: the block-2 trial spent 41 k tokens for 3 300 characters, mostly fixed prompt overhead. The figure is shown as an estimate.
- **Prices**:
  - Config `[llm] price_input`, `price_cached`, `price_output` (per million tokens) and `currency`, defaulting from a table in the binary for known models.
  - deepseek-v4-flash: $0.14 input, $0.0028 cached, $0.28 output (third-party tables checked 2026-10-24).
  - An unknown model has no price, so its cap is in tokens (`--max-tokens`).
- **Budget** (0047):
  - Default cap: 1.5 × the estimate.
  - Spend is measured, not estimated: the bank's `llm-requests/stats` token sums since the backfill started (baseline recorded at start), priced as above.
  - Checked before each session. When reached, backfill stops between sessions with `state.backfill.paused = "budget"`; `--max-cost`/`--max-tokens` resumes it.
- **Subscription lane** (0043): provider `claude-code` is refused unless `--allow-subscription`.
- **Pacing**:
  - Before each session, wait while the bank has more than 20 pending operations plus pending consolidations, polling every 5 s, so the server's queue stays short.
  - The run is foreground and resumable: cursors make Ctrl-C safe.
  - `state.backfill` records `{done, total, started_at, tokens_at_start, max_cost, max_tokens, currency, paused}`.
  - While a backfill is active and not paused, `jobs tick` continues it, up to 3 sessions per tick.

## Why
Backfill is the author's migration (0036) and a new user's first value. Reusing the flusher keeps one identity, privacy and dedup path. Measured spend makes the cap real whatever the estimate's error.
