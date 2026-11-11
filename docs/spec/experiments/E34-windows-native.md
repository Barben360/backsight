# E34 — Windows: Task Scheduler, Job Objects, pg0 and the VC++ DLLs (M7 block 2 Task 1)

Date: 2026-10-30. Decision 0080 Proof 3: the risky Windows pieces, checked before any product code on the author's Windows 11 Pro VM (build 26300, `ssh win11`), as `bstest` (standard user, reached over SSH) unless noted; `<admin>` is the admin account. No VC++ runtime in System32, `LongPathsEnabled=0`. Throwaway names `bs-e30-*` (the plan's working name; this file is E34), ports 28651/28655. Sources: `E34-scripts/` (`main.go` is the spike binary, cross-compiled with `GOOS=windows`; `w*.ps1` the checks). **V** = observed, **I** = inferred. Everything was removed after (tasks, `%LOCALAPPDATA%\bs-e30`, `~\.pg0`, uv's `HKCU\Software\Python\Astral`, `~\.local\bin\python*`, the earlier spike's `C:\Users\Ben\e-win` content); autologon for `bstest` stays (the VM run needs it).

## Setup
- **Autologon** (`<admin>`, admin, test VM only): `Winlogon` `AutoAdminLogon=1`, `DefaultUserName=bstest`, `DefaultDomainName=.`, `DefaultPassword` from a file copied and deleted, never echoed. After a restart `query user` shows `bstest` on `console`. **V**

## Checks

