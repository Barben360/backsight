# Backups and restore

How backsight backs up your memory, and how to bring a backup back. Words such as [tombstone](glossary.md#tombstone) and [epoch](glossary.md#epoch) are in the [glossary](glossary.md).

## Automatic backups

With backups on (setup's default), the daily job makes one backup and keeps **7 daily and 4 weekly** archives. They are written to `backups` under backsight's data directory (`~/.local/share/backsight/backups` on Linux).

## Which archives are kept

Each backup prunes the older ones. An archive stays when any of these holds:

- it is **less than 24 hours old**, whatever the retention says;
- it is the newest of one of the last `keep_daily` days;
- it is the newest of one of the last `keep_weekly` ISO weeks.

The 24-hour rule covers a second backup the same day: one you made by hand before a `forget`, and the safety archives of a restore or a server upgrade. The next backup does not delete them; they follow the daily and weekly slots once they are a day old.

Only archives backsight wrote (with their `.json` sidecar) are pruned. Other files in the folder, and archives copied in from elsewhere, are never touched.

Each archive is a dump of the server's database with a manifest (backsight version, the runtime the server ran on and its Hindsight version, server image, embedding models, bank), backsight's config, the server's Compose override (Docker only) and secrets. It is always encrypted.

The archives are yours to copy anywhere: another disk, a NAS, a cloud drive. `backsight doctor` warns when the last backup this machine can decrypt is more than 2 days old.

To change the folder or the retention, edit `config.toml`:

```toml
[backup]
dir = "/mnt/nas/backsight"   # an absolute path
keep_daily = 7
keep_weekly = 4
```

## The backup identity

Archives are encrypted with [age](https://age-encryption.org/). By default backsight generates an identity (a private key) during setup when backups are on (else on the first backup) and stores it in `secrets.toml`. **Save it somewhere else** (a password manager): if this machine's disk is lost, the backups cannot be decrypted without it.

```sh
backsight backup identity show | pass insert -m backsight/backup-identity
```

It prints the identity alone on stdout (the advice goes to stderr), so it pipes into a password manager (`pass`, `gpg -e`, a file on an encrypted disk) or shows at a terminal (on Windows, where there is no `pass`, run it at a terminal and copy it into your password manager). It needs a person's shell, with a terminal on one side of the pipe: an agent's command, which has none, and `--json` are refused.

To encrypt to a key you already keep elsewhere, set `backup.recipient` in `config.toml` to one or more age recipients (`age1…`) or SSH public keys (`ssh-ed25519 …`), separated by commas. Backups are then encrypted to them, and you decrypt with your own key (`--identity <file>`, an age identity file or an SSH private key).

## Making and checking a backup

| Command | Does |
|---|---|
| `backsight backup create` | Makes a backup now, then prunes older ones with the retention (never one less than 24 hours old). |
| `backsight backup list` | Lists backups, newest first. |
| `backsight backup verify <file>` | Decrypts the archive, restores it into a throwaway database of the runtime this install runs (the one a restore would use) and compares row counts. Your server is not touched. |

While a backup runs (a few seconds), hooks and jobs queue their writes; nothing is lost.

On the native runtime the throwaway database is a new Postgres cluster of pg0's own binaries under backsight's state directory, on a free loopback port with a password of its own, run in a transient systemd user unit (`<project>-verify-<id>.service`) that systemd stops after an hour at most. It is stopped and deleted after the check, also when the check fails or is interrupted. On Windows it runs under `server\native\tmp\verify-<id>`, in a Job Object of the command itself, so it ends with the command whatever happens. Before verifying, a postgres found running on the native database's files outside its unit is stopped, never adopted, and reported.

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
6. writes back the **purge tombstones** it read before the stop (the server's and this machine's): the archive's `<bank>-control` bank lacks the purges made since;
7. writes a new **restore epoch**, in your bank and in every other memory bank of the server (see below);
8. waits, as long as it takes and printing what it waits for, until the restored banks with purges have no queued retain left (one would recreate a purged document), the restore still in progress and the server on its temporary key; then purges again, as `forget --purge` does, every purged document the archive brought back (a document a tombstone names): its facts' observations, the document, and the briefing page of each project it touched;
9. compares the bank's settings, as the archive brought them back, with this backsight's template, and applies the template where they differ (if it cannot, it says so, and the next `client install` or `update apply` sends it);
10. starts the server again on its normal key, and only then clears the restore in progress.

After a restore, each client compares what it sent with what the server now holds, and sends again the sessions the backup did not have, from the local transcripts, with the final reports of their subagents (rebuilt from `<session>/subagents/agent-<id>.jsonl` by the same rules as when the subagent stopped: not with `components.write` off, not from an opted-out directory or a headless session, never one a purge deleted). The same reconcile purges again any purged document the server still holds: one a late queued retain recreated, or one another machine purged while the restore ran. Facts stored with `memory_store` after the backup was made cannot be recovered that way. What `forget --purge` deleted is never sent again (see [privacy](privacy.md#removing-what-was-stored)), even when the archive predates the purge.

`restore --plan` says how many tombstones restore checks (every one), and names the purges made after the archive was taken, which the archive may hold. Their turns are not sent again from your transcripts either.

**Note for purges made with backsight before this release**: until now, the turns a session wrote after one of its documents was purged were appended to the purged document's id, which Hindsight recreated. Restore, and the reconcile after it, purge that id again, those later turns with it. From this release on, they go to a new document (`conversation:<session>#2`), which a purge again leaves alone. Nothing to migrate. Such a session waits for its first segment on the new document, then is quarantined after 24 hours; `backsight retain retry-quarantine` then sends those turns again, to the new document.

### Other banks on the server

The archive is a dump of the server's whole database, so it brings back **every bank** on the server as it was, not only yours (a second machine may write to another bank of the same server). Restore gives each memory bank a new restore epoch and writes its purges back, so the clients that write to it also send again what the archive lacks. A server upgrade's rollback does the same, without asking.

A bank is proven backsight's when its `<bank>-control` bank is backsight's (named and filled by backsight); a docs bank (`<bank>-docs`) when it carries backsight's docs marker. Every other bank may not be backsight's: a bank with no control bank, someone else's `-control` or `-docs` bank, and, while the server's API cannot answer (the banks are then read from its database), every bank. Before anything changes, restore names those banks and exits 10, asking `other_banks`. A control bank restore creates for such a bank is marked adopted (its name reads "backsight control (adopted)"): backsight writes the bank's epoch and purges there, but it proves nothing, so every later restore asks about that bank again. To go on, and accept that those banks go back to the archive too:

```sh
backsight restore <file> --answer other_banks=revert
```

`restore --plan` names them and says it will ask. Someone else's control bank gets no epoch; a bank whose control bank turns out to be someone else's gets a warning instead.

Restore applies the archive's database only, never its config or secrets. It refuses an archive from another bank, with other embedding models, or made by a newer Hindsight than the one this install runs (update backsight first). It also refuses, before anything stops, when the server's `<bank>-control` bank (where the restore epoch is written) is not backsight's: another bank name than backsight's, or documents that are not its own. An archive from an older Hindsight is migrated when the server starts.

### Archives cross runtimes

An archive restores into either runtime, Docker or native, when its Hindsight version is not newer than the one this install runs: the dump is the same format on both. Restore compares the Hindsight version the manifest records (or, for an archive made before it did, the version in its image tag); an archive whose version it cannot tell is refused, to restore on the runtime it was taken on. Between two Docker installs nothing changed: the archive's image is compared as before.

Going from Docker to native moves the database from PostgreSQL 18.6 with pgvector 0.8.7 to PostgreSQL 18.1 with pgvector 0.8.5, an older minor release. That is why `backsight backup verify` runs in the runtime that will restore the archive, the one this install is configured for: verify a Docker archive on the native install before relying on it there.

### After a refused native start

On the native runtime, the database refuses to start when it is missing, empty, another database than the one backsight recorded, or has no recorded identity (see [troubleshooting](troubleshooting.md#the-native-server-does-not-start)). Every such refusal names `backsight restore <archive>`, and restore then works without the database it cannot read:

- it moves the refused database aside, never deleting it, to `<instance>.refused-<Unix time>` beside it (and its identity file to `identity.refused-<Unix time>`), and names it in place of a safety backup, which cannot be taken from a database that does not start;
- it does not read the old database's restore epoch or purges: the new epoch is above both this machine's last one and the archive's, and this machine's purges are kept;
- it creates a new database as a first install does, restores the archive into it and records its identity;
- the archive's other banks that are proven backsight's get an epoch too; the others get a warning.

If it fails after the move, it removes the new database (so that no start serves an empty or half-restored memory), keeps the refused one where it moved it, and exits 20: run the same `backsight restore <file>` again. Every client holds its sends until a restore succeeds.

## When a restore fails

- **Exit 10**: the server holds another bank not proven backsight's; nothing was changed. Rerun with `--answer other_banks=revert` if it may go back to the archive too.
- **Exit 40, "the restore waits at its purge step"**: interrupted (Ctrl-C, a timeout) while it waited for the restored queues, or a queue did not move for 10 minutes. The archive is in place and the epochs written; every client holds its sends, and the server runs on a temporary key. `backsight server start` resumes: it waits for the queues, purges again, applies the bank template, then starts the server on its API key. When the queue did not move, the message and `backsight doctor` name the operations: Hindsight runs again, when it starts, only the operations its own worker left in progress, so one an archive brought from another machine may never run. Then put the server back as it was with `backsight restore <safety archive> --no-safety`.
- **Exit 30**: something failed after the server stopped. Restore put the safety backup back, so the server is as it was. The output says what failed.
- **Exit 20 before anything changed**: a check failed (a lock held, Docker not running, a refused archive, a `<bank>-control` bank that is not backsight's). Fix the cause and rerun.
- **Exit 20, "the server did not start again on its API key"**: the restore (or its rollback, a server upgrade's included) is done and its epoch written, but the last start failed. Every client holds its sends until `backsight server start` succeeds: it starts the server on its key, on the Hindsight version its data is for (after a server upgrade's rollback, the version before), and finishes the restore. `backsight doctor` says so too, and names the safety archive as the fallback (`backsight restore <safety archive> --no-safety`).
- **Exit 20, "the native database changed while restore waited for the lock"**: restore judged the native database (refused or not) before taking the maintenance lock, and another command changed it meanwhile (it put the right one back, or the database broke). Nothing was changed: rerun `backsight restore <file>`, which judges it again.
- **Exit 20, "the new native database was removed"**: a restore after a refused native start failed; the refused database is kept where the message says. Rerun `backsight restore <file>`.
- **Exit 20 after a failed rollback**: the server may be stopped or partly restored. `backsight doctor` shows a `restore` error naming the safety archive. Run what it says: `backsight restore <safety archive> --no-safety` puts the server back as it was, or rerun the restore you wanted.

`--no-safety` skips the safety backup: use it only to restore a safety archive by hand.

## Restoring on a new machine

1. Install backsight and run `backsight setup` with the **same bank name** as the old install (the `server.bank` value in the old `config.toml`: `backsight` for an install from v0.1.4-m5 on, `claude-memory` before, or when the file has no `bank` line). An archive taken before the bank rename restores with `server.bank = "claude-memory"`; run `backsight bank rename` after it.
2. Import the old identity: `backsight backup identity import <file>` (or pipe it on stdin). With your own recipient, pass `--identity <file>` to restore instead.
3. Copy the archive over and run `backsight restore <file>`.
