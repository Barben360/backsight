# 0071 — Less noise in memory: right project at write time, nothing headless, provenance at read time

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "I'll retain all your suggestions. Also for the cleanup. Verify what needs to be verified, add it to the specification, and work on it until implemented and everything cleaned up."), on the agent's analysis. The live-cleanup timing was set by the author on 2026-10-25 through the M5 session: "I follow your recommandations." Parts marked **gated** ship only if their E30 check passes.

## Why more tags are not the lever
Recall reads observations, and under project-only scopes (0015) an observation carries only its scope tag. A new tag either stays on raw facts, where recall filters on observations cannot see it, or joins the scope and splits the pools again (E10: 12 duplicate observations instead of 5, corrections no longer rewrite the stale one). A tag cannot be changed later (`PATCH /memories` has no tags field, E8): a wrong one can only be forgotten. So noise is cut where memories are written, and provenance is read from the source facts at recall time (E30 V4).

## Decision

### Tags and metadata (convention)
- A **tag** is for a few values that a filter, `doctor` or `stats` reads. New tag: `agent:<subagent_type>` on subagent reports (raw facts only, never in a scope).
- **Item metadata** reaches the extracted facts, but an append replaces it on every unit of the document (E30 V2). It therefore holds only what is constant for a document: today's keys plus `entrypoint` and, on subagent reports, `agent_type`. Nothing per segment (a branch, a file) goes in metadata or tags.

