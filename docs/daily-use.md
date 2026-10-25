# Daily use

What backsight does during your Claude Code sessions, and the commands you use day to day.

## What happens in a session

You use Claude Code as usual. backsight works through hooks and memory tools:

- **At session start**, Claude gets a short guide on using the memory, and a briefing for the current project: a page the server keeps from the project's memories (open items, recent work, rules in force).
- **On each prompt**, backsight searches the memory with your prompt and adds the few most relevant memories to Claude's context, framed as notes to verify.
- **Claude searches and stores** with three memory tools: `memory_search`, `memory_read` and `memory_store`. It is told to search before factual questions, recommendations and new tasks, and to store durable decisions, corrections, conventions and pitfalls.
- **When the session ends**, or once its last turn is 30 minutes old, its text is redacted and sent to your server, which extracts memories from it in the background.

Hooks never block Claude Code: if the server is down, recall is skipped and writes wait in a local queue. When memory is unavailable, you see one message in the session.

## Recognizing a memory

When Claude uses a memory, it cites it on its own line:

> 🧠 **From Hindsight memory (my-project)** — the API client retries 3 times, decided on 2026-10-12 because the gateway drops idle connections.

If Claude searched and found nothing, it says so. Memories are hints from past sessions: Claude is told to check them against the code before relying on them.

## Projects and the transverse memory

Each memory belongs to a project: the Git repository you worked in (a worktree counts as its main repository), or the directory outside one. Recall searches the current project together with the transverse project, which holds rules valid everywhere (your preferences, your conventions), and also the whole memory and related projects, with a boost for the current project.

To give a repository another project name, or to keep it out of memory, commit a `.backsight.toml` at its root:

```toml
project = "my-project"
# retain = false   # never record sessions in this repository
```

## Skills

The plugin adds four skills you can call in Claude Code:

| Skill | Use |
|---|---|
| `/backsight:remember <fact>` | Store a fact now, for example a rule from now on. Claude picks the kind and the project. |
| `/backsight:pause` | Stop recording this session; `/backsight:pause project` stops recording this project. |
| `/backsight:memory-retro` | At the end of a project: Claude writes its decisions, pitfalls and lessons into memory. |
| `/backsight:memory` | Command-line access to the memory when the memory tools are missing or failing. |

Claude also calls them on its own when you say "remember", "from now on" or "pause memory".

## Searching from the terminal

```sh
backsight search "deploy docker compose" "backup restore"
```

Each argument is one short keyword query (at most 4). It searches like recall (the current and transverse projects, related projects and the whole memory); `--all` searches the whole memory without project scopes, `--project <name>` adds a project. `backsight read <id>` shows one memory with the facts it was built from.

`backsight ask "<question>"` has the server's LLM answer a question over many memories. It costs tokens, shown with the answer.

`backsight projects` lists the projects in memory with their counts and last activity.

## Storing a fact yourself

```sh
backsight store --kind decision "The billing service keeps PostgreSQL 16 for partial indexes (2026-10-24)."
```

Kinds: `decision`, `correction`, `convention`, `pitfall`, `state`, `lesson`. One fact per call, with the why and absolute dates. `--project <name>` stores it in another project; a transverse alias stores a user-wide rule. Text that looks like a secret is refused.

## Forgetting a memory

```sh
backsight forget <id>
```

This retires the memory: recall stops returning it. `backsight forget <id> --restore` brings it back.

For a leaked secret, `backsight forget <id> --purge --yes` deletes the memory, its source facts and the session documents they came from. Backups made before still hold it; the command lists them. `backsight memory scan` lists stored memories that look like they hold a secret, with the purge command for each.

## Pausing memory

| Command | Effect |
|---|---|
| `backsight pause` | Nothing typed in this session from now on is sent to memory. Inside Claude Code, use `/backsight:pause`. |
| `backsight resume` | The session is recorded again. |
| `backsight pause --project` | Adds this project's directory to `privacy.exclude_paths` in `config.toml`: no session in it is recorded. |
| `backsight resume --project` | Removes it. |

See [privacy.md](privacy.md) for the other opt-outs.

## Importing past sessions

Setup offers to import the sessions Claude Code still keeps (backfill). You can also run it later:

```sh
backsight retain backfill --plan
backsight retain backfill --since 2026-01-01 --max-cost 2
```

`--plan` shows the sessions, their size, the expected cost and duration. Backfill sends the oldest first, in the background, and stops at the spending cap (`--max-cost`, default 1.5 times the estimate). On the Claude subscription it is refused unless you add `--allow-subscription`. `backsight doctor` shows its progress.

## Seeing what memory does

- `backsight why` shows the last memories the hooks injected, with their scores and the rule that let them through.
- `backsight stats` shows the last 7 days: recalls, searches, citations, extraction cost (`backsight stats 30` for 30 days).
- `backsight doctor` checks the whole install.

## Browsing these docs

`backsight guide` lists these documents and `backsight guide <topic>` shows one in the terminal. `backsight guide search <question>` finds the section that answers a question, also when it is phrased differently or asked in another language.
