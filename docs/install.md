# Install

How to install backsight and set it up for Claude Code. The short path is in the [README](../README.md#before-you-start); words you may not know are in the [glossary](glossary.md).

## Requirements

- **Linux**, amd64 (the arm64 binary is built but not tested). macOS and Windows binaries are built but not tested yet.
- **Docker**, usable by your user without `sudo`, with **Docker Compose v2** (the `docker compose` plugin, not the old `docker-compose`):
  - install Docker Engine: [docs.docker.com/engine/install](https://docs.docker.com/engine/install/) (on Debian and Ubuntu, the distribution's `docker.io` package with its Compose v2 package works too);
  - let your user run it: `sudo usermod -aG docker $USER`, then log out and back in ([Linux post-install steps](https://docs.docker.com/engine/install/linux-postinstall/)). Members of the `docker` group have root-level access to the machine; [rootless Docker](https://docs.docker.com/engine/security/rootless/) avoids that and works too;
  - check with `docker compose version`, without `sudo`.
- **Claude Code** 2.1.309 or newer, with the `claude` command on your PATH: [install Claude Code](https://code.claude.com/docs/en/setup).
- **An LLM provider for the memory server**, which extracts memories from your sessions ([cost](../README.md#what-it-costs), [terms and data location](privacy.md#your-llm-provider)):
  - an API key from a pay-as-you-go account: [DeepSeek](https://platform.deepseek.com/api_keys) (top up a few dollars first), [Anthropic](https://platform.claude.com/settings/keys) or [OpenAI](https://platform.openai.com/api-keys), or a compatible endpoint;
  - or [Ollama](https://ollama.com/download) on this machine, with no key (on Linux it must listen on an address the server container reaches: see [Local models with Ollama](#local-models-with-ollama));
  - or your Claude subscription, which Anthropic's terms are against: see [the subscription lane](privacy.md#the-claude-subscription-lane).
- About **6 GB of free disk** for the server image and its models.
- `curl`, `tar`, and `minisign` or OpenSSL 3 to check the release signature.

The exact versions each release pins are in [compatibility.md](compatibility.md).

## Install the binary

```sh
curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.sh | sh
```

The script:

1. finds the newest release, pre-releases included, through GitHub's API (no account or token needed), and downloads it for your OS and architecture;
2. checks the signature of its `checksums.txt` with the release key written in the script, then the archive's checksum, and stops if either fails;
3. installs the binary under backsight's data directory, with the signed checksums next to it (the binary checks itself against them on its first run);
4. links `~/.local/bin/backsight` to a [shim](glossary.md#shim) that always points at the current version.

If `~/.local/bin` is not on your PATH, the script prints the `export PATH=…` line to add to your shell profile.

To install one given release instead of the newest, set its download URL:

```sh
curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.sh | BACKSIGHT_RELEASE_URL=https://github.com/Barben360/backsight/releases/download/<tag> sh
```

If the script cannot list the releases (no network to `api.github.com`, or GitHub's limit of 60 requests an hour for an address without a token), it stops and names `BACKSIGHT_RELEASE_URL`: set it as above, with a tag from the [releases page](https://github.com/Barben360/backsight/releases).

To install from release files you downloaded yourself (`checksums.txt`, `checksums.txt.minisig` and the archive in one directory), run `BACKSIGHT_RELEASE_DIR=<dir> sh install.sh`. Nothing is downloaded then.

## Run setup

```sh
backsight setup
```

Setup checks the machine, asks a few questions, shows its plan, and asks for your consent before changing anything. Then it:

1. writes backsight's config and generates the server's keys;
2. renders and starts the Hindsight server in Docker, on `127.0.0.1` only;
3. configures the memory bank;
4. checks that your LLM provider can extract a memory;
5. indexes these docs into a docs [bank](glossary.md#bank) for `backsight guide search`;
6. wires Claude Code: a plugin with the hooks, the memory tools and the skills, plus a permission rule for the memory tools;
7. sets how long Claude Code keeps its transcripts, as you answered;
8. installs the background jobs (a timer every 15 minutes and a daily one) and, with backups on, generates your backup identity, the key backups are encrypted to;
9. runs `backsight verify`: every hook and the memory tools as Claude Code runs them, then a store-and-recall round trip;
10. unwires an older Hindsight integration from Claude Code, if you chose to (see below);
11. queues your past sessions for import ([backfill](glossary.md#backfill)), if you accepted, and starts the first `jobs tick` at once so the import begins before setup ends.

The first run downloads about 2.4 GB: the server images (about 1.2 GB, with Docker's progress shown), then the embedding models at the server's first start (about 1.2 GB; setup prints the time spent while it waits). Expect several minutes on a home connection. Every step is journaled: if setup stops, `backsight setup --resume` continues where it stopped, the server it started included.

A rerun of `backsight setup` reuses your earlier answers and lists them under the plan. To change them, run `backsight setup --reconfigure`: it asks every question again, your earlier answers as the defaults (Enter keeps one). Or give one answer with `--answer id=value`, for example `--answer provider=openai`. Once the server is installed, the provider, model and backups answers live in `config.toml`: edit them there.

When it finishes, restart Claude Code (or run `/reload-plugins`) so the plugin loads.

## Setup questions

| Question | Choices | Default |
|---|---|---|
| `provider`: the LLM that extracts memories | `deepseek`, `anthropic`, `openai`, `ollama`, `claude-code` | `deepseek` |
| `model` | depends on the provider | the provider's preset |
| `retention`: how long Claude Code keeps transcripts | `3650` days, or `keep` (leave Claude Code's setting) | `3650` |
| `backups`: daily encrypted backups | `on`, `off` | `on` |
| `reference`, only when another Hindsight integration is wired: unwire it | `retire`, `keep` | `retire` |
| `backfill`, only when Claude Code keeps past sessions: import them in the background | `yes`, `no` | `yes` (`no` with `claude-code`) |
| `consent`: go ahead with the plan | `yes`, `no` | none, you must answer |

Notes on the choices:

- **deepseek** is the default (model `deepseek-flash`, DeepSeek V4.1 Flash, not the Pro model) and among the cheapest; DeepSeek processes and stores data **in the People's Republic of China**. **anthropic** (Claude Haiku 5.5) costs about the same per session. **ollama** keeps everything on your machine, with lower quality memories and slow extraction on a CPU. **claude-code** uses your Claude subscription: Anthropic's terms say developers "may not collect, store, or intermediate Claude.ai credentials or session tokens" ([legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)), so an Anthropic API key is recommended instead; the usage also counts against your plan. Costs: [README](../README.md#what-it-costs); each provider's terms: [privacy.md](privacy.md#your-llm-provider).
- **retention**: Claude Code deletes transcripts after 30 days by default. Transcripts are the only source for importing past sessions and rebuilding memory, so setup offers to keep them ten years.
- `backsight setup --plan` shows the checks, answers and steps without changing anything.

## Secrets

Setup never takes a key as an argument or an answer. In a terminal, after your consent, it asks for the LLM key at a hidden prompt. Run without a terminal (`--non-interactive`, or by an agent), it stops with exit code 10 and tells you to set it yourself:

```sh
backsight secret set llm_api_key
backsight setup --resume
```

`secret set` reads the value at a prompt, without echo (or from a pipe, `--from-env VAR` or `--from-file FILE`). The same two commands replace a wrong key.

With the `claude-code` provider, run `claude setup-token` first and set its token as `llm_api_key`.

`backsight secret list` shows which secrets are set, never their values. Setup generates the server's API key, its web UI key and the database password, and, with backups on, the backup identity (in its jobs step).

If the provider rejects the key (a typo, a revoked key), setup stops at the server step, says so, and leaves the server running. Set the right key with `backsight secret set llm_api_key`, then run `backsight setup --resume`: it takes the running server for its own and checks the key again.

## Local models with Ollama

With the `ollama` provider, the server, which runs in a container, calls Ollama at `http://host.docker.internal:11434`: your machine, as the container sees it. Setup's `base_url` question changes that address.

- **macOS and Windows (Docker Desktop)**: nothing to change. Docker Desktop forwards that address to Ollama on your machine (these platforms are not tested yet).
- **Linux (Docker Engine 20.10 or newer)**: backsight maps `host.docker.internal` to your machine's Docker gateway, but Ollama listens only on `127.0.0.1` by default, which the container cannot reach. Make Ollama listen on an address the container reaches, then restart it.

With the systemd service Ollama's installer sets up, run `sudo systemctl edit ollama`, add these two lines, then `sudo systemctl restart ollama`:

```ini
[Service]
Environment="OLLAMA_HOST=172.17.0.1"
```

Ollama has no authentication: anyone who reaches its port can use your models, download new ones and delete them. Choose the address with that in mind:

| `OLLAMA_HOST` | Who reaches Ollama | Trade-off |
|---|---|---|
| `172.17.0.1`, Docker's default bridge (`ip -4 addr show docker0` shows yours) | this machine and its containers, not your network | Ollama must start after Docker, which creates that address (add `After=docker.service` under `[Unit]` in the same override); the `ollama` command on this machine then needs `OLLAMA_HOST=172.17.0.1` too |
| `0.0.0.0`, every interface | also every device on your network | simplest; block port 11434 from outside with a firewall |

A firewall on this machine (ufw, firewalld) can also block the container: allow port 11434 from Docker's networks (`172.16.0.0/12` by default), with ufw `sudo ufw allow from 172.16.0.0/12 to any port 11434 proto tcp`.

To check that a container reaches Ollama before you run setup:

```sh
docker run --rm --add-host=host.docker.internal:host-gateway busybox wget -qO- http://host.docker.internal:11434/api/version
```

It prints Ollama's version. Setup then tests the model itself: its LLM check runs one extraction, and `backsight server llm check` runs it again at any time.

## After setup

- Save your **backup identity** somewhere else, a password manager for example: `backsight backup identity show` prints it. Without it, your backups cannot be decrypted. See [backups-and-restore.md](backups-and-restore.md).
- If you accepted the backfill, the import has already started and goes on in the background; `backsight doctor` shows its progress.
- `backsight doctor` checks the whole install at any time.
- Daily use: [daily-use.md](daily-use.md).

## Install with a coding agent

A coding agent (Claude Code or another) can install backsight for you: point it at [agent-guide.md](agent-guide.md). It drives the same commands with `--json` and asks you each question. You type secrets yourself, as `! backsight secret set <name>` in Claude Code.

## Where backsight keeps its files

`backsight paths` prints every directory. On Linux:

| What | Where |
|---|---|
| Binaries, the shim, the server's Compose files, backups | `~/.local/share/backsight` |
| `config.toml` and `secrets.toml` | `~/.config/backsight` |
| State: cursors, outboxes, journals, logs | `~/.local/state/backsight` |
| The command on PATH | `~/.local/bin/backsight` |

The XDG variables (`XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME`) move them. `BACKSIGHT_HOME=<dir>` puts everything under one directory instead, for a test install for example.

## Replacing an existing Hindsight install

If Claude Code is already wired to a Hindsight server through another integration (the Hindsight npm plugin, Python hooks, a retro timer), setup detects it. Once backsight's own wiring passes `verify`, setup unwires the old pieces from Claude Code, keeping a copy of each file it edits. Your CLAUDE.md is never edited.

The old server and its data are left running and untouched: stop it yourself once you no longer need it as a fallback. Once `backsight backup verify` passed on backsight's newest backup within the last 7 days, doctor's `reference-server` line says the old server can be stopped and gives the command; stopping keeps its data. Your past sessions reach the new memory through backfill. `backsight setup --undo` puts the old wiring back.

## Undo setup

```sh
backsight setup --undo
```

This reverses what setup changed: Claude Code's files (from the copies setup kept), the scheduler entries, the plugin, the server it started, and any old integration it unwired. Config, secrets and the memory database are kept. Add `--plan` to see the list first. To remove backsight completely, see [uninstall.md](uninstall.md).
