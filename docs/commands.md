# Commands

Every backsight command, grouped by use. `backsight <command> --help` shows the same flags with their defaults.

## Conventions

- `--json` (every command): print one JSON envelope `{schema_version, command, ok, data | error}` instead of text. Coding agents use it.
- `--plan`: show what the command would do, changing nothing. Commands that change configuration, your files or the server's setup have it. Data operations do not: `secret set`, `backup create`, `store`, `forget <id>` and `retain flush` do what you asked; `retain backfill --plan` and `forget --project --plan` preview the larger ones.
- `--non-interactive`: never prompt; when an answer or a secret is missing, exit 10 and say what is needed.
- `--answer id=value` (repeatable) or `--answers-file <file>`: answers for commands that ask questions (`setup`, `uninstall`). Never secrets.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | done |
| `2` | invalid invocation: unknown command, flag, argument or answer |
| `10` | you must act: answer a question, set a secret, run a command; the output says what, and how to resume |
| `20` | blocked by the machine (Docker not running, a lock held, a check failed); nothing was changed, or a rollback failed |
| `30` | failed and rolled back: the machine is as it was before |
| `40` | partly done and resumable: rerun the command named in the output |

## Install and wiring

| Command | Does |
|---|---|
| `backsight setup` | Sets up everything: server, bank, Claude Code wiring, jobs, verify, backfill offer. Flags: `--plan`, `--questions` (list the questions), `--answer`, `--answers-file`, `--non-interactive`, `--resume`, `--config-dir <dir>` (another Claude Code profile). |
| `backsight setup --undo` | Reverses setup; keeps config, secrets and the memory. `--plan`; `--force` restores files edited since. |
| `backsight verify` | Runs every hook and the memory tools as Claude Code would, then a store-and-recall round trip in a scratch bank. `--no-round-trip` skips the round trip (no LLM call); `--config-dir <dir>`. `--live` checks the real pipeline instead, read-only: this machine's latest settled session reached the server (every document of it: one per project its records belong to, `conversation:<session>` and `conversation:<session>@<project>`) and an observation was consolidated from it (exit 0 pass, 40 not yet, 20 failed with a fix). |
| `backsight client install` | Wires Claude Code: the plugin and its permission rule. Also makes `~/.local/bin/backsight` (a link to the shim) when nothing is there, and sends this version's bank template when the bank was given another one. `--plan`. |
| `backsight client retention` | Keeps Claude Code transcripts 3650 days; `--days N` sets another value, `--keep` stops managing it. `--plan`. |
| `backsight client retire` | Unwires another Hindsight integration from Claude Code. `--plan`. |
| `backsight client config plan` | Shows backsight's pending edits to Claude Code's files. |
| `backsight client config apply` | Applies them (a copy of each file is kept first). |
| `backsight client config restore [<run>]` | Puts Claude Code's files back as they were before a run (default: the last). `--plan`, `--force`. |
| `backsight uninstall` | Removes backsight from this machine. See [uninstall.md](uninstall.md). |
| `backsight paths` | Shows where backsight keeps its binaries, config and state. |
| `backsight version` | Prints the version; `--json` adds the release keys and the pinned Hindsight image. |

## Secrets

| Command | Does |
|---|---|
| `backsight secret list` | Shows which secrets are set, never their values. |
| `backsight secret set <name>` | Sets one, read at a prompt without echo, from a pipe, `--from-env VAR` or `--from-file FILE`. |

## Memory

| Command | Does |
|---|---|
| `backsight search <query>...` | Searches the memory, up to 4 short keyword queries. `--all`, `--project <name>`, `--limit N`, `--min-score X`. |
| `backsight read <id>` | Shows one memory with the facts it was built from. |
| `backsight store --kind <kind> <text>` | Stores one durable fact. `--project <name>`. |
| `backsight forget <id>` | Retires a memory (reversible with `--restore`); `--purge --yes` deletes it with its source facts and documents. |
| `backsight forget --project <name>` | Retires every raw fact of a project in the configured bank (each reversible with `backsight forget <id> --restore`); `--purge` deletes its documents and observations for good. `--source <s>` keeps to the facts of one source (`chat`, `subagent`, `note`) and the observations drawn from them. `--plan` lists the documents and counts; the write needs `--answer confirm_project=<name>`. |
| `backsight memory scan` | Lists stored memories that look like they hold a secret, with the purge command for each. |
| `backsight ask <question>` | The server's LLM answers over many memories (costs tokens). `--all` for the whole memory. |
| `backsight projects` | Lists projects with their memory counts and last activity. |
| `backsight pause` / `backsight resume` | Stops or restarts recording this session; `--project` for the whole project (`--plan` shows the config change); `--session <id>`. |
| `backsight why` | Shows the last memories the hooks injected and why. `--last N`, `--session <id>`. |
| `backsight stats [days]` | Usage over the last days (default 7); `--since`/`--until` for a fixed window. |

