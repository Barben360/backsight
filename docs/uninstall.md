# Uninstall

How to remove backsight, and what stays afterwards.

## Undo setup or uninstall

| | `backsight setup --undo` | `backsight uninstall` |
|---|---|---|
| Claude Code wiring (plugin, permission rule) | removed | removed |
| Scheduler entries | removed | removed |
| An old Hindsight integration setup unwired | put back | left unwired |
| The server | stopped, data kept | kept running (default), or stopped, or deleted |
| The binary and the command on PATH | kept | removed |
| Config, secrets, state | kept | kept, or deleted with `--purge-local` |
| Backups | kept | kept |

Use `setup --undo` to go back to how the machine was before setup, keeping backsight installed. Use `uninstall` to remove backsight itself.

If setup replaced another Hindsight integration and you want it back, run `backsight setup --undo` before uninstalling: uninstall does not put it back.

## Uninstall

```sh
backsight uninstall --plan
backsight uninstall
```

`--plan` lists what would be removed and kept. Without flags, uninstall removes, in this order:

1. the scheduler entries, after waiting for a running job to finish (up to 2 minutes);
2. the plugin and its marketplace from Claude Code;
3. the permission rule; it also offers to put back the transcript retention setup raised;
4. the shim, the `backsight` command on PATH and the binaries, last.

The server keeps running with your memory; uninstall prints the command to stop it yourself. Config, secrets, state and backups stay, so a later install picks up where you left off.

On Windows, uninstall also stops the native server and deletes its scheduled task (the task would run the removed shim), deletes the tasks' folder in Task Scheduler and removes backsight's directory from your user `PATH`, before the files. Files that Claude Code sessions still run are moved to `trash` under the data directory, and a cleaner deletes them within 5 minutes of their release; uninstall names that directory, to delete after closing Claude Code if it is still there. See [windows.md](windows.md).

## Stopping or deleting the server

| Flag | Effect |
|---|---|
| `--stop` | Also stops the server. Its containers and data are kept. |
| `--delete-data` | Also deletes the server and **every memory in its bank**. You must type the bank id to confirm. |

On the [native runtime](native-runtime.md), the database's unit runs a check that is part of backsight before every start, so it cannot start once backsight is gone: uninstall therefore stops and disables both units even without `--stop` (its plan says `stopped and disabled`), and keeps their data in `server/native/home`. A later install starts them again. `--delete-data` stops and disables both units before it removes anything, then removes the units and `server/native` (the database, Python, the virtual environments and the models); the password of a kept native server lives in `server/native/server.env`.

`--delete-data` removes the containers, their anonymous volumes and the Docker volumes backsight's own `compose.yaml` declares (the database and the model cache). Volumes declared only in your `compose.override.yaml`, the Docker images and your backups stay. The images take about 3 GB; uninstall prints the command that removes them, of the form `docker image rm ghcr.io/vectorize-io/hindsight@sha256:… pgvector/pgvector@sha256:…` (they are pulled by digest, so `docker image ls` shows them as `<none>`).

## Removing local files

`--purge-local` also deletes `config.toml`, `secrets.toml` and the state directory, the local metrics index (`index/`) and the `metrics/` and `dashboard/` directories beside it. Files and folders backsight did not write are kept and listed. Backups are never deleted, even inside the state directory.

With it, uninstall first deletes this machine's shared counts (the `backsight:feedback:<machine id>` document your other machines weigh) from the server this PC joined, before `--stop` or `--delete-data` touch the server. A server that does not answer within 5 seconds keeps the document: uninstall says so in one line and goes on, and your other machines ignore it after 30 days. Nothing is deleted while another server answers at the joined address.

Before deleting, it asks you to confirm when:

- `secrets.toml` holds your backup identity: without it, existing backups cannot be decrypted. Save it first with `backsight backup identity show`;
- setup unwired another Hindsight integration and its saved copies would be lost (run `backsight setup --undo` first to put it back);
- notes or sessions are still queued and would never be sent.

## Answers for scripts and agents

With `--non-interactive`, uninstall exits 10 when it needs an answer, and lists every answer it needs at once. Give them with `--answer` (or `--answers-file`):

| Answer | Meaning |
|---|---|
| `confirm_bank=<id>` | the bank id, to confirm `--delete-data` (a wrong id exits 2 with nothing changed) |
| `retention=keep` or `retention=restore` | keep the raised transcript retention (default), or put back the previous value |
| `identity_saved=yes` | the backup identity is saved elsewhere |
| `pre_images_ack=yes` | the saved copies of another integration's wiring may be deleted |
| `drop_queued=yes` | queued notes and sessions may be dropped |

## When uninstall stops

- **Exit 20 with nothing changed**: a backup, restore or update is running or did not finish, or a job did not finish within 2 minutes. `--stop` and `--delete-data` also refuse a remote server. Rerun once it is done.
- **Exit 40**: a step failed before the binaries were removed. The output gives the command to resume, with the same flags.
- **Exit 20 after the binaries step started**: the output lists the paths left to remove by hand.

If `~/.local/bin/backsight` is not a link backsight made, uninstall leaves it and says so.

## Setting up again over kept data

`uninstall --stop --purge-local` (or `--purge-local` alone) keeps the server's database, and its `.env` in the server's directory (`~/.local/share/backsight/server/.env` by default), the only copy left of its database password. Install backsight again, then run `backsight setup` as the first time: its server step adopts the database password, the API key and the UI key from that `.env` when `secrets.toml` lacks them, so the server opens the kept database and its clients' key still works. A value already in `secrets.toml` is never replaced.

Keep the same server directory and Compose project (`server.compose_dir`, `server.project`, the defaults unless you changed them). If the `.env` is gone too, setup stops and names the database volume. In that order:

1. put back the `secrets.toml` the server was installed with, from your own copy;
2. or set the password it was created with: `backsight secret set db_password`;
3. last, and only if its memories may go: delete the volume (`docker volume rm <project>_pg_data`), then run setup again. Your backups can bring the memories back afterwards with `backsight restore`, using the backup identity you saved.
