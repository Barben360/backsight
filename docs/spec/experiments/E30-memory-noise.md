# E30 — Where the noise in memory comes from, and checks for decision 0071

Date: 2026-10-25. Live bank `claude-memory` (Hindsight 1.1.0, 127.0.0.1:18888), read-only unless stated otherwise. Scratch bank `e30-scratch` for writes, deleted afterwards. **V** = observed. Scripts in `E30-scripts/`. Data stays local (`~/.local/share/backsight-dev/e30`), because prompts and memories are private.

## The bank
**V** 4 354 memories (observation 1 651, experience 1 774, world 929) and 1 067 documents.

| Tag (`GET /tags`) | Count |
|---|---|
| `project:ref-stack` | 2 599 |
| `project:.claude` (transverse) | 554 |
| `project:notes-bot` | 366 |
| `project:clinic-app` | 315 |
| `project:my-garage` | 150 |
| `project:backsight` | 90 |
| `project:workspace` | 70 |
| `project:dist` | 64 |
| `project:keyrings` | 30 |
| `project:Welcome` | 13 |
| `source:chat` / `source:subagent` / `source:note` | 1 658 / 689 / 356 |
| `kind:state` / `decision` / `convention` / `pitfall` / `lesson` / `correction` | 1 240 / 650 / 456 / 402 / 189 / 88 |

Wording noise in observation text (regex over all 1 658 observations) is small:

| Pattern | Share |
|---|---|
| transient states ("in progress", "was monitoring"…) | 0.8 % |
| hypotheses | 0.8 % |
| agent narration | 0.9 % |
| run logs | 0.5 % |

## V1 — Headless sessions
**V** The first `entrypoint` of every transcript file under `~/.claude/projects`:

| Entrypoint | Files | Where |
|---|---|---|
| `cli` | 151 (123 of them subagents) | interactive |
| `claude-desktop` | 2 | interactive |
| `sdk-ts` | 12 | all in `~/.claude-mem/notes-bot`, the last on 2026-10-04 |
| `sdk-cli` | 3 | `~/.crawler/workspace`, still running on 2026-10-25 |

Content:
- `project:notes-bot` holds the author's CV, self-rated skills and hobbies.
- `project:workspace` holds news-digest run logs.

## V2 — Append relabels a document; metadata reaches facts
Scratch bank, observations off, two synchronous retains into one document `conversation:e30`:
1. Segment 1: tags `project:alpha, source:chat`, metadata `{segment: 1, entrypoint: cli}`.
2. Then an append: tags `project:beta`, metadata `{segment: 2}`.

**V** After step 1, the alpha fact carries `project:alpha` and the item's metadata. After the append it carries `['source:chat', 'project:beta']` and `{segment: 2}`.

This matches the Hindsight code (`engine/retain/orchestrator.py`, delta retain): "the facts on UNCHANGED chunks … still carry the previous retain's tags … syncs them", via `update_memory_units_metadata_and_tags`.

Consequences:
- Item metadata is usable as provenance, but it is document-level: the last segment's metadata wins.
- A document must never mix projects. backsight's runs send different projects into `conversation:<sid>`.
- **V** In the live bank: `project:dist` = session `ea93e616` (161 records in `~`, 60 in `~/.npm-global/lib/node_modules/crawler/dist`, 9 in `~/.crawler/workspace/skills`). `project:keyrings` = session `4d2b8f35` (41 records in `~`, 8 in `~/.local/share/keyrings`). Both are interactive (`cli`). Their facts from `~` were relabelled.

## V3 — Transverse pool and path-majority attribution
**V** Of the 230 `project:.claude` observations, mentions by name:

| Name | Observations |
|---|---|
| trip-2031 | 30 |
| ironwood | 22 |
| backsight | 21 |
| ref-stack | 9 |
| garage / my-garage | 7 |
| clinic-app | 5 |
| crawler | 5 |

Together, ≈ 40 %. The transverse pool is in every project's `project` scope.

Example: the subagent fact "The work is in two unmerged commits on branch content/v1 in example/trip-2031" carries `project:.claude`, because the parent session ran from `~/.claude`.

The offline re-attribution and the blind grading of moved turns are recorded below (results).

## V4 — Recall payload and provenance
One recall (observations, `project:ref-stack`, budget low, 1 500 tokens), three runs each:

| `include` | Bytes | Latency |
|---|---|---|
| default (entities on) | 23 176 | 0.57–0.60 s |
| `entities: null` | 13 801 | 0.52–0.58 s |
| `entities: null, source_facts: {max_tokens_per_observation: 200}` | 42 143 | 0.55–0.56 s |

**V** `source_facts` maps each observation's `source_fact_ids` to facts with their tags (`kind:*`, `source:*`) and dates, in the same call. Observations mix kinds: 4–6 different `kind:` values on the top 6 hits.

## V6 — Internal helper agents
**V** On 2026-10-25 the bank received more than 1 000 `subagent:` documents in one day:
- text length p10 / p50 / p90 = 29 / 38 / 48 characters; 0–1 memory unit each;
- 929 of them from one session (41a3ab69), 33 from this one;
- example: "Reviewing checkMention in docs_commands_test.go".