## Sending sessions

| Command | Does |
|---|---|
| `backsight retain flush` | Sends the unsent turns of every session now, without the 30-minute wait. `--session <id>`. |
| `backsight retain backfill` | Imports past sessions within a spending cap. `--plan`, `--since YYYY-MM-DD`, `--project <name>`, `--max-cost X`, `--max-tokens N`, `--allow-subscription`, `--projects-dir <dir>`. |
| `backsight retain retry-quarantine` | Gives quarantined sessions and notes another try. `--session <id>`, `--notes`. |

## Server

| Command | Does |
|---|---|
| `backsight server status` | Shows the server's containers and API health. |
| `backsight server start` | Renders the Compose files if needed and starts the server. `--plan`. |
| `backsight server stop` | Stops the server; containers and data are kept. `--plan`. |
| `backsight server install` | Installs and starts the server alone (setup does it). `--plan`, `--provider`, `--model`, `--base-url`, `--api-port`, `--ui-port`. |
| `backsight server render` | Writes the server's `compose.yaml` and `.env` from config and secrets. `--plan`. |
| `backsight server upgrade` | Moves the server to the version this binary pins, after a verified safety backup. `--plan`, `--allow-unordered`. |
| `backsight server llm check` | Checks the extraction LLM on a fixed sample, storing nothing. |
| `backsight bank apply` | Creates the bank if needed and applies backsight's extraction settings, unless the bank was already given this version's. `--force` sends them anyway, over any edit made to them on the server. `--plan`. |

## Backups and restore

| Command | Does |
|---|---|
| `backsight backup create` | Makes an encrypted backup now. |
| `backsight backup list` | Lists backups, newest first. |
| `backsight backup verify <file>` | Decrypts a backup, restores it into a throwaway database and compares counts. `--identity <file>`. |
| `backsight backup identity show` | Prints the key that decrypts your backups (terminal only). |
| `backsight backup identity import [file]` | Stores a backup identity from a file or stdin. `--force` replaces another one. |
| `backsight restore <file>` | Restores a backup into the local server. `--plan`, `--identity <file>`, `--no-safety`, `--non-interactive`. |

Details: [backups-and-restore.md](backups-and-restore.md).

## Updates

| Command | Does |
|---|---|
| `backsight update check` | Lists newer releases with their notes. `--source <dir>`. |
| `backsight update apply` | Installs a newer release, rolling back on failure. `--plan`, `--version <v>`, `--source <dir>`, `--allow-unordered`. |
| `backsight changelog` | Prints release notes, newest first. `--since <v>`, `--source <dir>`. |

Details: [update.md](update.md).

## Diagnostics

| Command | Does |
|---|---|
| `backsight doctor` | Checks the whole install, read-only; each problem comes with its fix. |
| `backsight report` | Prints a redacted diagnostic and links to search and file a GitHub issue. Sends nothing. |
| `backsight guide` | Lists these documents; `backsight guide <topic>` shows one (`<topic>#<section>` one section), `backsight guide search <question>` searches them: semantically on the server, with a keyword search when the server cannot answer (`--mode keyword` asks for it). |
| `backsight guide index [--plan]` | Indexes these documents into the server's docs bank (`<bank>-docs`, no LLM call). Setup and `update apply` run it, and `guide search` when the bank holds other docs. Several backsight versions can share the bank: each tags the sections it holds, and a section is deleted only when no version holds it. The bank keeps the last 5 versions, by the time each was first indexed (recorded in its `docs:_versions` document): an older version's tags are removed, and its sections no kept version holds are deleted. A `<bank>-docs` that backsight did not make (another name, or documents that are not its sections) is left as it is: `guide index` refuses and `guide search` uses its keyword search, saying why. `--plan` lists the documents it would create, resend, retag, untag, prune (losing only a dropped version's tag) and delete, and the versions it would drop. The `/backsight:memory` skill may run `guide` and `guide search` without asking, never `guide index`. |

Details: [troubleshooting.md](troubleshooting.md).

## Background jobs

The OS scheduler (systemd user timers on Linux) runs these; you rarely need them by hand.

| Command | Does |
|---|---|
| `backsight jobs tick` | Every 15 minutes: sends settled sessions and queued notes, paces the backfill. |
| `backsight jobs maintain` | Daily: telemetry rotation, backup, update check (and update with `update.auto_apply`), doctor snapshot. |
| `backsight jobs install` | Installs the scheduler entries (setup does it). `--plan`. |
| `backsight jobs uninstall` | Removes them. `--plan`. |
| `backsight jobs refresh` | Rewrites entries whose template changed, without stopping a running job (updates do it). `--plan`. |
