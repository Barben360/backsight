# 0060 — The session briefing is the project's mental model

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto); supersedes 0059
- **Decided by**: agent, from the second E14 judgement after the M3 block 3 review

## Decision
- **Briefing**: SessionStart injects the project's mental model, the server-written page of `instructions.BriefingQuery` (Open items, Recent and In force, English, at most 1 200 characters asked). It is cut at a line past 1 600 characters, under a header that dates it and frames it as data, not instructions.
- **The page**:
  - id `backsight-brief-<slug>-<hash>`, tags `project:<name>`, `max_tokens` 600;
  - trigger `budget: low`, `mode: full`, `refresh_after_consolidation: true`. Hindsight refreshes it only when a consolidation touched the project's memories and the page is stale.
- **First session of a project**: the hook creates the page and injects the E8 briefing (5 most recent observations) that time. Any failure also falls back to E8.
- **Pruning**: `jobs tick` deletes the pages of projects with no memory in 14 days. It deletes every backsight page when `recall.briefing = "recent"`, which turns the feature off. Other mental models are never touched.
- **Ordering**: the E8 briefing and `projects` order memories by event time (`mentioned_at`), not `updated_at`. After a backfill, `updated_at` is the consolidation day.

## Why
- 0059 rested on a biased judge.
  - E8 and the reference were both ordered by `updated_at`, so E8 was a subset of the reference, and the reference missed each project's newest facts.
  - The model's true newer statements were therefore counted as unsupported.
- **The second judgement**: E8 rebuilt by event time, and a reference of each project's 300 most recent observations by event time, with dates.
  - The mental model wins on 5 of 5 projects, in both orders.
  - It has 0–2 unsupported or outdated statements per page, against 0–2 for E8.
  - n = 5 is small (sign test p ≈ 0.06), but the direction is consistent.
- **Cost**: about 96 000 input tokens per refresh (≈ $0.014 with deepseek-v4-flash), only for projects that start sessions and get new memories.
