# Configuration

Every key of `config.toml`, its default and what it does. Setup writes the file; you rarely need to edit it.

## The file

`config.toml` lives in backsight's config directory (`~/.config/backsight` on Linux; `backsight paths` prints it). Keys you leave out take their default. Unknown keys and invalid values are refused with the key's name, so a typo never goes unnoticed: `backsight doctor` reports it on its `config` line.

Secrets are not in this file: they live in `secrets.toml` next to it, set with `backsight secret set <name>` (see [install.md](install.md#secrets)).

Defaults below are written in TOML. `<data>` stands for backsight's data directory (`~/.local/share/backsight` on Linux) and `<user>` for the name of your home directory.

## When a change applies

| Keys | Applies |
|---|---|
| `recall.*`, `privacy.*`, `components.*`, `projects.*`, `hooks.*` | At the next hook or command: the hooks read the file each time. |
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
| `server.mode` | `"local"` | `"local"`: backsight runs the server in Docker on this machine and manages it. `"remote"`: you run the server elsewhere; backsight only talks to `server.url`, and the commands that manage a local server (`server`, `backup`, `uninstall --stop`) refuse. |
| `server.role` | `"both"` | `"both"`: this PC runs Claude Code, and its server too when `server.mode = "local"`. `"server"`: a host that only serves the memory, without Claude Code; setup will set it for such a host; it needs `server.mode = "local"`. |
| `server.ui_port` | `9999` | Port of the Hindsight web UI, on the same address as the API. Must differ from the API port. |
| `server.compose_dir` | `"<data>/server"` | Where the server's `compose.yaml`, `.env` and your own `compose.override.yaml` live. An absolute path. |
| `server.project` | `"backsight"` | The Docker Compose project name, and the prefix of the server's volumes. Another name runs a second install beside the first (lowercase letters, digits, `-` and `_`). |
| `server.digest` | `""` | Empty: the Hindsight image this release pins ([compatibility.md](compatibility.md)). A `sha256:…` digest holds another image of `ghcr.io/vectorize-io/hindsight`; you then own that choice. |

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

A price for another provider's model, so `backsight stats` shows a cost:

```toml
[llm]
price_input = 0.10
price_output = 0.50
```
