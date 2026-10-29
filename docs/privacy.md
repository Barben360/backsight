# Privacy

What backsight stores, where it goes, and how to keep things out of memory. Words you may not know are in the [glossary](glossary.md).

## What is stored

- **Your sessions**: the text of your prompts and Claude's replies, with one line per tool call (`Edit src/main.go`, for example). Tool results, thinking and file contents Claude read are not sent.
- **Subagent reports**: the final message of each subagent Claude starts, cut at 4,000 characters.
- **Facts Claude stores** with `memory_store`, and those you store with `backsight store` or `/backsight:remember`.

The server turns this text into memories: short facts tagged with their project, kept with the documents they came from.

## What leaves the machine

| To | What | When |
|---|---|---|
| **Your Hindsight server** | Everything above. It runs in Docker on your machine and listens on `127.0.0.1` only, unless you open it to your other PCs on a private address (`backsight server expose --private`); its API and web UI always require a key. | Each session, report and stored fact. |
| **The LLM provider you chose at setup** | Session text, reports and stored facts, which the server sends it to extract and consolidate memories, and the server's own summaries (the briefing pages). With `ollama`, nothing: the model runs on your machine. | In the background, after each send. |
| **Hugging Face** (`huggingface.co`) | Nothing of yours: the server downloads its embedding model and its reranker from there, into a Docker volume. | The server's first start, and after its model cache is deleted. |
| **The image registries** (`ghcr.io` for Hindsight, Docker Hub for pgvector) | Nothing of yours: Docker pulls the server's images, by digest. | Setup, and server upgrades. |
| **GitHub** | Nothing of yours: the install script and `update` download releases, and the binary checks once a day for a newer one. | Install, updates, the daily check. |

Setup shows the provider data flow and asks for your consent before anything is stored. Nothing else leaves the machine:

- backsight's own logs and usage events stay in local files (`backsight paths` shows the state directory); they are never sent. So does the record of which project each turn was filed under and which repositories its tool calls named (project names and counts, no text), which files subagent reports and shows how often the main conversation would have been filed elsewhere;
- the feedback journal and ledger (`feedback/` in the state directory that `backsight paths` shows) record which memories were shown and credited: memory ids, ranks, outcomes and counts only, never memory or conversation text. They stay local, and `backsight uninstall --purge-local` removes them. With `feedback.share_ledger` on (the default), `backsight jobs tick` copies the ledger's counts to one document per machine in your server's `<bank>-control` bank, for your other machines to weigh: memory ids, numbers (uses, misses) and times only, plus a random id of the machine, never text, a session or a project. `share_ledger = false` stops it; the last copy stays on the server, and your other machines ignore it after 30 days. `backsight uninstall --purge-local` deletes this machine's copy from the server first (when the server does not answer within 5 seconds, it stays and is ignored after 30 days);
- `backsight report` builds a diagnostic for a bug report but sends nothing: you read it and decide;
- backsight turns on none of Hindsight's optional tracing or metrics exporters.

## Your LLM provider

The provider receives the text of your sessions and handles it under its own terms, which you accept with your account: read them, and your employer's policy before you send work sessions.

