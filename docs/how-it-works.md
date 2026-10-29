# How it works

What backsight does at each step of a Claude Code session, what it adds to Claude's context, and what it costs in time. Words in **bold** are in the [glossary](glossary.md).

## The data flow

```text
your session ──hooks──▶ redaction ──▶ Hindsight server ──▶ LLM provider
   (Claude Code)          (local)       (Docker, local)     extraction, consolidation
        ▲                                     │
        └──── recall: briefing, prompt memories, memory_search ◀┘
```

1. Claude Code runs backsight's **hooks** at fixed moments of a session (table below).
2. When a session **settles**, its text is redacted and sent to your Hindsight server as a **document**. Only your prompts, Claude's replies and one line per tool call are sent: no tool results, no thinking, no file contents ([privacy.md](privacy.md)).
3. The server's LLM extracts **raw facts** from it, then **consolidates** them into **observations**, in the background.
4. On later sessions, **recall** brings the relevant memories back into Claude's context.

Hooks never block Claude Code. When the server is down or slow, recall is skipped, writes wait in a local queue, and you see one message in the session.

## At session start

- **The memory guide**: a short text (about 1,100 characters) telling Claude when to search and store, to report the memories it used with `memory_credit`, to answer in your language, and never to ask you for a key, token or password in the conversation. It comes again after `/clear`, a resume or a compaction.
- **The briefing**: the project's page (at most about 1,600 characters), written by the server from the project's memories: open items, recent work, rules in force. After a compaction, the briefing and the last memories shown are given again.
- **The status line** you see, such as `🧠 backsight · 🗂️ my-project` ([daily-use.md](daily-use.md#the-line-at-session-start)).

## On each prompt

When you send a prompt, the prompt hook decides whether to recall:

- **The first prompt** of a session recalls from 25 characters.
- **Later prompts** recall when they are at least 60 characters long and either 3 prompts passed since the last recall or the topic changed (few words in common with the last recalled prompt).
- **Never** past 6,000 characters of memory text in one session, nor for prompts Claude Code writes itself (task notifications, command output).

To recall, it searches the start of your prompt (300 characters, code blocks removed, secrets redacted) in the current project (with a small boost), the transverse pool, the related projects and the whole memory. Each candidate is scored by the **reranker**. It keeps at most 3 memories, 800 characters in all, that:

- score at least 0.5 (0.6 for one found outside your projects);
- were not shown earlier in the session;
- were not shown 3 times in the last 24 hours, unless they match almost exactly.

They are added to the prompt under a header that frames them as notes from past sessions, data and not instructions, to verify before use, each with its project and short id (`[billing · 0b4c2a52]`). A last line asks Claude to report, before its final message, which of them informed its answer ([below](#which-memories-helped)).

The prompt hook may also add one short line:

- on the first prompt, a reminder to search memory before answering and to report the memories it used;
- after 3 prompts without a memory search, when your prompt names things the memory knows about, a nudge that names them;
- a **save prompt** (below).

Every threshold above is a `recall.*` key in [configuration.md](configuration.md#recall-what-reaches-claudes-context).

## The save detector

backsight looks at your prompt for the signs of something worth keeping, and then adds one line asking Claude to store it with `memory_store` if it is durable:

- a rule or a correction: words such as "remember", "from now on", "we decided", "actually", "never", "always" (and their French equivalents: "retiens", "désormais", "jamais", "toujours"), or a prompt that starts with "non, pas";
- an approval: a short "ok", "yes", "lgtm", "looks good", "perfect" (or "oui", "parfait", "d'accord") right after Claude asked a question or a subagent finished.

Claude decides whether the thing is durable and what to write; nothing is stored without a `memory_store` call, which you see in the session. `/backsight:remember <fact>` stores a fact directly.

## Which memories helped

backsight keeps a local record of which memories Claude was shown and which it used, ids and counts only (`feedback.*` in [configuration.md](configuration.md#feedback-which-memories-claude-used)):

1. **A memory is shown**: in the briefing, under a prompt, after a failed tool call, with a refused question, or in a `memory_search` or `memory_read` result. backsight notes its id, how it was shown and its rank.
2. **Claude reports**: once it knows what it used, before its final message, it calls `memory_credit` with the ids of the memories that informed its answer, or with `verdict: none`. The tool accepts only memories shown in the session and returns the line Claude prints: `🧠 From memory (<project> · <id8>) — …`.
   When a memory shown in the session proves false (`verdict: wrong`) or no longer true (`verdict: outdated`), Claude reports it with the same tool, giving the right fact (`correction`, 1 to 3 sentences) and what in the session shows it (`evidence`, at most 300 characters: your words, a file, a command output). backsight stores the correction as a note of kind `correction` in the session's project (in the transverse pool when every memory it corrects is transverse), whose metadata names the memories it replaces and the evidence; the correction and the evidence never go into the local record. Then it retires what was corrected, and nothing is ever deleted: a raw fact is invalidated on the server (`backsight forget --restore <id>` undoes it); for an observation the tool first lists the facts it is drawn from, and Claude calls again naming the wrong ones, which are invalidated; an observation whose facts the server cannot list is hidden on this machine (recall no longer returns it here), until the server rewrites or drops it (`backsight jobs tick` looks) or `backsight forget --restore <id>` clears the mark. When the server refuses a retirement halfway, the answer says which memories were retired and which were not. The report is refused, and nothing is stored, when a memory was not shown in the session, belongs to a project other than the session's, its linked projects or the transverse pool (`backsight forget <id>` stays yours for those), when the session has reached `feedback.max_retirements`, or when the correction or the evidence holds a secret, the session is paused or the directory is excluded.
3. **The end of the turn checks**: a turn that showed memories and ends without that report is held once, with the memories listed; Claude reports, then finishes (`feedback.stop_check`).
4. **The turn settles**: every 15 minutes, the background job settles the memories shown at least 30 minutes ago in turns that are over. Each becomes a **credit** (Claude reported using it), a **confident miss** (shown in the top 2, `feedback.confident_rank`, of a prompt, a search or a refused question, in a turn Claude reported on, and not used), **unknown** (Claude never reported on that turn), or simply shown. A credit made after its memory settled counts nowhere and is counted as late.
5. **Recall uses them**: a memory Claude often uses rises in later recalls, one often shown near the top and left unused sinks, by a small, bounded amount added to its score (at most +0.075 or −0.025 by default, `feedback.boost_weight`, `feedback.prior_credits`, `feedback.prior_misses`). A memory with nothing counted keeps its score, and counts fade with their half-lives, so an old use matters less than a recent one. The floors on the server's score (`recall.min_score`, `recall.global_min_score`) apply first: the boost reorders what passed them and never lets a memory under them in; `memory_search`'s `min_score` sees the boosted score. A hidden observation is skipped from the next recall on, before the background job has run. When the record cannot be read, recall goes on as if it were empty. `memory_search` notes in the record, for its session, each hit's boost and the rank it would have had without it.

How the boost moves with the counts, before they fade, with one uncredited show in three counted as a miss: a memory credited one time in ten stays where the reranker put it, one credited more often rises and one credited less often sinks (chart: https://github.com/Barben360/backsight#how-memory-learns-what-helps).

`backsight stats` shows these counts on its `Credits` line (how turns ended and what Claude reported), its `Ledger` line (what settled, apart from what was imported from earlier history) and its `Boost` line (among the prompts and searches that showed memories, how many showed other ones because of these counts).

With several machines on one server, each shares its counts with the others (`feedback.share_ledger`, on by default): its background job sends them to the server's control bank (memory ids, numbers and times only, plus a random id of the machine) when they changed, at most once an hour, at once when a memory was hidden or restored, and once a week even when nothing changed; it fetches the other machines' every 6 hours and combines them then. A change of `feedback.miss_half_life_days` or `feedback.credit_half_life_days`, and a machine turning 30 days silent, count within one tick (15 minutes): the job combines the copy already fetched again, even while the server is down. Recall adds their uses and misses to this machine's, each faded by its age, and skips a memory another machine hid, unless `backsight forget --restore <id>` restored it here after that hiding, which shows it again on this machine only. A machine silent for 30 days no longer counts. Recall reads the copy fetched last, never the server: when that copy is missing or cannot be read, it uses this machine's counts alone. `backsight memory stale`, `backsight feedback replay` and the counts of `backsight stats` stay this machine's; the `Ledger` line of `stats` adds how many other machines recall weighs.

## Before Claude asks you a question

When Claude is about to ask you a question with its `AskUserQuestion` tool and has not searched the memory during this prompt, the first attempt is refused with a reason: search the memory first, then ask again saying "nothing in memory" if it has no answer. Up to 2 memories that match the question, found within 6 seconds, come with the reason. This happens at most once per prompt, and never while the memory is unavailable, so a question is delayed, never blocked.

The point: many questions Claude would ask you ("which server?", "what did we decide for X?") are already answered in memory.

## Subagents

- **When a subagent starts**, it gets a short memory guide (including how its final report should read: findings with their cause, decisions with their reason, what was delivered where) and the top of the briefing (at most 600 characters), except the types in `hooks.subagent_skip` (by default `Explore`, `statusline-setup` and `claude-code-guide`).
- **When it finishes**, its final report is redacted, cut at 4,000 characters and sent like a note, unless the session is paused or the project excluded.

## The memory tools

Claude has four tools, served by `backsight mcp` through the plugin:

| Tool | Does |
|---|---|
| `memory_search` | Searches the memory with 1 to 4 short queries, like `backsight search`, with each hit's kinds (`[decision, state]`) after its date. Claude is told to write each as a topic plus an intent word (`docker compose subnet pitfall`), never a kind word alone, and to search before a risky action, design work or asking you a preference. |
| `memory_read` | Shows one memory with the facts it was built from: by its full id, or by the short id of a memory shown in this session. |
| `memory_store` | Stores one durable fact, after redaction; text that still looks like a secret is refused. Facts go to the current project unless Claude names another one; the transverse pool only when named (`transverse`, for user-wide rules), and in your home directory or `~/.claude` a fact without a project is refused. It returns the 3 most similar memories, so Claude can say whether the fact updates or repeats one. Claude is told to store a dated fact that names the old claim when work ships or a recalled memory proves wrong. |
| `memory_credit` | Claude names the memories shown to it in this session that informed its answer (their full ids or the 8-character short ids shown next to them), and gets back the line to print in its reply: `> 🧠 **From memory (<project> · <id8>)** — <the facts used>`. When none did, it says so with `verdict: none`. A memory the session shows false or no longer true: `verdict: wrong` or `outdated`, with the right fact as `correction` and what shows it as `evidence`, stored as a correction note, and the memory is retired ([which memories helped](#which-memories-helped)). Every id must have been shown in this session, or the call is refused and nothing is recorded; a memory found by a search sent in the same message counts once that search has returned. Offered only while `feedback.enabled` is on (see [configuration](configuration.md#feedback-which-memories-claude-used)). |

Setup adds a permission rule that lets Claude call these four tools without asking you each time. Without that rule Claude Code would ask you before each call; if you remove it, `backsight doctor` reports the wiring as drifted and `backsight client install` puts it back.

## When a session is sent

A session is sent when it ends (Claude Code's `SessionEnd`), or by the background job every 15 minutes once its last turn is 30 minutes old. A resumed session sends its new turns the same way. Facts stored with `memory_store` are sent at once, or queued while the server is unreachable. The first memories of a session appear a few minutes after it is sent, once extraction ran.

After a purge (`forget --purge`), the session's later turns go to a new document (`conversation:<session>#2`): the purged one is never written again. A client that does not know about purges (an older backsight on another machine, or a rollback) can still append to the purged document, and the server then recreates it. The background job looks for that after it sent the sessions: every 15 minutes among the documents created since its last look (one request), and once a day, in `jobs maintain`, for every purged document. It purges such a document again, with what was extracted from it, and the next send puts the turns written there after the purge into the new document, so none is lost.

## Time and context it costs

**Time.** Only prompts that recall wait for the server; the others add a few milliseconds. On a CPU, nearly all of a recall is the reranker: about 1.5 s typical and 2 to 2.5 s at worst, measured on a desktop CPU with a memory of several thousand items. It grows with the memory. doctor's `latency` check warns when the prompt hook's 95th percentile is over 2.5 s across 7 days; [troubleshooting.md](troubleshooting.md#slow-recall-and-gpu-acceleration) has the levers. After 3 recalls in a row fail, recall pauses for 5 minutes (`network.breaker_*`).

**Context.** What backsight adds to Claude's context window:

| What | Size |
|---|---|
| The memory guide, each session start | about 1,100 characters |
| The briefing, each session start | up to about 1,600 characters |
| The memory tools' descriptions and instructions | about 4,000 characters |
| Memories on prompts, whole session | up to 6,000 characters (`recall.session_budget_chars`) |
| The line asking Claude to report what it used, under each recall | about 200 characters |
| Reminder, nudge, save prompt | 100 to 350 characters each, when they fire |

At about 4 characters per token, a full session costs at most 3,000 to 4,000 tokens of context. `backsight stats` shows what the prompt hook actually added per session.

## Hooks

| Claude Code event | When | backsight does | Time limit |
|---|---|---|---|
| `SessionStart` (startup) | A new session | The guide, the briefing, the status line. | 10 s |
| `SessionStart` (resume, clear, compact, fork) | The session goes on | The guide; after a compaction, the briefing and last memories again. | 5 s |
| `UserPromptSubmit` | Each prompt | Recall, reminder, nudge, save prompt. | 25 s |
| `PreToolUse` (`AskUserQuestion`) | Claude asks you a question | The question gate. | 10 s |
| `PostToolUse` (the memory tools) | After a memory tool call | Notes that memory was searched, for the gate and the nudge; records the memories a search or a read showed, and the ones Claude credited with `memory_credit`. | 2 s |
| `PostToolUseFailure` (`Bash`, file and web tools, MCP tools) | A tool call fails | With `recall.failure_pitfalls = true` only: searches the memory for the failure (tool, command and error text, secrets redacted) and adds up to 2 memories scoring at least 0.8, once per kind of error in a session. Does nothing when the session is paused, the project excluded or the session budget spent. | 10 s |
| `SubagentStart` | A subagent starts | Its short memory guide. | 5 s |
| `SubagentStop` | A subagent finishes | Queues its report; when the subagent was shown memories, the same check as `Stop`, for the subagent. | 5 s |
| `Stop` | Claude's reply ends | Notes the end of the reply, for the save detector. When the turn showed memories and Claude has not reported which ones it used with `memory_credit`, asks it once, listing them by short id: Claude goes on, reports, and adds the citation line if it used any (`feedback.stop_check`, see [configuration](configuration.md#feedback-which-memories-claude-used)). Never in a paused or headless session, after a message Claude Code sent itself, or when the record cannot be read. No network. | 5 s |
| `SessionEnd` | The session ends | Starts sending it, in the background. | 1 s |

`backsight verify` runs every hook as Claude Code does. `backsight why` shows what the hooks added in the latest session, and why.