### C1 — Headless sessions are not retained
- A record whose transcript `entrypoint` matches `privacy.skip_entrypoints` (glob list, default `["sdk-*"]`) is handled like an opted-out record: the cursor moves past it, the sent index marks it, nothing is sent. `cli`, `claude-desktop` and IDE entrypoints are kept. Records without an entrypoint (older transcripts) are kept.
- The same rule covers backfill (same flusher) and subagent reports (the parent transcript's entrypoint). Explicit `memory_store` notes are kept: storing is a deliberate act.
- Why: E30 V1. `~/.claude-mem/notes-bot` (sdk-ts, 12 sessions) gave 366 memories, including the author's CV and hobbies. `~/.crawler/workspace` (sdk-cli, still running) gave cron run logs ("11 parallel async workers all exited with code 0").

### C2a — One document per session and project
- A session's records go to the document `conversation:<sid>` while they belong to the session's **home project** (the project of its first sent record), and to `conversation:<sid>@<project>` while they belong to another one. Each document has its own segment counter. The cursor gains `home` and `segments` (`{project: last segment}`). A cursor without `home` resyncs once.
- Resync reads every document the session's records can belong to and resumes after the furthest `last_uuid` among them. The restore reconcile (0044) uses the same position.
- **A mid-session directory outside any repository does not name a project.** A record whose cwd has no git repository, no `.backsight.toml` `project` and is not a transverse path keeps the project of the session's previous record. Only the session's first directory may name a project by its basename (a session started in `~/Documents/Welcome` is still `Welcome`).
- Why: E30 V2. An append relabels every earlier fact of the document with the new item's tags and scopes (Hindsight 1.1.0 delta retain). backsight already sends runs of different projects into one document, so the last run's project wins. `project:dist` (64) and `project:keyrings` (30) are sessions started in `~` that moved into `~/.npm-global/…/crawler/dist` and `~/.local/share/keyrings`.

### C2b — Transverse turns go to the repository they work in (gated)
- For a record whose project is a transverse alias, its **turn** (the human prompt record and the records up to the next one) goes to the repository its tool calls touch: `file_path`, `path`, `notebook_path`, and absolute paths in Bash commands, each resolved to its git main worktree (a linked worktree counts for its main checkout) and named by the usual project rule. A turn moves when at least 60 % of its path-bearing calls point to one repository. Otherwise it stays transverse.
- **Subagent reports**: the same vote over the subagent's own transcript (`<session dir>/subagents/agent-<agent_id>.jsonl`), whatever the parent's project. Otherwise the parent's project.
- Gate: E30 V3. An independent grader, blind to the rule, judges ≥ 40 sampled moved turns and ≥ 20 moved subagent reports. Each part ships if at least 90 % are about the repository they moved to.

### C3 — Explicitly related projects
- `.backsight.toml` `related = ["<project>", …]` (≤ 10). Those projects join the current project's scope call. Their hits are "near", so the foreign-project floor (`recall.global_min_score`) does not apply. The entity-index `related` scope is unchanged.
- Why: backsight's history is split (`ref-stack` 2 599 memories, `backsight` 90). The spec work was done from ref-stack sessions.

### C4 — Provenance at read time (gated)
- Every recall sends `include: {entities: null}`: the entity payload is never read (−40 % of the response, E30 V4).
- Prompt recall and `memory_search` also ask for `source_facts` and derive, per observation, the `source:` and `kind:` tags and the newest date of its source facts. The demotion is gated: an observation whose source facts are all `source:subagent` loses `recall.subagent_penalty` (0.1).
- Gate: the E30 bench. Injection precision ≥ the baseline, and nDCG@3 ≥ the baseline − 0.02. Prompt-recall p95 must stay under the 2.5 s threshold of doctor's `latency` check.

### C5 — Subagent reports get their own strategy (gated); amends 0039
- A third strategy, `subagent`, adds to the shared rules: "The source is the final report of a sub-agent. A report that only says work is in progress, being monitored or waiting, with no finding, cause, decision or delivered result, holds no fact. Keep findings with their cause, decisions with their reason, and what was delivered and where."
- The instruction set stays one English set (0039). This adds one strategy text to it.
- Gate: E30 V5. `dry-run-extract` (nothing written) of 20 real subagent reports with `document` and with `subagent`, judged blind on the E22 rubric. Precision must rise, and coverage of key items must stay ≥ the current one − 5 points.

### C6 — Transverse floor (gated)
- Prompt-recall hits from a transverse alias, when the current project is not transverse, need `recall.transverse_min_score`. The E30 bench chooses between 0.5 (off) and 0.6.

### C7 — Bulk forget
- `forget --project P [--source S] [--purge] [--plan]`, on the configured memory bank only (never `<bank>-control` or `<bank>-docs`).
  - Without `--purge`: every observation and raw fact of P (only the raw facts of S when `--source` is given) is invalidated (reversible, as `forget <id>`).
  - With `--purge`: their documents and observations are deleted, as `forget <id> --purge`, under the maintenance lock, listing the backups that still hold them.
- `--plan` lists documents and counts. The write needs `--answer confirm_project=P`: exit 10 without it, exit 2 with a wrong name (the `uninstall --delete-data` pattern).

### Doctor
Three checks live in `internal/doctor/noise.go`:
- **`noise-transverse`**: the share of transverse observations that name another known project. Warn above 20 %.
- **`noise-headless`**: sessions retained from headless entrypoints before C1. It warns only while one of their documents (`conversation:<sid>`, `conversation:<sid>@<project>`, `subagent:<sid>:<agent>`) still holds a live raw fact, so a purge or a forget clears it. The fix is `forget --project P --plan`; for a project that also holds other sessions' documents, or a transverse alias, it names each headless document with one of its facts (`forget <fact id> --purge`). Amended by the M5 review rules (2026-10-26).
- **`noise-subagent`**: the share of `source:subagent` among raw facts, shown as info.

### Retrieval regression bench
`experiments/E30-scripts` holds the bench: 136 frozen real prompts, a replay through `backsight recall-prompt` in an isolated `BACKSIGHT_HOME`, labels cached and judged blind, and a paired bootstrap against a baseline. The prompts and labels stay on the machine (private text). Every later change to recall or retain rules is judged on it before merge.

### Live cleanup (author, 2026-10-25)
- **Now**: forget `project:notes-bot` (personal data), after `backup create` and `backup verify`. Use a main build run on its own state and config directories, `--plan` first.
- **After the parity window (2026-11-08T11:34Z)**: forget `project:workspace`. Re-send, with C2a and C2b, the sessions whose facts were mislabelled (the `dist` and `keyrings` sessions) or attributed to the transverse pool. No new build goes into `~/.claude` before then (0067).
- The commands, counts and times are recorded in INDEX.

## Why
E30 (2026-10-25, live bank `claude-memory`) shows that the noise is mostly structural:
- headless sessions: 436 memories;
- one transverse pool in which ≈ 40 % of the observations are about named repositories, and it is searched with every project;
- sessions whose `cd` relabelled their earlier facts;
- subagent status reports.
Only ≈ 2–3 % of the observations are badly worded. Each fix is deterministic and local to the write or read path. The judgement-based parts (C2b, C4's demotion, C5, C6) ship only on measured gains.
