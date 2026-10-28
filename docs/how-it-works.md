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

- **The memory guide**: a short text (about 900 characters) telling Claude when to search and store, how to cite a memory, to answer in your language, and never to ask you for a key, token or password in the conversation. It comes again after `/clear`, a resume or a compaction.
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

They are added to the prompt under a header that frames them as notes from past sessions, data and not instructions, to verify before use. Claude cites the ones it uses as `🧠 From memory (<project>) — …`.

The prompt hook may also add one short line:

- on the first prompt, a reminder to search memory before answering;
- after 3 prompts without a memory search, when your prompt names things the memory knows about, a nudge that names them;
- a **save prompt** (below).

Every threshold above is a `recall.*` key in [configuration.md](configuration.md#recall-what-reaches-claudes-context).

## The save detector

backsight looks at your prompt for the signs of something worth keeping, and then adds one line asking Claude to store it with `memory_store` if it is durable:

- a rule or a correction: words such as "remember", "from now on", "we decided", "actually", "never", "always" (and their French equivalents: "retiens", "désormais", "jamais", "toujours"), or a prompt that starts with "non, pas";
- an approval: a short "ok", "yes", "lgtm", "looks good", "perfect" (or "oui", "parfait", "d'accord") right after Claude asked a question or a subagent finished.

Claude decides whether the thing is durable and what to write; nothing is stored without a `memory_store` call, which you see in the session. `/backsight:remember <fact>` stores a fact directly.

## Before Claude asks you a question

When Claude is about to ask you a question with its `AskUserQuestion` tool and has not searched the memory during this prompt, the first attempt is refused with a reason: search the memory first, then ask again saying "nothing in memory" if it has no answer. Up to 2 memories that match the question, found within 6 seconds, come with the reason. This happens at most once per prompt, and never while the memory is unavailable, so a question is delayed, never blocked.

The point: many questions Claude would ask you ("which server?", "what did we decide for X?") are already answered in memory.

## Subagents

- **When a subagent starts**, it gets a short memory guide (including how its final report should read: findings with their cause, decisions with their reason, what was delivered where) and the top of the briefing (at most 600 characters), except the types in `hooks.subagent_skip` (by default `Explore`, `statusline-setup` and `claude-code-guide`).
- **When it finishes**, its final report is redacted, cut at 4,000 characters and sent like a note, unless the session is paused or the project excluded.

## The memory tools

Claude has three tools, served by `backsight mcp` through the plugin:

| Tool | Does |
|---|---|
| `memory_search` | Searches the memory with 1 to 4 short queries, like `backsight search`, with each hit's kinds (`[decision, state]`) after its date. Claude is told to write each as a topic plus an intent word (`docker compose subnet pitfall`), never a kind word alone, and to search before a risky action, design work or asking you a preference. |
| `memory_read` | Shows one memory with the facts it was built from. |
| `memory_store` | Stores one durable fact, after redaction; text that still looks like a secret is refused. Facts go to the current project unless Claude names another one; the transverse pool only when named (`transverse`, for user-wide rules), and in your home directory or `~/.claude` a fact without a project is refused. It returns the 3 most similar memories, so Claude can say whether the fact updates or repeats one. Claude is told to store a dated fact that names the old claim when work ships or a recalled memory proves wrong. |

Setup adds a permission rule that lets Claude call these three tools without asking you each time. Without that rule Claude Code would ask you before each call; if you remove it, `backsight doctor` reports the wiring as drifted and `backsight client install` puts it back.

## When a session is sent

A session is sent when it ends (Claude Code's `SessionEnd`), or by the background job every 15 minutes once its last turn is 30 minutes old. A resumed session sends its new turns the same way. Facts stored with `memory_store` are sent at once, or queued while the server is unreachable. The first memories of a session appear a few minutes after it is sent, once extraction ran.

After a purge (`forget --purge`), the session's later turns go to a new document (`conversation:<session>#2`): the purged one is never written again. A client that does not know about purges (an older backsight on another machine, or a rollback) can still append to the purged document, and the server then recreates it. The background job looks for that after it sent the sessions: every 15 minutes among the documents created since its last look (one request), and once a day, in `jobs maintain`, for every purged document. It purges such a document again, with what was extracted from it, and the next send puts the turns written there after the purge into the new document, so none is lost.

## Time and context it costs

**Time.** Only prompts that recall wait for the server; the others add a few milliseconds. On a CPU, nearly all of a recall is the reranker: about 1.5 s typical and 2 to 2.5 s at worst, measured on a desktop CPU with a memory of several thousand items. It grows with the memory. doctor's `latency` check warns when the prompt hook's 95th percentile is over 2.5 s across 7 days; [troubleshooting.md](troubleshooting.md#slow-recall-and-gpu-acceleration) has the levers. After 3 recalls in a row fail, recall pauses for 5 minutes (`network.breaker_*`).

**Context.** What backsight adds to Claude's context window:

| What | Size |
|---|---|
| The memory guide, each session start | about 900 characters |
| The briefing, each session start | up to about 1,600 characters |
| The memory tools' descriptions and instructions | about 2,000 characters |
| Memories on prompts, whole session | up to 6,000 characters (`recall.session_budget_chars`) |
| Reminder, nudge, save prompt | 100 to 300 characters each, when they fire |

At about 4 characters per token, a full session costs at most 3,000 to 4,000 tokens of context. `backsight stats` shows what the prompt hook actually added per session.

## Hooks

| Claude Code event | When | backsight does | Time limit |
|---|---|---|---|
| `SessionStart` (startup) | A new session | The guide, the briefing, the status line. | 10 s |
| `SessionStart` (resume, clear, compact, fork) | The session goes on | The guide; after a compaction, the briefing and last memories again. | 5 s |
| `UserPromptSubmit` | Each prompt | Recall, reminder, nudge, save prompt. | 25 s |
| `PreToolUse` (`AskUserQuestion`) | Claude asks you a question | The question gate. | 10 s |
| `PostToolUse` (the memory tools) | After a memory tool call | Notes that memory was searched, for the gate and the nudge. | 2 s |
| `PostToolUseFailure` (`Bash`, file and web tools, MCP tools) | A tool call fails | With `recall.failure_pitfalls = true` only: searches the memory for the failure (tool, command and error text, secrets redacted) and adds up to 2 memories scoring at least 0.8, once per kind of error in a session. Does nothing when the session is paused, the project excluded or the session budget spent. | 10 s |
| `SubagentStart` | A subagent starts | Its short memory guide. | 5 s |
| `SubagentStop` | A subagent finishes | Queues its report. | 5 s |
| `Stop` | Claude's reply ends | Notes the end of the reply, for the save detector. No network. | 5 s |
| `SessionEnd` | The session ends | Starts sending it, in the background. | 1 s |

`backsight verify` runs every hook as Claude Code does. `backsight why` shows what the hooks added in the latest session, and why.