None of these agent ids has a `subagents/agent-<id>.jsonl` transcript (or a `.meta.json`); the real subagents of the same session all have both. They are Claude Code's own helper agents (tool-call summaries for the UI and Remote Control). They fire SubagentStop, and each document costs one extraction.

Most of the 689 `source:subagent` raw facts come from them, e.g. "The assistant was monitoring CI checks on PR 238".

## Results

### V3 — C2b, path-majority attribution of transverse turns: not adopted
The rule is applied offline (`E30-scripts` is not needed: a one-off script over the 19 interactive transverse sessions, i.e. those started in `~` or `~/.claude`, and their subagents).
- **V** Paths: most of the path-bearing tool calls in these sessions point outside any repository (`/tmp`, `~/.config`, …). With every path counted, a 60 % majority moves 3 of 75 turns.
- With only paths inside a repository counted, and at least 2 such votes, it moves 19 of 75 turns: trip-2031 7, ref-stack 3, backup-tool 2, llm-server 2, backsight 2, my-garage 2, workspace 1. Only 2 subagent transcripts of transverse sessions exist; 1 would move.
- **Blind grading** by an independent agent (it saw no rule, no share, no label):
  - the 19 moved turns plus 15 controls (turns whose evidence stayed under the threshold, or with no evidence and a random candidate repository), shuffled;
  - moved turns: 16 yes, 2 partly, 1 no. Strict precision 84 %, 95 % counting "partly";
  - controls: 4 of 15 yes, so the grader discriminates;
  - the errors put trip-2031 work under my-garage, whose deploy files the turns touched.
- **Verdict**:
  - the gate (≥ 90 % on ≥ 40 turns) is not met: 84 % strict, n = 19, all the data this machine holds;
  - the gain is small (a quarter of the transverse turns), and the cost is turn tracking across incremental flushes;
  - C2b is not built. Most of the transverse pool's foreign content came from subagent reports and helper agents (V6), which C1 and the real-subagent check remove at the source.
  - It can be re-measured when more transverse sessions exist.

### V5 — C5, a `subagent` strategy: adopted
- **Reports**: 20 real subagent reports, a seeded sample of the 70 that have a final text (`subagent_collect.py`).
- **Extraction**: dry-run on scratch bank `e30-scratch`, which holds the live bank's strategies, labels and missions, plus `subagent` (`subagent_extract.py`). Nothing was written.
- **Judge**: deepseek-v4-pro, blind, A/B order seeded, E22 rubric (`subagent_judge.py`). 49 key items in all.

| Strategy | Facts | Coverage | Precision | Wrong or transient |
|---|---|---|---|---|
| `document` (today) | 66 | 42/49 = 86 % | 61/66 = 92 % | 5 |
| `subagent` | 55 | 45/49 = 92 % | 54/55 = 98 % | 1 |

Gate met: precision up, coverage not lower.

### Bench — C4 (provenance penalty), C6 (transverse floor) and entities off
- **Setup**:
  - 136 frozen real prompts (66 French, 70 English), every prompt of ≥ 60 characters on this machine, headless sessions left out (`extract.py`);
  - replayed through `backsight recall-prompt`, one at a time, each variant in its own `BACKSIGHT_HOME` (`replay.py`);
  - 266 (prompt, memory) pairs graded once, blind, by deepseek-v4-pro (`judge.py`).
- **Drift**: the live bank changed during the runs (two sessions writing, the notes-bot purge at 21:52Z), so each variant is also compared with a baseline replayed at the same time.
  - The same configuration replayed at two times differs by up to ±0.01 nDCG@3 (penalty runs +0.011 / −0.007).
  - The current build and the entities-off build replayed back to back differ by −0.003.

All rows compare with the entities-off build replayed at the same time; nDCG@3 with its 95 % CI, n = 72 gradable prompts.

| Variant | nDCG@3 | Useful injection | Injection precision |
|---|---|---|---|
| current build (entities on) | −0.003 (−0.010 … 0.000) | 0 | 0.90 vs 0.91 |
| `subagent_penalty = 0.1` | +0.011 (−0.015 … +0.038), rerun −0.007 (−0.016 … 0.000) | 0 | 0.91 vs 0.91 |
| `transverse_min_score = 0.6` | −0.006 (−0.039 … +0.025) | −0.007 | 0.89 |
| both | −0.016 (−0.034 … −0.003) | 0 | 0.90 |

Latency (`recall-prompt` wall time, including process start, on a machine also running CI): p50 1.6–2.1 s, p95 2.7–4.1 s for every variant. The spread between runs exceeds the spread between variants.

**Verdict**:
- **Entities off**: adopted. Same results, 40 % less payload.
- **Penalty**: not adopted. It passes the non-inferiority gate but shows no gain beyond noise, and it adds a request option and a ranking rule.
- **Transverse floor**: stays off (0.5), since 0.6 lowers nDCG@3 and useful injections.

The transverse pollution is handled where memories are written instead (C1, real subagents only, C5).
