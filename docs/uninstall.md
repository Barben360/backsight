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

## Stopping or deleting the server

| Flag | Effect |
|---|---|
| `--stop` | Also stops the server. Its containers and data are kept. |
| `--delete-data` | Also deletes the server and **every memory in its bank**. You must type the bank id to confirm. |

`--delete-data` removes the containers and the Docker volumes backsight's own `compose.yaml` declares (the database and the model cache). Volumes declared only in your `compose.override.yaml`, the Docker images and your backups stay.

## Removing local files

`--purge-local` also deletes `config.toml`, `secrets.toml` and the state directory. Files and folders backsight did not write are kept and listed. Backups are never deleted, even inside the state directory.

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
