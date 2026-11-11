# E33 - Baseline before the memory-workflow batch (2026-10-27)

Recorded on the author's real install **before** the batch ships: instruction texts v6, kinds on search hits, PostToolUseFailure pitfall recall. Read-only throughout (`stats`, `search`, `doctor`, `version`, `guide`); nothing retained, stored or forgotten, no LLM call.

## Build under measurement

- backsight 0.1.5-m5 (459caba, 2026-10-27), Hindsight 1.1.0, bank `backsight`: 6083 memories, 1476 documents, 0 awaiting consolidation.
- Instruction set live: **v5** (`internal/instructions.Version` at 459caba).
- Config: `~/.config/backsight/config.toml` holds only `[server]` and `[client] retention_days`; no `related`, no `recall.families`.

## 1. Usage baseline (`backsight stats`, 7 days to 2026-10-27 18:01 UTC)

| Metric | Value |
|---|---|
| Typed prompts / machine messages | 169 / 263 |
| Recalls, injecting something | 110, 27 with hits (25 %) |
| Skipped | same_topic 15, too_short 44 |
| Recall latency | p50 1343 ms, p95 2794 ms, 0/276 scope calls failed |
| Long prompts (>= 40 chars) followed by a search | 43 of 140 (31 %) |
| Replies after a search | empty 1, exact 20, free 2, silent 16 |
| Injections | 22: followed by a search 45 % (10), credited 36 % (8), contradicted next prompt 2 |
| Briefings | 28 (18 mental-model pages, 1 empty); 17 % of 36 recent memories returned again |
| Detector | fired 19, followed by a save 53 % (10) |
| MCP tools | memory_search 155, memory_read 2, **memory_store 169** (19 sessions, about 8.9 per session) |
| Notes stored in 7 days (doctor) | 187 (9 transverse) |
| Prompt-hook text per session | p95 7527 chars |
| Attribution | 265 main turns, path rule would file 47 elsewhere (18 %); subagent reports 0 |
| Extraction | 26.36 USD over 7 days |
| Doctor noise | transverse pool: 127/278 observations name another project; subagent reports 24 % of raw facts (899/3743) |

`memory_store` (169) is about 1.1 x the typed prompts (169) and 155 searches: the store/search ratio is the number block 1 (v6, "no memory-about-memory", close-the-loop) should move. `stats --json` is in `stats.json`.

## 2. Recall quality probe

`probe.sh` runs 20 fixed queries (5 intents x cwd in backsight, ref-stack, clinic-app, `~/.claude`, spec) through `backsight search --json --limit 8` from the right cwd. Raw outputs: `raw/qNN.json`; queries: `raw/queries.txt`. Redaction was checked (grep for key shapes: none). Re-run after the batch with the same script into another dir and diff.

Classification by hand, conservative (only clear cases counted):
- **MAM**: memory-about-memory, strictly "a memory was stored/retrieved". Subagent-narration facts (a subagent read/wrote something, no content) are counted separately.
- **Stale**: an open item later facts show done.
- **Off-topic**: clearly another project or subject than the query's cwd and intent.

| Query | Intent | cwd | Hits | MAM | Narr. | Stale | Off |
|---|---|---|---|---|---|---|---|
| q01 redaction secrets pitfall | pitfall | backsight | 3 | 0 | 0 | 0 | 0 |
| q02 release owner-only approval | decision | backsight | 2 | 0 | 0 | 0 | 0 |
| q03 open items next steps | state | backsight | 0 | - | - | - | - |
| q04 commit gitmoji PR | convention | backsight | 4 | 0 | 0 | 0 | 1 |
| q05 consolidation queue slow | pitfall | backsight | 4 | 0 | 0 | 0 | 0 |
| q06 stack deployment | decision | ref-stack | 8 | 1 | 0 | 0 | 0 |
| q07 docker compose | pitfall | ref-stack | 8 | 0 | 0 | 0 | 0 |
| q08 open items pending | state | ref-stack | 1 | 0 | 0 | 0 | 0 |
| q09 naming config | convention | ref-stack | 1 | 0 | 0 | 0 | 0 |
| q10 architecture clinic-app | decision | clinic-app | 8 | 0 | 0 | 0 | 0 |
| q11 bug fix regression | pitfall | clinic-app | 8 | 0 | 0 | 0 | 8 |
| q12 open items todo | state | clinic-app | 2 | 0 | 1 | 1 | 2 |
| q13 tests lint | convention | clinic-app | 1 | 0 | 1 | 0 | 1 |
| q14 communication style | convention | ~/.claude | 1 | 0 | 0 | 0 | 0 |
| q15 global rule tests mutation | decision | ~/.claude | 2 | 0 | 0 | 0 | 0 |
| q16 hooks settings | pitfall | ~/.claude | 4 | 0 | 0 | 0 | 2 |
| q17 open items to finish | state | ~/.claude | 0 | - | - | - | - |
| q18 spec experiment E30 | decision | spec | 8 | 0 | 1 | 0 | 0 |
| q19 open items spec backsight | state | spec | 8 | 0 | 0 | 1 | 0 |
| q20 experiment judge bench | pitfall | spec | 0 | - | - | - | - |
| **Total** | | | **73** | **1** | **3** | **2** | **14** |

