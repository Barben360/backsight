# E19 — Memory language: French vs English facts, multilingual vs English models (2026-10-24)

Question (author, 2026-10-24): would forcing every memory, and every interaction, into English improve recall, since English-only embedding models are more focused, and would it remove the language choice?

## Setup

- **Memories**: the 218 live observations of the author's bank (French), plus an English translation of each (DeepSeek `deepseek-v4-flash`, identifiers kept) as a proxy for English extraction.
- **Queries** (100, real, from the author's transcripts of the last 30 days): 30 French prompts, 30 English prompts (first 300 chars, fenced code removed, as the hook does), 40 `memory_search` queries written by the agent (55 of 58 such queries were already English). Prompts were also translated (FR→EN, EN→FR) to simulate "everyone types English / French".
- **Pipeline** (approximation of Hindsight recall without graph/temporal): dense top-30 ∪ BM25 top-30 → cross-encoder rerank (sigmoid scores) → top-10.
- **Model sets**: `multi-small` = multilingual-e5-small + mmarco-mMiniLMv2-L12 (today's preset); `en-small` = bge-small-en-v1.5 + ms-marco-MiniLM-L-6 (English preset); `multi-large` = bge-m3 + bge-reranker-v2-m3 (fp16 on the iGPU, top-15 ∪ 15 candidates, so slightly handicapped); `m3-embed+small-rr` = bge-m3 + mmarco reranker.
- **Ground truth**: every (query, memory) pair in the union of all top-10s graded once by `deepseek-v4-pro` (0 / 1 / 2, usefulness for the prompt, French original text), 3 386 pairs, so all configurations share the same labels.
- **Metrics**: nDCG@3 (graded); "useful injection" = share of prompts where the hook rule (top 3, score ≥ 0.5) injects at least one memory graded ≥ 1. Paired bootstrap (4 000 resamples) for differences.
- Scripts: `experiments/E19-scripts/` (data files stay local: they hold memory text).

## Results (nDCG@3)

| Config (facts / models) | FR prompts | EN prompts | agent queries | all |
|---|---|---|---|---|
| FR facts / multi-small (today) | 0.49 | 0.35 | 0.80 | 0.58 |
| EN facts / multi-small | 0.56 | 0.37 | 0.84 | 0.63 |
| EN facts / en-small | 0.49 | 0.49 | 0.81 | 0.63 |
| EN facts / en-small, every prompt translated to English | 0.63 | 0.49 | — | — |
| FR facts / bge-m3 + small reranker | 0.47 | 0.42 | 0.78 | 0.59 |
| EN facts / bge-m3 + small reranker | 0.52 | 0.40 | 0.82 | 0.62 |
| FR facts / multi-large | 0.76 | 0.51 | 0.82 | 0.72 |
| EN facts / multi-large | 0.75 | 0.50 | 0.83 | 0.72 |

Paired differences vs today (95 % CI):
- EN facts, same models: **+0.047** (+0.010 … +0.087) overall; agent +0.046 (+0.009 … +0.086); FR prompts +0.07 (n.s.).
- EN facts + English models: +0.042 (−0.023 … +0.105) overall, n.s.; EN prompts **+0.14** (+0.01 … +0.27); FR prompts −0.005 (n.s.).
- multi-large (either fact language): **+0.13 to +0.14** overall (+0.07 … +0.20); FR prompts **+0.27** (+0.15 … +0.39); EN prompts +0.15 to +0.16.
- bge-m3 embeddings with the small reranker: +0.015 (n.s.) with FR facts: the large gain comes from the **reranker**, not the embeddings.

Hook rule (useful injection rate on prompts):

| Config | FR prompts | EN prompts |
|---|---|---|
| FR facts / multi-small (today) | 37 % | 37 % |
| EN facts / multi-small | 40 % | 37 % |
| EN facts / en-small | **10 %** | 20 % |
| FR facts / multi-large | 57 % | 17 % (threshold 0.5 not calibrated for this reranker) |

Injection precision stays ≥ 0.9 everywhere except FR facts / en-small (0.78).

CPU latency (12 threads, this machine): mmarco reranker 1.7 s / 50 pairs; **bge-reranker-v2-m3 21.9 s / 50 pairs, 41 s / 100**; Hindsight reranks a median of 124 candidates per recall (live reranker log, p90 200). Query embedding: e5-small 8 ms, bge-m3 93 ms.

## Findings

1. **English-only models break prompt recall for a French speaker**: the hook searches with the raw prompt; English rerankers score French queries low, so useful injections on French prompts fall from 37 % to 10 %. Translating the prompt would need an LLM in the hook (excluded by 0008). Forcing the *user* to type English is not something the product can do.
2. **Storing facts in English helps a little with the current multilingual models** (+0.05 nDCG, significant overall and on agent queries, which are mostly English already).
3. **The reranker is the big lever**: a large multilingual reranker gains +0.14 overall and +0.27 on French prompts, and makes the fact language irrelevant (0.720 vs 0.717). It needs a GPU (22 s per 50 pairs on CPU, beyond the 17 s hook budget) — a quality argument for the v2 GPU service (0035).
4. The author writes both languages (57 French, 81 English prompts in 30 days), so any single-language model is the wrong default.

## Limits

One judge model; translated facts stand in for English extraction; no graph/temporal retrieval; 100 queries; the large reranker had fewer candidates (gain is conservative); thresholds kept at 0.5 for every reranker. To re-check inside Hindsight on the fresh install (M1): backfill the same sessions once with French facts and once with English facts, replay the same queries through the recall API.
