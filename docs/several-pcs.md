# Several PCs, one memory

How to share one memory between your PCs. One machine holds the server: the [**host**](glossary.md#host). The other PCs join it with `backsight connect`: the [**clients**](glossary.md#client). Words you may not know are in the [glossary](glossary.md).

- **Shared**: the memory itself, so a decision stored on one PC is recalled on the others, and which memories Claude used or hid on each PC (memory ids, numbers and times only; `feedback.share_ledger` in [configuration.md](configuration.md#feedback-which-memories-claude-used)).
- **Kept on each PC**: Claude Code's transcripts, the queue of notes waiting for the server, `config.toml` and `secrets.toml`.
- **The host's job**: the server, its backups, its updates and restores. A purge (`backsight forget <id> --purge --yes`) can be run from any PC and holds on all of them ([below](#restore-and-purge-with-several-pcs)).

## Choose the host

Pick a machine that is on whenever you work:

- **A PC that already runs backsight** for Claude Code: nothing to install, go on with [Open the server to your other PCs](#open-the-server-to-your-other-pcs).
- **A home server or a NAS** without Claude Code: `backsight setup --role server` there installs the server only ([A server-only host](install.md#a-server-only-host)). It needs Docker and an LLM provider. On Linux, user timers stop while you are logged out unless lingering is on (`sudo loginctl enable-linger <user>`), and with them the daily backup.

While the host is off or out of reach, clients do not lose anything: sessions stay in their transcripts and notes in their queue, sent once the server answers ([Away from home](#away-from-home)).

## Open the server to your other PCs

On the host, find its private address (`tailscale ip -4` for a tailnet, `ip addr` for the LAN), check the change, then make it:

```sh
backsight server expose --private <ip> --plan
backsight server expose --private <ip> --out <file>
```

The server then listens on that address as well as on `127.0.0.1`, and the host itself keeps using `127.0.0.1`. expose restarts the server under the maintenance lock, checks it answers on both addresses, and writes the [connection token](glossary.md#connection-token) to `<file>` ([below](#the-connection-token)). If the server does not come back healthy, `config.toml` is put back and the server started again on it (even one that was stopped before expose).

- **A tailnet address** (Tailscale: `100.x`) keeps the API key encrypted on the way.
- **A LAN address** sends the API key in clear to anyone on that network. expose stops and asks: on a network you trust, rerun with `--answer cleartext_lan=yes`.
- **A public address** is refused, and so is an address no interface of the host has: the message lists the ones it has ([Another PC cannot reach the server](troubleshooting.md#another-pc-cannot-reach-the-server)).

On every client, doctor's `server` check says how the API key travels: over a tailnet address (or a `*.ts.net` name), as information, that WireGuard encrypts it; over any other address, as a warning, that it travels unencrypted on that network, with the way to move the server to a tailnet address.

`backsight server expose --loopback` closes the server to the other PCs again.

After a reboot, Docker may start before the address exists (Tailscale or DHCP still coming up), and the server stays down, on the host too. backsight starts it again on its own once the address is back: see [After a reboot the server was down for a while](troubleshooting.md#after-a-reboot-the-server-was-down-for-a-while-private-exposure).

## The connection token

A client joins with a connection token. It holds the server's address on your private network and its web UI's, the bank, the **API key**, the [server's id](glossary.md#server-id) and Hindsight version, and the host's LLM provider, model and prices (for cost estimates). Never the LLM key.

`backsight server expose --private <ip> --out <file>` already wrote one. To make another at any time, on the host:

```sh
backsight server token --out <file>
```

The file is readable by you only (mode 0600; an existing file is replaced). Without `--out`, the token goes to a terminal only: `--json` or a piped output needs `--out`. The server must be exposed and running.

- Carry the file to the other PC yourself (`scp` over your own ssh, a USB stick), never through a chat or a shared folder.
- Once `connect` is done, delete it on both machines: the host keeps the file it wrote, and connect reminds you but never deletes it itself.
- Never paste it into a chat with Claude or another agent. backsight masks it before it stores the session, but the transcript on disk and the agent's provider have it: rotate the key ([A lost laptop](#a-lost-laptop)). connect refuses a token given on its command line, where your shell history keeps it, and tells you to rotate too.

## Join from another PC

1. [Install the binary](install.md#install-the-binary). A client needs Claude Code, not Docker.
2. Copy the token file over, then check what connect would do:

   ```sh
   backsight connect --token-file <file> --plan
   ```

3. Join, then delete the file here and on the host:

   ```sh
   backsight connect --token-file <file>
   ```

   `backsight connect --token -` reads the token on stdin instead, at a hidden prompt in a terminal.

connect first checks the server, reading only: it answers, accepts the key, holds the token's bank and has the token's server id. A token naming a public IP address is refused. When the server's Hindsight differs from this backsight's pin, connect warns and goes on. Each refusal leaves this PC as it was: see [connect refuses the token](troubleshooting.md#connect-refuses-the-token). When the server does not answer from this PC (Tailscale down, a firewall, a guest Wi-Fi), see [Another PC cannot reach the server](troubleshooting.md#another-pc-cannot-reach-the-server).

It then shows its plan and asks (`--answer consent=yes` without a terminal), and writes:

- `config.toml`: the server's address and bank, `server.mode = "remote"`, and the host's provider, model and prices in `[llm]`, never a key;
- the API key in `secrets.toml`;
- the server's id in `state.json`, so this PC notices if another server ever answers at that address.

Last, it runs setup's client steps: Claude Code's wiring, transcript retention, the background jobs, `backsight verify`, and the offer to import your past sessions ([backfill](glossary.md#backfill)), priced with the host's LLM. Nothing about the provider or backups: those are the host's. If the host extracts with a Claude subscription (the `claude-code` provider), your sessions are extracted on it too, so connect asks you to accept its terms first (`--answer ack_subscription=yes` without a terminal). If a setup step stops, `backsight setup --resume` goes on. Every flag is in [commands.md](commands.md#install-and-wiring).

Restart Claude Code (or run `/reload-plugins`): sessions open while you ran connect keep the server they started with until then.

A Claude Code session open while you ran connect started before this PC joined, so live sending leaves it to the backfill, even the turns you type after. Decline the backfill and it is not sent, unless you send it later with `backsight retain backfill` or `backsight retain flush --session <id>`.

## A PC that already has its own server

A PC that runs its own server can join a shared one with the same `backsight connect`. The plan adds what leaving its server means:

- **Its server is backed up, the archive verified, then stopped.** Its data and backups are kept. If the backup fails or does not verify, connect stops and nothing changes: see [connect stopped because the backup did not verify](troubleshooting.md#connect-stopped-because-the-backup-did-not-verify). The server must answer first: `backsight server start`.
- **Its past sessions are offered again** to the shared server, from the transcripts on this PC, counted and priced like the backfill offer: the host's LLM extracts them again.
- **Saved notes with no transcript left on this PC** stay in the old bank only: the plan counts them.

`backsight connect --undo` puts back this PC's `config.toml`, key and sends as they were before connect, and starts its own server again. Sessions sent to the shared server meanwhile stay there.

## Install the server on another machine over ssh

From a PC that uses the memory, one command installs the server on a Linux machine you reach over ssh, and joins it:

```sh
backsight server install --ssh user@host --plan
backsight server install --ssh user@host
```

- It runs your own `ssh`, with your keys and your `known_hosts`. An unknown or changed host key is refused, never accepted: connect once yourself with `ssh user@host` and check the fingerprint first.
- It reads the host, shows the plan and asks before it changes anything there; the LLM key goes to the host's secrets on stdin, never on a command line.
- It opens the server on the address your ssh reached. `--private <ip>` picks another: a tailnet address, or a LAN one with `--answer cleartext_lan=yes`, as for `server expose`.
- `--json` prints one progress line per step on stderr. `--non-interactive` never prompts: give `--answer consent=yes` and the LLM key with `--llm-key-from-file <file>`.
- After a failure, run the same command again: every step can run again, so it goes on from where it stopped. The host is never rolled back; `backsight setup --undo` there undoes its setup.

No ssh access to the host? `backsight server install --print-commands` prints the same steps as commands to run yourself, on the host then on this PC. Every refusal and its fix: [server install --ssh stopped](troubleshooting.md#server-install---ssh-stopped).

## Away from home

With a tailnet address, a laptop reaches the host from anywhere Tailscale is up; with a LAN address, only at home. A laptop that cannot reach the host, or a host that is off, costs you recall, not data:

- **Prompts.** backsight gives the server 3 s to accept a connection. Each prompt then costs at most about 3 s, until 3 prompts in a row failed: recall pauses for 5 minutes and prompts are no longer delayed (`network.breaker_*` in [configuration.md](configuration.md#network)). Hooks keep their time limits.
- **Notes** Claude stores wait in this PC's queue, and **sessions** in Claude Code's transcripts.
- **After an hour** without an answer, the next session's start line adds "⚠️ memory server silent since …", and doctor's `server-reach` check warns: when the server last answered, whether recall is paused, how many notes are queued and how many sessions wait. The `Server` line of `backsight stats` shows the same record, read on this PC without asking the server.
- **Back home**, the next `backsight jobs tick` (every 15 minutes) sends everything that waited.

Measured on a test machine, the prompt hook's 95th percentile was about 1.2 s with the client and the server on the same network, and about 1.4 s with 80 ms of delay added to the link.

## Different versions on the host and a PC

Each backsight pins one Hindsight version. When the server runs another, doctor's `version-skew` check on the client reports it: a warning when the major or minor version differs, information when only the patch does. Nothing is refused: hooks and recall keep working.

The bank's extraction settings are the host's. A client never sends its own, and its `backsight update apply` skips the server and the bank: doctor's `bank-config` check says so.

To update, run `backsight update apply` on the host first: it moves the server to its new pin and gives the bank the new settings. Then update each PC. The PC's `version-skew` check compares Hindsight versions, not backsight releases: it reports a difference until the PC's backsight pins the Hindsight version the server runs. See [update.md](update.md).

## A lost laptop

The laptop holds the API key. On the host, give the server a new one:

```sh
backsight server token --rotate --plan
backsight server token --rotate --out <file>
```

The old key and every token handed out before are refused from then on. The host follows on its own. On every other PC, run `backsight connect --token-file <file>` with the new token, then delete the file. Sessions open there follow at their next memory call. If the server does not run on the new key, the old key is put back and the server started on it, and the old token still works; when that fails too, the message says what to run.

## Restore and purge with several PCs

- **A restore on the host** ([Restoring a backup](backups-and-restore.md#restoring-a-backup)) gives the bank a new restore [epoch](glossary.md#epoch). Each client catches up at its next `backsight jobs tick`: it compares what it sent with what the server now holds, and sends again, from its own transcripts, the sessions the backup lacked. Meanwhile doctor's `restore-epoch` check says it has not caught up.
- **A purge** (`backsight forget <id> --purge --yes`, or `backsight forget --project <name> --purge`) on any PC writes [tombstones](glossary.md#tombstone) to the server. Every PC reads them before it sends anything, so the purged turns are never sent again from any PC's transcripts, not even after a restore ([Removing what was stored](privacy.md#removing-what-was-stored)).
- **A host replaced** by a fresh install and a restore of its backup ([Restoring on a new machine](backups-and-restore.md#restoring-on-a-new-machine)) keeps its server id, which lives in its data. A restore brings back the database, not `config.toml` nor `secrets.toml`, so the new host has a new API key and listens on `127.0.0.1` only: open it again there with `backsight server expose --private <ip> --out <file>`, then run `backsight connect --token-file <file>` on each client. Since the id is the same, each client's sends go on where they were. A host reinstalled without a restore has a new id: doctor's `server-id` check errors on every client, and they send nothing until you join the new server ([doctor: a different server answers at this address](troubleshooting.md#doctor-a-different-server-answers-at-this-address)).

## A session continued on two PCs

You copy a transcript to another PC and resume the session on both: a [split session](glossary.md#split-session). Nothing is lost: each PC sends the turns the server does not hold from it, as the session's next part. The part both PCs share may be stored twice. doctor's `split-session` check names such sessions, as information.

One case is caught later: both PCs sent the session's first part before the server stored either, and the later one replaced the other. The PC whose part was replaced reads the session back at a later `jobs tick` and sends its missing turns again. If it still cannot compare after 24 hours, it gives up, and doctor's `replace-unconfirmed` check warns: see [doctor: a session part was never confirmed](troubleshooting.md#doctor-a-session-part-was-never-confirmed).

After a restore of the host, this PC's part of such a session is sent again: stored twice rather than lost.

## What a client does not do

The server and its banks are the host's. On a client, the commands that manage the server (`server start`, `server stop`, `server status`, `server upgrade`, `server token`, `server expose`, `server llm check`, `backup create`, `restore`, …) refuse and name the command to run on the host, and so do `backsight bank apply` and `backsight guide index`, which write the host's banks. Nothing changes on the client. The full list: [commands.md](commands.md#server) and [A command says to run it on the server's host](troubleshooting.md#a-command-says-to-run-it-on-the-servers-host).
