# The native runtime

backsight runs its memory server in Docker by default. The native runtime runs the same server without Docker: Hindsight from PyPI, on [pg0](https://pypi.org/project/pg0-embedded/)'s PostgreSQL with pgvector, as two systemd user units of your own user. This page says what it is, what it lacks and how to set it up. Words you may not know are in the [glossary](glossary.md).

## What it is

- `backsight-db.service` runs the database: the PostgreSQL that pg0 ships, started directly by systemd, listening on `127.0.0.1` only.
- `backsight-server.service` runs Hindsight's API (`hindsight-api`) from a Python virtual environment, on `127.0.0.1:<the API port>` (8888 by default). It needs the database unit and stops with it.

The units are named after `server.project` (`<project>-db.service`, `<project>-server.service`) and live beside backsight's job timers in your user's systemd directory (`~/.config/systemd/user`). backsight writes, enables, starts, stops and disables them; you never edit them. Everything else lives under backsight's data directory, in `server/native` (`<server.compose_dir>/native`).

## What it lacks

- **No web UI.** The native runtime runs Hindsight's API, not its control plane; doctor and the `backsight` commands cover what you need.
- **This PC only.** The server listens on `127.0.0.1`: `backsight server expose --private` is refused, and so is `setup --role server` (a server-only host exists to serve other PCs). Use the docker runtime for a [shared memory](several-pcs.md).
- **Linux x86_64 (linux-amd64) only.** Other platforms are refused at every way in. Not tested on WSL.
- **A slightly older PostgreSQL.** pg0 ships PostgreSQL 18.1 with pgvector 0.8.5, where the Docker image runs 18.6 with 0.8.7: the dump format is the same, and backups restore across both ([backups and restore](backups-and-restore.md)).

## What it needs

- **A systemd user session**, reached from a login session of your user (an ssh login, not `su` or `sudo`): the server runs as your user's units. A container without systemd cannot run it.
- **Lingering** on for your user, so the units run while you are logged out and start at boot: `loginctl enable-linger` (once; no `sudo` needed on most distributions). Setup checks it and stops until it is on.
- About **3.5 GB of free disk**: Python, the virtual environment, the models and the first months of data.
- No Docker.

## Set it up

```sh
backsight setup --runtime native
```

Setup asks its usual questions; `--runtime native` is the `runtime` answer (`--answer runtime=native` is the same, and `backsight setup --resume` keeps it). Its pre-flight then checks the platform, the user session, lingering, the API port and the disk instead of Docker. The server step downloads uv, Python, Hindsight's packages (each checked against the hash this release pins) and the models, then creates the database, records its identity and starts both units. Without `--runtime`, setup installs on Docker, as before.

An install already on Docker keeps its runtime: `setup --runtime native` over it is refused, since its memory is in Docker's database. Moving an existing install from one runtime to the other is `backsight server runtime switch`, which takes a verified backup in one and restores it into the other.

## Where it keeps its files

Under `server/native` in backsight's data directory:

| Path | What |
|---|---|
| `uv/<version>/uv` | The pinned [uv](https://github.com/astral-sh/uv), checked against its sha256. It is never taken from your `PATH`. |
| `python/` | The pinned Python (3.11), installed by uv. |
| `venv/<Hindsight version>-<lock hash>/` | Hindsight's virtual environment, one per locked set of packages, so an upgrade installs beside the running one. |
| `home/` | The units' `HOME` (mode 0700): pg0's database and PostgreSQL in `home/.pg0`. |
| `hf/` | The embedding and reranking models (`HF_HOME`). |
| `server.env` | The server's environment, its keys included (mode 0600). The units read it; no key is ever on a command line or in a unit file. |
| `identity` | The identifier of the database backsight created or restored (mode 0600). |

## Logs and state

```sh
backsight server status
journalctl --user -u backsight-server.service
journalctl --user -u backsight-db.service
```

`backsight server start` and `backsight server stop` start, and stop and disable, both units. The units carry none of your shell's Python, uv, Hugging Face or PostgreSQL client variables: what you set for your own projects does not reach the server.

## The guards

The database never starts on the wrong data. Before each start, from backsight, at boot or when systemd restarts it, the database unit runs `backsight server native-check`: it refuses a database that is missing, empty, or another one than the identity recorded, and stops a PostgreSQL running on the database's files outside the unit, never adopting it. After each start, backsight checks that `fsync` is on and stops both units when it is not. backsight creates a database only when it installs the server, never at a plain start, so a lost database is never silently replaced by an empty memory. Each refusal names what to do ([troubleshooting](troubleshooting.md#the-native-server-does-not-start)); `backsight restore <archive>` works after every one of them.

Uninstalling removes backsight, and with it that guard: `backsight uninstall` therefore stops and disables both units even when it keeps the server, its data left in place; `--delete-data` stops and disables them before removing anything. Backups are never deleted.

## Licences

backsight does not ship these: they are downloaded on your machine when the server is installed, under their own licences.

| Component | Licence, and where to read it |
|---|---|
| uv, by Astral | MIT or Apache-2.0: [LICENSE-MIT](https://github.com/astral-sh/uv/blob/main/LICENSE-MIT), [LICENSE-APACHE](https://github.com/astral-sh/uv/blob/main/LICENSE-APACHE). |
| CPython, as built by [python-build-standalone](https://github.com/astral-sh/python-build-standalone) | The [Python Software Foundation License](https://docs.python.org/3/license.html); the libraries each build bundles keep their own licences, which python-build-standalone's repository and documentation list. |
| Hindsight (`hindsight-api-slim`) and the other Python packages | Each package's own: its licence ships in its `*.dist-info` directory in the virtual environment (`venv/<version>/lib/python3.11/site-packages/`), and its PyPI page names it. The packages and versions are those of backsight's lock, `internal/server/nativelock/requirements-linux-amd64.txt` in the source. |
| PostgreSQL and pgvector, shipped by `pg0-embedded` | The [PostgreSQL License](https://www.postgresql.org/about/licence/), and [pgvector's](https://github.com/pgvector/pgvector/blob/master/LICENSE); pg0's own is in its `*.dist-info`. |
| The models | As in the [README](../README.md#third-party-components). |
