# Privacy

What backsight stores, where it goes, and how to keep things out of memory. Words you may not know are in the [glossary](glossary.md).

## What is stored

- **Your sessions**: the text of your prompts and Claude's replies, with one line per tool call (`Edit src/main.go`, for example). Tool results, thinking and file contents Claude read are not sent. The same for Codex sessions, read from Codex's history in the Codex home backsight wired, and only once you wired it ([Codex](codex.md#what-is-recorded)).
- **Subagent reports**: the final message of each subagent Claude starts, cut at 4,000 characters; a report under `hooks.subagent_report_min_chars` characters (400 by default) with no URL, no issue or pull request reference and no commit hash is not sent.
- **Facts Claude stores** with `memory_store`, and those you store with `backsight store` or `/backsight:remember`.

The server turns this text into memories: short facts tagged with their project, kept with the documents they came from.

## The metrics index

backsight keeps a small database on your machine, `metrics.db` in the `index/` folder of the state directory (`backsight paths` shows it), readable by your user only on Linux and macOS (mode 0600, as are the feedback database `ledger.db` and their companion files). It builds it there, from its own usage events, the feedback journal, the local status files and the local record below (with the other PCs' figures, `machines.json`); `backsight stats index` shows its state. The statistics views and the dashboard read it.

The views (`backsight stats overview`, `quality`, `sessions`…) read this index and make read-only calls to your server. They print counts, times, ids, and project and session names; they never print the text of a memory, a prompt or an error, except `backsight stats memories <id> --text`, which you ask for.

- It holds ids, numbers, fixed words and timestamps only: never the text of a memory, a prompt, an error or the configuration. A value that is not a known word or a plain name is stored as `other`, or not at all.
- Project names and session ids in it are local names (from your folders and Claude Code's session ids). They never leave this machine, `backsight report` included.
- `backsight stats index rebuild` recreates it from the files on disk. `backsight uninstall --purge-local` deletes it.

## The local record

Four kinds of file in the `metrics/` folder of the state directory (`backsight paths` shows it) keep what the server only holds for a while, or does not keep at all. Each is one text file per month (`server-daily-2026-11.jsonl`, for example), one line per record. The metrics index reads them, and `backsight stats index rebuild` builds the index again from them; the daily job (`backsight jobs maintain`) deletes the months older than 13.

| File | What it holds |
|---|---|
| `server-daily-` | Once a day, the server's counts: memories by kind, documents, links, the server's tasks by status, the number of memories per project (the project's name), and per past day the memories created and the tokens the server's language model used. When the server cannot be read, a line says so with a short word for the reason. |
| `doctor-history-` | One line per daily job: the id and the status (ok, info, warning, error) of each doctor check. Not its message, and not the runs you start yourself. |
| `memory-docs-` | For each memory the server lists as created on a day: its id, the day, its kind, and what its source is: for a conversation, its document's id (such as `conversation:<session>@<project>#<n>`, which names the session and its project) and the session's id; for a note, an imported file or anything else, only the word `note`, `import` or `other` (never a file's name or path), or `none`. It also holds the days already listed, and for the sessions this PC sent in the last 7 days, the server's task that stored each one (id, status, times, retries) and how many memories the document gave. |
| `scan-history-` | One line per complete `backsight memory scan`: how many memories it flagged (stored, forgotten and briefing pages), how long it took. No id, no text. |

- Two more files sit beside them. `machines.json` caches the daily figures the other PCs sharing this server sent (see [What your PCs share with each other](#what-your-pcs-share-with-each-other)). `published.json` records this PC's last daily send of its own figures: when, up to which day, its size, and the class of a failure; the server and bank it went to are kept as a hash, to tell a change of server, never as their text.
- They hold ids, numbers, fixed words and times only: never the text of a memory, a prompt, an error or a document, never a file's name or path, never a setting's text or a key. Collecting them only reads the server: the calls are `GET` requests, each cut at 3 seconds.
- They stay on this machine. `backsight report` never includes them, and `backsight uninstall --purge-local` deletes them with the index; a plain `backsight uninstall` keeps them.
- The same daily job also adds one line to the usage log (`telemetry/`) a day, and again when you change a setting: the numeric and on-or-off settings of `[recall]` and `[feedback]`, and a hash of them, so the statistics can tell when a setting changed. A setting that is text is left out.

## The dashboard

`backsight dashboard` serves the memory dashboard in a browser. It listens on `127.0.0.1` only. Its pages make no request outside the dashboard. The dashboard itself reads the memory server you configured (a memory's text only on "Show text") and runs the doctor's checks. The footer's help link opens GitHub only when you click it.

- **What the pages show**: counts, ids, and project and session names, like `backsight stats`. A memory's text shows only on its own page, after a click on "Show text"; it is read from the server at that moment and never stored.
- **Copy summary** copies the Overview's numbers as plain text: counts, rates, statuses and commands, the project name when you filtered by one, the window's dates, and the backsight version and the time it was written, with no memory text and no session or memory id. It names the memory server's address only when that address is on this PC (`localhost` or any loopback address: `127.0.0.1`, `::1`); any other is written "a remote server". The same text is in the page's "Summary as text" disclosure.
- **The browser** (its history, its extensions) sees what the pages show. Use a browser profile you trust.
- **The sign-in cookie** is sent by the browser to every program that answers on the same host name (`127.0.0.1`, or `localhost` if you signed in through it), on any port: browsers do not separate cookies by port. A program that receives it can read every page and any memory's text until the session expires (2 hours without use) or the dashboard stops. Local development servers are the typical case: do not open untrusted local services in the same browser while you are signed in.
- **Over `ssh -L`**, the cookie lives in the browser of the other PC, and other users of that PC can reach the forwarded port. Forward only to a PC you trust.
- **`--open`** puts the one-time link on the browser's command line for a moment. The code works once and expires in 2 minutes.
- **Files**, under `dashboard/` in the state directory: `dashboard.json` (the process id, the port and the start time), `codes/` (the SHA-256 hashes of the one-time codes, each valid 2 minutes; removed when used, when presented after they expired, and when the dashboard starts or stops; beside a code, a `<hash>.claim` file marks the code being spent and is removed right after, or at the next start or stop if a crash left it) and `claim.lock` (an empty file that keeps two starts apart).
- `backsight report` never includes `dashboard/`. `backsight uninstall --purge-local` deletes it.

## What leaves the machine

| To | What | When |
|---|---|---|
| **Your Hindsight server** | Everything above. It runs on your machine, in Docker or natively (the [native runtime](native-runtime.md), the default on Windows), and listens on `127.0.0.1` only, unless you open it to your other PCs on a private address (`backsight server expose --private`); its API and web UI always require a key. | Each session, report and stored fact. |
| **The server's host**, on a PC that joined another's server ([below](#several-pcs)) | The same, over your private network, with recall queries and their results, and the API key in every request. | Each session, report and stored fact, each prompt and memory search, each `jobs tick`. |
| **The LLM provider you chose at setup** | Session text, reports and stored facts, which the server sends it to extract and consolidate memories, and the server's own summaries (the briefing pages). With `ollama`, nothing: the model runs on your machine. | In the background, after each send. |
| **Hugging Face** (`huggingface.co`) | Nothing of yours: the server downloads its embedding model and its reranker from there, into a Docker volume (on the native runtime, into `server/native/hf`). | The server's first start (native: its install), and after its model cache is deleted. |
| **The image registries** (`ghcr.io` for Hindsight, Docker Hub for pgvector) | Nothing of yours: Docker pulls the server's images, by digest. | Setup, and server upgrades. |
| **The native runtime's sources**, on the native runtime only | Nothing of yours: backsight downloads the pinned uv from GitHub and checks its SHA-256; uv downloads the pinned Python from its own source for Python builds, and Hindsight's packages from PyPI, checked against the hashes of backsight's lock. | Setup, and server upgrades. |
| **GitHub** | Nothing of yours: the install script and `update` download releases, and the binary checks once a day for a newer one. | Install, updates, the daily check. |

Setup shows the provider data flow and asks for your consent before anything is stored. Nothing else leaves the machine:

- backsight's own logs and usage events stay in local files (`backsight paths` shows the state directory); they are never sent. So does the record of which project each turn was filed under and which repositories its tool calls named (project names and counts, no text), which files subagent reports and shows how often the main conversation would have been filed elsewhere;
- the feedback journal and ledger (`feedback/` in the state directory that `backsight paths` shows) record which memories were shown and credited: memory ids, ranks, outcomes and counts only, never memory or conversation text. The counts also sit in `feedback/ledger.db`, a local database derived from the journal and rebuilt from it at any time: memory ids, the document id of a correction note, session and turn ids (in the keys that keep a show from being counted twice), the verdict word (`wrong` or `outdated`), numbers and times, never a memory, a prompt, an error, a path or a setting. They stay local, and `backsight uninstall --purge-local` removes them. With `feedback.share_ledger` on (the default), `backsight jobs tick` copies the ledger's counts to one document per machine in your server's `<bank>-control` bank, for your other machines to weigh: memory ids, numbers (uses, misses) and times only, plus a random id of the machine, never text, a session or a project. `share_ledger = false` stops it; the last copy stays on the server, and your other machines ignore it after 30 days. `backsight uninstall --purge-local` deletes this machine's copy from the server first (when the server does not answer within 5 seconds, it stays and is ignored after 30 days). The same setting shares this PC's daily figures: see [What your PCs share with each other](#what-your-pcs-share-with-each-other);
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

- **a connection token** (`bst1.…`), whole: it holds the server's API key;
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
| This session, from now on | `/backsight:pause` in Claude Code, `$backsight:pause` in Codex (or `backsight pause`); `/backsight:pause` again with "resume", or `backsight resume`, to record again. Turns typed while paused are never sent, and leave nothing in the feedback journal. |
| A project, on this machine | `backsight pause --project` in its directory: adds it to `privacy.exclude_paths` in `config.toml`. `backsight resume --project` removes it. |
| A repository, for everyone who clones it | commit a `.backsight.toml` with `retain = false` at its root. |
| Headless runs, on this machine ([headless Codex](codex.md#headless-codex)) | `privacy.skip_entrypoints` in `config.toml`: turns Claude Code recorded under one of these entrypoints (glob patterns) are never sent. The default, `["sdk-*", "codex_exec", "codex_sdk_*", "codex-app-server-sdk", "codex_mcp_server"]`, skips headless runs: Claude Code's Agent SDK (`sdk-cli`, `sdk-ts`, `sdk-py`), `codex exec`, Codex's SDKs, and Codex run as another agent's tool (its MCP server); `[]` sends them too. A `config.toml` that sets the key keeps its own value; only the former default, exactly `["sdk-*"]`, becomes today's on `backsight update apply`. |

An excluded project gets no briefing either.

doctor's `noise-headless` check names the projects that got headless sessions sent before they were skipped, for `forget --project` below.

## Removing what was stored

- `backsight forget <id>` retires a memory; recall stops returning it, and `--restore` brings it back.
- `backsight forget <id> --purge --yes` deletes the memory, its source facts and the documents they came from (for a session, every part of its document; while the server is still storing a part of it, the purge deletes nothing and asks you to run it again in a few minutes). Backups made before the purge still hold them; the command lists those backups, which you can delete yourself.
- `backsight forget --project <name> --plan` lists what a whole project holds in the bank: its documents and its memory counts. Without `--purge`, `backsight forget --project <name>` then retires every raw fact of it (each can be brought back with `backsight forget <id> --restore`); with `--purge`, it deletes those documents and the project's observations for good, and lists the older backups that still hold them. `--source <s>` keeps to one source (`chat`, `subagent`, `note`) and the observations drawn from it. Either way it asks you to type the project name: `--answer confirm_project=<name>`.

Memory is deleted only by these commands, with one exception: `jobs maintain` deletes, once, the stored subagent reports that fail the rule above (shorter than `hooks.subagent_report_min_chars`, with no URL, no issue or pull request reference and no commit hash), as `forget --purge` does but without tombstones, after the daily backup that still holds them, and again only when you change that setting or restore the server ([how it works](how-it-works.md#subagents)).

A purge stays a purge. Before deleting, it writes a **[tombstone](glossary.md#tombstone)** for each document: its id, its session and project, and the last turn it held (for a note, a hash of its text, never the text). Tombstones are kept for good in `state.json` and copied to the document `backsight:tombstones` of the `<bank>-control` bank, so your other machines and a reinstall know them too. The turns a tombstone covers are never sent again from your transcripts: not after a restore, not by `retain retry-quarantine`, not by `retain backfill`, and the saves made in them are not replayed. Later turns of the same session are still sent, to a new document (`conversation:<session>#2`, then `#3` after another purge): a purged document id is never written again, so a restore that brings the purged document back deletes it again without losing them. If the control bank cannot be written at that moment, the purge says so, and the next `jobs tick` writes the tombstone there.

A client that does not know about tombstones (an older backsight on another machine, a rollback to an older release) can still append to a purged session's id, and the server then recreates the document with what it sent. `jobs tick` looks for that every 15 minutes among the documents created since its last look (one request), and `jobs maintain` once a day for every tombstone (one request per tombstone, or per 200 documents of the bank when that is fewer). Either purges such a document again, with the facts drawn from what was appended; the turns written after the purge that it held go again to the session's current document, so they are not lost, while a turn written before the purge never goes again, even if its send was still queued when you purged. It waits while an append to the document is queued, never touches a later document of the session (`#2`) unless that one was purged too, and purges one document at most 3 times a day: past that an older client keeps writing to it, and `backsight doctor` asks you to update it. With the `claude-code` provider, or a server this machine does not run, it waits for `backsight retain purge-again --allow-subscription`, since the server consolidates again after a purge; until then the recreated document can be recalled, and `backsight doctor` names it.

## Secrets on disk

Keys live only in `secrets.toml` (backsight's config directory) and the server's rendered `.env`, both readable by your user only (mode 0600; on Windows an access list: you, SYSTEM and Administrators only), and in a connection token file until you delete it ([Several PCs](#several-pcs)). They never appear in command lines or answers: you type them at a prompt with `backsight secret set <name>`. Claude is told never to ask for a key in the conversation.

## Several PCs

When your PCs share one memory ([several-pcs.md](several-pcs.md)), each PC that joined (a client) talks to the server on another machine (the host):

- **What travels** between a client and the host: the text of its sessions and subagent reports, the notes stored on it, its recall queries and the memories they return, briefing pages, tombstones, and the shared counts of which memories Claude used or hid (`feedback.share_ledger`: memory ids, numbers and times only).
- **The API key goes with every request.** On a tailnet address (Tailscale), WireGuard encrypts the link, the key included. On a LAN address, the key travels in clear, readable by anyone on that network: that is why `backsight server expose` asks for `--answer cleartext_lan=yes` first, and doctor's `server` check warns on each client of such an address. The web UI, opened from another PC at the host's private address, takes its own key the same way.
- **The connection token holds the API key** (never the LLM key). `backsight server token --out <file>` writes it readable by you only (mode 0600; on Windows an access list: you, SYSTEM and Administrators only, set before the token is written); `backsight connect` reads it from a file or stdin and refuses it as an argument, where your shell history would keep it. Redaction masks a token before a session or note is stored, and `backsight report` and the errors a command prints mask it too. If it leaked anyway (pasted into a chat, a lost laptop), give the server a new key on the host: `backsight server token --rotate --out <file>`, then `backsight connect --token-file <file>` on each PC ([A lost laptop](several-pcs.md#a-lost-laptop)).
- **`backsight server install --ssh`** sends the host's LLM key on the stdin of `backsight secret set` there, over your ssh connection, never on a command line. The token comes back the same way and is neither shown nor written on this PC.
- **Whoever runs the host reads every PC's memories**: the server, its web UI and its backups hold them all, as on a server of your own. Share a host only with people you would show all your sessions to.

## What your PCs share with each other

With feedback and `feedback.share_ledger` on (both are the default), `backsight jobs tick` puts two documents per PC in your server's `<bank>-control` bank. The shared counts and the daily figures are both read by your other PCs.

- **The shared counts**, as described under [What leaves the machine](#what-leaves-the-machine): memory ids, numbers and times, and a random id of the PC. They are sent at most once an hour (unless a memory was hidden) and once a week even when nothing changed.
- **The daily figures**, once a UTC day, from `jobs tick`: a PC that has never sent them sends at once, and after that not before a minute between 00:00 and 01:00 UTC that differs on each PC (a failed try is made again an hour later). They are counts per UTC day, for up to the last 400 days: typed prompts, prompts that got memory, turns where Claude used memory and turns where it declared whether it did, wrong and outdated verdicts, characters of context added and sessions, sessions sent, and how many prompts fell in each of nine time buckets of the recall's delay; the same counts per backsight version that ran that day (at most 8 versions). With them go the operating system (Linux, macOS, Windows or other), the backsight version, a random machine id and the day this PC first sent them. The document is capped at 256 KiB, and a PC with no figures yet sends nothing.
- **What this PC reads**: every 6 hours `jobs tick` fetches the other PCs' daily figures into `metrics/machines.json` in the state directory, and the activity index keeps their numbers, dates, system names, version numbers and machine ids for `backsight stats --machines all` ([several-pcs.md](several-pcs.md#see-all-your-pcs-activity)). A document that cannot be read, is over 1 MiB or is not in the format is set aside and counted, never stored; one not renewed for 30 days is dropped. Reading stops when sharing stops: with feedback or `share_ledger` off, this PC fetches nothing and `--machines all` answers this PC's. The PCs on one server trust each other's figures, as they do the shared counts: a PC that writes false numbers to the control bank skews the merged figures, and nothing here signs them. Whoever can write to the bank is trusted not to.
- **What is never in them**: a memory id, a session, a project name, a path, a prompt or memory text, an error text, a setting, the computer's name or your user name. Every field is a number, a date, an operating-system name, the machine id or a version number; a version with characters other than letters, digits and `. + _ -`, or longer than 64 characters, is sent as `other` and left out of the per-version counts.
- **Where it goes**: the document `backsight:metrics:<machine id>` of `<bank>-control`, beside `backsight:feedback:<machine id>`. Whoever runs the server can read it, as they can read everything else on it.
- **Stopping it**: `share_ledger = false` in `[feedback]` (or feedback off) stops both documents. The last copy of each stays on the server. `backsight uninstall --purge-local` deletes this PC's copy from the server first, as it does the shared counts.

## Backups

Backups contain your whole memory and the server's secrets, so they are always encrypted with [age](https://age-encryption.org/), to the identity setup generated or to your own recipient. See [backups-and-restore.md](backups-and-restore.md).

## The Claude subscription lane

The `claude-code` provider has the server extract memories through your Claude subscription, with a token from `claude setup-token` that backsight stores for the server. It works, but Anthropic's terms are against it. The [Claude Code legal and compliance page](https://code.claude.com/docs/en/legal-and-compliance) says that developers building products "should use API key authentication", and that they "may not collect, store, or intermediate Claude.ai credentials or session tokens"; Anthropic may enforce this without notice. The [consumer terms](https://www.anthropic.com/legal/consumer-terms) (section 3) also forbid accessing the services "through automated or non-human means, whether through a bot, script, or otherwise", except with an API key or where Anthropic allows it.

So **use an Anthropic API key instead** (the `anthropic` provider): it is the supported way, billed separately from your plan, and about $0.04 a session with Claude Haiku 5.5. If you keep the subscription lane, you accept the risk to your account; its usage also counts against your plan, and backfill on it is refused unless you pass `--allow-subscription`.
