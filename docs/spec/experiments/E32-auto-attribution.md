# E32 — Automatic project attribution, and project families for recall

Date: 2026-10-26. The author's requirement (2026-10-26): "je veux vraiment que sans action, les mémoires soient le mieux attribuées possible". An explicit focus command may exist as an option, but it is not the answer. This follows E30 V3 (C2b, path-majority attribution of transverse turns, which failed its gate: 16/19 = 84 %) and decision 0071 (C2a as built).

Read only on the live install: transcripts under `~/.claude/projects`, nothing written to `~/.claude` or backsight's state, no call to the live server. Datasets and labels stay in a private scratch directory outside any repository, because transcripts are private. Below, the projects carry the invented names of commit 20c1b66; `spec` is this repository. Scripts are in `E32-scripts/`. **V** = observed.

## Setup
- **Turns** (`build.py`). A turn is a typed prompt plus the assistant's work up to the next prompt. A real subagent transcript (`subagents/agent-*.jsonl`) counts as one turn. Headless sessions (C1) and the work account are left out.
  - **V** 361 turns: 53 in sessions launched in `~/.claude`, 23 launched in `~`, 121 main and 163 subagent turns launched in a repository.
  - E30 counted 75 transverse turns; this machine still holds about that many.
- **Signals per turn**:
  - the `cwd` of its records;
  - explicit paths in tool inputs: `file_path`/`path`/`notebook_path`, plus absolute or `~` paths in any string input, `cd X`, `git -C X` and `--cwd X` (relative ones are ignored, since the shell may have moved);
  - Bash calls with no path, which count as implicit votes for the cwd's repository;
  - project names mentioned in the prompt, with the entity index's whole-word rule;
  - the previous turn's project.
- **Path → project**: Python reimplements `Resolver.Named`. `~` and `~/.claude` map to the transverse alias. Otherwise the nearest `.backsight.toml` `project` decides, then the git main worktree's basename. A plain directory gives no evidence.
  - A clone or worktree under `/tmp` or `~/.cache` counts for the kept checkout with the same origin remote, or for nothing. Without this, throwaway review clones named `bs` or `review-b1` would have become projects (**V** 76 and 37 calls).
- **Replay artefact, corrected**: 13 098 path mentions point into worktrees removed since (`~/.cache/backsight-dev/wt-*`, the clinic-app worktrees).
  - When the turn ran, live hooks saw those directories. Offline, git no longer resolves them.
  - `build.py` rebuilds them from the transcripts' own `git worktree add` and `git clone` commands: 78 directories, 50 of backsight and 23 of clinic-app.
  - All numbers below use this "live-equivalent" replay, unless marked raw.
- **Blind grading** (`sample.py`, `grading-instructions.md`): 218 turns, picked without looking at any strategy, then shuffled. The sample holds:
  - every transverse turn that did some work (52 + 21);
  - every main turn of the three sessions that touched another repository than their launch one (85);
  - 15 control turns from single-repository sessions;
  - 45 random subagent turns.
- **Freezing**: the strategies' answers were written to a separate file before grading. Only aggregate counts were printed, and the file was not reopened until both graders had finished.
- **Graders**:
  - **Primary**: an independent agent that saw only the digests, `grading-instructions.md` and a one-line description per project.
  - **Second rater**: this session, on every third turn (72).
  - **V** Agreement: 67/72 = 93 %, Cohen's κ 0.91. Lenient agreement, where either rater's `also` counts, is 72/72. The 5 disagreements were spec vs backsight (2), my-garage vs trip-2031 (2) and ref-stack vs transverse (1).
  - The primary grader marked 45 of the 218 turns `unsure`.

