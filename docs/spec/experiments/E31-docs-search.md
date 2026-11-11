# E31 — Docs search: semantic (docs bank) vs keyword

Date: 2026-10-25. Decision 0069's gate: `backsight guide search` stays semantic by default only if it ranks the right section higher than the offline keyword search. Backsight branch `feat/b4-guide` (issue 241) with the user docs of PR 252 (merged; docs hash `64c2abb5…`, 86 sections in 11 topics). Throwaway Hindsight 1.1.0 (mock LLM, Compose project `bs-e31`), multilingual embeddings and reranker (0039), the server's reranker settings of 0057 (30 candidates, 128 tokens each). **V** = observed.

## Questions
21 questions in `E31-scripts/questions.json`, 10 in French. Each names the section(s) that answer it; the labels were written before the first run and not changed after.
- **10 verbatim**: every question about Hindsight or backsight the author typed in the transcripts under `~/.claude/projects` (read only, user prompts outside subagents), quoted alone. They are few: the author mostly directs agents rather than asks.
- **11 derived**: a question in the agent's words, each from a real request or statement of the author (transcript or decision quote, given in `from`), on topics the docs cover: keeping work out of memory, transcripts retention, a second machine, where the key goes, agent install, local-only data, importing past sessions, erasing a project, update, uninstall, reporting a bug.
- Two real questions have no section to answer them and are left out: "What does GPU acceleration support impact? Dependencies installations?" and "On garde la mémoire en français, et on vérifie que le texte français est bien géré par Hindsight." (the docs say nothing about GPU, nor that facts are written in English, 0039).

## Method
`E31-scripts/run.sh <backsight checkout> [docs dir]`: builds the binary, starts the stack, runs `guide index`, then each question with `guide search --mode semantic|keyword --limit 10 --json`; `score.py` takes the rank of the first expected section in the top 10 (missing = reciprocal rank 0), and reports MRR, hit@1, hit@3, the paired wins and a paired bootstrap 95 % CI on the MRR difference (10 000 resamples, seed 31).

## Results (**V**, `results-2026-10-25.jsonl`)
| | Semantic MRR | hit@1 | hit@3 | Keyword MRR | hit@1 | hit@3 |
|---|---|---|---|---|---|---|
| all (21) | **0.568** | **0.43** | **0.62** | 0.298 | 0.14 | 0.43 |
| French (10) | **0.508** | **0.40** | **0.60** | 0.093 | 0.00 | 0.20 |
| English (11) | **0.623** | **0.45** | 0.64 | 0.485 | 0.27 | 0.64 |
| verbatim (10) | **0.360** | **0.20** | 0.40 | 0.306 | 0.10 | **0.50** |
| derived (11) | **0.758** | **0.64** | **0.82** | 0.292 | 0.18 | 0.36 |

- Per question: semantic better on 12, tied on 5, worse on 4. MRR difference **+0.270, CI +0.086 … +0.454**.
- Indexing the 86 sections and the 42 searches logged **0 LLM calls** in the docs bank (`llm-requests/stats?call_type=llm`); only the local embedding and reranker ran.
- The keyword search finds no French question's section first (it matches English words only: flags, command names).
- The verbatim questions are hard for both: long, conversational, and some ask about the old Hindsight setup (v01 "where are the Hindsight data backed up" ranks `privacy#where-it-goes` first, v08 asks for a proposal rather than a fact).

## Bank retrieval settings (**V**, tried on this set)
| Docs bank recall | Semantic MRR |
|---|---|
| Hindsight defaults (semantic, keyword, graph, temporal) | 0.355 and 0.379 on two runs (graph traversal changes the candidates between runs) |
| graph and temporal off | 0.512 |
| graph, temporal and keyword off (built) | 0.568, the same on repeated runs |

The reranker sees 30 candidates. Links between documentation chunks and dates mean nothing, and the keyword arm on English text pushes the right section out of the 30 for a French question (`Comment désinstaller backsight ?`: `uninstall#uninstall` was dropped by the prefilter). The settings were chosen on these 21 questions, so the 0.568 is optimistic; with the defaults semantic still led keyword (0.355-0.379 vs 0.298).

## Decision for 0069
Semantic search stays the default (it ranks the right section higher, CI above 0); the keyword search stays the offline fallback. The docs bank keeps the control bank's settings (chunks, no observations) plus graph, temporal and keyword retrieval off.
