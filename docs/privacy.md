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

Before anything is written to memory, backsight replaces secrets with `[redacted:<kind>]`:

- **backsight's own secrets**: the exact values of `secrets.toml` and of the secret-named entries of the server's `.env`, in their URL-escaped forms too, whatever their shape; and any age identity (`AGE-SECRET-KEY-1…`), the shape of your backup key;
- **private keys**: PEM blocks (`-----BEGIN … PRIVATE KEY-----`) and PGP private key blocks, whole;
- **known token shapes**: `sk-` and `sk-ant-`, Stripe (`sk_live_`, `rk_test_`…), GitHub (`ghp_`, `github_pat_`…), GitLab (`glpat-`…), npm, Slack (`xoxb-`…), AWS (`AKIA`, `ASIA`), Google (`AIza`), DigitalOcean (`dop_v1_`, `doo_v1_`, `dor_v1_`), Shopify (`shpat_`, `shpca_`, `shppa_`, `shpss_`), Twilio API key SIDs (`SK` and 32 hex), JWTs, and Slack and Discord webhook URLs;
- **authorization**: Bearer, Basic and other `Authorization:` schemes, and a Docker config's `"auth"` (base64 of user:password);
- **passwords in URLs** (an empty user included) and in netrc entries (`machine … login … password …`, `default login … password …`);
- **values of secret-named keys** (`KEY=secret`, `key: secret`, JSON): `password`, `pass`, `pw`, `passphrase`, `secret`, `token`, `api key`, `private_key`, `access_key`, `credentials` and their variants, a short plain password after a password or token key included; and random-looking values (32 hex characters or more, or base64 with `+`, `/`, `=` or percent escapes) of keys ending in `key`, `auth`, `sig`, `signature` or `identity` (`AccountKey=`, SAS `sig=`, `X-Amz-Signature=`, `client-key-data:`, `TWILIO_AUTH=`);
- **long hex after the words naming a secret** ("the token is …"), and TOTP seeds (upper-case base32) in `otpauth://` URLs or after `TOTP secret`, `2FA seed` and the like;
- **password options of command lines**: `--password`, `--pass`, `--passphrase`, `--pw`, `--secret`, `--token`, `curl -u`, `mysql -p`, `sshpass -p`, `redis-cli -a`, `-U user%password`, and `-p` of `docker login` (podman, nerdctl, buildah, skopeo, oras and `helm registry login` too);
- **high-entropy tokens anywhere**: 32 characters or more mixing upper case, lower case and digits.

Bare hex stays, because commits, sha256 digests and checksums look the same; so do UUIDs. A password written in a sentence ("my password is …") is not recognised.

This applies to session text, tool action lines, subagent reports, stored facts and recall queries. `memory_store` and `backsight store` refuse text that still looks like a secret.

Redaction can miss a secret in an unusual format. `backsight memory scan` re-checks what is already stored, with the same rules, and gives the purge command for each match.

## Keeping things out of memory

| Scope | How |
|---|---|
| This session, from now on | `/backsight:pause` in Claude Code (or `backsight pause`); `/backsight:pause` again with "resume", or `backsight resume`, to record again. Turns typed while paused are never sent. |
| A project, on this machine | `backsight pause --project` in its directory: adds it to `privacy.exclude_paths` in `config.toml`. `backsight resume --project` removes it. |
| A repository, for everyone who clones it | commit a `.backsight.toml` with `retain = false` at its root. |
| Headless runs, on this machine | `privacy.skip_entrypoints` in `config.toml`: turns Claude Code recorded under one of these entrypoints (glob patterns) are never sent. The default, `["sdk-*"]`, skips Agent SDK runs (`sdk-cli`, `sdk-ts`, `sdk-py`); `[]` sends them too. |

An excluded project gets no briefing either.

doctor's `noise-headless` check names the projects that got headless sessions sent before they were skipped, for `forget --project` below.

## Removing what was stored

- `backsight forget <id>` retires a memory; recall stops returning it, and `--restore` brings it back.
- `backsight forget <id> --purge --yes` deletes the memory, its source facts and the documents they came from. Backups made before the purge still hold them; the command lists those backups, which you can delete yourself.
- `backsight forget --project <name> --plan` lists what a whole project holds in the bank: its documents and its memory counts. Without `--purge`, `backsight forget --project <name>` then retires every raw fact of it (each can be brought back with `backsight forget <id> --restore`); with `--purge`, it deletes those documents and the project's observations for good, and lists the older backups that still hold them. `--source <s>` keeps to one source (`chat`, `subagent`, `note`) and the observations drawn from it. Either way it asks you to type the project name: `--answer confirm_project=<name>`.

A purge stays a purge. Before deleting, it writes a **tombstone** for each document: its id, its session and project, and the last turn it held (for a note, a hash of its text, never the text). Tombstones are kept for good in `state.json` and copied to the document `backsight:tombstones` of the `<bank>-control` bank, so your other machines and a reinstall know them too. The turns a tombstone covers are never sent again from your transcripts: not after a restore, not by `retain retry-quarantine`, not by `retain backfill`, and the saves made in them are not replayed. Later turns of the same session are still sent, to a new document (`conversation:<session>#2`, then `#3` after another purge): a purged document id is never written again, so a restore that brings the purged document back deletes it again without losing them. If the control bank cannot be written at that moment, the purge says so, and the next `jobs tick` writes the tombstone there.

## Secrets on disk

Keys live only in `secrets.toml` (backsight's config directory) and the server's rendered `.env`, both readable by your user only (mode 0600). They never appear in command lines or answers: you type them at a prompt with `backsight secret set <name>`. Claude is told never to ask for a key in the conversation.

## Backups

Backups contain your whole memory and the server's secrets, so they are always encrypted with [age](https://age-encryption.org/), to the identity setup generated or to your own recipient. See [backups-and-restore.md](backups-and-restore.md).

## The Claude subscription lane

With the `claude-code` provider, the server extracts memories through your Claude subscription. Anthropic's terms restrict routing subscription credentials through third-party tools and automated use, and the usage counts against your plan. Backfill on this lane is refused unless you pass `--allow-subscription`.