| Provider | Where your data is processed and stored | Terms | Privacy |
|---|---|---|---|
| `deepseek` | **In the People's Republic of China**, as DeepSeek's privacy policy says. | [Open platform terms](https://cdn.deepseek.com/policies/en-US/deepseek-open-platform-terms-of-service.html) | [Privacy policy](https://cdn.deepseek.com/policies/en-US/deepseek-privacy-policy.html) |
| `anthropic` | As Anthropic's commercial terms set. | [Commercial terms](https://www.anthropic.com/legal/commercial-terms) | [Privacy policy](https://www.anthropic.com/legal/privacy) |
| `openai` | As OpenAI's services agreement sets. | [Services agreement](https://openai.com/policies/services-agreement/) | [Privacy policy](https://openai.com/policies/privacy-policy/) |
| `ollama` | On your machine: nothing is sent. | The model's licence ([Qwen3 8B](https://huggingface.co/Qwen/Qwen3-8B): Apache-2.0) | n/a |
| `claude-code` | Anthropic, under your Claude plan's consumer terms; see [the subscription lane](#the-claude-subscription-lane). | [Consumer terms](https://www.anthropic.com/legal/consumer-terms) | [Privacy policy](https://www.anthropic.com/legal/privacy) |
| A compatible endpoint (`--answer base_url=…`) | Wherever its operator runs it. | Its operator's | Its operator's |

The third-party software and models the server runs come under their own licences: see [Third-party components](../README.md#third-party-components) in the README.

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
| This session, from now on | `/backsight:pause` in Claude Code (or `backsight pause`); `/backsight:pause` again with "resume", or `backsight resume`, to record again. Turns typed while paused are never sent, and leave nothing in the feedback journal. |
| A project, on this machine | `backsight pause --project` in its directory: adds it to `privacy.exclude_paths` in `config.toml`. `backsight resume --project` removes it. |
| A repository, for everyone who clones it | commit a `.backsight.toml` with `retain = false` at its root. |
| Headless runs, on this machine | `privacy.skip_entrypoints` in `config.toml`: turns Claude Code recorded under one of these entrypoints (glob patterns) are never sent. The default, `["sdk-*"]`, skips Agent SDK runs (`sdk-cli`, `sdk-ts`, `sdk-py`); `[]` sends them too. |

An excluded project gets no briefing either.

doctor's `noise-headless` check names the projects that got headless sessions sent before they were skipped, for `forget --project` below.

## Removing what was stored

- `backsight forget <id>` retires a memory; recall stops returning it, and `--restore` brings it back.
- `backsight forget <id> --purge --yes` deletes the memory, its source facts and the documents they came from. Backups made before the purge still hold them; the command lists those backups, which you can delete yourself.
- `backsight forget --project <name> --plan` lists what a whole project holds in the bank: its documents and its memory counts. Without `--purge`, `backsight forget --project <name>` then retires every raw fact of it (each can be brought back with `backsight forget <id> --restore`); with `--purge`, it deletes those documents and the project's observations for good, and lists the older backups that still hold them. `--source <s>` keeps to one source (`chat`, `subagent`, `note`) and the observations drawn from it. Either way it asks you to type the project name: `--answer confirm_project=<name>`.

A purge stays a purge. Before deleting, it writes a **[tombstone](glossary.md#tombstone)** for each document: its id, its session and project, and the last turn it held (for a note, a hash of its text, never the text). Tombstones are kept for good in `state.json` and copied to the document `backsight:tombstones` of the `<bank>-control` bank, so your other machines and a reinstall know them too. The turns a tombstone covers are never sent again from your transcripts: not after a restore, not by `retain retry-quarantine`, not by `retain backfill`, and the saves made in them are not replayed. Later turns of the same session are still sent, to a new document (`conversation:<session>#2`, then `#3` after another purge): a purged document id is never written again, so a restore that brings the purged document back deletes it again without losing them. If the control bank cannot be written at that moment, the purge says so, and the next `jobs tick` writes the tombstone there.

A client that does not know about tombstones (an older backsight on another machine, a rollback to an older release) can still append to a purged session's id, and the server then recreates the document with what it sent. `jobs tick` looks for that every 15 minutes among the documents created since its last look (one request), and `jobs maintain` once a day for every tombstone (one request per tombstone, or per 200 documents of the bank when that is fewer). Either purges such a document again, with the facts drawn from what was appended; the turns written after the purge that it held go again to the session's current document, so they are not lost, while a turn written before the purge never goes again, even if its send was still queued when you purged. It waits while an append to the document is queued, never touches a later document of the session (`#2`) unless that one was purged too, and purges one document at most 3 times a day: past that an older client keeps writing to it, and `backsight doctor` asks you to update it. With the `claude-code` provider, or a server this machine does not run, it waits for `backsight retain purge-again --allow-subscription`, since the server consolidates again after a purge; until then the recreated document can be recalled, and `backsight doctor` names it.

## Secrets on disk

Keys live only in `secrets.toml` (backsight's config directory) and the server's rendered `.env`, both readable by your user only (mode 0600). They never appear in command lines or answers: you type them at a prompt with `backsight secret set <name>`. Claude is told never to ask for a key in the conversation.

## Backups

Backups contain your whole memory and the server's secrets, so they are always encrypted with [age](https://age-encryption.org/), to the identity setup generated or to your own recipient. See [backups-and-restore.md](backups-and-restore.md).

## The Claude subscription lane

The `claude-code` provider has the server extract memories through your Claude subscription, with a token from `claude setup-token` that backsight stores for the server. It works, but Anthropic's terms are against it. The [Claude Code legal and compliance page](https://code.claude.com/docs/en/legal-and-compliance) says that developers building products "should use API key authentication", and that they "may not collect, store, or intermediate Claude.ai credentials or session tokens"; Anthropic may enforce this without notice. The [consumer terms](https://www.anthropic.com/legal/consumer-terms) (section 3) also forbid accessing the services "through automated or non-human means, whether through a bot, script, or otherwise", except with an API key or where Anthropic allows it.

So **use an Anthropic API key instead** (the `anthropic` provider): it is the supported way, billed separately from your plan, and about $0.04 a session with Claude Haiku 5.5. If you keep the subscription lane, you accept the risk to your account; its usage also counts against your plan, and backfill on it is refused unless you pass `--allow-subscription`.
