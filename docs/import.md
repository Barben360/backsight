# Importing memory files

`backsight import <folder>` sends a folder of notes into the memory: another tool's memory files (LifeOS, a notes vault, an export), or your own notes. Each file becomes one document, and the server extracts memories from it the same way it does from a session. Claude then recalls them like any other memory.

Setup offers it too: answer yes to "Import memory files from another tool or a notes folder (Markdown and text files), in the background?" and give the folder. On the subscription lane (provider `claude-code`), that yes also accepts sending the folder on your subscription. Setup shows the file count and the cost before you agree.

## Preview first

```
backsight import ~/notes --plan
```

The plan changes nothing. It shows, for each top-level folder, the files, characters, the number of facts to expect and the cost, and the files it skips with the reason. Read it before you import: a folder of logs or exports can cost more than you expect.

## What is read

- Files ending in `.md`, `.markdown` and `.txt`, in the folder and every subfolder.
- `--ext jsonl` (repeatable) adds one more extension for this run, for a feed you choose. A JSONL file is sent as text.
- Skipped and counted: files over 1 MB, files that are not text, empty files, hidden files and folders (the name starts with a dot), links.
- `--exclude <pattern>` (repeatable) skips paths that match, relative to the folder: `*` stays inside one folder, `**` crosses any depth. Example: `--exclude 'archive/**' --exclude '**/_index.md'`.
- A file's date is its header's `date:` or `created:` field when it has one, else the time it was last modified. Memories keep that date, so an old note reads as old.
- Secrets in the files (keys, tokens, passwords) are replaced with a placeholder before anything is sent, as for sessions.
- The memories go to the transverse project (the memory every project shares). `--project <name>` puts them in one project instead.

## Import

```
backsight import ~/notes
```

This only queues the folder. `~` is your home folder, in any shell. `jobs tick` sends up to 10 files each time it runs (every 15 minutes), after any past sessions still waiting to be sent. Nothing blocks your sessions in the meantime.

On the subscription lane (provider `claude-code`), add `--allow-subscription`: importing sends many files without you, which your plan's terms restrict.

## Cost and its cap

Each import gets a spending cap: 1.5 times the plan's estimate by default, or `--max-cost X` (in the price's currency) or `--max-tokens N`. When the import reaches it, it pauses and sends nothing more; the files already sent may still cost a little while the server extracts them. To go on, run the same command with a higher cap:

```
backsight import ~/notes --max-cost 2
```

Each run starts a new spending window: the cap counts what this run sends, and `import list` keeps the total.

## Follow it

- `backsight import list` shows each imported folder: its id, the files sent, the files no longer in the folder, and whether it is sending, waiting for past sessions, paused or done, with what it spent.
- `backsight stats bank` has a "Files imported" line, and the dashboard's Bank page a "Files imported" card: the files sent over the window, and the imports started, finished, paused at the cap and removed.

## Run it again

Running the same command on the same folder later sends only the new and changed files. The `--exclude` and `--ext` you gave the first time are reused when you give none. A file you deleted from the folder stays in the memory: `import list` counts it as no longer in the folder.

## Undo an import

```
backsight import list
backsight import purge <id> --plan
backsight import purge <id> --yes
```

`purge` deletes for good the documents that folder sent and every memory extracted from them, then forgets the folder: importing it again sends it anew. The id comes from `import list` (its first 4 characters are enough).

## Recipe: LifeOS

LifeOS (https://github.com/danielmiessler/LifeOS) keeps its memory in Markdown under `~/.claude/LIFEOS/MEMORY` and `~/.claude/LIFEOS/USER`. Import those two folders, not all of `~/.claude/LIFEOS`: the rest is LifeOS itself (its documentation and tools).

```
backsight import ~/.claude/LIFEOS/MEMORY --exclude '**/_index.md' --exclude 'KNOWLEDGE/_archive/**' --exclude 'KNOWLEDGE/_harvest-queue/**' --exclude 'SCRATCHPAD/**' --plan
backsight import ~/.claude/LIFEOS/USER --plan
```

Drop `--plan` once the plans look right. The exclusions leave out generated indexes, archives, the queue of notes not yet sorted, and scratch work.

Two LifeOS feeds are JSONL files, which the default reading skips: your corrections (`USER/ERRATA`) and the algorithm's reflections (`MEMORY/LEARNING/REFLECTIONS`). To add them:

```
backsight import ~/.claude/LIFEOS/USER/ERRATA --ext jsonl --plan
backsight import ~/.claude/LIFEOS/MEMORY/LEARNING/REFLECTIONS --ext jsonl --plan
```

A folder inside one already imported is its own import, and no file is sent twice.

LifeOS's `WORK/` notes overlap your recent sessions, which backfill may also send. The server merges the duplicate memories when it builds its summaries.

## When it stops

See [troubleshooting](troubleshooting.md#an-import-is-paused-or-not-moving).
