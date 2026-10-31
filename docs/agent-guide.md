# Installing backsight for a user: guide for coding agents

backsight gives Claude Code a long-term memory: one Go binary that runs a memory server in Docker on the user's machine and wires it into Claude Code. You are a coding agent (Claude Code or another) asked to install it. This page is all you need: do not browse the repository.

Quick start: if `backsight version` fails, install the binary with `curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.sh | sh`; then run `backsight setup --questions --json` and go on at step 2. Ask the user before every choice.

Follow these steps in order. Everything goes through `backsight` commands with `--json`; never edit backsight's files or Claude Code's settings by hand. Once installed, this page is also `backsight guide agent-guide`.

## Rules

- **Secrets never pass through you.** Never put a key in a command line, an answer or a file. When a key is needed, ask the user to run `backsight secret set <name>` in a terminal of their own, outside this session (the value is read without echo), or to pipe it from their password manager (`pass show <entry> | backsight secret set <name>`). Not with `!` in Claude Code: it runs the command without a terminal, so there is no prompt and nothing is saved. Never suggest `--from-env` or `echo` inside the session: the key would land in the transcript, which backsight sends to the memory server.
- **The user decides.** Show each question with its options, costs and caveats, say which is recommended, and wait for the answer. Never answer `consent` yourself.
- **Read the exit code and the JSON envelope** `{schema_version, command, ok, data | error}`:

