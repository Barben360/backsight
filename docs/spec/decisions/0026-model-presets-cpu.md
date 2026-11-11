# 0026 — CPU embedding/reranker presets in v1; GPU detected and reported

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Presets: multilingual (multilingual-e5-small + mmarco-mMiniLMv2-L12) and English (bge-small-en-v1.5 + ms-marco-MiniLM-L-6). The setup reports GPUs; an acceleration service is decided by the author (SPEC §20 Q3).

## Why
The author deprioritised performance (2026-10-23); C08 shows a GPU service is feasible later through the `cohere` reranker provider with no Hindsight change.
