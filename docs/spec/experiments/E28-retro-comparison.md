# E28 — The Python retro's real output vs session retain, before retiring it (2026-10-25)

SPEC §9.4's comparison, run at the M4 cut-over (0063 block 5, readiness). **V** = measured.

## Setup
- **Window**: 2026-10-23 17:00 → 2026-10-25 03:30 UTC. Both methods saw the same turns: the retro's runs up to 03:11 (60 min settle margin) and the dev bank's cursor in the same session (02:14). Nearly all of the week's activity is one long session (the backsight build).
- **Retro**: the 17 facts `memory-retro.py` really wrote in the window (`~/.hindsight/retro/retro-log.jsonl`), not a backtest.
- **Session retain**: the dev bank (`claude-memory`, settled retain, DeepSeek). 440 raw facts of project `ref-stack` in the window; 20 sampled (seed 28).
- **Coverage**: for each retro fact, the dev bank's top 5 recall hits (raw facts and observations). Judged held (2), partly held (1) or not held (0).
- **Precision**: the 37 facts mixed and shuffled under opaque keys. Judged for value (2 durable and useful, 1 of little lasting use, 0 noise), staleness and error.
- Judge: `deepseek-v4-pro`. Scripts: `experiments/E28-scripts/` (data local).

## Results **V**
| | Retro | Session retain |
|---|---|---|
| Facts in the window | 17 | 440 |
| Value 2 / 1 / 0 | 15 / 2 / 0 | 14 / 6 / 0 (of 20) |
| Mean value | 1.88 | 1.70 |
| Stale | 4 | 3 |
| Errors | 0 | 0 |

**Retro facts held by session retain: 10 fully, 5 partly, 2 not** (88 % at least partly).
- **Not held**: a final state (a merged branch deleted, both clones on `main`), and a research fact (Hindsight's release cadence and compatibility, which lives in the spec repo's research C03).
- **Partly held**: the gist is there, without the retro's synthesised lesson or one detail. For example, "shortening the query degraded quality, abandoned" is missing from the reranker latency fact.

## Reading
- The retro writes few, dense facts: high value per fact, but 4 of its 17 already outdated. Session retain writes about 25 times more, slightly less valuable each, and already holds what the retro adds in 15 of 17 cases.
- The retro's distinct value is its synthesised lessons. In backsight they come from the `memory-retro` skill, on demand. This confirms 0028 and E17/E18: `memory-retro.py` and its timer are retired at the cut-over.

## Limits
- One window and one project, dominated by a single session.
- Coverage uses recall's top 5, so a fact held but not retrieved counts as missing (an under-estimate).
- One judge. Language differs by method (French retro, English facts), so the precision grading is not fully blind.