## Strategies
| | Rule |
|---|---|
| S0 | The current rule (C2a), per record: a strong cwd name wins; otherwise the record keeps the previous project. |
| S1 | S0, but a turn that S0 leaves transverse takes the majority of its explicit repository evidence: ≥ 2 calls, top share ≥ 60 % (C2b-like). |
| S2 | S1. Then a prompt that names exactly one project, with no evidence against it, takes that project. Then continuity: a turn with no evidence keeps the previous turn's project, but only when that project came from evidence or a mention, never from the launch directory. A subagent continues the main turn that was running when it started. |
| S3 | S2, plus an override of a strong cwd repository. A turn's explicit evidence for another repository wins with ≥ 3 calls and ≥ 75 % of all its repository votes, implicit ones included. A mention of that repository plus one call, with no vote for the cwd repository, also wins. |
| S3c | S3 with S2's continuity in repository sessions too. |
| S5 | S3 with two changes. First, only **own** repositories count as evidence: those of a launch directory, or sharing its remote owner. A third-party clone, such as an AUR package build, an oh-my-zsh checkout or the Flutter SDK, is never a project. Second, a **writing** call weighs 3 and a read weighs 1; a writing call is Edit/Write, `git commit`/`push`/`merge`, `gh pr create`/`merge`, `sed -i` or a redirect. |
| S6 | S5, plus a session prior: a main turn that S5 leaves on its cwd, with no vote for the cwd, takes the session's earlier evidence majority (≥ 10 weighted votes, ≥ 75 %). |
| S4 | A local LLM decides only the turns that S2/S3 leave ambiguous. |

S5 and S6 were designed **after** seeing S3's wrong moves on these same labels (`score.py` lists them). Their numbers are therefore optimistic: see the verdict.

## Results (**V**, live-equivalent replay, primary grader's labels)
A "moved" turn is one that a strategy files elsewhere than S0 does.
- **Strict** precision: the move matches the label.
- **Lenient** precision: the move matches the label or the grader's `also`.
- **Leaks**: turns filed under a project their label is not, i.e. one project's text under another's tag.
- **Recall**: turns whose true project is a repository and that get it.

| Situation (graded turns) | S0 | S1 | S2 | S3 | S5 |
|---|---|---|---|---|---|
| launched in `~/.claude` (52; 27 truly a repo) | moves 0; recall 0/27 | 16 moved, 81 % / 81 % | 20 moved, 65 % / 70 % | 20, 65 % / 70 % | 15 moved, **87 % / 93 %**; recall 13/27 |
| launched in `~` (21; 7 truly a repo) | recall 0/7 | 1 moved, wrong | 1, wrong | 1, wrong | 0 moved; recall 0/7 |
| launched in a repo, main turns (100) | recall 41/100 | — | — | 22 moved, 86 % / 95 % | 24 moved, **88 % / 96 %**; recall 61/100 |
| launched in a repo, subagents (45) | recall 10/45 | — | — | 30 moved, 100 % | 30 moved, **100 %**; recall 40/45 |
| **All (218)** | accuracy 41 %; 33 % transverse; **94 leaks** | 17 moved, 76 % / 82 % | 21 moved, 62 % / 71 % | **73 moved, 85 % / 90 %**; 53 leaks | **69 moved, 93 % / 97 %**; accuracy 70 %; 27 % transverse; **46 leaks** |

Other rows:
- **S3c**: 75 moved, 84 % / 89 %. Continuity in repository sessions adds 2 moves, 1 of them wrong.
- **S6**: 70 moved, 93 % / 97 %. The session prior moves 1 more turn, because the sessions split their evidence between backsight and spec, so neither gets a 75 % majority.
- **On turns the grader was sure of**: S3 88 % (n = 66), S5 97 % (n = 62).
- **Raw replay**, without the removed worktrees rebuilt: S3 moves 65 at 75 % / 82 %. Its subagent precision falls from 100 % to 77 %, because a subagent's edits in its removed worktree no longer count and its reads of spec decisions win. Live, the hook sees the worktree, so the live-equivalent row is the one that applies.

### What the errors are
- **Every S0 error in repository sessions stays inside one family** (94 / 94): work on backsight or spec filed under `ref-stack`, the launch directory.
- **S1/S2/S3's wrong moves in transverse sessions**:
  - 4 turns of an AUR package build were filed under the package's clone;
  - a workspace repair was filed under the workspace's repository;
  - trip-2031 work was filed under my-garage, because the site is deployed through my-garage. This is E30's error again.
  - The continuity rule of S2 adds wrong moves, and adds no right one.
