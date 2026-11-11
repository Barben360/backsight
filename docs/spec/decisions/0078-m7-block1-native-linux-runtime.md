# 0078 — M7 block 1: a native Linux server runtime, and a runtime switch proven by a round trip on the author's install

- **Date**: 2026-10-29
- **Status**: accepted
- **Decided by**: author (2026-10-29: start M7 with the Linux runtime; move the live install to native and back to Docker with no data loss, on a few sessions run for that purpose; Windows next, on a VM over SSH; "I follow your recommendations." to every agent's call, after the builder check), on the agent's proposal; every point marked *agent's call* is open to veto
- **Builds on**: 0070 C (native Linux runtime), E29's "Verdict for 0070" (8 requirements), 0044 (restore epochs), 0073 (`server_id`)

## Intent
0070 C gives the server a second runtime: Hindsight from PyPI on embedded pg0 under a systemd user unit, with Docker still the Linux default. Block 1 builds it and, with it, **the way an existing install changes runtime**, because the author will move the live memory to native and back (author, 2026-10-29). That round trip is the proof, and the switch is a command users get, with a docs page for both directions.

The runtime is built for Windows next (author, 2026-10-29: a Windows VM over SSH with Claude Code). So nothing in the native runtime's shape may be Linux-only beyond what the service manager needs: a Windows per-user service later replaces the systemd unit, not the design.

### Goals
- `server.runtime = "native"` runs the memory with everything Docker gives except the web UI: install, start, stop, status, logs, backup, `backup verify`, restore, `server upgrade` with rollback, uninstall, doctor.
- E29's 8 requirements hold by construction, and each one has a test that fails without it.
- `server runtime switch native|docker` moves an install's memory between runtimes with nothing lost and no client action: same `server.url`, same `server_id`, no restore epoch.
- The author's install goes Docker → native → Docker on a few dedicated sessions, with the counts and recall identical at every step.

### Non-goals
- Native as the Linux default. Docker stays the default (0070 C).
- The web UI on native (0070 C: none unless the Control Plane comes later).
- Private exposure (`server expose --private`, 0073) on native. *Agent's call*: refused with a plain message in block 1; the author's install is loopback. It comes back with the TLS proxy (0076's carry) or the Windows block, whichever needs it first.
- macOS, Windows, WSL (below).
- GPU (0035, v2).

### Anti-goals (failure even if it "works")
- A start that comes up healthy on an empty database where the memory was (E29 §4: pg0 runs initdb silently).
- A switch that leaves the user with no working runtime, or with the memory in only one place before the new one is verified.
- A postgres left running outside the unit, reused or never stopped (E29 §4).
- A second runtime that makes Docker installs behave differently in any way.
- The database password readable by another local user (pg0's 0644 `instance.json`).

## Decision

### Layout (*agent's call*)
Under backsight's 0700 data directory, `server/native/`:
- `python/`: the uv-managed Python 3.11 at the image's patch release (requirement 4).
- `venv/<hindsight version>-<lock hash12>/`: one venv per locked set, so an upgrade installs beside the running one and a rollback switches back (as `<data>/bin/` does for backsight itself, 0067 block 2).
- `home/`: the unit's `HOME`, so pg0's `~/.pg0` (instance, `instance.json`, extracted Postgres) lands under 0700 (requirement 6).
- `hf/`: `HF_HOME`, the models.
- `server.env` (0600): the same Hindsight settings Docker's `.env` holds, plus `HINDSIGHT_API_DATABASE_URL=pg0://hindsight:<random>@hindsight:<pg port>?fsync=on` (requirements 1 and 6) and `RERANKER_PROVIDER=onnx` (requirement 8).
- `identity`: the instance's identity (below).

The unit is `<name>-server.service` in the user's systemd directory, written through the config editor like the job units (0042, 0025): `KillMode=control-group` (requirement 3), `Restart=on-failure`, `ExecStartPre=<shim> server native-check`, `ExecStart=<venv>/bin/hindsight-api` bound to `127.0.0.1` on the port `server.url` names. The API port is the one Docker publishes today, so `server.url` never changes on a switch. pg0 gets the next free port on loopback.

**The database with the API stopped** (*agent's call*). Backups, restore, `StopAPI` and the switch need Postgres up while Hindsight is down; a pg0 started from a shell is exactly the stray process requirement 3 forbids. Preferred: pg0 in its own unit, `<name>-db.service`, which `<name>-server.service` `Requires=` and orders `After=`, with Hindsight connecting by `postgresql://` to it; this mirrors Docker's two services and Windows' two services later. It needs pg0 to run under a unit in the foreground with `fsync=on` set through its own arguments or config; the plan's first task checks that on pg0 0.15.2. If pg0 cannot, the fallback is the single unit with `pg0://…?fsync=on`, and backsight runs pg0 in a transient `systemd-run --user` unit for maintenance, which the guard recognises as its own. Either way `fsync` is checked as `on` after every start (doctor and the switch).

**Platform**: native is `linux-amd64` only in block 1 (SPEC: Linux x86_64 first); elsewhere `--runtime native` refuses with a plain message.

### uv and the locked sets (*agent's call*)
- backsight downloads a pinned uv release and checks its sha256, which the binary carries; uv is never taken from `PATH`.
- Python is pinned to an exact 3.11 patch. When uv cannot provide the image's patch (E29: 3.11.15 against the image's 3.11.17), the closest 3.11 uv provides is pinned and recorded beside the lock (*agent's call*: the locked packages are what E29 found mattered; the patch differs in E29 too and parity held).
- The hash-locked sets (`requirements-<os>-<arch>.txt`, `--require-hashes`) are generated from the pinned Hindsight image's site-packages by a script in the repo (E29's method), and are embedded in the binary next to the image pin, with `pg0-embedded==0.15.2` (requirement 4). A CI job fails when the image pin and the locked set disagree.
- `Fetch` installs the venv for `Version()`; `Downloads` reports Python, wheels and models with their sizes.

### The identity check (requirement 2)
- At the first start and after every restore into the instance, backsight records the cluster's `pg_controldata` "Database system identifier" in `server/native/identity`.
- `server native-check` (hidden, the unit's `ExecStartPre`, so it also runs at boot and on `Restart=`) refuses the start when the instance directory is missing or empty, or when the identifier differs from the recorded one. The refusal names the instance path and `backsight restore`; doctor reports it as an error.
- A first install has no identity file and passes; a missing identity file beside an existing instance is an error, not a fresh start.

### The stale-process guard (requirement 3)
`server native-check`, `restore` and `backup verify` look for a postgres whose data directory is the instance's and which is not in the unit's cgroup; they report it and stop it (`pg0 stop`), never adopt it. Nothing in backsight starts `hindsight-api` outside the unit.

### Database tools and backups (requirement 5)
- `Database` runs pg0's bundled `psql`, `pg_dump`, `pg_restore` with their `lib` on `LD_LIBRARY_PATH`. The dump format is the one Docker writes, so archives cross runtimes.
- `VerifyDump` restores into a throwaway pg0 instance (own `HOME` under the data dir's tmp, own port) and stops and deletes it after.
- The archive manifest gains the runtime it came from and that runtime's server version (today it records only `hindsight_image` and `postgres_image`). `restore`'s version check compares across runtimes by Hindsight version, not image ref. `backup verify` runs in the runtime that will restore it (requirement 4: Docker → native goes to PostgreSQL 18.1 and pgvector 0.8.5 from 18.6 and 0.8.7).
- Every caller that still reads `compose.yaml` directly (`update apply`'s deployed-version reads) goes through `Runtime.Deployed()`, so a later Hindsight bump works on native.

### Linger (requirement 7)
`setup --runtime native` and `server runtime switch native` check `loginctl show-user $USER -p Linger`; when off, they stop with the command to run (`loginctl enable-linger`). doctor reports linger off as an error on a native install.

### `server upgrade` on native
Safety backup as on Docker; install the new venv beside the old; stop; point the unit at the new venv; start; smoke. Any failure points the unit back at the old venv and starts it, and keeps the safety backup (0067 block 2's rule). Old venvs beyond the previous one are removed after a successful upgrade.

### `server runtime switch <docker|native> [--plan]` (*agent's call* on the name and steps)
Runs under the maintenance lock, so jobs wait; hooks queue in the outbox meanwhile (M2's breaker).
1. Pre-flight for the target: Docker's engine and Compose, or linger, disk space and downloads for native. Downloads happen here, before anything stops.
2. Stop the source's API, then take a backup with the database still up and the API stopped, so nothing is written after the dump. Verify it in the **target** runtime.
3. Stop the source. Its data stays where it is (Docker volume or `server/native/home`).
4. Render and start the target with no data, restore the backup into it, record the identity (native), start.
5. Compare with the source's dump. **The gate is the data, exactly**: row counts and a hash of the primary keys of every table backsight backs up, the `server_id` and epoch documents, bank ids, the feedback ledger documents. **Recall is reported, not gated** (*agent's call*): Docker's reranker is torch and native's is onnx (requirement 8), E29 proved identical rankings only on the docs bank, and near-ties in a real memory could reorder without any data difference. The probes are the user's own: the texts of up to 20 recent memories of the bank, each searched for; the switch prints overlap@10 and how many found their own memory at rank 1, and warns below 0.9 overlap.
6. Only then write `server.runtime` to `config.toml`. Any failure before this stops the target and restarts the source as it was; the backup stays.

No restore epoch is bumped (*agent's call*): the dump is taken with the API stopped, so the target holds exactly what the source held and clients have nothing to reconcile. `server_id` and the epoch live in the control bank, so the dump carries them. The switch therefore does not reuse `restore` as it is (which writes a new epoch and restarts on a temporary key); it shares its dump and restore steps. The switch is recorded in the jobs log and shown by `stats` and doctor.

The source's data is kept until `server runtime clean` (shows what it deletes, asks before), and doctor reports it as info with its size. Switching back while the old data is still there restores over it: the switch always carries the current memory, never revives the old one.

### Fresh install on native
`setup --runtime native` installs a new memory on native (the Windows block needs this path, and CI uses it). `setup` without the flag is unchanged.

### Doctor on native
New checks: unit state, `fsync` is `on`, identity matches, no stray postgres, linger on, `home/` mode 0700, venv matches the lock, web UI "none on native" as info. Docker-only checks say "not on native".

### WSL
Not targeted in block 1 and not tested. In theory WSL2 with systemd enabled runs the native runtime as on Linux; the Windows block decides between a native Windows service and WSL, on the author's VM.

### Proof
1. **CI**, a `native-chain` job on every PR (hosted `ubuntu-latest`, with linger enabled for the runner user and the model cache shared with the Docker jobs): fresh native install → store → backup → verify → switch to Docker → switch back to native → counts and probes identical; plus the refusals (missing instance, empty instance, other identity, stray postgres, fsync), a failed upgrade rolling back, a failed switch leaving the source running.
2. **The author's machine** (author, 2026-10-29), each step with the author's go:
   1. verified backup of the live install;
   2. `server runtime switch native --plan`, then the switch;
   3. a few sessions run for this purpose, plus one `jobs maintain` (backup and verify on native);
   4. `server runtime switch docker`;
   5. counts, `server_id`, ledger and probes compared with step 1 plus what the dedicated sessions added; nothing missing;
   6. `server runtime clean` only on the author's go.
3. **Docs**: `docs/native-runtime.md` (what it is, what it lacks, the switch both ways, `clean`) and troubleshooting rows for each new doctor check.

### Exit
CI's `native-chain` green; the author's round trip done with the comparison clean; the block reviews (conformance, safety, test quality) addressed; a release.

## Why
The round trip on real data is the test 0070 C's "switching runtime is a backup in one and a restore in the other" needs, and the author's machine is the only place with real data. Building the switch into a command, not a recipe, puts E29's cross-runtime caveat (an older Postgres minor on native) and the identity check in code that runs every time, and gives Windows users the same path later. Keeping the source's data until `clean` means the worst case of a bad switch is a restart of the old runtime.
