# E7 — Retro engine: `claude -p` synthesis vs a Hindsight custom retain strategy

Date: 2026-10-23. Two settled sessions of project `my-garage` (2026-10-04, 2026-10-08), condensed exactly like `memory-retro.py` (typed prompts + final assistant replies). **V** = observed.

- (a) `memory-retro.py backtest` (Haiku triage + Sonnet synthesis + memory dedup, dry-run).
- (b) Hindsight `POST /memories/dry-run-extract` (no write) with `retain_extraction_mode: custom` and instructions derived from the retro rules (durable only; types Décision/Piège/Convention/État final/Leçon; absolute dates; exact identifiers; nothing for plans/hypotheses; ≤ 8 facts). Server LLM: DeepSeek `deepseek-v4-flash`.

| | (a) claude -p | (b) Hindsight strategy |
|---|---|---|
| Session 1 | 3 facts + 3 pending items | 8 facts (2 of them were "pending" in (a): deploy not yet confirmed) |
| Session 2 | 4 facts | 6 facts (same 4 + 1 convention + 1 lesson, both correct) |
| Quality | precise, stability-aware | as precise, more detailed; weaker on "is it settled?" |
| Cost | $0.047 + $0.040 (Claude subscription or API) | ≈ 6 k tokens per tranche on the server LLM (≈ $0.001) |
| Language | French | **English for session 1**: `custom` mode replaced the bank's French instruction → must be merged into the strategy text (or `llm_output_language`) |
| Pending tracking, closure signals, explicit dedup | yes | no (Hindsight consolidation merges duplicates into observations) |

Reading: (b) gives comparable facts for ~1/40 of the cost, uses the provider the user already configured for Hindsight, and removes the background `claude -p` (anti-goal on subscription use, decision 0010). What it loses (pending items, closure signals) is the least proven part of the retro. A `kind` entity-label group with `tag: true` (as the npm plugin does for `knowledge:*`) would also tag each fact `kind:<type>` deterministically.

Decision input: retro = binary selects settled, condensed tranches and retains them under a `retro` strategy (custom instructions incl. language), tags `source:retro`; no LLM call from the binary.
