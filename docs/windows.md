# backsight on Windows

backsight runs on Windows 11 x86_64 (amd64), for a standard user: no step asks for administrator rights or writes outside your user profile. Claude Code runs natively (its Git Bash), and the memory server runs as programs of your own user, without Docker.

## Install the binary

In PowerShell (Windows PowerShell 5.1, which Windows ships, or PowerShell 7):

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex"
```

`-ExecutionPolicy ByPass` applies to that one command: your execution policy is not changed. The script does what `install.sh` does on Linux:

1. finds the newest release, pre-releases included, through GitHub's API (no account or token needed), and downloads its `checksums.txt`, its signature and the Windows archive, `backsight_<version>_windows_amd64.zip`;
2. checks the signature of `checksums.txt` with the release key written in the script, then the archive's SHA-256, and stops if either fails. Windows has no minisign, so the script checks the signature itself, in plain PowerShell (BLAKE2b and Ed25519, under a second): no other program is downloaded or trusted;
3. runs the verified `backsight.exe`, which installs itself as `%LOCALAPPDATA%\backsight\bin\<version>\backsight.exe` with the signed checksums next to it (it checks itself against them on its first run), copies itself to the [shim](glossary.md#shim) `%LOCALAPPDATA%\backsight\bin\current.exe` and to the command `%LOCALAPPDATA%\Programs\backsight\backsight.exe`, and adds `%LOCALAPPDATA%\Programs\backsight` to your user `PATH` (the user's, not the machine's).

A terminal reads `PATH` when it starts: if the script says so, open a new terminal (PowerShell or Git Bash) before running `backsight`. Then:

```powershell
backsight setup
```

With `BACKSIGHT_HOME` set, everything goes under that directory instead (`data`, `config`, `state`, and `bin` for the command), as on Linux.

To install one given release instead of the newest, set its download URL first:

```powershell
$env:BACKSIGHT_RELEASE_URL = 'https://github.com/Barben360/backsight/releases/download/<tag>'
irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex
```

If the script cannot list the releases (no network to `api.github.com`, or GitHub's limit of 60 requests an hour for an address without a token), it stops and names `BACKSIGHT_RELEASE_URL`: set it as above, with a tag from the [releases page](https://github.com/Barben360/backsight/releases).

To install from release files you downloaded yourself (`checksums.txt`, `checksums.txt.minisig` and the zip in one directory), set `$env:BACKSIGHT_RELEASE_DIR` to that directory, then run the script (saved as `install.ps1`: `powershell -ExecutionPolicy ByPass -File install.ps1`). Nothing is downloaded then.

The script refuses, and installs nothing, when:

- the signature does not verify: `checksums.txt: bad signature`, `signed by another key`, `bad trusted comment signature`, `not a prehashed minisign signature` (a release is always signed prehashed), or `was made for another file`;
- the zip is not the one `checksums.txt` lists: `checksum mismatch`;
- the machine is not Windows x86_64: on Windows on Arm, there is no backsight yet; on Linux and macOS, use `install.sh`.

Any of these with a release from GitHub: download nothing more, and report it.

## The memory server

Windows has no service manager a standard user can use, so backsight supervises the server itself. A scheduled task of your own, `\backsight\server` (named after `server.project`), starts `backsight server supervise` at your logon and checks every 5 minutes that it still runs. That supervisor runs the database, then the memory server, restarts either if it stops, and stops both with it: if the supervisor dies, Windows ends both programs. Its files are `supervise.json` and `task.xml` in the server's directory (`server\native` under backsight's data directory); no secret is in either. The task runs with your own rights, in a console without a window.

- `backsight server stop` disables the task first (its 5-minute check would start the server again), then stops the server. `backsight server start` enables it again.
- The logs are `<project>-supervisor.log` (what the supervisor did), `<project>-db.log` and `<project>-api.log` in the `logs` directory of backsight's state directory, each kept to 10 MiB, the previous one as `.1`. A server that fails to start shows the last lines of the API's.
- With no one logged on at the PC's screen (over SSH, on a CI runner), the task cannot run. `backsight server start` then starts the supervisor itself and says so: `the server runs until you log off; it starts by itself at your next logon`.
- A path with `%` cannot be in a task (Task Scheduler would replace `%NAME%` with a variable's value): if your profile path holds one, set `BACKSIGHT_HOME` to a directory without it.

## The scheduled jobs

`backsight jobs tick` (every 15 minutes) and `backsight jobs maintain` (every day at 04:00) are two more scheduled tasks of your own, `\backsight\tick` and `\backsight\maintain` (named after `server.project`), registered the same way as the server's: with your own rights, in a console without a window, on battery too, with no time limit. A run missed while the PC was off or asleep runs as soon as it can, so a 04:00 maintain missed overnight runs at your next logon. Their files are `tick.xml` and `maintain.xml` in `tasks\<project>` under backsight's state directory, so that `backsight jobs uninstall` still deletes them after `server.project` changes. A task cannot carry environment variables, so a `BACKSIGHT_HOME` or `BACKSIGHT_STATE_DIR` set when the jobs (or the server's task) were installed is written into each task's command line. A failed run is not retried: the next one comes at its time. An update registers a changed task again, except while its job runs: that one waits for the next update or `backsight jobs refresh`.

## Setup

`backsight setup` on Windows x86_64 runs the server natively (`--runtime native` is the default there; `--runtime docker` keeps Docker, untested on Windows) and for this PC (`--role both`). Its pre-flight checks, before anything is written:

- 3.5 GB free for the server;
- the path length: Windows limits a path to 260 characters unless long paths are on, which needs administrator rights, and the server's Python environment has files 140 characters below its root, so that root (`server\native\venv\<version>` under the data directory) must stay within 110 characters. A long profile path: set `BACKSIGHT_HOME` to a short directory of yours, `C:\bs` say, and run setup from that shell;
- no `%` in the data directory, the server's directory or the shim's path (above);
- the API port can be bound on `127.0.0.1` (Hyper-V can reserve port ranges);
- `conhost.exe --headless` runs (Windows 10 version 1809 and later);
- Task Scheduler accepts your task: setup registers a probe task, `\<project>\probe`, which never runs, and deletes it at once.

When a policy blocks your scheduled tasks, setup warns and goes on: it writes `server.start_on_session = true` in `config.toml` and starts the server without the task, which then runs until you log off. Each Claude Code session starts the server when it is down (its SessionStart hook starts the supervisor the way `backsight server start` does without the task, and does not wait for it), and `backsight server start` starts it the same way, saying `Task Scheduler refused the server's task: the server runs until you log off, and starts with your next Claude Code session`. The server no longer starts at logon by itself. After `backsight server stop`, sessions leave it down until `backsight server start`. A later `backsight setup` whose probe task passes removes `server.start_on_session` (`backsight setup --undo` puts it back), and the next start registers the task.

`backsight server runtime switch` is refused on Windows: Docker on Windows is untested in this release.

## Doctor on Windows

`backsight doctor` checks the native server's own pieces on Windows, in place of Linux's units and linger:

- `native-task`: the scheduled task is registered and enabled (disabled after `backsight server stop` is shown as info);
- `supervisor`: the supervisor answers; after an update that changed its protocol it asks for a restart (`backsight server stop`, then `backsight server start`), and one still running an older backsight is shown as info;
- `native-dlls`: each copy of the VC++ runtime DLLs matches `dlls.json`;
- `native-private`: the data directory, the config directory, `secrets.toml` and `server.env` are readable by you, SYSTEM and Administrators only;
- `path-length`: the server's Python environment is within the path bound.

[Troubleshooting](troubleshooting.md) gives the fix for each.