- **S5's 5 wrong moves**:
  - 3 inside the {backsight, spec, ref-stack} family: a turn that writes a decision in spec while reading backsight code, or the reverse;
  - 2 between my-garage and trip-2031, where the turn creates or deploys the trip site through my-garage before its repository exists.
  - Only 2 of S5's 46 leaks fall outside a family.
- **What stays unattributed**:
  - turns that only discuss ("Q1: … I follow your recommendations"): 32 repository-launched main turns stay on `ref-stack` while the label is backsight or spec;
  - every turn launched in `~` (machine setup, with no repository evidence: 7 of 21 are truly a repository's).

### S4 (local LLM, ambiguous turns only)
- **Design** (`s4.py`):
  - **Input**: the project list that backsight can build alone (each known repository's name and the first prose line of its README) plus the turn's digest (≤ 1 500 characters: prompt, paths, first and last actions, start and end of the reply).
  - **Answer**: one name, or `transverse`; a name outside the list counts as `transverse`.
  - **Model**: Ollama on this machine, the only model installed (Qwen 27B Q4, CPU), temperature 0, thinking off.
  - No transcript text left the machine.
- **Ambiguous turns**: for a transverse turn, S2 fell back to continuity or found nothing. For a repository turn, it has foreign evidence below S3's threshold. **V** 127 of 361 turns, 40 %.
- **Measured** (**V**): ~850 prompt tokens and 4–7 output tokens per turn. 23–96 s per turn (median 46 s) on this machine's CPU while other jobs ran. The run was stopped after **11 of the 100** sampled ambiguous turns (4 repository main turns, 2 subagents, 3 in `~`, 2 in `~/.claude`), so **S4 is only partly measured**.
  - On those 11 turns, S4 is right on 9 (10 lenient) and files 1 under a wrong project. On the same turns, S3 and S5 are right on 3 and file 5 under a wrong project: discussion turns that they leave on the launch repository.
  - n = 11 is far too small for the gate. It only says that S4 targets the error mass that S5 leaves, the "discussion only" turns.
  - `s4.py` resumes where it stopped.
- **Cost per day at the author's volume**, measured on the transcripts:
  - **V** 24 turns per active day over the last 14 days, 107 on the busiest day (2026-10-25); 8 ambiguous on an average day, 28 on the busiest.
  - Tokens: about 7 k input per average day, 24 k on the busiest, plus ~200 output tokens.
  - With a hosted model at $1 per million input tokens, that is under $0.03 a day. Locally, it is about 20 minutes of CPU on the busiest day, in the background job, never in a hook.
  - The real cost is not money:
    - the classification needs the turn's text in a prompt, which a hosted model would receive (the author's provider already extracts every turn, so no new party);
    - the answer is not deterministic across model updates, against the author's credo "everything that can be deterministic remains deterministic".

## A.5 — The existing bank
**Affected share**: estimated from the transcripts, weighting each turn by its text and tool calls. The live server was not read; the metadata count (`conversation:<sid>` segments per project) needs a read of the live bank, which this experiment did not make.
- **`ref-stack`** (3 236 memories today): S5 moves **≈ 73 %** of its content.
  - 68 % goes to backsight and 6 % to spec, i.e. ≈ 2 200 memories to backsight and ≈ 190 to spec.
  - It comes from 2 sessions and 109 of their 126 subagent reports.
- **Transverse pool**: S5 moves **≈ 46 %** of its content: 36 % to trip-2031, 5 % to my-garage, 5 % to backsight. It comes from 3 of 19 sessions.
- **Sessions**: 28 are unchanged, 2 move whole to one project, and 3 are mixed.

**Re-tag without a re-send** (`retag.sh`, throwaway stack `bs-e32`, mock LLM, never the live server). Hindsight 1.1.0 has `PATCH /documents/{id} {"tags": [...]}`. It *replaces* the document's tags and propagates them to all its memory units; `observation_scopes` can be changed the same way.
- **V** Setup: a document with two segments tagged `project:alpha` held 20 memory units, all listed under `project:alpha`. A recall scoped to `project:alpha` returned 30 hits, and one scoped to `project:beta` returned 0.
- **V** After the PATCH to `project:beta` (`{"success": true}`), all 20 units carry `project:beta`. The alpha listing and the alpha recall return 0; the beta listing returns 20 and the beta recall 30.
- **V** A third segment appended later with `project:beta` worked. The document's **metadata is not changed** by the PATCH: `project` stayed `alpha` until the next segment rewrote it.
- **Consequences**:
  - A document that holds one project can be moved for free (no extraction): every `subagent:` report (109 documents of `ref-stack` sessions) and every C2a per-project segment document. backsight must then also rewrite its own view of the document (cursor and metadata `project`), because C2a's resync reads that metadata.
  - A legacy `conversation:<sid>` document mixes projects, and a tag set is per document. Here that is the 3 mixed sessions, including the two large `ref-stack` sessions. Splitting such a document needs a re-send: per-project documents, then a forget of the old one, at the extraction cost.
  - The memory-level `PATCH /memories/{id}` still takes no tags (E8). That remains true; the document-level PATCH is what is new here.

## Part B — Project families
Today a session in backsight reaches spec and `ref-stack` memories only through the global scope (stricter floor) or a prompt mention.

**Co-touch** (`families.py`, from the same turns, no bank read). A turn touches a project when at least one of its calls names it, explicitly or implicitly. **V**:

| Pair | Turns touching both | Sessions | Share of the smaller | Same remote owner | Same name prefix |
|---|---|---|---|---|---|
| backsight – spec | 123 | 4 | 0.82 | yes | no |
| backsight – ref-stack | 62 | 2 | 0.87 | yes | no |
| spec – ref-stack | 55 | 2 | 0.77 | yes | no |
| clinic-app – clinic-app-site | 22 | 3 | 0.92 | yes | yes |
| my-garage – trip-2031 | 4 | 1 | 0.33 | yes | no |
| every other pair | ≤ 2 | 1–2 | — | — | — |

- **Remote owner** is useless as a signal: 16 of the author's repositories share one owner, so it would link everything.
- **Name prefix** finds clinic-app's pair and trip-2031's three repositories (site, content, style). It misses backsight, whose name shares nothing with its spec.
- **Shared distinctive entities** were not computed: that needs the entity index, which would mean reading backsight's state or the live bank, both out of bounds here.

**Proposed rule** (automatic, no user action):
- Two projects become **linked** when, over the last 60 days, they are co-touched in **≥ 5 turns of ≥ 2 sessions**, and the co-touched turns are **≥ 20 % of the *larger* project's turns**.
- Linked projects are searched in the `project` scope, as C3's `related` already does: they count as near and get no boost.
- Links are pairwise, never transitive, and capped at C3's 10. They come from counters that retain keeps (repositories each turn's evidence names), so no transcript has to be re-read.
- On this machine the rule builds exactly two families: **{backsight, spec, ref-stack}** and **{clinic-app, clinic-app-site}**. It leaves my-garage – trip-2031 apart (4 turns, one session).
- To the author's question: yes. Working in backsight would then search spec and `ref-stack` in the project scope.

