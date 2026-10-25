# Privacy

What backsight stores, where it goes, and how to keep things out of memory.

## What is stored

- **Your sessions**: the text of your prompts and Claude's replies, with one line per tool call (`Edit src/main.go`, for example). Tool results, thinking and file contents Claude read are not sent.
- **Subagent reports**: the final message of each subagent Claude starts, cut at 4,000 characters.
- **Facts Claude stores** with `memory_store`, and those you store with `backsight store` or `/backsight:remember`.

The server turns this text into memories: short facts tagged with their project, kept with the documents they came from.

## Where it goes

- **Your Hindsight server**, which runs in Docker on your machine and listens on `127.0.0.1` only. Its API and web UI always require a key.
- **The LLM provider you chose at setup**: the server sends it session text to extract memories. With `ollama`, nothing leaves your machine. Setup shows this data flow and asks for your consent before anything is stored.

Nothing else leaves the machine:

- backsight's own logs and usage events stay in local files (`backsight paths` shows the state directory); they are never sent;
- `backsight report` builds a diagnostic for a bug report but sends nothing: you read it and decide;
- the binary also contacts GitHub once a day to check for releases, with no memory or usage data.

## Redaction

Before anything is written to memory, backsight replaces secrets with `[redacted:<kind>]`: private keys, known token prefixes (`sk-`, `sk-ant-`, `ghp_`, `github_pat_`, `AKIA`, JWTs and others), bearer headers, passwords in URLs, `KEY=secret` lines, and high-entropy values next to secret-like names. This applies to session text, subagent reports and stored facts. `memory_store` and `backsight store` refuse text that still looks like a secret.

Redaction can miss a secret in an unusual format. `backsight memory scan` re-checks what is already stored and gives the purge command for each match.

## Keeping things out of memory

| Scope | How |
|---|---|
| This session, from now on | `/backsight:pause` in Claude Code (or `backsight pause`); `/backsight:pause` again with "resume", or `backsight resume`, to record again. Turns typed while paused are never sent. |
| A project, on this machine | `backsight pause --project` in its directory: adds it to `privacy.exclude_paths` in `config.toml`. `backsight resume --project` removes it. |
| A repository, for everyone who clones it | commit a `.backsight.toml` with `retain = false` at its root. |

An excluded project gets no briefing either.

## Removing what was stored

- `backsight forget <id>` retires a memory; recall stops returning it, and `--restore` brings it back.
- `backsight forget <id> --purge --yes` deletes the memory, its source facts and the documents they came from. Backups made before the purge still hold them; the command lists those backups, which you can delete yourself.

## Secrets on disk

Keys live only in `secrets.toml` (backsight's config directory) and the server's rendered `.env`, both readable by your user only (mode 0600). They never appear in command lines or answers: you type them at a prompt with `backsight secret set <name>`. Claude is told never to ask for a key in the conversation.

## Backups

Backups contain your whole memory and the server's secrets, so they are always encrypted with [age](https://age-encryption.org/), to the identity setup generated or to your own recipient. See [backups-and-restore.md](backups-and-restore.md).

## The Claude subscription lane

With the `claude-code` provider, the server extracts memories through your Claude subscription. Anthropic's terms restrict routing subscription credentials through third-party tools and automated use, and the usage counts against your plan. Backfill on this lane is refused unless you pass `--allow-subscription`.
