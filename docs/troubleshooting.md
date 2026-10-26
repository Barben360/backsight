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

Setup's check or its server step says `permission denied` about `/var/run/docker.sock`: your user cannot use Docker without `sudo`.

```sh
sudo usermod -aG docker $USER
```

Log out and back in (a new terminal is not enough), check that `docker info` works without `sudo`, then `backsight setup --resume`. Members of the `docker` group have root-level access to the machine; [rootless Docker](https://docs.docker.com/engine/security/rootless/) is the alternative. If Docker is not running: `sudo systemctl enable --now docker`. If `docker compose` is unknown, install the Compose v2 plugin ([docs.docker.com/compose/install](https://docs.docker.com/compose/install/)).

### A port is taken

Setup says `port 8888 is taken by another program` (or `9999`, the web UI's).

- **backsight's own server, from the run that stopped**, may hold it: `backsight server status` shows it running. Stop it with `backsight server stop`, then `backsight setup --resume`; setup starts it again.
- **Another program**: `ss -ltnp` shows which one. Stop it, or give backsight other ports: `backsight setup --answer api_port=<port> --answer ui_port=<port>`.

### The provider rejected the key

The server step fails on its LLM check: the provider answered 401 or 403, or the message says the key is invalid or unauthorized. Set the key again, then resume:

```sh
backsight secret set llm_api_key
backsight setup --resume
```

If the resume then stops on a taken port, that is backsight's own server: see [above](#a-port-is-taken). Other provider answers mean other fixes: a balance to top up (DeepSeek answers 402), a model name the provider does not know, or a wrong base URL. `backsight server llm check` tests the provider again, storing nothing.

### Setup seems stuck

On the first run, the server step downloads the server image and its models, about 2.3 GB: several minutes on a home connection, longer on a slow one. `docker images` in another terminal shows the images arriving. If it really stops, interrupt it and run `backsight setup --resume`.

### Claude Code is missing or too old

Setup needs Claude Code 2.1.309 or newer, with `claude` on your PATH: `claude update`, or [install Claude Code](https://code.claude.com/docs/en/setup).

## The server is down

Symptoms: no memories in your sessions, one "⚠️ backsight · memory unavailable (server unreachable)" message, doctor's `server` check in error.

```sh
backsight server status
backsight server start
```

While the server is down, nothing is lost: sessions stay in Claude Code's transcripts and notes wait in a local queue. They are sent once it is back. If doctor says Docker is not running, start it (`systemctl start docker`) and check that your user can run `docker` without `sudo`.

If the server rejects the API key, the key in `secrets.toml` and the server's `.env` differ: `backsight server render` writes the `.env` from your secrets again, then `backsight server start`.

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
- **[Quarantined](glossary.md#quarantine) sessions or notes** were refused by the server. The reasons are in the quarantine folder doctor names. Fix the cause, then `backsight retain retry-quarantine`.
- **The notes queue is full** (`hooks.outbox_max_mb`, 500 MB by default): an error. Bring the server back, then `backsight jobs tick`.

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
| `pending-restore` | Putting Claude Code's files back (`setup --undo` or `client config restore`) stopped midway: some files are as they were before that run, others not. | Run the command doctor gives: `backsight setup --undo` resumes the undo; `backsight client config restore <run>` finishes the restore (add `--force` if it refuses a file changed since). |
| `pending-restore`: files no longer hold backsight's edits | Something rewrote a Claude Code file backsight edited. | `backsight client config apply` puts the edits back, each file backed up first. |

## Other doctor warnings

| doctor check | What to do |
|---|---|
| `retention`: Claude Code keeps transcripts 30 days | `backsight client retention` keeps them 3650 days, so backfill and rebuilds can read them. |
| `scheduler`: entries older than this binary's | `backsight jobs refresh` rewrites them without stopping a running job. |
| `bank-config`: the bank holds an older backsight template | `backsight bank apply` sends this version's (`update apply` and `client install` do it too). |
| `bank-config`: bank settings were changed on the server | Someone edited backsight's keys (the UI, the API). `backsight bank apply --force` puts backsight's values back and overwrites those edits; leave it if you made them on purpose. |
| `bank-config`: an older template, or a change made on the server | backsight has no record of the template it sent. `backsight bank apply` sends this version's, over any edit to its keys. |
| `backup`: no backup, or the last one is old | `backsight backup create`; check the daily job (`jobs` check). |
| `binary`: not self-verified, or does not match its release | Reinstall the release with `install.sh`, then `backsight client install`. |
| `config`: unknown key or invalid value | Correct the key doctor names in `config.toml`. Unknown keys are refused, not ignored. |
| `config`: secrets file readable by others | `chmod 600` on the file doctor names. |
| `disk`: low free space | Free space where doctor says: the queues, backups and the server's database grow there. |
| `untagged`: memories without a project tag | A server-side problem: report it (below). |
| `noise-transverse`: over 20 % of the transverse pool's observations name another project | New sessions go to the right project. Older ones: `backsight forget --project <transverse alias> --source subagent --plan`, then decide, or leave them. |
| `noise-headless`: headless (SDK) sessions were sent before they were skipped | `backsight forget --project <project> --plan` for each project doctor names, once the plan shows nothing else worth keeping. |
| `noise-subagent` (info): the share of raw facts that came from subagent reports | Information, not a fault. If one project's subagent reports drown its memories: `backsight forget --project <project> --source subagent --plan`, then decide. |
| `remotes`: two repositories share a project name | Their memories mix in one pool. Give one of them its own name: `project = "<another name>"` in a `.backsight.toml` at its root, left untracked (listed in `.git/info/exclude`): a file git tracks is not trusted for `project`. A remote no session used in the last 7 days is ignored, and a name every session took from an explicit `.backsight.toml` `project` key is not warned: that is a rename (the old name kept) or a pool you share on purpose. |
| `backsight-toml` (info): a `.backsight.toml` tracked by git sets `project` or `related` | Those two keys are ignored: a repository you clone could otherwise choose the project your sessions are stored under, or pull other projects' memories into its sessions. `retain = false` still applies. If the file is yours: `git rm --cached .backsight.toml`, add `.backsight.toml` to `.git/info/exclude` and commit the removal; the file stays on disk, untracked, and applies again. doctor looks at its own directory and at running sessions'. |
| `notes` (info): notes stored in the last 7 days | Information, not a fault: Claude stores facts without asking. Review them with `backsight memory notes` (`--project transverse` for the ones that reach every project); remove a wrong one with `backsight forget`. |
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
