# backsight on Windows

backsight runs on Windows 11 x86_64 (amd64), for a standard user: no step asks for administrator rights or writes outside your user profile. Claude Code runs natively (its main shell there is PowerShell; it runs backsight's hooks in Git Bash when it finds one), and the memory server runs as programs of your own user, without Docker. Words you may not know are in the [glossary](glossary.md).

## What you need

- Windows 11 on x86_64. Windows on Arm has no backsight yet.
- [Claude Code](https://code.claude.com/docs/en/setup) 2.1.309 or newer, installed natively (WSL is not tested). Claude Code on Windows uses PowerShell as its main shell, and offers its Bash tool and runs hooks in Git Bash only when it finds Git for Windows. backsight needs that Git Bash: without it Claude Code runs backsight's hooks in PowerShell, where SessionStart gets no environment file, so the commands Claude runs get no `BACKSIGHT_PROJECT` or `BACKSIGHT_SESSION` (the CLI then names the project from the directory it runs in). Claude Code finds Git for Windows installed in its usual places; a portable Git (PortableGit) or one in another directory needs the user environment variable `CLAUDE_CODE_GIT_BASH_PATH` set to its `bin\bash.exe`, in PowerShell: `[Environment]::SetEnvironmentVariable('CLAUDE_CODE_GIT_BASH_PATH', '<Git>\bin\bash.exe', 'User')`, then restart Claude Code from a new terminal. Doctor's `git-bash` check says which case you are in.
- Windows PowerShell 5.1 (Windows ships it) or PowerShell 7, for the install script.
- About **3.5 GB of free disk** for the server: Python, its packages, the models and the first months of data.
- An LLM provider, as on Linux ([install](install.md#requirements)).
- No Docker, and no administrator rights.

## Install the binary

In PowerShell:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex"
```

`-ExecutionPolicy ByPass` applies to that one command: your execution policy is not changed. The script does what `install.sh` does on Linux:

1. finds the newest release, pre-releases included, through GitHub's API (no account or token needed), and downloads its `checksums.txt`, its signature and the Windows archive, `backsight_<version>_windows_amd64.zip`;
2. checks the signature of `checksums.txt` with the release key written in the script, then the archive's SHA-256, and stops if either fails. Windows has no minisign, so the script checks the signature itself, in plain PowerShell (BLAKE2b and Ed25519, under a second): no other program is downloaded or trusted;
3. runs the verified `backsight.exe`, which installs itself as `%LOCALAPPDATA%\backsight\bin\<version>\backsight.exe` with the signed checksums next to it (it checks itself against them on its first run), copies itself to the [shim](glossary.md#shim) `%LOCALAPPDATA%\backsight\bin\current.exe` and to the command `%LOCALAPPDATA%\Programs\backsight\backsight.exe`, and adds `%LOCALAPPDATA%\Programs\backsight` to your user `PATH` (the user's, not the machine's).

A terminal reads `PATH` when it starts: if the script says so, open a new terminal (PowerShell or Git Bash) before running `backsight`.

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

## Set it up

```powershell
backsight setup
```

On Windows x86_64, setup runs the server natively (`--runtime native` is the default there; `--runtime docker` keeps Docker, untested on Windows) and for this PC (`--role both`). Its pre-flight checks, before anything is written:

- 3.5 GB free for the server;
- the path length: Windows limits a path to 260 characters unless long paths are on, which needs administrator rights, and the server's Python environment has files 140 characters below its root, so that root (`server\native\venv\<version>` under the data directory) must stay within 110 characters. A long profile path: set `BACKSIGHT_HOME` to a short directory of yours, `C:\bs` say, and run setup from that shell;
- no `%` in the data directory, the server's directory or the shim's path: Task Scheduler would replace `%NAME%` in the task's command line with a variable's value. Set `BACKSIGHT_HOME` to a directory without one;
- the API port can be bound on `127.0.0.1` (Hyper-V can reserve port ranges);
- `conhost.exe --headless` runs (Windows 10 version 1809 and later);
- Task Scheduler accepts your task: setup registers a probe task, `\<project>\probe`, which never runs, and deletes it at once.

Each check, and what to do when it fails: [setup on the native runtime stops at its pre-flight](troubleshooting.md#setup-on-the-native-runtime-stops-at-its-pre-flight). The rest of setup is as on Linux ([install](install.md#run-setup)); then restart Claude Code. With backups on, save the backup identity in a password manager: `backsight backup identity show`, run at a terminal, shows it (`pass`, which the Linux hint names, does not exist on Windows).

## What runs on your PC

The [native runtime](native-runtime.md), as on Linux: Hindsight's API from PyPI in a Python environment, on the PostgreSQL with pgvector that [pg0](https://pypi.org/project/pg0-embedded/) ships, both listening on `127.0.0.1` only. Setup's server step downloads, at the versions this release pins:

- [uv](https://github.com/astral-sh/uv) 0.13.1 (`uv-x86_64-pc-windows-msvc.zip`), checked against its SHA-256, which installs the rest. It is never taken from your `PATH`;
- CPython 3.11.17, installed by uv;
- Hindsight (`hindsight-api-slim`) and its packages, from PyPI, each checked against the hash backsight's Windows lock pins, among them `pg0-embedded` and `msvc-runtime` (the Microsoft Visual C++ runtime DLLs);
- the embedding and reranking models, from Hugging Face, at pinned commits.

pg0 always keeps its files in `%USERPROFILE%\.pg0`, whatever `BACKSIGHT_HOME` says. So backsight uses it only to unpack PostgreSQL 18.1 and create the database in backsight's own directory, then moves that PostgreSQL there and runs it itself. What pg0 leaves in `.pg0` is two empty folders.

Windows has no VC++ runtime of its own, and installing Microsoft's redistributable needs administrator rights. backsight copies the DLLs of the `msvc-runtime` package next to the programs that load them (PostgreSQL's `bin\`, pg0's `bin\` and onnxruntime's `capi\`), and records their SHA-256 in `dlls.json`: doctor's `native-dlls` check compares every copy with it.

Everything is under `server\native` in backsight's data directory (`%LOCALAPPDATA%\backsight\server\native` by default), Linux's names kept:

| Path | What |
|---|---|
| `uv\0.13.1\uv.exe` | The pinned uv. |
| `python\` | The pinned Python. |
| `venv\<Hindsight version>-<lock hash>\` | Hindsight's Python environment (`Scripts\`, `Lib\site-packages\`), one per locked set of packages, so an upgrade installs beside the running one. |
| `pg\18.1.0\` | The PostgreSQL pg0 unpacked, which the server runs. |
| `db\` | The database. |
| `home\` | The server's `HOME`. |
| `hf\` | The models (`HF_HOME`). |
| `tmp\` | The programs' `TEMP`, and `backsight backup verify`'s throwaway database. |
| `server.env` | The server's environment, its keys included. The supervisor reads it; no key is ever on a command line, in `supervise.json` or in the task. |
| `identity` | The identifier of the database backsight created or restored. |
| `supervise.json`, `task.xml` | What the supervisor runs, and the scheduled task (below). |
| `dlls.json` | The VC++ runtime DLLs and their SHA-256. |

The data directory, the config directory, `secrets.toml` and `server.env` are readable by you, SYSTEM and Administrators only: backsight sets that when it creates them, with inheritance off, so other users of the PC cannot read your memory or its keys. A `BACKSIGHT_HOME` elsewhere gets the same. Doctor's `native-private` check reads it back.

As on Linux, the native runtime serves this PC only and has no web UI: `backsight server expose --private` and `setup --role server` are refused. `backsight server runtime switch` is refused on Windows in this release: Docker on Windows is untested.

## The memory server

Windows has no service manager a standard user can use, so backsight supervises the server itself. A scheduled task of your own, `\backsight\server` (named after `server.project`), starts `backsight server supervise` at your logon and checks every 5 minutes that it still runs. That supervisor runs the database, then the memory server, restarts either if it stops (after 1 second, then twice as long each time, up to 5 minutes), and stops both with it: if the supervisor dies, Windows ends both programs. It answers on a named pipe of your own, which `backsight server status`, doctor and the other commands ask; a second supervisor of the same project exits at once, starting nothing. The task runs with your own rights, in a console without a window (`conhost.exe --headless`), on battery too, with no time limit. Neither `supervise.json` nor `task.xml` holds a secret.

- Before starting the database, the supervisor runs the check Linux's database unit runs, `backsight server native-check`: it refuses a database that is missing, empty or not the one backsight recorded, and stops a stray PostgreSQL running on its files (one from `server\native\pg` outside the supervisor), never adopting it. It also refuses a `supervise.json` that would run the database without `fsync=on`. After a refusal it starts nothing and stays refused, without retrying, until `backsight server stop`. `backsight server status` gives the reason (`refusal` in its JSON), and so it does when `backsight server start` refused the database before starting the supervisor. The reason stays in the file `refused` in `server\native` once the supervisor is stopped, for `backsight server status` and doctor, until a start passes the check. What each refusal means: [the native server does not start](troubleshooting.md#the-native-server-does-not-start).
- `backsight server stop` disables the task first (its 5-minute check would start the server again), then stops the server: the API first, then the database. `backsight server start` enables the task again.
- With no one logged on at the PC's screen (over SSH, on a CI runner), the task cannot run. `backsight server start` then starts the supervisor itself and says so: `the server runs until you log off; it starts by itself at your next logon`.

### When Task Scheduler refuses the task

A policy of your organization can block your own scheduled tasks. Setup's probe then fails, and setup warns and goes on: it writes `server.start_on_session = true` in `config.toml` and starts the server without the task. The server then runs until you log off, and no longer starts at logon by itself:

- each Claude Code session starts it when it is down: its SessionStart hook starts the supervisor the way `backsight server start` does without the task, and does not wait for it;
- `backsight server start` starts it the same way, saying `Task Scheduler refused the server's task: the server runs until you log off, and starts with your next Claude Code session`;
- `backsight server stop` writes the file `stopped` in `server\native` before it stops the server, and sessions then leave the server down; `backsight server start` removes it and starts the server. `backsight uninstall` writes it too, and sessions leave the server alone while a command holds it (`server stop`, `uninstall`, `server install`).

A later `backsight setup` whose probe task passes removes `server.start_on_session` (`backsight setup --undo` puts it back), and the next start registers the task.

### Logs

In the `logs` directory of backsight's state directory (`%LOCALAPPDATA%\backsight\state\logs` by default; `backsight paths` shows the state directory):

- `<project>-supervisor.log`: what the supervisor did (starts, restarts, refusals, stops), and what kept it from starting at all (a `config.toml` that does not load, a missing `supervise.json`); a `config.toml` that does not load names no project, so that goes to `backsight-supervisor.log`;
- `<project>-db.log`: the database's output;
- `<project>-api.log`: the memory server's output;
- `<project>-jobs-tick.log` and `<project>-jobs-maintain.log`: what each run of the scheduled jobs printed, between a `started` line and an `ended: exit <code>` line, with the reason of a failed run (the task itself keeps no output).

Each is kept to 10 MiB, the previous one as `.1`. The server's keys, and every value of `secrets.toml`, are replaced in them. A server that fails to start shows the last lines of the API's log.

## The scheduled jobs

`backsight jobs tick` (every 15 minutes) and `backsight jobs maintain` (every day at 04:00) are two more scheduled tasks of your own, `\backsight\tick` and `\backsight\maintain` (named after `server.project`), registered the same way as the server's: with your own rights, in a console without a window, on battery too, with no time limit. A run missed while the PC was off or asleep runs as soon as it can, so a 04:00 maintain missed overnight runs at your next logon. Their files are `tick.xml` and `maintain.xml` in `tasks\<project>` under backsight's state directory, so that `backsight jobs uninstall` still deletes them after `server.project` changes. A task cannot carry environment variables, so a `BACKSIGHT_HOME` or `BACKSIGHT_STATE_DIR` set when the jobs (or the server's task) were installed is written into each task's command line. A failed run is not retried: the next one comes at its time. An update registers a changed task again, except while its job runs: that one waits for the next update or `backsight jobs refresh`.

## No administrator rights, nothing machine-wide

backsight writes only:

- `%LOCALAPPDATA%\backsight` (or `BACKSIGHT_HOME`): the binaries, the shim, the config, the state, the server and its data, the backups;
- `%LOCALAPPDATA%\Programs\backsight\backsight.exe`, and that directory in your user `PATH` (`HKEY_CURRENT_USER`);
- your own scheduled tasks, in the folder `\<project>` of Task Scheduler;
- Claude Code's files in your profile, as on Linux;
- `%USERPROFILE%\.pg0`, which pg0 creates (two empty folders once the server is installed).

It never asks for elevation, never writes under `C:\Program Files`, `C:\Windows` or `HKEY_LOCAL_MACHINE`, and never installs the VC++ redistributable. On a PC where you are an administrator, it works the same way: PostgreSQL refuses to run with administrator rights, so the supervisor starts it with those rights removed. From an elevated terminal ("Run as administrator"), `backsight server start` does not start the server outside its task: a server started from there would be out of reach of your normal terminals and of Claude Code (`access denied`). Run it from a normal terminal; the task itself always runs with your normal rights.

## SmartScreen and Microsoft Defender

backsight's `.exe` files are not code-signed (a certificate costs money). What vouches for them is the release's minisign signature, which `install.ps1` checks before it runs anything, and the signed checksums each binary checks itself against.

- **SmartScreen** looks at programs Windows marks as downloaded from the internet, such as release files a browser saved. If it shows "Windows protected your PC" for `backsight.exe`, the file is unknown to it, not known to be bad: install with the script (with `BACKSIGHT_RELEASE_DIR` for files you downloaded), which checks the signature and the checksum before it runs the binary.
- **Smart App Control**, when it is on, may block programs that are not signed. backsight has no signed build in this release.
- **Microsoft Defender** scans what setup downloads, many files for the Python environment: the first install can take longer. A scan can also hold a file for a moment; backsight retries for a few seconds when Windows refuses a rename or a delete for that reason. If Defender reports `backsight.exe` itself, report it ([below](#troubleshooting)), and do not restore a file you have not checked against the release's `checksums.txt`.

## Updates and uninstall while Claude Code runs

Every Claude Code session runs `current.exe`, the shim in `bin` under backsight's data directory, and so does the server's supervisor. Windows cannot delete or overwrite a program that runs, but can rename it.

- `backsight update apply` renames the running shim to `current.exe.old-<n>` and puts the new one in its place; a later update or `backsight client install` deletes the copies nothing runs any more. The `backsight.exe` on your PATH, a copy of the shim, is swapped the same way (`backsight.exe.old-<n>`); `backsight uninstall` removes those copies with it. Doctor's `shim-copies` check counts those still running (info).
- The supervisor keeps running the backsight it started from. `update apply` restarts it only when the new backsight speaks another protocol to it, or when it still runs the old one after the update's own server upgrade (which restarts it); hooks queue their notes during that short outage. Otherwise its `supervisor` step says `the server keeps running backsight <old> until its next restart`, and doctor's `supervisor` check shows it as info.
- A server upgrade installs the new Python environment beside the running one, writes `supervise.json` for it and restarts the supervisor; a failed upgrade puts the previous environment back ([update](update.md#server-upgrades)).
- `backsight uninstall` stops the supervisor and deletes the server's task (its data stays in `server\native` unless `--delete-data`), deletes the jobs' tasks, then their folder `\<project>` in Task Scheduler, removes backsight's directory from your user `PATH` (no other entry), then deletes the files. A file a program still runs or holds is moved to `trash` under the data directory, and a cleaner (`cmd.exe`, without a window) tries to delete that directory every 5 seconds for 5 minutes after uninstall exits, then the data directory once nothing else is left in it.
- Uninstalling (or `setup --undo` of) a backsight wired to its own Claude Code profile (`--config-dir`) keeps the default profile's plugin enabled: Claude Code 2.1.316 on Windows, removing the marketplace from one profile, also deletes `enabledPlugins` `backsight@backsight` from `%USERPROFILE%\.claude\settings.json`. backsight puts the entry back through its config editor and its `client` step says so. The editor's copy of `settings.json` as it was is kept in the state directory of the backsight being uninstalled, so `uninstall --purge-local` deletes it with the rest; `setup --undo` keeps it.

What may be left to delete by hand:

- the `trash` directory, when it is still there after 5 minutes: uninstall names it; close Claude Code, then delete it;
- what uninstall keeps on purpose: your backups, and the config and state without `--purge-local`, the server's data without `--delete-data` ([uninstall](uninstall.md));
- `%USERPROFILE%\.pg0`, when uninstall kept the server's data: `--delete-data` removes its empty folders, and the folder itself when nothing else is in it.

## Troubleshooting

Start with `backsight doctor`. On a native Windows install, these checks replace Linux's `native-unit` and `native-linger`; `native-fsync`, `native-identity`, `native-stray`, `native-home` and `native-venv` are as on Linux (`native-home` reads who can read `home\`). The fix for each is in [troubleshooting](troubleshooting.md#other-doctor-warnings):

| Check | What it says |
|---|---|
| `native-task` | The scheduled task is registered and enabled. Disabled after `backsight server stop` is info (`backsight server start` enables it), and so is no task with `server.start_on_session`. Not registered: `backsight server start` registers it. |
| `supervisor` | The supervisor answers, and runs the database and the API. Not answering: `backsight server start`, then `<project>-supervisor.log`. After an update that changed its protocol, a warning: `backsight server stop`, then `backsight server start`. One running an older backsight is info. |
| `native-dlls` | Each copy of the VC++ runtime DLLs matches `dlls.json`. One missing or changed: `backsight server stop`, delete the `.backsight-complete` mark doctor names, then `backsight server install`. |
| `native-private` | The data directory, the config directory, `secrets.toml` and `server.env` are readable by you, SYSTEM and Administrators only. Otherwise doctor gives the `icacls` command that puts it right, to run in PowerShell. |
| `path-length` | The server's Python environment is within the path bound. Otherwise move backsight to a short `BACKSIGHT_HOME` (back up first). |
| `git-bash` | Whether Claude Code will find Git Bash, judged conservatively: ok when `CLAUDE_CODE_GIT_BASH_PATH` names an existing file, or Git for Windows is in `%ProgramFiles%\Git` or `%LOCALAPPDATA%\Programs\Git`; any other Git (a PortableGit, a `git` on `PATH` elsewhere) is a warning whose fix names the `bin\bash.exe` beside that `git.exe`. |
| `shim-copies` | Earlier copies of the shim an update renamed, still run by a session or the supervisor (info: the next update deletes those no longer running). |

Other things you may meet:

- **`backsight` is not found** right after the install: the terminal started before `PATH` changed. Open a new one, or run `"$LOCALAPPDATA/Programs/backsight/backsight.exe"` in Git Bash.
- **The server is down after a restart**: the task starts it at your logon. `backsight server status`, then the logs above. With `server.start_on_session`, the first Claude Code session starts it.
- **`BACKSIGHT_PROJECT` empty in a PowerShell command** while Git Bash is found (doctor's `git-bash` ok): Claude Code copies the hooks' environment file into PowerShell commands only when every line is a plain assignment, and drops a value with `/` or `\` on Windows. backsight writes `export BACKSIGHT_PROJECT='<project>'` and `export BACKSIGHT_SESSION='<id>'`, which pass for project names made of letters, digits, spaces, `-`, `_` and the like. A project named with `/` (only a `.backsight.toml` `project` can) is dropped from PowerShell commands, and a name with a `'` (a directory named so) makes the whole file unusable there: both still reach Bash commands, and backsight's CLI otherwise names the project from the directory it runs in.
- **A scheduled job that fails** (doctor's `jobs` check late, Task Scheduler's last run result not 0): `<project>-jobs-tick.log` or `<project>-jobs-maintain.log` in the logs above says what the run printed and why it stopped.
- **A server that keeps restarting**: `<project>-supervisor.log` says which program stopped and when; `<project>-db.log` and `<project>-api.log` hold their own output.
- **Over SSH**, `backsight server start` runs the server only until you log off (above): log on at the PC once for the task to take over.

To report a problem: `backsight report` ([troubleshooting](troubleshooting.md#reporting-a-problem)).

## Licences

backsight does not ship these: they are downloaded on your PC when the server is installed, under their own licences.

| Component | Licence, and where to read it |
|---|---|
| uv, by Astral (`uv-x86_64-pc-windows-msvc.zip`) | MIT or Apache-2.0: [LICENSE-MIT](https://github.com/astral-sh/uv/blob/main/LICENSE-MIT), [LICENSE-APACHE](https://github.com/astral-sh/uv/blob/main/LICENSE-APACHE). |
| CPython, as built by [python-build-standalone](https://github.com/astral-sh/python-build-standalone) | The [Python Software Foundation License](https://docs.python.org/3/license.html); the libraries each build bundles keep their own licences, which python-build-standalone's repository and documentation list. |
| Hindsight (`hindsight-api-slim`) and the other Python packages | Each package's own: its licence ships in its `*.dist-info` directory in the Python environment (`venv\<version>\Lib\site-packages\`), and its PyPI page names it. The packages and versions are those of backsight's Windows lock, `internal/server/nativelock/requirements-windows-amd64.txt` in the source. |
| The Microsoft Visual C++ runtime DLLs, from the [`msvc-runtime`](https://pypi.org/project/msvc-runtime/) package | Proprietary, Microsoft's terms for these runtime files: the package declares its licence as "Proprietary" on PyPI, and its `*.dist-info` holds what it ships of the terms. |
| PostgreSQL and pgvector, shipped by `pg0-embedded` | The [PostgreSQL License](https://www.postgresql.org/about/licence/), and [pgvector's](https://github.com/pgvector/pgvector/blob/master/LICENSE); pg0's own is in its `*.dist-info`. |
| The models | As in the [README](../README.md#third-party-components). |
