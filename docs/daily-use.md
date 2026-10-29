# Daily use

What backsight does during your Claude Code sessions, and the commands you use day to day. The details of each step, with what it costs in time and context, are in [how-it-works.md](how-it-works.md); words you may not know are in the [glossary](glossary.md).

## What happens in a session

You use Claude Code as usual. backsight works through hooks and memory tools:

- **At session start**, Claude gets a short guide on using the memory, and a briefing for the current project: a page the server keeps from the project's memories (open items, recent work, rules in force).
- **On each prompt**, backsight searches the memory with your prompt and adds the few most relevant memories to Claude's context, framed as notes to verify.
- **Claude searches and stores** with four memory tools: `memory_search`, `memory_read`, `memory_store` and `memory_credit`. It is told to search before factual questions, recommendations and new tasks, and to store durable decisions, corrections, conventions and pitfalls; `memory_credit` names the memories shown to it that informed an answer, and returns the citation line it prints; with the same tool, Claude reports a memory that proved wrong or outdated, which retires it ([below](#memories-claude-reports-wrong)). A turn that showed memories and ends without that report is held once: Claude is asked to make it, then finishes.
- **Before Claude asks you a question** with its question tool, it must have searched the memory during this prompt: the first attempt is sent back once, with the memories that match the question ([details](how-it-works.md#before-claude-asks-you-a-question)).
- **When you state a rule or approve something** ("from now on…", "we decided…", "ok"), a line asks Claude to store it if it is durable ([the save detector](how-it-works.md#the-save-detector)).
- **When the session ends**, or once its last turn is 30 minutes old, its text is redacted and sent to your server, which extracts memories from it in the background.

Hooks never block Claude Code: if the server is down, recall is skipped and writes wait in a local queue. When memory is unavailable, you see one message in the session.

## The line at session start

Each session starts with one line from backsight (Claude Code shows it after `SessionStart:startup says:`). Its first emoji tells the memory's state, and 🗂️ names the project. The word backsight is shown in rosewood; set `NO_COLOR=1` in the environment Claude Code starts from to turn the color off:

| Line | Means |
|---|---|
| 🧠 backsight · 🗂️ my-project | Recall and recording are on. |
| ⏸️ backsight · 🗂️ my-project · recall on, this project is not recorded | The project is excluded: `backsight pause --project`, `privacy.exclude_paths`, or a `.backsight.toml` with `retain = false`. Memories are still recalled. |
| 🔕 backsight · 🗂️ my-project · recall on, recording off | `components.write = false` in `config.toml`: no session is recorded. |
| ⚠️ backsight · 🗂️ my-project · memory unavailable (server unreachable) · run `backsight doctor` | No recall in this session for now; sessions are sent later. See [the server is down](troubleshooting.md#the-server-is-down). |
| 🛠️ backsight · 🗂️ my-project · memory unavailable (maintenance in progress), back when it ends | A backup, restore or update is running. |

More lines may follow: ⬆️ a newer release ([update](update.md)), ⚠️ Claude Code's wiring drifted (run `backsight client install`), ⚠️ sessions quarantined (run `backsight doctor`). The first session after setup also starts with 👋 and says where sessions are stored. If memory fails later in a session, one ⚠️ or 🛠️ line says so, once.

## Recognizing a memory

Each memory shown to Claude carries its project and a short id, the first 8 characters of its id. When Claude uses a memory, it reports it with the `memory_credit` tool, which hands back the line it prints:

> 🧠 **From memory (my-project · 0b4c2a52)** — the API client retries 3 times, decided on 2026-10-12 because the gateway drops idle connections.

The part in parentheses names the project and the short ids of the memories used; what follows the dash is Claude's own summary of what it used. When memories were shown but none helped, Claude reports that instead and prints no line. If Claude searched and found nothing, it says so. Memories are hints from past sessions: Claude is told to check them against the code before relying on them.

## Projects and the transverse memory

Each memory belongs to a project: the Git repository you worked in (a worktree counts as its main repository), or the directory outside one. Recall searches the current project together with the [transverse project](glossary.md#transverse-pool), which holds rules valid everywhere (your preferences, your conventions), and also the whole memory and related projects, with a boost for the current project.

To give a repository another project name, to search other projects with it, or to keep it out of memory, put a `.backsight.toml` at its root:

```toml
project = "my-project"
related = ["shared-lib", "infra"]   # projects searched with this one
# retain = false   # never record sessions in this repository
```

`project` and `related` apply only from a file git does not track: keep it untracked, for example by listing `.backsight.toml` in `.git/info/exclude`. A file git tracks came with the repository, and whoever published it would otherwise choose where your sessions are stored and which projects' memories reach them; backsight ignores those two keys there, and `backsight doctor` names the file. `retain = false` applies from any file, committed or not, so a repository can keep itself out of everyone's memory. Outside git, every file is trusted.

`related` lists at most 10 projects, which prompt recall, `memory_search` and `backsight search` search together with the current one, without its boost. The nearest `.backsight.toml` that sets `related` wins (a subdirectory's file over the repository's), and `related = []` clears a parent's list.

A subagent's report goes to the repository its work was in, which may not be the one the session started in: the repository its tool calls name most, where an edit or a commit counts three reads, when it clearly dominates. Only your own repositories count: those whose remote owner (for example `github.com/you`) is the owner of a repository you committed to and started a recorded session in (`backsight doctor` lists them; `projects.deny_owners` removes one), and the owners listed in `config.toml`:

```toml
[projects]
own_owners = ["github.com/your-org"]
```

A clone of someone else's repository (a package build, a framework checkout) never becomes a project, nor does a directory kept out of memory or a copy of it. A temporary clone of one of your repositories counts for that repository, under its usual name. A report keeps the project it was filed under, unless you exclude that repository later. For the main conversation, backsight only records which project this rule would have chosen; `backsight stats` and `backsight doctor` show how often it differs.

backsight also notes which of your repositories are worked on together: two projects touched in the same turns (a subagent's report counts as a turn), in at least 5 turns of 2 sessions over the last 60 days, making at least a fifth of the busier project's turns. A project you exclude, pause or forget leaves every family. `backsight projects` lists them ("worked on with"). With `recall.families = true` in `config.toml`, recall searches a project's family together with it, like `related`, without its boost. It is off by default until it is shown to improve recall.

## The language memories are written in

You can work in any language. Memories are always written **in English**:

- The server's extraction is told to write every fact and observation in English, whatever the language of the session, keeping identifiers, file paths, commands, versions and your quoted wording exactly as written. `/backsight:remember` asks Claude for English facts too.
- Claude still **answers in your language**: its memory guide tells it to.
- Search works across languages. Your prompts are searched as you type them, not translated, and the embedding and reranker models are multilingual (`multilingual-e5-small` and `mmarco-mMiniLMv2-L12-H384-v1`), so a French prompt finds English memories.

French sessions are read and extracted like English ones; only the facts that come out are in English. In tests on real French and English sessions, English facts were recalled as well as French ones, and one fact language keeps a single set of instructions to test.

There is no language setting. `backsight server llm check` checks that the extraction model writes English facts.

## Skills

The plugin adds five skills you can call in Claude Code:

| Skill | Use |
|---|---|
| `/backsight:remember <fact>` | Store a fact now, for example a rule from now on. Claude picks the kind and the project. |
| `/backsight:pause` | Stop recording this session; `/backsight:pause project` stops recording this project. |
| `/backsight:memory-retro` | At the end of a project: Claude reads the recent git history, searches the memory topic by topic for what is already stored, then writes the decisions (rejected alternatives included), pitfalls, open questions and lessons into memory. It also asks which recalled memory proved wrong and proposes, without applying, moving lasting rules into your CLAUDE.md, a `.claude/rules` file or a hook. |
| `/backsight:memory` | Command-line access to the memory (search, read, why) when the memory tools are missing or failing. |
| `/backsight:doctor` | backsight itself: Claude runs `doctor` and the docs to find what is wrong, fixes it with your OK, and for a bug drafts a GitHub issue from `backsight report` that it files only once you confirm. |

Claude also calls them on its own when you say "remember", "from now on" or "pause memory".

## Searching from the terminal

```sh
backsight search "deploy docker compose" "backup restore"
```

Each argument is one short keyword query (at most 4). It searches like recall (the current and transverse projects, related projects and the whole memory); `--all` searches the whole memory without project scopes, `--project <name>` adds a project. Each hit shows, after its date, the kinds of the facts it was built from (`[pitfall, state]`: a memory can merge several), and `subagent` when one of them came from a subagent's report, so a decision can be told from a state or a pitfall without reading it. `--json` has them as `kinds` and `subagent`. `backsight read <id>` shows one memory with the facts it was built from.

`backsight ask "<question>"` has the server's LLM answer a question over many memories. It costs tokens, shown with the answer.

`backsight projects` lists the projects in memory with their counts and last activity.

## Storing a fact yourself

```sh
backsight store --kind decision "The billing service keeps PostgreSQL 16 for partial indexes (2026-10-24)."
```

Kinds: `decision`, `correction`, `convention`, `pitfall`, `state`, `lesson`. One fact per call, with the why and absolute dates. Without `--project` the fact goes to the current directory's project; `--project <name>` stores it in another one. The transverse pool reaches every session, so a fact goes there only when asked by name: `--project transverse` for a user-wide rule (any of its names in `projects.transverse` works too, see [configuration.md](configuration.md#projects)). In your home directory or `~/.claude`, which belong to the transverse pool, a fact without `--project` is refused. Claude's `memory_store` follows the same rules (`project: "transverse"`). Text that looks like a secret is refused.

## Reviewing what was stored

Claude stores facts without asking you first. To see what it stored:

```sh
backsight memory notes
```

It lists the notes of the last 7 days, newest first: when, the project, the Claude Code session it came from, the kind and the text. `--days N`, `--limit N` and `--project <name>` (`transverse` for the transverse pool) narrow it. `backsight doctor` counts the notes of the week, the transverse pool's apart. A wrong note: find its memories with `backsight search`, then `backsight forget <id>`.

## Forgetting a memory

```sh
backsight forget <id>
```

This retires the [memory](glossary.md#memory): recall stops returning it. `backsight forget <id> --restore` brings it back.

For a leaked secret, `backsight forget <id> --purge --yes` deletes the memory, its source facts and the session documents they came from. Backups made before still hold it; the command lists them. `backsight memory scan` lists stored memories that look like they hold a secret, with the purge command for each.

To forget a whole project, start with its plan:

```sh
backsight forget --project old-prototype --plan
backsight forget --project old-prototype --answer confirm_project=old-prototype
```

The plan lists the project's documents and memory counts, changing nothing. The second command retires every [raw fact](glossary.md#fact) of the project; `--purge` deletes its documents and [observations](glossary.md#observation) for good instead. `--source subagent` (or `chat`, `note`) keeps to the facts of one source. See [privacy.md](privacy.md#removing-what-was-stored).

## Memories Claude reports wrong

When a memory shown in a session proves false or no longer true, Claude reports it with `memory_credit` (`verdict: wrong` or `outdated`), giving the right fact and what in the session shows it. backsight stores the right fact as a note and retires the wrong memory, at most 3 per session (`feedback.max_retirements`). Nothing is deleted:

- a [raw fact](glossary.md#fact) is invalidated on the server: recall stops returning it;
- for an [observation](glossary.md#observation), Claude is first shown the facts it is drawn from, then names the wrong ones, which are invalidated;
- an observation whose facts the server cannot list is marked hidden on this machine, until the server rewrites or drops it.

Claude tells you which memory it retired and why. To undo it:

```sh
backsight forget --restore <id>
```

The fact is valid again, or the observation no longer hidden, and the report no longer counts against it.

## Reviewing stale memories

```sh
backsight memory stale
```

It lists the memories worth a look, from this machine's record of what was shown to Claude and what it used. Each memory appears once, in the first section that fits, with its short id, the times it was shown, when it was last shown and the command that deals with it:

| Section | Lists | Command |
|---|---|---|
| Shown often, never used | Memories shown at least 10 times (`feedback.stale_shows`) that Claude never reported using. | `backsight forget <id>` retires one. |
| Hidden, not rewritten for 7 days | Observations hidden after a report, because the server could not list the facts they are drawn from, and not rewritten since. One hidden for less is not listed: the server may still rewrite it. | `backsight forget --restore <id>` shows one again. Until the server rewrites them they stay hidden from recall; `backsight forget <id>` retires one for good once the server lists its source facts. |
| Reported wrong or outdated | Raw facts Claude reported, already invalidated. | `backsight forget <id> --purge` alone only lists what deleting one for good would remove; adding `--yes` deletes it. |

`backsight forget --restore <id>` undoes a retirement. The command reads local files only and changes nothing in the memory; `--json` gives the same lists.

## Pausing memory

| Command | Effect |
|---|---|
| `backsight pause` | Nothing typed in this session from now on is sent to memory. Inside Claude Code, use `/backsight:pause`. |
| `backsight resume` | The session is recorded again. |
| `backsight pause --project` | Adds this project's directory to `privacy.exclude_paths` in `config.toml`: no session in it is recorded. |
| `backsight resume --project` | Removes it. |

See [privacy.md](privacy.md) for the other opt-outs.

## Importing past sessions

Setup offers to import the sessions Claude Code still keeps ([backfill](glossary.md#backfill)). You can also run it later:

```sh
backsight retain backfill --plan
backsight retain backfill --since 2026-01-01 --max-cost 2
```

`--plan` shows the sessions, their size, the expected cost and duration. Backfill sends the oldest first, in the background, and stops at the spending cap (`--max-cost`, default 1.5 times the estimate). On the Claude subscription it is refused unless you add `--allow-subscription`.

`backsight retain backfill` runs until the import is done and prints a line on stderr after each session (`3/42  sent  <session>  spent …`) and when it waits for the server. To see how far an import is, whether you started it or setup left it to `jobs tick`:

```sh
backsight retain backfill --status
```

It reads this machine's state only, so it answers at once, even with the server down: sessions done and left, the spend against the cap, whether a backfill is sending right now, and, when it paused, how to resume. `backsight doctor` shows the same progress in its `backfill` check.

To import a queue setup left to `jobs tick` in one go rather than three sessions every 15 minutes, run `backsight retain backfill` with the same `--projects-dir` setup used (and `--allow-subscription` on the Claude subscription): other filters start a new queue.

## Seeing what memory does

- `backsight why` shows the last memories the hooks injected, with their scores and the rule that let them through.
- `backsight stats` shows the last 7 days: recalls, searches, citations, which memories Claude reported using, wrong or outdated, what those reports retired, extraction cost (`backsight stats 30` for 30 days).
- `backsight feedback log --id <id>` shows the timeline of one memory: each time it was shown to Claude, where and at which rank, each time Claude reported using it, and how each showing was settled (credited, a confident miss when it was near the top and unused, or unknown when the turn ended without a report), then its counts. `--session <id>` shows a whole session instead (its first 8 characters are enough).
- `backsight doctor` checks the whole install.

## Browsing these docs

`backsight guide` lists these documents and `backsight guide <topic>` shows one in the terminal. `backsight guide search <question>` finds the section that answers a question, also when it is phrased differently or asked in another language. It searches a docs [bank](glossary.md#bank) on your server, which never mixes with your memories and costs no LLM call; when the server cannot answer, a keyword search over the docs in the binary answers instead, and the output says so.
