# Troubleshooting

What to do when setup or memory stops working, and how to report a problem. Words you may not know are in the [glossary](glossary.md).

## Start with doctor

```sh
backsight doctor
```

doctor checks the whole install and changes nothing: the server, the bank, Claude Code's wiring, the jobs, the queues, backups, updates, disk and recent usage. Each line is `ok`, `info`, `WARN` or `ERROR`, and every problem comes with a `fix:` line, usually a command to run. `backsight doctor --json` gives the same report as JSON.

`backsight verify` goes further: it runs every hook and the memory tools as Claude Code does, then a store-and-recall round trip (`--no-round-trip` skips the round trip and its LLM call). `backsight verify --live` checks real use, read-only: your latest session, 30 minutes after its last turn, reached the server and gave at least one observation. "pending" means not yet, with how long to wait; "fail" comes with a fix.

## Setup stopped

Setup records each step it finished. Whatever stopped it, fix the cause it names, then:

```sh
backsight setup --resume
```

It goes on from the step that failed, with your earlier answers. `backsight setup --plan` shows the checks and steps without changing anything, and `backsight doctor` checks what is already installed.

### Docker refuses your user

Setup's `docker` check or its server step says `permission denied` about `/var/run/docker.sock`: your user cannot use Docker without `sudo`.

```sh
sudo usermod -aG docker $USER
```