**Failure modes**:
- **A hub repository** (dotfiles, a shared CI repository, the transverse config): measuring against the larger project's turns keeps it from linking to every small project that touches it. Measuring against the smaller one, as in the table, would link it to everything.
- **A family too wide pulls noise.** `ref-stack` holds the retired stack's facts (old hooks, an old server), which would then compete with backsight's own facts in every backsight prompt. That is why links get no boost and stay under the reranker floor of the project scope.
- **Deployment coupling** (my-garage deploys trip-2031) would link two projects whose facts rarely help each other. N = 5 / 2 sessions excludes it today, but more deploy sessions would cross the threshold.
- **Asymmetry**: spec work often needs backsight facts, but a clinic-app session hardly ever needs its site's. A symmetric link is simpler, and the replay below should decide whether that matters.
- **Staleness**: a link computed from a past phase (the cut-over months) stays after the work stops. Over a 60-day window it decays by itself.

**What a replay must validate**, before building:
- frozen prompts from sessions of each family, plus prompts from outside them, replayed through `recall-prompt` in three variants: (a) today, (b) the family in the project scope, (c) the family in the related scope;
- metrics: nDCG@3 and useful injections, graded blind as in E30; out-of-family injections; the share of hits that come from a linked project; and no regression on out-of-family prompts;
- the bank state: run it after the re-attribution, or with tags remapped at query time, since today's `ref-stack` tag hides most backsight facts.

