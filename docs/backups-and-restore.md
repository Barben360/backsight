# Backups and restore

How backsight backs up your memory, and how to bring a backup back.

## Automatic backups

With backups on (setup's default), the daily job makes one backup and keeps **7 daily and 4 weekly** archives. They are written to `backups` under backsight's data directory (`~/.local/share/backsight/backups` on Linux).

Each archive is a dump of the server's database with a manifest (backsight version, server image, embedding models, bank), backsight's config, the server's Compose override and secrets. It is always encrypted.

The archives are yours to copy anywhere: another disk, a NAS, a cloud drive. `backsight doctor` warns when the last backup is more than 2 days old.

To change the folder or the retention, edit `config.toml`:

```toml
[backup]
dir = "/mnt/nas/backsight"   # an absolute path
keep_daily = 7
keep_weekly = 4
```

## The backup identity

Archives are encrypted with [age](https://age-encryption.org/). By default backsight generates an identity (a private key) on the first backup and stores it in `secrets.toml`. **Save it somewhere else** (a password manager): if this machine's disk is lost, the backups cannot be decrypted without it.

```sh
backsight backup identity show
```

This prints the identity in a terminal only, never to a pipe or in `--json`.

To encrypt to a key you already keep elsewhere, set `backup.recipient` in `config.toml` to one or more age recipients (`age1…`) or SSH public keys (`ssh-ed25519 …`), separated by commas. Backups are then encrypted to them, and you decrypt with your own key (`--identity <file>`, an age identity file or an SSH private key).

## Making and checking a backup

| Command | Does |
|---|---|
| `backsight backup create` | Makes a backup now, then prunes older ones with the retention. |
| `backsight backup list` | Lists backups, newest first. |
| `backsight backup verify <file>` | Decrypts the archive, restores it into a throwaway database, runs a recall and compares row counts. Your server is not touched. |

While a backup runs (a few seconds), hooks and jobs queue their writes; nothing is lost.

## Restoring a backup

```sh
backsight restore --plan <file>
backsight restore <file>
```

Restore replaces the local server's memory with the archive's. In order, it:

1. takes the maintenance lock, so no other backsight command or job changes the server meanwhile;
2. makes a **safety backup** of the server as it is now;
3. marks a restore in progress: every client holds its sends;
4. stops the server, replaces its database, and starts it on a temporary key only restore knows;
5. checks health, row counts and a recall;
6. writes a new **restore epoch**, then starts the server again on its normal key.

After a restore, each client compares what it sent with what the server now holds, and sends again the sessions the backup did not have, from the local transcripts. Facts stored with `memory_store` after the backup was made cannot be recovered that way.

Restore applies the archive's database only, never its config or secrets. It refuses an archive from another bank, with other embedding models, or made by a newer Hindsight than the one this binary pins (update backsight first). An archive from an older Hindsight is migrated when the server starts.

## When a restore fails

- **Exit 30**: something failed after the server stopped. Restore put the safety backup back, so the server is as it was. The output says what failed.
- **Exit 20 before anything changed**: a check failed (a lock held, Docker not running, a refused archive). Fix the cause and rerun.
- **Exit 20 after a failed rollback**: the server may be stopped or partly restored. `backsight doctor` shows a `restore` error naming the safety archive. Run what it says: `backsight restore <safety archive> --no-safety` puts the server back as it was, or rerun the restore you wanted.

`--no-safety` skips the safety backup: use it only to restore a safety archive by hand.

## Restoring on a new machine

1. Install backsight and run `backsight setup` with the **same bank name** as the old install (the `server.bank` value in the old `config.toml`, `claude-memory` by default).
2. Import the old identity: `backsight backup identity import <file>` (or pipe it on stdin). With your own recipient, pass `--identity <file>` to restore instead.
3. Copy the archive over and run `backsight restore <file>`.
