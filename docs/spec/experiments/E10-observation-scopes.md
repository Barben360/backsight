# E10-E12 — Observation scopes: duplicates, corrections, rebuilding a bank

Date: 2026-10-24. Hindsight 1.1.0 sandbox (copy of the live DB, port 18888), server LLM DeepSeek. **V** = observed.

## E10 — same session written by session retain (`source:chat`) and by the retro (`source:retro`)
| Scopes on both writes | Observations | Content |
|---|---|---|
| `combined` (today's setting) | **12** | near-duplicate pairs (the kubectl image change ×2, the OutOfSync cause ×2, the blocked CronJob ×2): chat and retro facts have different tag sets (`source:*`, `kind:*`), so they consolidate into different pools |
| `[["project:e10b"]]` (project only) | **5** | one observation per topic, no duplicate; observation tags = `project:e10b` only (`kind`/`source` stay on the raw facts) |

## E11 — a correction contradicting an existing observation
- `combined`: the stale observation ("écoute sur 7777", tags `source:chat`) was **not** updated; the correction (`source:note`, `kind:correction`) created separate observations → stale and corrected observations coexist in recall. **V**
- Project-only scope: the single observation was **rewritten** by consolidation: "Le service e11b écoute sur le port 7778 depuis le 2026-10-24. Il écoutait auparavant sur le port 7777…" **V** — corrections need no invalidation, only a fact in the same project pool.

## Tag globs (1.1.0 code, **V**)
`default_observation_scopes` and explicit scopes accept globs (`project:*`, `fnmatchcase`, consolidator.py l.509-525): the bank default `[["project:*"]]` scopes every write by its own project tag. Scopes are stamped on each memory at retain time ("changing the default later never re-scopes memories already retained", fact_extraction.py l.3770-3781).

## E12 — rebuilding an existing bank under project-only scopes
Steps: bank `default_observation_scopes = [["project:*"]]` → `UPDATE memory_units SET observation_scopes = '[["project:*"]]'` on world/experience (246 rows) → `DELETE /observations` (cleared 136; it also reset `consolidated_at` of all facts) → `POST /consolidate`.
- **V** 533 s for 246 facts, 0 failures; 150 observations, **all** project-tagged (before: 136, of which 86 untagged).
- Recall usefulness on 30 real prompts (hook scopes, ≥ 0.5): before 15/30 prompts with a hit (mean top 0.40), after 13/30 (0.34). Before, one catch-all observation ("Raspberry Pi 5 neo …") was the top hit for 7 prompts; after, hits are more specific but another broad one recurs. Judged equivalent; sticky muting handles repetition.
- During the rebuild, observation recall is nearly empty → the hook must fall back to `world`/`experience` types while a rebuild runs.

## Consequences
- Every write uses project-only scopes; the bank default is `[["project:*"]]` (also covers writes made by other tools).
- Corrections = a `memory_store` fact in the right project; consolidation rewrites the observation. Soft invalidation stays a user command (`forget`), not an agent tool.
- Adopt includes an optional, resumable "rebuild observations" step (time ∝ facts; ~0.46 facts/s here: 246 facts in 533 s) with recall fallback during the rebuild; it also clears the untagged-observation debt.
- `kind:` filtering applies to raw facts (list endpoint with `type=world,experience`), not to observations.
