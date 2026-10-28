# Glossary

The words these docs, `backsight doctor` and `--help` use, in one or two lines each.

## Hindsight

The open-source memory server backsight installs and runs for you, in Docker: [vectorize-io/hindsight](https://github.com/vectorize-io/hindsight). It stores what your sessions taught, turns it into memories with an LLM, and searches them.

## Bank

One memory store on the Hindsight server, named by an id. Your memory is the bank `backsight` (`server.bank` in [configuration.md](configuration.md); `claude-memory` on installs before v0.1.4-m5 until `backsight bank rename` moves it). backsight keeps two more beside it: `backsight-docs` holds these docs for `backsight guide search`, and `backsight-control` holds backsight's own records (tombstones, restore epochs). Neither mixes with your memories.

## Memory

One thing the server remembers: a short fact in English, tagged with its project and dated, kept with the documents it came from. A memory is either a raw fact or an observation. `backsight read <id>` shows one.

## Fact

Short for **raw fact**: what extraction writes from one document, one durable statement with a kind (decision, correction, convention, pitfall, state, lesson) and a date. Facts you or Claude store with `backsight store` or `memory_store` are raw facts too.

## Observation

A memory the server consolidates from several raw facts on one subject, and updates as new facts arrive. Most memories that recall returns are observations.

## Document

What a session, a subagent report or a stored fact is sent as: the text the server extracts facts from, with an id such as `conversation:<session>`. Purging a memory deletes the documents it came from.

## Extraction and consolidation

**Extraction** is the server's LLM reading a document and writing its raw facts. **Consolidation** is the server merging raw facts into observations. Both run in the background, on the provider you chose at setup, and are what backsight costs you ([README](../README.md#what-it-costs)).

## Project

The unit memories are grouped by: the Git repository a session ran in (a worktree counts as its main repository), or the directory outside one. A `.backsight.toml` at a repository's root, kept untracked by git, can give it another name ([daily-use.md](daily-use.md#projects-and-the-transverse-memory)).

## Transverse pool

The project that holds what is true everywhere: your preferences, conventions and rules. Sessions started in your home directory or `~/.claude` go there; a stored fact goes there only when named (`--project transverse`, or `project: "transverse"` for Claude), and `backsight memory notes --project transverse` lists the recent ones. Recall always searches it together with the current project. Its names are `projects.transverse` in [configuration.md](configuration.md#projects).

## Related projects

Projects that a repository's untracked `.backsight.toml` lists under `related`: recall searches them together with the current project. A file git tracks is not trusted for `related` or `project`.

## Recall

Finding the memories that match a prompt or a question and bringing them into Claude's context: on each prompt (the prompt hook), at session start (the briefing), and when Claude calls `memory_search`. See [how-it-works.md](how-it-works.md).

## Briefing

The short page Claude gets at session start about the current project: open items, recent work, rules in force. The server writes it from the project's memories.

## Retain

Sending text to the server so it becomes memory: Hindsight's word, and backsight's command group (`backsight retain flush`, `backsight retain backfill`). These docs mostly say **send**.

## Settle

A session settles when it ends, or once its last turn is 30 minutes old: it is then sent. A session you resume later sends its new turns the same way.

## Backfill

Sending your past sessions, the ones Claude Code still keeps on disk, so memory starts with your history. It runs in the background within a spending cap (`backsight retain backfill`).

## Outbox

The local queue where stored facts and subagent reports wait while the server is unreachable. They are sent once it is back; nothing is lost.

## Quarantine

Sessions or notes the server refused or did not store, set aside so they do not block the others. They are kept locally with the reason, and `backsight retain retry-quarantine` tries them again once the cause is fixed. `backsight doctor` counts them.

## Tombstone

The record a purge leaves for each document it deleted: its id, session, project and last turn, never its text. It makes sure the purged turns are never sent again, from your transcripts, a backfill, a retry or another machine, and that a restore of an older backup deletes them again.

## Epoch

Short for **restore epoch**: a counter that each restore of a backup raises on the server. Every machine compares it with the one it last saw, and when it moved, catches up before sending anything: it sends again what the restored backup lacks and purges again what was purged since. `backsight jobs tick` does it.

## Shim

The small program `~/.local/bin/backsight` points to. It always starts the current version of backsight, so an update or a rollback changes one link, and Claude Code's hooks never call a stale binary.

## Hooks, plugin, memory tools, skills

How backsight plugs into Claude Code. The **plugin** `backsight` holds the **hooks** (commands Claude Code runs at session start, on each prompt, and so on), the four **memory tools** Claude calls (`memory_search`, `memory_read`, `memory_store`, `memory_credit`, served over MCP) and the **skills** (`/backsight:remember`, `/backsight:pause`, `/backsight:memory-retro`, `/backsight:memory`). See [how-it-works.md](how-it-works.md#hooks).

## Embedding model and reranker

The two small models the server runs on your CPU to search: the **embedding model** (`multilingual-e5-small`) turns text into vectors to find candidates, the **reranker** (`mmarco-mMiniLMv2-L12-H384-v1`) scores each candidate against the query. Both are downloaded from Hugging Face on the server's first start and work across languages.
