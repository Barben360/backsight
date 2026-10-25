# backsight

[![CI](https://github.com/Barben360/backsight/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Barben360/backsight/actions/workflows/ci.yml?query=branch%3Amain)
[![Secret scan](https://github.com/Barben360/backsight/actions/workflows/gitleaks.yml/badge.svg?branch=main)](https://github.com/Barben360/backsight/actions/workflows/gitleaks.yml?query=branch%3Amain)
[![Release](https://img.shields.io/github/v/release/Barben360/backsight?include_prereleases&sort=semver)](https://github.com/Barben360/backsight/releases)
[![License: MIT](https://img.shields.io/github/license/Barben360/backsight)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/Barben360/backsight)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/Barben360/backsight)](https://goreportcard.com/report/github.com/Barben360/backsight)
[![Status: preview, Linux](https://img.shields.io/badge/preview-Linux%20%28macOS%20and%20Windows%20later%29-orange)](docs/compatibility.md)
[![Hindsight 1.1.0](https://img.shields.io/badge/Hindsight-1.1.0-blue)](docs/compatibility.md)

Long-term memory for Claude Code, on a [Hindsight](https://github.com/vectorize-io/hindsight) server you run yourself.

backsight is one Go binary. It installs a Hindsight server in Docker, wires it into Claude Code, and keeps it running: updates, encrypted backups, checks and repairs. Claude then remembers what you decided, fixed and learned in past sessions, across all your projects, and recalls it when it matters.

> A *backsight* is the surveying reading taken back on a known point before moving forward.

backsight is an independent project, not affiliated with or endorsed by Anthropic or Vectorize.

## What it does for you

- **Remembers your sessions.** When a session ends, its text is redacted and sent to your server, which extracts durable facts: decisions, fixes, conventions, pitfalls.
- **Recalls at the right moment.** Each session starts with a briefing on the current project. Each prompt brings the few most relevant memories into Claude's context, and Claude searches the memory before answering factual questions.
- **Stays yours.** The server runs on your machine, on `127.0.0.1`. You choose which LLM extracts the memories, and can pause or exclude anything.
- **Looks after itself.** Daily encrypted backups, signed updates that roll back on failure, and `backsight doctor` to say what is wrong and how to fix it.

## Requirements

- **Linux** (amd64). macOS and Windows come later.
- **Docker** with Compose v2, usable without `sudo`.
- **Claude Code** 2.1.309 or newer.
- **An LLM provider key** for extraction: DeepSeek (cheapest), Anthropic, OpenAI or a compatible endpoint. A local Ollama or your Claude subscription also work, with caveats.

Details: [docs/install.md](docs/install.md#requirements).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.sh | sh
backsight setup
```

The script checks the release's signature and checksum before installing anything. `backsight setup` asks a few questions (the LLM provider, transcript retention, backups), shows its plan, and asks for your consent. It then asks you to type your provider key with `backsight secret set llm_api_key`; keys are never passed as arguments. The first run downloads the server image (a few GB). Restart Claude Code when it finishes.

A coding agent can do the install for you: point it at [docs/agent-guide.md](docs/agent-guide.md).

## Example

Monday, in a session on your `billing` project:

```text
> The invoices job times out against the new gateway. Let's raise the client timeout to 60 s and retry twice.
```

The session ends; backsight sends it to your server, which extracts the decision. On Thursday, in a new session:

```text
> Why does the invoices job take so long on failures?
```

backsight adds the memory to your prompt, and Claude answers with it:

> 🧠 **From Hindsight memory (billing)** — the invoices client timeout was raised to 60 s with 2 retries on 2026-10-12, because the new gateway is slow to answer.
>
> A failing call can take up to 3 minutes: 60 s for each of the 3 attempts.

## Privacy

- The server runs locally, on loopback, with its API and web UI behind keys.
- Session text goes to the LLM provider you chose, and nowhere else. With Ollama, nothing leaves the machine.
- Secrets are redacted before anything is stored, and facts that look like secrets are refused.
- `/backsight:pause` stops recording a session; `backsight pause --project` or a `.backsight.toml` with `retain = false` excludes a project.
- Backups are always encrypted.
- No telemetry, no automatic reports. The binary's only other connection is a daily check for new releases on GitHub.

Details: [docs/privacy.md](docs/privacy.md).

## Daily use

You mostly use Claude Code as before. Commands you may need:

| Command | Use |
|---|---|
| `backsight doctor` | Check everything, with a fix for each problem. |
| `backsight search "<keywords>"` | Search the memory yourself. |
| `backsight store --kind decision "<fact>"` | Store a fact (in Claude Code: `/backsight:remember`). |
| `backsight forget <id>` | Retire a wrong memory. |
| `backsight pause` / `backsight resume` | Stop or restart recording (in Claude Code: `/backsight:pause`). |
| `backsight stats` | See how memory was used this week. |

`backsight guide` browses this documentation in the terminal, and `backsight guide search "<question>"` searches it. All of it is in [docs/](docs/): [daily use](docs/daily-use.md), [commands](docs/commands.md), [backups and restore](docs/backups-and-restore.md), [troubleshooting](docs/troubleshooting.md).

## Update and uninstall

- `backsight update apply` installs a newer release: it checks the signature, and rolls back on failure. You are told when one is available. See [docs/update.md](docs/update.md).
- `backsight uninstall` removes backsight and keeps your memory and backups; `--delete-data` also deletes the memory. See [docs/uninstall.md](docs/uninstall.md).

## Status

backsight is a **preview**: it runs daily on its author's machine, but expect rough edges and changes between releases. Linux is the only tested platform. Each release pins one Hindsight version, listed in [docs/compatibility.md](docs/compatibility.md).

Found a problem? `backsight report` prints a redacted diagnostic and a link to file a GitHub issue; it sends nothing itself. For a security problem, see [SECURITY.md](SECURITY.md).

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT, see [LICENSE](LICENSE).
