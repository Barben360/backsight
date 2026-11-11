# 0057 — Reranker settings for the prompt-hook latency target

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, M3 block 3 (0056), from experiment E25

## Decision
- `server render` sets these for the local reranker:
  - `HINDSIGHT_API_RERANKER_MAX_CANDIDATES=30` (was 60);
  - `HINDSIGHT_API_RERANKER_MAX_TOKENS_PER_CANDIDATE=128` (was none).
- They are constants of the rendered stack, not config keys. A user with a GPU or a remote reranker can override them in `compose.override.yaml`.
- The skip-global-scope lever of 0056 is not built: these settings alone meet the target on this machine.

## Why
- On CPU, reranking is about 98 % of a recall's time.
- With these settings, E25 measures the prompt path at p95 2.2 s, from 6.3 s, against 0048's 2.5 s target.
- The same 59 of 100 prompts receive a useful injection, and injection precision rises from 80 % to 89 %.
- The nDCG@3 loss (−0.020, CI −0.055 … +0.004) is not significant.
- E24, on prompts typed after 2026-10-24, checks the result on prompts that played no part in the choice.