**W1 — the server task** (`schtasks /Create /XML`, SID principal, `InteractiveToken`, `LeastPrivilege`, `LogonTrigger` + `TimeTrigger` repeating `PT5M`, `IgnoreNew`, `PT0S`, battery allowed, `StartWhenAvailable`, action `conhost.exe --headless "<spike>" run …`):
- Created and run by a standard user over SSH. **V**
- A second `/Run` while one runs: "currently running", Last Result `0x80070420`, nothing started. **V**
- `/Create /F` of a changed XML while an instance runs: the instance keeps running (W1b). **V**
- The instance killed at 23:39:23 was back at 23:43:05 by the repetition trigger (≤ 5 min). **V**
- `/Change /DISABLE` does not stop a running instance (stopping is the pipe's job); `/ENABLE` restores the triggers. **V**
- After a VM restart, autologon → the logon trigger started it (23:44:15). **V**
- With no console logon (earlier spike, 2026-10-30) `/Run` leaves it `Ready`. **V**

**W2 — `conhost.exe --headless`**: the child runs in the console session with no visible window (`GetConsoleWindow`/`IsWindowVisible`: false, `MainWindowHandle` 0). Paths with spaces, `&` and non-ASCII reach argv exactly. **V** But:
- Task Scheduler **expands `%VAR%` in the action's arguments**: a path `…\a%USERNAME%b\…` became `…\abstestb\…`, the program was not found, and the task reported success (Last Result 0). **V**
- `conhost --headless` does not pass the child's exit code: a child exiting 3 shows Last Result 0. **V** (Nothing relies on it: RestartOnFailure is not used.)

**W3 — Job Object**: the spike creates a named job with `KILL_ON_JOB_CLOSE | BREAKAWAY_OK`, assigns itself, starts `ping`, `cmd /c ping` (a grandchild) and a `ping` with `CREATE_BREAKAWAY_FROM_JOB`:
- Another process opens the job by name (`OpenJobObject(JOB_OBJECT_QUERY)`) and `IsProcessInJob` is true for child, grandchild and spike, false for the breakaway child; `QueryFullProcessImageName` works on another of the user's processes. **V**
- `Stop-Process -Force` of the spike: child and grandchild gone within 2 s, the breakaway child alive. Same when the task starts the spike. **V**
- A job named without prefix is in the creator's session namespace (`Local\`): the spike started by the task (session 1) is **not** found by name from the SSH session (session 0). A `Global\` name is created by a standard user and found from other sessions. **V**
- `Invoke-CimMethod Win32_Process` and `Get-CimInstance Win32_Process` are **denied** to a standard user over SSH (`0x80041003`). **V**

**W4 — pg0 and Postgres from backsight's own directory**:
- `pg0.exe start` fails with `0xC0000135` (DLL not found) unless the VC++ DLLs are reachable (W5). **V**
- `pg0 start --name bs-e30 --data-dir <dir>\db --port 28651 -u hindsight -n hindsight -c fsync=on`: extracts Postgres 18.1.0 and pgvector 0.8.5 into `%USERPROFILE%\.pg0\installation\18.1.0`, creates the cluster **in `--data-dir`**, creates the role and database, starts it and exits 0. **V**
- When pg0's stdout is a pipe the caller's read never ends: the postmaster inherits the pipe. With stdout and stderr to a file (`cmd /c "… > file 2>&1"`) pg0 returns at once. **V** (PowerShell's `Start-Process -Wait` waits for descendants too: not a fix.)
- `ALTER ROLE … PASSWORD` from `psql -f -` on stdin with `PGPASSWORD=postgres`: rc 0. `pg0 stop --name bs-e30`: clean shutdown. **V**
- `instances\bs-e30` holds only `instance.json` (263 bytes: pid, port, `data_dir`, `installation_dir`, user, pg0's default password `postgres`, database, version). Deleting that directory leaves the data intact. **V**
- The installation is 283 MB in 4 854 files. Copied to `<dir>\pg\18.1.0`, its `postgres.exe -D <dir>\db -p 28651 -c fsync=on -c listen_addresses=127.0.0.1` comes up ("ready to accept connections"); `pg_ctl stop -m fast` stops it; the copy's `initdb` runs. **V**
- As `<admin>` (an admin token over SSH): `initdb` runs (it drops its own admin rights); `postgres.exe` refuses ("Execution of PostgreSQL by a user with administrative permissions is not permitted"). **V**
- A restricted token as `pg_ctl` builds it starts it: `CreateRestrictedToken(token, 0, {Administrators, Power Users} deny-only, no privileges deleted)` **and the user's SID added to the token's default DACL**. Without the DACL step postgres starts but fails to run its own `postgres -V` ("Permission denied", "could not locate matching postgres executable"); with `DISABLE_MAX_PRIVILEGE` the same. With both as `pg_ctl`: `pg_isready` accepting, clean stop. **V**

**W5 — the VC++ runtime, app-local**: System32 has no `msvcp140.dll` nor `vcruntime140_1.dll`. **V** The `msvc-runtime` 14.44.35112 wheel installs `concrt140`, `msvcp140`, `msvcp140_1`, `msvcp140_2`, `msvcp140_atomic_wait`, `msvcp140_codecvt_ids`, `vcruntime140`, `vcruntime140_1`, `vcruntime140_threads` into the venv root and `Scripts\`. **V**
- With `PATH=System32` only, every one of the installation's `postgres`, `psql`, `pg_ctl`, `initdb`, `pg_dump`, `pg_restore`, `pg_controldata`, `pg_isready` and `pg0.exe` fail with `0xC0000135`; with the nine DLLs copied into the installation's `bin\` all answer `--version`. **V**
- `import onnxruntime` with the DLLs beside `python.exe` (`Scripts\`) fails ("DLL load failed while importing onnxruntime_pybind11_state"); with them copied into `onnxruntime\capi\` only, it imports (1.31.1). **V**

**W6 — uv and Python for Windows**:
- uv 0.13.1 `uv-x86_64-pc-windows-msvc.zip`, sha256 `3b2661a463a5baea7f5076ea2b25ab71d77f36a6c0d809e5350651792f80a672` (the release's `.sha256` asset). **V**
- `uv python list --only-downloads 3.11` offers `cpython-3.11.17-windows-x86_64-none`. **V**
- Over SSH, `uv python install 3.11.17` installs the patch directory, then fails creating the minor-version junction `cpython-3.11-windows-x86_64-none` ("untrusted mount point", os error 448) and exits 2. Under the task (interactive logon) the same command exits 0. **V** uv has no option to skip that junction.
- Without `--no-bin` uv writes `python3.11.exe` into `~\.local\bin`; by default it also registers the Python under `HKCU\Software\Python\Astral` (PEP 514). **V** Linux already passes `--no-bin`.
- The lock resolves on Windows with the Windows-only `colorama`, `msvc-runtime`, `tzdata`, `winloop` (0.8.1); `onnxruntime` 1.31.1; install 70 s from a warm network. The deepest path below the venv root is 133 characters (0080's preflight assumed 140). **V**

**W7 — the named pipe**: a server with DACL `D:P(A;;GA;;;<user SID>)`, `FILE_FLAG_FIRST_PIPE_INSTANCE` and `PIPE_REJECT_REMOTE_CLIENTS`: the user connects and gets the answer; a second server on the name fails ("All pipe instances are busy"); `<admin>`, elevated admin, is refused ("Access is denied"). **V**

**W8 — no console logon**: from SSH, a process started with `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS | CREATE_BREAKAWAY_FROM_JOB` (the first flag set `detach_windows.go` tries) runs on after the SSH call returns (session 0). **V**

## Verdict for the plan (M7 block 2)
Every piece of 0080's design holds; none needs admin. Rulings, written into `plans/2026-10-30-m7-block2-windows.md`:
1. The supervisor's Job Object and single-instance mutex are named `Global\backsight-<project>-<user SID>` (not `Local\`): a supervisor started detached from SSH (session 0) and one from the task (session 1) must see each other, and doctor's stray check from any session must find the job.
2. pg0 always runs with stdout and stderr to a file under `<Root>\tmp\`, never a pipe.
3. The nine `msvc-runtime` DLLs are copied (with their sha256 in the manifest) into the copied installation's `bin\`, pg0's `bin\` and `onnxruntime\capi\`.
4. The restricted token is `pg_ctl`'s: flags 0, Administrators and Power Users deny-only, no privilege removed, the user's SID added to the default DACL.
5. Preflight refuses a backsight root or shim path containing `%` (Task Scheduler expands it), naming `BACKSIGHT_HOME`; the task XML still escapes `&`, `<`, `>`.
6. `uv python install` gets `--no-bin --no-registry` on Windows; an exit whose stderr is the minor-version link error is accepted when `python\cpython-3.11.17-windows-x86_64-none\python.exe` then runs `-c "import sys"`.
7. backsight never uses CIM/WMI on Windows (denied to standard users over SSH).
8. The copied installation is moved (a rename on the same volume) from pg0's `installation\18.1.0`, copied only across volumes; pg0 extracts again if it ever needs it.
9. Preflight's path bound stays `venv root + 140 ≤ 250` (measured 133: 7 to spare).