Reading:
- 73 hits; 3 of 20 queries empty (state/open-items phrasing on backsight and `~/.claude`, and a bench query): "open items" is the weakest intent. MAM is rare in search hits (1 strict + 3 subagent-narration = 5 % of hits) but those memories are what the hygiene list below targets.
- Off-topic 14 (19 %) is dominated by q11: an clinic-app cwd with a generic "bug fix regression" query returns 8 backsight/ref-stack pitfalls (cross-project global scope). Real signal for the global floor, not for v6.
- Stale: `9a029674` ("only remaining item is the fresh-install case") and `8c2baa6a` (lists issue 274 as new; closed not planned 2026-10-27). The close-the-loop fact `a71981a0` ("notes claiming these PRs were pending are out of date") is the pattern v6 should make routine; it sits next to the stale ones, nothing links them.
- Hits show no kind, so intent cannot be checked per hit until block 2.

## 3. Injection replay (E30 frozen prompts): skipped

`scripts/replay-families.sh` needs `<data dir>/prompts.json` (the 136 frozen prompts, not in the spec repo or on this machine's backsight tree) and runs `backsight recall-prompt` in throwaway homes with copied `secrets.toml`. `recall-prompt` is outside the read-only command list this baseline was allowed, so it was not run. The injection baseline is the live `stats` row above (22 injections of 110 recalls). The 2026-10-26 families replay (decision index, 0072) remains the reference for the 136-prompt bench: 115 injections in both variants.

## 4. Hygiene

### Memory-about-memory candidates for `backsight forget`, one by one (none forgotten)

All say a memory/note was stored or retrieved and add no fact of their own (two state outright that the decision "is not stated").

| id | project | text (abridged) |
|---|---|---|
| `1f85469b-afbf-49d2-9975-8bd38386603b` | ref-stack | 2026-10-25 the agent stored the bare-binary signing decision in memory so it remains available |
| `22263df9-bb94-4e32-b1df-012027cdaf4f` | ref-stack | 2026-10-25 a pitfall note about the "Sys" test-helper was stored in the memory bank |
| `49f374a1-1f35-46aa-bb88-ff5924176678` | ref-stack | 2026-10-25 a memory was stored recording a Hindsight epoch pitfall |
| `5e1a88ac-5e6a-48cb-87c5-687b88758851` | ref-stack | a subagent's closing note recorded a design memory about update-apply was stored; no details |
| `6fc00316-750d-4124-8fdc-feff2920a7c6` | ref-stack | the subagent stored the control-bank decision in memory as part of its final report |
| `08374fa4-da10-4a94-adcc-503eb3a003e0` | ref-stack | the subagent recorded its purge-local identity decision as a durable note; decision not stated |
| `b44e633f-d4ea-4712-a0a6-066427980573` | ref-stack | the subagent recorded its Reconcile version-comparison decision in memory; decision not stated |
| `cbaf50cc-5155-423b-8726-8600bf29cf91` | ref-stack | a subagent retrieved the `install.sh` URL from its memory instead of a fresh lookup |
| `7cea774b-1f8d-4c0e-9f7a-d926502bc289` | my-garage | a retrospective saved 9 new notes to long-term memory (includes a correction; check before forgetting) |
| `26079181-4774-4789-9c62-8943acc4c655` | backsight | decisions are kept in the memory bank "ref-stack" and retrieved with the memory_search tool |

Borderline, keep: `1e100383` (citation format convention), `3af39336` (dev profile path), `1029e454` (trip-2031 delivery state), `a006678b` (memory_store shows 3 similar memories: product behaviour). The search was by wording (20 queries, `raw/hygiene/`), so the list is a sample, not exhaustive. Stale-state candidates: `9a029674`, `8c2baa6a`.

### `related` configuration

- `~/.config/backsight/config.toml`: no `related`, no `[recall]` section.
- No `.backsight.toml` exists under `~/Workspace/github.com/Barben360/` (searched to depth 4), tracked or not. So no repository has `project` or `related` set. A tracked one would be ignored for both (trusted only for `retain = false`); it must be untracked.

Recommendation (nothing changed): untracked `.backsight.toml` in each repo whose history is split.

```toml
# backsight/.backsight.toml
related = ["ref-stack", "spec"]
# ref-stack/.backsight.toml
related = ["backsight", "spec"]
# spec/.backsight.toml
related = ["backsight", "ref-stack"]
```

Evidence: the probe shows backsight facts filed under `ref-stack` (e.g. `ce5d9c52`, `de2ebe33`), and q06/q08/q19 pull backsight facts into ref-stack and the spec cwd through the global floor only. `related` puts them in the project scope with the normal floor. The 2026-10-26 families replay (`recall.families`) was neutral on the frozen bench, so `related` is the explicit, cheap alternative; re-measure with `probe.sh` and the `Injections` row.

## Re-run after the batch

`experiments/E33-baseline/probe.sh <new dir>`, then compare hit counts, MAM, stale and off-topic by hand; `backsight stats` over a comparable week (memory_store per session, injections credited).
