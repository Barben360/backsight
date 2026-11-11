# 0065 — Parity telemetry: a fixed window measured by `stats --parity` against C12

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, applying SPEC §1.2 and 0063 block 5 ("parity telemetry starts")

## Decision
- **Window**: from the cut-over, the end of `setup`'s `reference` step in the main profile (`<state>/setup.json`, 2026-10-25T11:33:22Z), rounded up to **2026-10-25T11:34Z → 2026-11-08T11:34Z** (14 days). After the cut-over, all telemetry comes from the main profile: the dev profile `~/.claude-backsight-dev` was archived into `~/.local/state/backsight/cutover-backup-20261025/` the same day.
- **Command**: `backsight stats --since 2026-10-25T11:34Z --until 2026-11-08T11:34Z --parity`.
  - `--since`/`--until` (RFC 3339 or `YYYY-MM-DD`, UTC) give `stats` an absolute window instead of the last N days, so the report can be rerun later on the same events. Telemetry is kept 12 months (`jobs maintain`).
  - `--parity` (hidden, author's machine) adds one line per metric: the baseline, the measured value with its counts, and a verdict, `pass`, `fail` or `too few` (denominator < 10).
- **Baseline**: C12's numbers for **this machine** (2026-10-07 → 2026-10-23, `research/C12-usage-data.md`), since §1.2 is about the author's Linux machine. The work machine's numbers (heavy use) are out of scope (author, 2026-10-25) and do not set the bar.

| Metric | C12 (this machine) | Rule | `stats` field |
|---|---|---|---|
| Recalls that injected something | 26 % (16/61) | ≥ | `prompts.with_hits / prompts.recalls` |
| Prompt hook p95 | 4.2 s | ≤ | `prompts.p95_ms` |
| Failed recall scope calls | 0 % (0/124) | ≤ | `prompts.scope_failures / prompts.scope_calls` |
| Injections followed by a search | 44 % (7/16) | ≥ | `injections.searched / injections.n` |
| Injections credited | 19 % (3/16) | ≥ | `injections.credited / injections.n` |
| Replies after a search that cite the memory (exact, free or empty) | 24 % (4/17) | ≥ | `turns.credit` |

- **Reading the result**: parity holds when no metric fails. `too few` is not a pass: the author decides at the end whether that metric gets a longer window or is waived. The small baseline counts (16 injections, 17 replies) make one-point differences noise, so every line shows its counts.
- **Not compared**: C12's search rate on long prompts was measured only on the work machine (34 %); `stats` shows it without a verdict. The AskUserQuestion gate fired once in C12, so it has no rate to compare.

## Why
§1.2 needs a two-week comparison after the cut-over, and `stats [days]` counts back from now, so the same report could not be produced again at the end of the window, or by a reviewer. An absolute window and fixed baseline rules leave nothing to choose on 2026-11-08 other than reading the result.

The definitions differ where the products differ: C12's "hit ≥ 0.5" is the Python hook's injection floor, and backsight's "injected something" applies its own floors (SPEC §10.2). Both measure "the prompt hook brought something", the outcome §1.2 is about. C12's latency is the recall alone; backsight's p95 is the whole hook (a stricter measure).