| Exit | Meaning | What you do |
|---|---|---|
| 0 | done | go on |
| 2 | invalid invocation or answer | fix the command; `error.message` names the problem |
| 10 | the user must act | ask each of `error.questions` that has `"asked": true`, ask the user to set each of `error.secrets`, show each of `error.actions` (`run_as_user`, `relogin`, `install`) with its `why`; then run `error.resume` |
| 20 | blocked by the machine | show `error.message` and the failed pre-flight checks with their `fix`; rerun once fixed |
| 30 | failed and rolled back | show `error.message`: the machine is as before; rerun once the cause is fixed |
| 40 | partly done, resumable | show the message, fix if asked, then run `error.resume` with `--json` (setup's is `backsight setup --resume`: add `--non-interactive` too); `verify --live` waits `data.wait_s` seconds first |

Exit 10 from setup's `scheduler` pre-flight check means this machine has no systemd user session (a container, a shell opened with `su`): nothing is written yet. Tell the user that with `scheduler=off` nothing runs on a timer (`backsight jobs tick` every 15 min and `backsight jobs maintain` daily, which makes the backup, then run only by hand or from their own scheduler), and that an ssh login as that user avoids it; if they choose `scheduler=off`, run `error.resume`, the same command with `--answer scheduler=off`.

## 1. Install the binary

If `backsight version` works, skip this step. Otherwise:

```sh
curl -fsSL https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.sh | sh
```

The script checks the release's minisign signature (with `minisign`, or `openssl` 3; on macOS, whose LibreSSL cannot, the user installs minisign first) and its checksum, then installs the binary under backsight's data directory and links `~/.local/bin/backsight` to its shim (`<data>/bin/current`, which `update apply` moves to each new version). If it prints an `export PATH=…` line, run commands with that PATH (and tell the user to add it to their shell profile).

To install one given release, add `BACKSIGHT_RELEASE_URL=https://github.com/Barben360/backsight/releases/download/<tag>` before `sh`. From release files already downloaded into a directory: `BACKSIGHT_RELEASE_DIR=<dir> sh install.sh`.

## 2. List the questions

```sh
backsight setup --questions --json
```

Each question has `id`, `prompt`, `options[{value, label, cost, caveat}]`, `default`, `recommended`, `required`, and `asked`. Questions with `"asked": false` are already settled by an existing install's config or an earlier run: tell the user, don't ask again. If the provider answer changes, list the questions again: the model and other questions follow the provider.

Hidden answers, for unusual machines only: `api_port`, `ui_port` (defaults 8888 and 9999), `project` (another Compose project name, to run beside another install; that install also needs its own HOME or Claude Code profile, since two installs cannot share a profile), `scheduler=off` (a machine without a systemd user session, such as a container: setup's pre-flight stops with exit 10 and asks for it; it also removes the entries an earlier run wrote, and `backsight setup --resume --answer scheduler=off` goes on from the earlier run without asking for consent again).

The native runtime, only when the user asks for it (no Docker, Linux x86_64, this PC only, no web UI): add `--runtime native` to every setup command (the same as the hidden answer `runtime=native`; see [the native runtime](native-runtime.md)). Its pre-flight exits 10 when lingering is off, with the action `loginctl enable-linger` for the user to run, or when no systemd user session is reachable (an ssh login as that user is the way; `scheduler=off` does not help there).

A machine without Claude Code that only holds the memory for the user's other PCs: add `--role server` to every setup command (the same as the hidden answer `role=server`). It skips the Claude Code questions and steps; then the user opens the server with `backsight server expose --private <address>` in a terminal of their own, since the connection token it hands out holds the API key.

## 3. Show the plan, then run setup

```sh
backsight setup --plan --json --answer provider=<p> --answer <id>=<value> …
```

Show the user the pre-flight checks and the planned steps. When they agree, add `--answer consent=yes` and run:

```sh
backsight setup --non-interactive --json --answer provider=<p> … --answer consent=yes
```

Progress lines `{"event":"progress","step","pct","eta_s"}` arrive on stderr; stdout holds the one envelope. During the server step a line comes every 5 seconds with a `detail` saying what runs (the image download, then the server's first start and its model download). The first run downloads about 2.4 GB: tell the user it takes several minutes.

Expect exit 10 the first time: the LLM key is not set. Ask the user to run `backsight secret set llm_api_key` in a terminal of their own (with the `claude-code` provider, first `claude setup-token`), then run `backsight setup --resume --non-interactive --json`.

## 4. Finish

On exit 0, `data` holds:
- `steps`: what was done;
- `restart_required`: if true, tell the user to restart Claude Code (or run `/reload-plugins`);
- `next_steps`: relay each one (the backup key to save, the background backfill);
- `try_first`: suggest it.

### Following the import of past sessions

When the user accepted the import, setup queued it and started the first `jobs tick`, which sends at most three sessions per run, every 15 minutes. `backsight retain backfill --status --json` reads its progress from this machine alone, without the server: `data.done`, `data.total`, `data.left`, `data.spent_cost` against `data.max_cost`, `data.running` (a backfill is sending now), `data.paused` and `data.finished`. Poll it every few minutes rather than in a tight loop.

To finish the import now, with the user's go-ahead, run `backsight retain backfill --json` in the background. It blocks until the queue is empty, which can take hours (setup's `backfill` step and `backsight retain backfill --plan` give the estimate). Pass the same `--projects-dir` as setup's `backfill` step, and `--allow-subscription` on the `claude-code` provider: other filters start a new queue. After each session, a line `{"event":"progress","command":"retain backfill","step":"session","status","session","done","total","spent_cost","spent_tokens","currency"}` arrives on stderr; `"step":"queue"` or `"final"` with `"status":"waiting"` and a `detail` says it waits for the server. Exit 0 with `data.finished` means done; exit 40 means paused (`data.paused`: `budget` needs a higher `--max-cost`, which is the user's call; `interrupted` or `server` resume with a rerun).

`backsight doctor --json` checks the whole install at any time. About an hour after the user's first real session, `backsight verify --live --json` checks the real pipeline, read-only: exit 0 means this machine's latest settled session reached the server and an observation was consolidated from it; exit 40 means not yet, rerun after `data.wait_s` seconds; exit 20 means it failed, with `data.fix`. `backsight setup --undo --json` reverses setup and keeps the config, the secrets and the memories.

## Joining a shared memory or installing a remote server

The user's PCs can share one memory, held by the server on one machine, the host: [several-pcs.md](several-pcs.md). The other PCs join it with a connection token, which holds the server's API key.

**Never read, print, copy or pass a token's value or a key**, never ask the user to paste one into the conversation, and never open the token file. If a token reached the conversation anyway, tell the user to give the server a new key on the host, in a terminal of their own: `backsight server token --rotate --out <file>`; every other PC is then refused until it joins again with `backsight connect --token-file <file>`.

### Joining the host from this PC

1. Ask the user to make a token on the host, in a terminal of their own: `backsight server token --out <file>` (the server must be exposed first: `backsight server expose --private <ip> --out <file>` writes one too). They carry the file to this PC themselves (`scp`, a USB stick), never through a chat, and give you its path only.
2. If `backsight version` fails, install the binary ([step 1](#1-install-the-binary)). A PC that joins needs Claude Code, not Docker.
3. Show the plan:

   ```sh
   backsight connect --plan --json --token-file <file>
   ```

   Show the user `data.steps`, `data.warnings` (a different major or minor Hindsight version on the host is a warning, never a refusal), `data.notes` (information, such as a host whose Hindsight differs only by its patch version: nothing to do) and, for a PC that runs its own server, `data.own_server` (its server is backed up, verified, then stopped). When `data.questions` holds `ack_subscription`, the host extracts with a Claude subscription: show its caveat; only the user accepts it.
4. When they agree, run it with the answers the plan named:

   ```sh
   backsight connect --non-interactive --json --token-file <file> --answer consent=yes
   ```

   Add `--answer ack_subscription=yes` once the user accepted those terms. connect then runs setup's client steps: exit 10 and 20 mean what they mean for setup (ask each of `error.questions` with `"asked": true`, then run `error.resume`; or show the failed checks and their fix). Each refusal before the change leaves the PC as it was.
5. On exit 0, relay `data.next_steps` (delete the token file here) and `data.setup.next_steps` (the background import, how to undo); when `data.restart_required` is true, tell the user to restart Claude Code (or run `/reload-plugins`). The host's copy of the token file goes too: the user deletes it there.

### Installing the server on another machine over ssh

For a Linux machine without Claude Code that the user reaches with `ssh user@host` (their keys, their `known_hosts`: an unknown host key is refused, so they connect once themselves and check the fingerprint first). The install sends the host's LLM key, which never passes through you: you only show the plan, and the user runs the install itself in a terminal of their own.

1. Show the plan, which reads the host, reads no key and changes nothing:

   ```sh
   backsight server install --ssh user@host --plan --json
   ```

   Pass the host's setup answers the user chose, as in step 2 (`--answer provider=<p>`, `--answer model=<m>`…). An exit 10 names what is missing: the address the other PCs will use (`--private <ip>`, a tailnet or LAN address of the host; a LAN one also needs `--answer cleartext_lan=yes`, the user's call, since the API key then travels in clear on that network), or a signature checker on the host. A plan step `ack_subscription` means the host would extract with a Claude subscription: show its caveat; only the user accepts it.
2. Once the user agrees, give them the command to run in a terminal of their own: `backsight server install --ssh user@host` with the flags and answers you agreed, without `--plan` nor `--json`. It shows the plan and asks there: the subscription's terms when the host extracts with a Claude subscription, the consent, then the LLM key at a hidden prompt (only when the host's provider needs one and the host has none yet); then it joins the server from this PC, and connect asks its own questions. Never run it yourself, and never pass `--llm-key-from-file` or `--llm-key-from-env`.
3. The user reads the outcome in their terminal. After a failure (exit 40 when the ssh connection ended part way), they fix what the message says and run the same command again there: it goes on from where it stopped. The host is never rolled back; `backsight setup --undo` there undoes its setup. Never run its resume command yourself: it can carry a key flag.

No ssh access to the host: `backsight server install --print-commands --json` lists the commands to run, each with where (the host or this PC) and why. Show them to the user, who runs them: the host's key at its own hidden prompt, and the token file carried by hand.

## When something goes wrong

`backsight report --json` builds a redacted diagnostic (versions, the doctor report, local counts, recent backsight errors; no memory text, prompt, transcript, secret, home path, project name or git remote) with `data.search_url` (existing issues with the same error class), `data.issue_url` (a new issue with the title and a summary filled in) and `data.paste` (the full diagnostic to paste into it). It sends nothing. Show the user the diagnostic and both links; never open, submit or post them yourself: the user decides whether to report.

## Is memory working?

When the user asks whether memory works or is worth it, run `backsight stats overview --json` and read two fields of `data`: `verdict` (four axes, found, right, cost and flowing; each has a `state` and its `reasons`) and `attention` (each item has `text`, `why` and the `command` that helps). Report the verdict in plain words, then the attention items with their commands.

- Drill down with `backsight stats quality`, `cost`, `bank`, `pipeline` or `system` (each `--json`), `stats sessions --has used|wrong|undeclared|memory`, `stats memories <id>`, `stats projects`, `stats days`. `backsight stats defs <id>` gives a metric's definition and when it is judged.
- Each view answers one question: quality (does memory show up and is it right), cost (context, time and server tokens), bank (size and what to review), pipeline (are writes arriving), system (checks, server, jobs).
- Every rate carries `num` and `den`, its sample. Statuses use confidence bounds, not the raw point: a rate is `watch` or `bad` only when its interval is past the line.
- `too_few` means not enough data yet and `no_data` means nothing to count: neither is a failure. Say so rather than calling it a problem.
- Put flags after the view word: `backsight stats overview --days 30`, `--project <name>`.
- Never run `backsight stats index rebuild` without the user: it replaces the local index.
- Never pass `--text` unless the user asks for a memory's text.

## The dashboard

`backsight dashboard` is a web page for the user; it is not an interface for you: never start the dashboard on your own. When the user asks for it, run `backsight dashboard --open` in the background: it opens their browser. For a link to give them (another browser, a lost link), run `backsight dashboard link`. The links are one-time and valid 2 minutes.

- Rule: never put a session secret, a cookie or a login code on a command line (no `curl` with them). The pages need a signed-in browser; you do not.
- For numbers, use `backsight stats <view> --json`: it returns what the pages show.
- Exit 20 with `a dashboard is already running`: one runs already; `backsight dashboard link` prints a link to it.
- To stop it, the user presses Ctrl+C in its terminal (or stop the background process you started). It stops by itself after 2 hours without use.

## More

The user documentation covers each topic in detail: [install](install.md), [daily use](daily-use.md), [commands](commands.md), [privacy](privacy.md), [backups and restore](backups-and-restore.md), [update](update.md), [uninstall](uninstall.md) (`backsight uninstall --plan --json` first, then its `--answer` ids), [troubleshooting](troubleshooting.md), [several PCs](several-pcs.md).
