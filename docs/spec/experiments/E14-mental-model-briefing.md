# E14 — A per-project mental model as the SessionStart briefing

Date: 2026-10-24. Hindsight 1.1.0 sandbox (copy of the live DB, rebuilt under project-only scopes, E12). **V** = observed.

`POST /mental-models {id: "brief-my-garage", source_query: "Current state of this project: what was delivered recently, decisions in force, known pitfalls and anything left unfinished or to verify. Most recent first, with dates.", tags: ["project:my-garage"], max_tokens: 600}` → refreshed asynchronously by the server LLM (DeepSeek); `GET` returns cached `content` (no LLM at read time).

Result (**V**): a structured page — "Open items / to verify (latest first)", "Delivered recently" (commits, changes), "Decisions in force (with literal values)" — markedly more useful than E8's "5 most recent observations" (it surfaced an unanswered user question and validated checks). Problems: written in **English** (no language rule in the query), **3 762 chars** despite `max_tokens: 600`, and it says it covers **only one session** (2026-10-11) although the project has facts from several.

Consequences: promising replacement for the E8 briefing, read with one cached GET at hook time; needs (1) the language rule in the source query, (2) truncation by section at the hook (open items, then decisions), (3) a check of coverage across sessions, (4) refresh only for projects active in the last 14 days (cost = one reflect-like call per refresh). Kept as an experiment-gated M3 item; v1 ships the E8 briefing.

## M3 rerun (2026-10-25)
Dev install (Hindsight 1.1.0, 0057's reranker settings), 5 projects (ref-stack, clinic-app, my-garage, .claude, workspace), scripts in `E14-scripts/`. **V** = observed.

- **Mental model**:
  - `source_query` asks for at most 1 200 characters under the headings Open items / Recent / In force, with absolute dates, covering every recent session and only what the memories support;
  - `max_tokens` 600, trigger `budget: low`, `mode: full`;
  - refreshed once per project, then deleted.
- **E8 briefing**: as the hook builds it (5 most recent observations, ≤ 220 characters each, ≤ 1 200 in all).
- **Judge**: `deepseek-v4-pro`, blind pairwise, both orders, with the project's 40 most recent observations as the reference. It penalises unsupported or outdated statements.

| Project | Winner (both orders) | Unsupported statements, E8 / model |
|---|---|---|
| ref-stack | E8 | 0 / 12 |
| clinic-app | E8 | 0 / 9 |
| my-garage | model | 0 / 5–6 |
| .claude | E8 | 0 / 17–18 |
| workspace | model | 0 / 1–2 |

- **Cost**: a refresh takes about 25 s and about 96 000 input tokens (≈ $0.014 with deepseek-v4-flash). Lengths are 1 078–2 023 characters, still over the asked 1 200 in 3 of 5.
- **What the model does well**: when its synthesis holds, it is the better page (my-garage, workspace). It surfaces open items and rules that 5 recent facts miss.
- **What it does badly**: it states open items and recent work that the newest memories contradict or do not support. For example, "M2 blocks 2–4 not started" when they were done. Synthesis over all memories mixes old and new states. A briefing that misleads at session start costs more than one that is incomplete.
- **Bias**: the reference holds only the 40 most recent memories, so true statements drawn from older memories count as unsupported. The judge still preferred E8 even with that in mind.

**Conclusion**: the mental model does not replace the E8 briefing (decision 0059). It could come back when Hindsight can bound a refresh to recent memories, or as an on-demand page (`/backsight:memory`) rather than an injection.

### Second judgement (after the M3 block 3 review)
The first judgement was biased by construction, so its "E8 wins 3 of 5" is withdrawn.
- **The bias**: E8 and the reference were both listed by `time_field=updated_at`. On this backfilled bank that is the consolidation day (2026-10-24), not when things happened. E8 was therefore a subset of the reference ("0 unsupported" guaranteed), and the reference missed each project's newest facts (clinic-app's stopped at 2026-09-21 while facts up to 2026-10-04 exist).
- **The rerun** (`judge2.py`):
  - E8 rebuilt by event time (`mentioned_at`), as the fixed hook now does;
  - reference: each project's 300 most recent observations by event time, each with its date;
  - the judge also counts outdated statements;
  - same mental models (same bank state).

| Project | Winner (both orders) | Unsupported E8 / model | Outdated E8 / model |
|---|---|---|---|
| ref-stack | model | 0 / 0 | 0–2 / 2 |
| clinic-app | model | 0 / 0 | 0 / 0 |
| my-garage | model | 0 / 0 | 0 / 0 |
| .claude | model | 0 / 1 | 0 / 0 |
| workspace | model | 0 / 1–2 | 0 / 0 |

**Conclusion (replaces the one above)**: the mental model is the better briefing on every project measured (n = 5, sign test p ≈ 0.06). It becomes the briefing (decision 0060, superseding 0059).
