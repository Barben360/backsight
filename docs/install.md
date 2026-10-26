# Install

How to install backsight and set it up for Claude Code.

## Requirements

- **Linux**, amd64 (the arm64 binary is built but not tested). macOS and Windows binaries are built but not tested yet.
- **Docker**, usable by your user without `sudo` (the `docker` group, or rootless Docker), with **Docker Compose v2** (the `docker compose` plugin, not the old `docker-compose`).
- **Claude Code** 2.1.309 or newer, with the `claude` command on your PATH.
- **An LLM provider for the memory server**: a DeepSeek, Anthropic or OpenAI API key (or a compatible endpoint), a local Ollama, or your Claude subscription. The server uses it to extract memories from your sessions.
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
4. links `~/.local/bin/backsight` to a shim that always points at the current version.

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
5. indexes these docs into a docs bank for `backsight guide search`;
6. wires Claude Code: a plugin with the hooks, the memory tools and the skills, plus a permission rule for the memory tools;
7. sets how long Claude Code keeps its transcripts, as you answered;
8. installs the background jobs (a timer every 15 minutes and a daily one);
9. runs `backsight verify`: every hook and the memory tools as Claude Code runs them, then a store-and-recall round trip;
10. unwires an older Hindsight integration from Claude Code, if you chose to (see below);
11. queues your past sessions for import (backfill), if you accepted.

The first run downloads the server image, a few GB: expect several minutes. Every step is journaled: if setup stops, `backsight setup --resume` continues where it stopped.

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

- **deepseek** is the cheapest measured option. **ollama** keeps everything on your machine, with lower quality memories and slow extraction on a CPU. **claude-code** uses your Claude subscription: Anthropic's terms restrict routing subscription credentials through third-party tools, and the usage counts against your plan.
- **retention**: Claude Code deletes transcripts after 30 days by default. Transcripts are the only source for importing past sessions and rebuilding memory, so setup offers to keep them ten years.
- `backsight setup --plan` shows the checks, answers and steps without changing anything.

## Secrets

Setup never takes a key as an argument or an answer. When the LLM key is missing, setup stops and tells you to set it:

```sh
backsight secret set llm_api_key
```

The value is read at a prompt, without echo (or from a pipe, `--from-env VAR` or `--from-file FILE`). Then run `backsight setup --resume`.

With the `claude-code` provider, run `claude setup-token` first and set its token the same way.

`backsight secret list` shows which secrets are set, never their values. Setup generates the others (the server's API key, its web UI key, the database password, the backup identity).

## After setup

- Save your **backup identity** somewhere else, a password manager for example: `backsight backup identity show` prints it. Without it, your backups cannot be decrypted. See [backups-and-restore.md](backups-and-restore.md).
- If you accepted the backfill, past sessions are sent in the background; `backsight doctor` shows its progress.
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
