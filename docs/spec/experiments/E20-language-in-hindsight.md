# E20 — Fact language inside Hindsight: French vs English facts (2026-10-24)

Question: does E19's +0.047 nDCG@3 for English facts (translated facts, offline pipeline) hold inside Hindsight, with real extraction and the real recall API? Planned by 0039 for M1.

## Setup

- **Server**: a throwaway backsight stack (Hindsight 1.1.0, compose project `backsight-e20`), multilingual preset (multilingual-e5-small + mmarco-mMiniLMv2-L12, 60 candidates), extraction `deepseek-v4-flash`, no `HINDSIGHT_API_LLM_OUTPUT_LANGUAGE`.
- **Banks**: `e20-en` and `e20-fr`, same backsight bank template except the first sentence of every strategy text and mission: "Write every fact in English / in French, whatever the language of the source".
- **Data**: the same 43 transcripts and 15 explicit saves of this machine, sent once to each bank by `backsight retain backfill` (two backsight homes).
- **Result**: e20-en 2 042 memory units, 774 observations; e20-fr 2 106 units, 804 observations. Language compliance (stop-word count): 2 041/2 042 English, 2 104/2 106 French.
- **Queries**: the 100 E19 queries (30 French prompts, 30 English prompts, 40 agent `memory_search` queries), first 300 chars, sent to `POST /memories/recall` with `types: [observation]`, `budget: low`; top 10 kept with the reranker score.
- **Labels**: every (query, memory) pair of both top-10s (2 000 pairs) graded 0/1/2 once by `deepseek-v4-pro` with the E19 rubric; the ideal ranking for nDCG takes the best grades across both banks.
- **Metrics**: as E19 (nDCG@3; useful injection = the hook rule, top 3 with reranker ≥ 0.5, injects ≥ 1 memory graded ≥ 1; injection precision); paired bootstrap, 4 000 resamples; queries with no useful memory in either bank dropped from the paired difference (97 left).
- Scripts: `experiments/E20-scripts/` (data stays local: it holds memory text).

## Results

| Bank | Group | n | nDCG@3 | Useful injection | Injection precision |
|---|---|---|---|---|---|
| e20-en | FR prompts | 30 | 0.51 | 53 % | 0.69 |
| e20-en | EN prompts | 30 | 0.45 | 40 % | 0.79 |
| e20-en | agent | 40 | 0.81 | 90 % | 0.95 |
| e20-en | all | 100 | **0.615** | 64 % | 0.86 |
| e20-fr | FR prompts | 30 | 0.57 | 57 % | 0.76 |
| e20-fr | EN prompts | 30 | 0.43 | 47 % | 0.55 |
| e20-fr | agent | 40 | 0.82 | 93 % | 0.98 |
| e20-fr | all | 100 | **0.634** | 68 % | 0.83 |

Paired difference EN − FR (95 % CI): all **−0.018** (−0.081 … +0.042); FR prompts −0.059 (−0.176 … +0.066); EN prompts +0.016 (−0.131 … +0.143); agent −0.012 (−0.085 … +0.055). None significant.

Extraction cost (Hindsight `llm-requests/stats`): EN $0.71, FR $0.74 — the same within noise.

## Findings

1. **E19's gain does not reproduce inside Hindsight.** With real extraction, graph/temporal retrieval and the live reranker, fact language makes no measurable difference (point estimate slightly in favour of French, CI −0.08 … +0.04). E19's translated facts were a weaker proxy than expected.
2. The absolute level matches E19 (0.58–0.63 there, 0.62–0.63 here), so the offline pipeline was a fair stand-in for ranking quality, not for the language effect.
3. The extractor follows the language rule almost perfectly (≥ 99.9 % compliance either way), so the rule itself is reliable.
4. Consequence for 0039: the recall argument is gone, the rest stands — one language for facts whatever the user writes (the author writes both; a shared server mixes users), one instruction set to test and size, no setup question. No reason found to reverse it; a French-speaking user loses nothing measurable either way.

## Limits

One judge model, one machine's data (43 sessions), 100 queries; wide CIs on the 30-query prompt groups (a 0.1 effect on French prompts cannot be excluded); one extraction per bank (no repeat to measure extraction variance).
