# 0074 — Memory feedback: the agent credits what helped and retires what is wrong

- **Date**: 2026-10-28
- **Status**: accepted; revised the same day after two independent reviews (design and agent behaviour; feasibility in the code), before any plan
- **Decided by**: author (2026-10-28: "if it can print it can call", crediting by a tool call whose result says what to print; the agent may retire a wrong memory itself; ledger local first; softened curve with α=3, β=9, confident misses and a 90-day half-life; curve parameters configurable; the 2D curve in the README; earlier versions' history feeds the ledger; everything logged for later audit; crediting must never miss what was used, hence a declaration on every turn that showed memories, enforced at Stop; "I'm ok with your specification"; interleaved with M6), on the agent's proposal; every point marked *agent's call* is open to veto

## Intent
Today nothing flows back from a session to the memory. The Stop hook spots the 🧠 citation with a regex (`creditRE`), the `outcome` event logs it, and only `stats` reads it. The credit is per reply, not per memory: three injected memories and one cited means three "credited" ids. A memory the agent finds wrong is answered by a new correction fact, while the wrong fact keeps its full weight in recall until consolidation happens to merge the two.

This decision closes the loop with three deterministic signals per memory (shown, credited, wrong or outdated), an append-only journal, a ledger derived from it, a bounded ranking boost, and agent-requested retirement.

### Goals
- Every credit names the exact memory, recorded by the binary, not guessed from reply text.
- Every turn that showed memories ends with an explicit declaration: what was used, or nothing.
- A memory the agent proves wrong or outdated leaves recall in the same turn, reversibly, on every PC.
- Memories that keep helping rank a little higher; memories that keep being ignored when they were the top match rank a little lower, and recover with time.
- Everything is journaled: any ranking can be explained, any parameter change replayed on real history.

### Non-goals
- Learning query-to-memory relevance: that stays the reranker's job. The ledger is per memory, not per query.
- Sharing the ledger between PCs now (author: local first). Retirements live on the server, so they are shared anyway.
- Deleting anything without the user: retirement is Hindsight's reversible invalidation; purging stays user-only.

### Anti-goals (failure even if it "works")
- An ignored-but-correct memory sinking out of reach because it was shown for off-topic prompts.
- A runaway or misled agent retiring memories in bulk.
- A credit for an id the agent never saw counting; a credit the agent made being lost.
- A hook missing its deadline, or blocking, because of the ledger or the journal.

## What Hindsight 1.1.0 allows (checked 2026-10-28 against the live `/openapi.json`)
- No feedback, usage or boost field or endpoint; recall cannot weight an id.
- `PATCH /memories/{id}`: `text` (re-embeds, rebuilds its observations) or `state=invalidated|valid` with a `reason`. Invalidation takes the fact out of recall and consolidation, **prunes its links and derived observations** (their other sources are consolidated again), and keeps it in the archive; `valid` reverts it, its observations coming back with the next consolidation. World and experience facts only; observations are derived.
- Observations carry `source_memory_ids`; `GET /memories/{id}/history` gives an observation's changes.
- Per-memory tags and metadata cannot change after the write (E8); a document's tags apply to every fact in it and changing them re-runs consolidation. **Tags cannot carry counters**; the ledger lives in backsight and acts by re-ranking on the client.
- `proof_count` counts how often extraction saw a fact, not how often it helped.

## Decision

### Measured starting point (telemetry, 2026-10-24 to 2026-10-28, author's machine)
203 typed prompts: 88 (43 %) showed memories, 75 of them through the agent's own `memory_search`, 32 through prompt recall. Of 429 finished replies, 43 cited memory (34 in the exact format, 9 freely) and 385 said nothing about it. Silence under the old instructions is therefore very weak evidence of non-use.

### Signals
1. **Shown**: every memory id put in front of the agent, with its path and rank (below).
2. **Credited**: the agent's `memory_credit` call with the ids that informed its answer. Each show earns at most one credit: a credit attaches to that id's latest uncredited show in the session, so credits never exceed shows.
3. **Wrong or outdated**: the same tool with that verdict, the correction and the evidence.

### Who records what (journal first)
Shows and credits are journal facts, never inferred from session state alone: the session-state write is a try-lock that skips when busy (§6.1), so it cannot be the record.
| Event | Writer | Rank |
|---|---|---|
| prompt-recall shows | UserPromptSubmit hook, before it returns | position in the injected block |
| gate shows | PreToolUse `AskUserQuestion` hook (its lines gain ids) | position in the reason |
| `recent` briefing shows | SessionStart hook | none (never a miss) |
| `memory_search` and `memory_read` shows | PostToolUse hook, from the response: `memory_search` prints each hit's merged rank (`#1`, `#2`…, the order **before** hits are regrouped by project) | merged rank; none for `read` |
| credits and verdicts | PostToolUse hook on `memory_credit` (it has `session_id`, `prompt_id`, `agent_id`, the tool input and response) | — |
- Only hooks write shows and credits: only they receive the turn (`prompt_id`) and the `agent_id` (block 1 plan). The MCP server resolves its session **at each call** from the by-pid record (0056); a call that finds no current record returns "memory session not ready, call again". It validates `memory_credit` against the journal and journals nothing itself; from block 3 it adds a `recall` audit event (boost, rank without boost, set changed).
- **Turns**: UserPromptSubmit writes a turn pointer (`feedback/turns/<session>.json`: the hook's `prompt_id`, or a random id when absent, the prompt number, a machine-message flag and the journal offset) and a `turn` event; every other writer stamps its events with that turn. SessionStart writes turn 0 before the briefing, so the first turn's reads include it.
- The PostToolUse regex (`memoryToolRE`) gains `memory_credit`.
- The `model` briefing is a page, not memories, and records nothing.

### `memory_credit` (fourth MCP tool; amends 0021)
| Parameter | |
|---|---|
| `ids` | 1-10 memory ids, full or their first 8 characters; empty with `none` |
| `verdict` | `used` (default), `none` (no shown memory informed the answer), `wrong`, `outdated` |
| `correction` | required for `wrong`/`outdated`: the right fact, 1-3 sentences, absolute dates (the `memory_store` rubric) |
| `evidence` | required for `wrong`/`outdated`: what in this session contradicts the memory (the user's words, a file, a command output), ≤ 300 chars |
- **Validation, all or nothing**, against the journal's shows for the session (and the session state when the journal is unreadable): each id must resolve to exactly one id shown in this session. An unknown or ambiguous id returns an error naming it; an id from a search whose result has not arrived yet (parallel calls) gets "call after the result". Nothing is recorded on error.
- **`used`** returns the line to print, built by the binary: `> 🧠 **From memory (<project> · <id8>[, <id8>…])** — <the facts you used, in your words>`. **`none`** returns "noted". The instructions no longer ask for a citation format; the tool result does.
- **`wrong` / `outdated`** stores the correction through the notes outbox like `memory_store` (kind `correction`, document metadata `replaces=<ids>`, `evidence`), then retires:
  - a **raw fact**: `PATCH state=invalidated`, reason `<verdict>: <evidence> → <correction>, ≤ 300 chars (note:<uuid>)`. Its derived observations are pruned by the server, on every PC.
  - an **observation**: the first call retires nothing and returns its source facts (`<id8>` + text each, now counted as shown) with "call again naming the source facts that are wrong, or all of them". The second call invalidates the named sources, like `forget <observation id>` already does for all of them (`curate.go`). When the server cannot list the sources, the observation is hidden locally instead (journaled `hide`), and lifted when `GET /history` shows a change after the note, or by `forget --restore`.
  - The result tells the agent to say to the user which memory it retired, why, and the undo command (`backsight forget --restore <id>`).
- **Guards** (*agent's call*, from the design review): at most 3 retirements per session, counted from the journal; only memories of the current project, its transverse aliases and its linked projects (§4), never an unrelated project's; past either limit the tool refuses and names `backsight forget` for the user.
- Supersedes 0016 and amends §2 ("the binary never deletes memory on its own") and §7 ("invalidation is not an agent tool in v1"): retirement happens only on an explicit verdict with evidence, is reversible, and is shown to the user. Purge stays a user command.
- `memory_store` stays the tool for new facts and for corrections of something not shown in the session.

### Ids where the agent can see them
Prompt-recall and gate lines gain the short id: `- [<project> · <id8>] <text> (<age>)`. `memory_search` keeps the full id on each hit. `memory_read` and `memory_credit` accept full ids or 8-character prefixes of ids shown in the session.

### Every turn that showed memories ends with a declaration (author: "it should never miss an opportunity to credit what it used")
Claude Code has no hook between the model's text and the user's screen: the reply streams as it is written. Stop, which fires once when the main agent finishes its turn, is the last point, and it can block to make the agent continue. Two layers:
1. **Declare before the final message** (instructions v7): "When memories were shown to you this turn, call `memory_credit` once you know what you used, before your final message: the ids that informed it, or `verdict: none`; then print the line it returns." The common case costs one tool call and no extra round. The prompt-recall block ends with the same one-line reminder.
2. **Stop check** (`feedback.stop_check`, default `always`): when the turn showed at least one memory (journal shows of this turn; the briefing on the first turn only) and the journal holds no `memory_credit` line for this turn, the Stop hook blocks with a reason listing those memories (`<id8>` + the first 60 characters each) and "call `memory_credit` with the ids that informed your answer, or `verdict: none`; if you credit any, add the line it returns". Local only, within the Stop budget.
   - **Once per turn**: the guard is journaled with the turn, and `stop_hook_active` (new `hookInput` field) never blocks. A turn still without a declaration is journaled `undeclared`.
   - **Never blocks**: a headless or SDK session (`parentEntrypoint`), a machine message or task notification turn, or when the journal cannot be read (fails open, journaled `check_skipped`).
   - **Interrupted turns** (no Stop) and turns cut by compaction settle as `unknown`: their shows are counted, never as misses.
   - `stop_check = "cited"` blocks only when the reply carries a 🧠 citation or the agent itself searched or read memory this turn; `"off"` disables it. On the measured week, `cited` would differ from `always` on 13 of 88 turns (injection-only turns), so `always` costs little more.
   - The same check runs at **SubagentStop** for a subagent that was shown memories; its credits carry its `agent_id` from PostToolUse.
   - Amends §9.3 "No Stop blocking in v1": the author asked for complete crediting; E34 measures what it costs, starting right after block 1.
- **Honesty flags** (*agent's call*, audit only): at Stop, the hook compares the final reply (`last_assistant_message`) with the texts of the memories shown this turn (token overlap, the texts are in the session state and the MCP server's call record), and journals `none_with_overlap` (declared none, but the reply repeats a shown memory) and `credit_without_overlap`. They never feed the boost; `stats` counts them and E34 reads them.

### Journal: every feedback event, kept for audit (author: "audit everything that happened later")
Append-only `<state>/feedback/journal-YYYY-MM.jsonl`, one line per event, written with a single `O_APPEND` write like telemetry, never rewritten, never pruned automatically; ids and numbers only, no memory text (the evidence and correction live in the note on the server). Each line: `schema` (journal schema version), `seq` (per-process counter), `ts`, `session`, `turn`, `agent`, `project`, `binary_version`, `v` (instruction set). Parameters are recorded where they act: `stop` lines carry `stop_check`, `settle` lines the values they used. Readers skip and count torn or unknown-schema lines.
| Event | Fields |
|---|---|
| `shows` | `path` (`prompt`, `search`, `gate`, `read`, `briefing`), `call` (tool call id or hook id), one item per hit: `id`, `rank`, `rank_without_boost`, `reranker`, `boost`, `source_fact_ids` (observations); plus `set_changed`, `order_changed` (did the boost change what was shown) |
| `credit` | `ids[]`, `verdict`, `ok`, `error`, `note` |
| `stop` | `outcome` (`declared`, `blocked`, `undeclared`, `check_skipped`), honesty flags |
| `settle` | per show: key, `outcome` (`credit`, `confident_miss`, `shown`, `unknown`), entry before and after |
| `retire` | `id`, `type` (`raw`, `observation`), `action` (`invalidated`, `sources_listed`, `hidden`), `note`, server answer |
| `hide`, `unhide` | `id`, `why` (`no_sources`, `rewritten`, `restored`) |
| `stale` | the ids a `memory stale` run listed |
| `import` | source key and what it produced (below) |
- **Audit commands** (read-only): `backsight feedback log [--id <id>] [--session S] [--since D] [--json]` prints the timeline of one memory or session; `backsight feedback rebuild [--plan] [--no-import]` recomputes the ledger; `backsight feedback replay [--set key=value…] [--json]` recomputes it with other parameters and reports how many memories cross zero, the biggest movers, and how many journaled recalls would change their set or order.
- Server side, Hindsight's audit log and the invalidated archive (reason included) keep the retirements; `feedback log --id` shows both.
- A journal that cannot be written never fails a hook or a recall: the event is lost, `doctor feedback-journal` errors, and the ledger stops changing until it can be written again. Doctor warns above 200 MB.

### Ledger: a cache of the journal
`<state>/feedback/ledger.json` `{schema, watermark, memories: {id: {shown, credits, credits_at, misses, misses_at, wrong, last_shown, last_credited, hidden?, imported{shown, credits}}}}`, rewritten atomically under its own lock (stale-break like the session state), never under a network lock.
- **Settle** runs in `jobs tick` only (every 15 minutes; an early trigger may come later). A show settles once it is 30 min old and its turn is over (a later turn or a `stop` line; or 2 h old), so a credit made a few turns later still attaches; a credit after that is journaled as late and counts nowhere. Credited → credit; not credited, declared turn (a `memory_credit` call of that turn and agent), rank ≤ `confident_rank`, path prompt, search or gate → confident miss; `unknown` turns and briefing or read shows never miss; every show → `shown++`. Settle keys are `(session, turn, call, id)`: a rerun or two concurrent ticks add nothing, and a resumed session's new shows settle normally.
- **The fold is deterministic**: events in `(ts, seq)` order, each decay computed from the event's own `ts`, never the wall clock. `misses = misses × ½^(Δdays / miss_half_life_days) + new`; `credits = credits × ½^(Δdays / credit_half_life_days) + new` (credits fade too, much slower, so an old favourite cannot sit at the ceiling forever). Reads apply the same decay to "now".
- The prompt hook and `memory_search` read the ledger once per call; a missing or unreadable ledger means no boost and no hiding, never an error on the hot path.
- `forget <id>` drops the entry; `forget --restore <id>` journals `unhide` and clears `wrong`. Losing the ledger costs nothing: `feedback rebuild` brings it back, hiding included.
- Observation ids can change when consolidation rewrites an observation from scratch; v1 keys by the shown id and records `source_fact_ids`, so a later rebuild can re-key credits through the sources if E34 shows the loss matters.

### Import of earlier history (author: "the stats we already have … could start feeding the system")
`backsight feedback import [--since D] [--plan] [--json]` seeds the journal from what earlier versions left; local only, no LLM, idempotent. `update apply` runs it once after installing the version that brings the ledger (*agent's call*); `--plan` shows the counts first. Narrowed after the feasibility review, because older data cannot name what it cannot see:
- **Sources**: `memory_search` results in retained transcripts (ids and printed scores; rank = descending score), telemetry `prompt.ids` (injected, in order) and `tool_use.ids`, both since 2026-10-24. Hook-injected text in transcripts has no ids before this decision and is skipped; the older Hindsight plugin's results only if a sample shows ids. Keys: `(session, transcript record uuid or prompt n, id)`.
- **What it produces**: `shown` for every imported show; a **credit** only for a turn whose reply cites memory and that showed exactly one memory; **no misses at all**: with 385 silent replies out of 429, silence before this decision says nothing reliable.
- Imported counts stay apart (`imported{}`, `import` events), so `stats`, `replay` and `rebuild --no-import` can show or drop them.

### Ranking
```
boost = k × ( (credits + α) / (credits + misses + α + β) − α / (α + β) )
```
- Added to the merged score with the current-project boost (`recall.project_boost`, 0.05). Floors on the server's reranker score (`min_score` for prompt recall, `global_min_score`) are unchanged, so the boost reorders but never admits a hit the reranker refused. Floors on the boosted score (`memory_search`'s `min_score`) see it. `Engine.Search` keeps the order without the boost for the journal.
- Hidden observations are filtered out of every recall path.
- Defaults (author): k = 0.1, α = 3, β = 9 (neutral credit rate 25 %), `confident_rank` = 2, misses half-life 90 days, credits half-life 365 days. Bounds: ceiling +0.075, floor −0.025; a memory never shown scores exactly 0. With 1 show in 3 a confident miss: never credited, 10 shows −0.006, 30 shows −0.012; shown 3×/week and never credited it levels off near −0.016; 30 misses then unseen climb back half-way in 90 days.
- Known biases, measured rather than corrected in v1: position (rank 1 is credited more than rank 2) and rich-get-richer (a boosted memory is shown and credited more). The journal keeps `rank_without_boost`, so E34 can measure both and `replay` can test a smaller k.
- Sticky muting (§10.2) stays as is: it limits repetition within 24 h; the ledger is the long-term signal.

### Configuration (A.1, all values configurable; author)
```toml
[feedback]
enabled = true              # memory_credit, journal, ledger, boost, hiding; false: the tool is not offered and recall ignores the ledger
boost_weight = 0.1          # k; 0 keeps the journal, ledger and retirements, no ranking effect
prior_credits = 3.0         # α
prior_misses = 9.0          # β
confident_rank = 2          # an uncredited show counts as a miss only at this rank or better
miss_half_life_days = 90    # 0: misses never fade
credit_half_life_days = 365 # 0: credits never fade
max_retirements = 3         # per session
stale_shows = 10            # stale report: shown at least this often and never credited
stop_check = "always"       # always | cited | off
```
`config validate` refuses negative values, α or β ≤ 0, `confident_rank` < 1, an unknown `stop_check`.

### Instructions (instruction set v7; amends 0020's citation text)
The guide, the short guide and the reminder drop "cite it exactly …" for: "When memories were shown to you this turn, call `memory_credit` once you know what you used, before your final message: the ids that informed it (it returns the line to print), or `verdict: none`. If a memory proved wrong or outdated, call it with that verdict, the correction and the evidence." The `memory_credit` description carries the rubric and one example per verdict. The `memory-retro` skill's feedback step (step 3) uses `memory_credit` for memories shown in the session and `memory_store` otherwise.

### Telemetry, stats, report, doctor (§15, A.4)
- Telemetry keeps its role (counts and timings); the journal holds the per-memory detail. New events `credit {verdict, ids_n, ok}` and `feedback_settle {shows, credits, misses, unknown}`; `outcome` gains `credit_source` (`tool`, `citation`) so windows spanning the upgrade stay comparable; existing fields keep their meaning.
- `stats`: on turns with shown memories, share declared before Stop, after a block, undeclared; honesty flags; measured confident share; credits, `none` and retirements per day; share of recalls whose set the boost changed; imported versus live counts.
- **`backsight memory stale`** (report only, §2): memories with `shown ≥ stale_shows` and no credit, hidden observations, and memories with a `wrong`/`outdated` verdict, each with its `backsight forget <id>` command. `--json` envelope; journaled `stale`.
- Doctor: `feedback-journal` (above), `feedback-ledger` (watermark behind the journal by more than a day: ticks are not settling).
- Turns live in the turn pointer, not the session state (whose writes can be skipped); ledger and journal carry `schema`, and an unknown schema is read as empty with a doctor warning, never guessed.

### README and docs
- The README gains a short "How memory learns what helps" section with the 2D chart: boost against times shown, one line per credit rate (never, 5 %, 10 %, 25 %, always; with one uncredited show in three a confident miss, 10 % is the neutral rate, where the credit share equals α/(α+β)), default parameters, before decay; one sentence each on confident misses and the half-lives.
- The chart is two SVGs (`docs/assets/feedback-curve-light.svg`, `-dark.svg`, shown like the logo with two `<img>` tags and GitHub's `#gh-light-mode-only` and `#gh-dark-mode-only` fragments, not `<picture>`), generated from one function by `go run ./scripts/feedback-curve` from the config defaults; coordinates rounded to 2 decimals so the output is the same on every architecture. A test regenerates them and fails when the committed files differ. Accessibility: each line is labelled directly and has its own dash pattern (identity never by colour alone), and line and text contrast against each background is ≥ 3:1 and ≥ 4.5:1, computed and stated in the PR.
- `docs/configuration.md` (the `[feedback]` keys), `docs/how-it-works.md` (the loop), `docs/daily-use.md` (`memory stale`, `feedback log`, undoing a retirement), `docs/agent-guide.md` (the tool). User-facing text names no decision or experiment (CONTRIBUTING).

### Later: shared ledger (author: after local; waits for M6's client side)
**Built** in block 4 (backsight v0.1.11-m6); what differs from the text below is in the INDEX row "0074 block 4, as built".
One document per machine in the control bank, `backsight:feedback:<machine_id>` (no LLM work, the tombstones pattern), replaced by `jobs tick` when the local ledger changed; readers sum all machines' entries (each decayed from its own timestamps), cached for 6 h. Raw-fact retirements already reach every PC; only the local fallback hiding and the boost wait for this.

### Blocks
1. Journal (writers, schema, torn-line tolerance), shows from every path with ranks, short ids in prompt recall and gate lines, `memory_credit` with `used` and `none`, the Stop and SubagentStop check, honesty flags, ledger with settle in `jobs tick` and the deterministic fold, instruction set v7, `credit`/`feedback_settle` events, `stats` lines, `feedback log`, `feedback rebuild`. E34's declaration part starts here.
2. `wrong`/`outdated`: evidence, correction note, raw-fact invalidation, the observation two-step through its sources, the local hiding fallback, guards, `memory stale`.
3. Import of earlier history, `feedback replay`, then the ranking boost and hiding in recall (so the boost starts with history), `[feedback]` config and validation, README chart with its generator and test, docs.
4. Later: shared ledger.

### E34 — feedback check (declaration part two weeks after block 1; the rest two weeks after block 3)
- Declaration: on turns that showed memories, share declared before Stop (target ≥ 90 %), after a block, `undeclared` (target 0). A low first share means the instruction needs work, not that the check should go.
- Cost of the Stop check: blocks per day, latency added per blocked turn (p50, p95), tokens of the continuation, and how often the user sees a citation line after the answer. If it costs more than the author accepts, `stop_check = "cited"` and compare the undeclared share.
- Honesty: `none_with_overlap` and `credit_without_overlap` rates, plus 20 `none` and 20 `used` declarations read by the author against the reply.
- Retirements: every one of the period reviewed by the author; one wrongful retirement switches `wrong`/`outdated` to hide-only until the cause is fixed.
- Ranking: share of recalls whose set the boost changed (journal `shows`), whether the changes look right on a 20-prompt sample, position and rich-get-richer effects from `rank_without_boost`, `replay` with a smaller k.
- Measured confident share against the 35 % the curve was drawn with; import weight (`replay` with and without imported counts).

## Why
- **A tool call over a parsed citation** (author): the call is a structured record with exact ids, checked against what was shown; the regex only ever knew that a reply cited something. Printing and calling depend on the same agent compliance, so the call costs one round trip and buys determinism.
- **Journal first** (author's audit request; feasibility review): the session state is best-effort by design, so a show or credit recorded only there can vanish and turn a correct credit into "unknown id" or a false Stop block. One append per event, one writer per event, and a deterministic fold make the ledger disposable and every ranking explainable.
- **Confident misses, decay and a stronger prior** (author, from the interactive curve): with α=1, β=3 and every uncredited show a miss, a memory lost 0.018 after 10 shows, the gap between neighbouring hits in a typical search (about 0.02), and never came back once shown less.
- **Retire through the sources** (author's retirement choice; design review): invalidating a raw fact already prunes the observations built from it on the server, so retiring the wrong source fixes the observation on every PC; a local hide is only the fallback. 0016's two risks are answered: ids not shown in the session are refused, and a multi-source observation is never retired wholesale without the agent naming its sources.
- **Evidence and tighter guards** (design review): a verdict must point at what contradicts the memory, which makes a misled or lazy retirement visible in the audit; 3 per session and no unrelated projects bound the damage.
- **Import without misses** (author: feed the system from earlier versions; both reviews): earlier versions could not name the memory a reply used, and 90 % of replies said nothing, so only shows and unambiguous credits are imported.
- **Stop check `always`** (author: "never miss"): both reviews suggested `cited` until measured; on the measured week the two differ on 13 of 88 show turns, so the author's default stays and E34 starts measuring with block 1.

## Rejected
- **Tags as counters**: cannot change per memory; document tags re-run consolidation and split pools (0071).
- **Parsing ids from the citation in the Stop hook**: deterministic parsing, but still a guess about intent, and no channel for `wrong`/`outdated`.
- **A hook before the reply is shown**: Claude Code has none (the text streams); Stop with one block is the closest, and the declare-before-final-message instruction keeps the block rare.
- **Blocking until the agent complies**: a loop risk with no gain over one block plus an `undeclared` record.
- **Penalising every uncredited show**: sinks correct memories shown for off-topic prompts.
- **Text-overlap attribution of old credits** (feasibility review): a guessed credit is worse than none.
- **Position-normalised credit or a band-limited boost in v1** (design review): sound ideas, not yet justified by data; the journal keeps what E34 needs to decide.
- **Hiding as the main observation path** (both reviews): local, lost with a stale rewrite check, and divergent from `forget`.
- **Reward only, no penalty from misses**: simplest, but the author asked for an unboost; kept as the fallback if E34 finds the penalty harmful (`prior_misses` high or `boost_weight` 0).
