# 0081 — Memory import: a folder of text files into the memory

- **Date**: 2026-11-02
- **Status**: accepted
- **Decided by**: author (2026-11-02: "allow the user import a path that would be entirely read and imported (with a dry-run with an estimation of how many memories there are and how much it will cost)"; "during agentic setup, the agent would ask the user if they want to import some memory files and ask from where"; a decision record plus a backsight issue; rulings 1–7 below confirmed with "I confirm. Let's go"; started now, ahead of 0079 block 7), on the agent's proposal; every point marked *agent's call* is open to veto

## Intent
Someone who comes to backsight from another memory tool, or with a notes folder, starts with an empty memory: today only Claude Code transcripts (§9.1, backfill 0037) and explicit notes (§9.2) reach it. This decision adds one more write path: a folder read recursively, each text file sent as a document, after a dry-run that shows how many files, how much content, roughly how many facts, what it costs and how long it takes.

The first target is LifeOS (https://github.com/danielmiessler/LifeOS, checked 2026-11-02): its memory is a tree of Markdown folders split by purpose (WORK, KNOWLEDGE with People, Companies, Ideas and Research, LEARNING, RELATIONSHIP, OBSERVABILITY, STATE). Its exact layout changes between releases, so backsight reads it as any folder; no adapter is written for it.

### Goals
- `backsight import <path> --plan` answers, before anything is sent: which files, which are skipped and why, characters and tokens per top-level folder, approximate facts, cost and duration.
- The import runs in the background under a spending cap, resumes after an interruption, and never sends a file twice when it is run again on an unchanged folder.
- An import can be undone as a whole.
- Setup offers it, and an agent driving setup (§11.2) can ask for the path and show the plan.

### Non-goals
- Adapters per tool (LifeOS, Obsidian, Notion export…): the generic reader plus `--exclude` and the per-folder breakdown cover them. One may come later if a format carries structure worth mapping (types, links).
- Formats other than plain text: PDF, HTML, JSON, databases.
- Keeping memory in sync with the source: re-running the import is the sync; nothing watches the folder.
- Deleting a memory because its source file disappeared.

### Anti-goals (failure even if it "works")
- A large or noisy folder (logs, state files, drafts) crowding out session memories in recall.
- A spend the user did not see in the plan, or one that exceeds the cap.
- An import that cannot be told apart from session memories, or cannot be removed without touching them.
- Notes dated "today" in bulk, so that decay treats years-old content as fresh.

## Decision

### Command (§17)
`backsight import <path> [--plan] [--exclude glob]… [--ext E]… [--project P] [--max-cost N | --max-tokens N] [--json]`, plus `import list [--json]` and `import purge <source> [--plan]`.
- `--plan` writes nothing and sends nothing (the project rule for anything touching the server).
- Without `--plan` the command records the import as queued, prints the plan it accepted, and returns; `jobs tick` sends it (below). It refuses under the maintenance lock, like backfill.

### What is read (rulings 1, author)
- Files ending in `.md`, `.markdown` or `.txt` (case-insensitive), recursively under `<path>`.
- `--ext jsonl` (repeatable) adds an extension for this run, for a feed someone chose; JSONL is sent as text, one document per file, under the same 1 MB cap. *Agent's call*, after the LifeOS check below: its two durable JSONL feeds would otherwise be unreachable, while its other JSONL files are telemetry, so the default stays text-only.
- Skipped, each counted with its reason in the plan: larger than 1 MB; not valid UTF-8 or containing a NUL byte; empty after the frontmatter; matched by an `--exclude` glob (relative to `<path>`, `**` allowed).
- Hidden files and folders (name starting with `.`, e.g. `.git`, `.obsidian`) are skipped. Symbolic links are not followed. *Agent's call.*
- Every file goes through the same redaction as sessions (§13); the plan reports how many redactions it made, never their text.

### LifeOS coverage (checked 2026-11-02 against `LIFEOS/DOCUMENTATION/Memory/MemorySystem.md` on the repository's `main`)
LifeOS keeps its memory under `~/.claude/LIFEOS/MEMORY/` and `~/.claude/LIFEOS/USER/`.
| What | Where | Format | Read by default |
|---|---|---|---|
| Hot-layer memory (48 × 256 chars) | `USER/PRINCIPAL/PRINCIPAL_MEMORY.md`, `USER/DIGITAL_ASSISTANT/DA_MEMORY.md` | Markdown | yes |
| Knowledge (People, Companies, Ideas, Blogs, Research), ideas | `MEMORY/KNOWLEDGE/**`, `MEMORY/IDEAS/**` | Markdown, kb-v3 frontmatter (`created` gives the date) | yes |
| Learnings, wisdom | `MEMORY/LEARNING/{SYSTEM,ALGORITHM,SYNTHESIS}/YYYY-MM/`, `MEMORY/WISDOM/**` | Markdown | yes |
| Work units with their decisions | `MEMORY/WORK/<ts>_<slug>/ISA.md` | Markdown | yes |
| Research, plans, reference, project notes, registries | `MEMORY/{RESEARCH,PLANS,REFERENCE,PROJECT}/**`, `USER/PROJECTS.md`, `USER/CONTACTS.md` | Markdown | yes |
| Errata (the user's corrections and ratings) | `USER/ERRATA/errata-YYYY-MM.jsonl` | JSONL | no: `--ext jsonl` |
| Algorithm reflections | `MEMORY/LEARNING/REFLECTIONS/algorithm-reflections.jsonl` | JSONL | no: `--ext jsonl` |
| Telemetry, runtime state, logs, raw transcripts | `MEMORY/{OBSERVABILITY,STATE,STATVS,VOICE,SECURITY,SKILLS,LEDGER,EVIDENCE}/`, `LEARNING/FAILURES/**/transcript.jsonl` | JSONL, JSON, SQLite | no (not text by extension) |
| Generated or unvetted Markdown | `**/_index.md` (generated MOCs), `KNOWLEDGE/_archive/` (expired seedlings), `KNOWLEDGE/_harvest-queue/` (regex candidates), `SCRATCHPAD/` | Markdown | yes, unless excluded |

So the default reading covers every durable LifeOS memory except errata and reflections. The rest of `~/.claude/LIFEOS/` is LifeOS itself (its documentation in Markdown, its tools, a built dashboard with `.txt` files), so the docs page's recipe imports the two memory roots, not `LIFEOS/`:
- `backsight import ~/.claude/LIFEOS/MEMORY --exclude '**/_index.md' --exclude 'KNOWLEDGE/_archive/**' --exclude 'KNOWLEDGE/_harvest-queue/**' --exclude 'SCRATCHPAD/**' --plan`
- `backsight import ~/.claude/LIFEOS/USER --plan`
- optionally `backsight import ~/.claude/LIFEOS/USER/ERRATA --ext jsonl --plan` and the same for `MEMORY/LEARNING/REFLECTIONS`. (The second root's plan already lists `ERRATA/` as skipped by extension, and a later root inside an earlier one is its own source.) `WORK/` overlaps the last 30 days of transcripts (LifeOS's own retention) that backfill also sends; consolidation merges the duplicates, and the plan says so when both are pending.

### Plan
Per top-level folder of `<path>` and in total: files read, files skipped by reason, characters, input and output tokens, approximate facts, cost, duration. One file is one document, so each is estimated on its own with the backfill model (`EstimateSession`, 0051; facts ≈ characters / 600) and the estimates are summed. The fact count is labelled approximate: extraction decides the real number. Prices come from `[llm]` or the built-in table (0051); with no known price, `--max-tokens` is required, as for backfill. The per-folder breakdown is what lets someone see that a log folder is most of the cost and exclude it.

### Documents
- **Source**: one import root is one source, id `sha256(machine_id + "\n" + absolute root)`, first 12 hex characters (*agent's call*: the same path on two PCs is two sources).
- **Document id**: `import:<source>/<relative path>` with `/` separators on every OS; a purged id follows the tombstone successor rule of `LiveDocumentID` (`#2`, `#3`…).
- **Retain item**: the file's content (frontmatter kept, it carries context such as a type or a name), context `"file imported from <relative path>"`, strategy `import` (below), tags `project:<P>` and `source:import`, observation scope `project:<P>`, metadata `{source, path, sha256, machine_id, binary_version}`.
- **Date** (ruling 4, author): the frontmatter's `date`, else `created`, else the file's modification time, as the item's timestamp.
- **Project** (ruling 3, author): the transverse project by default (the first `transverse` alias of the config), since notes from tools like LifeOS are personal rather than tied to a repository; `--project P` sends the whole import to P.

### Strategy `import` (amends §11.3 step 5 and 0071's strategies; *agent's call*)
`bank apply` adds a fourth retain strategy, `import`, with the shared rules and: "The source is a file imported from another memory or notes tool. Keep durable statements (decisions, conventions, pitfalls, facts about people, projects and tools) with their reason. A log line, a status, a to-do or a draft without a conclusion holds no fact." The `document` strategy is not reused: it keeps every durable statement of a note someone chose to store, which is too generous for a whole folder.

### Sending, pacing and cap (ruling 2, author)
- `jobs tick` sends queued imports after the backfill step, through the same pacing and the same spending cap mechanism as backfill (0047, 0051): `--max-cost` defaults to 1.5 × the plan's estimate, spending is measured from the server's LLM usage, a reached cap pauses the import (`paused: budget`) until the user raises it. While a backfill is pending, the import waits behind it so that one cap is spent at a time.
- On the subscription lane (`claude-code` provider) the import is refused unless the user acknowledges it, as for backfill.
- `operation_id` = UUIDv5(bank, document, sha256 of content), so a repeated send collapses (E21).

### Re-import (ruling 5, author)
A local manifest per source (`<state>/import/<source>.json`: root, project, files with document id and sha256, sent state) drives a run on the same root: an unchanged file is skipped; a changed one is sent again with the same document id, replacing its document; a new one is added. A file that disappeared stays in memory: only `import purge` removes anything. A root inside, or containing, another source's root skips the files that source already holds, and the plan lists them (*agent's call*: no file is sent under two sources). If the manifest is lost, the server's documents under `import:<source>/` and their `sha256` metadata rebuild it (*agent's call*).

### Undo
`import purge <source>` deletes the source's documents through the existing purge path (`PurgeWrites`, tombstones in `backsight:tombstones`), so restores and other PCs honour it, then removes the manifest. `--plan` lists the documents it would delete. `import list` shows each source: root, project, files, sent, paused or done, and its spend.

### Setup (ruling 6, author; amends §11.3)
A new step after **Backfill**: question `import`, "Import memory files from another tool or a notes folder?", default and recommendation `no`. On `yes`, a follow-up `import_path` asks for the folder; setup runs the plan and shows its numbers as the option's cost, then asks for consent. An agent driving setup (§11.2) asks the user for the path and relays the plan before answering. The step only queues the import; `jobs tick` sends it.
*(2026-11-02, as built in block 2: the question reads "…a notes folder (Markdown and text files), in the background?", and on `claude-code` its yes carries the subscription caveat. The folder answer is kept absolute (`~` is the home folder). Only this run's answer is checked, so a saved folder deleted since does not stop a later setup. The step leaves alone a folder this PC already imported, and an import that cannot be queued is reported as skipped with the command to run: it never fails setup.)*

### Telemetry, stats, dashboard (§15, A.3, A.4)
- A.3: `import/<source>.json` (manifest above).
- A.4: an `import` event (`action`: plan, queue, send, pause, done, purge; `source`; files, chars, estimated and spent tokens and cost; skipped counts by reason). No paths or text.
- `stats` and the dashboard count imported documents apart from sessions, by the `source:import` tag and the `import:` id prefix. 0079 block 6's retain-operations view shows them as their own kind.
*(2026-11-02, as built in block 2, author's choice "stats + small card": `stats bank` has a metric `imports.sent` ("Files imported"), the files carried by this PC's `import` send events over the window, with the events by action. The dashboard's Bank page has a matching card. The telemetry index keeps an import event's `action` and `files`, and the events carry the import's project. Counting server documents by tag, and the block 6 view, are not built.)*

### Docs
README (a short "Coming from another tool" section), `docs/` page for `import`, `troubleshooting.md` for a paused import.

### Blocks
1. **Command**: reader, plan, manifest, sending through `jobs tick` under the cap, re-import, `list`, `purge`, the `import` strategy, telemetry. Tests on fixture folders: skip reasons, dates, re-import of unchanged, changed, new and deleted files, a purge, the cap pausing.
2. **Setup and close**: the `import` and `import_path` questions with the agent contract, docs and README, `stats` counts, then a live proof (a small real folder imported on the author's machine, recalled, then purged with no session document touched), review and release.
*(2026-11-02: the author dropped the live proof; the tests run against a fake server.)*

Ordered before 0079 block 7 (ruling 7, author): both touch `jobs tick`, and this one merges first.

## Why
- One generic reader covers every tool that stores memory as text files; adapters would couple backsight to formats it does not control.
- Every part except the reader and the strategy already exists for sessions: redaction, estimates, prices, the cap, pacing, purge and tombstones. Import reuses them, so its cost is mostly the reader, the manifest and setup.
- The quality risk (noise crowding recall) cannot be measured before release on the author's machine, who has no notes folder; the stricter `import` strategy, the per-folder plan, the `source:import` tag and a one-command purge make a bad import visible and reversible instead.

## Rejected
- **A LifeOS adapter first**: its layout changes between releases, and the generic reader with `--exclude` already handles it.
- **Reusing the `document` strategy**: too generous for a whole folder (keeps every durable statement).
- **A separate spending cap for imports**: two caps spent at once would hide the total.
- **Deleting memories when a source file disappears**: a synced folder or a moved file would silently erase memory.
- **An agent reading the folder and calling `memory_store` per fact**: no pacing, cap, re-import or batch purge, and it spends the session's context.
