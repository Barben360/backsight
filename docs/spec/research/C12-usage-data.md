# C12 — What the current hooks are actually used for (usage data)

Source: `python3 ~/.claude/hooks/memory-search.py stats 30` and `~/.hindsight/multibank-events.jsonl` on the author's Linux machine, 2026-10-07 → 2026-10-23 (VERIFIED, run 2026-10-23). The new hook (`memory-search.py`) was installed on 2026-10-23; earlier events come from `hindsight-multibank.py`, which logs the same schema.

## Volume (this machine)

| Metric | Value |
|---|---|
| Human prompts | 91 (+3 machine messages) over 16 days; light use except 2026-10-10/11 and 2026-10-23 |
| Prompt hook decisions | recall 64, skip 30 (`same_topic` 16, `too_short` 14) |
| Recalls with at least one hit ≥ 0.5 | 16 / 61 (26%) |
| Recall latency (GPU reranker, 2-3 scopes in parallel) | p50 1.9 s, p95 4.2 s |
| Bank recalls failed | 0 / 124 |
| Injections followed by a `memory_search` call | 7 / 16 (44%) |
| Injections credited in the reply (exact format) | 3 / 16 (19%) |
| `memory_search` MCP calls | 24; CLI searches 2 |
| AskUserQuestion gate | 1 (allowed, search already done) |
| Replies after a search that cite the memory | 24% (exact 4, free 0, empty 0, silent 13) |

## Memory content (DB, 2026-10-23)

| Tag | world | experience | observation |
|---|---|---|---|
| `project:.claude` (transverse) | 70 | 48 | — |
| `project:ref-stack` | 57 | 36 | 57 |
| `project:my-garage` | 30 | 33 | — |
| `project:workspace` | 12 | 9 | — |
| **no tag at all** | — | — | **99** |

- 99 consolidated observations carry no tag: they were consolidated while the plugin ran with `observationScopes: "shared"` (until 2026-10-23). They are only reachable through the whole-bank scope, never through a project scope. The source machine's stats point to a retag procedure (`~/.hindsight/retag-20261023b-README.md`) that is not part of the export.
- Documents ingested through the plugin's `hindsight_ingest_document` (e.g. "Décision: …") show as `[?]` (no project) in `memory_search` results: to confirm in C4 whether that tool applies `retainTags`.


## Source machine (heavy daily use) — the real evidence

Source: `memory-search.py stats 30` and `memory-retro.py report 30` run by the author on the source machine on 2026-10-23 (VERIFIED, logs in `~/Downloads/*-30d-20261023.log` on the author's Linux machine; only aggregate numbers are copied here, no memory content: that machine's memory is work content). Hardware: NVIDIA GPU reranker.

| Metric | Value |
|---|---|
| Human prompts / machine messages (harness: agent results, notifications, peer sessions) | 1297 / **599** (32% of hook traffic is not typed by the user) |
| Prompt hook decisions | 798 recalls (62%); skipped: `too_short` 279, `same_topic` 220 |
| Recalls with a hit ≥ 0.5 | 286 / 798 (36%) |
| Recall latency (GPU) | p50 1.6 s, **p95 1.9 s** |
| Bank recalls failed | 0 / 2041 |
| Memories injected | 953 over 405 evaluated injections |
| Injection credited in the reply | 38 / 405 (9%) |
| Injection followed by a `memory_search` call | 173 / 405 (**43%**) |
| AskUserQuestion gate | 25 allowed (search already done), **8 denied** (question asked before any search: 24% of questions) |
| Search rate on prompts ≥ 40 chars | 34% overall; per ISO week 28% → 32% → **41%** (rising) |
| Replies after a search that mention the memory | 35% (exact credit 78, free 17, explicit empty 16, silent 203) |
| Untagged observations | 1 (after the source's retag procedure `~/.hindsight/retag-20261023b-README.md`, not exported) |

Retro (`memory-retro.py`, first day of operation, 2026-10-23):

| Metric | Value |
|---|---|
| Runs | 5 (1 dry run), 3 with new material, 0 errors |
| Tranches triaged → durable | 14 → 12 (86%) |
| Facts written / dropped as duplicates | 28 / 0 |
| Cost | **$0.534** for the day (≈ $0.02 per fact) |
| Pending provisional items after last run | 14 |
| Facts by type | Pitfall 11, Convention 5, Final state 5, Decision 4, Lesson 3 |
| Facts by project | 6 projects incl. transverse |

Quality (author's facts read by the assistant, not copied): specific, dated, actionable (ports, log field names, merge conventions, PR/version states), mostly pitfalls — the kind of knowledge the per-turn retain does not consolidate.

## Reading (both machines)

- **Reliability is a solved problem**: 0 failures over 2165 bank recalls; p95 1.9 s on NVIDIA, 4.2 s on the Radeon iGPU.
- **Injection works as a cue, not as an answer**: injected memories are rarely credited (9%) but trigger an explicit search 43% of the time. The search habit grows week over week (28% → 41%), which is what the nudges and the CLAUDE.md section aim for.
- **The AskUserQuestion gate earns its keep**: it caught 8 premature questions out of 33 (24%).
- **Harness messages are a third of the traffic**: filtering machine-written prompts (`MACHINE_RE`) is essential, or recalls are wasted on agent results and notifications.
- **Citation discipline is weak**: 65% of replies after a search say nothing about the memory. A measurement worth keeping; the credit format may need simplification.
- **The retro is high-value and cheap at this volume**: 86% of tranches judged durable, 28 specific facts for $0.53 on one heavy day. Cost scales with use and is billed to the Claude account: needs a visible budget (decision 0005, cost visibility).

## Reading (this machine only)

- Data on this machine is thin (96 prompts): enough to confirm the mechanics work (0 failures, p95 4.2 s), not enough to rank features. The source machine (heavy daily use since 2026-10-02, `STATS_SINCE = 2026-10-07`) has the real evidence: its calibration tests (`tests/test_stats_credit.py`, regexes "calibrated on real transcripts") and constants (`MIN_GAP`, `STICKY_*`, `PROMPT_HITS`, `MIN_SCORE`) encode it.
- Only 26% of recalls return something useful, and the model credits injected memories 19% of the time: the injection channel is cheap but weak; the explicit `memory_search` tool is used more (24 calls).
- The AskUserQuestion gate fired once: no evidence for or against it here.

## Implications for hindsight-claude

- The port must keep the telemetry (local only) so features can be judged on data: per-prompt decision, recall hits/latency, injection outcome, tool calls, citation kind.
- Thresholds and gating constants should be ported as-is (they come from the heavier-use machine) and exposed as config, not re-tuned from this machine's thin data.
- Untagged observations are a migration concern (C13) and a design point: consolidation scope must always produce project-tagged observations.
- Port the gate, the machine-message filter, the topic-shift/short-prompt skips and the retro: all justified by the source data. Keep injection, but treat it as a cue (short, few memories).
- Retro needs a budget cap and a cost line in `doctor`/`stats`; its default (on/off) is a D10 decision with cost shown to the user.
- The source's retag procedure should be obtained before designing the migration of untagged observations (C13).
