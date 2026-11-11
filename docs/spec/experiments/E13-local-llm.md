# E13 — Local LLM lane (Ollama) for extraction

Date: 2026-10-24. Hindsight 1.1.0 sandbox in host network mode (port 38888) with `HINDSIGHT_API_LLM_PROVIDER=ollama`, `LLM_BASE_URL=http://127.0.0.1:11434/v1`, `LLM_OLLAMA_NUM_CTX=16384`; Ollama on this machine's CPU (Ryzen, 24 threads; the iGPU was not used). Same two condensed my-garage sessions and French durable-facts instructions as E7/E9, `dry-run-extract`. **V** = observed.

| Model | Session 1 (2026-10-04) | Session 2 (2026-10-08) |
|---|---|---|
| DeepSeek `deepseek-v4-flash` (E7/E9 reference) | 8 precise facts, French once the language rule is in the strategy | 6 precise facts incl. the root causes |
| `qwen3:8b` | 8 facts in 136 s, **in English** despite the rule; narrative ("The assistant pushed…"), some transient (deleted a temp config) | 7 facts in 114 s, French; vaguer root causes (no `spec.namespace`/CRD detail in the main fact, no "replace bitnami by registry.k8s.io" decision) |

Reading: an 8B local model produces valid structured output (the lane works mechanically) but noticeably weaker, less durable facts and ignores the language rule half the time; extraction latency (~2 min per 30 k-char tranche on CPU) is acceptable for async retain. A 27B run was started for comparison (see below if completed).

Decision input: offer Ollama as the private lane with an explicit "lower quality, larger models recommended" caveat; the provider check (`dry-run-extract`, SPEC §11.3) should also verify the output language and warn when it drifts.

## 27B result (completed)
| Model | Session 1 | Session 2 |
|---|---|---|
| `Qwen3.8-27B` Q4_K_XL (Ollama, CPU) | 4 facts in 531 s, French, precise (commit ids, rollback condition) | 5 facts in 564 s, French, both root causes and the fix with exact identifiers |

A 27B local model reaches DeepSeek-like quality (fewer facts, all durable) but at ~9 min per 30 k-char tranche on CPU: workable for a light user with GPU offload, a growing backlog for a heavy CPU-only user. Recommendation: the local lane needs a ≥ 20B-class model and a GPU; the provider check reports the measured seconds per tranche.