Log out and back in (a new terminal is not enough), check that `docker info` works without `sudo`, then `backsight setup --resume`. Members of the `docker` group have root-level access to the machine; [rootless Docker](https://docs.docker.com/engine/security/rootless/) is the alternative. If Docker is not running: `sudo systemctl enable --now docker`. If `docker compose` is unknown (setup's `compose` check says `Docker Compose v2 is missing`), install the Compose v2 plugin ([docs.docker.com/compose/install](https://docs.docker.com/compose/install/)).

### A port is taken

Setup says `port 8888 is taken by another program` (or `9999`, the web UI's).

- **backsight's own server, from the run that stopped**: setup takes it for its own and goes on, unless you changed the `project` answer. Then stop it with `backsight server stop` first, or keep the earlier project.
- **Another program**: `ss -ltnp` shows which one. Stop it, or give backsight other ports: `backsight setup --answer api_port=<port> --answer ui_port=<port>`.

### The provider rejected the key

The server step fails on its LLM check with `the <provider> API rejected the key in llm_api_key`. The server it started keeps running. Set the key again, then resume:

```sh
backsight secret set llm_api_key
backsight setup --resume
```

With the `claude-code` provider on a server-only host (no Claude Code there), run `claude setup-token` on a machine that has Claude Code, and set its token on the host with `backsight secret set llm_api_key`.

The resume takes the running server for its own and checks the key again. Other provider answers name the provider's reason, with other fixes: a balance to top up (DeepSeek answers 402), a model name the provider does not know, or a wrong base URL. `backsight server llm check` tests the provider again, storing nothing.

### Setup seems stuck

On the first run, the server step downloads the server images and its models, about 2.4 GB: several minutes on a home connection, longer on a slow one. Setup shows Docker's download progress, then the time spent while the server's first start downloads its models. If it really stops, interrupt it and run `backsight setup --resume`.

### An image does not unpack

Setup pulls each image up to 3 times before it stops, and says what Docker's last line points at: the network, the disk, or an image Docker got but could not unpack. That last one reads `failed to extract layer … failed to unmount /var/lib/containerd/tmpmounts/… device or resource busy`. It happens on this machine, not on the network: something holds containerd's temporary mounts while it unpacks the layers, most often an on-access antivirus scanner.

1. Pull the image named in the message yourself, for example `docker pull pgvector/pgvector:pg18@sha256:…`: Docker keeps the layers it has, and a second pull usually passes.
2. Run `backsight setup --resume`.
3. If it keeps failing, look for mounts the failed pulls left: `findmnt | grep containerd/tmpmounts`, and remove each with `sudo umount <path>`. Excluding `/var/lib/containerd` from the scanner, or pausing it during setup, prevents it.

### Claude Code is missing or too old

Setup's `claude` check needs Claude Code 2.1.309 or newer, with `claude` on your PATH: `claude update`, or [install Claude Code](https://code.claude.com/docs/en/setup). A host that only serves the memory for your other PCs needs no Claude Code: run `backsight setup --role server` there, and setup does not look for `claude`.

## The server is down

Symptoms: no memories in your sessions, one "⚠️ backsight · memory unavailable (server unreachable)" message, doctor's `server` check in error.

```sh
backsight server status
backsight server start
```

While the server is down, nothing is lost: sessions stay in Claude Code's transcripts and notes wait in a local queue. They are sent once it is back. If doctor says Docker is not running, start it (`systemctl start docker`) and check that your user can run `docker` without `sudo`.

If the server rejects the API key, the key in `secrets.toml` and the server's `.env` differ: `backsight server render` writes the `.env` from your secrets again, then `backsight server start`.

On another PC, after `backsight server token --rotate` on the server's host: the server now runs on a new API key and the old token no longer works. Every other PC joins again with the new token, which `backsight server token --out <file>` writes on the host: carry the file to each PC and run `backsight connect --token-file <file>` there, then delete the file.

### The native server does not start

On the native runtime (`server.runtime = "native"`), the database's unit, `backsight-db.service` (`<server.project>-db.service`), checks the database before every start: from `backsight server start`, at boot, and when systemd restarts it. When the check refuses, neither unit starts, `backsight server start` prints the reason, and `journalctl --user -u backsight-db.service` shows it after a boot. The database lives in `home/.pg0/instances/hindsight` under the native directory (`<server.compose_dir>/native`), and the identifier of the one backsight created or restored is kept beside it, in `identity`.

| The message says the native database is | What happened | What to do |
|---|---|---|
| *missing* | The database's directory is gone (or the native directory was emptied, or `server.compose_dir` now points elsewhere). backsight creates a database only when it installs the native server, never at a plain start, so it does not create an empty one in its place: your sessions would start over on an empty memory. | Put the directory back if you moved or lost it (from your own copy of the disk), then `backsight server start`. Otherwise restore your last backup: `backsight restore <archive>`. |
| *empty* | The directory is there but holds no database (no `data/PG_VERSION`). | The same: put it back, or `backsight restore <archive>`. |
| *not the one backsight recorded* | Another database sits there: its identifier (`pg_controldata`'s "Database system identifier") is not the one in `identity`, for example a copy from another PC. | Put the right one back, or `backsight restore <archive>`. |
| *has no recorded identity* | A database is there but `identity` is missing, so backsight cannot tell it is yours. | `backsight restore <archive>`: it restores into a fresh database and records its identity. Do not write `identity` by hand to get past the check. |

`backsight restore <archive>` works on each of these: it moves the refused database aside, never deleting it, to `hindsight.refused-<Unix time>` beside it, creates a new database, restores the archive into it and records its identity ([after a refused native start](backups-and-restore.md#after-a-refused-native-start)). Delete a `.refused-` directory yourself once you no longer need it.

`a postgres (pid N) runs on … outside backsight-db.service: stopping it, never adopting it`, in the journal: a postgres started outside the unit (by hand, or left by an interrupted command) held the database. The check stops it with the bundled `pg_ctl` (a fast shutdown) and the unit then starts its own. If it is still running 30 seconds later, the start fails and names its pid: stop it (`kill -INT <pid>`), then `backsight server start`. `pg_ctl` stops the process the data directory's `postmaster.pid` names, so when that file names another pid (or is missing) the check stops nothing and the start fails naming both: stop the stray yourself the same way.

`fsync is off on the native database`: the database answered with `fsync` off, so an OS crash could corrupt it, and `backsight server start` stopped and disabled both units. The unit runs it with `fsync=on`; look for a drop-in that changes it (`systemctl --user cat backsight-db.service`), remove it, then `backsight server render` and `backsight server start`.

### After a reboot the server was down for a while (private exposure)

A server opened to other PCs (`backsight server expose --private <ip>`) listens on that address and on `127.0.0.1` from one container. When Docker starts at boot before the address exists (Tailscale or DHCP still coming up, or a new DHCP lease), the container cannot listen on it and does not start, and Docker does not try again: this machine's own memory is down too.

backsight starts it again on its own once the address is there: `backsight jobs tick` (every 15 minutes) and the next Claude Code session (at most every 5 minutes) run `backsight server start` in the background, its output in `logs/server-start.log` in the state directory (`backsight paths` shows it). A start that does not bring the server up is not repeated at every look: the next waits 15 minutes, then twice as long each time up to 6 hours, and the count starts over once a `backsight server start` brings the server up (the background one or yours) or backsight sees it stopped by you or no longer exposed on a private address, or, on Linux, the machine restarted since (`server-start-backoff.json` in the state directory holds the count; delete it to try at the next look), and a `server-start.log` over 1 MiB moves to `server-start.log.1`. A server you stopped with `backsight server stop` stays stopped. doctor's `server` check says whether the address is still missing.

To start Docker after Tailscale on Linux, so the address is there first:

```sh
sudo systemctl edit docker.service
```

and add:

```ini
[Unit]
After=tailscaled.service
Wants=tailscaled.service
```

To stop depending on the address, `backsight server expose --loopback` takes the server back to this machine only.

## A command says to run it on the server's host

`this PC is a client of the server at <url>: run backsight <command> on that server's host`: this PC's `server.mode` is `remote`, so the server runs on another machine, and the commands that manage it or write its banks only work there: [the list](commands.md#server). Log in to the machine at the address the message names and run the command it gives. Nothing changed on this PC.

## doctor: a different server answers at this address

doctor's `server-id` check in error, and the session banner "⚠️ a different memory server answers at the joined address": this PC joined a server, and the one answering at its address now has another id (or none). Its host was reinstalled without a restore, or the address now leads to another machine. Each server has its own id, kept in its data, so a host replaced by a fresh install and a restore of its backup keeps its id. Its API key is new, though, and it listens on `127.0.0.1` only (a restore brings back the database, not `config.toml` nor `secrets.toml`): open it there again with `backsight server expose --private <ip> --out <file>`, then join it from each PC with `backsight connect --token-file <file>`; their sends go on where they were ([Several PCs, one memory](several-pcs.md#restore-and-purge-with-several-pcs)).

While it lasts, this PC sends nothing: sessions stay in Claude Code's transcripts and notes in this PC's queue, so nothing is lost. Nor does it write anything else there: `forget` (with `--restore`, `--purge` or `--project`) refuses, `verify`'s round trip fails saying why, and no briefing page is created at session start; recall and the briefing still read what that server holds. To go on with the server now at that address, make a token on its host (`backsight server token --out <file>`) and join it again with `backsight connect --token-file <file>`. If the old server comes back at the address, sending resumes on its own at the next `backsight jobs tick`.

## doctor: a session part was never confirmed

doctor's `replace-unconfirmed` warning: in the last 30 days, this PC gave up checking a part of a session it sent. The first part of a document replaces whatever the server holds under that document, so when two PCs send it before the server stores either, the server keeps only the later one. Each PC therefore reads the document again after such a send, and sends its missing turns again as the next part (the `split-session` row of [Other doctor warnings](#other-doctor-warnings)). When the document still cannot be compared 24 hours after the send (absent with nothing to explain it, never readable, with no original_text to compare, or the server's queue never clear), the part is given up at its next look: reported in the tick's log and as a `replace_unconfirmed` telemetry event, which the check reads. It names up to 5 parts, newest first, as `<first 8 characters of the session>/<document>`.

Some of that session's turns from this PC may be missing on the server. They are not lost: they are still in Claude Code's transcript of the session on this PC, as long as its retention keeps it (doctor's `retention` check). No command sends a given-up part again: a known limit of this version. When the session named is this PC's latest settled one, `backsight verify --live` compares each of its documents on the server with what this PC sent, by where each part ends (its number and last turn), not by its turns: it fails naming one that is absent or ends elsewhere. It passes, naming it as continued on another PC, a document whose later part is another PC's, even when that PC's first part replaced this one's: there the missing turns do not show. For an older session, no command compares it. The warning ends 30 days after the last part given up.

## connect refuses the token

`backsight connect` checks the token and the server before it changes anything: after each of these refusals, nothing changed on this PC.

- **"the token holds the API key: pass it on stdin (--token -) or in a file (--token-file F)"**: the token was given on the command line, where your shell history, and an agent's transcript if an agent ran it, keep it. connect never shows it. Since the key is exposed, run `backsight server token --rotate --out <file>` on the server's host, then `backsight connect --token-file <file>` here (or `backsight connect --token -` and paste it at the hidden prompt).
- **"connect: the server rejected this token's key (rotated?)"**: the host gave its server a new API key (`backsight server token --rotate`) after this token was made. Make a new token on the host (`backsight server token --out <file>`) and run `backsight connect --token-file <file>` with it.
- **"the server at <url> is not the one this token was made for: get a fresh token there"**: another server answers at the token's address. Its host was reinstalled without a restore (a new server has a new id), or the address now leads to another machine. Make a token on the machine that runs the server now (`backsight server token --out <file>`).
- **"is a public address"**: the token names a public IP address, where the API key would cross the internet. On the server's host, open it on a LAN or tailnet address instead (`backsight server expose --private <ip>`), which hands out a new token.
- **"does not answer"**: this PC does not reach the server: see [Another PC cannot reach the server](#another-pc-cannot-reach-the-server).
- **"an update did not finish"**: an update stopped half way, and its way back would write `state.json` and `config.toml` back as they were before it, undoing part of the switch. Run `backsight doctor`, which says how to finish or roll back the update, then run `backsight connect` again (the same for `connect --undo`).
- **"a send of session <id> is still running"**: connect, and `connect --undo`, start this PC's sends over for the other server, and wait up to 2 minutes for the sends already running to end first (one would otherwise record its session as sent to the new server). Run the same command again a little later.
- **"a restore-epoch reconcile is running"**: after a restore of the server, `backsight jobs tick` catches this PC up with it, which may mark sessions to send again. connect, and `connect --undo`, wait up to 2 minutes for that to end before they start the sends over (it would otherwise mark sessions among the new server's). Run the same command again a little later.

## connect stopped because the backup did not verify

On a PC with its own server, `backsight connect` backs it up and verifies the archive (decrypts it and restores it into a throwaway database) before it stops that server. If the backup fails (**"backing up this PC's server failed"**) or the archive does not verify (**"the backup <file> did not verify"**), connect exits 20 and nothing changed: the server still runs, `config.toml`, the key and the sends are as they were; an archive made is kept.

- The backup failed: the server must run for its database to be dumped. Check `backsight server status`, start it with `backsight server start`, and run `backsight backup create` to see the error alone.
- The archive did not verify: run `backsight backup verify <file>` on the archive the message names; [backups-and-restore.md](backups-and-restore.md) says what its errors mean. Once `backsight backup create` then `backsight backup verify <file>` pass, run `backsight connect` again.

If this PC had no backup identity yet, the first backup made one: the message says it was added to `secrets.toml` (`backsight backup identity show` shows it, to keep elsewhere), and nothing else changed.

If its server does not answer at all (**"this PC's server at <url> does not answer"**), connect refuses before the backup: start it with `backsight server start`, then run `backsight connect` again.

Once the backup verified, connect stops the server under the maintenance lock, then writes. If the stop or a write fails, it starts the server again and puts the rest back (exit 30). If it exits 20 instead, the message says what is left; when `config.toml` could not be put back, it names the copy kept in `connect/<time>/` to copy over it.

## Another PC cannot reach the server

`backsight server expose --private <ip>` opens the server on one address of its host. Two refusals stop it before anything changes:

- **"is not an address of this machine"**: the address belongs to no network interface of the host, so the server could not listen on it. Pick one of the private addresses the message lists; for a tailnet, check that Tailscale is up (`tailscale ip -4`).
- **"the API key travels unencrypted on this network"**: the address is on a LAN, not a tailnet, and every request carries the API key in clear to anyone on that network. Prefer the host's tailnet address (Tailscale: `100.x`). On a network you trust, rerun the same command with `--answer cleartext_lan=yes`.

If the command exits 30, the server did not answer on the new address: `config.toml` was put back and the server started again on it. If it exits 20 after the change, `config.toml` was put back but the server did not start again on it, or `config.toml` could not be put back and the message names what it still sets: the message says what to run.

From the other PC, `backsight connect` says **"the server at <url> does not answer"**, or, on a PC that joined, doctor's `server` check says **"Hindsight unreachable at …"**. Nothing changed on that PC. Check, in order:

- On the host: `backsight server status` shows the server running, and `network.bind_ip` in its `config.toml` is the address the token names.
- From the other PC: `curl http://<ip>:8888/health` (the API port, 8888 unless you chose another). Any answer proves the path; a timeout or a refused connection does not.
- A tailnet: Tailscale is up on both machines (`tailscale status` lists the host), and no Tailscale ACL keeps this PC from the host's API port.
- A LAN: both machines are on the same network, not a guest Wi-Fi that isolates its devices, and no firewall on the host blocks the API port (with ufw: `sudo ufw allow from <subnet> to any port 8888 proto tcp`).

Then run `backsight connect` again; a PC that joined sends what waited at its next `backsight jobs tick`.

`backsight server install` (and setup's server step) gives the server an id, which every connection token carries. If the server does not answer that step, install warns `server id: …` and goes on: memory works on this machine without it. `backsight server install` again writes it, and so does the first connection token made: `backsight server expose --private <ip>`, or `backsight server token` once the server is exposed.

## server install --ssh stopped

`backsight server install --ssh user@host` reads the host, plans and asks before it changes anything there. Each refusal below, up to the consent and the key, leaves both machines as they were.

- **"this PC has no ssh client (OpenSSH)"** (exit 20): install OpenSSH's client here, or print the steps to run on the host yourself with `backsight server install --print-commands`.
- **"this PC is a server-only host"** (exit 2): a server-only host holds no memory of its own to join from. Run the command from a PC that uses the memory.
- **"…'s host key is not in your known_hosts and backsight never accepts an unknown one"** (exit 10): backsight never accepts a host key it does not know. Connect once yourself (`ssh user@host`), check that the fingerprint ssh shows matches the host's (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` there), accept it, then run the command again.
- **"the host key of … changed since you last connected"**, or **"is marked revoked"** (exit 20): an attack, or a host reinstalled since. Check with whoever runs it; backsight never removes a host key from `known_hosts`.
- **"ssh could not log in to … without a prompt"** (exit 10): backsight runs ssh without a password prompt. Load your key into the agent (`ssh-add`) or install it on the host (`ssh-copy-id user@host`). **"ssh could not reach …"** quotes ssh's own reason (name, network, firewall).
- **"did not answer the probe as sh does"** or **"cannot read what … runs"**: the host's login shell printed something else than the answer, or the probe failed there (its last line is quoted). Check that `ssh user@host uname -s` prints only `Linux`.
- **"installs on a Linux host only for now"**, **"for which backsight has no release"**: the host is not x86_64 or aarch64 Linux. Install backsight by hand there, as on any machine ([install.md](install.md)); `backsight server install --print-commands` prints the steps to run there.
- **"neither minisign nor OpenSSL 3"** (exit 10): the host needs one of them to check the release signature: install `minisign` (or OpenSSL 3) there. **"lacks tar"** or **"mktemp"**: install them there.
- **"on user@host: backsight … gave no answer backsight can read"**, **"… exited with code …"** or **"… answered something backsight here cannot read"** (exit 20): backsight on the host answered with something else than its JSON, or failed without one (its last stderr line is quoted). Check that `ssh user@host` runs a login shell that prints nothing extra, and that backsight there works (`backsight version` there).
- **"release … has no archive for linux/…"** (exit 20): the release matching this PC carries no build for the host's machine; nothing was changed there. Install by hand there, or use `backsight server install --print-commands` once a release covers it.
- **"… runs backsight x and this PC y"**: an install over ssh never replaces the backsight of an existing install (its updates migrate data). Run `backsight update apply` on the older one so both run the same release, then run the command again.
- **"is set up as a PC with Claude Code"**: the host already uses the memory itself. Open its server there instead (`backsight server expose --private <ip>`, then `backsight server token --out <file>`), carry the file here and run `backsight connect --token-file <file>`. **"is a client of another memory server"**: install the server on that server's host instead.
- **"the ssh connection reached … at …, which other PCs cannot use"** (exit 10): your ssh went through a loopback, a public address or a tunnel, so that address is no good for your other PCs. Rerun with `--private <ip>`, a LAN or tailnet address of the host (`tailscale ip -4` or `ip addr` there). **"is not a tailnet address"**: as for `backsight server expose`, a LAN address carries the API key in clear; rerun with `--answer cleartext_lan=yes` on a network you trust, or give a tailnet address.
- **"this backsight is a development build"**: no release matches it, so there is nothing to copy. Run the command from a released backsight (`backsight server install --print-commands` refuses a development build too). **"the release source has no version"**: the release of this PC's version cannot be found; with `--source <dir>`, point it at a directory holding that release.
- **"no terminal and nothing on stdin"**, or **"--non-interactive asks no key at a prompt"** (exit 10): the LLM key has nowhere to come from. Pass it with `--llm-key-from-file <file>` (first line), `--llm-key-from-env <var>` or piped on stdin (first line), never as an answer.
- **"would extract with a Claude subscription"** (exit 10): the host's provider is `claude-code`. Read the terms the question shows, then rerun with `--answer ack_subscription=yes`; at a terminal the command asks them itself.

Once the host started changing, a failure keeps what was done there (the host is never rolled back) and prints the same command as `resume`: every step can run again, so running it goes on from where it stopped: the release the host already runs is not sent again, setup resumes from its journal, and the LLM key is set again only when the command carries `--llm-key-from-file` or `--llm-key-from-env` (without either, a key already set on the host is kept).

- **"the release did not install on …"** (exit 20): the host's `install.sh` refused the release this PC sent (its reason is quoted: a signature or checksum mismatch, a cut transfer), and the host may still run its previous backsight. The host's `install.sh` trusts this version's signing key only: during a signing key rotation, a release signed by the next key may pass this PC's check and fail there. Run the command again; if it keeps failing, report it.
- **"on user@host: …"**: backsight on the host refused a step (setup, server expose, server token) with its own exit code and reason, for example Docker not running there. Fix it on the host as the message says, then run the same command here.
- **"the connection token could not be read back"**, **"came back unreadable"** or **"did not come back over ssh"** (exit 20): `backsight server token` ran on the host, but its token did not reach this PC whole. The file it wrote is removed once read; run the same command again, which writes a new one.
- **"the install on … stopped part way"** (exit 40), or **"the ssh connection to … dropped"**: the connection ended during a step, which may have run in part. Run the same command again.
- **"lingering is off"** (a warning): on the host, your user's services stop when you log out, so its backups and update checks only run while you are logged in there. Turn lingering on there: `sudo loginctl enable-linger <user>`.

Declining connect's own question once the host is installed leaves the host installed and this PC as it was (exit 0, connect skipped): running the same command later joins it.

To undo the install on the host, run `backsight setup --undo` there; on this PC, `backsight connect --undo` goes back to its own server when it had one.

## Memories are not recalled

1. Restart Claude Code, or run `/reload-plugins`: a session started before setup or an update has no plugin, or the old one.
2. Run `backsight doctor`. A `client` warning ("Claude Code wiring drifted") means the plugin, the permission rule or the shim changed: run `backsight client install`.
3. Check that this project is not paused or excluded: `backsight resume` in the session, or `backsight resume --project` in its directory (see [privacy.md](privacy.md)).
4. A new project has no memories yet: they appear after its first sessions are sent and processed.

`backsight why` shows what the hooks injected in the latest session, and why.

## Slow recall and GPU acceleration

doctor's `latency` check warns when the prompt hook's p95 is over 2.5 s across 7 days. On CPU, the reranker takes nearly all of a recall: it scores each candidate memory against your prompt. Computing the prompt's embedding is a small part.

**What a GPU changes in this version: nothing by itself.** The server runs embeddings and the reranker on the CPU. setup and doctor's `gpu` check only report the GPUs they find, and backsight installs no GPU driver, CUDA or other GPU runtime. Managed GPU acceleration is planned for a later version. The pinned server image (Hindsight 1.1.0) ships CPU-only builds of PyTorch and ONNX Runtime, so Hindsight's device options (`HINDSIGHT_API_EMBEDDINGS_ONNX_DEVICE=cuda`, for one) fail in it, and its local reranker finds no GPU.

Two ways to make recall faster, both in the server's `compose.override.yaml`, next to its `compose.yaml` (`~/.local/share/backsight/server` on Linux). backsight never writes that file, and backups include it.

**Fewer rerank candidates.** backsight sets `HINDSIGHT_API_RERANKER_MAX_CANDIDATES=30`. Fewer is faster, at the risk of missing a useful memory:

```yaml
services:
  hindsight:
    environment:
      HINDSIGHT_API_RERANKER_MAX_CANDIDATES: "20"
```

**Your own GPU reranker.** Run a reranking service on the GPU (as another service in `compose.override.yaml`, or on the host) and point Hindsight at it:

- `HINDSIGHT_API_RERANKER_PROVIDER: tei` with `HINDSIGHT_API_RERANKER_TEI_URL` for a Text Embeddings Inference server;
- or `HINDSIGHT_API_RERANKER_PROVIDER: cohere` with `HINDSIGHT_API_RERANKER_COHERE_BASE_URL` and `HINDSIGHT_API_RERANKER_COHERE_MODEL` for a server with a Cohere-compatible rerank API (set the model: Hindsight's default, `rerank-english-v3.0`, is English-only). Hindsight requires `HINDSIGHT_API_RERANKER_COHERE_API_KEY` to be set, even when your server ignores it.

Serve a multilingual reranker, ideally the same `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`, so prompts in any language still rank (see [daily-use.md](daily-use.md)). The service, its drivers and its GPU runtime are yours to install and keep: backsight does not check or manage them, and doctor's `latency` check tells you whether it helped.

Leave the embeddings as backsight sets them: the stored memories were embedded with `multilingual-e5-small`, and another model's vectors do not match them.

After editing the override, `backsight server start` recreates the server with it.

## Sessions are not saved

A session is sent when it ends, or by the 15-minute job once its last turn is 30 minutes old. `backsight retain flush` sends every unsent session now.

doctor's `writes` check counts what is waiting:

- **Queued notes** wait for the server: bring it back, then `backsight jobs tick`.
- **[Quarantined](glossary.md#quarantine) sessions or notes** were refused by the server. The reasons are in the quarantine folder doctor names. Fix the cause, then `backsight retain retry-quarantine`. A session is also quarantined when the server's document of it has no valid segment number (sending would replace it): backsight checks this before every append, not only when it rebuilds its position from the server.
- **The notes queue is full** (`hooks.outbox_max_mb`, 500 MB by default): an error. Bring the server back, then `backsight jobs tick`.
- **"nothing sent: this PC's sends started over for another server (connect) since this tick began"** (a `jobs tick`), or **"backfill stopped: this PC's sends started over for another server (connect) since it began"** (a `retain backfill`): `backsight connect` ran while they did. They send nothing more to the old server, delete nothing there (no purge done again, no briefing page pruned) and record nothing for it; the queued notes stay queued. The next `backsight jobs tick` sends to the new server; a backfill is offered again by connect's setup steps.

doctor's `jobs` check warns when a scheduled job is late. On Linux, `systemctl --user list-timers 'backsight-*'` shows the timers. User timers stop while you are logged out unless lingering is on (`loginctl enable-linger`); sessions are then sent at the next login.

## Extraction fails or costs too much

- doctor's `bank` check reports failed operations: the LLM provider refused or failed. Open the Hindsight web UI's operations page for the provider's errors.
- `backsight server llm check` tests the extraction model on a fixed sample, storing nothing: it checks the key, the model, the output format and the language.
- A wrong or expired key: `backsight secret set llm_api_key`, then `backsight server render` and `backsight server start`.
- With Ollama on Linux, the server cannot connect: Ollama listens only on `127.0.0.1` by default. See [Local models with Ollama](install.md#local-models-with-ollama).
- `backsight stats` shows the extraction tokens and cost of the last days.
- A backfill paused on its budget: `backsight retain backfill --plan`, then `backsight retain backfill --max-cost <higher>`.

## A lock or an unfinished operation

| doctor check | Meaning | What to do |
|---|---|---|
| `maintenance` | An update, backup or restore holds the maintenance lock. | Wait for it. If no backsight command runs, the lock is stale: doctor names the file to remove, and `backsight server start` removes a lock whose process is gone (a lock names its owner's process by its start time and boot, so a reused process id or a clock change does not mislead it; a lock written by an older build is judged by its process's start time against the lock's time, which a clock change can mislead, until it is taken again). |
| `locks` | A stale lock from a command that died. | The next run of that command takes it over. |
| `restore` | A restore did not finish. | Run the command doctor gives: rerun the restore, or put the safety archive back with `backsight restore <safety archive> --no-safety`; when it waits at its purge step or only the start on the API key failed, `backsight server start` finishes it. Do not cancel the operations a stalled purge step names (Hindsight's `DELETE /v1/default/banks/<bank>/operations/<id>`): one a worker is running still writes its document after the cancel, once the queue already looks empty, so a purged document can come back. See [backups-and-restore.md](backups-and-restore.md). |
| `update-incomplete` | An update did not finish. | Run the command doctor gives, usually `backsight update apply`, which resumes it. See [update.md](update.md). |
| `restore-epoch` | The server was restored and this machine has not caught up. Or, as an error naming a bank and a number of documents: purged documents are on the server again and the last reconcile could not purge them again; every send holds meanwhile. | `backsight jobs tick` (the 15-minute job does it); for the error, check `backsight server status` first. |
| `purged-documents` | A purged document is on the server again: an older backsight on another machine, a rollback, or another client unaware of purges appended to its id, and the server recreated it with what it sent; recall can return it until it is purged again. It names the document and its session. A warning while `jobs tick` will purge it (it waits for a queued append, or the tick has not looked yet); an error when it waits for you, or no tick looked for an hour. | For the error: `backsight retain purge-again` (with the `claude-code` provider or a server this machine does not run, `--allow-subscription`). When it says an older client keeps writing to it, update backsight on every machine that writes to the bank first. |
| `pending-restore` | Putting Claude Code's files back (`setup --undo` or `client config restore`) stopped midway: some files are as they were before that run, others not. | Run the command doctor gives: `backsight setup --undo` resumes the undo; `backsight client config restore <run>` finishes the restore (add `--force` if it refuses a file changed since). |
| `pending-restore`: files no longer hold backsight's edits | Something rewrote a Claude Code file backsight edited. | `backsight client config apply` puts the edits back, each file backed up first. |

## Other doctor warnings

| doctor check | What to do |
|---|---|
| `platform` (setup's check): `<os>/<arch> has no server image` | The server runs on linux, macOS or Windows, on amd64 or arm64. On another system, run the server on a machine that is one of those and connect this client to it (`backsight connect`). |
| `compose` (setup's check): Docker Compose v2 is missing | backsight runs the server as a compose project: install the Docker Compose v2 plugin, then `backsight setup --resume`. See [Docker refuses your user](#docker-refuses-your-user). |
| `reference` (information, in setup's checks and in doctor): pieces of an older Hindsight install are wired into Claude Code | Setup asks what to do with them (`retire` unwires them, `keep` leaves them); `backsight setup --undo` puts the old wiring back. doctor says the same and names `backsight client retire`, which unwires them. When it cannot read Claude Code's files, doctor warns. See [install.md](install.md#replacing-an-existing-hindsight-install). |
| `api_port`, `ui_port` (setup's checks): a port is taken | See [A port is taken](#a-port-is-taken). |
| `hook <arg>` (`backsight verify`): a hook failed | One check per hook, named by its argument (`hook session-start`). It fails when the hook exits with an error, prints something that is not JSON, or failed behind the supervisor (doctor shows it). Run `backsight doctor`, then `backsight client install` if its `client` check warns. |
| `transcripts` (information, in setup's checks): how many Claude Code transcripts this PC holds, their size and the oldest's date | Information, not a fault: that is what a backfill can read. See `retention` below for how long Claude Code keeps them. |
| `transcripts` (doctor): unknown transcript records this week | Claude Code wrote record types backsight does not know. A warning when a type is new this week, or when there are at least twice as many as last week and at least 5. Claude Code's transcript format may have changed: check that the types doctor names carry no conversation, then report it (below). |
| `backfill`: paused, or in progress | Information while `jobs tick` sends it (progress as done/total). A warning when it is paused: on its budget, `backsight retain backfill --plan`, then `backsight retain backfill --max-cost <higher>`; for another reason doctor gives, `backsight retain backfill` carries on. It also warns when the backfill state cannot be read. |
| `recall` (warning): over 5 % of the recall calls failed in 7 days (checked from 20 calls) | The server did not answer, or failed, some of the recall calls prompts make. `backsight server status` on its host; `backsight stats 7` shows when. See [The server is down](#the-server-is-down). |
| `latency` (warning): the prompt hook's 95th percentile is over 2500 ms in 7 days (checked from 20 recalls) | Recall is slow. See [Slow recall and GPU acceleration](#slow-recall-and-gpu-acceleration). |
| `learning` (warning): memory not learning | In the last 24 hours, sends were tried and the server accepted none. `backsight retain flush` shows why, then `backsight server status`. Information instead when sessions have run but none has ended or settled yet. See [Sessions are not saved](#sessions-are-not-saved). |
| `searches` (warning): searches after an injection fell by half | Over the week against the week before, at least 10 injections in each: Claude searches less after recall injects memories, which means the instructions stopped working or recall turned noisy. `backsight stats 14` and `backsight why` show what recall injects. |
| `update` (information): a release is available, or the last update check failed | A failed check (offline, rate limited) is no fault of the install. `backsight update check` looks again; with a release available, `backsight changelog` shows its notes and `backsight update apply` installs it. See [update.md](update.md). |
| `mcp` (`backsight verify`): no MCP handshake, or a memory tool missing | `backsight mcp` did not answer the MCP handshake or did not list every memory tool (the message names the tool). Check doctor's `client` check, run `backsight client install` if it warns, then `backsight verify` again. |
| `round_trip` (`backsight verify`): the store-and-recall round trip failed | The scratch bank could not be made, or nothing was extracted or recalled within the wait: the message says which step. `backsight server llm check` tests the extraction model; `backsight verify --no-round-trip` skips this step. |
| `retention`: Claude Code keeps transcripts 30 days | `backsight client retention` keeps them 3650 days, so backfill and rebuilds can read them. |
| `server-reach`: the server has not answered for over an hour (and the session banner "⚠️ memory server silent since …") | This PC records when its server last answered: at each recall and at each `backsight jobs tick`. The check says when that was, whether recall is paused (the breaker), how many notes are queued and how many sessions have turns not sent yet (counted from the transcripts by the last tick, sessions still open included). Once the server answers doctor again, the check says so and stops warning. `backsight stats` shows the same record on its `Server` line, without asking the server. Check the server on its host (`backsight server status` there) and the network between the two machines (for a tailnet, `tailscale status`). Nothing is lost meanwhile: sessions stay in Claude Code's transcripts and notes in this PC's queue, and the next tick after the server answers sends them. |
| A prompt takes about 3 s while the server is away (a laptop off the home network, a server address that silently drops packets) | backsight gives a server 3 s to accept the connection, then counts the call as failed; after 3 failed prompts the breaker pauses recall and prompts are no longer delayed. Nothing to fix unless the server should be reachable: see `server-reach` above. |
| `version-skew`: the server runs another Hindsight than this backsight pins (warning when the major or minor version differs, information when only the patch does) | Only on a PC that joined a server. Nothing is refused: hooks keep working and recall goes on. The two sides can read each other's data differently, though, so line them up. On the server's host, `backsight update apply` moves it to its backsight's pin; or update this PC's backsight to the host's version. |
| `split-session` (information): a session continued on two PCs | A transcript copied to this PC and resumed on both PCs: the server held it up to a turn of the other PC, which this copy does not contain. backsight reads the session's document before appending, so this is found whether the other PC continued it before or after this PC's last send; when both PCs send within moments of each other, it is found at the next send. Rather than leave this PC's turns unsent, backsight sent what this PC had not sent yet as the session's next part, numbered after the other PC's. One case is found later: both PCs sent the first part of the same document before the server stored either (a session copied before either PC sent it, or a project both PCs started in it), and the server kept only the later one, which replaced the other PC's turns there. The PC whose first part was replaced reads the document again at a tick at least 5 minutes after its send, and sends its missing turns again as the next part, even if the session has no new turn. So nothing is lost unless a part is given up (below); the part both PCs share may be stored twice. That tick waits while the server's queue may still change the document: a queued retain names it or names several documents, the queue is longer than backsight reads (500 operations per status), or the queue cannot be read; the session's other sends go on meanwhile. A document purged meanwhile is never sent again. After a restore of the host, the looks start over against the restored document, and the catch-up may send the whole session again from the PC that did not send its last part, as its transcript holds it, the shared part included. After 24 hours a part still unconfirmed (its document absent with nothing to explain it, never readable, with no original_text to compare, or the queue never clear) is given up at its next look (a tick reads at most 50 documents; the others wait for the next tick): never recreated on the server, it is reported in the tick's log and by doctor's `replace-unconfirmed` warning (see [doctor: a session part was never confirmed](#doctor-a-session-part-was-never-confirmed)), and `backsight verify --live` cannot tell whether that PC's turns are missing once another PC's part follows them. The check names the sessions. Nothing to fix. |
| `scheduler`: entries older than this binary's | `backsight jobs refresh` rewrites them without stopping a running job. |
| `bank-config`: the bank holds an older backsight template | `backsight bank apply` sends this version's (`update apply` and `client install` do it too). |
| `bank-config`: bank settings were changed on the server | Someone edited backsight's keys (the UI, the API). `backsight bank apply --force` puts backsight's values back and overwrites those edits; leave it if you made them on purpose. |
| `bank-config`: an older template, or a change made on the server | backsight has no record of the template it sent. `backsight bank apply` sends this version's, over any edit to its keys. |
| `bank-config` (info): the server's host applies the bank's settings | This PC is a client of a shared server: the bank's settings are the host's backsight's, and this PC never sends its own. Keys that differ only mean the host runs another backsight version. To give the bank a newer template, update backsight on the host, or run `backsight bank apply` there. |
| `backup`: no backup, or the last one is old | `backsight backup create`; check the daily job (`jobs` check). Archives made for another backup key (a previous install's, its identity deleted) do not count: this machine cannot decrypt them; `backsight backup identity import` adds that key if you saved it. |
| `backup` (info): backups are made on the server's host | This PC is a client of a shared server: its memory lives on the server, and the server's host backs it up (`backsight backup create` there; this PC refuses it). Archives this PC made before it joined, of its own server, stay where they are. |
| `binary`: not self-verified, or does not match its release | Reinstall the release with `install.sh`, then `backsight client install`. |
| `config`: unknown key or invalid value | Correct the key doctor names in `config.toml`. Unknown keys are refused, not ignored. |
| `config`: secrets file readable by others | `chmod 600` on the file doctor names. |
| `disk`: low free space | Free space where doctor says: the queues, backups and the server's database grow there. |
| `untagged`: memories without a project tag | A server-side problem: report it (below). |
| `noise-transverse`: over 20 % of the transverse pool's observations name another project | New sessions go to the right project. Older ones: `backsight forget --project <transverse alias> --source subagent --plan`, then decide, or leave them. |
| `noise-transverse`: growing (its share is 5 points and 3 observations or more above last week's, in a pool of 50 or more) | Something files work in the transverse pool again. `backsight retain reattribute --plan` shows what today's rules would move; the `attribution` check shows how often turns would be filed elsewhere. |
| `reattribution`: sessions mix projects in their documents, or the daily re-attribution does not run | An older version sent a session that moved between repositories as one document. `backsight retain reattribute --plan` lists them with the cost of sending them again per project; `--answer resend=yes` does it, `--answer resend=no` keeps them (the check then only informs). The daily job moves in place, with no extraction, every document that belongs whole to another project, after a backup it verified; when it cannot (backups off, a remote server, a backup that did not verify, the `claude-code` provider), the check says why and `backsight retain reattribute` does it by hand. |
| `noise-headless`: headless (SDK) sessions were sent before they were skipped | `backsight forget --project <project> --plan` for each project doctor names, once the plan shows nothing else worth keeping. |
| `noise-subagent` (info): the share of raw facts that came from subagent reports | Information, not a fault. If one project's subagent reports drown its memories: `backsight forget --project <project> --source subagent --plan`, then decide. |
| `attribution` (info): how often the path rule would file a turn elsewhere | Information, not a fault. Subagent reports already follow the repository their work is in; for the main conversation the rule only records what it would have done, and this check shows how often that differs. |
| `own-owners` (info): the remote owners whose repositories count as yours | Learnt from repositories you committed to and started a recorded session in. One that is not yours (a project you only contributed to): `projects.deny_owners = ["<host/owner>"]` in `config.toml`. |
| `remotes`: two repositories share a project name | Their memories mix in one pool. Give one of them its own name: `project = "<another name>"` in a `.backsight.toml` at its root, left untracked (listed in `.git/info/exclude`): a file git tracks is not trusted for `project`. A remote no session used in the last 7 days is ignored, and a name every session took from an explicit `.backsight.toml` `project` key is not warned: that is a rename (the old name kept) or a pool you share on purpose. |
| `backsight-toml` (info): a `.backsight.toml` tracked by git sets `project` or `related` | Those two keys are ignored: a repository you clone could otherwise choose the project your sessions are stored under, or pull other projects' memories into its sessions. `retain = false` still applies. If the file is yours: `git rm --cached .backsight.toml`, add `.backsight.toml` to `.git/info/exclude` and commit the removal; the file stays on disk, untracked, and applies again. doctor looks at its own directory and at running sessions'. |
| `notes` (info): notes stored in the last 7 days | Information, not a fault: Claude stores facts without asking. Review them with `backsight memory notes` (`--project transverse` for the ones that reach every project); remove a wrong one with `backsight forget`. |
| `transverse-keyword` (warn): documents tagged `project:transverse` | An older replay of a `memory_store` call that named `transverse` kept the word as a project, which a pool recall never searches. Run `backsight retain reattribute`: it re-tags them to the canonical alias in place (the daily `jobs maintain` does it too). Notes of real projects are never moved. |
| `feedback-journal`: cannot append to the journal | The record of which memories were shown to Claude and which it used is lost until it can be written; hooks and recall go on. Make the folder doctor names, and its files, writable by your user, and check free space there. |
| `feedback-journal`: above 200 MB | Move the month files older than two months out of the folder doctor names: the counts already settled stay, the tick reads only the last two months. A later `backsight feedback rebuild` would drop what the moved files counted. |
| `feedback-ledger`: not settled for more than a day | The 15-minute `jobs tick` settles the journal into per-memory counts. Run `backsight jobs tick`: its output says why if it cannot; the `jobs` check says whether the scheduler runs it. |
| `feedback-ledger`: ledger unreadable (damaged, or written by another version) | A damaged one is kept aside as `ledger.json.corrupt-<time>` and rebuilt from the journal by the next `jobs tick`. One written by another version stays as it is: `backsight feedback rebuild` recomputes it from the journal, and until then the tick settles nothing. |
| `feedback-shared`: the other machines' counts not fetched for more than a day | The 15-minute `jobs tick` fetches them every 6 hours while the server answers. Run `backsight jobs tick`: its output says why if it cannot; the `jobs` check says whether the scheduler runs it. Until then recall uses the counts fetched last. `share_ledger = false` in `[feedback]` stops sharing. |
| `reference-server` (info): the old Hindsight server can be stopped | Shown once `backsight backup verify` passed on backsight's newest backup within the last 7 days. Run the stop command doctor gives: the old server's data is kept, and its start command brings it back. See [install.md](install.md#replacing-an-existing-hindsight-install). |

## Reporting a problem

```sh
backsight report
```

`report` builds a diagnostic: versions, the doctor report, local counts and recent backsight errors. It is redacted: no memory text, prompt, transcript, secret or server answer; your home directory, user name, machine name, bank, the host of a server on another machine (backsight's or the reference one), git remotes, project names and backsight's directories are replaced by placeholders. So is where your machines live: private, CGNAT, link-local and ULA addresses show as `<lan-ip>`, hosts under `.local`, `.lan`, `.home.arpa` or `.internal` as `<lan-host>`, and the user part of a URL as `<user>`; loopback and public addresses stay. A git remote is hidden in every form an error quotes it: whole, as `host:owner/repo`, without its host, and its owner alone. Project names are every one this machine knows: the bank's project tags (when the server answers), the projects of local sessions, cursors and telemetry, the names `.backsight.toml` files give, and the transverse aliases; names everyone's install shares (`.claude`, `backsight`) stay. A name under 4 characters is hidden as a path segment, after `project:` or `--project`, and in the doctor check that lists it, not as an ordinary word. It prints:

- a link that searches existing GitHub issues for the same error;
- a link that opens a new issue with a title and a summary filled in;
- the full diagnostic to paste into it.

It sends nothing. Read the diagnostic, then decide whether to file it. Security problems go through GitHub's private vulnerability reporting instead, see [SECURITY.md](https://github.com/Barben360/backsight/blob/main/SECURITY.md).
