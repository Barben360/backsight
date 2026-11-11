# E23 — M2 replay gate: backsight's prompt recall vs the reference hook (2026-10-24)

Gate (decision 0054): on real prompts, backsight's point estimate ≥ the reference on nDCG@3 and on useful injections, with the paired difference's 95 % CI lower bound > −0.05.

## Setup
- **Prompts**: every human prompt ≥ 60 chars typed on this machine in the last 30 days (2026-09-24 → 2026-10-24), excluding this session and the smoke test, deduplicated: exactly 100 (49 French, 51 English).
- **backsight**: `backsight recall-prompt --cwd <dir> <prompt>` (the hook's query: 300 chars without code fences, redacted; project + global + related scopes; reranker ≥ 0.5; foreign projects ≥ 0.6; ≤ 3 hits within 800 chars), dev bank (backfill of this machine's transcripts, English facts, Hindsight 1.1.0).
- **Reference**: the reference hook's own functions imported read-only (`search_scopes`, `prompt_recall`, first 3 hits), reference bank (live retain + the source-machine export, French facts, Hindsight 0.10.1).
- Both stateless (no seen set, no sticky muting), one prompt at a time (a first run with 3 workers overloaded both CPU rerankers, pushed backsight to its 17 s budget and was discarded).
- **Labels**: every (prompt, memory) pair of both top-10s (438 pairs) graded 0/1/2 once by `deepseek-v4-pro`, blind (opaque keys, both systems mixed and shuffled).
- Scripts: `experiments/E23-scripts/` (data local).

## Results

| | nDCG@3 | Useful injection | Injection precision | Prompts injected |
|---|---|---|---|---|
| backsight | 0.605 | 59 % | **0.79** | 66 |
| reference | 0.523 | 58 % | 0.58 | 65 |

Paired differences (95 % CI): nDCG@3 **+0.082** (−0.068 … +0.233, n = 75 with a gradable ideal); useful injection **+0.010** (−0.100 … +0.120, n = 100).

| Group | nDCG@3 diff | Useful injection | Injection precision (bs / ref) |
|---|---|---|---|
| French (49) | −0.033 (−0.264 … +0.205) | 45 % vs 53 % (−0.082, −0.224 … +0.061) | 0.77 / 0.80 |
| English (51) | +0.163 (−0.033 … +0.354) | 73 % vs 63 % (+0.098, −0.059 … +0.255) | 0.81 / 0.47 |

Latency (sequential, CPU rerankers): backsight p50 4.0 s, p95 4.5 s; reference p50 4.6 s, p95 5.4 s.

## Findings
1. **Point estimates favour backsight on both gate metrics, but non-inferiority at −0.05 is not established**: with 100 prompts the intervals span about ±0.15. Under 0054's rule the gate is **not passed** (inconclusive), not failed.
2. **Precision is the clear gain**: 79 % of backsight's injected memories are useful vs 58 % for the reference (fewer distracting injections), driven by English prompts (0.81 vs 0.47).
3. **French prompts are the weak spot** (useful injection −8 points): of the 9 French prompts where only the reference injected something useful, 5 had nothing above backsight's floors (French prompt vs English facts on the small multilingual reranker, as E19 predicted for cross-lingual scores), 3 ranked only non-useful memories, 1 had a useful memory cut by the 3-hit cap.
4. Neither system meets 0048's prompt-hook latency target (p95 ≤ 2.5 s) on this machine's CPU rerankers.

## Limits
backsight ran without its related scope (the dev install had no entity index: no scheduled `jobs tick`, and a `/tags` wildcard bug found later, backsight PR 138), while the reference used its routing — a handicap for backsight; one machine's 30 days of prompts (all there is: n is capped by the data, not chosen); different banks (product vs product, as E22); one judge; a gate rule fixed before the run and not changed after it.
