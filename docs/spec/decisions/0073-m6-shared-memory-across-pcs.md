# 0073 — M6 in five blocks: one memory shared by a user's PCs, proven on libvirt VMs

- **Date**: 2026-10-27
- **Status**: accepted
- **Decided by**: author (2026-10-27: the audience, the VM bench, transport stays as specified, "never lose" for a session continued on two PCs; "I approve as is"), on the agent's proposal; every point marked *agent's call* is open to veto

## Intent
M6 is for **anyone who wants one memory shared between their PCs** (author, 2026-10-27). The usual shape: one machine (often the desktop, sometimes a home server) holds the backsight server; laptops and other PCs connect to it. A laptop away from home keeps working: notes queue locally, the breaker (M2) stops network attempts, and everything goes out once the server answers again. The author's own machines are not the target: 0033 still keeps the work machine's memory separate.

### Goals
- A second PC joins an existing memory with one command and no hand edit (§1 criterion 1 applied to a second machine).
- Several clients on one server never store a session twice (0038), including a transcript copied between PCs and two clients flushing at once. The one exception is a copied session then continued on both PCs, where nothing is lost and the shared part may be stored twice (below).
- Every client tells its user plainly whether it reaches the server, since when, and what is waiting to be sent.
- The server's host keeps owning backups, updates of the server, restore and purges; clients reconcile with what it does (0044 epochs, purge tombstones).

### Non-goals
- **Encrypted transport.** The author kept §11.6 as is (2026-10-27): API and UI keys mandatory, public addresses refused, a tailnet address preferred, any other private address only after the `cleartext_lan=yes` warning. The TLS proxy and managed SSH tunnel move to M7, before the stable release.
- Merging two existing memories (0033): a PC that already had its own server re-sends its transcripts to the shared one; its old bank is kept, not merged.
- Per-machine keys or per-bank permissions: Hindsight has one shared key (C10).
- macOS and Windows clients (M7).

### Anti-goals (failure even if it "works")
- A hook that blocks or slows a prompt past its deadline because the server is far or down.
- A client that silently stops sending: an unreachable server must show in `doctor`, the banner and `stats`.
- A client that touches the server's data or lifecycle (backup, upgrade, `--delete-data`, restore) from another PC.
- The API key on a command line, in shell history, in a log or in `report`.

## Decision

### Topologies (both supported)
1. **A PC hosts**: the PC keeps its local install and switches to `network.exposure = "private"`; it stays a client of its own server. *Agent's call*: private exposure publishes the ports on both `127.0.0.1` and `network.bind_ip` (today `render.go` publishes only on `bind_ip`), so the host's own `server.url` stays loopback and keeps working when the LAN address changes.
2. **A headless host**: a machine with Docker and no Claude Code runs the server role only, installed by `server install --ssh user@host` or by hand with `--print-commands`.

