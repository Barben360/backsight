# C13 — Migrating existing installs

Source: author's Linux machine (VERIFIED 2026-10-23) and the source-machine export `~/Downloads/hindsight-setup-20261023` (README "Excluded on purpose").

## What exists per install

| Item | Where | Notes |
|---|---|---|
| Memory | Postgres DB `hindsight` (one bank `coding-agent::memoire`) | 323 memory units, 11 documents, 390 entities, 5 mental models (knowledge pages) here |
| Bank config overrides | Hindsight API (`/banks/{id}/config`) | Identical on both machines (`bank-config.overrides.json`) |
| Plugin config | `~/.hindsight/coding-agent.json` (holds the API token) | |
| Client state | `~/.hindsight/multibank-state/` (per-session injection state), `multibank-events.jsonl` (telemetry), `entities-index.json` (rebuilt daily), `retro/` (cursors, pending, log), `projects.json`, optional `routes.json` | Names keep the old "multibank" prefix |
| Claude Code wiring | `~/.claude/settings.json` hook groups, `~/.claude.json` MCP servers, `~/.claude/CLAUDE.md` section, `~/.claude/hooks/*`, `~/.claude/skills/*` | |
| Stack | compose dir (`~/ref-stack` here), `.env`, `reranker.env`, host reranker venv + user unit | |
| Backups | here: `hindsight-local-backup` (03:30), `hindsight-offsite-backup` (03:45, age → private git repo), `claude-proton-backup` (04:00) | Custom timers outside the stack repo |

## Two machines, two separate memories

- Each machine has its own server and bank; nothing is shared. The source machine's memory is work-oriented (the author's day-job projects), this one personal.
- The data model is the same (single bank, `project:` tags), so a merge is technically a matter of re-retaining or copying memory units — to confirm in C3 whether Hindsight offers bank export/import or document-level copy, and how entities/links/observations behave on import.

## Known debts to clean during migration

- 99 untagged observations (C12).
- `memory_search` shows `[?]` for ingested documents (C12/C4).
- Leftover legacy names: `multibank-*` files, `hindsight-multibank` references in old backups.


## Untagged observations: the source's retag procedure (VERIFIED)

Source: `retag-20261023b-{README.md,apply.sql}` from the source machine (copied by the author to `~/Downloads` on the Linux machine, 2026-10-23; work project names omitted here).

- **Diagnosis on the source**: 309 observations without a `project:` tag (tags `{}`, `observation_scopes` NULL), created 2026-10-02 → 2026-10-23, 207 of them in a burst on 2026-10-23. All 309 have `source_memory_ids`, all sources tagged: 294 single-project, 15 two-project. Baseline outside the burst: ~1-3 % of new observations per day. Suspected cause: the consolidator does not always copy the sources' tags onto the observation; the burst is unexplained.
- **Rule** (one SQL transaction, backup of the old tags first): R1 sources agree on one project → that tag (293); R2 sources disagree → transverse `project:.claude` (15); R3 no usable source → transverse (0). Other tags kept. Rollback = restore the saved tag arrays.
- **Prevention proposed there, not done**: periodic re-tag with the same SQL; test other `observationScopes`; report the defect upstream.

## Same rule, dry run on this machine (read-only SELECT, 2026-10-23)

| Rule | Count |
|---|---|
| R1 → `project:.claude` | 56 |
| R1 → `project:my-garage` | 30 |
| R1 → `project:workspace` | 9 |
| R1 → `project:ref-stack` | 2 |
| R2 mixed → transverse | 2 |
| R3 none | 0 |

All 99 are resolvable deterministically. Timeline: observations consolidated after this machine switched from `observationScopes: shared` to `combined` (2026-10-23 ~18:39 UTC) are tagged (54 tagged vs 4 untagged, the 4 within the first 4 minutes — likely in-flight work). So most of this machine's debt comes from the old `shared` setting, while the source shows a residual 1-3 %/day even under `combined`.

## Implications (retag)

- The retag rule is a natural **deterministic periodic job** of the binary (dry-run, backup of old tags, idempotent), plus a `doctor` check counting untagged observations — a safety net until upstream is fixed.
- Worth checking whether Hindsight ≥ 1.0 fixes tag propagation on consolidation (C3 lists 55 releases in 2026), and suggesting an upstream report to the author (decision 0002: suggest, never auto-file).
- It writes directly into Hindsight's database: acceptable only behind a schema-version check, since migrations run at each container start (C3).

## Implications for hindsight-claude

- An `adopt`/`import` path is needed: detect an existing reference install (compose stack, plugin, Python hooks), take over its DB in place (no data copy), convert the client state, rewire Claude Code, retire the old pieces with backups — the same steps as today's manual alignment, made deterministic.
- Whether the two machines should end up sharing one memory is a product decision for the author (work vs personal content), not a technical default: to raise with D3.
- Migration must be idempotent and dry-runnable like any other migration (decision 0005).
