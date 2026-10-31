# Configuration

Every key of `config.toml`, its default and what it does. Setup writes the file; you rarely need to edit it.

## The file

`config.toml` lives in backsight's config directory (`~/.config/backsight` on Linux; `backsight paths` prints it). Keys you leave out take their default. Unknown keys and invalid values are refused with the key's name, so a typo never goes unnoticed: `backsight doctor` reports it on its `config` line.

Secrets are not in this file: they live in `secrets.toml` next to it, set with `backsight secret set <name>` (see [install.md](install.md#secrets)).

Defaults below are written in TOML. `<data>` stands for backsight's data directory (`~/.local/share/backsight` on Linux) and `<user>` for the name of your home directory.

## When a change applies

| Keys | Applies |
|---|---|
| `recall.*`, `privacy.*`, `components.*`, `projects.*`, `hooks.*`, `feedback.*` | At the next hook or command: the hooks read the file each time. |
| `llm.*`, `network.*`, `server.ui_port`, the port of `server.url` | After `backsight server render` and `backsight server start`, which write the server's files again and recreate it. Then `backsight server llm check` tests an LLM change. |
| `backup.*`, `update.*` | At the next daily job, or the next `backup` or `update` command. |
| `client.retention_days` | At the next `backsight client install` or `backsight client retention`. |

## Top level

| Key | Default | Effect |
|---|---|---|
| `schema_version` | `1` | Version of this file's format. Setup writes it; a file from a newer backsight is refused. |

## `[server]`: the memory server

| Key | Default | Effect |
|---|---|---|
| `server.url` | `"http://127.0.0.1:8888"` | The server's API address. With `server.mode = "local"`, its port is the API port the server publishes, and it must be explicit. |
| `server.bank` | `"claude-memory"` | The [bank](glossary.md#bank) that holds your memory. Setup writes `"backsight"` for a new install; without the key the bank is `claude-memory`, the name of installs before v0.1.4-m5, which `backsight bank rename` moves to `backsight` (the update to v0.1.4-m5 does it). backsight also uses `<bank>-docs` for these docs and `<bank>-control` for its own records. |
| `server.mode` | `"local"` | `"local"`: backsight runs the server in Docker on this machine and manages it. `"remote"`: you run the server elsewhere; backsight only talks to `server.url`, and the commands that manage a local server or write its banks refuse ([the list](commands.md#server)). `backsight connect` sets it, with `server.url` and `server.bank`, from the token of the server's host ([several PCs](several-pcs.md)). |
| `server.role` | `"both"` | `"both"`: this PC runs Claude Code, and its server too when `server.mode = "local"`. `"server"`: a host that only serves the memory, without Claude Code; `backsight setup --role server` sets it (see [install](install.md#a-server-only-host) and [several PCs](several-pcs.md#choose-the-host)); it needs `server.mode = "local"`. |
| `server.ui_port` | `9999` | Port of the Hindsight web UI, on the same address as the API. Must differ from the API port. |
| `server.compose_dir` | `"<data>/server"` | Where the server's `compose.yaml`, `.env` and your own `compose.override.yaml` live. An absolute path. |
| `server.project` | `"backsight"` | The Docker Compose project name, and the prefix of the server's volumes. Another name runs a second install beside the first (lowercase letters, digits, `-` and `_`). |
| `server.digest` | `""` | Empty: the Hindsight image this release pins ([compatibility.md](compatibility.md)). A `sha256:…` digest holds another image of `ghcr.io/vectorize-io/hindsight`; you then own that choice. |
| `server.runtime` | `""` | What runs the server when `server.mode = "local"`. Empty (or `"docker"`): Docker Compose, the default. `"native"`: Hindsight from PyPI on pg0's Postgres, under two systemd user units (`<project>-db.service` and `<project>-server.service`), everything else under `<server.compose_dir>/native`. Native runs on Linux x86_64 and Windows x86_64 only (on Windows, a scheduled task and backsight's supervisor instead of the units: [backsight on Windows](windows.md)), and serves this PC only (loopback: `network.exposure = "private"` and `server.role = "server"` are refused with it). Setup sets it, or switching the runtime of an existing install, which moves the memory across; never set it by hand. |
| `server.start_on_session` | `false` | Windows, native runtime: each Claude Code session (its SessionStart hook) starts the server's supervisor when none answers, without waiting for it. Setup sets it only when Task Scheduler refused the server's scheduled task (its `task` pre-flight check); the task otherwise starts the server at logon. Ignored elsewhere. |

## `[client]`: Claude Code's own settings

| Key | Default | Effect |
|---|---|---|
| `client.retention_days` | `0` | The least number of days Claude Code keeps its transcripts (`cleanupPeriodDays`) that backsight keeps in Claude Code's settings. `0`: not managed. Setup sets it from your `retention` answer (3650 recommended), `backsight client retention` changes it. |

## `[projects]`

| Key | Default | Effect |
|---|---|---|
| `projects.transverse` | `[".claude", "<user>"]` | Names of the [transverse](glossary.md#transverse-pool) project: sessions in a directory with one of these names (your home, `~/.claude`) store user-wide memories, and `--project transverse` means it (a stored fact goes there only when named). The first name is the one memories are stored under. |
| `projects.own_owners` | `[]` | Remote owners whose repositories are yours, written `host/owner` (`github.com/your-org`). backsight already counts the owner of a repository you committed to and started a recorded session in (`backsight doctor` lists them); list here the others. A subagent's report goes to the repository its work was in only when that repository is yours: a clone of someone else's code never becomes a project. |
| `projects.deny_owners` | `[]` | Remote owners whose repositories are never yours, even when backsight learnt them from a session (`host/owner`). It wins over `own_owners` and what was learnt. |

## `[privacy]`

| Key | Default | Effect |
|---|---|---|
| `privacy.exclude_paths` | `[]` | Directories whose sessions are never sent and get no briefing (their subdirectories and a project's worktrees too). `backsight pause --project` adds the current project, `backsight resume --project` removes it. |
| `privacy.skip_entrypoints` | `["sdk-*"]` | Claude Code entrypoints (glob patterns) whose turns are never sent. The default skips Agent SDK runs (`sdk-cli`, `sdk-ts`, `sdk-py`); `[]` sends them too. |

## `[components]`

| Key | Default | Effect |
|---|---|---|
| `components.read` | `true` | Recall: memories on each prompt, the `AskUserQuestion` gate and the `memory_search` tool. `false` turns them off (`memory_search` then refuses). |
| `components.write` | `true` | Recording: sessions, subagent reports and stored facts are sent. `false` sends nothing, `memory_store` and `backsight store` refuse, and the session line shows 🔕. |

## `[recall]`: what reaches Claude's context

How these fit together: [how-it-works.md](how-it-works.md#on-each-prompt).

| Key | Default | Effect |
|---|---|---|
| `recall.min_score` | `0.5` | Relevance floor (the reranker's score, 0 to 1) for a memory to be added to a prompt or to the gate's answer. `memory_search` and `backsight search` use their own floor, 0.4. |
| `recall.global_min_score` | `0.6` | A higher floor for prompt memories found in the whole memory, outside the current, transverse and related projects. |
| `recall.prompt_hits` | `3` | At most this many memories added to one prompt. |
| `recall.query_chars` | `300` | The first characters of your prompt, code blocks removed and secrets redacted, that are searched. |
| `recall.prompt_chars` | `800` | At most this many characters of memory text added to one prompt. |
| `recall.min_gap` | `3` | After a recall, the next one waits this many prompts, unless the topic changes. |
| `recall.recall_min_chars` | `60` | Shorter prompts are not searched (the first prompt of a session is, from 25 characters). |
| `recall.reminder_min_chars` | `40` | Shortest prompt that gets the first-prompt reminder or a quiet-prompt nudge. |
| `recall.quiet_prompts` | `3` | Prompts in a row without a memory search before a nudge reminds Claude of the memory. |
| `recall.sticky_max` | `3` | A memory added this many times within `recall.sticky_window_h` is not added again, across sessions… |
| `recall.sticky_bypass` | `0.97` | …unless it matches the prompt at least this well. |
| `recall.sticky_window_h` | `24` | The window, in hours, that `recall.sticky_max` counts over. |
| `recall.project_boost` | `0.05` | Added to the score of the current project's memories, so they rank first at equal relevance. |
| `recall.session_budget_chars` | `6000` | Memory text the prompt hook may add over a whole session; recall stops once it is spent. |
| `recall.briefing` | `"model"` | The briefing at session start. `"model"`: a page the server keeps per project from its memories (one LLM call to refresh it after new memories). `"recent"`: the project's 5 most recent memories, no LLM call. |
| `recall.families` | `false` | Search the projects you work on together with the current one (worked out on this machine from your sessions, listed by `backsight projects`) in its project scope, like `related`, without its boost. Off until it is shown to improve recall. |
| `recall.failure_pitfalls` | `false` | When a tool call fails (`Bash`, `Edit`, `Write`, `MultiEdit`, `WebFetch` or an MCP server's tool), search the memory for that failure and hand Claude up to 2 memories scoring at least 0.8, once per kind of error and session, within `recall.session_budget_chars`. Off until it is shown to help. **Needs v0.1.6-m5 or newer:** an older backsight refuses a `config.toml` with a key it does not know, so remove the line before installing an older version. A failed `backsight update apply` is not affected: its rollback puts back the `config.toml` you had before the update. backsight never writes this key. |

## `[llm]`: the model that extracts memories

| Key | Default | Effect |
|---|---|---|
| `llm.provider` | `"deepseek"` | The provider the server calls: `deepseek`, `anthropic`, `openai`, `ollama`, `claude-code`, or another Hindsight provider id. Setup's `provider` answer. Its key is the `llm_api_key` secret. |
| `llm.model` | `"deepseek-flash"` | The model name, as the provider spells it. |
| `llm.base_url` | `""` | The provider's API address. Empty: the provider's own. Set it for a compatible endpoint, or for Ollama. |
| `llm.price_input` | `0.0` | Price per million input tokens, for `backsight stats` and the backfill estimate and cap. `0` with `price_output` also `0`: the binary's table, which knows `deepseek-flash` and its retired name `deepseek-v4-flash` only, at DeepSeek's peak prices ($0.30 input, $0.006 cached, $1.20 output per million tokens; off-peak is half, so estimates and the cap never under-count); other models then show tokens without a cost. |
| `llm.price_cached` | `0.0` | Price per million cached input tokens. `0` with a price set: cached tokens cost the input price. |
| `llm.price_output` | `0.0` | Price per million output tokens, thinking tokens included. |
| `llm.currency` | `"USD"` | The currency the prices and caps are in. |

Check your provider's current prices: they change, and some (DeepSeek's) differ by time of day.

On a client of another PC's server (`server.mode = "remote"`), the server's host extracts the memories: `[llm]` there describes the host's provider, model and prices, for cost estimates only (`backsight stats`, setup's backfill offer). `backsight connect` writes them from the connection token, never a key: a client needs no `llm_api_key`.

## `[embeddings]`

| Key | Default | Effect |
|---|---|---|
| `embeddings.preset` | `"multilingual"` | The embedding and reranker models (`multilingual-e5-small` and `mmarco-mMiniLMv2-L12-H384-v1`), so memories are found in any language. `"english"` is reserved: this version refuses it. |

## `[network]`

| Key | Default | Effect |
|---|---|---|
| `network.exposure` | `"loopback"` | `"loopback"`: the server listens on `127.0.0.1` only. `"private"`: it listens on `network.bind_ip` as well, so other machines of your network can reach it, over plain HTTP; its API and UI still need their keys. `backsight server expose` sets both keys and restarts the server. |
| `network.bind_ip` | `""` | With `"private"` exposure, the address to listen on: a private LAN or tailnet address (10/8, 172.16/12, 192.168/16, 100.64/10, IPv6 ULA), never a public one. |
| `network.breaker_failures` | `3` | After this many prompts in a row where the server could not be reached, recall pauses… |
| `network.breaker_pause_min` | `5` | …for this many minutes, so an unreachable server does not slow every prompt. |

## `[hooks]`

| Key | Default | Effect |
|---|---|---|
| `hooks.subagent_skip` | `["Explore", "statusline-setup", "claude-code-guide"]` | Subagent types that get no memory guide when they start. Their final reports are still sent. |
| `hooks.outbox_warn_mb` | `50` | Size of the notes queue (notes waiting for the server) at which `backsight store` warns and `backsight doctor` warns. At least 1 and at most `hooks.outbox_max_mb`. |
| `hooks.outbox_max_mb` | `500` | Size of the notes queue at which it stops taking notes (`backsight store` refuses and `backsight doctor` reports an error), so a server down for long cannot fill the disk. |

## `[backup]`

| Key | Default | Effect |
|---|---|---|
| `backup.enabled` | `true` | The daily job makes an encrypted backup. `backsight backup create` works either way. |
| `backup.dir` | `"<data>/backups"` | Where backups are written. An absolute path; another disk is a good choice. |
| `backup.recipient` | `""` | Empty: backups are encrypted to the identity backsight generates (`backsight backup identity show`). Else one or more [age](https://age-encryption.org/) recipients, comma-separated, that you hold the keys of. |
| `backup.keep_daily` | `7` | Daily backups kept. At least 1. |
| `backup.keep_weekly` | `4` | Weekly backups kept, beyond the daily ones. |

## `[update]`

| Key | Default | Effect |
|---|---|---|
| `update.auto_apply` | `false` | `true`: the daily job installs a newer release itself, with the same checks and rollback as `backsight update apply`. `false`: it only tells you. |
| `update.source` | `""` | Empty: the GitHub releases of `Barben360/backsight`. Else an absolute path to a directory of downloaded release files. |
| `update.prereleases` | `false` | Offer pre-releases even when the installed version is a stable one. A pre-release install is offered pre-releases anyway. |

## `[feedback]`: which memories Claude used

When memories were shown to Claude during a turn, it reports which ones informed its answer with the `memory_credit` tool. backsight keeps a local record of what was shown and what was used, ids and counts only, never the text of a memory, a prompt or a reply. backsight never writes this section: an older backsight refuses a `config.toml` with keys it does not know, so remove the section before installing an older version.

| Key | Default | Effect |
|---|---|---|
| `feedback.enabled` | `true` | Record which memories are shown and which Claude reports using, and offer Claude the `memory_credit` tool. `false`: nothing is recorded, the tool is not offered, and Claude is asked instead to cite the memories it uses as `🧠 From memory (<project>) — …`. |
| `feedback.confident_rank` | `2` | A memory shown this high or higher in a recall or a search (1 is the top) and not used counts as a miss, when Claude reported what it used that turn. At least 1. |
| `feedback.miss_half_life_days` | `90` | A miss counts half as much after this many days, a quarter after twice as many. `0`: misses never fade. |
| `feedback.credit_half_life_days` | `365` | The same for the times Claude used a memory. `0`: they never fade. |
| `feedback.stop_check` | `"always"` | When the end of a turn asks Claude to report what it used, if it has not: `"always"` whenever the turn showed memories; `"cited"` only when its reply cites a memory or it searched or read the memory itself; `"off"` never. |
| `feedback.max_retirements` | `3` | The most memories Claude may report wrong or outdated in one session (`memory_credit` with `verdict: wrong` or `outdated`), counting every earlier turn of the session, a resumed one included. Past it, the report is refused, nothing is stored, and Claude is told to hand you the `backsight forget <id>` command instead. Undoing a report with `backsight forget --restore` does not give the slot back within the same session. `0`: Claude never reports one. At least 0. |
| `feedback.stale_shows` | `10` | `backsight memory stale` lists a memory shown this many times or more that Claude never used, counting the shows recorded live, not those imported from earlier history. At least 1. |
| `feedback.boost_weight` | `0.1` | How far what Claude reported moves a memory in recall (the boost's weight, k below). `0`: recall ignores it, but still skips hidden memories. At least 0. |
| `feedback.prior_credits` | `3.0` | The uses every memory starts with before anything is counted (α below): the higher, the more counts it takes to move a memory. More than 0. |
| `feedback.prior_misses` | `9.0` | The misses every memory starts with (β below). With `prior_credits`, the share of uses a memory needs to rise: above α / (α + β), 25% by default, it rises; below, it sinks. More than 0. |
| `feedback.share_ledger` | `true` | Share these counts with your other machines through your server: every 15 minutes `backsight jobs tick` sends this machine's (memory ids, numbers and times only, at most once an hour unless a memory was hidden, and once a week even when nothing changed) and fetches the others' every 6 hours, so that recall weighs what Claude used on any of them and a memory hidden on one is hidden on the others, unless restored there later. `false`: this machine neither sends nor reads them; what it sent last stays on the server, and the other machines ignore it after 30 days. |

Every 15 minutes, `backsight jobs tick` turns that record into counts per memory, in `feedback/ledger.json` under the state directory. Each memory shown is counted once its turn is over and it was shown at least 30 minutes ago (or 2 hours ago when the turn never ended), so a use Claude reports a few turns later still counts: as used, as a miss (see `confident_rank`), as shown, or as unknown when Claude never reported what it used that turn (an interrupted turn, for instance), which never counts as a miss. A memory shown in a briefing, opened with `memory_read` or recalled after a failed tool call is never a miss. A use reported after that counts nowhere. `backsight forget <id>` drops a memory's counts.

Recall then adds to each memory's score k × ((uses + α) / (uses + misses + α + β) − α / (α + β)), uses and misses faded by their half-lives: a memory Claude often uses climbs by up to k × β / (α + β) (0.075 by default), one often shown at the top and not used sinks by up to k × α / (α + β) (0.025), and a memory with nothing counted keeps its score. The floors on the server's score (`recall.min_score`, `recall.global_min_score`) apply before the boost, so it reorders memories but never lets one under them in. An observation hidden after Claude reported it wrong or outdated is not recalled on this machine. With `share_ledger` on, recall also adds the other machines' uses and misses, and skips what one of them hid unless `backsight forget --restore <id>` restored it here since; when their counts have not been fetched or cannot be read, it uses this machine's alone. When the ledger cannot be read, recall goes on without boost and without hiding.

## Examples

Quieter recall: fewer, closer memories.

```toml
[recall]
min_score = 0.6
prompt_hits = 2
```

A briefing without LLM calls, and backups on another disk:

```toml
[recall]
briefing = "recent"

[backup]
dir = "/mnt/backup/backsight"
keep_daily = 14
```

Ask Claude what it used only when it cited a memory or looked one up itself:

```toml
[feedback]
stop_check = "cited"
```

A price for another provider's model, so `backsight stats` shows a cost:

```toml
[llm]
price_input = 0.10
price_output = 0.50
```