### The connection token (*agent's call* on the format)
`bst1.` + base64url of JSON `{url, bank, api_key, ui_url, hindsight_version, server_id, llm: {provider, model, prices}}`. The `llm` part (never its key) lets a remote client estimate a backfill's cost, which it cannot otherwise see (`purge_again.go`'s `remoteHold`).
- **`server_id` belongs to the data** (*agent's call*): a random id written into the control bank at the first server install. A restore brings it back, so a host replaced by fresh install + restore keeps its clients (they only need the new URL, `connect` again); a fresh install without restore gets a new id. A client that finds another id at its address stops sending (doctor error) until `connect` runs again.
- **The key never passes through an agent** (§11.2, §13): the token is read from stdin (`connect --token -`) or a file (`--token-file F`), never an argument. `server token` writes it to a 0600 file (`--out F`) and prints it only when stdout is a terminal; with a non-terminal stdout and no `--out` it refuses. `server token --rotate` generates a new API key, re-renders and restarts the server, and writes the new token the same way (a lost laptop: every other client runs `connect` again). redact gains the `bst1.` pattern, so a token pasted into a session is masked before retain, `report` and logs. `server install --ssh` carries the token over ssh and never prints it.

### The bank belongs to the host (*agent's call*, from the anti-goal "a client never touches the server's lifecycle")
`bank apply` and `guide index` run only where `server.mode = "local"`. In remote mode `client install` and `update apply` skip them (today `update apply` runs both in remote mode, `update_apply.go` `planDocs`/`bank`); doctor's `bank-config` check on a client is info naming the host's command. Clients on different versions therefore never overwrite each other's bank settings; the host's binary is the one whose template applies.

### Blocks (each closed by its own reviews, then the M6 review)
1. **Server side.**
   - `server expose --private <ip> [--plan]` and `server expose --loopback`: switches an existing install's exposure, re-renders, restarts (maintenance lock, health and smoke after), and prints the token. A non-tailnet address needs `--answer cleartext_lan=yes` after the warning; a public address is refused (already in config validation).
   - `server token [--rotate]` as above.
   - **Server-only role**: `setup --role server` (and the remote half of `--ssh`) skips every Claude Code step (pre-flight `claude` check, `client install`, retention, backfill) and installs the scheduler entries `jobs maintain` needs on the host (backups, update check, purge re-apply). doctor on such a host reports the client checks as "not a client". *Agent's call*: `role` is recorded in `config.toml` (`server.role = "both" | "server"`, default `both`); a remote-mode client is `server.mode = "remote"` as today.
2. **Client side.**
   - `connect --token - [--plan]`: checks health, the key, the bank and the Hindsight version against `docs/compatibility.md`'s pin, then writes `server.url`, `server.bank`, `server.mode = "remote"` and the key into secrets, and records `server_id`. Then `setup` runs its client steps (client install, retention, jobs, backfill with its cost question).
   - **A PC that already has its own server** (*agent's call*): `connect` shows the plan: its transcripts are re-sent to the shared server (cost estimate as in setup's backfill question, from the token's `llm` part, consent asked); backfill already replays the explicit saves those transcripts hold. Saves whose transcripts are gone stay in the old bank only: the plan counts them (read from the old bank before it stops). Its local server is stopped with `--stop` semantics after a verified backup, its data and backups kept. `connect --undo` points it back at its own server.
   - **A session continued on two PCs** (author, 2026-10-27: "never lose" wins over "never store twice"): when a transcript copied to a second PC is resumed on both, the second PC may not find the server's `last_uuid` in its copy. It then sends the records its own sent-index lacks as the next segment; the shared part may be stored twice, and doctor reports it as info (`split-session`, with the session id). Goal 2's "never twice" holds for every other case, which the bench proves.
   - **Bank refusals in remote mode**: `client install` and `update apply` skip `bank apply` and `guide index` (above).
   - **Version skew**: a client whose compatibility pin differs from the server's Hindsight version warns in doctor (info when only the patch differs); hooks keep working. `update apply` in remote mode already skips `server upgrade`; doctor names the host's command.
   - **Reachability**: doctor `server-reach` (last successful call, breaker state, outbox size, sessions waiting) and the SessionStart banner when the server has not answered for over an hour. `server_id` mismatch (a different server answers at the address) stops sending and is a doctor error until `connect` is run again.
   - **Remote-mode refusals**: `uninstall --delete-data`, `backup create`, `restore`, `server *` refuse with the host's command (some already do; the block makes it complete and tested).
3. **`server install --ssh user@host` and `--print-commands`** (§11.6, 0024): system `ssh` with the user's keys and `known_hosts` (an unknown host key is refused, never auto-accepted); copies the release matching the client's version and verifies its signature on the host; runs `setup --role server --non-interactive --json` there with the LLM provider answers and the key passed over ssh stdin; brings back the token and runs `connect`. `--print-commands` prints the same steps for a user without ssh access.
4. **Proof on the VM bench** (below), then a container chain in CI.
   - Two clients on one server: the VM client and an isolated client profile on the host (fresh-install.sh's isolation: own `HOME`, Claude Code profile, compose project; the author's install is never touched).
   - Dedup: a session's transcript copied from client A to B, then B's backfill sends nothing of it; both clients flush the same copied session at once and the server holds each record once (count by record uuid); a copied session resumed on both PCs loses no record of either continuation and shows in doctor's `split-session`.
   - Offline laptop: the client VM's network cut (`virsh domif-setlink … down`) during sessions and notes; hooks within deadlines; doctor and banner show it; link back up; everything sent once.
   - Restore and purge across clients: a restore on the host makes both clients reconcile (0044); a `forget --purge` on one client is honoured by the other's later sends (tombstones).
   - Key rotation: the old token is refused, `connect` with the new one recovers, nothing lost.
   - Latency: prompt-hook p95 measured with `tc netem` at 0, 20 and 80 ms added delay on the client VM. Recorded, not gated (*agent's call*), except that no hook exceeds its deadline.
   - CI (*agent's call*): a cheaper container chain on the hosted runner (server stack + two client homes on the Docker network, mock LLM) re-runs the dedup and rotation proofs on every PR; the VM bench runs locally per block and before the release.
5. **Docs, spec and the M6 review.** The spec follows this decision: §11.6 (`--ssh` runs `setup --role server --json` on the host, superseding 0024's `server install --json` there, so the host gets its scheduler entries too), §17's command list (`server expose`, `server token`, `connect --token-file`, `connect --undo`, `setup --role`), Appendix A.1 (`server.role`), and an INDEX row amending 0024. A "Several PCs, one memory" page in `docs/` (embedded in `guide`), the agent guide's remote section, troubleshooting rows for every new doctor check, `docs/configuration.md` for `server.role`. Then the M6 review (conformance, safety, test quality), as for M4 and M5.

### The VM bench (author, 2026-10-27: libvirt VMs, driven autonomously)
- `scripts/vm-bench.sh up|down|reset|ssh <vm>` on `qemu:///system`, `default` NAT network and pool; Ubuntu 24.04 cloud image (checksum verified, cached), `virt-install --cloud-init` with a throwaway ssh key under the bench directory.
- `bs-m6-server`: 4 vCPU, 8 GB, Docker from Ubuntu's packages; installed only through `server install --ssh` (block 3) or `--role server` (earlier blocks).
- `bs-m6-client`: 2 vCPU, 4 GB, systemd user session with linger (real timers), the `claude` CLI for `client install`; sessions are synthetic transcripts (no Claude Code login, no cost).
- LLM: Hindsight's mock LLM, as in fresh-install.sh. No paid extraction anywhere in the bench.
- Every resource is named `bs-m6-*`; `down` removes only those. Nothing in `~/.claude`, the live compose project `backsight` or its volumes is touched (convention of 2026-10-25).

### Exit (ROADMAP M6)
Two clients on one backsight server, a session never retained twice, a second PC joined with one command and no hand edit, all proven on the VM bench and the CI chain; the M6 review addressed.