## Verdict
- **Attribution**:
  - **None of the specified strategies passes** the 0071 gate (≥ 90 % strict precision on ≥ 40 moved turns). S3 is the best: 85 % strict (90 % lenient) on 73 moved turns, and it halves the leaks (94 → 53).
  - **S5 reaches 93 % strict (97 % lenient) on 69 moved turns**, with 2 leaks outside a family instead of 8. Its two changes are principled: never count a third-party clone as a project, and weigh writes over reads. But they were chosen after seeing S3's errors on these labels.
  - **Recommendation**: build S5 only after a confirmation on fresh turns: a new blind sample of ≥ 40 moved turns from sessions after this one, graded with `grading-instructions.md`. Ship it with the family rule, since its remaining errors fall inside a family.
  - S2's continuity and S6's session prior bring nothing, so leave both out. S4 is not needed for the precision of moves. Its value is the "discussion only" turns, which only a content reader can attribute: 9 of 11 right on a first, too small sample. Measure it fully on the same fresh sample before deciding.
- **Implementation note**: S5 decides per turn, after the turn's tool calls. backsight's Stop-time flush already sees a whole turn, and SubagentStop sees a whole subagent transcript. The added state is the turn's vote counters, not a re-read.
- **Bank**: about three quarters of `ref-stack`'s memories and half of the transverse pool belong elsewhere.
  - Single-project documents can be moved with `PATCH /documents/{id}`, at no extraction cost.
  - Mixed legacy documents need a re-send.

## Scripts (`E32-scripts/`)
Run in order, with `E32_DIR` set to a private directory (and `E32_SKIP_PROJECT` to leave out a work account):
1. `build.py`: turns and signals;
2. `sample.py`: the sample and the blind grading file;
3. `strategies.py`: frozen answers (`E32_FROZEN` names the file);
4. grading, per `grading-instructions.md`;
5. `score.py` (`E32_FAMILIES="a,b,c;d,e"` adds the family-level rows);
6. `s4.py`: the local LLM;
7. `families.py`;
8. `retag.sh <backsight checkout>`: the throwaway re-tag check.

## Confirmation (2026-10-26, blind)
The 143 turns not used to design S5 (`confirm.py`; strategies run unchanged on all 361 turns, then filtered), graded blind by a new independent grader who read only the digests, `projects.md` and the instructions (`score.py --confirm`).
- **V** S5 moves 80 turns, 76 right: **95 % strict** (96 % on the turns the grader was sure of) → passes 0071's gate (≥ 90 % on ≥ 40 moved). S3: 81 moved, 95 %.
- The 4 wrong moves: 3 inside the backsight / spec / old-stack family (spec instead of backsight), 1 content-free turn (`/compact`) sent to a project.
- **Limit**: 79 of the 80 moves are subagent turns; no repository main turn moved in this set (it holds 21 and 3 launched in `~` or `~/.claude`). So this confirms S5 **on subagents**; on main turns S5 rests on the design set only (24 moves at 88 % in repo sessions, 15 at 87 % in `~/.claude`, both below 90 % strict). Main turns need fresh sessions recorded after 2026-10-26 (a since-date in `build.py`), or a shadow mode that logs S5's answer without applying it.
