# 0080 — M7 block 2: backsight on Windows x86_64, client and native server, without admin rights

- **Date**: 2026-10-30
- **Status**: accepted (author, 2026-10-30: "I approve decision 0080")
- **Decided by**: author (2026-10-30: the target picture, "It's perfect this way"; "We will also need to test WSL after that"; the real DeepSeek key on the VM), on the agent's proposal, after a builder check; every point marked *agent's call* is open to veto
- **Builds on**: 0070 D (native is the default on macOS and Windows), 0078 (the native Linux runtime, the switch, E29's 8 requirements), 0067 (shim, versioned binaries, `update apply`), 0027 (signed releases)

## Intent
A developer on their own Windows 11 x64 PC installs backsight and gets the memory working the way Linux users get it, without Docker Desktop and **without admin rights**. Designing for no admin covers the locked-down company laptop; a PC where the user is admin works the same way.

The Windows release asset already exists (`windows_amd64`, cross-compiled and vetted), but nothing has run on Windows: every CI job is Linux, about 30 tests fake tools with `#!/bin/sh` scripts, and the native runtime is systemd from end to end. The Windows spike (2026-10-30, the author's VM over `ssh win11`) showed that Hindsight 1.1.0 runs natively on Windows with pg0 and the onnx reranker once the VC++ runtime DLLs sit beside the binaries. This block turns that spike into the product.

### Who and where (author, 2026-10-30)
- One developer per PC, Windows 11 x64, possibly without admin rights.
- Claude Code runs natively on Windows, with Git for Windows installed (Claude Code's Bash tool needs it), not inside WSL.
- The PC is both client and local server (native runtime, the default on Windows). A Windows client of a Linux server (`connect`, 0076) must also work.

### Goals
- `install.ps1` (no admin) → `backsight setup` → memory works in Claude Code on Windows: hooks record, recall injects, MCP tools answer, skills run.
- The native runtime on Windows has everything it has on Linux: install, start, stop, status, logs, backup, `backup verify`, restore, `server upgrade` with rollback, uninstall, doctor, and E29's 8 requirements, each with a test that fails without it.
- The server comes back by itself after a logon and after a crash, without admin.
- `update apply` and `uninstall` work while Claude Code sessions are open, which hold `current.exe` open.
- CI runs the unit tests and a native chain on Windows on every PR.

### Non-goals
- WSL. It must be tested in a later pass, once this block is done (author, 2026-10-30). WSL is not installed on the VM yet.
- Windows on ARM; PCs shared by several users; a Windows PC as a server for other PCs (private exposure stays loopback-only, as 0078 left it).
- Starting the server before logon (a boot trigger or S4U task). The memory is only used while the user is logged on.
- Docker on Windows as a tested path. It keeps working as far as it works today (nothing refuses it), and is untested in this block. *Agent's call*: `server runtime switch` is refused on Windows in this block, because one side of it would be untested; it comes back with the macOS block or on request.
- Code-signing the `.exe` (a certificate costs money). The docs say what SmartScreen or Defender may show.
- macOS (the next block).

### Anti-goals (failure even if it "works")
- Any step that asks for admin rights or a UAC prompt, or installs anything machine-wide (the VC++ redistributable included).
- A postgres or hindsight-api process left running after the supervisor that started it is gone.
- The database password or `secrets.toml` readable by another standard user of the PC.
- An installer whose trust is weaker than `install.sh`'s, for example checksums trusted only because they came over HTTPS.
- A Windows change that alters what Linux installs do.

## Decision

### The supervisor replaces systemd (*agent's call*)
Windows has no per-user service manager. A real Windows service needs admin, and an HKCU `Run` key cannot restart a crashed server. So **backsight supervises the processes itself**, and a per-user Task Scheduler entry starts the supervisor:
- `backsight server supervise` (hidden) runs `server native-check`, then pg0's `postgres.exe` in the foreground. It waits for `pg_isready`, then starts `hindsight-api.exe`. Both children go in a **Job Object with kill-on-close**: if the supervisor dies for any reason, Windows kills both. This is Windows' `KillMode=control-group` (requirement 3).
- A child that exits unexpectedly is restarted with backoff. The supervisor exits non-zero when restarts are exhausted, and the task's restart-on-failure starts it again. The supervisor's log goes to the state directory's `logs/`, and `server logs` reads it, as journalctl does on Linux.
- A per-user named pipe controls the supervisor: `\\.\pipe\backsight-<project>`, ACL for the current user only. It handles `stop` (API first, then `pg_ctl stop -m fast`, then exit 0, so the task does not restart it), `status`, `api-stop` and `api-start`. `api-stop`/`api-start` give maintenance what Linux gets from having two units: the database up with the API down, for backups, `StopAPI` and restore.
- **The task**, written as Task Scheduler XML through `schtasks /Create /XML`:
  - Principal: the user's SID, not `DOMAIN\name`. Over SSH, `USERDOMAIN` is `WORKGROUP` and the name does not resolve (spike, 2026-10-30).
  - `InteractiveToken`, `LeastPrivilege`.
  - A logon trigger, plus a trigger repeating every 5 minutes.
  - No execution time limit, battery allowed, `StartWhenAvailable`, `MultipleInstancesPolicy=IgnoreNew`.
  
  `schtasks` is used, not the PowerShell ScheduledTasks cmdlets: those go through CIM, which refuses a standard user's network logon ("Access denied", spike, 2026-10-30).

  Spike as `bstest`, a standard user, over SSH (2026-10-30):
  - `schtasks /Create /XML` with the SID and `InteractiveToken` succeeds.
  - S4U and boot-trigger tasks are refused ("Access is denied"), which confirms the non-goal.
  - `schtasks /Run` with no console logon leaves the task `Ready` without running it, which confirms the no-session path below.
- **Recovering from a supervisor crash** (builder check):
  - `RestartOnFailure` covers a task that fails to launch, not a process that exits non-zero, so it is not relied on.
  - The repeating trigger with `IgnoreNew` brings a dead supervisor back within 5 minutes, and does nothing while one runs.
  - `server stop` therefore disables the task (`schtasks /Change /DISABLE`) before stopping the supervisor, and `server start` enables it again.
  - A supervisor whose children keep failing backs off up to 5 minutes between restarts, and never exits on its own.
- **No interactive session** (SSH, CI): an `InteractiveToken` task does not run without a console logon.
  - `server start` sees the task cannot run and starts the supervisor detached, breaking away from the caller's job as `detach_windows.go` already does.
  - It says the server will not come back after logoff or restart until the next logon; the task stays registered for that logon.
  - CI's chain and the agent's SSH steps use this path.
- **Updates**: on Linux, a backsight-only `update apply` leaves the server running, because the units run Postgres and Hindsight, not backsight. On Windows the same holds:
  - The supervisor keeps running on its `current.exe.old-<n>`, and the pipe's `status` answers with a protocol version.
  - `update apply` restarts the supervisor only when that version changes, or when `server upgrade` runs anyway. Each restart is a short outage, and hooks queue in the outbox meanwhile.
  - Doctor reports a supervisor running an older backsight as info.
- **Elevated users**: `postgres.exe` refuses to run with an admin token. The supervisor starts it with a restricted token (Administrators removed), as `pg_ctl` does, so hosted CI runners and admins with UAC off work too.
- **No console window**: the task's action is `conhost.exe --headless "<shim>" server supervise`. The plan's first task checks this on the VM; the fallback is a supervisor binary entry built as a GUI-subsystem program.
- "Running" never comes from parsing `schtasks /Query` output, which is localized. It comes from the pipe answering.
- Linger has no Windows equivalent and needs none: the logon trigger is the "start without me" Linux gets from linger. Preflight and doctor skip linger on Windows.

The native runtime's Go code splits along this line. A service-manager seam (systemd units on Linux, task + supervisor on Windows) sits under the runtime, and everything else is shared: the fetch, the lock, the db tools, the identity, the guard, the switch's dump and restore steps. *Agent's call*: the builder shapes the seam, provided Linux's rendered units stay byte-identical (golden tests).

### pg0, Postgres and the layout on Windows (*agent's call*)
- pg0 ignores `HOME` and `USERPROFILE` on Windows and always uses `C:\Users\<user>\.pg0` (spike). So pg0 is used only to extract Postgres and create the instance, with `--data-dir` under backsight's root (`server\native\db\`). backsight then copies the extracted installation to `server\native\pg\<version>\`, runs that copy's `postgres.exe` directly (as Linux does, 0078 as built), and deletes pg0's `instances\<name>` entry once the real passwords are set. Nothing backsight depends on stays under `~\.pg0`.
- `server\native\` keeps Linux's names: `python\`, `venv\<hindsight>-<hash12>\`, `hf\`, `server.env`, `identity`.
  - The venv layout is Windows': `Scripts\*.exe`, `Lib\site-packages`.
  - uv is `uv-x86_64-pc-windows-msvc.zip` at the pinned version and sha256 (requirement 4), and `uv.exe` is never taken from `PATH`.
- **The VC++ runtime**: the Windows lock pins and hashes the `msvc-runtime` wheel (14.44.35112 in the spike). backsight copies its `msvcp140*.dll`/`vcruntime140*.dll` next to `postgres.exe`, `initdb.exe`, `pg_*.exe` and onnxruntime's DLL. This is app-local, needs no admin, and comes from the same hash-checked set as everything else. Doctor checks that each copy is present and matches the wheel.
- **Paths near MAX_PATH**:
  - Long paths are off by default (`LongPathsEnabled=0` on the VM) and need admin to turn on.
  - The deepest file in the spike's venv is 136 characters below the venv root.
  - Preflight refuses a native install whose venv root plus 140 is over 250, naming `BACKSIGHT_HOME` as the way out. Doctor runs the same check.
- **Tool environment**: Linux's fixed `PATH=/usr/bin:/bin`, `LANG` and `LD_LIBRARY_PATH` become, on Windows, `SYSTEMROOT`, `SystemDrive`, `TEMP`/`TMP` (under backsight's root), `USERPROFILE`, `LOCALAPPDATA`, `APPDATA` and `COMSPEC`, with `PATH` set to the tool's own directory plus `System32`. Python's variables are unset as on Linux.

### The lock for Windows (*agent's call*)
`requirements-windows-amd64.txt` is embedded beside the Linux lock, hash-locked for `win_amd64` with the same pinned Python patch (3.11.17, which uv 0.13.1 provides on Windows). It is generated by a script from the Linux lock:
- Every package present on both platforms gets the Linux lock's version.
- Windows-only packages are added with their markers: `winloop` instead of `uvloop`, `msvc-runtime`, `colorama`, `tzdata` and whatever the resolve needs. Each is listed in the script.

CI fails when a shared package's version differs between the two locks, so the image pin still drives both. The Windows lock's hash12 gives Windows its own native version id, which is expected.

Some Linux-pinned versions may have no `win_amd64` wheel. Those go in a named exception list in the script, each entry with its reason, and CI accepts only the listed differences. The Linux lock is never changed for Windows' sake: changing it would change Linux's version id and upgrade Linux installs, which is an anti-goal.

### E29's requirements on Windows
1. fsync: `-c fsync=on` on `postgres.exe`'s command line, checked `on` after every start, as on Linux.
2. Identity:
   - `pg_controldata` exists on Windows, so `native-check` runs at the head of `supervise` and refuses before starting anything.
   - A refusal exits with a code the supervisor treats as final: no restart loop. It writes the reason where `server status` and doctor read it.
3. Strays:
   - A stray is a live `postgres.exe` whose image is under `server\native\pg\` and which is not in the supervisor's Job Object. It is found with `postmaster.pid`, a Toolhelp process snapshot, `QueryFullProcessImageName` and `IsProcessInJob`.
   - Strays are stopped with `pg_ctl stop -m fast`, never adopted.
4. Pins: uv, Python, both locks and pg0 as above.
5. Dumps: the same format both ways. `backup verify` uses a throwaway instance under the root's `tmp\` on a free port, run by the supervisor in a throwaway mode in its own Job Object.
6. Privacy:
   - backsight's data root and `secrets.toml` get a protected DACL: the user, SYSTEM and Administrators only, with inheritance off. It is set with `SetNamedSecurityInfo` when the directory or file is created.
   - Doctor reads the DACL and reports any other principal with read access. On Windows this replaces the 0700/0600 mode checks, which Go cannot express there: it reports 0777/0666.
   - `BACKSIGHT_HOME=C:\x` therefore gets the same privacy as `%LOCALAPPDATA%`.
7. Linger: not applicable (above).
8. onnx reranker: as on Linux.

### What breaks on Windows today, and the rule for each (*agent's call*)
From the code survey of 2026-10-30:
- **Mode bits used as "is executable"** (`uvInstalled`, `shimMissing`, doctor `native-home`): replaced by a per-OS helper. On Windows, "executable" means the file exists with an `.exe` name. These three are bugs on Windows today: uv re-downloads every time, the shim always reads as missing, and doctor always errors.
- **Replacing a running `current.exe`**: Windows cannot delete or overwrite a running exe, but can rename it. `pointShim` renames the old shim to a unique `current.exe.old-<n>`, and `.old-*` files nobody holds open are deleted on later runs. This fixes `update apply`, which swaps the shim twice in one run and fails on the second swap with today's fixed `.old` name. `checksums.txt` and `.minisig` are copied beside `current.exe`, so the self-check stops reporting "Unsigned".
- **`backsight` on `PATH`**: `%LOCALAPPDATA%\Programs\backsight\backsight.exe` is a copy, swapped the same way, with that directory added to the user `PATH` (HKCU, no admin). Git Bash runs `backsight`. A `.cmd` would not run without its extension.
- **Atomic writes and concurrent readers**: a rename over a file another process is reading fails with "Access is denied". Writes retry briefly (bounded, about 2 s). backsight's own readers of `state.json` and session files open with `FILE_SHARE_DELETE`. Lock-file removal retries the same way.
- **Uninstall while sessions run**:
  - First stop the supervisor task and delete the jobs.
  - Then delete what can be deleted, and rename what is held open into the data root's `trash\`.
  - Start a detached cleaner that retries for a few minutes after uninstall exits.
  - Anything still left is named, with "close Claude Code, then delete <path>".
- **Scheduled jobs**: `schtasks` with flags becomes the same XML route as the supervisor: a hidden console, no battery stop, no 72 h limit, `StartWhenAvailable` so a missed 04:00 maintain runs at the next logon. A task cannot carry environment variables, so a non-default `BACKSIGHT_HOME` or `BACKSIGHT_STATE_DIR` reaches the job as an argument the hidden commands accept.
- **Hooks**:
  - Already exec form (`command` = the shim, `args`), so they work with Git Bash or without it (Claude Code docs).
  - The skills' `Bash('<shim>' …)` allowed-tools and bodies use the shim with forward slashes (`C:/Users/…/current.exe`), which both Git Bash and PowerShell run. The plan checks that Claude Code's permission rules match them.
  - Pairing a hook with its MCP server by parent PID is checked on Windows. If Claude Code's process tree differs, pairing falls back to the session id the hook already carries.
  - `CLAUDE_ENV_FILE`'s `export` lines are checked under Git Bash.
- **Transcript paths**: attribution recognizes `C:\…` and `C:/…` roots as it does `/home/…`.
- **Doctor `FreeBytes`**: implemented on Windows (`GetDiskFreeSpaceEx`).
- **The API port**: preflight checks that it can be bound, because Hyper-V can reserve port ranges. None are reserved on the VM today.

### `install.ps1` (*agent's call*)
It does what `install.sh` does, with the same trust root:
- Fetch the newest tag, `checksums.txt` and `checksums.txt.minisig`.
- Verify the minisign signature against the key the script embeds: Ed25519 over the BLAKE2b-512 prehash, plus the trusted comment, which must contain `file:checksums.txt`.
- Check the zip's sha256 and extract `backsight.exe` into `<data>\bin\<version>\` with the checksums beside it.
- Copy `current.exe` and the `PATH` copy, and add the user `PATH` entry.

Neither PowerShell 5.1 nor .NET has Ed25519 or BLAKE2b. So the script carries a small pure-PowerShell verifier: BLAKE2b and Ed25519 verification over `System.Numerics.BigInteger`. It runs on Windows PowerShell 5.1 (preinstalled) and PowerShell 7. CI tests it against real release signatures and against a tampered file, a wrong key and a missing trusted comment. *Rejected*: trusting checksums because they came over HTTPS (an anti-goal), and downloading a `minisign.exe` (another binary to trust).

### Fresh install and setup on Windows
`setup` on Windows defaults to `--runtime native` and `--role both`. `setup --runtime native` was refused off Linux by 0078; this block lifts that for `windows/amd64` only. Preflight covers:
- disk space;
- path length;
- the API port can be bound;
- `conhost --headless` is available;
- Task Scheduler accepts the user's task. When policy blocks it, setup stops with the reason and a fallback: `backsight server start` by hand, plus the SessionStart hook starting the server when it is down. *Agent's call*: that hook fallback is built in this block, behind a config flag setup sets only when the task is refused.

### Proof
1. **CI, `windows-latest`, on every PR**:
   - `go test ./...` on Windows. The `#!/bin/sh` fake tools are rewritten as portable Go helpers: the test binary re-execs itself as the fake.
   - A `native-chain-windows` job: fresh native install → store → recall → backup → verify → restore → failed upgrade rolls back → refusals (missing instance, other identity, stray postgres, fsync off) → uninstall leaves no process and no task.
   - Hosted runners are admin and have the VC++ runtime installed, so CI cannot prove "no admin" or "app-local DLLs".
2. **The author's VM over SSH** (`ssh win11`). It has no VC++ runtime, `LongPathsEnabled=0` and no Git yet; this run proves those two things.
   - The test runs as `bstest`, a standard user created on 2026-10-30 for this purpose, which reaches the VM with the agent's SSH key.
   - Run there:
     1. `install.ps1`;
     2. `setup`;
     3. Git for Windows and Claude Code installed for that user;
     4. sessions that record and recall;
     5. a VM restart and logon, after which the server is back;
     6. a killed `postgres.exe` that the supervisor restarts;
     7. `jobs maintain`;
     8. `update apply` from the block's first release to its next, with a session open;
     9. `uninstall` with a session open.
   - The author signs Claude Code in on the VM as `bstest`. The agent cannot do that sign-in.
   - The language model is the author's real DeepSeek key (author, 2026-10-30), set by the author with `backsight secret set` in a VM terminal as `bstest`. CI keeps the mock model.
   - SSH and SCP to the VM run without approval (author, 2026-10-30: "You may SSH and SCP as much as you want … It's a throwaway VM").
   - The logon and restart steps need a console logon. The agent sets Windows autologon for `bstest` on the VM, which is admin-only and fine on a test VM.
   - `update apply` uses two real releases cut during the block by the usual release process: the first carries the Windows client, `install.ps1` and the runtime, the second the fixes the VM run finds. The block therefore ships in at least two releases.
   - A Windows client of a Linux server: the VM runs `connect` against a throwaway server in the Linux PC's sandbox (`scripts/sandbox.sh`, Compose project `bs-sandbox`), reached through an SSH tunnel. It never touches the author's live memory.
3. **The plan's first task**, before any product code, checks on the VM as `bstest`:
   - `schtasks /Create /XML` with a logon trigger and restart-on-failure;
   - `conhost --headless`;
   - the Job Object kill-on-close of a task-started process;
   - pg0 `--data-dir` plus running the copied `postgres.exe`;
   - the `msvc-runtime` DLLs for onnxruntime.
   
   Any that fails changes the design through a ruling recorded in the plan, not silently.
   
   Who rules (*agent's call*, as in 0078):
   - The agent rules and carries on, and the rulings are listed for the author at the block's end.
   - It stops and asks only when a way forward would break an anti-goal, for example one that needs admin.
4. **Docs**: `docs/windows.md` (install, what native on Windows is, SmartScreen or Defender notes, no admin, troubleshooting), plus doctor's new checks in troubleshooting.

### Exit
CI's Windows jobs green; the VM run done as `bstest` with every step above; the block reviews addressed (conformance, safety, test quality); a release whose Windows asset `install.ps1` installs.

## Why
Docker Desktop is the hurdle 0070 exists to remove, and on a company laptop it is often not allowed at all. The constraint that shapes the block is no admin, so each piece is the non-admin form of what Linux does:
- a Task Scheduler entry and a Job Object for the systemd unit and its cgroup;
- app-local DLLs for a system package;
- a DACL for chmod;
- a renamable copy for a symlink.

backsight supervising its own processes is the only option that restarts a crash, needs no admin and gives "everything dies with it" (requirement 3). It is also what macOS's launchd agent can reuse if launchd's own `KeepAlive` falls short. Testing as a separate standard user on a VM without the VC++ runtime is what makes "no admin" a fact rather than a hope, since every hosted CI runner is admin.
