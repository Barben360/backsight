# Troubleshooting

What to do when memory stops working, and how to report a problem.

## Start with doctor

```sh
backsight doctor
```

doctor checks the whole install and changes nothing: the server, the bank, Claude Code's wiring, the jobs, the queues, backups, updates, disk and recent usage. Each line is `ok`, `info`, `WARN` or `ERROR`, and every problem comes with a `fix:` line, usually a command to run. `backsight doctor --json` gives the same report as JSON.

`backsight verify` goes further: it runs every hook and the memory tools as Claude Code does, then a store-and-recall round trip (`--no-round-trip` skips the round trip and its LLM call). `backsight verify --live` checks real use, read-only: your latest session, 30 minutes after its last turn, reached the server and gave at least one observation. "pending" means not yet, with how long to wait; "fail" comes with a fix.

## The server is down

Symptoms: no memories in your sessions, one "memory unavailable" message, doctor's `server` check in error.

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
- **Quarantined sessions or notes** were refused by the server. The reasons are in the quarantine folder doctor names. Fix the cause, then `backsight retain retry-quarantine`.
- **The notes queue is full** (500 MB): an error. Bring the server back, then `backsight jobs tick`.

doctor's `jobs` check warns when a scheduled job is late. On Linux, `systemctl --user list-timers 'backsight-*'` shows the timers. User timers stop while you are logged out unless lingering is on (`loginctl enable-linger`); sessions are then sent at the next login.

## Extraction fails or costs too much

- doctor's `bank` check reports failed operations: the LLM provider refused or failed. Open the Hindsight web UI's operations page for the provider's errors.
- `backsight server llm check` tests the extraction model on a fixed sample, storing nothing: it checks the key, the model, the output format and the language.
- A wrong or expired key: `backsight secret set llm_api_key`, then `backsight server render` and `backsight server start`.
- `backsight stats` shows the extraction tokens and cost of the last days.
- A backfill paused on its budget: `backsight retain backfill --plan`, then `backsight retain backfill --max-cost <higher>`.

## A lock or an unfinished operation

| doctor check | Meaning | What to do |
|---|---|---|
| `maintenance` | An update, backup or restore holds the maintenance lock. | Wait for it. If no backsight command runs, the lock is stale: doctor names the file to remove. |
| `locks` | A stale lock from a command that died. | The next run of that command takes it over. |
| `restore` | A restore did not finish. | Run the command doctor gives: rerun the restore, or put the safety archive back with `backsight restore <safety archive> --no-safety`. See [backups-and-restore.md](backups-and-restore.md). |
| `update-incomplete` | An update did not finish. | Run the command doctor gives, usually `backsight update apply`, which resumes it. See [update.md](update.md). |
| `restore-epoch` | The server was restored and this machine has not caught up. | `backsight jobs tick` (the 15-minute job does it). |
| `pending-restore` | Putting Claude Code's files back (`setup --undo` or `client config restore`) stopped midway: some files are as they were before that run, others not. | Run the command doctor gives: `backsight setup --undo` resumes the undo; `backsight client config restore <run>` finishes the restore (add `--force` if it refuses a file changed since). |
| `pending-restore`: files no longer hold backsight's edits | Something rewrote a Claude Code file backsight edited. | `backsight client config apply` puts the edits back, each file backed up first. |

## Other doctor warnings

| doctor check | What to do |
|---|---|
| `retention`: Claude Code keeps transcripts 30 days | `backsight client retention` keeps them 3650 days, so backfill and rebuilds can read them. |
| `scheduler`: entries older than this binary's | `backsight jobs refresh` rewrites them without stopping a running job. |
| `bank-config`: bank settings differ from backsight's | `backsight bank apply --force` puts backsight's values back. |
| `backup`: no backup, or the last one is old | `backsight backup create`; check the daily job (`jobs` check). |
| `binary`: not self-verified, or does not match its release | Reinstall the release with `install.sh`, then `backsight client install`. |
| `config`: unknown key or invalid value | Correct the key doctor names in `config.toml`. Unknown keys are refused, not ignored. |
| `config`: secrets file readable by others | `chmod 600` on the file doctor names. |
| `disk`: low free space | Free space where doctor says: the queues, backups and the server's database grow there. |
| `untagged`: memories without a project tag | A server-side problem: report it (below). |
| `noise-transverse`: over 20 % of the transverse pool's observations name another project | New sessions go to the right project. Older ones: `backsight forget --project <transverse alias> --source subagent --plan`, then decide, or leave them. |
| `noise-headless`: headless (SDK) sessions were sent before they were skipped | `backsight forget --project <project> --plan` for each project doctor names, once the plan shows nothing else worth keeping. |
| `noise-subagent` (info): the share of raw facts that came from subagent reports | Information, not a fault. If one project's subagent reports drown its memories: `backsight forget --project <project> --source subagent --plan`, then decide. |
| `remotes`: two repositories share a project name | Their memories mix in one pool. Give one of them its own name: `project = "<another name>"` in a `.backsight.toml` at its root, committed so every clone uses it. |
| `reference-server` (info): the old Hindsight server can be stopped | Shown once `backsight backup verify` passed on backsight's newest backup within the last 7 days. Run the stop command doctor gives: the old server's data is kept, and its start command brings it back. See [install.md](install.md#replacing-an-existing-hindsight-install). |

## Reporting a problem

```sh
backsight report
```

`report` builds a diagnostic: versions, the doctor report, local counts and recent backsight errors. It is redacted: no memory text, prompt, transcript, secret or server answer; your home directory, user name, machine name, bank, the host of a server on another machine (backsight's or the reference one), git remotes, project names and backsight's directories are replaced by placeholders. A git remote is hidden in every form an error quotes it: whole, as `host:owner/repo`, without its host, and its owner alone. Project names are every one this machine knows: the bank's project tags (when the server answers), the projects of local sessions, cursors and telemetry, the names `.backsight.toml` files give, and the transverse aliases; names everyone's install shares (`.claude`, `backsight`) stay. A name under 4 characters is hidden as a path segment, after `project:` or `--project`, and in the doctor check that lists it, not as an ordinary word. It prints:

- a link that searches existing GitHub issues for the same error;
- a link that opens a new issue with a title and a summary filled in;
- the full diagnostic to paste into it.

It sends nothing. Read the diagnostic, then decide whether to file it. Security problems go through GitHub's private vulnerability reporting instead, see [SECURITY.md](../SECURITY.md).
