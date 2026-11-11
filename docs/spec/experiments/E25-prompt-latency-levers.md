# E25 — Prompt-recall latency levers on CPU

Date: 2026-10-25. Dev install (Hindsight 1.1.0, local reranker `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1` on CPU, bank `claude-memory`). Target: prompt-hook p95 ≤ 2.5 s (0048), carried from M2 (0055, 0056 block 3). **V** = observed.

## Where the time goes (**V**)
A traced recall (`trace: true`, one scope): 1.69 s in total, of which reranking took 1.66 s and retrieval 0.017 s. Reranking covers 60 candidates (`RERANKER_MAX_CANDIDATES=60`, set by `server render`), about 27 ms per pair. The candidates are about 580 characters at the median (≈ 150 tokens), 1 500 at most. The prompt path runs the project and global scopes in parallel, and they share the CPU, so one prompt pays about two reranks.

## Method
- **Prompts**: the 100 prompts of E23, replayed sequentially through `backsight recall-prompt` (scopes, floors and selection of the hook). For each variant, the dev Hindsight is restarted with a `compose.override.yaml` and warmed up with 3 prompts; the override is removed at the end.
- **Variants**: the number of candidates reranked (`HINDSIGHT_API_RERANKER_MAX_CANDIDATES`) and a per-candidate token cap (`HINDSIGHT_API_RERANKER_MAX_TOKENS_PER_CANDIDATE`, applied by Hindsight before any reranker).
- **Quality**, measured with E23's labels (0/1/2 by `deepseek-v4-pro`; 94–97 % of each variant's top-3 is labeled, and unlabeled pairs are left out):
  - useful injections;
  - prompts with a useful injection;
  - injection precision;
  - the paired nDCG@3 difference with the current setting (bootstrap 95 % CI).
- **Agreement**: how many prompts inject exactly the current setting's set.
- Scripts are in `E25-scripts/` (`run.py`, `analyze.py`, `quality.py`, `probe.py`). No LLM cost.

## Results (**V**)
| Variant | p50 | p95 | Same set as current | Prompts with a useful injection | Useful / labeled injections | nDCG@3 vs current |
|---|---|---|---|---|---|---|
| current: 60 candidates, whole text | 4.3 s | 6.3 s | 100 | 59 | 82 / 103 (80 %) | — |
| 30 candidates | 2.9 s | 4.3 s | 88 | 59 | 82 / 97 (85 %) | −0.004 (−0.016 … +0.006) |
| 128 tokens per candidate | 2.7 s | 4.0 s | 82 | 60 | 81 / 98 (83 %) | −0.006 (−0.028 … +0.014) |
| **30 candidates, 128 tokens** | **1.5 s** | **2.2 s** | 78 | **59** | **81 / 91 (89 %)** | −0.020 (−0.055 … +0.004) |
| 20 candidates, 128 tokens | 1.1 s | 1.5 s | 73 | 58 | 76 / 83 (92 %) | −0.040 (−0.083 … −0.003) |

The current setting is slower than in E23 (p95 4.5 s), as the bank has grown since then.

## Conclusions
1. Reranking is the whole cost on CPU. Only the two reranker levers together reach the target: 30 candidates with 128 tokens gives p95 2.2 s, from 6.3 s.
2. At 30 candidates and 128 tokens, the same 59 prompts get a useful injection. The injections dropped are mostly not useful (precision 80 % → 89 %). The nDCG@3 loss (−0.020) is not significant, and its CI lower bound sits at the edge of 0054's −0.05 margin.
3. 20 candidates costs a significant −0.040 nDCG@3 for 0.7 s more of saving: rejected.
4. **Decision**: `server render` sets 30 candidates and 128 tokens per candidate (decision 0057). The variants were chosen on E23's prompts, so the check on new prompts is E24 (M4 cut-over, 0056).

## Limits
- One machine, one CPU, a bank of a few hundred observations. A GPU reranker makes these levers unnecessary.
- The "skip the global scope when the project scope answers" lever was not needed and was not measured.

## Amendments (M3 block 3 review, 2026-10-25)
- **Non-inferiority**: the chosen setting's nDCG@3 CI lower bound (−0.055; −0.051 to −0.068 when unlabeled pairs count as 0 or 2) is below 0054's −0.05. It is a trade-off accepted on purpose, not a pass, and no selection rule was written before the run. See the INDEX row on 0057. E24 replays the old setting beside it.
- **Wording**: "the same 59 prompts" means 1 lost a useful injection and 1 gained one. Of the 26 injections dropped, 14 were not useful, 8 were useful and 4 were unlabeled. The precision gain has no CI.
- **Truncation risk**: Hindsight cuts candidates at 128 BPE tokens (≈ 500 characters) before the reranker, so about half the candidates lose their tail. That tail can be the actionable clause or identifiers.
- **Baseline latency**: it is above E23's because the related scope now runs (the entity index exists since 2026-10-25), making up to 3 parallel reranks. It is not because the bank grew.
- **Contaminated run**: a first run overlapped `go test -race` runs and was killed and discarded. The scored run overlapped only a reviewer's few searches during warm-up (first-20 median 4 301 ms against 4 284 ms for the rest).
